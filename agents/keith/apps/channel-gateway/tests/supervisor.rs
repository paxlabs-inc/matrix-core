use std::collections::{BTreeMap, BTreeSet};
use std::fs;
use std::path::PathBuf;

use keith_agent_types::{ProfileId, SessionId, UtcTimestamp};
use keith_channel_adapters::{
    ChannelAccountLifecycle, ChannelAdapterKind, DiscordAdapter, DiscordConfig, DiscordCursor,
    MatrixAdapter, MatrixConfig, MatrixCursor,
};
use keith_channel_core::{
    CHANNEL_CONTRACT_V2, ChannelAdapterErrorKindV2, ChannelAdapterErrorV2,
    ChannelConversationKindV2, ChannelConversationV2, ChannelEventKindV2, ChannelEventV2,
    ChannelIdentityV2, ChannelMessageV2, ReconnectPolicy,
};
use keith_channel_gateway::{
    AccountQueueLimits, ChannelAccountConfig, ChannelAccountKey, ChannelGatewayError,
    ChannelGatewayRuntime, ChannelGatewaySupervisor, ChannelRouteRule, GatewayGroupPolicy,
    NotificationBudgetConfig, RawWebhookIngress, VerifiedWebhookIngress, WebhookIngressRouter,
};
use keith_credentials::{CredentialOwner, CredentialRef, SecretValue};
use sha2::{Digest, Sha256};

struct Scratch {
    root: PathBuf,
}

impl Scratch {
    fn new(label: &str) -> Self {
        let root = std::env::temp_dir().join(format!(
            "keith-channel-gateway-{label}-{}-{}",
            std::process::id(),
            UtcTimestamp::now().expect("time").unix_millis()
        ));
        fs::create_dir_all(&root).expect("scratch directory");
        Self { root }
    }

    fn store(&self) -> PathBuf {
        self.root.join("accounts.json")
    }
}

impl Drop for Scratch {
    fn drop(&mut self) {
        if self
            .root
            .file_name()
            .and_then(|name| name.to_str())
            .is_some_and(|name| name.starts_with("keith-channel-gateway-"))
        {
            let _ = fs::remove_dir_all(&self.root);
        }
    }
}

fn credential(account: &str, name: &str) -> CredentialRef {
    CredentialRef::new(name, CredentialOwner::Channel(account.to_owned()))
        .expect("credential reference")
}

fn adapter(account: &str) -> DiscordAdapter {
    DiscordAdapter::new(
        DiscordConfig::production(account, 1 << 9),
        SecretValue::new(format!("runtime-secret-{account}").into_bytes()).expect("secret"),
        DiscordCursor::default(),
    )
    .expect("Discord adapter")
}

fn route(id: &str, conversation: Option<&str>) -> ChannelRouteRule {
    ChannelRouteRule {
        id: id.to_owned(),
        enabled: true,
        priority: 10,
        conversation: conversation.map(str::to_owned),
        thread: None,
        sender: None,
        profile_id: ProfileId::new(),
        session_id: SessionId::new(),
        group: GatewayGroupPolicy {
            require_mention: false,
            shared_memory: true,
            retained_participants: BTreeSet::new(),
            tool_principals: BTreeSet::new(),
            schedule_principals: BTreeSet::new(),
            proactive_posts: true,
        },
    }
}

fn config(account: &str) -> ChannelAccountConfig {
    ChannelAccountConfig {
        key: ChannelAccountKey::new(ChannelAdapterKind::Discord, account),
        enabled: true,
        credential_refs: BTreeMap::from([(
            "bot_token".to_owned(),
            credential(account, &format!("{account}-token")),
        )]),
        credential_generation: 1,
        queue_limits: AccountQueueLimits {
            concurrency: 1,
            pending: 64,
            pending_per_session: 64,
            seen_messages: 256,
            ..AccountQueueLimits::default()
        },
        notification_budget: NotificationBudgetConfig {
            capacity: 2,
            window_ms: 500,
        },
        routes: vec![route(&format!("route-{account}"), None)],
    }
}

