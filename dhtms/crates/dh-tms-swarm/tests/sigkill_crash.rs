use dh_tms_swarm::{SwarmPlane, SwarmUpdate};
use std::io::{BufRead, BufReader, Write};
use std::path::{Path, PathBuf};
use std::process::{Command, Stdio};

fn child_main(dir: &Path) {
    let wal_path = dir.join("swarm.wal");
    let sp = SwarmPlane::open(dir, &wal_path, Some(0xCAFE_BABE)).expect("child open swarm");
    let batch: Vec<SwarmUpdate> = (0..200u32)
        .map(|d| SwarmUpdate {
            doc_id: d,
            seeders: Some(100 + d),
            leechers: Some(20 + d),
            checked_epoch_min: 30_000_000 + d,
            tracker_id: 1,
        })
        .collect();
    sp.update_batch_durable(&batch, true)
        .expect("synced swarm batch");
    println!("SYNCED_200");
    let _ = std::io::stdout().flush();

    for round in 0..2000u32 {
        let u = SwarmUpdate {
            doc_id: 200 + (round % 800),
            seeders: Some(500 + round),
            leechers: Some(50 + round),
            checked_epoch_min: 30_001_000 + round,
            tracker_id: 1,
        };
        let _ = sp.update_batch_durable(&[u], false);
    }
    std::thread::sleep(std::time::Duration::from_secs(30));
}

#[test]
fn sigkill_crash_recovery_swarm_zero_torn_slots() {
    if let Ok(child_dir) = std::env::var("DH_TMS_SWARM_CRASH_CHILD_DIR") {
        child_main(&PathBuf::from(child_dir));
        return;
    }

    let dir = std::env::temp_dir().join(format!(
        "dh_tms_swarm_sigkill_{}_{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    ));
    let _ = std::fs::remove_dir_all(&dir);
    std::fs::create_dir_all(&dir).unwrap();

    let init = vec![(-1i32, -1i32, 0u32); 2_000];
    SwarmPlane::create(&dir, 0xCAFE_BABE, &init).unwrap();

    let exe = std::env::current_exe().unwrap();
    let mut child = Command::new(exe)
        .arg("--exact")
        .arg("sigkill_crash_recovery_swarm_zero_torn_slots")
        .arg("--nocapture")
        .env("DH_TMS_SWARM_CRASH_CHILD_DIR", &dir)
        .stdout(Stdio::piped())
        .stderr(Stdio::null())
        .spawn()
        .expect("spawn crash child");

    let stdout = child.stdout.take().unwrap();
    let mut reader = BufReader::new(stdout);
    let mut line = String::new();
    let mut got_ready = false;
    while reader.read_line(&mut line).unwrap() > 0 {
        if line.contains("SYNCED_200") {
            got_ready = true;
            break;
        }
        line.clear();
    }
    assert!(got_ready, "child did not reach SYNCED_200");
    std::thread::sleep(std::time::Duration::from_millis(5));
    child.kill().expect("SIGKILL child");
    let _ = child.wait();

    let wal_path = dir.join("swarm.wal");
    {
        let mut f = std::fs::OpenOptions::new()
            .append(true)
            .open(&wal_path)
            .unwrap();
        f.write_all(&[0x53, 0x57, 0x41, 0x4C, 0x01, 0x00, 0x99])
            .unwrap();
        f.sync_data().unwrap();
    }

    let sp = SwarmPlane::open(&dir, &wal_path, Some(0xCAFE_BABE))
        .expect("swarm reopen after SIGKILL + torn tail");
    for d in 0..200u32 {
        let s = sp.read(d).expect("read");
        assert_eq!(s.seeders, Some(100 + d), "lost seeders for doc_id={d}");
        assert_eq!(s.leechers, Some(20 + d), "lost leechers for doc_id={d}");
        assert_eq!(s.checked_epoch_min, 30_000_000 + d);
        assert_eq!(s.peak_seeders, (100 + d) as u16);
    }
    sp.checkpoint().expect("post-recovery checkpoint");
    drop(sp);

    let sp2 = SwarmPlane::open(&dir, &wal_path, Some(0xCAFE_BABE)).expect("post-checkpoint reopen");
    let s199 = sp2.read(199).unwrap();
    assert_eq!(s199.seeders, Some(299));
    assert_eq!(s199.leechers, Some(219));

    let _ = std::fs::remove_dir_all(&dir);
}
