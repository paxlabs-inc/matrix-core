#![forbid(unsafe_code)]

use std::fmt::Display;
use std::sync::{Mutex, MutexGuard};

use keith_agent_types::{
    ArtifactId, CURRENT_SCHEMA_VERSION, CommitmentId, DeliveryId, EntityId, EntryId, GoalId, JobId,
    ProfileId, Revision, SessionId, TurnId, UtcTimestamp,
};
use keith_channel_core::{AdapterFailure, OutboundMessage, ReplyRoute, RetryClass, SendReceipt};
use keith_protocol::DeliveryProjection;
use keith_state_store_core::{DeliveryRepository, VersionedRecord, WritePrecondition};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "source", content = "id")]
pub enum DeliverySource {
    Interactive(EntityId),
    Scheduled(JobId),
    Child(EntityId),
    Commitment(CommitmentId),
    Attention(EntityId),
    Refinement(EntityId),
    Goal(GoalId),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DeliveryState {
    Pending,
    Claimed,
    RetryScheduled,
    Sent,
    PermanentFailure,
    Cancelled,
}

impl DeliveryState {
    pub const fn is_terminal(self) -> bool {
        matches!(self, Self::Sent | Self::PermanentFailure | Self::Cancelled)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DeliveryItem {
    pub id: DeliveryId,
    pub stable_key: String,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub turn_id: Option<TurnId>,
    pub final_id: Option<EntryId>,
    pub source: DeliverySource,
    pub route: ReplyRoute,
    pub text: String,
    pub artifacts: Vec<ArtifactId>,
    pub state: DeliveryState,
    pub attempt_count: u32,
    pub not_before: UtcTimestamp,
    pub safe_error: Option<String>,
    pub receipt: Option<SendReceipt>,
    pub platform_idempotency: bool,
    pub possible_duplicate: bool,
    pub claim_token: Option<EntityId>,
    pub claim_expires_at: Option<UtcTimestamp>,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub revision: Revision,
}

impl DeliveryItem {
    pub fn projection(&self) -> DeliveryProjection {
        DeliveryProjection {
            delivery_id: self.id.clone(),
            state: delivery_state_name(self.state).to_owned(),
            terminal: self.state.is_terminal(),
            turn_id: self.turn_id.clone(),
            final_id: self.final_id.clone(),
            acknowledged: self.state == DeliveryState::Sent,
        }
    }
}

const fn delivery_state_name(state: DeliveryState) -> &'static str {
    match state {
        DeliveryState::Pending => "pending",
        DeliveryState::Claimed => "claimed",
        DeliveryState::RetryScheduled => "retry_scheduled",
        DeliveryState::Sent => "sent",
        DeliveryState::PermanentFailure => "permanent_failure",
        DeliveryState::Cancelled => "cancelled",
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NewDelivery {
    pub stable_key: String,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub turn_id: Option<TurnId>,
    pub final_id: Option<EntryId>,
    pub source: DeliverySource,
    pub route: ReplyRoute,
    pub text: String,
    pub artifacts: Vec<ArtifactId>,
    pub platform_idempotency: bool,
    pub not_before: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DeliveryClaim {
    pub item: DeliveryItem,
    pub token: EntityId,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DeliveryConfig {
    pub max_pending: usize,
    pub max_text_bytes: usize,
    pub max_artifacts: usize,
    pub max_attempts: u32,
    pub initial_backoff_ms: i64,
    pub max_backoff_ms: i64,
    pub claim_lease_ms: i64,
}

impl Default for DeliveryConfig {
    fn default() -> Self {
        Self {
            max_pending: 1_024,
            max_text_bytes: 256 * 1_024,
            max_artifacts: 32,
            max_attempts: 8,
            initial_backoff_ms: 1_000,
            max_backoff_ms: 15 * 60 * 1_000,
            claim_lease_ms: 60_000,
        }
    }
}

#[derive(Debug, Error)]
pub enum DeliveryError {
    #[error("delivery item is invalid")]
    Invalid,
    #[error("delivery outbox is full")]
    Full,
    #[error("delivery was not found")]
    NotFound,
    #[error("delivery stable key conflicts with different content")]
    StableKeyConflict,
    #[error("delivery claim is stale or owned by another worker")]
    StaleClaim,
    #[error("delivery state transition is illegal")]
    IllegalTransition,
    #[error("delivery revision overflow")]
    RevisionOverflow,
    #[error("delivery repository failed: {0}")]
    Repository(String),
    #[error("delivery record is corrupt: {0}")]
    Corrupt(String),
    #[error("delivery outbox lock was poisoned")]
    LockPoisoned,
}

struct StoredDelivery {
    item: DeliveryItem,
    storage_revision: Revision,
}

pub struct DeliveryOutbox<R> {
    repository: R,
    config: DeliveryConfig,
    serial: Mutex<()>,
}

impl<R> DeliveryOutbox<R>
where
    R: DeliveryRepository,
    R::Error: Display,
{
    /// Creates a bounded transactional delivery outbox.
    ///
    /// # Errors
    ///
    /// Returns an error for zero or negative limits.
    pub fn new(repository: R, config: DeliveryConfig) -> Result<Self, DeliveryError> {
        if config.max_pending == 0
            || config.max_text_bytes == 0
            || config.max_artifacts == 0
            || config.max_attempts == 0
            || config.initial_backoff_ms <= 0
            || config.max_backoff_ms < config.initial_backoff_ms
            || config.claim_lease_ms <= 0
        {
            return Err(DeliveryError::Invalid);
        }
        Ok(Self {
            repository,
            config,
            serial: Mutex::new(()),
        })
    }

    /// Inserts exactly one item per stable key, returning an identical prior item on replay.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid content, capacity, key conflict, or persistence failure.
    pub fn enqueue(
        &self,
        new: NewDelivery,
        now: UtcTimestamp,
    ) -> Result<DeliveryItem, DeliveryError> {
        validate_new(&new, self.config)?;
        let _guard = self.lock()?;
        let records = self.load_all()?;
        if let Some(existing) = records
            .iter()
            .find(|stored| stored.item.stable_key == new.stable_key)
        {
            if same_delivery(&existing.item, &new) {
                return Ok(existing.item.clone());
            }
            return Err(DeliveryError::StableKeyConflict);
        }
        if records
            .iter()
            .filter(|stored| !stored.item.state.is_terminal())
            .count()
            >= self.config.max_pending
        {
            return Err(DeliveryError::Full);
        }
        let item = DeliveryItem {
            id: DeliveryId::new(),
            stable_key: new.stable_key,
            profile_id: new.profile_id,
            session_id: new.session_id,
            turn_id: new.turn_id,
            final_id: new.final_id,
            source: new.source,
            route: new.route,
            text: new.text,
            artifacts: new.artifacts,
            state: DeliveryState::Pending,
            attempt_count: 0,
            not_before: new.not_before,
            safe_error: None,
            receipt: None,
            platform_idempotency: new.platform_idempotency,
            possible_duplicate: false,
            claim_token: None,
            claim_expires_at: None,
            created_at: now,
            updated_at: now,
            revision: Revision::ZERO,
        };
        self.repository
            .put_delivery(encode(&item)?, WritePrecondition::Missing)
            .map_err(repository_error)?;
        Ok(item)
    }

    /// Transactionally claims the oldest due item for one worker lease.
    ///
    /// # Errors
    ///
    /// Returns an error for persistence or revision failure.
    pub fn claim_next(&self, now: UtcTimestamp) -> Result<Option<DeliveryClaim>, DeliveryError> {
        self.claim_next_matching(now, |_| true)
    }

    /// Transactionally claims the oldest due item for one channel adapter.
    ///
    /// # Errors
    ///
    /// Returns an error for an empty channel, persistence, or revision failure.
    pub fn claim_next_for_channel(
        &self,
        channel: &str,
        now: UtcTimestamp,
    ) -> Result<Option<DeliveryClaim>, DeliveryError> {
        if channel.trim().is_empty() {
            return Err(DeliveryError::Invalid);
        }
        self.claim_next_matching(now, |item| item.route.channel == channel)
    }

    fn claim_next_matching(
        &self,
        now: UtcTimestamp,
        matches_route: impl Fn(&DeliveryItem) -> bool,
    ) -> Result<Option<DeliveryClaim>, DeliveryError> {
        let _guard = self.lock()?;
        let mut records = self.load_all()?;
        records.sort_by_key(|stored| (stored.item.not_before, stored.item.created_at));
        let Some(mut stored) = records.into_iter().find(|stored| {
            matches!(
                stored.item.state,
                DeliveryState::Pending | DeliveryState::RetryScheduled
            ) && stored.item.not_before <= now
                && matches_route(&stored.item)
        }) else {
            return Ok(None);
        };
        let token = EntityId::new();
        stored.item.state = DeliveryState::Claimed;
        stored.item.attempt_count = stored.item.attempt_count.saturating_add(1);
        stored.item.claim_token = Some(token.clone());
        stored.item.claim_expires_at = Some(UtcTimestamp::from_unix_millis(
            now.unix_millis().saturating_add(self.config.claim_lease_ms),
        ));
        stored.item.safe_error = None;
        self.put_existing(&mut stored, now)?;
        Ok(Some(DeliveryClaim {
            item: stored.item,
            token,
        }))
    }

    pub fn outbound(&self, claim: &DeliveryClaim) -> OutboundMessage {
        OutboundMessage {
            route: claim.item.route.clone(),
            idempotency_key: claim.item.stable_key.clone(),
            text: claim.item.text.clone(),
            artifacts: claim.item.artifacts.clone(),
        }
    }

    /// Persists a platform acknowledgement and marks the delivery sent.
    ///
    /// # Errors
    ///
    /// Returns an error for a stale claim or persistence failure.
    pub fn acknowledge(
        &self,
        claim: &DeliveryClaim,
        mut receipt: SendReceipt,
        now: UtcTimestamp,
    ) -> Result<DeliveryItem, DeliveryError> {
        let _guard = self.lock()?;
        let mut stored = self.required(&claim.item.id)?;
        verify_claim(&stored.item, claim)?;
        receipt.duplicate_possible |= stored.item.possible_duplicate;
        stored.item.state = DeliveryState::Sent;
        stored.item.receipt = Some(receipt);
        stored.item.safe_error = None;
        stored.item.claim_token = None;
        stored.item.claim_expires_at = None;
        self.put_existing(&mut stored, now)?;
        Ok(stored.item)
    }

    /// Classifies a failed send into bounded retry or permanent terminal state.
    ///
    /// # Errors
    ///
    /// Returns an error for a stale claim or persistence failure.
    pub fn fail(
        &self,
        claim: &DeliveryClaim,
        failure: &AdapterFailure,
        now: UtcTimestamp,
    ) -> Result<DeliveryItem, DeliveryError> {
        let _guard = self.lock()?;
        let mut stored = self.required(&claim.item.id)?;
        verify_claim(&stored.item, claim)?;
        let retryable = !matches!(failure.class, RetryClass::Permanent)
            && stored.item.attempt_count < self.config.max_attempts;
        stored.item.state = if retryable {
            DeliveryState::RetryScheduled
        } else {
            DeliveryState::PermanentFailure
        };
        stored.item.safe_error = Some(bounded_error(&failure.safe_message));
        stored.item.claim_token = None;
        stored.item.claim_expires_at = None;
        if retryable {
            let backoff = failure
                .retry_after_ms
                .and_then(|value| i64::try_from(value).ok())
                .unwrap_or_else(|| self.backoff_ms(stored.item.attempt_count));
            stored.item.not_before = UtcTimestamp::from_unix_millis(
                now.unix_millis()
                    .saturating_add(backoff.min(self.config.max_backoff_ms)),
            );
        }
        self.put_existing(&mut stored, now)?;
        Ok(stored.item)
    }

    /// Recovers expired claims after worker/daemon restart.
    ///
    /// Ambiguous sends are marked possible-duplicate unless platform idempotency is available.
    ///
    /// # Errors
    ///
    /// Returns an error for persistence failure.
    pub fn recover_expired(&self, now: UtcTimestamp) -> Result<Vec<DeliveryItem>, DeliveryError> {
        let _guard = self.lock()?;
        let mut recovered = Vec::new();
        for mut stored in self.load_all()?.into_iter().filter(|stored| {
            stored.item.state == DeliveryState::Claimed
                && stored
                    .item
                    .claim_expires_at
                    .is_some_and(|expiry| expiry <= now)
        }) {
            stored.item.state = if stored.item.attempt_count >= self.config.max_attempts {
                DeliveryState::PermanentFailure
            } else {
                DeliveryState::RetryScheduled
            };
            stored.item.possible_duplicate |= !stored.item.platform_idempotency;
            stored.item.safe_error =
                Some("delivery claim expired before acknowledgement".to_owned());
            stored.item.claim_token = None;
            stored.item.claim_expires_at = None;
            stored.item.not_before = now;
            self.put_existing(&mut stored, now)?;
            recovered.push(stored.item);
        }
        Ok(recovered)
    }

    /// Cancels a non-terminal item and makes its state visible to clients.
    ///
    /// # Errors
    ///
    /// Returns an error for missing, terminal, or unpersistable records.
    pub fn cancel(
        &self,
        id: &DeliveryId,
        reason: &str,
        now: UtcTimestamp,
    ) -> Result<DeliveryItem, DeliveryError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        if stored.item.state.is_terminal() || reason.trim().is_empty() {
            return Err(DeliveryError::IllegalTransition);
        }
        stored.item.state = DeliveryState::Cancelled;
        stored.item.safe_error = Some(bounded_error(reason));
        stored.item.claim_token = None;
        stored.item.claim_expires_at = None;
        self.put_existing(&mut stored, now)?;
        Ok(stored.item)
    }

    /// Reads one visible delivery state.
    ///
    /// # Errors
    ///
    /// Returns an error when the repository record is corrupt or unavailable.
    pub fn get(&self, id: &DeliveryId) -> Result<Option<DeliveryItem>, DeliveryError> {
        let _guard = self.lock()?;
        self.load(id).map(|stored| stored.map(|stored| stored.item))
    }

    /// Lists all visible delivery states.
    ///
    /// # Errors
    ///
    /// Returns an error when repository records are corrupt or unavailable.
    pub fn list(&self) -> Result<Vec<DeliveryItem>, DeliveryError> {
        let _guard = self.lock()?;
        Ok(self
            .load_all()?
            .into_iter()
            .map(|stored| stored.item)
            .collect())
    }

    fn backoff_ms(&self, attempt: u32) -> i64 {
        let exponent = attempt.saturating_sub(1).min(62);
        self.config
            .initial_backoff_ms
            .saturating_mul(1_i64.checked_shl(exponent).unwrap_or(i64::MAX))
            .min(self.config.max_backoff_ms)
    }

    fn required(&self, id: &DeliveryId) -> Result<StoredDelivery, DeliveryError> {
        self.load(id)?.ok_or(DeliveryError::NotFound)
    }

    fn load(&self, id: &DeliveryId) -> Result<Option<StoredDelivery>, DeliveryError> {
        self.repository
            .get_delivery(id.as_entity_id())
            .map_err(repository_error)?
            .map(decode)
            .transpose()
    }

    fn load_all(&self) -> Result<Vec<StoredDelivery>, DeliveryError> {
        self.repository
            .list_deliveries()
            .map_err(repository_error)?
            .into_iter()
            .map(decode)
            .collect()
    }

    fn put_existing(
        &self,
        stored: &mut StoredDelivery,
        now: UtcTimestamp,
    ) -> Result<(), DeliveryError> {
        let next = stored
            .storage_revision
            .checked_next()
            .ok_or(DeliveryError::RevisionOverflow)?;
        stored.item.revision = next;
        stored.item.updated_at = now;
        self.repository
            .put_delivery(
                encode(&stored.item)?,
                WritePrecondition::Exact(stored.storage_revision),
            )
            .map_err(repository_error)?;
        stored.storage_revision = next;
        Ok(())
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, DeliveryError> {
        self.serial.lock().map_err(|_| DeliveryError::LockPoisoned)
    }
}

fn validate_new(new: &NewDelivery, config: DeliveryConfig) -> Result<(), DeliveryError> {
    if new.stable_key.trim().is_empty()
        || new.stable_key.len() > 256
        || new.text.trim().is_empty()
        || new.text.len() > config.max_text_bytes
        || new.artifacts.len() > config.max_artifacts
        || new.route.channel.trim().is_empty()
        || new.route.external_account.trim().is_empty()
        || new.route.conversation.trim().is_empty()
    {
        return Err(DeliveryError::Invalid);
    }
    Ok(())
}

fn same_delivery(item: &DeliveryItem, new: &NewDelivery) -> bool {
    item.profile_id == new.profile_id
        && item.session_id == new.session_id
        && item.turn_id == new.turn_id
        && item.final_id == new.final_id
        && item.source == new.source
        && item.route == new.route
        && item.text == new.text
        && item.artifacts == new.artifacts
        && item.platform_idempotency == new.platform_idempotency
}

fn verify_claim(item: &DeliveryItem, claim: &DeliveryClaim) -> Result<(), DeliveryError> {
    if item.state != DeliveryState::Claimed || item.claim_token.as_ref() != Some(&claim.token) {
        return Err(DeliveryError::StaleClaim);
    }
    Ok(())
}

fn bounded_error(value: &str) -> String {
    const MAX: usize = 512;
    let value = value.trim();
    if value.len() <= MAX {
        return value.to_owned();
    }
    let mut boundary = MAX;
    while !value.is_char_boundary(boundary) {
        boundary -= 1;
    }
    value[..boundary].to_owned()
}

fn encode(item: &DeliveryItem) -> Result<VersionedRecord, DeliveryError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: item.id.as_entity_id().clone(),
        revision: item.revision,
        updated_at: item.updated_at,
        payload: serde_json::to_value(item)
            .map_err(|error| DeliveryError::Corrupt(error.to_string()))?,
    })
}

fn decode(record: VersionedRecord) -> Result<StoredDelivery, DeliveryError> {
    let item: DeliveryItem = serde_json::from_value(record.payload)
        .map_err(|error| DeliveryError::Corrupt(error.to_string()))?;
    if item.id.as_entity_id() != &record.id || item.revision != record.revision {
        return Err(DeliveryError::Corrupt(
            "record identity or revision mismatch".to_owned(),
        ));
    }
    Ok(StoredDelivery {
        item,
        storage_revision: record.revision,
    })
}

fn repository_error(error: impl Display) -> DeliveryError {
    DeliveryError::Repository(error.to_string())
}

#[cfg(test)]
mod tests {
    use std::io::{BufRead, BufReader};
    use std::thread;

    use keith_channel_adapters::JsonLineAdapter;
    use keith_channel_core::{AdapterCapability, AdapterFeatures, ChannelAdapter};
    use keith_connection::{LocalStream, local_stream_pair};
    use keith_state_store::EmbeddedStore;
    use tempfile::TempDir;

    use super::*;

    fn new_delivery(key: &str, idempotent: bool) -> NewDelivery {
        NewDelivery {
            stable_key: key.to_owned(),
            profile_id: ProfileId::new(),
            session_id: SessionId::new(),
            turn_id: None,
            final_id: None,
            source: DeliverySource::Attention(EntityId::new()),
            route: ReplyRoute {
                channel: "json".to_owned(),
                external_account: "account".to_owned(),
                conversation: "conversation".to_owned(),
                thread: None,
                reply_to_message: None,
            },
            text: "result".to_owned(),
            artifacts: Vec::new(),
            platform_idempotency: idempotent,
            not_before: UtcTimestamp::UNIX_EPOCH,
        }
    }

    fn adapter(stream: LocalStream) -> JsonLineAdapter<LocalStream> {
        JsonLineAdapter::new(
            stream,
            AdapterFeatures {
                capabilities: std::collections::BTreeSet::from([AdapterCapability::IdempotentSend]),
                max_attachment_bytes: 1_024,
                requests_per_minute: None,
            },
            4_096,
        )
    }

    #[test]
    fn stable_keys_claim_retry_permanent_failure_and_cancellation_are_visible() {
        let outbox = DeliveryOutbox::new(
            EmbeddedStore::open_in_memory().expect("store"),
            DeliveryConfig {
                max_attempts: 2,
                initial_backoff_ms: 10,
                max_backoff_ms: 20,
                ..DeliveryConfig::default()
            },
        )
        .expect("outbox");
        let mut new = new_delivery("stable", true);
        new.turn_id = Some(TurnId::new());
        new.final_id = Some(EntryId::new());
        let first = outbox
            .enqueue(new.clone(), UtcTimestamp::UNIX_EPOCH)
            .expect("enqueue");
        let replay = outbox
            .enqueue(new, UtcTimestamp::from_unix_millis(1))
            .expect("replay");
        assert_eq!(first.id, replay.id);
        assert_eq!(first.projection().turn_id, first.turn_id);
        assert_eq!(first.projection().final_id, first.final_id);
        assert!(!first.projection().acknowledged);
        let claim = outbox
            .claim_next(UtcTimestamp::UNIX_EPOCH)
            .expect("claim")
            .expect("due");
        let retry = outbox
            .fail(
                &claim,
                &AdapterFailure {
                    class: RetryClass::Retryable,
                    safe_message: "temporary".to_owned(),
                    retry_after_ms: None,
                },
                UtcTimestamp::UNIX_EPOCH,
            )
            .expect("retry");
        assert_eq!(retry.state, DeliveryState::RetryScheduled);
        assert_eq!(retry.not_before, UtcTimestamp::from_unix_millis(10));
        let claim = outbox
            .claim_next(UtcTimestamp::from_unix_millis(10))
            .expect("claim again")
            .expect("due again");
        let failed = outbox
            .fail(
                &claim,
                &AdapterFailure {
                    class: RetryClass::Retryable,
                    safe_message: "still unavailable".to_owned(),
                    retry_after_ms: None,
                },
                UtcTimestamp::from_unix_millis(10),
            )
            .expect("permanent after max");
        assert_eq!(failed.state, DeliveryState::PermanentFailure);
        assert!(failed.projection().terminal);

        let cancel = outbox
            .enqueue(new_delivery("cancel", true), UtcTimestamp::UNIX_EPOCH)
            .expect("enqueue cancellation");
        assert_eq!(
            outbox
                .cancel(
                    &cancel.id,
                    "user cancelled",
                    UtcTimestamp::from_unix_millis(1)
                )
                .expect("cancel")
                .state,
            DeliveryState::Cancelled
        );
    }

    #[test]
    fn real_send_before_acknowledgement_failure_is_classified_for_retry() {
        let outbox = DeliveryOutbox::new(
            EmbeddedStore::open_in_memory().expect("store"),
            DeliveryConfig::default(),
        )
        .expect("outbox");
        outbox
            .enqueue(new_delivery("before-ack", false), UtcTimestamp::UNIX_EPOCH)
            .expect("enqueue");
        let claim = outbox
            .claim_next(UtcTimestamp::UNIX_EPOCH)
            .expect("claim")
            .expect("item");
        let (platform, gateway) = local_stream_pair().expect("socket pair");
        drop(platform);
        let failure = adapter(gateway)
            .send(&outbox.outbound(&claim))
            .expect_err("closed platform must fail");
        let item = outbox
            .fail(&claim, &failure, UtcTimestamp::UNIX_EPOCH)
            .expect("schedule retry");
        assert_eq!(item.state, DeliveryState::RetryScheduled);
        assert!(!item.possible_duplicate);
    }

    #[test]
    fn acknowledgement_crash_recovers_with_honest_duplicate_state_and_receipt() {
        let root = TempDir::new().expect("root");
        let database = root.path().join("deliveries.db");
        let config = DeliveryConfig {
            claim_lease_ms: 10,
            ..DeliveryConfig::default()
        };
        let outbox =
            DeliveryOutbox::new(EmbeddedStore::open(&database, None).expect("store"), config)
                .expect("outbox");
        outbox
            .enqueue(new_delivery("after-ack", false), UtcTimestamp::UNIX_EPOCH)
            .expect("enqueue");
        let first_claim = outbox
            .claim_next(UtcTimestamp::UNIX_EPOCH)
            .expect("claim")
            .expect("item");
        let (platform, gateway) = local_stream_pair().expect("socket pair");
        let platform_thread = thread::spawn(move || {
            let mut line = String::new();
            BufReader::new(platform)
                .read_line(&mut line)
                .expect("platform receives send");
            line
        });
        let receipt = adapter(gateway)
            .send(&outbox.outbound(&first_claim))
            .expect("platform acknowledged");
        assert!(!platform_thread.join().expect("platform").is_empty());
        drop(outbox);

        let restarted = DeliveryOutbox::new(
            EmbeddedStore::open(&database, None).expect("restart store"),
            config,
        )
        .expect("restart outbox");
        let recovered = restarted
            .recover_expired(UtcTimestamp::from_unix_millis(10))
            .expect("recover ambiguous claim");
        assert_eq!(recovered.len(), 1);
        assert!(recovered[0].possible_duplicate);
        let second_claim = restarted
            .claim_next(UtcTimestamp::from_unix_millis(10))
            .expect("reclaim")
            .expect("item");
        let sent = restarted
            .acknowledge(&second_claim, receipt, UtcTimestamp::from_unix_millis(11))
            .expect("persist receipt");
        assert_eq!(sent.state, DeliveryState::Sent);
        assert!(sent.receipt.expect("receipt").duplicate_possible);
        assert_eq!(restarted.list().expect("list").len(), 1);
    }
}
