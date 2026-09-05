#![forbid(unsafe_code)]

#[path = "../src/email.rs"]
mod email;
#[path = "../src/google_chat.rs"]
mod google_chat;
#[path = "../src/matrix.rs"]
mod matrix;
#[path = "../src/teams.rs"]
mod teams;

use email::{
    EmailAdapter, EmailConfig, EmailCursor, EmailDeliveryState, EmailEventVerifier,
    EmailIngestOutcome, EmailUpload, EmailVerifiedEvent,
};
use google_chat::{
    GoogleChatAdapter, GoogleChatConfig, GoogleChatCursor, GoogleChatIngestOutcome,
    GoogleChatRequestVerifier, GoogleChatVerifiedClaims,
};
use keith_agent_types::{ArtifactId, ProfileId, UtcTimestamp};
use keith_channel_core::{
    AdapterEvent, ChannelAdapter, ChannelAdapterV2, ChannelCapabilityV2, ChannelConnectionHealthV2,
    ChannelEventKindV2, OutboundMessage, ReplyRoute, RetryClass,
};
use keith_credentials::{CredentialOwner, CredentialRef, SecretValue};
use matrix::{MatrixAdapter, MatrixConfig, MatrixCursor, MatrixNormalizedEvent, MatrixUpload};
use serde_json::json;
use teams::{
    TeamsAdapter, TeamsConfig, TeamsCursor, TeamsIngestOutcome, TeamsRequestVerifier,
    TeamsVerifiedClaims,
};

const FUTURE: UtcTimestamp = UtcTimestamp::from_unix_millis(4_102_444_800_000);

#[derive(Debug)]
struct VerificationError(&'static str);

impl std::fmt::Display for VerificationError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter.write_str(self.0)
    }
}

struct PinnedTeamsIngress;

impl TeamsRequestVerifier for PinnedTeamsIngress {
    type Error = VerificationError;

    fn verify(
        &self,
        authorization: &str,
        expected_audience: &str,
        _now: UtcTimestamp,
    ) -> Result<TeamsVerifiedClaims, Self::Error> {
        if authorization != "Bearer signed-teams-fixture" {
            return Err(VerificationError("invalid signed fixture"));
        }
        Ok(TeamsVerifiedClaims {
            issuer: "https://api.botframework.com".to_owned(),
            audience: expected_audience.to_owned(),
            key_id: "fixture-signing-key".to_owned(),
            service_url: "https://smba.trafficmanager.net/teams".to_owned(),
            expires_at: FUTURE,
        })
    }
}

struct PinnedGoogleChatIngress;

impl GoogleChatRequestVerifier for PinnedGoogleChatIngress {
    type Error = VerificationError;

    fn verify(
        &self,
        authorization: &str,
        expected_audience: &str,
        _now: UtcTimestamp,
    ) -> Result<GoogleChatVerifiedClaims, Self::Error> {
        if authorization != "Bearer signed-google-fixture" {
            return Err(VerificationError("invalid signed fixture"));
        }
        Ok(GoogleChatVerifiedClaims {
            issuer: "https://accounts.google.com".to_owned(),
            audience: expected_audience.to_owned(),
            subject: "chat-system".to_owned(),
            email: Some("chat@system.gserviceaccount.com".to_owned()),
            expires_at: FUTURE,
        })
    }
}

struct PinnedEmailIngress;

impl EmailEventVerifier for PinnedEmailIngress {
    type Error = VerificationError;

    fn verify(
        &self,
        headers: &[(String, String)],
        _body: &[u8],
        expected_audience: &str,
        _now: UtcTimestamp,
    ) -> Result<EmailVerifiedEvent, Self::Error> {
        if !headers
            .iter()
            .any(|(name, value)| name == "X-Provider-Signature" && value == "signed-email-fixture")
        {
            return Err(VerificationError("invalid signed fixture"));
        }
        Ok(EmailVerifiedEvent {
            provider: "mail-provider".to_owned(),
            audience: expected_audience.to_owned(),
            event_id: "provider-event-1".to_owned(),
            expires_at: FUTURE,
        })
    }
}

fn profile() -> ProfileId {
    ProfileId::new()
}

fn credential(account: &str) -> CredentialRef {
    CredentialRef::new(account, CredentialOwner::Channel(account.to_owned()))
        .expect("channel credential")
}

fn token() -> SecretValue {
    SecretValue::new("provider-access-token").expect("secret")
}

