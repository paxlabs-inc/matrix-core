#![allow(
    clippy::needless_pass_by_value,
    clippy::similar_names,
    clippy::too_many_lines
)]

use std::collections::BTreeMap;
use std::io::{BufRead, BufReader, Read, Write};
use std::net::{SocketAddr, TcpListener, TcpStream};
use std::thread;

use keith_agent_types::ArtifactId;
use keith_channel_core::{
    ChannelAdapterErrorKindV2, ChannelAdapterV2, ChannelCapabilityV2, ChannelEventKindV2,
    ChannelOperationV2, ChannelOutboundMessageV2, ChannelReceiptStateV2, ReplyRoute,
};
use keith_credentials::SecretValue;
use serde_json::{Value, json};
use tungstenite::Message;

#[path = "../src/slack.rs"]
mod slack;

use slack::{SlackAdapter, SlackConfig, SlackCursor, SlackUpload, SlackWebhookOutcome};

fn secret(value: &str) -> SecretValue {
    SecretValue::new(value.as_bytes().to_vec()).expect("test secret")
}

fn config(api_base: String) -> SlackConfig {
    let mut config = SlackConfig::production("T1DC2JH3J", "UBOT");
    config.api_base = api_base;
    config.max_event_bytes = 64 * 1_024;
    config.max_attachment_bytes = 4_096;
    config.max_attachments = 4;
    config.max_staged_bytes = 8_192;
    config.max_rich_content_bytes = 8_192;
    config.timeout_ms = 2_000;
    config.deduplication_capacity = 16;
    config.requests_per_minute = 50;
    config
}

fn make_adapter(api_base: String, app_token: Option<&str>, cursor: SlackCursor) -> SlackAdapter {
    SlackAdapter::new(
        config(api_base),
        secret("xoxb-test"),
        app_token.map(secret),
        secret("8f742231b10e8888abcd99yyyzzz85a5"),
        cursor,
    )
    .expect("Slack adapter")
}

fn read_http_request(stream: &mut TcpStream) -> (String, Vec<u8>) {
    let mut reader = BufReader::new(stream.try_clone().expect("clone HTTP stream"));
    let mut headers = String::new();
    let mut content_length = 0;
    loop {
        let mut line = String::new();
        reader.read_line(&mut line).expect("HTTP header");
        if line == "\r\n" {
            break;
        }
        if line.to_ascii_lowercase().starts_with("content-length:") {
            content_length = line
                .split_once(':')
                .expect("content length")
                .1
                .trim()
                .parse::<usize>()
                .expect("numeric content length");
        }
        headers.push_str(&line);
    }
    let mut body = vec![0; content_length];
    reader.read_exact(&mut body).expect("HTTP body");
    (headers, body)
}

fn respond_json(stream: &mut TcpStream, status: &str, body: &Value, extra_headers: &str) {
    let body = body.to_string();
    write!(
        stream,
        "HTTP/1.1 {status}\r\nContent-Type: application/json\r\nContent-Length: {}\r\n{extra_headers}Connection: close\r\n\r\n{body}",
        body.len()
    )
    .expect("HTTP response");
}

fn socket_envelope(envelope_id: &str, event_id: &str, event: Value) -> Value {
    json!({
        "envelope_id": envelope_id,
        "type": "events_api",
        "accepts_response_payload": false,
        "payload": {
            "type": "event_callback",
            "team_id": "T1DC2JH3J",
            "api_app_id": "A1",
            "event_id": event_id,
            "event_time": 1_531_420_618,
            "event_context": format!("context-{event_id}"),
            "event": event
        }
    })
}

fn send_socket_json(socket: &mut tungstenite::WebSocket<TcpStream>, value: &Value) {
    socket
        .send(Message::Text(value.to_string().into()))
        .expect("Socket Mode event");
}

fn expect_ack(socket: &mut tungstenite::WebSocket<TcpStream>, envelope_id: &str) {
    let ack: Value = serde_json::from_str(
        socket
            .read()
            .expect("Socket Mode acknowledgement")
            .to_text()
            .expect("text acknowledgement"),
    )
    .expect("acknowledgement JSON");
    assert_eq!(ack["envelope_id"], envelope_id);
}

