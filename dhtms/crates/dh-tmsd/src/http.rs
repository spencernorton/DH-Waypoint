use crate::engine::{DaemonEngine, DaemonError, DashstatsRequest};
use std::io::{self, BufRead, BufReader, Read, Write};
use std::net::TcpListener;
#[cfg(unix)]
use std::os::unix::fs::PermissionsExt;
#[cfg(unix)]
use std::os::unix::net::UnixListener;
use std::path::Path;
use std::sync::atomic::{AtomicBool, AtomicUsize, Ordering};
use std::sync::Arc;
use std::time::Duration;

const MAX_BODY_BYTES: usize = 16 * 1024 * 1024;
const MAX_HEADER_LINES: usize = 64;
const MAX_HEADER_LINE_BYTES: usize = 8192;
const MAX_CONCURRENT_CONNS: usize = 128;

struct ConnGuard {
    active: Arc<AtomicUsize>,
}

impl Drop for ConnGuard {
    fn drop(&mut self) {
        self.active.fetch_sub(1, Ordering::AcqRel);
    }
}

fn try_acquire_conn(active: &Arc<AtomicUsize>) -> Option<ConnGuard> {
    let mut cur = active.load(Ordering::Acquire);
    loop {
        if cur >= MAX_CONCURRENT_CONNS {
            return None;
        }
        match active.compare_exchange_weak(cur, cur + 1, Ordering::AcqRel, Ordering::Acquire) {
            Ok(_) => {
                return Some(ConnGuard {
                    active: Arc::clone(active),
                })
            }
            Err(next) => cur = next,
        }
    }
}

/// Constant-time equality for optional shared-secret token verification.
fn ct_eq(a: &[u8], b: &[u8]) -> bool {
    if a.len() != b.len() {
        return false;
    }
    let mut diff = 0u8;
    for (&x, &y) in a.iter().zip(b.iter()) {
        diff |= x ^ y;
    }
    diff == 0
}

enum LineRead {
    Eof,
    Line(String),
    TooLong,
}

fn read_line_bounded<R: BufRead>(reader: &mut R, max_bytes: usize) -> io::Result<LineRead> {
    let mut buf = Vec::with_capacity(128);
    let mut limited = reader.take((max_bytes + 1) as u64);
    let n = limited.read_until(b'\n', &mut buf)?;
    if n == 0 {
        return Ok(LineRead::Eof);
    }
    if buf.len() > max_bytes {
        return Ok(LineRead::TooLong);
    }
    let s = String::from_utf8(buf)
        .map_err(|_| io::Error::new(io::ErrorKind::InvalidData, "non-utf8 http header"))?;
    Ok(LineRead::Line(s))
}

fn write_json_response<W: Write>(
    mut w: W,
    status: u16,
    reason: &str,
    body: &[u8],
) -> io::Result<()> {
    write!(
        w,
        "HTTP/1.1 {status} {reason}\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
        body.len()
    )?;
    w.write_all(body)?;
    w.flush()
}