fn route(channel: &str, account: &str, conversation: &str) -> ReplyRoute {
    ReplyRoute {
        channel: channel.to_owned(),
        external_account: account.to_owned(),
        conversation: conversation.to_owned(),
        thread: None,
        reply_to_message: None,
    }
}

#[test]
fn work_channel_configuration_rejects_control_and_unbounded_identities() {
    assert!(!teams::valid_channel_identity("account\nheader"));
    assert!(!teams::valid_channel_identity(&"x".repeat(513)));

    let teams_config = TeamsConfig::production(
        "bot-app-id\r\ninjected",
        "teams-work",
        profile(),
        credential("teams-work"),
    );
    assert!(TeamsAdapter::new(teams_config, token(), TeamsCursor::default()).is_err());

    let chat_config = GoogleChatConfig::production(
        "https://keith.example/chat\nspoofed",
        "google-work",
        profile(),
        credential("google-work"),
    );
    assert!(GoogleChatAdapter::new(chat_config, token(), GoogleChatCursor::default()).is_err());

    let email_config = EmailConfig::provider(
        "mail-provider\r\nspoofed",
        "https://mail.example",
        "keith-email",
        "mail-work",
        "keith@example.com",
        profile(),
        credential("mail-work"),
    );
    assert!(EmailAdapter::new(email_config, token(), EmailCursor::default()).is_err());

    let matrix_config = MatrixConfig::production(
        "https://matrix.example",
        "matrix-work\nspoofed",
        "@keith:matrix.example",
        profile(),
        credential("matrix-work"),
    );
    assert!(MatrixAdapter::new(matrix_config, token(), MatrixCursor::default()).is_err());
}