fn serve_socket(listener: TcpListener) {
    let (stream, _) = listener.accept().expect("first Socket Mode connection");
    let mut socket = tungstenite::accept(stream).expect("first WebSocket handshake");
    let message = json!({
        "type": "message",
        "user": "U123",
        "text": "<@UBOT> status please",
        "ts": "1531420618.000200",
        "thread_ts": "1531420618.000100",
        "client_msg_id": "M1",
        "channel": "C123",
        "blocks": [{"type":"section","text":{"type":"mrkdwn","text":"status please"}}],
        "files": [{
            "id":"F1",
            "name":"status.txt",
            "mimetype":"text/plain",
            "size":12,
            "url_private_download":"https://files.slack.com/files-pri/F1/download"
        }]
    });
    send_socket_json(&mut socket, &socket_envelope("env-1", "Ev01", message));
    expect_ack(&mut socket, "env-1");
    send_socket_json(
        &mut socket,
        &json!({"type":"disconnect","reason":"refresh_requested"}),
    );
    drop(socket);

    let (stream, _) = listener.accept().expect("second Socket Mode connection");
    let mut socket = tungstenite::accept(stream).expect("second WebSocket handshake");
    let replay = json!({
        "type":"message","user":"U123","text":"duplicate","ts":"1531420618.000200",
        "client_msg_id":"M1","channel":"C123"
    });
    send_socket_json(&mut socket, &socket_envelope("env-2", "Ev01", replay));
    expect_ack(&mut socket, "env-2");
    let reaction = json!({
        "type":"reaction_added","user":"U456","reaction":"eyes",
        "item":{"type":"message","channel":"C123","ts":"1531420618.000200"},
        "event_ts":"1531420619.000100"
    });
    send_socket_json(&mut socket, &socket_envelope("env-3", "Ev02", reaction));
    expect_ack(&mut socket, "env-3");
    let edit = json!({
        "type":"message","subtype":"message_changed","channel":"C123",
        "message":{
            "user":"U123","text":"updated status","ts":"1531420618.000200",
            "thread_ts":"1531420618.000100"
        },
        "event_ts":"1531420620.000100"
    });
    send_socket_json(&mut socket, &socket_envelope("env-4", "Ev03", edit));
    expect_ack(&mut socket, "env-4");
    let deletion = json!({
        "type":"message","subtype":"message_deleted","channel":"C123",
        "deleted_ts":"1531420618.000200","user":"U123",
        "event_ts":"1531420621.000100"
    });
    send_socket_json(&mut socket, &socket_envelope("env-5", "Ev04", deletion));
    expect_ack(&mut socket, "env-5");
}

fn serve_connection_urls(listener: TcpListener, socket_address: SocketAddr) {
    for _ in 0..2 {
        let (mut stream, _) = listener.accept().expect("apps.connections.open request");
        let (headers, body) = read_http_request(&mut stream);
        assert!(headers.starts_with("POST /apps.connections.open HTTP/1.1"));
        assert!(
            headers
                .to_ascii_lowercase()
                .contains("authorization: bearer xapp-test")
        );
        assert_eq!(
            serde_json::from_slice::<Value>(&body).expect("request JSON"),
            json!({})
        );
        respond_json(
            &mut stream,
            "200 OK",
            &json!({"ok":true,"url":format!("ws://{socket_address}")}),
            "",
        );
    }
}