fn event(account: &str, conversation: &str, message_id: &str) -> ChannelEventV2 {
    ChannelEventV2 {
        contract: CHANNEL_CONTRACT_V2,
        event_id: message_id.to_owned(),
        delivery_attempt: 1,
        event: ChannelEventKindV2::MessageCreated(ChannelMessageV2 {
            message_id: message_id.to_owned(),
            account_id: account.to_owned(),
            conversation: ChannelConversationV2 {
                platform_id: conversation.to_owned(),
                kind: ChannelConversationKindV2::Direct,
                thread_id: None,
                reply_to_message_id: None,
            },
            sender: ChannelIdentityV2 {
                platform_id: format!("sender-{conversation}"),
                display_name: None,
                is_bot: false,
            },
            text: format!("message-{message_id}"),
            attachments: Vec::new(),
            rich_content: Vec::new(),
            mentions: Vec::new(),
            occurred_at: UtcTimestamp::from_unix_millis(1_000),
            metadata: BTreeMap::new(),
        }),
        metadata: BTreeMap::new(),
    }
}

fn add(supervisor: &mut ChannelGatewaySupervisor, account: &str, now: UtcTimestamp) {
    let adapter = adapter(account);
    supervisor
        .add_managed_account(config(account), &adapter, now)
        .expect("account registration");
}

#[test]
fn heterogeneous_real_adapters_share_fair_scheduling_without_sharing_account_state() {
    let scratch = Scratch::new("heterogeneous");
    let now = UtcTimestamp::from_unix_millis(5_000);
    let mut supervisor = ChannelGatewaySupervisor::open(scratch.store()).expect("supervisor");
    add(&mut supervisor, "discord-heterogeneous", now);

    let matrix_account = "matrix-heterogeneous";
    let matrix = MatrixAdapter::new(
        MatrixConfig::production(
            "https://matrix.example",
            matrix_account,
            "@keith:example",
            ProfileId::new(),
            credential(matrix_account, "matrix-runtime-access"),
        ),
        SecretValue::new(b"matrix-runtime-secret".to_vec()).expect("matrix secret"),
        MatrixCursor::default(),
    )
    .expect("Matrix adapter");
    let mut matrix_config = config(matrix_account);
    matrix_config.key.kind = ChannelAdapterKind::Matrix;
    matrix_config.credential_refs = BTreeMap::from([(
        "access_token".to_owned(),
        credential(matrix_account, "matrix-runtime-access"),
    )]);
    supervisor
        .add_managed_account(matrix_config, &matrix, now)
        .expect("Matrix account");

    let discord_key = ChannelAccountKey::new(ChannelAdapterKind::Discord, "discord-heterogeneous");
    let matrix_key = ChannelAccountKey::new(ChannelAdapterKind::Matrix, matrix_account);
    supervisor
        .enqueue(
            &discord_key,
            &event(
                "discord-heterogeneous",
                "discord-conversation",
                "discord-message",
            ),
            now,
        )
        .expect("Discord enqueue");
    supervisor
        .enqueue(
            &matrix_key,
            &event(matrix_account, "matrix-room", "matrix-message"),
            now,
        )
        .expect("Matrix enqueue");
    let dispatched = [
        supervisor.take_fair(now).expect("first dispatch").account,
        supervisor.take_fair(now).expect("second dispatch").account,
    ];
    assert!(dispatched.contains(&discord_key));
    assert!(dispatched.contains(&matrix_key));
    assert_eq!(
        supervisor
            .delivery_partition(&matrix_key)
            .expect("Matrix partition")
            .channel,
        "matrix"
    );
}