#[test]
#[allow(clippy::too_many_lines)]
fn work_channels_verified_ingress_and_real_request_planning() {
    let teams_base = "https://smba.trafficmanager.net/teams";
    let mut teams = TeamsAdapter::new(
        TeamsConfig::production(
            "bot-app-id",
            "teams-work",
            profile(),
            credential("teams-work"),
        ),
        token(),
        TeamsCursor::default(),
    )
    .expect("Teams adapter");
    let teams_event = format!(
        r#"{{
          "type":"message",
          "id":"teams-in-1",
          "timestamp":"2026-08-30T10:00:00Z",
          "serviceUrl":"{teams_base}",
          "from":{{"id":"29:user","name":"Person"}},
          "conversation":{{"id":"conversation-1"}},
          "text":"hello @Keith",
          "replyToId":"root-activity",
          "attachments":[{{
            "contentType":"application/vnd.microsoft.card.adaptive",
            "name":"status-card",
            "content":{{"type":"AdaptiveCard","version":"1.5"}}
          }}],
          "entities":[{{
            "type":"mention",
            "mentioned":{{"id":"28:keith","name":"Keith"}}
          }}]
        }}"#
    );
    assert_eq!(
        teams
            .ingest_verified_activity(
                "Bearer signed-teams-fixture",
                teams_event.as_bytes(),
                &PinnedTeamsIngress,
            )
            .expect("verified Teams event"),
        TeamsIngestOutcome::Queued
    );
    let teams_event = teams.receive_v2().expect("Teams v2 inbound");
    let ChannelEventKindV2::MessageCreated(teams_inbound) = teams_event.event else {
        panic!("expected Teams message");
    };
    assert_eq!(teams_inbound.conversation.platform_id, "conversation-1");
    assert_eq!(
        teams_inbound.conversation.thread_id.as_deref(),
        Some("root-activity")
    );
    assert_eq!(teams_inbound.rich_content.len(), 1);
    assert_eq!(teams_inbound.mentions.len(), 1);
    assert!(teams.setup_diagnostics().callback_verified);
    let teams_setup = teams.account_setup_v2();
    teams_setup.validate().expect("Teams account setup");
    assert_eq!(
        teams_setup.connection_health,
        ChannelConnectionHealthV2::Connected
    );
    assert!(teams_setup.safe_test_supported);
    let teams_test_request = teams
        .prepare_test_connection()
        .expect("prepare Teams safe test");
    assert_eq!(teams_test_request.method, "GET");
    assert_eq!(
        teams_test_request.url,
        "https://smba.trafficmanager.net/teams/v3/conversations/conversation-1/members"
    );
    std::hint::black_box(TeamsAdapter::test_connection);
    assert_eq!(
        teams
            .cursor()
            .recent_activity_ids
            .last()
            .map(String::as_str),
        Some("teams-in-1")
    );
    teams
        .capabilities_v2()
        .validate()
        .expect("Teams v2 capabilities");
    assert!(
        teams
            .capabilities_v2()
            .supports(ChannelCapabilityV2::Mentions)
    );
    let teams_request = teams
        .prepare_send(&OutboundMessage {
            route: route("teams", "teams-work", "conversation-1"),
            idempotency_key: "teams-intent-1".to_owned(),
            text: "reply from Keith".to_owned(),
            artifacts: Vec::new(),
        })
        .expect("prepare Teams send");
    assert_eq!(teams_request.method, "POST");
    assert_eq!(
        teams_request.url,
        "https://smba.trafficmanager.net/teams/v3/conversations/conversation-1/activities"
    );
    assert_eq!(
        teams_request.idempotency_key.as_deref(),
        Some("teams-intent-1")
    );
    assert!(
        String::from_utf8(teams_request.body)
            .expect("Teams request JSON")
            .contains("clientActivityId")
    );
    let chat_config = GoogleChatConfig::production(
        "https://keith.example/chat",
        "google-work",
        profile(),
        credential("google-work"),
    );
    let mut chat = GoogleChatAdapter::new(chat_config, token(), GoogleChatCursor::default())
        .expect("Google Chat adapter");
    let chat_event = br#"{
      "type":"MESSAGE",
      "eventTime":"2026-08-30T10:01:00Z",
      "space":{"name":"spaces/AAA"},
      "user":{"name":"users/123"},
      "message":{
        "name":"spaces/AAA/messages/google-in-1",
        "sender":{"name":"users/123"},
        "text":"hello Keith",
        "thread":{"name":"spaces/AAA/threads/thread-1"},
        "attachment":[{
          "contentName":"brief.pdf",
          "contentType":"application/pdf",
          "attachmentDataRef":{"resourceName":"attachments/brief"},
          "size":2048
        }]
      }
    }"#;
    assert_eq!(
        chat.ingest_verified_event(
            "Bearer signed-google-fixture",
            chat_event,
            &PinnedGoogleChatIngress,
        )
        .expect("verified Google Chat event"),
        GoogleChatIngestOutcome::Queued
    );
    let AdapterEvent::Inbound(chat_inbound) = chat.receive().expect("Google Chat inbound") else {
        panic!("expected Google Chat message");
    };
    assert_eq!(chat_inbound.conversation, "spaces/AAA");
    assert_eq!(
        chat_inbound.thread.as_deref(),
        Some("spaces/AAA/threads/thread-1")
    );
    assert_eq!(chat_inbound.reply_target, None);
    assert_eq!(chat_inbound.attachments[0].byte_length, 2048);
    assert_eq!(chat.cursor().recent_event_ids.len(), 1);
    assert!(chat.setup_diagnostics().callback_verified);
    let chat_setup = chat.account_setup_v2();
    chat_setup.validate().expect("Google Chat account setup");
    assert_eq!(
        chat_setup.connection_health,
        ChannelConnectionHealthV2::Connected
    );
    assert!(chat_setup.safe_test_supported);
    let chat_test_request = chat
        .prepare_test_connection()
        .expect("prepare Google Chat safe test");
    assert_eq!(chat_test_request.method, "GET");
    assert_eq!(
        chat_test_request.url,
        "https://chat.googleapis.com/v1/spaces/AAA"
    );
    std::hint::black_box(GoogleChatAdapter::test_connection);
    assert!(chat.capabilities().inbound_cards);
    chat.capabilities_v2()
        .validate()
        .expect("Google Chat v2 capabilities");
    let mut chat_route = route("google_chat", "google-work", "spaces/AAA");
    chat_route.thread = Some("spaces/AAA/threads/thread-1".to_owned());
    let chat_request = chat
        .prepare_send(&OutboundMessage {
            route: chat_route,
            idempotency_key: "google-intent-1".to_owned(),
            text: "reply from Keith".to_owned(),
            artifacts: Vec::new(),
        })
        .expect("prepare Google Chat send");
    assert_eq!(chat_request.method, "POST");
    assert!(
        chat_request
            .url
            .starts_with("https://chat.googleapis.com/v1/spaces/AAA/messages?messageReplyOption=")
    );
    assert!(
        String::from_utf8(chat_request.body)
            .expect("Google Chat request JSON")
            .contains("spaces/AAA/threads/thread-1")
    );
    chat.revoke();
    assert!(chat.setup_diagnostics().revoked);
}

