use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::thread::{self, JoinHandle};
use std::time::Duration;

use keith_agent_types::ArtifactId;
use keith_channel_core::{
    AdapterCapability, AdapterEvent, ChannelAdapter, ChannelAdapterV2, ChannelCapabilityV2,
    ChannelEventKindV2, ChannelReceiptStateV2, InboundIntent, OutboundMessage, RetryClass,
};
use keith_credentials::SecretValue;

#[allow(dead_code)]
#[path = "../src/whatsapp.rs"]
mod whatsapp;

use whatsapp::{
    WhatsAppCloudAdapter, WhatsAppCloudConfig, WhatsAppCursor, WhatsAppDeliveryState,
    WhatsAppTemplate, WhatsAppUpload, WhatsAppUploadKind,
};

const WEBHOOK_BODY: &[u8] = br#"{"object":"whatsapp_business_account","entry":[{"id":"waba-1","changes":[{"field":"messages","value":{"messaging_product":"whatsapp","metadata":{"display_phone_number":"15550001111","phone_number_id":"phone-1"},"messages":[{"from":"15551234567","id":"wamid.inbound-1","timestamp":"1700000000","type":"audio","context":{"from":"15550001111","id":"wamid.parent"},"audio":{"id":"media-1","mime_type":"audio/ogg; codecs=opus","sha256":"media-sha","voice":true}}],"statuses":[{"id":"wamid.outbound-1","recipient_id":"15551234567","status":"delivered","timestamp":"1700000001"}]}}]}]}"#;

#[derive(Debug)]
struct Request {
    path: String,
    headers: String,
    body: Vec<u8>,
}

struct Response {
    status: u16,
    content_type: &'static str,
    body: Vec<u8>,
}

fn secret(value: &str) -> SecretValue {
    SecretValue::new(value.as_bytes().to_vec()).expect("valid test secret")
}

fn config() -> WhatsAppCloudConfig {
    WhatsAppCloudConfig::production("v99.0", "waba-1", "phone-1")
}

fn signature(secret: &[u8], body: &[u8]) -> String {
    whatsapp::webhook_signature(secret, body)
}

fn serve(
    build_responses: impl FnOnce(&str) -> Vec<Response>,
) -> (String, JoinHandle<Vec<Request>>) {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind Graph HTTP boundary");
    let address = listener.local_addr().expect("Graph HTTP boundary address");
    let api_base = format!("http://{address}");
    let responses = build_responses(&api_base);
    let handle = thread::spawn(move || {
        let mut requests = Vec::new();
        for response in responses {
            let (mut stream, _) = listener.accept().expect("accept Graph request");
            let request = read_request(&mut stream);
            let reason = if response.status == 200 {
                "OK"
            } else {
                "Too Many Requests"
            };
            write!(
                stream,
                "HTTP/1.1 {} {reason}\r\nContent-Type: {}\r\nContent-Length: {}\r\nConnection: close\r\n\r\n",
                response.status,
                response.content_type,
                response.body.len()
            )
            .expect("write Graph response headers");
            stream
                .write_all(&response.body)
                .expect("write Graph response body");
            requests.push(request);
        }
        requests
    });
    (api_base, handle)
}

fn read_request(stream: &mut TcpStream) -> Request {
    stream
        .set_read_timeout(Some(Duration::from_secs(2)))
        .expect("request timeout");
    let mut bytes = Vec::new();
    let mut buffer = [0_u8; 8_192];
    let header_end = loop {
        let read = stream.read(&mut buffer).expect("read Graph request");
        assert!(read > 0, "Graph request closed before headers");
        bytes.extend_from_slice(&buffer[..read]);
        if let Some(index) = bytes.windows(4).position(|window| window == b"\r\n\r\n") {
            break index + 4;
        }
    };
    let headers = String::from_utf8(bytes[..header_end].to_vec()).expect("UTF-8 headers");
    let content_length = headers
        .lines()
        .find_map(|line| {
            let (name, value) = line.split_once(':')?;
            name.eq_ignore_ascii_case("content-length")
                .then(|| value.trim().parse::<usize>().expect("content length"))
        })
        .unwrap_or(0);
    while bytes.len().saturating_sub(header_end) < content_length {
        let read = stream.read(&mut buffer).expect("read Graph request body");
        assert!(read > 0, "Graph request closed before body");
        bytes.extend_from_slice(&buffer[..read]);
    }
    let path = headers
        .lines()
        .next()
        .and_then(|line| line.split_ascii_whitespace().nth(1))
        .expect("request path")
        .to_owned();
    Request {
        path,
        headers,
        body: bytes[header_end..header_end + content_length].to_vec(),
    }
}

