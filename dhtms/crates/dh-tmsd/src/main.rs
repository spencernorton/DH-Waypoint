use dh_tmsd::engine::DaemonEngine;
use dh_tmsd::fixture::build_synthetic_fixture;
use dh_tmsd::http::{serve_tcp, serve_unix};
use std::path::PathBuf;
use std::sync::atomic::AtomicBool;
use std::sync::Arc;

fn arg_val(args: &[String], flag: &str) -> Option<String> {
    args.windows(2).find(|w| w[0] == flag).map(|w| w[1].clone())
}

fn has_flag(args: &[String], flag: &str) -> bool {
    args.iter().any(|a| a == flag)
}

fn main() {
    let args: Vec<String> = std::env::args().collect();
    if args.len() < 2 {
        eprintln!("Usage: dh-tmsd <serve|init-fixture> [flags...]");
        std::process::exit(2);
    }
    match args[1].as_str() {
        "init-fixture" => {
            let out = PathBuf::from(arg_val(&args, "--out").expect("--out required"));
            let manifest = build_synthetic_fixture(&out).expect("init-fixture failed");
            println!("{}", serde_json::to_string_pretty(&manifest).unwrap());
        }
        "serve" => {
            let db_root = PathBuf::from(arg_val(&args, "--db-root").expect("--db-root required"));
            let wal_dir = PathBuf::from(arg_val(&args, "--wal-dir").expect("--wal-dir required"));
            let listen = arg_val(&args, "--listen").unwrap_or_else(|| "127.0.0.1:7841".into());
            let pg_lsn: u64 = arg_val(&args, "--pg-lsn")
                .unwrap_or_else(|| "1".into())
                .parse()
                .expect("invalid --pg-lsn");
            let lease_ttl_ms: u64 = arg_val(&args, "--initial-lease-ms")
                .unwrap_or_else(|| "60000".into())
                .parse()
                .expect("invalid --initial-lease-ms");
            let auth_token = std::env::var("DH_TMS_AUTH_TOKEN")
                .ok()
                .filter(|s| !s.is_empty())
                .map(Arc::new);

            let mut engine = DaemonEngine::open(&db_root, &wal_dir, pg_lsn, lease_ttl_ms)
                .expect("failed to open dh-tmsd generation");
            engine.allow_clock_override = has_flag(&args, "--allow-test-clock");
            let engine = Arc::new(engine);
            let shutdown = Arc::new(AtomicBool::new(false));
            eprintln!(
                "dh-tmsd listening on {listen} (generation={}, docs={}, auth={})",
                engine.generation_id,
                engine.core.num_docs,
                auth_token.is_some()
            );
            if let Some(sock_str) = listen.strip_prefix("unix://") {
                #[cfg(unix)]
                {
                    serve_unix(engine, &PathBuf::from(sock_str), auth_token, shutdown)
                        .expect("serve_unix failed");
                }
                #[cfg(not(unix))]
                {
                    panic!("unix:// sockets are only supported on Unix");
                }
            } else {
                serve_tcp(engine, &listen, auth_token, shutdown).expect("serve_tcp failed");
            }
        }
        other => {
            eprintln!("Unknown subcommand: {other}");
            std::process::exit(2);
        }
    }
}