#[test]
#[allow(clippy::too_many_lines)]
fn work_channels_email_threading_idempotency_and_restart() {
    let mut config = EmailConfig::provider(
        "mail-provider",
        "https://api.mail-provider.example",
        "keith-email-events",
        "email-work",
        "keith@example.com",
        profile(),
        credential("email-work"),
    );
    config.provider_guarantees_idempotency = true;
    let mut email =
        EmailAdapter::new(config.clone(), token(), EmailCursor::default()).expect("email adapter");
    let event = br#"{
      "message_id":"<email-in-1@example.net>",
      "from":"person@example.net",
      "to":["keith@example.com"],
      "subject":"Quarterly brief",
      "text_body":"Please review the attachment.",
      "html_body":null,
      "in_reply_to":"<thread-root@example.net>",
      "references":["<thread-root@example.net>"],
      "received_at":"2026-08-30T10:02:00Z",
      "attachments":[{
        "id":"attachment-1",
        "file_name":"brief.pdf",
        "media_type":"application/pdf",
        "byte_length":4096,
        "download_url":"https://mail.example.net/attachments/1"
      }]
    }"#;
    let signature_headers = vec![(
        "X-Provider-Signature".to_owned(),
        "signed-email-fixture".to_owned(),
    )];
    assert_eq!(
        email
            .ingest_provider_event(&signature_headers, event, &PinnedEmailIngress)
            .expect("verified email event"),
        EmailIngestOutcome::Queued
    );
    let AdapterEvent::Inbound(inbound) = email.receive().expect("email inbound") else {
        panic!("expected email message");
    };
    assert_eq!(inbound.conversation, "<thread-root@example.net>");
    assert_eq!(
        inbound.reply_target.as_deref(),
        Some("<thread-root@example.net>")
    );
    let artifact_id = ArtifactId::new();
    email
        .stage_artifact(
            artifact_id.clone(),
            EmailUpload {
                file_name: "answer.txt".to_owned(),
                media_type: "text/plain".to_owned(),
                bytes: b"approved".to_vec(),
            },
        )
        .expect("stage email attachment");
    let provider_request = email
        .prepare_send(&OutboundMessage {
            route: route("email", "email-work", "<thread-root@example.net>"),
            idempotency_key: "email-intent-1".to_owned(),
            text: "Approved.".to_owned(),
            artifacts: vec![artifact_id],
        })
        .expect("prepare email provider send");
    assert_eq!(provider_request.method, "POST");
    assert_eq!(
        provider_request.url,
        "https://api.mail-provider.example/messages"
    );
    assert_eq!(
        provider_request.idempotency_key.as_deref(),
        Some("email-intent-1")
    );
    assert_eq!(email.delivery_state("email-intent-1"), None);
    let provider_body = String::from_utf8(provider_request.body).expect("email provider JSON");
    assert!(provider_body.contains("In-Reply-To"));
    assert!(provider_body.contains("YXBwcm92ZWQ="));

    let mut durable_cursor = email.cursor().clone();
    durable_cursor.deliveries.insert(
        "email-uncertain-1".to_owned(),
        EmailDeliveryState::PossibleDuplicate,
    );
    let cursor_json = serde_json::to_vec(&durable_cursor).expect("serialize email cursor");
    let cursor: EmailCursor = serde_json::from_slice(&cursor_json).expect("restore email cursor");
    let mut restarted = EmailAdapter::new(config, token(), cursor).expect("restart email adapter");
    assert_eq!(
        restarted
            .ingest_provider_event(&signature_headers, event, &PinnedEmailIngress)
            .expect("deduplicate after restart"),
        EmailIngestOutcome::Duplicate
    );
    assert_eq!(restarted.setup_diagnostics().tracked_threads, 1);
    assert_eq!(restarted.setup_diagnostics().possible_duplicate_count, 1);
    let email_setup = restarted.account_setup_v2();
    email_setup.validate().expect("email account setup");
    assert_eq!(
        email_setup.connection_health,
        ChannelConnectionHealthV2::Connected
    );
    assert!(email_setup.safe_test_supported);
    let email_test_request = restarted
        .prepare_test_connection()
        .expect("prepare email safe test");
    assert_eq!(email_test_request.method, "GET");
    assert_eq!(
        email_test_request.url,
        "https://api.mail-provider.example/account"
    );
    std::hint::black_box(EmailAdapter::test_connection);
    restarted
        .capabilities_v2()
        .validate()
        .expect("email v2 capabilities");
    restarted.revoke();
    assert!(restarted.setup_diagnostics().revoked);
}

