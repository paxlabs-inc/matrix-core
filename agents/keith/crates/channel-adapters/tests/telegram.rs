use std::io::{Read, Write};
use std::net::{TcpListener, TcpStream};
use std::thread::{self, JoinHandle};
use std::time::Duration;

use keith_agent_types::ArtifactId;
use keith_channel_core::{
    AdapterCapability, AdapterEvent, ChannelAdapter, ChannelAdapterV2, ChannelCapabilityV2,
    ChannelEventKindV2, InboundIntent, OutboundMessage, RetryClass,
};
use keith_credentials::SecretValue;

#[allow(dead_code)]
#[path = "../src/telegram.rs"]
mod telegram;

use telegram::{
    TelegramAdapter, TelegramConfig, TelegramCursor, TelegramIngress, TelegramUpload,
    TelegramUploadKind, TelegramWebhookOutcome,
};

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

fn webhook_config(account: &str) -> TelegramConfig {
    TelegramConfig::production(account, TelegramIngress::Webhook)
}

fn serve(responses: Vec<Response>) -> (String, JoinHandle<Vec<Request>>) {
    let listener = TcpListener::bind("127.0.0.1:0").expect("bind HTTP boundary");
    let address = listener.local_addr().expect("HTTP boundary address");
    let handle = thread::spawn(move || {
        let mut requests = Vec::new();
        for response in responses {
            let (mut stream, _) = listener.accept().expect("accept HTTP request");
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
            .expect("write response headers");
            stream
                .write_all(&response.body)
                .expect("write response body");
            requests.push(request);
        }
        requests
    });
    (format!("http://{address}"), handle)
}

