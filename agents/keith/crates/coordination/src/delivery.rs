use std::collections::BTreeMap;
use std::fmt::Display;
use std::sync::{Mutex, MutexGuard};

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, ConversationId, DeliveryId, EntityId, EventId, ProfileId, Revision,
    SessionId, UtcTimestamp,
};
use keith_state_store_core::StateRecordRepository;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    ConversationDelivery, CoordinationWrite, DeliveryClaim, DeliveryState,
    DurableCoordinationRepository, MAX_SAFE_DETAIL_BYTES, MAX_STABLE_KEY_BYTES, SupersessionTarget,
    TargetedSupersession, WritePrecondition,
};

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct DeliveryCoordinatorConfig {
    pub max_queued: usize,
    pub max_installation_claims: usize,
    pub max_claims_per_profile: usize,
    pub max_attempts: u32,
    pub lease_millis: i64,
    pub initial_backoff_millis: i64,
    pub max_backoff_millis: i64,
}

impl Default for DeliveryCoordinatorConfig {
    fn default() -> Self {
        Self {
            max_queued: 16_384,
            max_installation_claims: 64,
            max_claims_per_profile: 4,
            max_attempts: 8,
            lease_millis: 60_000,
            initial_backoff_millis: 1_000,
            max_backoff_millis: 300_000,
        }
    }
}