fn dispatch(
    engine: &DaemonEngine,
    method: &str,
    path: &str,
    body: &[u8],
) -> (u16, &'static str, Vec<u8>) {
    let route_path = path.split('?').next().unwrap_or(path);
    let query_str = path.split_once('?').map(|x| x.1).unwrap_or("");

    let res: Result<Vec<u8>, DaemonError> = (|| match (method, route_path) {
        ("GET", "/v1/health") => {
            let resp = engine.health();
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/search") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_search(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/lookup") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_lookup(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/rss") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_rss(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("GET", "/v1/dashstats") => {
            let mut tier = String::from("base");
            let mut now_epoch = None;
            for pair in query_str.split('&') {
                if let Some((k, v)) = pair.split_once('=') {
                    match k {
                        "tier" => tier = v.to_string(),
                        "now_epoch" => now_epoch = v.parse().ok(),
                        _ => {}
                    }
                }
            }
            let resp = engine.handle_dashstats(DashstatsRequest { tier, now_epoch })?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/dashstats") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_dashstats(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/substring") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_substring(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/hydrate") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_hydrate(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/hash/lookup") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_hash_lookup(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/policy/revoke") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_policy_revoke(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/policy/batch") => {
            let resp = engine.handle_policy_batch(body)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/policy/lease") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_policy_lease(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/swarm/update") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_swarm_update(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/swarm/select-stale") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_swarm_select_stale(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/swarm/checkpoint") => {
            let resp = engine.handle_swarm_checkpoint()?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        ("POST", "/v1/l0/upsert") => {
            let req = serde_json::from_slice(body)
                .map_err(|e| DaemonError::BadRequest(format!("invalid json: {e}")))?;
            let resp = engine.handle_l0_upsert(req)?;
            serde_json::to_vec(&resp).map_err(|e| DaemonError::Internal(e.to_string()))
        }
        _ => Err(DaemonError::NotFound),
    })();

    match res {
        Ok(bytes) => (200, "OK", bytes),
        Err(DaemonError::BadRequest(msg)) => {
            let payload = serde_json::json!({"error": msg, "code": "bad_request"});
            (
                400,
                "Bad Request",
                serde_json::to_vec(&payload).unwrap_or_default(),
            )
        }
        Err(DaemonError::NotFound) => (
            404,
            "Not Found",
            br#"{"error":"not_found","code":"not_found"}"#.to_vec(),
        ),
        Err(DaemonError::LeaseExpired {
            now_ms,
            valid_until_ms,
            pg_lsn,
        }) => {
            let payload = serde_json::json!({
                "error": "governance_lease_expired",
                "code": "lease_expired",
                "now_ms": now_ms,
                "valid_until_ms": valid_until_ms,
                "pg_lsn": pg_lsn
            });
            (
                503,
                "Service Unavailable",
                serde_json::to_vec(&payload).unwrap_or_default(),
            )
        }
        Err(DaemonError::Internal(msg)) => {
            let payload = serde_json::json!({"error": msg, "code": "internal_error"});
            (
                500,
                "Internal Server Error",
                serde_json::to_vec(&payload).unwrap_or_default(),
            )
        }
    }
}

pub fn handle_connection<S: Read + Write>(
    engine: &DaemonEngine,
    auth_token: Option<&str>,
    mut stream: S,
) {
    let mut reader = BufReader::new(&mut stream);
    let req_line = match read_line_bounded(&mut reader, MAX_HEADER_LINE_BYTES) {
        Ok(LineRead::Line(l)) => l,
        Ok(LineRead::TooLong) => {
            let _ = write_json_response(
                reader.into_inner(),
                414,
                "URI Too Long",
                br#"{"error":"request_line_too_long","code":"bad_request"}"#,
            );
            return;
        }
        _ => return,
    };
    let mut parts = req_line.split_whitespace();
    let Some(method) = parts.next() else { return };
    let Some(path) = parts.next() else { return };

    let mut content_length: usize = 0;
    let mut provided_token: Option<String> = None;
    let mut headers_terminated = false;

    for _ in 0..MAX_HEADER_LINES {
        match read_line_bounded(&mut reader, MAX_HEADER_LINE_BYTES) {
            Ok(LineRead::Eof) => {
                headers_terminated = true;
                break;
            }
            Ok(LineRead::TooLong) => {
                let _ = write_json_response(
                    reader.into_inner(),
                    431,
                    "Request Header Fields Too Large",
                    br#"{"error":"header_too_large","code":"bad_request"}"#,
                );
                return;
            }
            Ok(LineRead::Line(line)) => {
                if line == "\r\n" || line == "\n" {
                    headers_terminated = true;
                    break;
                }
                if let Some((k, v)) = line.split_once(':') {
                    let key = k.trim();
                    let val = v.trim();
                    if key.eq_ignore_ascii_case("content-length") {
                        match val.parse::<usize>() {
                            Ok(n) => content_length = n,
                            Err(_) => {
                                let _ = write_json_response(
                                    reader.into_inner(),
                                    400,
                                    "Bad Request",
                                    br#"{"error":"invalid_content_length","code":"bad_request"}"#,
                                );
                                return;
                            }
                        }
                    } else if key.eq_ignore_ascii_case("x-dhtms-token") {
                        provided_token = Some(val.to_string());
                    } else if key.eq_ignore_ascii_case("authorization") {
                        if let Some(tok) = val
                            .strip_prefix("Bearer ")
                            .or_else(|| val.strip_prefix("bearer "))
                        {
                            provided_token = Some(tok.trim().to_string());
                        }
                    }
                }
            }
            Err(_) => return,
        }
    }

    if !headers_terminated {
        let _ = write_json_response(
            reader.into_inner(),
            431,
            "Request Header Fields Too Large",
            br#"{"error":"too_many_headers","code":"bad_request"}"#,
        );
        return;
    }

    if content_length > MAX_BODY_BYTES {
        let _ = write_json_response(
            reader.into_inner(),
            413,
            "Payload Too Large",
            br#"{"error":"payload_too_large","code":"payload_too_large"}"#,
        );
        return;
    }

    let mut body = vec![0u8; content_length];
    if content_length > 0 && reader.read_exact(&mut body).is_err() {
        return;
    }

    let route_path = path.split('?').next().unwrap_or(path);
    if let Some(expected) = auth_token {
        if !expected.is_empty() && !(method == "GET" && route_path == "/v1/health") {
            let ok = provided_token
                .as_deref()
                .is_some_and(|got| ct_eq(got.as_bytes(), expected.as_bytes()));
            if !ok {
                let _ = write_json_response(
                    reader.into_inner(),
                    401,
                    "Unauthorized",
                    br#"{"error":"unauthorized","code":"unauthorized"}"#,
                );
                return;
            }
        }
    }

    let (status, reason, resp_body) = dispatch(engine, method, path, &body);
    let _ = write_json_response(reader.into_inner(), status, reason, &resp_body);
}

#[cfg(unix)]
pub fn serve_unix(
    engine: Arc<DaemonEngine>,
    socket_path: &Path,
    auth_token: Option<Arc<String>>,
    shutdown: Arc<AtomicBool>,
) -> io::Result<()> {
    if socket_path.exists() {
        let _ = std::fs::remove_file(socket_path);
    }
    if let Some(parent) = socket_path.parent() {
        std::fs::create_dir_all(parent)?;
    }
    let listener = UnixListener::bind(socket_path)?;
    std::fs::set_permissions(socket_path, std::fs::Permissions::from_mode(0o600))?;
    listener.set_nonblocking(true)?;
    let active_conns = Arc::new(AtomicUsize::new(0));
    while !shutdown.load(Ordering::Acquire) {
        match listener.accept() {
            Ok((mut stream, _)) => {
                let _ = stream.set_nonblocking(false);
                let _ = stream.set_read_timeout(Some(Duration::from_secs(10)));
                let _ = stream.set_write_timeout(Some(Duration::from_secs(10)));
                let Some(guard) = try_acquire_conn(&active_conns) else {
                    let _ = write_json_response(
                        &mut stream,
                        503,
                        "Service Unavailable",
                        br#"{"error":"server_busy","code":"server_busy"}"#,
                    );
                    continue;
                };
                let eng = Arc::clone(&engine);
                let tok = auth_token.clone();
                std::thread::spawn(move || {
                    let _guard = guard;
                    handle_connection(&eng, tok.as_deref().map(String::as_str), stream);
                });
            }
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                std::thread::sleep(Duration::from_millis(5));
            }
            Err(e) => return Err(e),
        }
    }
    let _ = std::fs::remove_file(socket_path);
    Ok(())
}

