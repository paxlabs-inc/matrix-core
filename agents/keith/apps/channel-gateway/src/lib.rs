#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet, VecDeque};
use std::fs::{self, File};
use std::path::PathBuf;
use std::sync::mpsc::{self, Receiver, Sender, TryRecvError};
use std::thread;
use std::time::Duration;

use keith_agent_types::{ProfileId, SessionId, UtcTimestamp};
use keith_channel_adapters::{
    ChannelAccountHealth, ChannelAccountLifecycle, ChannelAdapterCatalog, ChannelAdapterKind,
    ChannelCatalogError, ManagedChannelAdapter,
};
use keith_channel_core::{
    CHANNEL_CONTRACT_V2, ChannelAccountSetupV2, ChannelAdapterErrorKindV2, ChannelAdapterErrorV2,
    ChannelCapabilitiesV2, ChannelCommandV2, ChannelConnectionHealthV2, ChannelConversationKindV2,
    ChannelEventKindV2, ChannelEventV2, ChannelMessageV2, ChannelOperationReceiptV2,
    ChannelOperationV2, EnqueueOutcome, GatewayLimits, GatewayQueue, InboundIntent, InboundMessage,
    ReconnectCursorV2, ReconnectPolicy, RoutedInbound,
};
use keith_credentials::{CredentialOwner, CredentialRef};
use serde::{Deserialize, Serialize};

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelAccountKey {
    pub kind: ChannelAdapterKind,
    pub account_id: String,
}

impl ChannelAccountKey {
    pub fn new(kind: ChannelAdapterKind, account_id: impl Into<String>) -> Self {
        Self {
            kind,
            account_id: account_id.into(),
        }
    }