#[test]
#[allow(clippy::too_many_lines)]
fn multiple_accounts_are_fair_isolated_durable_rotatable_and_removable() {
    let scratch = Scratch::new("supervision");
    let now = UtcTimestamp::from_unix_millis(10_000);
    let mut supervisor = ChannelGatewaySupervisor::open(scratch.store()).expect("supervisor");
    for account in ["discord-a", "discord-b", "discord-c", "discord-d"] {
        add(&mut supervisor, account, now);
        for index in 0..4 {
            supervisor
                .enqueue(
                    &ChannelAccountKey::new(ChannelAdapterKind::Discord, account),
                    &event(
                        account,
                        &format!("conversation-{account}"),
                        &format!("{account}-{index}"),
                    ),
                    now,
                )
                .expect("enqueue");
        }
    }

    let mut first_round = Vec::new();
    for _ in 0..4 {
        let dispatched = supervisor.take_fair(now).expect("fair dispatch");
        first_round.push(dispatched.account.account_id.clone());
        supervisor
            .complete(&dispatched.account, &dispatched.routed.session_id, now)
            .expect("complete");
    }
    assert_eq!(
        first_round,
        ["discord-a", "discord-b", "discord-c", "discord-d"]
    );

    let isolated = supervisor
        .enqueue(
            &ChannelAccountKey::new(ChannelAdapterKind::Discord, "discord-a"),
            &event("discord-b", "foreign", "foreign-1"),
            now,
        )
        .expect_err("cross-account event must fail");
    assert!(matches!(isolated, ChannelGatewayError::CrossAccount));

    let key = ChannelAccountKey::new(ChannelAdapterKind::Discord, "discord-a");
    let partition = supervisor.delivery_partition(&key).expect("partition");
    assert!(partition.matches("discord", "discord-a"));
    assert!(!partition.matches("discord", "discord-b"));
    let rotated = supervisor
        .rotate_credential(
            &key,
            "bot_token",
            credential("discord-a", "discord-a-token-v2"),
            now,
        )
        .expect("rotation");
    assert_eq!(rotated.credential_generation, 2);

    supervisor
        .enqueue(&key, &event("discord-a", "restart", "restart-1"), now)
        .expect("durable enqueue");
    drop(supervisor);

    let mut restarted = ChannelGatewaySupervisor::open(scratch.store()).expect("restart");
    assert_eq!(restarted.health().len(), 4);
    assert_eq!(
        restarted
            .record(&key)
            .expect("restored account")
            .config
            .credential_generation,
        2
    );
    let restored = (0..16)
        .find_map(|_| restarted.take_fair(now))
        .expect("restored inbound");
    assert!(
        restarted
            .record(&restored.account)
            .expect("account")
            .pending_inbound
            .iter()
            .any(|pending| pending.routed.message.message_id == restored.routed.message.message_id)
    );
    restarted
        .complete(&restored.account, &restored.routed.session_id, now)
        .expect("complete restored");
    restarted.remove_account(&key).expect("remove account");
    drop(restarted);
    let reopened = ChannelGatewaySupervisor::open(scratch.store()).expect("removed restart");
    assert!(reopened.record(&key).is_none());

    let persisted = fs::read_to_string(scratch.store()).expect("persisted account store");
    assert!(!persisted.contains("runtime-secret"));
}

#[test]
fn routing_fails_closed_on_ambiguity_and_webhooks_require_authentication_and_replay_guard() {
    let scratch = Scratch::new("routing");
    let now = UtcTimestamp::from_unix_millis(50_000);
    let mut supervisor = ChannelGatewaySupervisor::open(scratch.store()).expect("supervisor");
    let account = "discord-routes";
    let adapter = adapter(account);
    let mut account_config = config(account);
    let mut competing = route("competing", None);
    competing.profile_id = ProfileId::new();
    competing.session_id = SessionId::new();
    account_config.routes.push(competing);
    supervisor
        .add_managed_account(account_config, &adapter, now)
        .expect("account");
    let key = ChannelAccountKey::new(ChannelAdapterKind::Discord, account);
    assert!(matches!(
        supervisor
            .enqueue(&key, &event(account, "conversation", "ambiguous"), now)
            .expect_err("ambiguous route"),
        ChannelGatewayError::AmbiguousRoute
    ));

    let webhook_store = scratch.root.join("webhook-replay.json");
    let mut webhooks = WebhookIngressRouter::open(&webhook_store, 8).expect("webhook router");
    webhooks.register(&key).expect("webhook account");
    let body = br#"{"type":"message","id":"delivery-1"}"#.to_vec();
    let secret = b"webhook-authentication-key";
    let signature = sign(secret, &body);
    let raw = RawWebhookIngress {
        account: key.clone(),
        delivery_id: "delivery-1".to_owned(),
        received_at: now,
        signature: signature.clone(),
        body: body.clone(),
    };
    assert!(
        VerifiedWebhookIngress::verify(raw.clone(), now, 1024, 1_000, |_, bytes, presented| {
            sign(secret, bytes) == presented
        })
        .is_ok()
    );
    assert!(matches!(
        VerifiedWebhookIngress::verify(raw.clone(), now, 1024, 1_000, |_, _, _| false),
        Err(ChannelGatewayError::WebhookAuthentication)
    ));
    let admitted =
        VerifiedWebhookIngress::verify(raw.clone(), now, 1024, 1_000, |_, bytes, presented| {
            sign(secret, bytes) == presented
        })
        .expect("verified webhook");
    webhooks.admit(admitted).expect("first delivery");
    drop(webhooks);
    let mut webhooks =
        WebhookIngressRouter::open(&webhook_store, 8).expect("restart webhook router");
    let replay = VerifiedWebhookIngress::verify(raw, now, 1024, 1_000, |_, bytes, presented| {
        sign(secret, bytes) == presented
    })
    .expect("verified replay");
    assert!(matches!(
        webhooks.admit(replay),
        Err(ChannelGatewayError::WebhookReplay)
    ));
}