pub fn serve_tcp(
    engine: Arc<DaemonEngine>,
    addr: &str,
    auth_token: Option<Arc<String>>,
    shutdown: Arc<AtomicBool>,
) -> io::Result<()> {
    let listener = TcpListener::bind(addr)?;
    listener.set_nonblocking(true)?;
    let active_conns = Arc::new(AtomicUsize::new(0));
    while !shutdown.load(Ordering::Acquire) {
        match listener.accept() {
            Ok((mut stream, _)) => {
                let _ = stream.set_nonblocking(false);
                let _ = stream.set_nodelay(true);
                let _ = stream.set_read_timeout(Some(Duration::from_secs(10)));
                let _ = stream.set_write_timeout(Some(Duration::from_secs(10)));
                let Some(guard) = try_acquire_conn(&active_conns) else {
                    let _ = write_json_response(
                        &mut stream,
                        503,
                        "Service Unavailable",
                        br#"{"error":"server_busy","code":"server_busy"}"#,
                    );
                    continue;
                };
                let eng = Arc::clone(&engine);
                let tok = auth_token.clone();
                std::thread::spawn(move || {
                    let _guard = guard;
                    handle_connection(&eng, tok.as_deref().map(String::as_str), stream);
                });
            }
            Err(e) if e.kind() == io::ErrorKind::WouldBlock => {
                std::thread::sleep(Duration::from_millis(5));
            }
            Err(e) => return Err(e),
        }
    }
    Ok(())
}