#[test]
fn whatsapp_rejects_graph_path_injection_before_network_io() {
    let mut unsafe_account = config();
    unsafe_account.phone_number_id = "phone-1/../../me?fields=token".to_owned();
    let result = WhatsAppCloudAdapter::new(
        unsafe_account,
        secret("access-token"),
        secret("app-secret"),
        secret("verify-token"),
        WhatsAppCursor::default(),
    );
    let Err(failure) = result else {
        panic!("unsafe phone identity must be rejected");
    };
    assert_eq!(failure.class, RetryClass::Permanent);
    assert_eq!(
        failure.safe_message,
        "invalid WhatsApp Cloud adapter configuration"
    );

    let adapter = WhatsAppCloudAdapter::new(
        config(),
        secret("access-token"),
        secret("app-secret"),
        secret("verify-token"),
        WhatsAppCursor::default(),
    )
    .expect("WhatsApp adapter");
    let failure = adapter
        .download_media("media-1/../../me?fields=token")
        .expect_err("unsafe media identity must be rejected before network I/O");
    assert_eq!(failure.class, RetryClass::Permanent);
    assert_eq!(failure.safe_message, "WhatsApp media identity is unsafe");
}

#[test]
#[allow(clippy::too_many_lines)]
fn whatsapp_verified_webhook_normalizes_voice_reply_status_restart_and_isolation() {
    let webhook_signature = signature(b"app-secret", WEBHOOK_BODY);
    let mut adapter = WhatsAppCloudAdapter::new(
        config(),
        secret("access-token"),
        secret("app-secret"),
        secret("verify-token"),
        WhatsAppCursor::default(),
    )
    .expect("WhatsApp adapter");
    assert_eq!(
        adapter.verify_challenge("subscribe", b"verify-token", "challenge-42"),
        Some("challenge-42")
    );
    assert_eq!(
        adapter.verify_challenge("subscribe", b"wrong-token", "challenge-42"),
        None
    );

    let outcome = adapter
        .ingest_webhook(&webhook_signature, WEBHOOK_BODY)
        .expect("verified WhatsApp webhook");
    assert_eq!(outcome.messages, 1);
    assert_eq!(outcome.statuses, 1);
    let AdapterEvent::Inbound(message) = adapter.receive().expect("WhatsApp inbound") else {
        panic!("expected WhatsApp inbound event");
    };
    assert_eq!(message.channel, "whatsapp_cloud");
    assert_eq!(message.external_account, "phone-1");
    assert_eq!(message.conversation, "15551234567");
    assert_eq!(message.sender, "15551234567");
    assert_eq!(message.reply_target.as_deref(), Some("wamid.parent"));
    assert_eq!(message.intent, InboundIntent::Prompt);
    assert_eq!(message.attachments[0].id, "media-1");
    assert_eq!(message.attachments[0].sha256.as_deref(), Some("media-sha"));
    assert!(message.attachments[0].download_url.is_none());
    let status = adapter.take_status().expect("WhatsApp delivery status");
    assert_eq!(status.message_id, "wamid.outbound-1");
    assert_eq!(status.state, WhatsAppDeliveryState::Delivered);
    assert!(
        adapter
            .features()
            .capabilities
            .contains(&AdapterCapability::Attachments)
    );
    let capabilities = adapter.capabilities_v2();
    capabilities.validate().expect("WhatsApp v2 capabilities");
    assert!(capabilities.supports(ChannelCapabilityV2::Voice));
    assert!(capabilities.supports(ChannelCapabilityV2::ReadReceipts));
    assert!(!capabilities.supports(ChannelCapabilityV2::Typing));
    let created = adapter.receive_v2().expect("WhatsApp v2 message");
    assert!(matches!(
        created.event,
        ChannelEventKindV2::MessageCreated(_)
    ));
    assert!(adapter.reconnect_cursor_v2().is_some());
    let receipt = adapter.receive_v2().expect("WhatsApp v2 receipt");
    assert!(matches!(
        receipt.event,
        ChannelEventKindV2::Receipt {
            state: ChannelReceiptStateV2::Delivered,
            ..
        }
    ));

    let duplicate = adapter
        .ingest_webhook(&webhook_signature, WEBHOOK_BODY)
        .expect("duplicate WhatsApp webhook");
    assert_eq!(duplicate.duplicates, 2);
    let cursor = adapter.cursor().clone();
    let mut restarted = WhatsAppCloudAdapter::new(
        config(),
        secret("access-token"),
        secret("app-secret"),
        secret("verify-token"),
        cursor,
    )
    .expect("restarted WhatsApp adapter");
    let duplicate = restarted
        .ingest_webhook(&webhook_signature, WEBHOOK_BODY)
        .expect("restart duplicate");
    assert_eq!(duplicate.duplicates, 2);

    let mut other_config = config();
    other_config.phone_number_id = "phone-other".to_owned();
    let mut isolated = WhatsAppCloudAdapter::new(
        other_config,
        secret("other-access-token"),
        secret("app-secret"),
        secret("other-verify-token"),
        WhatsAppCursor::default(),
    )
    .expect("isolated WhatsApp adapter");
    let isolation = isolated
        .ingest_webhook(&webhook_signature, WEBHOOK_BODY)
        .expect_err("wrong phone number must be isolated");
    assert_eq!(isolation.class, RetryClass::Permanent);
    assert_eq!(
        isolation.safe_message,
        "WhatsApp webhook belongs to another phone number"
    );

    let authentication = restarted
        .ingest_webhook(
            "sha256=0000000000000000000000000000000000000000000000000000000000000000",
            b"not-json",
        )
        .expect_err("signature must fail before JSON parsing");
    assert_eq!(
        authentication.safe_message,
        "WhatsApp webhook authentication failed"
    );
    assert!(!authentication.safe_message.contains("app-secret"));
    let malformed_body = b"not-json";
    let malformed_signature = signature(b"app-secret", malformed_body);
    let malformed = restarted
        .ingest_webhook(&malformed_signature, malformed_body)
        .expect_err("verified malformed event must be rejected");
    assert_eq!(malformed.safe_message, "WhatsApp webhook body is malformed");
}