#[test]
#[allow(clippy::too_many_lines)]
fn work_channels_matrix_rich_sync_cursor_idempotency_and_revocation() {
    let sync_body = r#"{
      "next_batch":"s72595_4483_1934",
      "rooms":{"join":{"!room:example.org":{
        "state":{"events":[]},
        "timeline":{"events":[
          {
            "type":"m.room.message",
            "event_id":"$message-1",
            "sender":"@alice:example.org",
            "origin_server_ts":1788084180000,
            "content":{
              "msgtype":"m.text",
              "body":"hello Keith",
              "m.relates_to":{
                "rel_type":"m.thread",
                "event_id":"$thread-root",
                "m.in_reply_to":{"event_id":"$previous"}
              }
            }
          },
          {
            "type":"m.room.message",
            "event_id":"$edit-1",
            "sender":"@alice:example.org",
            "origin_server_ts":1788084181000,
            "content":{
              "msgtype":"m.text",
              "body":"* corrected",
              "m.new_content":{"msgtype":"m.text","body":"corrected"},
              "m.relates_to":{"rel_type":"m.replace","event_id":"$message-1"}
            }
          },
          {
            "type":"m.reaction",
            "event_id":"$reaction-1",
            "sender":"@alice:example.org",
            "origin_server_ts":1788084182000,
            "content":{"m.relates_to":{
              "rel_type":"m.annotation",
              "event_id":"$message-1",
              "key":"👍"
            }}
          }
        ]}
      },"!encrypted:example.org":{
        "state":{"events":[{
          "type":"m.room.encryption",
          "event_id":"$encryption-state",
          "sender":"@alice:example.org",
          "origin_server_ts":1788084182500,
          "content":{"algorithm":"m.megolm.v1.aes-sha2"}
        }]},
        "timeline":{"events":[{
          "type":"m.room.encrypted",
          "event_id":"$encrypted-1",
          "sender":"@alice:example.org",
          "origin_server_ts":1788084183000,
          "content":{"algorithm":"m.megolm.v1.aes-sha2","ciphertext":"opaque"}
        }]}
      }}}
    }"#;
    let config = MatrixConfig::production(
        "https://matrix.example.org",
        "matrix-work",
        "@keith:example.org",
        profile(),
        credential("matrix-work"),
    );
    let mut matrix = MatrixAdapter::new(config.clone(), token(), MatrixCursor::default())
        .expect("Matrix adapter");
    assert_eq!(
        matrix
            .ingest_sync_response(sync_body.as_bytes())
            .expect("Matrix sync response"),
        4
    );
    let normalized = (0..4)
        .map(|_| matrix.receive_rich().expect("Matrix normalized event"))
        .collect::<Vec<_>>();
    let message_index = normalized
        .iter()
        .position(|event| matches!(event, MatrixNormalizedEvent::Message(_)))
        .expect("Matrix message");
    let edit_index = normalized
        .iter()
        .position(|event| matches!(event, MatrixNormalizedEvent::Edit { .. }))
        .expect("Matrix edit");
    let reaction_index = normalized
        .iter()
        .position(|event| matches!(event, MatrixNormalizedEvent::Reaction { .. }))
        .expect("Matrix reaction");
    assert!(message_index < edit_index && edit_index < reaction_index);
    let MatrixNormalizedEvent::Message(message) = &normalized[message_index] else {
        unreachable!("message variant was selected above");
    };
    assert_eq!(message.thread.as_deref(), Some("$thread-root"));
    assert_eq!(message.reply_target.as_deref(), Some("$previous"));
    let MatrixNormalizedEvent::Edit {
        target_event_id,
        text,
        ..
    } = &normalized[edit_index]
    else {
        unreachable!("edit variant was selected above");
    };
    assert_eq!(target_event_id, "$message-1");
    assert_eq!(text, "corrected");
    let MatrixNormalizedEvent::Reaction { key, .. } = &normalized[reaction_index] else {
        unreachable!("reaction variant was selected above");
    };
    assert_eq!(key, "👍");
    assert!(matches!(
        normalized
            .iter()
            .find(|event| matches!(event, MatrixNormalizedEvent::UnsupportedEncrypted { .. })),
        Some(MatrixNormalizedEvent::UnsupportedEncrypted { .. })
    ));
    assert_eq!(
        matrix.cursor().next_batch.as_deref(),
        Some("s72595_4483_1934")
    );
    assert!(!matrix.capabilities().end_to_end_encryption);
    assert_eq!(matrix.setup_diagnostics().encrypted_room_count, 1);
    let matrix_setup = matrix.account_setup_v2();
    matrix_setup.validate().expect("Matrix account setup");
    assert_eq!(
        matrix_setup.connection_health,
        ChannelConnectionHealthV2::Connected
    );
    assert!(matrix_setup.safe_test_supported);
    let matrix_test_request = matrix
        .prepare_test_connection()
        .expect("prepare Matrix safe test");
    assert_eq!(matrix_test_request.method, "GET");
    assert_eq!(
        matrix_test_request.url,
        "https://matrix.example.org/_matrix/client/v3/account/whoami"
    );
    std::hint::black_box(MatrixAdapter::test_connection);
    matrix
        .capabilities_v2()
        .validate()
        .expect("Matrix v2 capabilities");
    let cursor_json = serde_json::to_vec(matrix.cursor()).expect("serialize Matrix cursor");
    let durable_cursor: MatrixCursor =
        serde_json::from_slice(&cursor_json).expect("restore Matrix cursor");
    let mut restarted_matrix =
        MatrixAdapter::new(config, token(), durable_cursor).expect("restart Matrix adapter");
    assert_eq!(
        restarted_matrix
            .ingest_sync_response(sync_body.as_bytes())
            .expect("deduplicate Matrix sync after restart"),
        0
    );

    let edit_request = matrix
        .prepare_room_event(
            "!room:example.org",
            "m.room.message",
            "matrix-intent-1",
            &json!({
                "msgtype": "m.text",
                "body": "* final wording",
                "m.new_content": {"msgtype": "m.text", "body": "final wording"},
                "m.relates_to": {"rel_type": "m.replace", "event_id": "$message-1"},
            }),
        )
        .expect("prepare Matrix idempotent edit");
    assert_eq!(edit_request.method, "PUT");
    assert!(edit_request.url.contains(
        "/_matrix/client/v3/rooms/%21room%3Aexample.org/send/m.room.message/matrix-intent-1"
    ));
    assert!(
        String::from_utf8(edit_request.body)
            .expect("Matrix edit JSON")
            .contains("m.replace")
    );
    let receipt_request = matrix
        .prepare_read_receipt("!room:example.org", "$message-1")
        .expect("prepare Matrix receipt");
    assert!(
        receipt_request
            .url
            .contains("/_matrix/client/v3/rooms/%21room%3Aexample.org/receipt/m.read/%24message-1")
    );
    std::hint::black_box(MatrixAdapter::send_read_receipt);

    matrix
        .stage_artifact(
            ArtifactId::new(),
            MatrixUpload {
                file_name: "before-revoke.txt".to_owned(),
                media_type: "text/plain".to_owned(),
                bytes: b"bounded".to_vec(),
            },
        )
        .expect("stage Matrix media");
    matrix.revoke();
    let failure = matrix.receive_rich().expect_err("revoked Matrix account");
    assert_eq!(failure.class, RetryClass::Permanent);
    assert!(matrix.setup_diagnostics().revoked);
}

