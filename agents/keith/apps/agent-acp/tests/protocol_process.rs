use std::io::{BufRead, BufReader, Read, Write};
use std::net::{SocketAddr, TcpListener, TcpStream};
use std::process::{Child, Command, Stdio};
use std::thread;
use std::time::{Duration, Instant};

use keith_agent_types::{ProfileId, WorkspaceId};
use serde_json::{Value, json};
use tungstenite::client::IntoClientRequest as _;
use tungstenite::http::header::AUTHORIZATION;
use tungstenite::{Message, connect};

const MANAGED_TOKEN: &str = "keith-managed-test-token-with-more-than-thirty-two-bytes";
const MANAGED_TOKEN_ENVIRONMENT: &str = "KEITH_ACP_MANAGED_TEST_BEARER";

struct ManagedProcess {
    child: Child,
    address: SocketAddr,
    _directory: tempfile::TempDir,
}

impl Drop for ManagedProcess {
    fn drop(&mut self) {
        let _ = self.child.kill();
        let _ = self.child.wait();
    }
}

fn run_protocol(lines: &[Value]) -> Vec<Value> {
    run_protocol_lines(&lines.iter().map(Value::to_string).collect::<Vec<_>>())
}

fn run_protocol_lines(lines: &[String]) -> Vec<Value> {
    run_protocol_lines_with_options(lines, &[])
}

fn run_protocol_lines_with_options(lines: &[String], options: &[&str]) -> Vec<Value> {
    let directory = tempfile::tempdir().expect("temporary ACP process root");
    let state_root = directory.path().join("state");
    let socket = directory.path().join("missing-agentd.sock");
    let mut child = Command::new(env!("CARGO_BIN_EXE_keith-agent-acp"))
        .arg("--socket")
        .arg(socket)
        .arg("--state-root")
        .arg(state_root)
        .arg("--profile")
        .arg(ProfileId::new().to_string())
        .arg("--workspace")
        .arg(WorkspaceId::new().to_string())
        .arg("--workspace-root")
        .arg(env!("CARGO_MANIFEST_DIR"))
        .args(options)
        .stdin(Stdio::piped())
        .stdout(Stdio::piped())
        .stderr(Stdio::piped())
        .spawn()
        .expect("spawn real ACP agent process");
    {
        let mut stdin = child.stdin.take().expect("ACP stdin");
        for line in lines {
            stdin
                .write_all(line.as_bytes())
                .expect("write protocol fixture");
            stdin.write_all(b"\n").expect("terminate protocol frame");
        }
    }
    let output = child
        .wait_with_output()
        .expect("wait for ACP agent process");
    assert!(
        output.status.success(),
        "ACP process failed: {}",
        String::from_utf8_lossy(&output.stderr)
    );
    BufReader::new(output.stdout.as_slice())
        .lines()
        .map(|line| {
            serde_json::from_str(&line.expect("read ACP response")).expect("valid JSON response")
        })
        .collect()
}

fn spawn_managed_process() -> ManagedProcess {
    let reservation = TcpListener::bind("127.0.0.1:0").expect("reserve managed ACP port");
    let address = reservation.local_addr().expect("managed ACP port");
    drop(reservation);
    let directory = tempfile::tempdir().expect("temporary managed ACP process root");
    let process_root = directory.path();
    let child = Command::new(env!("CARGO_BIN_EXE_keith-agent-acp"))
        .arg("--socket")
        .arg(process_root.join("missing-agentd.sock"))
        .arg("--state-root")
        .arg(process_root.join("state"))
        .arg("--profile")
        .arg(ProfileId::new().to_string())
        .arg("--workspace")
        .arg(WorkspaceId::new().to_string())
        .arg("--workspace-root")
        .arg(env!("CARGO_MANIFEST_DIR"))
        .arg("--transport")
        .arg("managed")
        .arg("--listen")
        .arg(address.to_string())
        .arg("--bearer-token-env")
        .arg(MANAGED_TOKEN_ENVIRONMENT)
        .env(MANAGED_TOKEN_ENVIRONMENT, MANAGED_TOKEN)
        .stdin(Stdio::null())
        .stdout(Stdio::null())
        .stderr(Stdio::null())
        .spawn()
        .expect("spawn real managed ACP agent process");
    let deadline = Instant::now() + Duration::from_secs(10);
    while TcpStream::connect(address).is_err() {
        assert!(
            Instant::now() < deadline,
            "managed ACP listener did not start"
        );
        thread::sleep(Duration::from_millis(20));
    }
    ManagedProcess {
        child,
        address,
        _directory: directory,
    }
}

