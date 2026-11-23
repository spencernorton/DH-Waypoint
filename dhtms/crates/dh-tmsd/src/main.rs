use dh_tmsd::builder::build_generation_from_ndjson;
use dh_tmsd::engine::EngineHost;
use dh_tmsd::fixture::build_synthetic_fixture;
use dh_tmsd::http::{serve_tcp, serve_unix};
use std::fs::File;
use std::io::{self, BufReader};
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
        eprintln!("Usage: dh-tmsd <serve|build|init-fixture> [flags...]");
        std::process::exit(2);
    }
    match args[1].as_str() {
        "init-fixture" => {
            let out = PathBuf::from(arg_val(&args, "--out").expect("--out required"));
            let manifest = build_synthetic_fixture(&out).expect("init-fixture failed");
            println!("{}", serde_json::to_string_pretty(&manifest).unwrap());
        }
        "build" => {
            let input = arg_val(&args, "--in").unwrap_or_else(|| "-".into());
            let out = PathBuf::from(arg_val(&args, "--out").expect("--out required"));
            let gen_id: u64 = arg_val(&args, "--generation-id")
                .unwrap_or_else(|| "1".into())
                .parse()
                .expect("invalid --generation-id");
            let outcome = if input == "-" {
                let stdin = io::stdin();
                build_generation_from_ndjson(stdin.lock(), &out, gen_id).expect("build failed")
            } else {
                let f = File::open(&input).expect("open --in failed");
                build_generation_from_ndjson(BufReader::new(f), &out, gen_id).expect("build failed")
            };
            println!(
                "{}",
                serde_json::to_string_pretty(&outcome.manifest).unwrap()
            );
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

            let host = Arc::new(
                EngineHost::open_or_init(
                    &db_root,
                    &wal_dir,
                    pg_lsn,
                    lease_ttl_ms,
                    has_flag(&args, "--allow-test-clock"),
                )
                .expect("failed to initialize dh-tmsd generation"),
            );
            let snap = host.load();
            let shutdown = Arc::new(AtomicBool::new(false));
            eprintln!(
                "dh-tmsd listening on {listen} (generation={}, docs={}, auth={})",
                snap.generation_id,
                snap.core.num_docs,
                auth_token.is_some()
            );
            if let Some(sock_str) = listen.strip_prefix("unix://") {
                #[cfg(unix)]
                {
                    serve_unix(host, &PathBuf::from(sock_str), auth_token, shutdown)
                        .expect("serve_unix failed");
                }
                #[cfg(not(unix))]
                {
                    panic!("unix:// sockets are only supported on Unix");
                }
            } else {
                serve_tcp(host, &listen, auth_token, shutdown).expect("serve_tcp failed");
            }
        }
        other => {
            eprintln!("Unknown subcommand: {other}");
            std::process::exit(2);
        }
    }
}