#[test]
fn work_channels_matrix_malformed_sync_does_not_advance_durable_state() {
    let config = MatrixConfig::production(
        "https://matrix.example.org",
        "matrix-transactional",
        "@keith:example.org",
        profile(),
        credential("matrix-transactional"),
    );
    let mut matrix =
        MatrixAdapter::new(config, token(), MatrixCursor::default()).expect("Matrix adapter");
    let malformed = br#"{
      "next_batch":"batch-malformed",
      "rooms":{"join":{"!room:example.org":{"timeline":{"events":[
        {
          "type":"m.room.message",
          "event_id":"$kept-after-retry",
          "sender":"@alice:example.org",
          "origin_server_ts":1788084180000,
          "content":{"msgtype":"m.text","body":"must survive retry"}
        },
        {
          "type":"m.room.message",
          "event_id":"$malformed",
          "sender":"@alice:example.org",
          "origin_server_ts":1788084181000,
          "content":{"msgtype":"m.text","body":""}
        }
      ]}}}}
    }"#;
    matrix
        .ingest_sync_response(malformed)
        .expect_err("malformed sync is rejected transactionally");
    assert_eq!(matrix.cursor(), &MatrixCursor::default());

    let corrected = br#"{
      "next_batch":"batch-corrected",
      "rooms":{"join":{"!room:example.org":{"timeline":{"events":[
        {
          "type":"m.room.message",
          "event_id":"$kept-after-retry",
          "sender":"@alice:example.org",
          "origin_server_ts":1788084180000,
          "content":{"msgtype":"m.text","body":"must survive retry"}
        },
        {
          "type":"m.room.message",
          "event_id":"$corrected",
          "sender":"@alice:example.org",
          "origin_server_ts":1788084181000,
          "content":{"msgtype":"m.text","body":"now valid"}
        }
      ]}}}}
    }"#;
    assert_eq!(
        matrix
            .ingest_sync_response(corrected)
            .expect("corrected sync is accepted"),
        2
    );
    let MatrixNormalizedEvent::Message(first) =
        matrix.receive_rich().expect("first retried Matrix event")
    else {
        panic!("expected first Matrix message");
    };
    assert_eq!(first.message_id, "$kept-after-retry");
}