fn http_request(
    address: SocketAddr,
    method: &str,
    path: &str,
    bearer: Option<&str>,
    body: &str,
) -> String {
    let mut stream = TcpStream::connect(address).expect("connect managed ACP HTTP endpoint");
    let authorization = bearer.map_or_else(String::new, |token| {
        format!("Authorization: Bearer {token}\r\n")
    });
    write!(
        stream,
        "{method} {path} HTTP/1.1\r\nHost: {address}\r\n{authorization}Content-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n{body}",
        body.len()
    )
    .expect("write managed ACP HTTP request");
    let mut response = String::new();
    stream
        .read_to_string(&mut response)
        .expect("read managed ACP HTTP response");
    response
}

fn read_sse_replay(address: SocketAddr, path: &str) -> String {
    let mut stream = TcpStream::connect(address).expect("connect managed ACP SSE endpoint");
    write!(
        stream,
        "GET {path} HTTP/1.1\r\nHost: {address}\r\nAuthorization: Bearer {MANAGED_TOKEN}\r\nLast-Event-ID: 0\r\nConnection: close\r\n\r\n"
    )
    .expect("write managed ACP SSE request");
    stream
        .set_read_timeout(Some(Duration::from_secs(10)))
        .expect("set SSE timeout");
    let mut response = Vec::new();
    let mut chunk = [0_u8; 4096];
    loop {
        let count = stream.read(&mut chunk).expect("read managed ACP SSE event");
        if count == 0 {
            break;
        }
        response.extend_from_slice(&chunk[..count]);
        let text = String::from_utf8_lossy(&response);
        if text.contains("data: ") && text.contains("\n\n") {
            break;
        }
    }
    String::from_utf8(response).expect("UTF-8 managed ACP SSE response")
}

#[test]
fn real_process_reports_malformed_json_rpc_without_losing_prior_responses() {
    let responses = run_protocol_lines(&[
        json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": { "protocolVersion": 1 }
        })
        .to_string(),
        "{not-json".to_owned(),
    ]);
    assert_eq!(responses.len(), 2);
    assert_eq!(responses[0]["id"], 1);
    assert_eq!(responses[0]["result"]["protocolVersion"], 1);
    assert!(responses[1]["id"].is_null());
    assert_eq!(responses[1]["error"]["code"], -32700);
}

#[test]
fn real_process_negotiates_v1_and_handles_json_rpc_batches() {
    let responses = run_protocol(&[
        json!({
            "jsonrpc": "2.0",
            "id": 1,
            "method": "initialize",
            "params": { "protocolVersion": 1 }
        }),
        json!([
            {
                "jsonrpc": "2.0",
                "method": "session/cancel",
                "params": { "sessionId": "unknown-session" }
            },
            {
                "jsonrpc": "2.0",
                "id": 2,
                "method": "keith/unknown",
                "params": {}
            }
        ]),
    ]);
    assert_eq!(responses.len(), 2);
    assert_eq!(responses[0]["id"], 1);
    assert_eq!(responses[0]["result"]["protocolVersion"], 1);
    assert_eq!(responses[0]["result"]["agentInfo"]["name"], "keith");
    assert!(responses[0]["result"]["agentCapabilities"]["sessionCapabilities"]["fork"].is_object());
    let batch = responses[1].as_array().expect("batch response array");
    assert_eq!(batch.len(), 1);
    assert_eq!(batch[0]["id"], 2);
    assert_eq!(batch[0]["error"]["code"], -32601);
}

#[test]
fn real_process_refuses_unsupported_protocol_versions() {
    let responses = run_protocol(&[json!({
        "jsonrpc": "2.0",
        "id": 7,
        "method": "initialize",
        "params": { "protocolVersion": 2 }
    })]);
    assert_eq!(responses.len(), 1);
    assert_eq!(responses[0]["id"], 7);
    assert_eq!(responses[0]["error"]["code"], -32602);
}