#[test]
#[ignore = "requires loopback networking; run during network-enabled qualification"]
#[allow(clippy::too_many_lines)]
fn whatsapp_text_media_template_receipt_download_rotation_and_health_use_real_http() {
    let (api_base, server) = serve(|base| {
        vec![
            Response {
                status: 200,
                content_type: "application/json",
                body: br#"{"messaging_product":"whatsapp","contacts":[{"input":"15551234567","wa_id":"15551234567"}],"messages":[{"id":"wamid.text"}]}"#
                    .to_vec(),
            },
            Response {
                status: 200,
                content_type: "application/json",
                body: br#"{"id":"uploaded-media-1"}"#.to_vec(),
            },
            Response {
                status: 200,
                content_type: "application/json",
                body: br#"{"messaging_product":"whatsapp","messages":[{"id":"wamid.media"}]}"#
                    .to_vec(),
            },
            Response {
                status: 200,
                content_type: "application/json",
                body: br#"{"messaging_product":"whatsapp","messages":[{"id":"wamid.template"}]}"#
                    .to_vec(),
            },
            Response {
                status: 200,
                content_type: "application/json",
                body: br#"{"success":true}"#.to_vec(),
            },
            Response {
                status: 200,
                content_type: "application/json",
                body: br#"{"id":"phone-1","display_phone_number":"15550001111"}"#.to_vec(),
            },
            Response {
                status: 200,
                content_type: "application/json",
                body: format!(
                    "{{\"url\":\"{base}/media-download\",\"mime_type\":\"image/jpeg\",\"sha256\":\"download-sha\",\"file_size\":10}}"
                )
                .into_bytes(),
            },
            Response {
                status: 200,
                content_type: "image/jpeg",
                body: b"image-data".to_vec(),
            },
        ]
    });
    let mut adapter_config = config();
    adapter_config.api_base = api_base;
    adapter_config.timeout_ms = 2_000;
    let mut adapter = WhatsAppCloudAdapter::new(
        adapter_config,
        secret("access-token-old"),
        secret("app-secret"),
        secret("verify-token"),
        WhatsAppCursor::default(),
    )
    .expect("WhatsApp HTTP adapter");
    adapter.rotate_access_token(secret("access-token-new"));
    let route = keith_channel_core::ReplyRoute {
        channel: "whatsapp_cloud".to_owned(),
        external_account: "phone-1".to_owned(),
        conversation: "15551234567".to_owned(),
        thread: None,
        reply_to_message: Some("wamid.inbound".to_owned()),
    };

    let text_receipt = adapter
        .send(&OutboundMessage {
            route: route.clone(),
            idempotency_key: "outbox-text".to_owned(),
            text: "hello from Keith".to_owned(),
            artifacts: Vec::new(),
        })
        .expect("WhatsApp text send");
    assert_eq!(text_receipt.platform_message_id, "wamid.text");
    assert!(text_receipt.duplicate_possible);

    let artifact_id = ArtifactId::new();
    adapter
        .stage_artifact(
            artifact_id.clone(),
            WhatsAppUpload {
                kind: WhatsAppUploadKind::Image,
                file_name: "result.jpg".to_owned(),
                media_type: "image/jpeg".to_owned(),
                bytes: b"outbound-image".to_vec(),
            },
        )
        .expect("stage WhatsApp media");
    let media_receipt = adapter
        .send(&OutboundMessage {
            route: route.clone(),
            idempotency_key: "outbox-media".to_owned(),
            text: "result".to_owned(),
            artifacts: vec![artifact_id],
        })
        .expect("WhatsApp media send");
    assert_eq!(media_receipt.platform_message_id, "wamid.media");

    let template_receipt = adapter
        .send_template(
            &route,
            &WhatsAppTemplate {
                name: "appointment_reminder".to_owned(),
                language_code: "en_US".to_owned(),
                components: vec![serde_json::json!({
                    "type": "body",
                    "parameters": [{"type": "text", "text": "Tuesday"}]
                })],
            },
        )
        .expect("WhatsApp template send");
    assert_eq!(template_receipt.platform_message_id, "wamid.template");
    adapter
        .mark_read("wamid.inbound")
        .expect("WhatsApp read receipt");
    adapter.reconnect().expect("WhatsApp health reconnect");
    let download = adapter
        .download_media("media-download-id")
        .expect("WhatsApp media download");
    assert_eq!(download.media_type, "image/jpeg");
    assert_eq!(download.sha256.as_deref(), Some("download-sha"));
    assert_eq!(download.bytes, b"image-data");

    let requests = server.join().expect("Graph HTTP completion");
    assert!(
        requests
            .iter()
            .all(|request| request.headers.contains("Bearer access-token-new"))
    );
    assert_eq!(requests[0].path, "/v99.0/phone-1/messages");
    assert!(String::from_utf8_lossy(&requests[0].body).contains("wamid.inbound"));
    assert_eq!(requests[1].path, "/v99.0/phone-1/media");
    assert!(
        requests[1]
            .headers
            .to_ascii_lowercase()
            .contains("multipart/form-data")
    );
    assert!(
        requests[1]
            .body
            .windows(b"outbound-image".len())
            .any(|window| window == b"outbound-image")
    );
    assert_eq!(requests[2].path, "/v99.0/phone-1/messages");
    assert!(String::from_utf8_lossy(&requests[2].body).contains("uploaded-media-1"));
    assert!(String::from_utf8_lossy(&requests[3].body).contains("appointment_reminder"));
    assert!(String::from_utf8_lossy(&requests[4].body).contains("\"status\":\"read\""));
    assert_eq!(requests[5].path, "/v99.0/phone-1");
    assert_eq!(requests[6].path, "/v99.0/media-download-id");
    assert_eq!(requests[7].path, "/media-download");
}