    fn validate(&self) -> Result<(), ChannelGatewayError> {
        if self.account_id.trim().is_empty() {
            Err(ChannelGatewayError::Invalid(
                "channel account identity is empty".to_owned(),
            ))
        } else {
            Ok(())
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AccountQueueLimits {
    pub concurrency: usize,
    pub pending: usize,
    pub pending_per_session: usize,
    pub seen_messages: usize,
    pub attachments: usize,
    pub attachment_bytes: u64,
    pub total_attachment_bytes: u64,
}

impl Default for AccountQueueLimits {
    fn default() -> Self {
        let limits = GatewayLimits::default();
        Self {
            concurrency: limits.global_concurrency,
            pending: limits.max_pending,
            pending_per_session: limits.max_pending_per_session,
            seen_messages: limits.max_seen_messages,
            attachments: limits.max_attachments,
            attachment_bytes: limits.max_attachment_bytes,
            total_attachment_bytes: limits.max_total_attachment_bytes,
        }
    }
}

impl AccountQueueLimits {
    fn gateway(self) -> GatewayLimits {
        GatewayLimits {
            global_concurrency: self.concurrency,
            max_pending: self.pending,
            max_pending_per_session: self.pending_per_session,
            max_seen_messages: self.seen_messages,
            max_attachments: self.attachments,
            max_attachment_bytes: self.attachment_bytes,
            max_total_attachment_bytes: self.total_attachment_bytes,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NotificationBudgetConfig {
    pub capacity: u32,
    pub window_ms: u64,
}

impl Default for NotificationBudgetConfig {
    fn default() -> Self {
        Self {
            capacity: 64,
            window_ms: 60_000,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NotificationBudgetState {
    pub remaining: u32,
    pub resets_at: UtcTimestamp,
}

impl Default for NotificationBudgetState {
    fn default() -> Self {
        Self {
            remaining: u32::MAX,
            resets_at: UtcTimestamp::UNIX_EPOCH,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(clippy::struct_excessive_bools)]
pub struct GatewayGroupPolicy {
    pub require_mention: bool,
    pub shared_memory: bool,
    pub retained_participants: BTreeSet<String>,
    pub tool_principals: BTreeSet<String>,
    pub schedule_principals: BTreeSet<String>,
    pub proactive_posts: bool,
}

impl Default for GatewayGroupPolicy {
    fn default() -> Self {
        Self {
            require_mention: true,
            shared_memory: false,
            retained_participants: BTreeSet::new(),
            tool_principals: BTreeSet::new(),
            schedule_principals: BTreeSet::new(),
            proactive_posts: false,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelRouteRule {
    pub id: String,
    pub enabled: bool,
    pub priority: i32,
    pub conversation: Option<String>,
    pub thread: Option<String>,
    pub sender: Option<String>,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub group: GatewayGroupPolicy,
}

impl ChannelRouteRule {
    fn validate(&self) -> Result<(), ChannelGatewayError> {
        if self.id.trim().is_empty()
            || [
                self.conversation.as_deref(),
                self.thread.as_deref(),
                self.sender.as_deref(),
            ]
            .into_iter()
            .flatten()
            .any(|value| value.trim().is_empty())
            || self
                .group
                .retained_participants
                .iter()
                .chain(&self.group.tool_principals)
                .chain(&self.group.schedule_principals)
                .any(|value| value.trim().is_empty())
        {
            return Err(ChannelGatewayError::Invalid(
                "channel route rule is malformed".to_owned(),
            ));
        }
        Ok(())
    }

    fn specificity(&self) -> u8 {
        u8::from(self.conversation.is_some())
            + u8::from(self.thread.is_some())
            + u8::from(self.sender.is_some())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelAccountConfig {
    pub key: ChannelAccountKey,
    pub enabled: bool,
    pub credential_refs: BTreeMap<String, CredentialRef>,
    pub credential_generation: u64,
    pub queue_limits: AccountQueueLimits,
    #[serde(default)]
    pub notification_budget: NotificationBudgetConfig,
    pub routes: Vec<ChannelRouteRule>,
}

impl ChannelAccountConfig {
    fn validate(&self, catalog: &ChannelAdapterCatalog) -> Result<(), ChannelGatewayError> {
        self.key.validate()?;
        let definition = catalog
            .definition(self.key.kind)
            .ok_or(ChannelGatewayError::UnknownAccount)?;
        for name in &definition.required_credential_names {
            let reference = self.credential_refs.get(name).ok_or_else(|| {
                ChannelGatewayError::Invalid(format!(
                    "channel account is missing credential reference {name}"
                ))
            })?;
            if !matches!(
                &reference.owner,
                CredentialOwner::Channel(owner) if owner == &self.key.account_id
            ) {
                return Err(ChannelGatewayError::Invalid(format!(
                    "credential reference {name} belongs to another account"
                )));
            }
        }
        GatewayQueue::new(self.queue_limits.gateway())
            .map_err(|error| ChannelGatewayError::Invalid(error.to_string()))?;
        if self.notification_budget.capacity == 0 || self.notification_budget.window_ms == 0 {
            return Err(ChannelGatewayError::Invalid(
                "channel notification budget must have positive capacity and window".to_owned(),
            ));
        }
        if self.routes.is_empty() {
            return Err(ChannelGatewayError::Invalid(
                "channel account has no deterministic route".to_owned(),
            ));
        }
        for route in &self.routes {
            route.validate()?;
        }
        let unique = self
            .routes
            .iter()
            .map(|route| route.id.as_str())
            .collect::<BTreeSet<_>>();
        if unique.len() != self.routes.len() {
            return Err(ChannelGatewayError::Invalid(
                "channel route identities are duplicated".to_owned(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChannelAccountRecord {
    pub config: ChannelAccountConfig,
    pub setup: ChannelAccountSetupV2,
    pub capabilities: ChannelCapabilitiesV2,
    pub health: ChannelAccountHealth,
    #[serde(default)]
    pub reconnect_cursor: Option<ReconnectCursorV2>,
    #[serde(default)]
    pub notification_budget: NotificationBudgetState,
    #[serde(default)]
    pub pending_inbound: Vec<DurableInbound>,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
#[allow(clippy::struct_excessive_bools)]
pub struct DurableInbound {
    pub route_id: String,
    pub shared_group_memory: bool,
    pub tools_allowed: bool,
    pub schedules_allowed: bool,
    pub proactive_posts_allowed: bool,
    pub routed: RoutedInbound,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct PersistedAccounts {
    version: u16,
    accounts: Vec<ChannelAccountRecord>,
}

pub struct ChannelAccountStore {
    path: PathBuf,
    accounts: BTreeMap<ChannelAccountKey, ChannelAccountRecord>,
}

impl ChannelAccountStore {
    /// Opens or creates the durable, secret-free account registry.
    ///
    /// # Errors
    ///
    /// Returns an error for malformed, duplicate, or unreadable state.
    pub fn open(path: impl Into<PathBuf>) -> Result<Self, ChannelGatewayError> {
        let path = path.into();
        let accounts = match fs::read(&path) {
            Ok(bytes) => {
                let persisted: PersistedAccounts = serde_json::from_slice(&bytes)
                    .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))?;
                if persisted.version != 1 {
                    return Err(ChannelGatewayError::Persistence(
                        "channel account store version is unsupported".to_owned(),
                    ));
                }
                let mut accounts = BTreeMap::new();
                for account in persisted.accounts {
                    if accounts
                        .insert(account.config.key.clone(), account)
                        .is_some()
                    {
                        return Err(ChannelGatewayError::Persistence(
                            "channel account store contains duplicate accounts".to_owned(),
                        ));
                    }
                }
                accounts
            }
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => BTreeMap::new(),
            Err(error) => return Err(ChannelGatewayError::Persistence(error.to_string())),
        };
        Ok(Self { path, accounts })
    }

    pub fn records(&self) -> impl ExactSizeIterator<Item = &ChannelAccountRecord> {
        self.accounts.values()
    }

    fn put(&mut self, record: ChannelAccountRecord) -> Result<(), ChannelGatewayError> {
        let key = record.config.key.clone();
        let previous = self.accounts.insert(key.clone(), record);
        if let Err(error) = self.flush() {
            match previous {
                Some(previous) => {
                    self.accounts.insert(key, previous);
                }
                None => {
                    self.accounts.remove(&key);
                }
            }
            return Err(error);
        }
        Ok(())
    }

    fn remove(
        &mut self,
        key: &ChannelAccountKey,
    ) -> Result<Option<ChannelAccountRecord>, ChannelGatewayError> {
        let removed = self.accounts.remove(key);
        if let Err(error) = self.flush() {
            if let Some(record) = &removed {
                self.accounts.insert(key.clone(), record.clone());
            }
            return Err(error);
        }
        Ok(removed)
    }

    fn flush(&self) -> Result<(), ChannelGatewayError> {
        if let Some(parent) = self.path.parent() {
            fs::create_dir_all(parent)
                .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))?;
        }
        let persisted = PersistedAccounts {
            version: 1,
            accounts: self.accounts.values().cloned().collect(),
        };
        let bytes = serde_json::to_vec(&persisted)
            .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))?;
        let temporary = self
            .path
            .with_extension(format!("{}.tmp", std::process::id()));
        fs::write(&temporary, bytes)
            .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))?;
        File::open(&temporary)
            .and_then(|file| file.sync_all())
            .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))?;
        keith_platform::replace_file(&temporary, &self.path)
            .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))
    }
}

struct AccountRuntime {
    record: ChannelAccountRecord,
    queue: GatewayQueue,
    route_context: BTreeMap<String, PendingRouteContext>,
    in_flight: BTreeMap<SessionId, String>,
}

#[allow(clippy::struct_excessive_bools)]
struct PendingRouteContext {
    route_id: String,
    shared_group_memory: bool,
    tools_allowed: bool,
    schedules_allowed: bool,
    proactive_posts_allowed: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
#[allow(clippy::struct_excessive_bools)]
pub struct RoutedAccountInbound {
    pub account: ChannelAccountKey,
    pub route_id: String,
    pub shared_group_memory: bool,
    pub tools_allowed: bool,
    pub schedules_allowed: bool,
    pub proactive_posts_allowed: bool,
    pub routed: RoutedInbound,
}

pub struct ChannelGatewaySupervisor {
    catalog: ChannelAdapterCatalog,
    store: ChannelAccountStore,
    accounts: BTreeMap<ChannelAccountKey, AccountRuntime>,
    fair_order: VecDeque<ChannelAccountKey>,
}

impl ChannelGatewaySupervisor {
    /// Restores every configured account with a fresh bounded queue while retaining durable
    /// setup, health, routing, credential generation, and reconnect facts.
    ///
    /// # Errors
    ///
    /// Returns an error for corrupt catalog registrations, routes, or queue limits.
    pub fn open(path: impl Into<PathBuf>) -> Result<Self, ChannelGatewayError> {
        let store = ChannelAccountStore::open(path)?;
        let mut catalog = ChannelAdapterCatalog::built_in();
        let mut accounts = BTreeMap::new();
        let mut fair_order = VecDeque::new();
        let now = UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH);
        for stored in store.records() {
            stored.config.validate(&catalog)?;
            catalog
                .register_account(
                    stored.config.key.kind,
                    stored.setup.clone(),
                    stored.capabilities.clone(),
                )
                .map_err(ChannelGatewayError::Catalog)?;
            let mut record = stored.clone();
            record.health.queue_depth = 0;
            record.health.reconnect_cursor_present = record.reconnect_cursor.is_some();
            refresh_notification_budget(&mut record, now);
            match record.health.lifecycle {
                ChannelAccountLifecycle::RateLimited
                    if record
                        .health
                        .throttled_until
                        .is_some_and(|until| until > now) => {}
                ChannelAccountLifecycle::Failed if record.config.enabled => {}
                _ if record.config.enabled => {
                    record.health.lifecycle = ChannelAccountLifecycle::Reconnecting;
                    record.health.connection = ChannelConnectionHealthV2::Disconnected;
                    record.health.throttled_until = None;
                }
                _ => {
                    record.health.lifecycle = ChannelAccountLifecycle::Paused;
                    record.health.connection = ChannelConnectionHealthV2::Disconnected;
                    record.health.throttled_until = None;
                }
            }
            record
                .health
                .validate()
                .map_err(ChannelGatewayError::Adapter)?;
            let key = record.config.key.clone();
            let mut queue = GatewayQueue::new(record.config.queue_limits.gateway())
                .map_err(|error| ChannelGatewayError::Invalid(error.to_string()))?;
            let mut route_context = BTreeMap::new();
            for pending in &record.pending_inbound {
                queue
                    .enqueue(pending.routed.clone())
                    .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))?;
                route_context.insert(
                    inbound_key(&pending.routed.message),
                    PendingRouteContext {
                        route_id: pending.route_id.clone(),
                        shared_group_memory: pending.shared_group_memory,
                        tools_allowed: pending.tools_allowed,
                        schedules_allowed: pending.schedules_allowed,
                        proactive_posts_allowed: pending.proactive_posts_allowed,
                    },
                );
            }
            record.health.queue_depth = queue.pending_count();
            accounts.insert(
                key.clone(),
                AccountRuntime {
                    record,
                    queue,
                    route_context,
                    in_flight: BTreeMap::new(),
                },
            );
            fair_order.push_back(key);
        }
        Ok(Self {
            catalog,
            store,
            accounts,
            fair_order,
        })
    }

    /// Adds one configured real adapter account and persists it before making it runnable.
    ///
    /// # Errors
    ///
    /// Returns an error for duplicate accounts, invalid routes, setup, capabilities, credentials,
    /// or persistence.
    pub fn add_account(
        &mut self,
        config: ChannelAccountConfig,
        setup: ChannelAccountSetupV2,
        capabilities: ChannelCapabilitiesV2,
        now: UtcTimestamp,
    ) -> Result<ChannelAccountHealth, ChannelGatewayError> {
        config.validate(&self.catalog)?;
        if self.accounts.contains_key(&config.key) {
            return Err(ChannelGatewayError::DuplicateAccount);
        }
        for required in &setup.required_credential_names {
            if !config.credential_refs.contains_key(required) {
                return Err(ChannelGatewayError::Invalid(format!(
                    "channel account is missing setup credential reference {required}"
                )));
            }
        }
        self.catalog
            .register_account(config.key.kind, setup.clone(), capabilities.clone())
            .map_err(ChannelGatewayError::Catalog)?;
        if setup.account_id != config.key.account_id {
            self.catalog
                .remove_account(config.key.kind, &setup.account_id);
            return Err(ChannelGatewayError::Invalid(
                "adapter setup account differs from gateway account".to_owned(),
            ));
        }
        let health = ChannelAccountHealth {
            kind: config.key.kind,
            account_id: config.key.account_id.clone(),
            lifecycle: if config.enabled {
                ChannelAccountLifecycle::Starting
            } else {
                ChannelAccountLifecycle::Paused
            },
            connection: ChannelConnectionHealthV2::Disconnected,
            queue_depth: 0,
            queue_capacity: config.queue_limits.pending,
            restart_count: 0,
            consecutive_failures: 0,
            throttled_until: None,
            last_event_at: None,
            last_delivery_at: None,
            reconnect_cursor_present: setup.reconnect_cursor_present,
            credential_generation: config.credential_generation,
            safe_error: None,
        };
        health.validate().map_err(ChannelGatewayError::Adapter)?;
        let record = ChannelAccountRecord {
            config: config.clone(),
            setup,
            capabilities,
            health: health.clone(),
            reconnect_cursor: None,
            notification_budget: NotificationBudgetState {
                remaining: config.notification_budget.capacity,
                resets_at: notification_budget_deadline(now, config.notification_budget.window_ms),
            },
            pending_inbound: Vec::new(),
            updated_at: now,
        };
        if let Err(error) = self.store.put(record.clone()) {
            self.catalog
                .remove_account(config.key.kind, &config.key.account_id);
            return Err(error);
        }
        let queue = GatewayQueue::new(config.queue_limits.gateway())
            .map_err(|error| ChannelGatewayError::Invalid(error.to_string()))?;
        self.accounts.insert(
            config.key.clone(),
            AccountRuntime {
                record,
                queue,
                route_context: BTreeMap::new(),
                in_flight: BTreeMap::new(),
            },
        );
        self.fair_order.push_back(config.key);
        Ok(health)
    }

    /// Adds one real managed adapter using its exact setup and capability declarations.
    ///
    /// # Errors
    ///
    /// Returns the same validation, duplicate, or persistence errors as `add_account`.
    pub fn add_managed_account(
        &mut self,
        config: ChannelAccountConfig,
        adapter: &dyn ManagedChannelAdapter,
        now: UtcTimestamp,
    ) -> Result<ChannelAccountHealth, ChannelGatewayError> {
        if adapter.kind() != config.key.kind {
            return Err(ChannelGatewayError::Invalid(
                "managed adapter kind differs from account configuration".to_owned(),
            ));
        }
        let key = config.key.clone();
        let cursor = adapter.reconnect_cursor_v2();
        let mut health = self.add_account(
            config,
            adapter.account_setup(),
            adapter.capabilities_v2(),
            now,
        )?;
        if let Some(runtime) = self.accounts.get_mut(&key) {
            runtime.record.reconnect_cursor = cursor;
            runtime.record.health.reconnect_cursor_present =
                runtime.record.reconnect_cursor.is_some();
            health.reconnect_cursor_present = runtime.record.health.reconnect_cursor_present;
            self.store.put(runtime.record.clone())?;
        }
        Ok(health)
    }

    pub fn health(&self) -> Vec<ChannelAccountHealth> {
        self.accounts
            .values()
            .map(|runtime| runtime.record.health.clone())
            .collect()
    }

    pub fn record(&self, key: &ChannelAccountKey) -> Option<&ChannelAccountRecord> {
        self.accounts.get(key).map(|runtime| &runtime.record)
    }

    /// Routes and enqueues one normalized event into only its owning account queue.
    ///
    /// # Errors
    ///
    /// Returns an error for unknown, paused, failed, cross-account, ambiguous, or forbidden group
    /// routes and for account-local backpressure.
    pub fn enqueue(
        &mut self,
        key: &ChannelAccountKey,
        event: &ChannelEventV2,
        now: UtcTimestamp,
    ) -> Result<EnqueueOutcome, ChannelGatewayError> {
        let runtime = self
            .accounts
            .get_mut(key)
            .ok_or(ChannelGatewayError::UnknownAccount)?;
        if !runtime.record.config.enabled
            || matches!(
                runtime.record.health.lifecycle,
                ChannelAccountLifecycle::Paused
                    | ChannelAccountLifecycle::Failed
                    | ChannelAccountLifecycle::Removing
                    | ChannelAccountLifecycle::Removed
            )
        {
            return Err(ChannelGatewayError::AccountUnavailable);
        }
        event
            .validate(&runtime.record.capabilities)
            .map_err(ChannelGatewayError::Adapter)?;
        let resolved = resolve_event(&runtime.record.config, event)?;
        let message_key = inbound_key(&resolved.routed.message);
        let durable = DurableInbound {
            route_id: resolved.route_id.clone(),
            shared_group_memory: resolved.shared_group_memory,
            tools_allowed: resolved.tools_allowed,
            schedules_allowed: resolved.schedules_allowed,
            proactive_posts_allowed: resolved.proactive_posts_allowed,
            routed: resolved.routed.clone(),
        };
        let context = PendingRouteContext {
            route_id: resolved.route_id,
            shared_group_memory: resolved.shared_group_memory,
            tools_allowed: resolved.tools_allowed,
            schedules_allowed: resolved.schedules_allowed,
            proactive_posts_allowed: resolved.proactive_posts_allowed,
        };
        let outcome = runtime
            .queue
            .enqueue(resolved.routed)
            .map_err(|error| ChannelGatewayError::Queue(error.to_string()))?;
        if outcome == EnqueueOutcome::Queued {
            runtime.route_context.insert(message_key, context);
            runtime.record.pending_inbound.push(durable);
        }
        runtime.record.health.queue_depth = runtime.queue.pending_count();
        runtime.record.health.last_event_at = Some(now);
        runtime.record.health.lifecycle = ChannelAccountLifecycle::Running;
        runtime.record.health.connection = ChannelConnectionHealthV2::Connected;
        runtime.record.updated_at = now;
        self.store.put(runtime.record.clone())?;
        Ok(outcome)
    }

    /// Takes at most one ready message from each account in round-robin order.
    pub fn take_fair(&mut self, _now: UtcTimestamp) -> Option<RoutedAccountInbound> {
        let rounds = self.fair_order.len();
        for _ in 0..rounds {
            let key = self.fair_order.pop_front()?;
            self.fair_order.push_back(key.clone());
            let runtime = self.accounts.get_mut(&key)?;
            if !matches!(
                runtime.record.health.lifecycle,
                ChannelAccountLifecycle::Starting
                    | ChannelAccountLifecycle::Running
                    | ChannelAccountLifecycle::Reconnecting
            ) {
                continue;
            }
            if let Some(routed) = runtime.queue.take_ready() {
                runtime.record.health.queue_depth = runtime.queue.pending_count();
                let message_key = inbound_key(&routed.message);
                let context = runtime.route_context.get(&message_key)?;
                runtime
                    .in_flight
                    .insert(routed.session_id.clone(), message_key);
                return Some(RoutedAccountInbound {
                    account: key,
                    route_id: context.route_id.clone(),
                    shared_group_memory: context.shared_group_memory,
                    tools_allowed: context.tools_allowed,
                    schedules_allowed: context.schedules_allowed,
                    proactive_posts_allowed: context.proactive_posts_allowed,
                    routed,
                });
            }
        }
        None
    }

    /// Releases expired per-account rate limits without disturbing still-throttled peers.
    ///
    /// # Errors
    ///
    /// Returns a persistence error while leaving unrelated account workers operational.
    pub fn release_expired_rate_limits(
        &mut self,
        now: UtcTimestamp,
    ) -> Result<Vec<ChannelAccountKey>, ChannelGatewayError> {
        let keys = self
            .accounts
            .iter()
            .filter(|(_, runtime)| {
                runtime.record.health.lifecycle == ChannelAccountLifecycle::RateLimited
                    && runtime
                        .record
                        .health
                        .throttled_until
                        .is_some_and(|until| until <= now)
            })
            .map(|(key, _)| key.clone())
            .collect::<Vec<_>>();
        for key in &keys {
            let runtime = self
                .accounts
                .get_mut(key)
                .ok_or(ChannelGatewayError::UnknownAccount)?;
            runtime.record.health.lifecycle = ChannelAccountLifecycle::Reconnecting;
            runtime.record.health.connection = ChannelConnectionHealthV2::Disconnected;
            runtime.record.health.throttled_until = None;
            runtime.record.health.safe_error = None;
            runtime.record.updated_at = now;
            self.store.put(runtime.record.clone())?;
        }
        Ok(keys)
    }

    /// Releases the per-session in-flight guard for one account.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown account/session or persistence failure.
    pub fn complete(
        &mut self,
        key: &ChannelAccountKey,
        session_id: &SessionId,
        now: UtcTimestamp,
    ) -> Result<(), ChannelGatewayError> {
        let runtime = self
            .accounts
            .get_mut(key)
            .ok_or(ChannelGatewayError::UnknownAccount)?;
        runtime
            .queue
            .complete(session_id)
            .map_err(|error| ChannelGatewayError::Queue(error.to_string()))?;
        let message_key = runtime.in_flight.remove(session_id).ok_or_else(|| {
            ChannelGatewayError::Queue("session had no durable dispatch".to_owned())
        })?;
        runtime.route_context.remove(&message_key);
        runtime
            .record
            .pending_inbound
            .retain(|pending| inbound_key(&pending.routed.message) != message_key);
        runtime.record.health.queue_depth = runtime.queue.pending_count();
        runtime.record.updated_at = now;
        self.store.put(runtime.record.clone())
    }

    /// Pauses one account without discarding its durable routes or pending inbound work.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown account or failed durable update.
    pub fn pause(
        &mut self,
        key: &ChannelAccountKey,
        now: UtcTimestamp,
    ) -> Result<ChannelAccountHealth, ChannelGatewayError> {
        self.set_enabled(key, false, ChannelAccountLifecycle::Paused, now)
    }

    /// Re-enables a paused account and schedules an isolated reconnect.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown account or failed durable update.
    pub fn resume(
        &mut self,
        key: &ChannelAccountKey,
        now: UtcTimestamp,
    ) -> Result<ChannelAccountHealth, ChannelGatewayError> {
        self.set_enabled(key, true, ChannelAccountLifecycle::Reconnecting, now)
    }

    fn set_enabled(
        &mut self,
        key: &ChannelAccountKey,
        enabled: bool,
        lifecycle: ChannelAccountLifecycle,
        now: UtcTimestamp,
    ) -> Result<ChannelAccountHealth, ChannelGatewayError> {
        let runtime = self
            .accounts
            .get_mut(key)
            .ok_or(ChannelGatewayError::UnknownAccount)?;
        runtime.record.config.enabled = enabled;
        runtime.record.health.lifecycle = lifecycle;
        runtime.record.health.connection = ChannelConnectionHealthV2::Disconnected;
        runtime.record.health.throttled_until = None;
        runtime.record.health.safe_error = None;
        runtime.record.updated_at = now;
        self.store.put(runtime.record.clone())?;
        Ok(runtime.record.health.clone())
    }

    /// Replaces one named credential reference and advances the durable generation. Secret bytes
    /// never enter gateway state.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown, cross-account, or overflowing credential reference, or a
    /// failed durable update.
    pub fn rotate_credential(
        &mut self,
        key: &ChannelAccountKey,
        name: &str,
        reference: CredentialRef,
        now: UtcTimestamp,
    ) -> Result<ChannelAccountHealth, ChannelGatewayError> {
        let runtime = self
            .accounts
            .get_mut(key)
            .ok_or(ChannelGatewayError::UnknownAccount)?;
        if !runtime.record.config.credential_refs.contains_key(name)
            || !matches!(
                &reference.owner,
                CredentialOwner::Channel(owner) if owner == &key.account_id
            )
        {
            return Err(ChannelGatewayError::Invalid(
                "rotated credential reference is unknown or cross-account".to_owned(),
            ));
        }
        runtime
            .record
            .config
            .credential_refs
            .insert(name.to_owned(), reference);
        runtime.record.config.credential_generation = runtime
            .record
            .config
            .credential_generation
            .checked_add(1)
            .ok_or_else(|| {
                ChannelGatewayError::Invalid("credential generation overflowed".to_owned())
            })?;
        runtime.record.health.credential_generation = runtime.record.config.credential_generation;
        runtime.record.health.lifecycle = ChannelAccountLifecycle::Reconnecting;
        runtime.record.health.connection = ChannelConnectionHealthV2::Disconnected;
        runtime.record.health.safe_error = None;
        runtime.record.updated_at = now;
        self.store.put(runtime.record.clone())?;
        Ok(runtime.record.health.clone())
    }

    /// Applies a classified adapter failure only to its owning account.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown account or failed durable update.
    pub fn record_adapter_error(
        &mut self,
        key: &ChannelAccountKey,
        error: &ChannelAdapterErrorV2,
        now: UtcTimestamp,
    ) -> Result<ChannelAccountHealth, ChannelGatewayError> {
        let runtime = self
            .accounts
            .get_mut(key)
            .ok_or(ChannelGatewayError::UnknownAccount)?;
        runtime.record.health.consecutive_failures =
            runtime.record.health.consecutive_failures.saturating_add(1);
        runtime.record.health.safe_error = Some(error.safe_message.clone());
        match error.kind {
            ChannelAdapterErrorKindV2::RateLimit => {
                let retry = error.retry_after_ms.unwrap_or(1_000).min(3_600_000);
                runtime.record.health.lifecycle = ChannelAccountLifecycle::RateLimited;
                runtime.record.health.connection = ChannelConnectionHealthV2::RateLimited;
                runtime.record.health.throttled_until = Some(UtcTimestamp::from_unix_millis(
                    now.unix_millis()
                        .saturating_add(i64::try_from(retry).unwrap_or(i64::MAX)),
                ));
            }
            ChannelAdapterErrorKindV2::TransientNetwork
            | ChannelAdapterErrorKindV2::UncertainAcknowledgement
            | ChannelAdapterErrorKindV2::StaleCursor => {
                runtime.record.health.lifecycle = ChannelAccountLifecycle::Reconnecting;
                runtime.record.health.connection = ChannelConnectionHealthV2::Disconnected;
                runtime.record.health.restart_count =
                    runtime.record.health.restart_count.saturating_add(1);
            }
            ChannelAdapterErrorKindV2::Authentication
            | ChannelAdapterErrorKindV2::Permission
            | ChannelAdapterErrorKindV2::PermanentDestination
            | ChannelAdapterErrorKindV2::MalformedEvent
            | ChannelAdapterErrorKindV2::UnsupportedFeature => {
                runtime.record.health.lifecycle = ChannelAccountLifecycle::Failed;
                runtime.record.health.connection = ChannelConnectionHealthV2::Failed;
            }
            ChannelAdapterErrorKindV2::Cancelled => {}
        }
        runtime.record.updated_at = now;
        self.store.put(runtime.record.clone())?;
        Ok(runtime.record.health.clone())
    }

    /// Marks one adapter connected after a successful start or reconnect.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown account or failed durable update.
    pub fn record_adapter_ready(
        &mut self,
        key: &ChannelAccountKey,
        cursor: Option<ReconnectCursorV2>,
        now: UtcTimestamp,
    ) -> Result<ChannelAccountHealth, ChannelGatewayError> {
        let runtime = self
            .accounts
            .get_mut(key)
            .ok_or(ChannelGatewayError::UnknownAccount)?;
        runtime.record.health.lifecycle = ChannelAccountLifecycle::Running;
        runtime.record.health.connection = ChannelConnectionHealthV2::Connected;
        runtime.record.health.consecutive_failures = 0;
        runtime.record.health.safe_error = None;
        runtime.record.health.throttled_until = None;
        runtime.record.reconnect_cursor = cursor;
        runtime.record.health.reconnect_cursor_present = runtime.record.reconnect_cursor.is_some();
        runtime.record.updated_at = now;
        self.store.put(runtime.record.clone())?;
        Ok(runtime.record.health.clone())
    }

    /// Records a successful delivery timestamp without storing message content or secrets.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown account or failed durable update.
    pub fn record_delivery(
        &mut self,
        key: &ChannelAccountKey,
        now: UtcTimestamp,
    ) -> Result<(), ChannelGatewayError> {
        let runtime = self
            .accounts
            .get_mut(key)
            .ok_or(ChannelGatewayError::UnknownAccount)?;
        runtime.record.health.last_delivery_at = Some(now);
        runtime.record.updated_at = now;
        self.store.put(runtime.record.clone())
    }

    /// Returns the exact durable outbox partition that this account is allowed to claim.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown or disabled account.
    pub fn delivery_partition(
        &self,
        key: &ChannelAccountKey,
    ) -> Result<DeliveryPartition, ChannelGatewayError> {
        let runtime = self
            .accounts
            .get(key)
            .ok_or(ChannelGatewayError::UnknownAccount)?;
        if !runtime.record.config.enabled
            || matches!(
                runtime.record.health.lifecycle,
                ChannelAccountLifecycle::Paused
                    | ChannelAccountLifecycle::Failed
                    | ChannelAccountLifecycle::Removing
                    | ChannelAccountLifecycle::Removed
            )
        {
            return Err(ChannelGatewayError::AccountUnavailable);
        }
        Ok(DeliveryPartition {
            channel: key.kind.channel().to_owned(),
            external_account: key.account_id.clone(),
        })
    }

    /// Reserves one proactive notification from an account-local fixed-window budget.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown account or route, disabled proactive posting, exhausted
    /// budget, or failed durable update.
    pub fn reserve_proactive_notification(
        &mut self,
        key: &ChannelAccountKey,
        route_id: &str,
        now: UtcTimestamp,
    ) -> Result<u32, ChannelGatewayError> {
        let runtime = self
            .accounts
            .get_mut(key)
            .ok_or(ChannelGatewayError::UnknownAccount)?;
        let route = runtime
            .record
            .config
            .routes
            .iter()
            .find(|route| route.enabled && route.id == route_id)
            .ok_or(ChannelGatewayError::MissingRoute)?;
        if !route.group.proactive_posts {
            return Err(ChannelGatewayError::ProactivePostDenied);
        }
        refresh_notification_budget(&mut runtime.record, now);
        runtime.record.notification_budget.remaining = runtime
            .record
            .notification_budget
            .remaining
            .checked_sub(1)
            .ok_or(ChannelGatewayError::NotificationBudgetExhausted)?;
        runtime.record.updated_at = now;
        let remaining = runtime.record.notification_budget.remaining;
        self.store.put(runtime.record.clone())?;
        Ok(remaining)
    }

    /// Removes account state, routes, queues, and catalog registration durably.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown account or failed durable removal.
    pub fn remove_account(
        &mut self,
        key: &ChannelAccountKey,
    ) -> Result<ChannelAccountRecord, ChannelGatewayError> {
        let mut record = self
            .accounts
            .get(key)
            .map(|runtime| runtime.record.clone())
            .ok_or(ChannelGatewayError::UnknownAccount)?;
        record.health.lifecycle = ChannelAccountLifecycle::Removed;
        record.health.connection = ChannelConnectionHealthV2::Revoked;
        self.store.remove(key)?;
        self.accounts.remove(key);
        self.fair_order.retain(|candidate| candidate != key);
        self.catalog.remove_account(key.kind, &key.account_id);
        Ok(record)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DeliveryPartition {
    pub channel: String,
    pub external_account: String,
}

impl DeliveryPartition {
    pub fn matches(&self, channel: &str, external_account: &str) -> bool {
        self.channel == channel && self.external_account == external_account
    }
}

fn notification_budget_deadline(now: UtcTimestamp, window_ms: u64) -> UtcTimestamp {
    UtcTimestamp::from_unix_millis(
        now.unix_millis()
            .saturating_add(i64::try_from(window_ms).unwrap_or(i64::MAX)),
    )
}

fn refresh_notification_budget(record: &mut ChannelAccountRecord, now: UtcTimestamp) {
    let config = record.config.notification_budget;
    if now >= record.notification_budget.resets_at {
        record.notification_budget.remaining = config.capacity;
        record.notification_budget.resets_at = notification_budget_deadline(now, config.window_ms);
    } else {
        record.notification_budget.remaining =
            record.notification_budget.remaining.min(config.capacity);
    }
}

fn resolve_event(
    config: &ChannelAccountConfig,
    event: &ChannelEventV2,
) -> Result<RoutedAccountInbound, ChannelGatewayError> {
    if !event.contract.is_compatible_with(CHANNEL_CONTRACT_V2) {
        return Err(ChannelGatewayError::Invalid(
            "channel event contract is incompatible".to_owned(),
        ));
    }
    let is_group = match &event.event {
        ChannelEventKindV2::MessageCreated(message) => {
            message.conversation.kind != ChannelConversationKindV2::Direct
        }
        ChannelEventKindV2::Command(command) => {
            command.conversation.kind != ChannelConversationKindV2::Direct
        }
        _ => false,
    };
    let message = match &event.event {
        ChannelEventKindV2::MessageCreated(message) => message_to_inbound(config, message),
        ChannelEventKindV2::Command(command) => command_to_inbound(config, command),
        ChannelEventKindV2::CancellationRequested { cancellation_id } => InboundMessage {
            channel: config.key.kind.channel().to_owned(),
            external_account: config.key.account_id.clone(),
            conversation: "control".to_owned(),
            thread: None,
            sender: config.key.account_id.clone(),
            message_id: event.event_id.clone(),
            reply_target: None,
            text: cancellation_id.clone(),
            attachments: Vec::new(),
            occurred_at: UtcTimestamp::now().unwrap_or(UtcTimestamp::UNIX_EPOCH),
            intent: InboundIntent::Cancel,
        },
        _ => return Err(ChannelGatewayError::UnsupportedEvent),
    };
    if message.external_account != config.key.account_id
        || message.channel != config.key.kind.channel()
    {
        return Err(ChannelGatewayError::CrossAccount);
    }
    let route = select_route(&config.routes, &message)?;
    if is_group {
        if route.group.require_mention && !message_mentions_profile(event) {
            return Err(ChannelGatewayError::MentionRequired);
        }
        if !route.group.retained_participants.is_empty()
            && !route.group.retained_participants.contains(&message.sender)
        {
            return Err(ChannelGatewayError::ParticipantDenied);
        }
    }
    Ok(RoutedAccountInbound {
        account: config.key.clone(),
        route_id: route.id.clone(),
        shared_group_memory: is_group && route.group.shared_memory,
        tools_allowed: !is_group || route.group.tool_principals.contains(&message.sender),
        schedules_allowed: !is_group || route.group.schedule_principals.contains(&message.sender),
        proactive_posts_allowed: !is_group || route.group.proactive_posts,
        routed: RoutedInbound {
            profile_id: route.profile_id.clone(),
            session_id: route.session_id.clone(),
            message,
        },
    })
}

fn select_route<'a>(
    routes: &'a [ChannelRouteRule],
    message: &InboundMessage,
) -> Result<&'a ChannelRouteRule, ChannelGatewayError> {
    let mut candidates = routes
        .iter()
        .filter(|route| route.enabled)
        .filter(|route| {
            route
                .conversation
                .as_ref()
                .is_none_or(|value| value == &message.conversation)
                && route
                    .thread
                    .as_ref()
                    .is_none_or(|value| message.thread.as_ref() == Some(value))
                && route
                    .sender
                    .as_ref()
                    .is_none_or(|value| value == &message.sender)
        })
        .collect::<Vec<_>>();
    candidates.sort_by(|left, right| {
        right
            .priority
            .cmp(&left.priority)
            .then_with(|| right.specificity().cmp(&left.specificity()))
            .then_with(|| left.id.cmp(&right.id))
    });
    let selected = candidates
        .first()
        .copied()
        .ok_or(ChannelGatewayError::MissingRoute)?;
    if let Some(second) = candidates.get(1)
        && selected.priority == second.priority
        && selected.specificity() == second.specificity()
        && (selected.profile_id != second.profile_id || selected.session_id != second.session_id)
    {
        return Err(ChannelGatewayError::AmbiguousRoute);
    }
    Ok(selected)
}

fn message_to_inbound(config: &ChannelAccountConfig, message: &ChannelMessageV2) -> InboundMessage {
    InboundMessage {
        channel: config.key.kind.channel().to_owned(),
        external_account: message.account_id.clone(),
        conversation: message.conversation.platform_id.clone(),
        thread: message.conversation.thread_id.clone(),
        sender: message.sender.platform_id.clone(),
        message_id: message.message_id.clone(),
        reply_target: message.conversation.reply_to_message_id.clone(),
        text: message.text.clone(),
        attachments: message
            .attachments
            .iter()
            .map(|attachment| attachment.attachment.clone())
            .collect(),
        occurred_at: message.occurred_at,
        intent: InboundIntent::Prompt,
    }
}

fn command_to_inbound(config: &ChannelAccountConfig, command: &ChannelCommandV2) -> InboundMessage {
    InboundMessage {
        channel: config.key.kind.channel().to_owned(),
        external_account: command.account_id.clone(),
        conversation: command.conversation.platform_id.clone(),
        thread: command.conversation.thread_id.clone(),
        sender: command.sender.platform_id.clone(),
        message_id: command.command_id.clone(),
        reply_target: command.conversation.reply_to_message_id.clone(),
        text: format!("/{} {}", command.name, command.arguments)
            .trim()
            .to_owned(),
        attachments: Vec::new(),
        occurred_at: command.occurred_at,
        intent: InboundIntent::Prompt,
    }
}

fn inbound_key(message: &InboundMessage) -> String {
    [
        message.channel.as_str(),
        message.external_account.as_str(),
        message.conversation.as_str(),
        message.thread.as_deref().unwrap_or_default(),
        message.message_id.as_str(),
    ]
    .join("\0")
}

fn message_mentions_profile(event: &ChannelEventV2) -> bool {
    matches!(
        &event.event,
        ChannelEventKindV2::MessageCreated(message) if !message.mentions.is_empty()
    )
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RawWebhookIngress {
    pub account: ChannelAccountKey,
    pub delivery_id: String,
    pub received_at: UtcTimestamp,
    pub signature: String,
    pub body: Vec<u8>,
}

pub struct VerifiedWebhookIngress {
    raw: RawWebhookIngress,
}

impl VerifiedWebhookIngress {
    /// Authenticates the raw body before any parser can access it.
    ///
    /// # Errors
    ///
    /// Returns an authentication error for invalid identity, size, freshness, or signature.
    pub fn verify(
        raw: RawWebhookIngress,
        now: UtcTimestamp,
        max_body_bytes: usize,
        max_clock_skew_ms: u64,
        verifier: impl FnOnce(&ChannelAccountKey, &[u8], &str) -> bool,
    ) -> Result<Self, ChannelGatewayError> {
        raw.account.validate()?;
        let age = now.unix_millis().abs_diff(raw.received_at.unix_millis());
        if raw.delivery_id.trim().is_empty()
            || raw.signature.trim().is_empty()
            || raw.body.is_empty()
            || raw.body.len() > max_body_bytes
            || max_clock_skew_ms == 0
            || age > max_clock_skew_ms
            || !verifier(&raw.account, &raw.body, &raw.signature)
        {
            return Err(ChannelGatewayError::WebhookAuthentication);
        }
        Ok(Self { raw })
    }

    pub const fn account(&self) -> &ChannelAccountKey {
        &self.raw.account
    }

    pub fn delivery_id(&self) -> &str {
        &self.raw.delivery_id
    }

    pub fn body(&self) -> &[u8] {
        &self.raw.body
    }
}

pub struct WebhookIngressRouter {
    path: Option<PathBuf>,
    accounts: BTreeSet<ChannelAccountKey>,
    seen: BTreeSet<(ChannelAccountKey, String)>,
    order: VecDeque<(ChannelAccountKey, String)>,
    replay_capacity: usize,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct PersistedWebhookReplay {
    version: u16,
    accounts: BTreeSet<ChannelAccountKey>,
    order: VecDeque<(ChannelAccountKey, String)>,
}

impl WebhookIngressRouter {
    /// Creates a bounded replay guard and account router.
    ///
    /// # Errors
    ///
    /// Returns an error when replay capacity is zero.
    pub fn new(replay_capacity: usize) -> Result<Self, ChannelGatewayError> {
        if replay_capacity == 0 {
            return Err(ChannelGatewayError::Invalid(
                "webhook replay capacity must be positive".to_owned(),
            ));
        }
        Ok(Self {
            path: None,
            accounts: BTreeSet::new(),
            seen: BTreeSet::new(),
            order: VecDeque::new(),
            replay_capacity,
        })
    }

    /// Opens a durable bounded replay guard and restores registered account identities.
    ///
    /// # Errors
    ///
    /// Returns an error for zero capacity or malformed, duplicate, unreadable, or unsupported
    /// replay state.
    pub fn open(
        path: impl Into<PathBuf>,
        replay_capacity: usize,
    ) -> Result<Self, ChannelGatewayError> {
        let path = path.into();
        let mut persisted = match fs::read(&path) {
            Ok(bytes) => serde_json::from_slice::<PersistedWebhookReplay>(&bytes)
                .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))?,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => PersistedWebhookReplay {
                version: 1,
                accounts: BTreeSet::new(),
                order: VecDeque::new(),
            },
            Err(error) => return Err(ChannelGatewayError::Persistence(error.to_string())),
        };
        if replay_capacity == 0 || persisted.version != 1 {
            return Err(ChannelGatewayError::Persistence(
                "webhook replay state version or capacity is invalid".to_owned(),
            ));
        }
        while persisted.order.len() > replay_capacity {
            persisted.order.pop_front();
        }
        let mut seen = BTreeSet::new();
        for (account, delivery_id) in &persisted.order {
            account.validate()?;
            if delivery_id.trim().is_empty()
                || !persisted.accounts.contains(account)
                || !seen.insert((account.clone(), delivery_id.clone()))
            {
                return Err(ChannelGatewayError::Persistence(
                    "webhook replay state is malformed or duplicated".to_owned(),
                ));
            }
        }
        for account in &persisted.accounts {
            account.validate()?;
        }
        Ok(Self {
            path: Some(path),
            accounts: persisted.accounts,
            seen,
            order: persisted.order,
            replay_capacity,
        })
    }

    /// Adds one configured webhook account to the ingress router.
    ///
    /// # Errors
    ///
    /// Returns an error for a malformed account identity.
    pub fn register(&mut self, account: &ChannelAccountKey) -> Result<(), ChannelGatewayError> {
        account.validate()?;
        let inserted = self.accounts.insert(account.clone());
        if let Err(error) = self.flush() {
            if inserted {
                self.accounts.remove(account);
            }
            return Err(error);
        }
        Ok(())
    }

    /// Removes an account and its replay identities from durable state.
    ///
    /// # Errors
    ///
    /// Returns an error when the durable replay registry cannot be updated.
    pub fn remove(&mut self, account: &ChannelAccountKey) -> Result<(), ChannelGatewayError> {
        let removed_account = self.accounts.remove(account);
        let old_seen = self.seen.clone();
        let old_order = self.order.clone();
        self.seen.retain(|(key, _)| key != account);
        self.order.retain(|(key, _)| key != account);
        if let Err(error) = self.flush() {
            if removed_account {
                self.accounts.insert(account.clone());
            }
            self.seen = old_seen;
            self.order = old_order;
            return Err(error);
        }
        Ok(())
    }

    /// Admits one already-authenticated body exactly once for its account.
    ///
    /// # Errors
    ///
    /// Returns an error for unknown accounts or replayed delivery identities.
    pub fn admit(
        &mut self,
        ingress: VerifiedWebhookIngress,
    ) -> Result<VerifiedWebhookIngress, ChannelGatewayError> {
        if !self.accounts.contains(ingress.account()) {
            return Err(ChannelGatewayError::UnknownAccount);
        }
        let key = (ingress.account().clone(), ingress.delivery_id().to_owned());
        let old_order = self.order.clone();
        let old_seen = self.seen.clone();
        if !self.seen.insert(key.clone()) {
            return Err(ChannelGatewayError::WebhookReplay);
        }
        self.order.push_back(key);
        while self.order.len() > self.replay_capacity {
            if let Some(expired) = self.order.pop_front() {
                self.seen.remove(&expired);
            }
        }
        if let Err(error) = self.flush() {
            self.order = old_order;
            self.seen = old_seen;
            return Err(error);
        }
        Ok(ingress)
    }

    fn flush(&self) -> Result<(), ChannelGatewayError> {
        let Some(path) = &self.path else {
            return Ok(());
        };
        if let Some(parent) = path.parent() {
            fs::create_dir_all(parent)
                .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))?;
        }
        let bytes = serde_json::to_vec(&PersistedWebhookReplay {
            version: 1,
            accounts: self.accounts.clone(),
            order: self.order.clone(),
        })
        .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))?;
        let temporary = path.with_extension(format!("{}.tmp", std::process::id()));
        fs::write(&temporary, bytes)
            .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))?;
        File::open(&temporary)
            .and_then(|file| file.sync_all())
            .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))?;
        keith_platform::replace_file(&temporary, path)
            .map_err(|error| ChannelGatewayError::Persistence(error.to_string()))
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum AdapterWorkerControl {
    Pause,
    Resume,
    Reconnect,
    TestConnection,
    AdmitVerified(Box<ChannelEventV2>),
    Execute(Box<ChannelOperationV2>),
    Stop,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum AdapterWorkerEvent {
    Started,
    Paused,
    Resumed,
    Inbound {
        event: Box<ChannelEventV2>,
        reconnect_cursor: Option<ReconnectCursorV2>,
    },
    Executed(Box<ChannelOperationReceiptV2>),
    ConnectionTested {
        setup: Box<ChannelAccountSetupV2>,
        reconnect_cursor: Option<ReconnectCursorV2>,
    },
    Reconnected {
        reconnect_cursor: Option<ReconnectCursorV2>,
    },
    Failed(ChannelAdapterErrorV2),
    Stopped,
}

pub struct ChannelAccountWorker {
    control: Sender<AdapterWorkerControl>,
    events: Receiver<AdapterWorkerEvent>,
    join: thread::JoinHandle<()>,
}

impl ChannelAccountWorker {
    /// Starts an isolated OS thread for one real adapter instance.
    pub fn spawn(adapter: Box<dyn ManagedChannelAdapter>, reconnect: ReconnectPolicy) -> Self {
        Self::spawn_with_mode(adapter, reconnect, ChannelWorkerMode::Pull)
    }

    /// Starts an event-driven worker for webhook adapters. Verified normalized events enter via
    /// `AdmitVerified`; outbound operations and lifecycle controls still execute on the isolated
    /// adapter thread.
    pub fn spawn_push(adapter: Box<dyn ManagedChannelAdapter>, reconnect: ReconnectPolicy) -> Self {
        Self::spawn_with_mode(adapter, reconnect, ChannelWorkerMode::Push)
    }

    #[allow(clippy::too_many_lines)]
    fn spawn_with_mode(
        mut adapter: Box<dyn ManagedChannelAdapter>,
        reconnect: ReconnectPolicy,
        mode: ChannelWorkerMode,
    ) -> Self {
        let (control_tx, control_rx) = mpsc::channel();
        let (event_tx, event_rx) = mpsc::channel();
        let join = thread::spawn(move || {
            let _ = event_tx.send(AdapterWorkerEvent::Started);
            let mut paused = false;
            loop {
                let control = if paused || mode == ChannelWorkerMode::Push {
                    match control_rx.recv() {
                        Ok(control) => Some(control),
                        Err(_) => break,
                    }
                } else {
                    match control_rx.try_recv() {
                        Ok(control) => Some(control),
                        Err(TryRecvError::Empty) => None,
                        Err(TryRecvError::Disconnected) => break,
                    }
                };
                if let Some(control) = control {
                    match control {
                        AdapterWorkerControl::Pause => {
                            paused = true;
                            let _ = event_tx.send(AdapterWorkerEvent::Paused);
                            continue;
                        }
                        AdapterWorkerControl::Resume => {
                            paused = false;
                            let _ = event_tx.send(AdapterWorkerEvent::Resumed);
                            continue;
                        }
                        AdapterWorkerControl::Reconnect => {
                            match reconnect_adapter(adapter.as_mut(), reconnect) {
                                Ok(()) => {
                                    let _ = event_tx.send(AdapterWorkerEvent::Reconnected {
                                        reconnect_cursor: adapter.reconnect_cursor_v2(),
                                    });
                                }
                                Err(error) => {
                                    let _ = event_tx.send(AdapterWorkerEvent::Failed(error));
                                }
                            }
                            continue;
                        }
                        AdapterWorkerControl::TestConnection => {
                            match adapter.test_connection() {
                                Ok(()) => {
                                    let _ = event_tx.send(AdapterWorkerEvent::ConnectionTested {
                                        setup: Box::new(adapter.account_setup()),
                                        reconnect_cursor: adapter.reconnect_cursor_v2(),
                                    });
                                }
                                Err(error) => {
                                    let _ = event_tx.send(AdapterWorkerEvent::Failed(error));
                                }
                            }
                            continue;
                        }
                        AdapterWorkerControl::AdmitVerified(event) => {
                            let result = event.validate(&adapter.capabilities_v2());
                            match result {
                                Ok(()) => {
                                    let _ = event_tx.send(AdapterWorkerEvent::Inbound {
                                        event,
                                        reconnect_cursor: adapter.reconnect_cursor_v2(),
                                    });
                                }
                                Err(error) => {
                                    let _ = event_tx.send(AdapterWorkerEvent::Failed(error));
                                }
                            }
                            continue;
                        }
                        AdapterWorkerControl::Execute(operation) => {
                            match adapter.execute_v2(&operation) {
                                Ok(receipt) => {
                                    let _ = event_tx
                                        .send(AdapterWorkerEvent::Executed(Box::new(receipt)));
                                }
                                Err(error) => {
                                    let _ = event_tx.send(AdapterWorkerEvent::Failed(error));
                                }
                            }
                            continue;
                        }
                        AdapterWorkerControl::Stop => break,
                    }
                }
                if paused {
                    continue;
                }
                match adapter.receive_v2() {
                    Ok(event) => {
                        if event_tx
                            .send(AdapterWorkerEvent::Inbound {
                                event: Box::new(event),
                                reconnect_cursor: adapter.reconnect_cursor_v2(),
                            })
                            .is_err()
                        {
                            break;
                        }
                    }
                    Err(error) if error.kind == ChannelAdapterErrorKindV2::RateLimit => {
                        let delay = error.retry_after_ms.unwrap_or(1_000).min(30_000);
                        if event_tx.send(AdapterWorkerEvent::Failed(error)).is_err() {
                            break;
                        }
                        thread::sleep(Duration::from_millis(delay));
                    }
                    Err(error) if error.is_retryable() => {
                        if event_tx.send(AdapterWorkerEvent::Failed(error)).is_err() {
                            break;
                        }
                        match reconnect_adapter(adapter.as_mut(), reconnect) {
                            Ok(()) => {
                                let _ = event_tx.send(AdapterWorkerEvent::Reconnected {
                                    reconnect_cursor: adapter.reconnect_cursor_v2(),
                                });
                            }
                            Err(error) => {
                                let _ = event_tx.send(AdapterWorkerEvent::Failed(error));
                            }
                        }
                    }
                    Err(error) => {
                        let _ = event_tx.send(AdapterWorkerEvent::Failed(error));
                        break;
                    }
                }
            }
            let _ = event_tx.send(AdapterWorkerEvent::Stopped);
        });
        Self {
            control: control_tx,
            events: event_rx,
            join,
        }
    }

    /// Sends one lifecycle or operation command to the isolated adapter thread.
    ///
    /// # Errors
    ///
    /// Returns an error if the worker has stopped.
    pub fn control(&self, command: AdapterWorkerControl) -> Result<(), ChannelGatewayError> {
        self.control
            .send(command)
            .map_err(|_| ChannelGatewayError::WorkerStopped)
    }

    /// Returns the next worker event without blocking the gateway scheduler.
    ///
    /// # Errors
    ///
    /// Returns an error if the worker has stopped and no events remain.
    pub fn try_event(&self) -> Result<Option<AdapterWorkerEvent>, ChannelGatewayError> {
        match self.events.try_recv() {
            Ok(event) => Ok(Some(event)),
            Err(TryRecvError::Empty) => Ok(None),
            Err(TryRecvError::Disconnected) => Err(ChannelGatewayError::WorkerStopped),
        }
    }

    /// Stops and joins the account worker.
    ///
    /// # Errors
    ///
    /// Returns an error if the worker thread panicked.
    pub fn shutdown(self) -> Result<(), ChannelGatewayError> {
        let _ = self.control.send(AdapterWorkerControl::Stop);
        self.join
            .join()
            .map_err(|_| ChannelGatewayError::WorkerPanicked)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum ChannelWorkerMode {
    Pull,
    Push,
}

pub struct ChannelGatewayRuntime {
    supervisor: ChannelGatewaySupervisor,
    workers: BTreeMap<ChannelAccountKey, ChannelAccountWorker>,
}

impl ChannelGatewayRuntime {
    pub const fn new(supervisor: ChannelGatewaySupervisor) -> Self {
        Self {
            supervisor,
            workers: BTreeMap::new(),
        }
    }

    pub const fn supervisor(&self) -> &ChannelGatewaySupervisor {
        &self.supervisor
    }

    pub fn supervisor_mut(&mut self) -> &mut ChannelGatewaySupervisor {
        &mut self.supervisor
    }

    /// Attaches one isolated real adapter worker to an already-durable account.
    ///
    /// # Errors
    ///
    /// Returns an error for unknown or duplicate account workers.
    pub fn attach_worker(
        &mut self,
        key: ChannelAccountKey,
        adapter: Box<dyn ManagedChannelAdapter>,
        reconnect: ReconnectPolicy,
        push_ingress: bool,
    ) -> Result<(), ChannelGatewayError> {
        if self.supervisor.record(&key).is_none() {
            return Err(ChannelGatewayError::UnknownAccount);
        }
        if adapter.kind() != key.kind || adapter.account_setup().account_id != key.account_id {
            return Err(ChannelGatewayError::CrossAccount);
        }
        if self.workers.contains_key(&key) {
            return Err(ChannelGatewayError::DuplicateAccount);
        }
        let worker = if push_ingress {
            ChannelAccountWorker::spawn_push(adapter, reconnect)
        } else {
            ChannelAccountWorker::spawn(adapter, reconnect)
        };
        self.workers.insert(key, worker);
        Ok(())
    }

    /// Drains bounded worker events, updating only the owning account's queue and health.
    ///
    /// # Errors
    ///
    /// Returns persistence/routing errors; a worker failure is recorded without stopping peers.
    pub fn maintain(&mut self, now: UtcTimestamp) -> Result<usize, ChannelGatewayError> {
        let released = self.supervisor.release_expired_rate_limits(now)?;
        for key in released {
            if let Some(worker) = self.workers.get(&key) {
                let _ = worker.control(AdapterWorkerControl::Reconnect);
            }
        }
        let keys = self.workers.keys().cloned().collect::<Vec<_>>();
        let mut processed = 0_usize;
        for key in keys {
            for _ in 0..64 {
                let event = match self.workers.get(&key) {
                    Some(worker) => worker.try_event(),
                    None => break,
                };
                let event = match event {
                    Ok(Some(event)) => event,
                    Ok(None) => break,
                    Err(ChannelGatewayError::WorkerStopped) => {
                        self.supervisor.record_adapter_error(
                            &key,
                            &ChannelAdapterErrorV2 {
                                kind: ChannelAdapterErrorKindV2::TransientNetwork,
                                safe_message: "channel adapter worker stopped".to_owned(),
                                retry_after_ms: None,
                            },
                            now,
                        )?;
                        break;
                    }
                    Err(error) => return Err(error),
                };
                processed = processed.saturating_add(1);
                self.apply_worker_event(&key, event, now)?;
            }
        }
        Ok(processed)
    }

    fn apply_worker_event(
        &mut self,
        key: &ChannelAccountKey,
        event: AdapterWorkerEvent,
        now: UtcTimestamp,
    ) -> Result<(), ChannelGatewayError> {
        match event {
            AdapterWorkerEvent::Started => {}
            AdapterWorkerEvent::Paused => {
                self.supervisor.pause(key, now)?;
            }
            AdapterWorkerEvent::Resumed => {
                self.supervisor.resume(key, now)?;
            }
            AdapterWorkerEvent::Inbound {
                event,
                reconnect_cursor,
            } => {
                if let Err(error) = self.supervisor.enqueue(key, &event, now) {
                    let account_error = match error {
                        ChannelGatewayError::Persistence(_) => return Err(error),
                        ChannelGatewayError::Adapter(error) => error,
                        other => ChannelAdapterErrorV2 {
                            kind: ChannelAdapterErrorKindV2::MalformedEvent,
                            safe_message: other.to_string(),
                            retry_after_ms: None,
                        },
                    };
                    self.supervisor
                        .record_adapter_error(key, &account_error, now)?;
                } else {
                    self.supervisor
                        .record_adapter_ready(key, reconnect_cursor, now)?;
                }
            }
            AdapterWorkerEvent::Executed(_) => {
                self.supervisor.record_delivery(key, now)?;
            }
            AdapterWorkerEvent::ConnectionTested {
                setup,
                reconnect_cursor,
            } => {
                if setup.account_id == key.account_id {
                    self.supervisor
                        .record_adapter_ready(key, reconnect_cursor, now)?;
                } else {
                    self.supervisor.record_adapter_error(
                        key,
                        &ChannelAdapterErrorV2 {
                            kind: ChannelAdapterErrorKindV2::Permission,
                            safe_message: "connection test resolved another account".to_owned(),
                            retry_after_ms: None,
                        },
                        now,
                    )?;
                }
            }
            AdapterWorkerEvent::Reconnected { reconnect_cursor } => {
                self.supervisor
                    .record_adapter_ready(key, reconnect_cursor, now)?;
            }
            AdapterWorkerEvent::Failed(error) => {
                self.supervisor.record_adapter_error(key, &error, now)?;
            }
            AdapterWorkerEvent::Stopped => {
                self.supervisor.record_adapter_error(
                    key,
                    &ChannelAdapterErrorV2 {
                        kind: ChannelAdapterErrorKindV2::TransientNetwork,
                        safe_message: "channel adapter worker stopped".to_owned(),
                        retry_after_ms: None,
                    },
                    now,
                )?;
            }
        }
        Ok(())
    }

    /// Sends an adapter-verified normalized webhook event to its isolated push worker.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown or stopped account worker.
    pub fn admit_verified_event(
        &self,
        key: &ChannelAccountKey,
        event: ChannelEventV2,
    ) -> Result<(), ChannelGatewayError> {
        self.worker(key)?
            .control(AdapterWorkerControl::AdmitVerified(Box::new(event)))
    }

    /// Runs the platform's safe read-only connection test on its isolated worker.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown or stopped account worker.
    pub fn test_connection(&self, key: &ChannelAccountKey) -> Result<(), ChannelGatewayError> {
        self.worker(key)?
            .control(AdapterWorkerControl::TestConnection)
    }

    /// Replaces an account worker after credential rotation or configuration migration.
    ///
    /// # Errors
    ///
    /// Returns an error for account mismatch, worker shutdown, or attachment failure.
    pub fn replace_worker(
        &mut self,
        key: &ChannelAccountKey,
        adapter: Box<dyn ManagedChannelAdapter>,
        reconnect: ReconnectPolicy,
        push_ingress: bool,
    ) -> Result<(), ChannelGatewayError> {
        if adapter.kind() != key.kind || adapter.account_setup().account_id != key.account_id {
            return Err(ChannelGatewayError::CrossAccount);
        }
        if let Some(worker) = self.workers.remove(key) {
            worker.shutdown()?;
        }
        self.attach_worker(key.clone(), adapter, reconnect, push_ingress)
    }

    /// Advances the durable credential reference and replaces the live adapter with an instance
    /// constructed from the new secret generation.
    ///
    /// # Errors
    ///
    /// Returns an error for account mismatch, invalid credential references, persistence, or
    /// worker replacement failure. A persisted generation remains reconnecting if replacement
    /// fails so the control plane never reports the old worker as healthy.
    #[allow(clippy::too_many_arguments)]
    pub fn rotate_credential(
        &mut self,
        key: &ChannelAccountKey,
        name: &str,
        reference: CredentialRef,
        adapter: Box<dyn ManagedChannelAdapter>,
        reconnect: ReconnectPolicy,
        push_ingress: bool,
        now: UtcTimestamp,
    ) -> Result<ChannelAccountHealth, ChannelGatewayError> {
        if adapter.kind() != key.kind || adapter.account_setup().account_id != key.account_id {
            return Err(ChannelGatewayError::CrossAccount);
        }
        let health = self
            .supervisor
            .rotate_credential(key, name, reference, now)?;
        self.replace_worker(key, adapter, reconnect, push_ingress)?;
        Ok(health)
    }

    /// Executes one outbound operation after exact account-route validation.
    ///
    /// # Errors
    ///
    /// Returns an error for cross-account routes or an unknown/stopped worker.
    pub fn execute(
        &self,
        key: &ChannelAccountKey,
        operation: ChannelOperationV2,
    ) -> Result<(), ChannelGatewayError> {
        self.supervisor.delivery_partition(key)?;
        if let Some((channel, external_account)) = operation_route(&operation)
            && (channel != key.kind.channel() || external_account != key.account_id)
        {
            return Err(ChannelGatewayError::CrossAccount);
        }
        self.worker(key)?
            .control(AdapterWorkerControl::Execute(Box::new(operation)))
    }

    /// Reserves account-local notification capacity before dispatching a proactive operation.
    ///
    /// # Errors
    ///
    /// Returns the same route, budget, account, isolation, and worker errors as the underlying
    /// supervisor and operation execution paths.
    pub fn execute_proactive(
        &mut self,
        key: &ChannelAccountKey,
        route_id: &str,
        operation: ChannelOperationV2,
        now: UtcTimestamp,
    ) -> Result<u32, ChannelGatewayError> {
        let remaining = self
            .supervisor
            .reserve_proactive_notification(key, route_id, now)?;
        self.execute(key, operation)?;
        Ok(remaining)
    }

    /// Pauses both the worker and its durable account scheduler.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown/stopped worker or failed durable update.
    pub fn pause(
        &mut self,
        key: &ChannelAccountKey,
        now: UtcTimestamp,
    ) -> Result<ChannelAccountHealth, ChannelGatewayError> {
        self.worker(key)?.control(AdapterWorkerControl::Pause)?;
        self.supervisor.pause(key, now)
    }

    /// Resumes both the worker and its durable account scheduler.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown/stopped worker or failed durable update.
    pub fn resume(
        &mut self,
        key: &ChannelAccountKey,
        now: UtcTimestamp,
    ) -> Result<ChannelAccountHealth, ChannelGatewayError> {
        self.worker(key)?.control(AdapterWorkerControl::Resume)?;
        self.supervisor.resume(key, now)
    }

    /// Stops the worker before durably removing its account state and routes.
    ///
    /// # Errors
    ///
    /// Returns an error for worker shutdown, unknown accounts, or failed durable removal.
    pub fn remove_account(
        &mut self,
        key: &ChannelAccountKey,
    ) -> Result<ChannelAccountRecord, ChannelGatewayError> {
        if let Some(worker) = self.workers.remove(key) {
            worker.shutdown()?;
        }
        self.supervisor.remove_account(key)
    }

    fn worker(
        &self,
        key: &ChannelAccountKey,
    ) -> Result<&ChannelAccountWorker, ChannelGatewayError> {
        self.workers
            .get(key)
            .ok_or(ChannelGatewayError::UnknownAccount)
    }
}

fn operation_route(operation: &ChannelOperationV2) -> Option<(&str, &str)> {
    let route = match operation {
        ChannelOperationV2::SendMessage(message) => &message.route,
        ChannelOperationV2::EditMessage { route, .. }
        | ChannelOperationV2::DeleteMessage { route, .. }
        | ChannelOperationV2::AddReaction { route, .. }
        | ChannelOperationV2::RemoveReaction { route, .. }
        | ChannelOperationV2::SetTyping { route, .. } => route,
        ChannelOperationV2::Cancel { .. } => return None,
    };
    Some((&route.channel, &route.external_account))
}

fn reconnect_adapter(
    adapter: &mut dyn ManagedChannelAdapter,
    policy: ReconnectPolicy,
) -> Result<(), ChannelAdapterErrorV2> {
    let mut attempt = 0;
    loop {
        match adapter.reconnect_v2() {
            Ok(()) => return Ok(()),
            Err(error) if error.is_retryable() => {
                let Some(delay) = policy.delay_ms(attempt) else {
                    return Err(error);
                };
                attempt = attempt.saturating_add(1);
                thread::sleep(Duration::from_millis(delay));
            }
            Err(error) => return Err(error),
        }
    }
}

#[derive(Debug)]
pub enum ChannelGatewayError {
    Invalid(String),
    Persistence(String),
    Queue(String),
    Catalog(ChannelCatalogError),
    Adapter(ChannelAdapterErrorV2),
    DuplicateAccount,
    UnknownAccount,
    AccountUnavailable,
    MissingRoute,
    AmbiguousRoute,
    CrossAccount,
    MentionRequired,
    ParticipantDenied,
    ProactivePostDenied,
    NotificationBudgetExhausted,
    UnsupportedEvent,
    WebhookAuthentication,
    WebhookReplay,
    WorkerStopped,
    WorkerPanicked,
}

impl std::fmt::Display for ChannelGatewayError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        match self {
            Self::Invalid(message) | Self::Persistence(message) | Self::Queue(message) => {
                formatter.write_str(message)
            }
            Self::Catalog(error) => write!(formatter, "{error}"),
            Self::Adapter(error) => formatter.write_str(&error.safe_message),
            Self::DuplicateAccount => formatter.write_str("channel account already exists"),
            Self::UnknownAccount => formatter.write_str("channel account was not found"),
            Self::AccountUnavailable => formatter.write_str("channel account is unavailable"),
            Self::MissingRoute => formatter.write_str("channel route was not found"),
            Self::AmbiguousRoute => formatter.write_str("channel route is ambiguous"),
            Self::CrossAccount => formatter.write_str("channel event crossed account isolation"),
            Self::MentionRequired => formatter.write_str("group route requires a mention"),
            Self::ParticipantDenied => formatter.write_str("group participant is not allowed"),
            Self::ProactivePostDenied => {
                formatter.write_str("channel route does not allow proactive posts")
            }
            Self::NotificationBudgetExhausted => {
                formatter.write_str("channel notification budget is exhausted")
            }
            Self::UnsupportedEvent => formatter.write_str("channel event has no session action"),
            Self::WebhookAuthentication => formatter.write_str("webhook authentication failed"),
            Self::WebhookReplay => formatter.write_str("webhook delivery was replayed"),
            Self::WorkerStopped => formatter.write_str("channel adapter worker stopped"),
            Self::WorkerPanicked => formatter.write_str("channel adapter worker panicked"),
        }
    }
}

impl std::error::Error for ChannelGatewayError {}
