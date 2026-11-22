use dh_tms_policy::{PolicyPlane, PolicyState, PolicyTier, RevokeReason};
use std::io::{BufRead, BufReader, Write};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

fn child_main(dir: &Path) {
    let wal_path = dir.join("policy.wal");
    let p = PolicyPlane::open(
        dir,
        &wal_path,
        Some(0xDEAD_BEEF),
        1_794_355_200,
        1,
        u64::MAX / 2,
    )
    .expect("child open policy");
    for d in 0..50u32 {
        p.revoke_immediate(d, RevokeReason::CsamOrDelete, 100 + d as u64, true)
            .expect("revoke synced");
    }
    println!("SYNCED_50");
    let _ = std::io::stdout().flush();

    for (seq, d) in (1000u64..).zip(50..9_990u32) {
        let _ = p.revoke_immediate(d, RevokeReason::Hold, seq, false);
    }
    std::thread::sleep(std::time::Duration::from_secs(30));
}

#[test]
fn sigkill_crash_recovery_zero_resurrection() {
    if let Ok(child_dir) = std::env::var("DH_TMS_POLICY_CRASH_CHILD_DIR") {
        child_main(&PathBuf::from(child_dir));
        return;
    }

    let dir = std::env::temp_dir().join(format!(
        "dh_tms_policy_sigkill_{}_{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    ));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    let mut st = PolicyState::new(10_000);
    for d in 0..10_000u32 {
        st.public.insert(d);
        st.pub_approved.insert(d);
        st.payload_expires[d as usize] = 1_800_000_000;
    }
    st.save(&dir, 0xDEAD_BEEF).unwrap();

    let exe = std::env::current_exe().unwrap();
    let mut child = Command::new(exe)
        .arg("--exact")
        .arg("sigkill_crash_recovery_zero_resurrection")
        .arg("--nocapture")
        .env("DH_TMS_POLICY_CRASH_CHILD_DIR", &dir)
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .expect("spawn crash child");

    let stdout = child.stdout.take().unwrap();
    let mut reader = BufReader::new(stdout);
    let mut line = String::new();
    let mut got_ready = false;
    while reader.read_line(&mut line).unwrap() > 0 {
        if line.contains("SYNCED_50") {
            got_ready = true;
            break;
        }
        line.clear();
    }
    assert!(got_ready, "child did not reach SYNCED_50");
    std::thread::sleep(std::time::Duration::from_millis(5));
    child.kill().expect("SIGKILL child");
    let _ = child.wait();

    let wal_path = dir.join("policy.wal");
    {
        let mut f = std::fs::OpenOptions::new()
            .append(true)
            .open(&wal_path)
            .unwrap();
        f.write_all(&[0x50, 0x57, 0x41, 0x4C, 0xFF, 0x00, 0x11, 0x22, 0x33])
            .unwrap();
        f.sync_data().unwrap();
    }

    let now_sec = 1_794_355_200u32;
    let now_ms = (now_sec as u64) * 1000;
    let recovered = PolicyPlane::open(&dir, &wal_path, Some(0xDEAD_BEEF), now_sec, 1, u64::MAX / 2)
        .expect("reopen after SIGKILL must succeed cleanly");

    for d in 0..50u32 {
        assert!(
            !recovered
                .authorize_egress_one(d, PolicyTier::Base, now_sec, now_ms)
                .unwrap(),
            "RESURRECTION DETECTED for doc_id={d} after SIGKILL!"
        );
        assert!(
            !recovered
                .authorize_egress_one(d, PolicyTier::Pub, now_sec, now_ms)
                .unwrap(),
            "PUB RESURRECTION DETECTED for doc_id={d} after SIGKILL!"
        );
        assert!(
            recovered.is_tombstoned_for_scrub(d),
            "doc_id={d} lost CSAM tombstone!"
        );
    }

    recovered
        .revoke_immediate(9_999, RevokeReason::Excluded, 999_999, true)
        .expect("post-recovery revoke");
    drop(recovered);

    let reopened2 = PolicyPlane::open(&dir, &wal_path, Some(0xDEAD_BEEF), now_sec, 1, u64::MAX / 2)
        .expect("second reopen");
    assert!(!reopened2
        .authorize_egress_one(9_999, PolicyTier::Base, now_sec, now_ms)
        .unwrap());
    assert!(reopened2
        .authorize_egress_one(9_998, PolicyTier::Pub, now_sec, now_ms)
        .unwrap());

    let _ = std::fs::remove_dir_all(&dir);
}
