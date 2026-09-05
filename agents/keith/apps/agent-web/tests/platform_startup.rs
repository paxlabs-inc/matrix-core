use std::io::{Read, Write};
use std::net::TcpStream;
use std::path::PathBuf;
use std::time::Duration;

use keith_agent_web::{
    OpenAiCompatibilityConfig, PlatformCompatibilityConfig, WebServer, WebServerConfig,
};
use keith_credentials::MasterKey;

#[tokio::test(flavor = "multi_thread", worker_threads = 2)]
#[allow(clippy::too_many_lines)]
async fn platform_web_startup_serves_browser_and_guarded_compatibility_boundaries() {
    let directory = tempfile::tempdir().unwrap();
    let assets = PathBuf::from(env!("CARGO_MANIFEST_DIR")).join("static");
    assert!(assets.join("ui/index.html").is_file());
    let listener = tokio::net::TcpListener::bind("127.0.0.1:0").await.unwrap();
    let address = listener.local_addr().unwrap();
    let server = WebServer::new(WebServerConfig {
        bind: address,
        exact_origin: format!("http://{address}"),
        daemon_socket: PathBuf::from("platform-startup.sock"),
        asset_root: assets,
        credential_root: directory.path().join("credentials"),
        credential_key: MasterKey::from_bytes([0x31; 32]),
        login_secret: b"platform-login-secret".to_vec(),
        session_lifetime: Duration::from_secs(60),
        mutation_limit_per_second: 8,
        daemon_timeout: Duration::from_secs(1),
        openai_compatibility: Some(OpenAiCompatibilityConfig {
            api_key: b"platform-openai-compatibility-key".to_vec(),
            allow_non_loopback: false,
            max_in_flight: 2,
        }),
        platform_compatibility: Some(PlatformCompatibilityConfig {
            api_key: b"platform-native-compatibility-key".to_vec(),
            allow_non_loopback: false,
            max_in_flight: 2,
        }),
    })
    .unwrap();
    let task = tokio::spawn(server.serve_listener(listener));
    let response = request(
        address,
        b"GET /login HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n".to_vec(),
    )
    .await;
    assert!(response.starts_with("HTTP/1.1 200 OK"));
    assert!(response.contains("Opening Keith"));
    assert!(response.contains("/assets/ui/_next/static/"));
    assert!(!response.contains("agent_web_bg.wasm"));

    let unauthenticated = request(
        address,
        b"GET /v1/models HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n".to_vec(),
    )
    .await;
    assert!(unauthenticated.starts_with("HTTP/1.1 401 Unauthorized"));
    assert!(unauthenticated.contains("invalid_api_key"));

    let unavailable = request(
        address,
        b"GET /v1/models HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer platform-openai-compatibility-key\r\nConnection: close\r\n\r\n".to_vec(),
    )
    .await;
    assert!(unavailable.starts_with("HTTP/1.1 503 Service Unavailable"));
    assert!(unavailable.contains("keith_native_api_unavailable"));

    assert_native_platform_boundary(address).await;

    let advisory_body = br#"{"model":"keith","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"openwebui_search","description":"Search through the client UI","parameters":{"type":"object","properties":{"query":{"type":"string"}},"required":["query"]}}}],"tool_choice":"auto"}"#;
    let advisory = request(
        address,
        format!(
            "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer platform-openai-compatibility-key\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            advisory_body.len()
        )
        .into_bytes()
        .into_iter()
        .chain(advisory_body.iter().copied())
        .collect(),
    )
    .await;
    assert!(advisory.starts_with("HTTP/1.1 503 Service Unavailable"));
    assert!(advisory.contains("keith_native_api_unavailable"));

    let forced_body = br#"{"model":"keith","messages":[{"role":"user","content":"hello"}],"tools":[{"type":"function","function":{"name":"openwebui_search","parameters":{"type":"object"}}}],"tool_choice":"required"}"#;
    let forced = request(
        address,
        format!(
            "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer platform-openai-compatibility-key\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            forced_body.len()
        )
        .into_bytes()
        .into_iter()
        .chain(forced_body.iter().copied())
        .collect(),
    )
    .await;
    assert!(forced.starts_with("HTTP/1.1 400 Bad Request"));
    assert!(forced.contains("unsupported_feature"));

    let oversized_body = vec![b' '; 129 * 1024];
    let oversized = request(
        address,
        format!(
            "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer platform-openai-compatibility-key\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            oversized_body.len()
        )
        .into_bytes()
        .into_iter()
        .chain(oversized_body)
        .collect(),
    )
    .await;
    task.abort();
    let _ = task.await;
    assert!(oversized.starts_with("HTTP/1.1 413 Payload Too Large"));
    assert!(oversized.contains("request_too_large"));
}

async fn assert_native_platform_boundary(address: std::net::SocketAddr) {
    let unauthenticated = request(
        address,
        b"GET /platform/v1/catalog HTTP/1.1\r\nHost: 127.0.0.1\r\nConnection: close\r\n\r\n"
            .to_vec(),
    )
    .await;
    assert!(unauthenticated.starts_with("HTTP/1.1 401 Unauthorized"));
    assert!(unauthenticated.contains("authentication_error"));

    let unavailable = request(
        address,
        b"GET /platform/v1/catalog HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer platform-native-compatibility-key\r\nConnection: close\r\n\r\n".to_vec(),
    )
    .await;
    assert!(unavailable.starts_with("HTTP/1.1 503 Service Unavailable"));
    assert!(unavailable.contains("keith_unavailable"));

    let oversized_body = vec![b' '; 257 * 1024];
    let oversized = request(
        address,
        format!(
            "POST /platform/v1/profiles/profile-a/commands HTTP/1.1\r\nHost: 127.0.0.1\r\nAuthorization: Bearer platform-native-compatibility-key\r\nContent-Type: application/json\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
            oversized_body.len()
        )
        .into_bytes()
        .into_iter()
        .chain(oversized_body)
        .collect(),
    )
    .await;
    assert!(oversized.starts_with("HTTP/1.1 413 Payload Too Large"));
    assert!(oversized.contains("payload_too_large"));
}

async fn request(address: std::net::SocketAddr, request: Vec<u8>) -> String {
    tokio::task::spawn_blocking(move || {
        let mut stream = TcpStream::connect(address).unwrap();
        stream.write_all(&request).unwrap();
        let mut response = String::new();
        stream.read_to_string(&mut response).unwrap();
        response
    })
    .await
    .unwrap()
}