fn read_request(stream: &mut TcpStream) -> Request {
    stream
        .set_read_timeout(Some(Duration::from_secs(2)))
        .expect("request timeout");
    let mut bytes = Vec::new();
    let mut buffer = [0_u8; 8_192];
    let header_end = loop {
        let read = stream.read(&mut buffer).expect("read HTTP request");
        assert!(read > 0, "HTTP request closed before headers");
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
        let read = stream.read(&mut buffer).expect("read HTTP request body");
        assert!(read > 0, "HTTP request closed before body");
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
fn telegram_rejects_bot_tokens_that_can_escape_the_api_path() {
    let result = TelegramAdapter::new(
        webhook_config("bot-42"),
        secret("123:token/../../getMe?leak=1"),
        secret("webhook_secret-42"),
        TelegramCursor::default(),
    );
    let Err(failure) = result else {
        panic!("unsafe bot token must be rejected before network I/O");
    };

    assert_eq!(failure.class, RetryClass::Permanent);
    assert_eq!(
        failure.safe_message,
        "invalid Telegram adapter configuration"
    );
    assert!(!failure.safe_message.contains("token/../../"));
}

#[test]
#[allow(clippy::too_many_lines)]
fn telegram_verified_webhook_normalizes_topics_voice_commands_and_deduplicates() {
    let body = br#"{
        "update_id": 9001,
        "message": {
            "message_id": 77,
            "message_thread_id": 12,
            "date": 1700000000,
            "chat": {"id": -10042, "type": "supergroup"},
            "from": {"id": 31415, "is_bot": false, "first_name": "Ada"},
            "reply_to_message": {"message_id": 70},
            "text": "/steer examine this",
            "voice": {
                "file_id": "voice-file-id",
                "file_unique_id": "voice-unique",
                "duration": 2,
                "mime_type": "audio/ogg",
                "file_size": 120
            }
        }
    }"#;
    let mut adapter = TelegramAdapter::new(
        webhook_config("bot-42"),
        secret("123:bot-token"),
        secret("webhook_secret-42"),
        TelegramCursor::default(),
    )
    .expect("Telegram adapter");

    assert_eq!(
        adapter
            .ingest_webhook(b"webhook_secret-42", body)
            .expect("verified webhook"),
        TelegramWebhookOutcome::Enqueued
    );
    let AdapterEvent::Inbound(message) = adapter.receive().expect("normalized inbound") else {
        panic!("expected Telegram inbound event");
    };
    assert_eq!(message.channel, "telegram");
    assert_eq!(message.external_account, "bot-42");
    assert_eq!(message.conversation, "-10042");
    assert_eq!(message.thread.as_deref(), Some("12"));
    assert_eq!(message.sender, "31415");
    assert_eq!(message.reply_target.as_deref(), Some("70"));
    assert_eq!(message.intent, InboundIntent::Steer);
    assert_eq!(message.attachments[0].id, "voice-file-id");
    assert_eq!(message.attachments[0].media_type, "audio/ogg");
    assert!(message.attachments[0].download_url.is_none());
    assert_eq!(adapter.cursor().next_update_id, Some(9002));
    assert!(
        adapter
            .features()
            .capabilities
            .contains(&AdapterCapability::Attachments)
    );
    let capabilities = adapter.capabilities_v2();
    capabilities.validate().expect("Telegram v2 capabilities");
    assert!(capabilities.supports(ChannelCapabilityV2::Voice));
    assert!(capabilities.supports(ChannelCapabilityV2::Typing));
    assert!(!capabilities.supports(ChannelCapabilityV2::ReadReceipts));
    let command = adapter.receive_v2().expect("Telegram v2 command");
    let ChannelEventKindV2::Command(command) = command.event else {
        panic!("expected Telegram v2 command event");
    };
    assert_eq!(command.name, "steer");
    assert_eq!(command.arguments, "examine this");
    assert!(adapter.reconnect_cursor_v2().is_some());
    adapter.rotate_token(secret("rotated-bot-token"));

    assert_eq!(
        adapter
            .ingest_webhook(b"webhook_secret-42", body)
            .expect("duplicate webhook"),
        TelegramWebhookOutcome::Duplicate
    );
    let cursor = adapter.cursor().clone();
    let mut restarted = TelegramAdapter::new(
        webhook_config("bot-42"),
        secret("123:bot-token"),
        secret("webhook_secret-42"),
        cursor,
    )
    .expect("restarted Telegram adapter");
    assert_eq!(
        restarted
            .ingest_webhook(b"webhook_secret-42", body)
            .expect("restart duplicate"),
        TelegramWebhookOutcome::Duplicate
    );

    let wrong_account = TelegramAdapter::new(
        webhook_config("bot-other"),
        secret("456:other-token"),
        secret("other_secret"),
        TelegramCursor::default(),
    )
    .expect("isolated Telegram adapter");
    assert_ne!(
        wrong_account.cursor().recent_update_ids,
        adapter.cursor().recent_update_ids
    );

    let failure = restarted
        .ingest_webhook(b"wrong-secret", b"not-json")
        .expect_err("signature must fail before JSON parsing");
    assert_eq!(failure.class, RetryClass::Permanent);
    assert_eq!(
        failure.safe_message,
        "Telegram webhook authentication failed"
    );
    assert!(!failure.safe_message.contains("123:bot-token"));
    let malformed = restarted
        .ingest_webhook(b"webhook_secret-42", b"not-json")
        .expect_err("verified malformed event must be rejected");
    assert_eq!(malformed.class, RetryClass::Permanent);
    assert_eq!(malformed.safe_message, "Telegram webhook body is malformed");
}

#[test]
#[ignore = "requires loopback networking; run during network-enabled qualification"]
#[allow(clippy::too_many_lines)]
fn telegram_poll_send_typing_edit_media_download_and_reconnect_use_real_http() {
    let polling = br#"{"ok":true,"result":[{"update_id":10,"message":{"message_id":5,"date":1700000001,"chat":{"id":99},"from":{"id":7},"text":"hello"}}]}"#;
    let responses = vec![
        Response {
            status: 200,
            content_type: "application/json",
            body: polling.to_vec(),
        },
        Response {
            status: 200,
            content_type: "application/json",
            body: br#"{"ok":true,"result":{"message_id":101}}"#.to_vec(),
        },
        Response {
            status: 200,
            content_type: "application/json",
            body: br#"{"ok":true,"result":true}"#.to_vec(),
        },
        Response {
            status: 200,
            content_type: "application/json",
            body: br#"{"ok":true,"result":{"message_id":101}}"#.to_vec(),
        },
        Response {
            status: 200,
            content_type: "application/json",
            body: br#"{"ok":true,"result":{"id":42,"is_bot":true}}"#.to_vec(),
        },
        Response {
            status: 200,
            content_type: "application/json",
            body: br#"{"ok":true,"result":{"message_id":102}}"#.to_vec(),
        },
        Response {
            status: 200,
            content_type: "application/json",
            body: br#"{"ok":true,"result":{"file_id":"media-id","file_path":"voice/file.ogg"}}"#
                .to_vec(),
        },
        Response {
            status: 200,
            content_type: "audio/ogg",
            body: b"real-voice-bytes".to_vec(),
        },
    ];
    let (api_base, server) = serve(responses);
    let mut config = TelegramConfig::production(
        "bot-42",
        TelegramIngress::Polling {
            timeout_seconds: 1,
            limit: 10,
        },
    );
    config.api_base = api_base;
    config.timeout_ms = 2_000;
    let mut adapter = TelegramAdapter::new(
        config,
        secret("123:bot-token"),
        secret("webhook_secret"),
        TelegramCursor::default(),
    )
    .expect("Telegram polling adapter");

    let AdapterEvent::Inbound(inbound) = adapter.receive().expect("poll Telegram") else {
        panic!("expected polled inbound");
    };
    assert_eq!(inbound.message_id, "5");
    assert_eq!(adapter.cursor().next_update_id, Some(11));
    let route = inbound.reply_route();
    let sent = adapter
        .send(&OutboundMessage {
            route: route.clone(),
            idempotency_key: "outbox-1".to_owned(),
            text: "reply".to_owned(),
            artifacts: Vec::new(),
        })
        .expect("send Telegram reply");
    assert_eq!(sent.platform_message_id, "101");
    assert!(sent.duplicate_possible);
    adapter.send_typing(&route).expect("Telegram typing");
    adapter
        .edit_message(&route, "101", "revised")
        .expect("Telegram edit");
    adapter.reconnect().expect("Telegram health reconnect");

    let artifact_id = ArtifactId::new();
    adapter
        .stage_artifact(
            artifact_id.clone(),
            TelegramUpload {
                kind: TelegramUploadKind::Voice,
                file_name: "reply.ogg".to_owned(),
                media_type: "audio/ogg".to_owned(),
                bytes: b"outbound-voice".to_vec(),
            },
        )
        .expect("stage Telegram voice");
    let media_receipt = adapter
        .send(&OutboundMessage {
            route,
            idempotency_key: "outbox-2".to_owned(),
            text: String::new(),
            artifacts: vec![artifact_id],
        })
        .expect("send Telegram voice");
    assert_eq!(media_receipt.platform_message_id, "102");
    let download = adapter
        .download_file("media-id")
        .expect("download Telegram media");
    assert_eq!(download.file_name, "file.ogg");
    assert_eq!(download.bytes, b"real-voice-bytes");

    let requests = server.join().expect("HTTP boundary completion");
    assert!(requests[0].path.ends_with("/bot123:bot-token/getUpdates"));
    assert!(requests[1].path.ends_with("/bot123:bot-token/sendMessage"));
    assert!(
        requests[2]
            .path
            .ends_with("/bot123:bot-token/sendChatAction")
    );
    assert!(
        requests[3]
            .path
            .ends_with("/bot123:bot-token/editMessageText")
    );
    assert!(requests[4].path.ends_with("/bot123:bot-token/getMe"));
    assert!(requests[5].path.ends_with("/bot123:bot-token/sendVoice"));
    assert!(
        requests[5]
            .headers
            .to_ascii_lowercase()
            .contains("multipart/form-data")
    );
    assert!(
        requests[5]
            .body
            .windows(b"outbound-voice".len())
            .any(|window| window == b"outbound-voice")
    );
    assert!(requests[6].path.ends_with("/bot123:bot-token/getFile"));
    assert!(
        requests[7]
            .path
            .ends_with("/file/bot123:bot-token/voice/file.ogg")
    );
}

#[test]
#[ignore = "requires loopback networking; run during network-enabled qualification"]
fn telegram_rate_limit_is_classified_without_secret_leakage() {
    let (api_base, server) = serve(vec![Response {
        status: 429,
        content_type: "application/json",
        body: br#"{"ok":false,"error_code":429,"description":"Too Many Requests","parameters":{"retry_after":3}}"#
            .to_vec(),
    }]);
    let mut config = TelegramConfig::production(
        "bot-42",
        TelegramIngress::Polling {
            timeout_seconds: 1,
            limit: 1,
        },
    );
    config.api_base = api_base;
    let mut adapter = TelegramAdapter::new(
        config,
        secret("secret-bot-token"),
        secret("webhook_secret"),
        TelegramCursor::default(),
    )
    .expect("Telegram adapter");
    let failure = adapter.receive().expect_err("Telegram rate limit");
    assert_eq!(failure.class, RetryClass::RateLimited);
    assert_eq!(failure.retry_after_ms, Some(3_000));
    assert!(!failure.safe_message.contains("secret-bot-token"));
    server.join().expect("rate-limit boundary");
}