#[test]
#[ignore = "requires loopback networking; run during network-enabled qualification"]
fn whatsapp_rate_limit_and_unsupported_routes_are_truthful() {
    let (api_base, server) = serve(|_| {
        vec![Response {
            status: 429,
            content_type: "application/json",
            body: br#"{"error":{"message":"rate limited","type":"OAuthException","code":130429,"is_transient":true}}"#
                .to_vec(),
        }]
    });
    let mut adapter_config = config();
    adapter_config.api_base = api_base;
    let mut adapter = WhatsAppCloudAdapter::new(
        adapter_config,
        secret("access-token"),
        secret("app-secret"),
        secret("verify-token"),
        WhatsAppCursor::default(),
    )
    .expect("WhatsApp adapter");
    let failure = adapter
        .send(&OutboundMessage {
            route: keith_channel_core::ReplyRoute {
                channel: "whatsapp_cloud".to_owned(),
                external_account: "phone-1".to_owned(),
                conversation: "15551234567".to_owned(),
                thread: None,
                reply_to_message: None,
            },
            idempotency_key: "outbox-rate".to_owned(),
            text: "rate test".to_owned(),
            artifacts: Vec::new(),
        })
        .expect_err("WhatsApp rate limit");
    assert_eq!(failure.class, RetryClass::RateLimited);
    assert!(!failure.safe_message.contains("access-token"));
    server.join().expect("rate-limit boundary");

    let threaded = keith_channel_core::ReplyRoute {
        channel: "whatsapp_cloud".to_owned(),
        external_account: "phone-1".to_owned(),
        conversation: "15551234567".to_owned(),
        thread: Some("not-supported".to_owned()),
        reply_to_message: None,
    };
    let failure = adapter
        .send_template(
            &threaded,
            &WhatsAppTemplate {
                name: "template".to_owned(),
                language_code: "en_US".to_owned(),
                components: Vec::new(),
            },
        )
        .expect_err("WhatsApp threads are unsupported");
    assert_eq!(failure.class, RetryClass::Permanent);
}