#[test]
fn slack_signed_webhook_uses_official_hmac_vector_and_rejects_before_parse() {
    let body = b"token=xyzz0WbapA4vBCDEFasx0q6G&team_id=T1DC2JH3J&team_domain=testteamnow&channel_id=G8PSS9T3V&channel_name=foobar&user_id=U2CERLKJA&user_name=roadrunner&command=%2Fwebhook-collect&text=&response_url=https%3A%2F%2Fhooks.slack.com%2Fcommands%2FT1DC2JH3J%2F397700885554%2F96rGlfmibIGlgcZRskXaIFfN&trigger_id=398738663015.47445629121.803a0bc887a14d10d2c447fce8b6703c";
    let mut adapter = make_adapter(
        "http://127.0.0.1:9".to_owned(),
        None,
        SlackCursor::default(),
    );
    let outcome = adapter
        .ingest_webhook(
            1_531_420_618,
            "v0=a2114d57b48eac39b9ad189dd8316235a7b4a8d21a10bd27519666489c69b503",
            body,
            1_531_420_618,
        )
        .expect("official Slack signature vector");
    let SlackWebhookOutcome::Event(Some(event)) = outcome else {
        panic!("normalized Slack command required");
    };
    let event = *event;
    let ChannelEventKindV2::Command(command) = event.event else {
        panic!("Slack command required");
    };
    assert_eq!(command.name, "/webhook-collect");
    assert_eq!(command.sender.platform_id, "U2CERLKJA");
    let setup = adapter.setup_diagnostics();
    setup.validate().expect("Slack setup diagnostics");
    assert!(setup.webhook_configured);
    assert!(!setup.socket_or_polling_configured);
    assert!(!setup.required_credential_names.contains("app_token"));
    assert!(matches!(
        adapter
            .ingest_webhook(
                1_531_420_618,
                "v0=a2114d57b48eac39b9ad189dd8316235a7b4a8d21a10bd27519666489c69b503",
                body,
                1_531_420_618,
            )
            .expect("duplicate webhook acknowledged without redispatch"),
        SlackWebhookOutcome::Event(None)
    ));

    let error = adapter
        .ingest_webhook(1, "v0=invalid", b"not-json", 1)
        .expect_err("invalid signature denied before malformed parsing");
    assert_eq!(error.kind, ChannelAdapterErrorKindV2::Authentication);
}

#[test]
fn slack_socket_inbound_thread_attachment_duplicate_restart_and_reaction_are_real() {
    let socket_listener = TcpListener::bind("127.0.0.1:0").expect("Socket Mode listener");
    let socket_address = socket_listener.local_addr().expect("Socket Mode address");
    let socket_server = thread::spawn(move || serve_socket(socket_listener));
    let api_listener = TcpListener::bind("127.0.0.1:0").expect("Slack API listener");
    let api_address = api_listener.local_addr().expect("Slack API address");
    let api_server = thread::spawn(move || serve_connection_urls(api_listener, socket_address));

    let api_base = format!("http://{api_address}");
    let mut adapter = make_adapter(api_base.clone(), Some("xapp-test"), SlackCursor::default());
    let capabilities = adapter.capabilities_v2();
    assert!(capabilities.supports(ChannelCapabilityV2::Threads));
    assert!(capabilities.supports(ChannelCapabilityV2::RichContent));
    assert!(!capabilities.supports(ChannelCapabilityV2::Typing));
    assert!(!capabilities.supports(ChannelCapabilityV2::ReadReceipts));

    let first = adapter.receive_v2().expect("Slack Socket Mode message");
    let ChannelEventKindV2::MessageCreated(message) = first.event else {
        panic!("Slack message required");
    };
    assert_eq!(message.message_id, "M1");
    assert_eq!(
        message.conversation.thread_id.as_deref(),
        Some("1531420618.000100")
    );
    assert_eq!(
        message.conversation.reply_to_message_id.as_deref(),
        Some("1531420618.000100")
    );
    assert_eq!(message.attachments.len(), 1);
    assert_eq!(message.rich_content.len(), 1);
    assert_eq!(message.mentions[0].identity.platform_id, "UBOT");
    assert!(matches!(
        adapter.receive_v2().expect("Slack refresh request").event,
        ChannelEventKindV2::ReconnectRequired { .. }
    ));
    let cursor = adapter.cursor().clone();
    assert_eq!(cursor.recent_event_ids, vec!["Ev01"]);
    drop(adapter);

    let mut adapter = make_adapter(api_base, Some("xapp-test"), cursor);
    adapter.reconnect_v2().expect("fresh Socket Mode URL");
    let reaction = adapter
        .receive_v2()
        .expect("duplicate skipped and reaction returned");
    assert_eq!(reaction.event_id, "Ev02");
    let ChannelEventKindV2::Reaction(reaction) = reaction.event else {
        panic!("Slack reaction required");
    };
    assert_eq!(reaction.reaction, "eyes");
    assert!(matches!(
        adapter.receive_v2().expect("Slack message edit").event,
        ChannelEventKindV2::MessageEdited(_)
    ));
    assert!(matches!(
        adapter.receive_v2().expect("Slack message deletion").event,
        ChannelEventKindV2::MessageDeleted(_)
    ));
    assert_eq!(
        adapter.cursor().recent_event_ids,
        vec!["Ev01", "Ev02", "Ev03", "Ev04"]
    );

    socket_server.join().expect("Socket Mode server");
    api_server.join().expect("Slack connection API server");
}