#[test]
fn work_channels_reject_unverified_cross_account_and_unsupported_paths() {
    let mut teams = TeamsAdapter::new(
        TeamsConfig::production(
            "bot-app-id",
            "teams-isolated",
            profile(),
            credential("teams-isolated"),
        ),
        token(),
        TeamsCursor::default(),
    )
    .expect("Teams adapter");
    let auth_failure = teams
        .ingest_verified_activity("Bearer wrong", b"not-json", &PinnedTeamsIngress)
        .expect_err("authentication precedes JSON parsing");
    assert_eq!(auth_failure.class, RetryClass::Permanent);
    assert!(auth_failure.safe_message.contains("authentication failed"));

    let hostile_service_url = br#"{
      "type":"message",
      "id":"signed-but-hostile",
      "timestamp":"2026-08-30T10:00:00Z",
      "serviceUrl":"https://attacker.example",
      "from":{"id":"29:user"},
      "conversation":{"id":"conversation-1"},
      "text":"redirect the bot token"
    }"#;
    let service_url_failure = teams
        .ingest_verified_activity(
            "Bearer signed-teams-fixture",
            hostile_service_url,
            &PinnedTeamsIngress,
        )
        .expect_err("signed service URL must match the verified JWT claim");
    assert!(service_url_failure.safe_message.contains("service URL"));

    let cross_account = teams
        .send(&OutboundMessage {
            route: route("teams", "another-account", "conversation"),
            idempotency_key: "cross-account".to_owned(),
            text: "must not send".to_owned(),
            artifacts: Vec::new(),
        })
        .expect_err("cross-account route denied");
    assert_eq!(cross_account.class, RetryClass::Permanent);

    let wrong_credential = TeamsAdapter::new(
        TeamsConfig::production(
            "bot-app-id",
            "teams-isolated",
            profile(),
            credential("different-account"),
        ),
        token(),
        TeamsCursor::default(),
    )
    .err()
    .expect("credential scope denied");
    assert_eq!(wrong_credential.class, RetryClass::Permanent);

    let chat = GoogleChatAdapter::new(
        GoogleChatConfig::production(
            "https://keith.example/chat",
            "google-isolated",
            profile(),
            credential("google-isolated"),
        ),
        token(),
        GoogleChatCursor::default(),
    )
    .expect("Google Chat adapter");
    let unsafe_route = chat
        .prepare_send(&OutboundMessage {
            route: route(
                "google_chat",
                "google-isolated",
                "spaces/AAA?messageReplyOption=REPLY_MESSAGE_FALLBACK_TO_NEW_THREAD",
            ),
            idempotency_key: "google-unsafe-route".to_owned(),
            text: "must not send".to_owned(),
            artifacts: Vec::new(),
        })
        .expect_err("Google Chat route query injection denied");
    assert_eq!(unsafe_route.class, RetryClass::Permanent);

    teams.revoke();
    assert!(teams.setup_diagnostics().revoked);
    assert_eq!(
        teams.reconnect().expect_err("revoked Teams account").class,
        RetryClass::Permanent
    );
}
