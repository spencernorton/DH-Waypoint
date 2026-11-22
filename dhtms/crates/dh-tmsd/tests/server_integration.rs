use dh_tmsd::engine::DaemonEngine;
use dh_tmsd::fixture::build_synthetic_fixture;
use dh_tmsd::http::serve_unix;
use serde_json::Value;
use std::io::{Read, Write};
use std::os::unix::net::UnixStream;
use std::path::Path;
use std::sync::atomic::{AtomicBool, Ordering};
use std::sync::Arc;
use std::time::Duration;

fn unix_req(
    sock: &Path,
    method: &str,
    path: &str,
    body: Option<&str>,
    token: Option<&str>,
) -> (u16, Value) {
    let mut s = UnixStream::connect(sock).expect("connect unix sock");
    let b = body.unwrap_or("");
    let auth_hdr = token
        .map(|t| format!("X-DHTMS-Token: {t}\r\n"))
        .unwrap_or_default();
    let raw = format!(
        "{method} {path} HTTP/1.1\r\nHost: localhost\r\n{auth_hdr}Content-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{b}",
        b.len()
    );
    s.write_all(raw.as_bytes()).unwrap();
    let mut resp = String::new();
    s.read_to_string(&mut resp).unwrap();
    let (hdr, json_body) = resp.split_once("\r\n\r\n").expect("http framing");
    let status: u16 = hdr
        .lines()
        .next()
        .unwrap()
        .split_whitespace()
        .nth(1)
        .unwrap()
        .parse()
        .unwrap();
    let v: Value = serde_json::from_str(json_body).expect("json response");
    (status, v)
}