fn outbound_route() -> ReplyRoute {
    ReplyRoute {
        channel: "slack".to_owned(),
        external_account: "T1DC2JH3J".to_owned(),
        conversation: "C123".to_owned(),
        thread: Some("1531420618.000100".to_owned()),
        reply_to_message: Some("1531420618.000200".to_owned()),
    }
}

fn outbound_message(key: &str, artifacts: Vec<ArtifactId>) -> ChannelOutboundMessageV2 {
    ChannelOutboundMessageV2 {
        route: outbound_route(),
        idempotency_key: key.to_owned(),
        text: "Keith result".to_owned(),
        artifacts,
        rich_content: Vec::new(),
        metadata: BTreeMap::new(),
    }
}

#[test]
fn slack_outbound_reply_edit_reaction_delete_file_rate_limit_cancellation_and_isolation_are_real() {
    let upload_listener = TcpListener::bind("127.0.0.1:0").expect("upload listener");
    let upload_address = upload_listener.local_addr().expect("upload address");
    let upload_server = thread::spawn(move || {
        let (mut stream, _) = upload_listener.accept().expect("external upload");
        let (headers, body) = read_http_request(&mut stream);
        assert!(headers.starts_with("POST /upload/F1 HTTP/1.1"));
        assert_eq!(body, b"report bytes");
        write!(
            stream,
            "HTTP/1.1 200 OK\r\nContent-Length: 2\r\nConnection: close\r\n\r\nOK"
        )
        .expect("upload response");
    });

    let api_listener = TcpListener::bind("127.0.0.1:0").expect("Slack Web API listener");
    let api_address = api_listener.local_addr().expect("Slack Web API address");
    let api_server = thread::spawn(move || {
        let expected = [
            "auth.test",
            "chat.postMessage",
            "chat.update",
            "reactions.add",
            "chat.delete",
            "files.getUploadURLExternal",
            "files.completeUploadExternal",
            "chat.postMessage",
        ];
        let mut requests = Vec::new();
        for method in expected {
            let (mut stream, _) = api_listener.accept().expect("Slack Web API request");
            let (headers, body) = read_http_request(&mut stream);
            assert!(headers.starts_with(&format!("POST /{method} HTTP/1.1")));
            assert!(
                headers
                    .to_ascii_lowercase()
                    .contains("authorization: bearer xoxb-test")
            );
            let request: Value = serde_json::from_slice(&body).expect("Slack request JSON");
            let (status, response, extra_headers) = match method {
                "auth.test" => (
                    "200 OK",
                    json!({"ok":true,"team_id":"T1DC2JH3J","user_id":"UBOT"}),
                    "",
                ),
                "chat.postMessage"
                    if !requests
                        .iter()
                        .any(|(seen, _): &(String, Value)| seen == "chat.postMessage") =>
                {
                    (
                        "200 OK",
                        json!({"ok":true,"channel":"C123","ts":"1531420620.000100"}),
                        "",
                    )
                }
                "chat.update" => (
                    "200 OK",
                    json!({"ok":true,"channel":"C123","ts":"1531420620.000100"}),
                    "",
                ),
                "reactions.add" | "chat.delete" => ("200 OK", json!({"ok":true}), ""),
                "files.getUploadURLExternal" => (
                    "200 OK",
                    json!({
                        "ok":true,
                        "upload_url":format!("http://{upload_address}/upload/F1"),
                        "file_id":"F1"
                    }),
                    "",
                ),
                "files.completeUploadExternal" => (
                    "200 OK",
                    json!({"ok":true,"files":[{"id":"F1","title":"report.txt"}]}),
                    "",
                ),
                "chat.postMessage" => (
                    "429 Too Many Requests",
                    json!({"ok":false,"error":"ratelimited"}),
                    "Retry-After: 2\r\n",
                ),
                _ => unreachable!(),
            };
            respond_json(&mut stream, status, &response, extra_headers);
            requests.push((method.to_owned(), request));
        }
        requests
    });

    let mut adapter = make_adapter(
        format!("http://{api_address}"),
        None,
        SlackCursor::default(),
    );
    adapter.test_connection().expect("safe Slack auth test");
    let send = ChannelOperationV2::SendMessage(outbound_message("delivery-1", Vec::new()));
    let sent = adapter.execute_v2(&send).expect("Slack threaded reply");
    assert_eq!(
        sent.platform_message_id.as_deref(),
        Some("1531420620.000100")
    );
    assert_eq!(sent.state, ChannelReceiptStateV2::Accepted);

    adapter
        .execute_v2(&ChannelOperationV2::EditMessage {
            route: outbound_route(),
            platform_message_id: "1531420620.000100".to_owned(),
            text: "Updated result".to_owned(),
            rich_content: Vec::new(),
        })
        .expect("Slack edit");
    adapter
        .execute_v2(&ChannelOperationV2::AddReaction {
            route: outbound_route(),
            platform_message_id: "1531420620.000100".to_owned(),
            reaction: "white_check_mark".to_owned(),
        })
        .expect("Slack reaction");
    adapter
        .execute_v2(&ChannelOperationV2::DeleteMessage {
            route: outbound_route(),
            platform_message_id: "1531420620.000100".to_owned(),
        })
        .expect("Slack deletion");

    let artifact = ArtifactId::new();
    adapter
        .stage_artifact(
            artifact.clone(),
            SlackUpload {
                file_name: "report.txt".to_owned(),
                media_type: "text/plain".to_owned(),
                bytes: b"report bytes".to_vec(),
                title: Some("Status report".to_owned()),
            },
        )
        .expect("stage Slack file");
    let file_receipt = adapter
        .execute_v2(&ChannelOperationV2::SendMessage(outbound_message(
            "delivery-file",
            vec![artifact],
        )))
        .expect("Slack external file upload");
    assert_eq!(file_receipt.platform_message_id.as_deref(), Some("F1"));

    let rate_limit = adapter
        .execute_v2(&ChannelOperationV2::SendMessage(outbound_message(
            "delivery-rate-limit",
            Vec::new(),
        )))
        .expect_err("Slack rate limit");
    assert_eq!(rate_limit.kind, ChannelAdapterErrorKindV2::RateLimit);
    assert_eq!(rate_limit.retry_after_ms, Some(2_000));

    adapter
        .execute_v2(&ChannelOperationV2::Cancel {
            cancellation_id: "delivery-cancelled".to_owned(),
        })
        .expect("cancel queued delivery");
    assert_eq!(
        adapter
            .execute_v2(&ChannelOperationV2::SendMessage(outbound_message(
                "delivery-cancelled",
                Vec::new(),
            )))
            .expect_err("cancelled delivery not sent")
            .kind,
        ChannelAdapterErrorKindV2::Cancelled
    );
    assert_eq!(
        adapter
            .execute_v2(&ChannelOperationV2::SetTyping {
                route: outbound_route(),
                active: true,
            })
            .expect_err("Slack typing honestly unsupported")
            .kind,
        ChannelAdapterErrorKindV2::UnsupportedFeature
    );
    let mut foreign = outbound_message("foreign", Vec::new());
    foreign.route.external_account = "T-OTHER".to_owned();
    assert_eq!(
        adapter
            .execute_v2(&ChannelOperationV2::SendMessage(foreign))
            .expect_err("cross-account route denied")
            .kind,
        ChannelAdapterErrorKindV2::Permission
    );

    let requests = api_server.join().expect("Slack Web API server");
    upload_server.join().expect("Slack upload server");
    assert_eq!(requests[1].1["thread_ts"], "1531420618.000100");
    assert_eq!(requests[1].1["client_msg_id"], "delivery-1");
    assert_eq!(requests[3].1["name"], "white_check_mark");
    assert_eq!(requests[6].1["files"][0]["id"], "F1");
}