#[test]
fn managed_http_sse_authenticates_replays_and_closes_a_real_connection() {
    let process = spawn_managed_process();
    let unauthorized = http_request(process.address, "PUT", "/acp/sse/integration", None, "");
    assert!(unauthorized.starts_with("HTTP/1.1 401"));

    let created = http_request(
        process.address,
        "PUT",
        "/acp/sse/integration",
        Some(MANAGED_TOKEN),
        "",
    );
    assert!(created.starts_with("HTTP/1.1 201"), "{created}");
    let initialize = json!({
        "jsonrpc": "2.0",
        "id": 41,
        "method": "initialize",
        "params": { "protocolVersion": 1 }
    })
    .to_string();
    let accepted = http_request(
        process.address,
        "POST",
        "/acp/sse/integration/messages",
        Some(MANAGED_TOKEN),
        &initialize,
    );
    assert!(accepted.starts_with("HTTP/1.1 202"), "{accepted}");

    let first_replay = read_sse_replay(process.address, "/acp/sse/integration/events");
    assert!(first_replay.starts_with("HTTP/1.1 200"), "{first_replay}");
    assert!(first_replay.contains("id: 1"));
    assert!(first_replay.contains("\"id\":41"));
    assert!(first_replay.contains("\"protocolVersion\":1"));
    let second_replay = read_sse_replay(process.address, "/acp/sse/integration/events");
    assert!(second_replay.contains("id: 1"));
    assert!(second_replay.contains("\"id\":41"));

    let closed = http_request(
        process.address,
        "DELETE",
        "/acp/sse/integration",
        Some(MANAGED_TOKEN),
        "",
    );
    assert!(closed.starts_with("HTTP/1.1 204"), "{closed}");
    let after_close = http_request(
        process.address,
        "POST",
        "/acp/sse/integration/messages",
        Some(MANAGED_TOKEN),
        &initialize,
    );
    assert!(after_close.starts_with("HTTP/1.1 404"), "{after_close}");
}

#[test]
fn managed_websocket_preserves_frames_and_refuses_versions_without_downgrade() {
    let process = spawn_managed_process();
    let endpoint = format!("ws://{}/acp/ws", process.address);
    let mut request = endpoint.clone().into_client_request().unwrap();
    request.headers_mut().insert(
        AUTHORIZATION,
        format!("Bearer {MANAGED_TOKEN}").parse().unwrap(),
    );
    let (mut websocket, _) = connect(request).expect("connect authenticated managed ACP WS");
    websocket
        .send(Message::Text(
            json!({
                "jsonrpc": "2.0",
                "id": 51,
                "method": "initialize",
                "params": { "protocolVersion": 1 }
            })
            .to_string()
            .into(),
        ))
        .unwrap();
    let response = websocket.read().unwrap().into_text().unwrap();
    let response: Value = serde_json::from_str(&response).unwrap();
    assert_eq!(response["id"], 51);
    assert_eq!(response["result"]["protocolVersion"], 1);
    websocket.close(None).unwrap();

    let mut request = endpoint.into_client_request().unwrap();
    request.headers_mut().insert(
        AUTHORIZATION,
        format!("Bearer {MANAGED_TOKEN}").parse().unwrap(),
    );
    let (mut websocket, _) = connect(request).expect("connect second managed ACP WS");
    websocket
        .send(Message::Text(
            json!({
                "jsonrpc": "2.0",
                "id": 52,
                "method": "initialize",
                "params": { "protocolVersion": 2 }
            })
            .to_string()
            .into(),
        ))
        .unwrap();
    let response = websocket.read().unwrap().into_text().unwrap();
    let response: Value = serde_json::from_str(&response).unwrap();
    assert_eq!(response["id"], 52);
    assert_eq!(response["error"]["code"], -32602);
}

#[cfg(feature = "unstable-acp-v2")]
#[test]
fn draft_v2_is_a_separate_runtime_gated_process_handler() {
    let responses = run_protocol_lines_with_options(
        &[
            json!({
                "jsonrpc": "2.0",
                "id": 61,
                "method": "initialize",
                "params": {
                    "protocolVersion": 2,
                    "info": { "name": "keith-v2-test", "version": "1" },
                    "capabilities": {}
                }
            })
            .to_string(),
            json!({
                "jsonrpc": "2.0",
                "id": 62,
                "method": "session/list",
                "params": {}
            })
            .to_string(),
        ],
        &["--unstable-acp-v2", "true"],
    );
    assert_eq!(responses.len(), 2);
    assert_eq!(responses[0]["id"], 61);
    assert_eq!(responses[0]["result"]["protocolVersion"], 2);
    assert_eq!(
        responses[0]["result"]["_meta"]["keith"]["protocol"],
        "acp/v2-draft"
    );
    assert_eq!(responses[1]["id"], 62);
    assert_eq!(responses[1]["result"]["sessions"], json!([]));
}