#[test]
fn isolated_real_adapter_workers_contain_one_accounts_terminal_event_failure() {
    let scratch = Scratch::new("workers");
    let now = UtcTimestamp::from_unix_millis(75_000);
    let mut supervisor = ChannelGatewaySupervisor::open(scratch.store()).expect("supervisor");
    add(&mut supervisor, "worker-a", now);
    add(&mut supervisor, "worker-b", now);
    let key_a = ChannelAccountKey::new(ChannelAdapterKind::Discord, "worker-a");
    let key_b = ChannelAccountKey::new(ChannelAdapterKind::Discord, "worker-b");
    let mut runtime = ChannelGatewayRuntime::new(supervisor);
    runtime
        .attach_worker(
            key_a.clone(),
            Box::new(adapter("worker-a")),
            ReconnectPolicy::default(),
            true,
        )
        .expect("worker A");
    runtime
        .attach_worker(
            key_b.clone(),
            Box::new(adapter("worker-b")),
            ReconnectPolicy::default(),
            true,
        )
        .expect("worker B");

    let mut malformed = event("worker-a", "conversation-a", "malformed-a");
    malformed.delivery_attempt = 0;
    runtime
        .admit_verified_event(&key_a, malformed)
        .expect("submit malformed normalized event");
    runtime
        .admit_verified_event(&key_b, event("worker-b", "conversation-b", "accepted-b"))
        .expect("submit valid normalized event");

    for _ in 0..10_000 {
        runtime.maintain(now).expect("maintain workers");
        let health = runtime.supervisor().health();
        let a_failed = health.iter().any(|account| {
            account.account_id == "worker-a" && account.lifecycle == ChannelAccountLifecycle::Failed
        });
        let b_running = health.iter().any(|account| {
            account.account_id == "worker-b"
                && account.lifecycle == ChannelAccountLifecycle::Running
        });
        if a_failed && b_running {
            break;
        }
        std::thread::yield_now();
    }
    let health = runtime.supervisor().health();
    assert!(health.iter().any(|account| {
        account.account_id == "worker-a" && account.lifecycle == ChannelAccountLifecycle::Failed
    }));
    assert!(health.iter().any(|account| {
        account.account_id == "worker-b" && account.lifecycle == ChannelAccountLifecycle::Running
    }));
    let dispatched = runtime
        .supervisor_mut()
        .take_fair(now)
        .expect("healthy account dispatch");
    assert_eq!(dispatched.account, key_b);
    runtime.remove_account(&key_a).expect("remove worker A");
    runtime.remove_account(&key_b).expect("remove worker B");
}