#[test]
fn unix_socket_full_contract_auth_policy_egress_v2_swarm_and_fail_closed_lease() {
    let base = std::env::temp_dir().join(format!(
        "dh_tmsd_integ_{}_{}",
        std::process::id(),
        std::time::SystemTime::now()
            .duration_since(std::time::UNIX_EPOCH)
            .unwrap()
            .as_nanos()
    ));
    let _ = std::fs::remove_dir_all(&base);
    let db_root = base.join("db");
    let wal_dir = base.join("wal");
    let sock_path = base.join("dh-tmsd.sock");
    let tok = "test-shared-secret-token";

    build_synthetic_fixture(&db_root).expect("build fixture");
    let engine = Arc::new(DaemonEngine::open(&db_root, &wal_dir, 42, 60_000).expect("open engine"));
    let shutdown = Arc::new(AtomicBool::new(false));
    let sh_srv = Arc::clone(&shutdown);
    let sock_srv = sock_path.clone();
    let srv_tok = Some(Arc::new(tok.to_string()));
    let srv_handle = std::thread::spawn(move || {
        serve_unix(engine, &sock_srv, srv_tok, sh_srv).expect("serve_unix");
    });

    for _ in 0..100 {
        if sock_path.exists() {
            break;
        }
        std::thread::sleep(Duration::from_millis(10));
    }

    // 1. Health check (exempt from token auth for container HEALTHCHECK)
    let (st, v) = unix_req(&sock_path, "GET", "/v1/health", None, None);
    assert_eq!(st, 200);
    assert_eq!(v["status"], "ok");
    assert_eq!(v["lease_active"], true);
    assert_eq!(v["num_docs"], 100);

    // 1b. Missing or wrong token MUST return 401 Unauthorized on data/mutation endpoints!
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/search",
        Some(r#"{"query":"usual suspects","mode":"and","tier":"pub","limit":20}"#),
        None,
    );
    assert_eq!(st, 401);
    assert_eq!(v["code"], "unauthorized");

    // 2. Search "usual suspects" under pub tier using server wall clock (NO now_epoch needed!)
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/search",
        Some(
            r#"{"query":"usual suspects","mode":"and","tier":"pub","limit":20,"include_facets":true,"include_files":true}"#,
        ),
        Some(tok),
    );
    assert_eq!(st, 200);
    let docs = v["docs"].as_array().unwrap();
    assert_eq!(
        docs.len(),
        1,
        "only the 1 clean pub_approved doc must appear"
    );
    let v1_hex = docs[0]["info_hash_v1"].as_str().unwrap().to_string();
    let v2_hex = docs[0]["info_hash_v2"].as_str().unwrap().to_string();
    assert_eq!(v1_hex.len(), 40);
    assert_eq!(v2_hex.len(), 64);
    assert!(docs[0]["magnet_xt"]
        .as_str()
        .unwrap()
        .starts_with("urn:btih:"));
    assert_eq!(docs[0]["files"].as_array().unwrap().len(), 2);

    // 3. Adult FC2-PPV MUST NOT appear in search even under base tier!
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/search",
        Some(r#"{"query":"fc2 ppv","mode":"and","tier":"base","limit":20}"#),
        Some(tok),
    );
    assert_eq!(st, 200);
    assert_eq!(v["docs"].as_array().unwrap().len(), 0);

    // 4. Accented title search & unlinked fallback (Amelie -> finds both linked and CT_UNKNOWN unlinked!)
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/lookup",
        Some(
            r#"{"content_type":1,"content_source":"tmdb","content_id":"102","tier":"base","limit":20}"#,
        ),
        Some(tok),
    );
    assert_eq!(st, 200);
    assert_eq!(v["linked"].as_array().unwrap().len(), 1);
    assert_eq!(v["unlinked"].as_array().unwrap().len(), 1);

    // 5. TV episode range lookup S01E03 (expanded from S01E02-E04!) + 800-file S02 pack hydration!
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/lookup",
        Some(
            r#"{"content_type":2,"content_source":"tmdb","content_id":"201","season":1,"episode":3,"tier":"pub","limit":20}"#,
        ),
        Some(tok),
    );
    assert_eq!(st, 200);
    assert_eq!(v["linked"].as_array().unwrap().len(), 1);
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/lookup",
        Some(
            r#"{"content_type":2,"content_source":"tmdb","content_id":"201","season":2,"episode":5,"tier":"pub","limit":20,"include_files":true}"#,
        ),
        Some(tok),
    );
    assert_eq!(st, 200);
    assert_eq!(v["linked"][0]["files"].as_array().unwrap().len(), 800);

    // 6. Revoke by BEP-52 v2 64-hex hash -> immediately blocks v1 identity at egress!
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/policy/revoke",
        Some(&format!(
            r#"{{"info_hash":"{v2_hex}","reason":"csam_delete","pg_lsn":100,"sync":true}}"#
        )),
        Some(tok),
    );
    assert_eq!(st, 200);
    assert_eq!(v["ok"], true);
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/hash/lookup",
        Some(&format!(r#"{{"info_hash":"{v1_hex}","tier":"base"}}"#)),
        Some(tok),
    );
    assert_eq!(st, 200);
    assert_eq!(v["found"], true);
    assert_eq!(
        v["eligible"], false,
        "v2 revocation MUST atomically revoke v1 identity!"
    );

    // 7. Swarm durable update + select-stale + checkpoint
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/swarm/select-stale",
        Some(r#"{"limit":10,"min_age_min":10}"#),
        Some(tok),
    );
    assert_eq!(st, 200);
    let stale_items = v["items"].as_array().unwrap();
    assert!(!stale_items.is_empty());
    let first_hash = stale_items[0]["info_hash_v1"].as_str().unwrap();
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/swarm/update",
        Some(&format!(
            r#"{{"updates":[{{"info_hash":"{first_hash}","seeders":777,"leechers":33,"checked_epoch_min":30000050}}],"sync":true}}"#
        )),
        Some(tok),
    );
    assert_eq!(st, 200);
    assert_eq!(v["applied"], 1);
    let (st, _) = unix_req(
        &sock_path,
        "POST",
        "/v1/swarm/checkpoint",
        Some("{}"),
        Some(tok),
    );
    assert_eq!(st, 200);

    // 8. Live L0 upsert
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/l0/upsert",
        Some(
            r#"{"docs":[{"doc_id":10,"name":"Reclassified.Live.2026.1080p","content_type":1,"published_epoch":1800000050,"created_epoch":1800000050}],"sync":true}"#,
        ),
        Some(tok),
    );
    assert_eq!(st, 200);
    assert_eq!(v["l0_count"], 1);

    // 9. Expire governance lease -> stale LSN renew MUST fail -> reads MUST fail closed 503 -> newer LSN recovers!
    let (st, _) = unix_req(
        &sock_path,
        "POST",
        "/v1/policy/lease",
        Some(r#"{"pg_lsn":101,"ttl_ms":0}"#),
        Some(tok),
    );
    assert_eq!(st, 200);
    let (st, _) = unix_req(
        &sock_path,
        "POST",
        "/v1/policy/lease",
        Some(r#"{"pg_lsn":101,"ttl_ms":60000}"#),
        Some(tok),
    );
    assert_eq!(st, 400, "stale pg_lsn <= expired fence must be rejected");
    let (st, v) = unix_req(
        &sock_path,
        "POST",
        "/v1/search",
        Some(r#"{"query":"amelie","tier":"pub","limit":10}"#),
        Some(tok),
    );
    assert_eq!(st, 503);
    assert_eq!(v["code"], "lease_expired");

    let (st, _) = unix_req(
        &sock_path,
        "POST",
        "/v1/policy/lease",
        Some(r#"{"pg_lsn":102,"ttl_ms":60000}"#),
        Some(tok),
    );
    assert_eq!(st, 200);
    let (st, v) = unix_req(&sock_path, "GET", "/v1/dashstats?tier=pub", None, Some(tok));
    assert_eq!(st, 200);
    assert!(v["movies_30d"].as_u64().unwrap() >= 1);

    shutdown.store(true, Ordering::Release);
    srv_handle.join().unwrap();
    let _ = std::fs::remove_dir_all(&base);
}