impl DeliveryCoordinatorConfig {
    fn validate(self) -> Result<Self, DeliveryCoordinatorError> {
        if self.max_queued == 0
            || self.max_installation_claims == 0
            || self.max_claims_per_profile == 0
            || self.max_claims_per_profile > self.max_installation_claims
            || self.max_attempts == 0
            || self.lease_millis <= 0
            || self.initial_backoff_millis <= 0
            || self.max_backoff_millis < self.initial_backoff_millis
        {
            return Err(DeliveryCoordinatorError::Invalid(
                "delivery coordinator limits are inconsistent",
            ));
        }
        Ok(self)
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConversationDeliveryEnqueue {
    pub stable_source_key: String,
    pub conversation_id: ConversationId,
    pub source_event_id: EventId,
    pub source_profile_id: ProfileId,
    pub destination_profile_id: ProfileId,
    pub participant_session_id: SessionId,
    pub policy_snapshot_key: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConversationDeliveryClaim {
    pub delivery: ConversationDelivery,
    pub token: EntityId,
    pub fence: u64,
    pub lease_expires_at: UtcTimestamp,
}

impl ConversationDeliveryClaim {
    fn matches(&self, delivery: &ConversationDelivery, now: UtcTimestamp) -> bool {
        delivery.state == DeliveryState::Claimed
            && delivery.id == self.delivery.id
            && delivery.revision == self.delivery.revision
            && delivery.claim.as_ref().is_some_and(|claim| {
                claim.token == self.token
                    && claim.fence == self.fence
                    && claim.expires_at == self.lease_expires_at
                    && claim.expires_at > now
            })
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ProcessingGuarantee {
    AtLeastOnce,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum DeliveryProjectionPrincipal {
    HumanOwner,
    Profile(ProfileId),
}

pub trait DeliveryProjectionAuthorizer {
    type Error: std::error::Error + Send + Sync + 'static;

    fn can_view_delivery(
        &self,
        principal: &DeliveryProjectionPrincipal,
        delivery: &ConversationDelivery,
    ) -> Result<bool, Self::Error>;
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SafeDeliveryProjection {
    pub id: DeliveryId,
    pub conversation_id: ConversationId,
    pub source_event_id: EventId,
    pub source_profile_id: ProfileId,
    pub destination_profile_id: ProfileId,
    pub participant_session_id: SessionId,
    pub state: DeliveryState,
    pub attempt_count: u32,
    pub retry_at: Option<UtcTimestamp>,
    pub lease_expires_at: Option<UtcTimestamp>,
    pub safe_error: Option<String>,
    pub supersession: Option<TargetedSupersession>,
    pub revision: Revision,
}

impl From<ConversationDelivery> for SafeDeliveryProjection {
    fn from(value: ConversationDelivery) -> Self {
        Self {
            id: value.id,
            conversation_id: value.conversation_id,
            source_event_id: value.source_event_id,
            source_profile_id: value.source_profile_id,
            destination_profile_id: value.destination_profile_id,
            participant_session_id: value.participant_session_id,
            state: value.state,
            attempt_count: value.attempt_count,
            retry_at: value.retry_at,
            lease_expires_at: value.claim.map(|claim| claim.expires_at),
            safe_error: value.safe_error,
            supersession: value.supersession,
            revision: value.revision,
        }
    }
}

#[derive(Debug, Error)]
pub enum DeliveryCoordinatorError {
    #[error("invalid delivery request: {0}")]
    Invalid(&'static str),
    #[error("delivery coordinator repository failed: {0}")]
    Repository(String),
    #[error("delivery was not found")]
    NotFound,
    #[error("delivery claim is stale or expired")]
    StaleClaim,
    #[error("delivery is not eligible for this operation")]
    NotEligible,
    #[error("delivery queue reached its configured bound")]
    QueueFull,
    #[error("delivery projection is not authorized")]
    Unauthorized,
    #[error("delivery revision overflow")]
    RevisionOverflow,
}

pub struct ConversationDeliveryCoordinator<R> {
    repository: DurableCoordinationRepository<R>,
    config: DeliveryCoordinatorConfig,
    serial: Mutex<()>,
    fairness_cursor: Mutex<Option<ProfileId>>,
}

impl<R> ConversationDeliveryCoordinator<R>
where
    R: StateRecordRepository,
    R::Error: Display,
{
    pub fn new(
        repository: R,
        config: DeliveryCoordinatorConfig,
    ) -> Result<Self, DeliveryCoordinatorError> {
        Ok(Self {
            repository: DurableCoordinationRepository::new(repository),
            config: config.validate()?,
            serial: Mutex::new(()),
            fairness_cursor: Mutex::new(None),
        })
    }

    pub const fn processing_guarantee(&self) -> ProcessingGuarantee {
        ProcessingGuarantee::AtLeastOnce
    }

    pub fn enqueue(
        &self,
        request: ConversationDeliveryEnqueue,
    ) -> Result<ConversationDelivery, DeliveryCoordinatorError> {
        validate_enqueue(&request)?;
        let _guard = self.lock()?;
        let deliveries = self.list()?;
        if let Some(existing) = deliveries
            .iter()
            .find(|delivery| delivery.stable_source_key == request.stable_source_key)
        {
            if same_enqueue(existing, &request) {
                return Ok(existing.clone());
            }
            return Err(DeliveryCoordinatorError::Invalid(
                "stable source key is already bound to different work",
            ));
        }
        let queued = deliveries
            .iter()
            .filter(|delivery| !is_terminal(delivery.state))
            .count();
        if queued >= self.config.max_queued {
            return Err(DeliveryCoordinatorError::QueueFull);
        }
        let value = ConversationDelivery {
            version: CURRENT_SCHEMA_VERSION,
            id: delivery_id(&request.stable_source_key),
            stable_source_key: request.stable_source_key,
            conversation_id: request.conversation_id,
            source_event_id: request.source_event_id,
            source_profile_id: request.source_profile_id,
            destination_profile_id: request.destination_profile_id,
            participant_session_id: request.participant_session_id,
            policy_snapshot_key: request.policy_snapshot_key,
            state: DeliveryState::Pending,
            attempt_count: 0,
            last_claim_fence: 0,
            claim: None,
            retry_at: None,
            safe_error: None,
            supersession: None,
            revision: Revision::ZERO,
        };
        self.write(value.clone(), WritePrecondition::Missing)?;
        Ok(value)
    }

    pub fn claim_next(
        &self,
        now: UtcTimestamp,
    ) -> Result<Option<ConversationDeliveryClaim>, DeliveryCoordinatorError> {
        let _guard = self.lock()?;
        self.recover_expired_locked(now)?;
        let deliveries = self.list()?;
        let active = deliveries
            .iter()
            .filter(|delivery| active_claim(delivery, now))
            .collect::<Vec<_>>();
        if active.len() >= self.config.max_installation_claims {
            return Ok(None);
        }
        let mut active_by_profile = BTreeMap::<ProfileId, usize>::new();
        for delivery in active {
            *active_by_profile
                .entry(delivery.destination_profile_id.clone())
                .or_default() += 1;
        }
        let mut eligible = deliveries
            .into_iter()
            .filter(|delivery| {
                matches!(
                    delivery.state,
                    DeliveryState::Pending | DeliveryState::Retryable
                ) && delivery.retry_at.is_none_or(|retry_at| retry_at <= now)
                    && delivery.attempt_count < self.config.max_attempts
                    && active_by_profile
                        .get(&delivery.destination_profile_id)
                        .copied()
                        .unwrap_or_default()
                        < self.config.max_claims_per_profile
            })
            .collect::<Vec<_>>();
        if eligible.is_empty() {
            return Ok(None);
        }
        eligible.sort_by(|left, right| {
            left.attempt_count
                .cmp(&right.attempt_count)
                .then_with(|| left.stable_source_key.cmp(&right.stable_source_key))
        });
        let mut profiles = eligible
            .iter()
            .map(|delivery| delivery.destination_profile_id.clone())
            .collect::<Vec<_>>();
        profiles.sort();
        profiles.dedup();
        let selected_profile = {
            let cursor = self.fairness_cursor()?;
            cursor
                .as_ref()
                .and_then(|previous| profiles.iter().find(|profile| *profile > previous))
                .cloned()
                .unwrap_or_else(|| profiles[0].clone())
        };
        let mut value = eligible
            .into_iter()
            .find(|delivery| delivery.destination_profile_id == selected_profile)
            .ok_or(DeliveryCoordinatorError::NotEligible)?;
        let previous_revision = value.revision;
        let revision = next_revision(previous_revision)?;
        let attempt = value
            .attempt_count
            .checked_add(1)
            .ok_or(DeliveryCoordinatorError::RevisionOverflow)?;
        let fence = value
            .last_claim_fence
            .checked_add(1)
            .ok_or(DeliveryCoordinatorError::RevisionOverflow)?;
        let token = EntityId::new();
        let expires_at = add_millis(now, self.config.lease_millis);
        value.state = DeliveryState::Claimed;
        value.attempt_count = attempt;
        value.last_claim_fence = fence;
        value.retry_at = None;
        value.safe_error = None;
        value.revision = revision;
        value.claim = Some(DeliveryClaim {
            token: token.clone(),
            fence,
            owner_profile_id: value.destination_profile_id.clone(),
            attempt,
            revision,
            expires_at,
        });
        self.write(
            value.clone(),
            WritePrecondition::Revision(previous_revision),
        )?;
        *self.fairness_cursor()? = Some(selected_profile);
        Ok(Some(ConversationDeliveryClaim {
            delivery: value,
            token,
            fence,
            lease_expires_at: expires_at,
        }))
    }

    pub fn renew(
        &self,
        claim: &ConversationDeliveryClaim,
        now: UtcTimestamp,
    ) -> Result<ConversationDeliveryClaim, DeliveryCoordinatorError> {
        let _guard = self.lock()?;
        let mut value = self.required(&claim.delivery.id)?;
        if !claim.matches(&value, now) {
            return Err(DeliveryCoordinatorError::StaleClaim);
        }
        let previous_revision = value.revision;
        let revision = next_revision(previous_revision)?;
        let fence = value
            .last_claim_fence
            .checked_add(1)
            .ok_or(DeliveryCoordinatorError::RevisionOverflow)?;
        let expires_at = add_millis(now, self.config.lease_millis);
        value.last_claim_fence = fence;
        value.revision = revision;
        value.claim = Some(DeliveryClaim {
            token: claim.token.clone(),
            fence,
            owner_profile_id: value.destination_profile_id.clone(),
            attempt: value.attempt_count,
            revision,
            expires_at,
        });
        self.write(
            value.clone(),
            WritePrecondition::Revision(previous_revision),
        )?;
        Ok(ConversationDeliveryClaim {
            delivery: value,
            token: claim.token.clone(),
            fence,
            lease_expires_at: expires_at,
        })
    }

    pub fn finalize(
        &self,
        claim: &ConversationDeliveryClaim,
        now: UtcTimestamp,
    ) -> Result<ConversationDelivery, DeliveryCoordinatorError> {
        self.finish_claim(claim, now, DeliveryState::Finalized, None, None)
    }

    pub fn retry(
        &self,
        claim: &ConversationDeliveryClaim,
        now: UtcTimestamp,
        safe_error: impl Into<String>,
    ) -> Result<ConversationDelivery, DeliveryCoordinatorError> {
        let detail = safe_detail(safe_error.into())?;
        let target = if claim.delivery.attempt_count >= self.config.max_attempts {
            DeliveryState::DeadLetter
        } else {
            DeliveryState::Retryable
        };
        let retry_at = (target == DeliveryState::Retryable)
            .then(|| add_millis(now, self.backoff_millis(claim.delivery.attempt_count)));
        self.finish_claim(claim, now, target, Some(detail), retry_at)
    }

    pub fn acknowledge_publication(
        &self,
        id: &DeliveryId,
        expected_revision: Revision,
    ) -> Result<ConversationDelivery, DeliveryCoordinatorError> {
        let _guard = self.lock()?;
        let mut value = self.required(id)?;
        if value.state == DeliveryState::Published {
            return Ok(value);
        }
        if value.state != DeliveryState::Finalized || value.revision != expected_revision {
            return Err(DeliveryCoordinatorError::NotEligible);
        }
        let previous = value.revision;
        value.revision = next_revision(previous)?;
        value.state = DeliveryState::Published;
        self.write(value.clone(), WritePrecondition::Revision(previous))?;
        Ok(value)
    }

    pub fn cancel(
        &self,
        id: &DeliveryId,
        expected_revision: Revision,
        safe_reason: impl Into<String>,
    ) -> Result<ConversationDelivery, DeliveryCoordinatorError> {
        self.terminal_transition(
            id,
            expected_revision,
            DeliveryState::Cancelled,
            safe_reason.into(),
            None,
        )
    }

    pub fn supersede(
        &self,
        id: &DeliveryId,
        expected_revision: Revision,
        target: SupersessionTarget,
        superseded_by_event_id: EventId,
        reason: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<ConversationDelivery, DeliveryCoordinatorError> {
        let reason = safe_detail(reason.into())?;
        let supersession = TargetedSupersession {
            target,
            superseded_by_event_id,
            reason: reason.clone(),
            occurred_at: now,
        };
        self.terminal_transition(
            id,
            expected_revision,
            DeliveryState::Superseded,
            reason,
            Some(supersession),
        )
    }

    pub fn recover_expired(
        &self,
        now: UtcTimestamp,
    ) -> Result<Vec<ConversationDelivery>, DeliveryCoordinatorError> {
        let _guard = self.lock()?;
        self.recover_expired_locked(now)
    }

    pub fn safe_projection<A: DeliveryProjectionAuthorizer>(
        &self,
        principal: &DeliveryProjectionPrincipal,
        authorizer: &A,
    ) -> Result<Vec<SafeDeliveryProjection>, DeliveryCoordinatorError> {
        let _guard = self.lock()?;
        let mut projected = Vec::new();
        for delivery in self.list()? {
            let authorized = authorizer
                .can_view_delivery(principal, &delivery)
                .map_err(|error| DeliveryCoordinatorError::Repository(error.to_string()))?;
            if authorized {
                projected.push(delivery.into());
            }
        }
        Ok(projected)
    }

    fn finish_claim(
        &self,
        claim: &ConversationDeliveryClaim,
        now: UtcTimestamp,
        state: DeliveryState,
        safe_error: Option<String>,
        retry_at: Option<UtcTimestamp>,
    ) -> Result<ConversationDelivery, DeliveryCoordinatorError> {
        let _guard = self.lock()?;
        let mut value = self.required(&claim.delivery.id)?;
        if !claim.matches(&value, now) {
            return Err(DeliveryCoordinatorError::StaleClaim);
        }
        let previous = value.revision;
        value.revision = next_revision(previous)?;
        value.state = state;
        value.claim = None;
        value.retry_at = retry_at;
        value.safe_error = safe_error;
        self.write(value.clone(), WritePrecondition::Revision(previous))?;
        Ok(value)
    }

    fn terminal_transition(
        &self,
        id: &DeliveryId,
        expected_revision: Revision,
        state: DeliveryState,
        reason: String,
        supersession: Option<TargetedSupersession>,
    ) -> Result<ConversationDelivery, DeliveryCoordinatorError> {
        let reason = safe_detail(reason)?;
        let _guard = self.lock()?;
        let mut value = self.required(id)?;
        if value.revision != expected_revision
            || !matches!(
                value.state,
                DeliveryState::Pending | DeliveryState::Claimed | DeliveryState::Retryable
            )
        {
            return Err(DeliveryCoordinatorError::NotEligible);
        }
        value.revision = next_revision(expected_revision)?;
        value.state = state;
        value.claim = None;
        value.retry_at = None;
        value.safe_error = Some(reason);
        value.supersession = supersession;
        self.write(
            value.clone(),
            WritePrecondition::Revision(expected_revision),
        )?;
        Ok(value)
    }

    fn recover_expired_locked(
        &self,
        now: UtcTimestamp,
    ) -> Result<Vec<ConversationDelivery>, DeliveryCoordinatorError> {
        let mut recovered = Vec::new();
        for mut value in self.list()?.into_iter().filter(|delivery| {
            delivery.state == DeliveryState::Claimed
                && delivery
                    .claim
                    .as_ref()
                    .is_some_and(|claim| claim.expires_at <= now)
        }) {
            let previous = value.revision;
            value.revision = next_revision(previous)?;
            value.claim = None;
            value.safe_error = Some("worker lease expired; processing outcome is unknown".into());
            if value.attempt_count >= self.config.max_attempts {
                value.state = DeliveryState::DeadLetter;
                value.retry_at = None;
            } else {
                value.state = DeliveryState::Retryable;
                value.retry_at = Some(add_millis(now, self.backoff_millis(value.attempt_count)));
            }
            self.write(value.clone(), WritePrecondition::Revision(previous))?;
            recovered.push(value);
        }
        Ok(recovered)
    }

    fn backoff_millis(&self, attempt: u32) -> i64 {
        let shift = attempt.saturating_sub(1).min(30);
        self.config
            .initial_backoff_millis
            .saturating_mul(1_i64 << shift)
            .min(self.config.max_backoff_millis)
    }

    fn required(&self, id: &DeliveryId) -> Result<ConversationDelivery, DeliveryCoordinatorError> {
        self.repository
            .delivery(id)
            .map_err(repository_error)?
            .ok_or(DeliveryCoordinatorError::NotFound)
    }

    fn list(&self) -> Result<Vec<ConversationDelivery>, DeliveryCoordinatorError> {
        self.repository.deliveries().map_err(repository_error)
    }

    fn write(
        &self,
        delivery: ConversationDelivery,
        precondition: WritePrecondition,
    ) -> Result<(), DeliveryCoordinatorError> {
        self.repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(delivery, precondition)])
            .map_err(repository_error)
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, DeliveryCoordinatorError> {
        self.serial
            .lock()
            .map_err(|_| DeliveryCoordinatorError::Repository("delivery lock poisoned".into()))
    }

    fn fairness_cursor(
        &self,
    ) -> Result<MutexGuard<'_, Option<ProfileId>>, DeliveryCoordinatorError> {
        self.fairness_cursor
            .lock()
            .map_err(|_| DeliveryCoordinatorError::Repository("fairness lock poisoned".into()))
    }
}

fn validate_enqueue(request: &ConversationDeliveryEnqueue) -> Result<(), DeliveryCoordinatorError> {
    let valid_key = |value: &str| {
        !value.is_empty()
            && value.len() <= MAX_STABLE_KEY_BYTES
            && value.bytes().all(|byte| {
                byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b':' | b'.' | b'/')
            })
    };
    if !valid_key(&request.stable_source_key)
        || !valid_key(&request.policy_snapshot_key)
        || request.source_profile_id == request.destination_profile_id
    {
        return Err(DeliveryCoordinatorError::Invalid(
            "delivery identity or stable key is invalid",
        ));
    }
    Ok(())
}

fn same_enqueue(value: &ConversationDelivery, request: &ConversationDeliveryEnqueue) -> bool {
    value.conversation_id == request.conversation_id
        && value.source_event_id == request.source_event_id
        && value.source_profile_id == request.source_profile_id
        && value.destination_profile_id == request.destination_profile_id
        && value.participant_session_id == request.participant_session_id
        && value.policy_snapshot_key == request.policy_snapshot_key
}

fn active_claim(value: &ConversationDelivery, now: UtcTimestamp) -> bool {
    value.state == DeliveryState::Claimed
        && value
            .claim
            .as_ref()
            .is_some_and(|claim| claim.expires_at > now)
}

fn is_terminal(state: DeliveryState) -> bool {
    matches!(
        state,
        DeliveryState::Published
            | DeliveryState::DeadLetter
            | DeliveryState::Cancelled
            | DeliveryState::Superseded
    )
}

fn delivery_id(stable_source_key: &str) -> DeliveryId {
    let digest = Sha256::digest(format!("conversation-delivery\0{stable_source_key}").as_bytes());
    let mut bytes = [0_u8; 16];
    bytes.copy_from_slice(&digest[..16]);
    DeliveryId::from(EntityId::from_u128(u128::from_be_bytes(bytes)))
}

fn next_revision(value: Revision) -> Result<Revision, DeliveryCoordinatorError> {
    value
        .checked_next()
        .ok_or(DeliveryCoordinatorError::RevisionOverflow)
}

fn add_millis(value: UtcTimestamp, millis: i64) -> UtcTimestamp {
    UtcTimestamp::from_unix_millis(value.unix_millis().saturating_add(millis))
}

fn safe_detail(value: String) -> Result<String, DeliveryCoordinatorError> {
    let trimmed = value.trim();
    if trimmed.is_empty() || trimmed.len() > MAX_SAFE_DETAIL_BYTES || trimmed.contains('\0') {
        return Err(DeliveryCoordinatorError::Invalid("safe detail is invalid"));
    }
    Ok(trimmed.to_owned())
}

fn repository_error(error: impl Display) -> DeliveryCoordinatorError {
    DeliveryCoordinatorError::Repository(error.to_string())
}

#[cfg(test)]
mod tests {
    use super::*;
    use keith_state_store::EmbeddedStore;

    fn profile(value: u128) -> ProfileId {
        ProfileId::from(EntityId::from_u128(value))
    }

    fn request(key: &str, destination: u128) -> ConversationDeliveryEnqueue {
        ConversationDeliveryEnqueue {
            stable_source_key: key.into(),
            conversation_id: ConversationId::from(EntityId::from_u128(10)),
            source_event_id: EventId::from(EntityId::from_u128(20 + destination)),
            source_profile_id: profile(1),
            destination_profile_id: profile(destination),
            participant_session_id: SessionId::from(EntityId::from_u128(30 + destination)),
            policy_snapshot_key: "policy:exact:1".into(),
        }
    }

    #[test]
    fn delivery_claim_recovery_is_fenced_bounded_and_at_least_once() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("state.sqlite");
        let config = DeliveryCoordinatorConfig {
            max_attempts: 2,
            lease_millis: 10,
            initial_backoff_millis: 1,
            max_backoff_millis: 2,
            ..DeliveryCoordinatorConfig::default()
        };
        let store = EmbeddedStore::open(&path, None).unwrap();
        let coordinator = ConversationDeliveryCoordinator::new(store, config).unwrap();
        let original = coordinator.enqueue(request("source:one", 2)).unwrap();
        assert_eq!(
            coordinator.enqueue(request("source:one", 2)).unwrap(),
            original
        );
        let claim = coordinator
            .claim_next(UtcTimestamp::from_unix_millis(1))
            .unwrap()
            .unwrap();
        assert_eq!(
            coordinator.processing_guarantee(),
            ProcessingGuarantee::AtLeastOnce
        );
        drop(coordinator);

        let store = EmbeddedStore::open(&path, None).unwrap();
        let recovered = ConversationDeliveryCoordinator::new(store, config).unwrap();
        let states = recovered
            .recover_expired(UtcTimestamp::from_unix_millis(12))
            .unwrap();
        assert_eq!(states[0].state, DeliveryState::Retryable);
        assert!(matches!(
            recovered.renew(&claim, UtcTimestamp::from_unix_millis(12)),
            Err(DeliveryCoordinatorError::StaleClaim)
        ));
        let second = recovered
            .claim_next(UtcTimestamp::from_unix_millis(14))
            .unwrap()
            .unwrap();
        let dead = recovered
            .retry(
                &second,
                UtcTimestamp::from_unix_millis(15),
                "provider failed permanently",
            )
            .unwrap();
        assert_eq!(dead.state, DeliveryState::DeadLetter);
        assert_eq!(dead.attempt_count, 2);
    }

    #[test]
    fn delivery_scheduler_applies_installation_profile_limits_and_round_robin_fairness() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let coordinator = ConversationDeliveryCoordinator::new(
            store,
            DeliveryCoordinatorConfig {
                max_installation_claims: 3,
                max_claims_per_profile: 1,
                ..DeliveryCoordinatorConfig::default()
            },
        )
        .unwrap();
        coordinator.enqueue(request("source:a", 2)).unwrap();
        coordinator.enqueue(request("source:b", 2)).unwrap();
        coordinator.enqueue(request("source:c", 3)).unwrap();
        let first = coordinator.claim_next(UtcTimestamp(1)).unwrap().unwrap();
        let second = coordinator.claim_next(UtcTimestamp(1)).unwrap().unwrap();
        assert_ne!(
            first.delivery.destination_profile_id,
            second.delivery.destination_profile_id
        );
        assert!(coordinator.claim_next(UtcTimestamp(1)).unwrap().is_none());
        coordinator.retry(&first, UtcTimestamp(2), "retry").unwrap();
        let third = coordinator
            .claim_next(UtcTimestamp(2_000))
            .unwrap()
            .unwrap();
        assert_eq!(third.delivery.destination_profile_id, profile(2));
    }

    #[test]
    fn targeted_supersession_names_only_the_obsolete_delivery() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let coordinator =
            ConversationDeliveryCoordinator::new(store, DeliveryCoordinatorConfig::default())
                .unwrap();
        let first = coordinator.enqueue(request("source:first", 2)).unwrap();
        let second = coordinator.enqueue(request("source:second", 3)).unwrap();
        let superseded = coordinator
            .supersede(
                &first.id,
                first.revision,
                SupersessionTarget::SourceEvent {
                    source_event_id: first.source_event_id.clone(),
                },
                EventId::from(EntityId::from_u128(900)),
                "newer exact context supersedes this source event",
                UtcTimestamp(10),
            )
            .unwrap();
        assert_eq!(superseded.state, DeliveryState::Superseded);
        assert_eq!(
            coordinator
                .repository
                .delivery(&second.id)
                .unwrap()
                .unwrap()
                .state,
            DeliveryState::Pending
        );
    }
}