#[test]
fn throttling_and_group_policy_are_account_local_and_survive_fair_scheduling() {
    let scratch = Scratch::new("rate-limit");
    let now = UtcTimestamp::from_unix_millis(100_000);
    let mut supervisor = ChannelGatewaySupervisor::open(scratch.store()).expect("supervisor");
    add(&mut supervisor, "throttled", now);
    add(&mut supervisor, "healthy", now);
    let throttled = ChannelAccountKey::new(ChannelAdapterKind::Discord, "throttled");
    let healthy = ChannelAccountKey::new(ChannelAdapterKind::Discord, "healthy");
    supervisor
        .enqueue(
            &throttled,
            &event("throttled", "conversation-a", "throttled-1"),
            now,
        )
        .expect("throttled enqueue");
    supervisor
        .enqueue(
            &healthy,
            &event("healthy", "conversation-b", "healthy-1"),
            now,
        )
        .expect("healthy enqueue");
    supervisor
        .record_adapter_error(
            &throttled,
            &ChannelAdapterErrorV2 {
                kind: ChannelAdapterErrorKindV2::RateLimit,
                safe_message: "account-local rate limit".to_owned(),
                retry_after_ms: Some(500),
            },
            now,
        )
        .expect("rate limit");
    let ready = supervisor
        .take_fair(now)
        .expect("healthy account remains ready");
    assert_eq!(ready.account, healthy);
    supervisor
        .complete(&ready.account, &ready.routed.session_id, now)
        .expect("complete healthy");
    assert!(
        supervisor
            .release_expired_rate_limits(UtcTimestamp::from_unix_millis(100_499))
            .expect("early release")
            .is_empty()
    );
    assert_eq!(
        supervisor
            .release_expired_rate_limits(UtcTimestamp::from_unix_millis(100_500))
            .expect("release")
            .as_slice(),
        std::slice::from_ref(&throttled)
    );
    let resumed = supervisor
        .take_fair(UtcTimestamp::from_unix_millis(100_500))
        .expect("resumed account");
    assert_eq!(resumed.account, throttled);

    let group_account = "group-account";
    let group_adapter = adapter(group_account);
    let mut group_config = config(group_account);
    group_config.routes[0].group.require_mention = true;
    supervisor
        .add_managed_account(group_config, &group_adapter, now)
        .expect("group account");
    let mut group_event = event(group_account, "group", "group-1");
    let ChannelEventKindV2::MessageCreated(message) = &mut group_event.event else {
        unreachable!();
    };
    message.conversation.kind = ChannelConversationKindV2::Channel;
    assert!(matches!(
        supervisor
            .enqueue(
                &ChannelAccountKey::new(ChannelAdapterKind::Discord, group_account),
                &group_event,
                now,
            )
            .expect_err("group mention policy"),
        ChannelGatewayError::MentionRequired
    ));
}

#[test]
fn proactive_notification_budgets_are_durable_and_account_local() {
    let scratch = Scratch::new("notification-budget");
    let now = UtcTimestamp::now().expect("time");
    let mut supervisor = ChannelGatewaySupervisor::open(scratch.store()).expect("supervisor");
    add(&mut supervisor, "budget-a", now);
    add(&mut supervisor, "budget-b", now);
    let key_a = ChannelAccountKey::new(ChannelAdapterKind::Discord, "budget-a");
    let key_b = ChannelAccountKey::new(ChannelAdapterKind::Discord, "budget-b");
    assert_eq!(
        supervisor
            .reserve_proactive_notification(&key_a, "route-budget-a", now)
            .expect("first reservation"),
        1
    );
    assert_eq!(
        supervisor
            .reserve_proactive_notification(&key_a, "route-budget-a", now)
            .expect("second reservation"),
        0
    );
    assert!(matches!(
        supervisor
            .reserve_proactive_notification(&key_a, "route-budget-a", now)
            .expect_err("account A budget exhausted"),
        ChannelGatewayError::NotificationBudgetExhausted
    ));
    assert_eq!(
        supervisor
            .reserve_proactive_notification(&key_b, "route-budget-b", now)
            .expect("account B remains available"),
        1
    );
    drop(supervisor);

    let mut restarted = ChannelGatewaySupervisor::open(scratch.store()).expect("restart");
    assert_eq!(
        restarted
            .record(&key_a)
            .expect("restored A")
            .notification_budget
            .remaining,
        0
    );
    assert_eq!(
        restarted
            .reserve_proactive_notification(
                &key_a,
                "route-budget-a",
                UtcTimestamp::from_unix_millis(now.unix_millis().saturating_add(500)),
            )
            .expect("window reset"),
        1
    );
}

fn sign(secret: &[u8], body: &[u8]) -> String {
    let mut digest = Sha256::new();
    digest.update(secret);
    digest.update(body);
    format!("{:x}", digest.finalize())
}
