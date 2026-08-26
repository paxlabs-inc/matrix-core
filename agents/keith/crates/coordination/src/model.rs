use std::collections::{BTreeMap, BTreeSet};

use keith_agent_types::{
    AssignmentId, AuditId, CURRENT_SCHEMA_VERSION, ConversationId, DeliveryId, EntityId, EventId,
    ProfileId, Revision, RoundId, SchemaVersion, SessionId, TimeZoneName, UtcTimestamp,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use keith_state_store_core::{
    Collection, RecordMutation, StateRecordRepository, VersionedRecord,
    WritePrecondition as StateWritePrecondition,
};

pub const MAX_STABLE_KEY_BYTES: usize = 160;
pub const MAX_OBJECTIVE_BYTES: usize = 16_384;
pub const MAX_SAFE_DETAIL_BYTES: usize = 2_048;
pub const MAX_PARTICIPANTS: usize = 256;
pub const MAX_DEPENDENCIES: usize = 256;
pub const MAX_ACTIVE_DELIVERIES: usize = 1_024;
pub const MAX_OWNERSHIP_HISTORY: usize = 1_024;
pub const MAX_BATCH_OPERATIONS: usize = 4_096;

macro_rules! coordination_id {
    ($name:ident) => {
        #[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
        #[serde(transparent)]
        pub struct $name(pub EntityId);

        impl $name {
            pub fn new() -> Self {
                Self(EntityId::new())
            }
        }

        impl Default for $name {
            fn default() -> Self {
                Self::new()
            }
        }
    };
}

coordination_id!(OwnershipTransferId);

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DeliveryState {
    Pending,
    Claimed,
    Finalized,
    Published,
    Retryable,
    DeadLetter,
    Cancelled,
    Superseded,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DeliveryClaim {
    pub token: EntityId,
    pub fence: u64,
    pub owner_profile_id: ProfileId,
    pub attempt: u32,
    pub revision: Revision,
    pub expires_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConversationDeliveryPurpose {
    #[default]
    Peer,
    CoordinationRound,
    Assignment,
}

impl ConversationDeliveryPurpose {
    const fn is_peer(value: &Self) -> bool {
        matches!(value, Self::Peer)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationDelivery {
    pub version: SchemaVersion,
    pub id: DeliveryId,
    pub stable_source_key: String,
    pub conversation_id: ConversationId,
    pub source_event_id: EventId,
    pub source_profile_id: ProfileId,
    pub destination_profile_id: ProfileId,
    #[serde(default, skip_serializing_if = "ConversationDeliveryPurpose::is_peer")]
    pub purpose: ConversationDeliveryPurpose,
    pub participant_session_id: SessionId,
    pub policy_snapshot_key: String,
    pub state: DeliveryState,
    pub attempt_count: u32,
    pub last_claim_fence: u64,
    pub claim: Option<DeliveryClaim>,
    pub retry_at: Option<UtcTimestamp>,
    pub safe_error: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub supersession: Option<TargetedSupersession>,
    pub revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "target", deny_unknown_fields)]
pub enum SupersessionTarget {
    Delivery {
        delivery_id: DeliveryId,
    },
    Assignment {
        assignment_id: AssignmentId,
    },
    RoundBranch {
        round_id: RoundId,
        source_event_id: EventId,
    },
    SourceEvent {
        source_event_id: EventId,
    },
    ContextRevision {
        conversation_id: ConversationId,
        revision: Revision,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TargetedSupersession {
    pub target: SupersessionTarget,
    pub superseded_by_event_id: EventId,
    pub reason: String,
    pub occurred_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MentionPolicy {
    ExplicitOnly,
    AllParticipants,
    CoordinatorSelected,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RoundState {
    Open,
    Quiet,
    Blocked,
    Converged,
    BudgetClosed,
    Cancelled,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CollaborationRound {
    pub version: SchemaVersion,
    pub id: RoundId,
    pub stable_key: String,
    pub conversation_id: ConversationId,
    pub trigger_event_id: EventId,
    pub eligible_participants: BTreeSet<ProfileId>,
    pub mention_policy: MentionPolicy,
    pub state: RoundState,
    pub max_depth: u16,
    pub remaining_depth: u16,
    pub max_turns: u32,
    pub remaining_turns: u32,
    pub active_deliveries: BTreeSet<DeliveryId>,
    pub terminal_reason: Option<String>,
    pub revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RoundStateTransition {
    pub state: RoundState,
    pub remaining_depth: u16,
    pub remaining_turns: u32,
    pub active_deliveries: BTreeSet<DeliveryId>,
    pub reason: Option<String>,
}

impl CollaborationRound {
    /// Applies one revision-fenced transition. Repeating the immediately committed transition is
    /// an idempotent replay; any other stale revision is rejected.
    pub fn transition_replay_safe(
        &self,
        expected_revision: Revision,
        transition: RoundStateTransition,
    ) -> Result<Self, CoordinationError> {
        let already_applied = self.state == transition.state
            && self.remaining_depth == transition.remaining_depth
            && self.remaining_turns == transition.remaining_turns
            && self.active_deliveries == transition.active_deliveries
            && self.terminal_reason == transition.reason;
        if already_applied
            && (expected_revision == self.revision
                || expected_revision.checked_next() == Some(self.revision))
        {
            return Ok(self.clone());
        }
        if expected_revision != self.revision {
            return Err(CoordinationError::RevisionConflict);
        }
        let mut next = self.clone();
        next.revision = expected_revision
            .checked_next()
            .ok_or(CoordinationError::Invalid("round revision overflow"))?;
        next.state = transition.state;
        next.remaining_depth = transition.remaining_depth;
        next.remaining_turns = transition.remaining_turns;
        next.active_deliveries = transition.active_deliveries;
        next.terminal_reason = transition.reason;
        validate_round(&next)?;
        validate_round_update(self, &next)?;
        Ok(next)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AssignmentState {
    Proposed,
    Ready,
    Claimed,
    Active,
    Blocked,
    Completed,
    Cancelled,
    Transferred,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AssignmentClaim {
    pub token: EntityId,
    pub claimant: ProfileId,
    pub expires_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DueMetadata {
    pub due_at: Option<UtcTimestamp>,
    pub time_zone: Option<TimeZoneName>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OwnershipTransfer {
    pub id: OwnershipTransferId,
    pub stable_key: String,
    pub from_profile_id: ProfileId,
    pub to_profile_id: ProfileId,
    pub actor_profile_id: ProfileId,
    pub expected_revision: Revision,
    pub source_event_id: EventId,
    pub occurred_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CanonicalHandoffEventIntent {
    pub stable_key: String,
    pub event_id: EventId,
    pub assignment_id: AssignmentId,
    pub conversation_id: ConversationId,
    pub from_profile_id: ProfileId,
    pub to_profile_id: ProfileId,
    pub source_event_id: EventId,
    pub ownership_revision: Revision,
    pub occurred_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AssignmentRecord {
    pub version: SchemaVersion,
    pub id: AssignmentId,
    pub stable_key: String,
    pub conversation_id: ConversationId,
    pub objective: String,
    pub owner_profile_id: ProfileId,
    pub creator_profile_id: ProfileId,
    pub dependencies: BTreeSet<AssignmentId>,
    pub state: AssignmentState,
    pub claim: Option<AssignmentClaim>,
    pub priority: i16,
    pub due: Option<DueMetadata>,
    pub source_event_id: EventId,
    pub result_event_id: Option<EventId>,
    pub block_reason: Option<String>,
    pub revision: Revision,
    pub ownership_history: Vec<OwnershipTransfer>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AssignmentTransition {
    pub state: AssignmentState,
    pub claim: Option<AssignmentClaim>,
    pub result_event_id: Option<EventId>,
    pub block_reason: Option<String>,
}

impl AssignmentRecord {
    /// Applies one revision-fenced state transition. The resulting record revision is also the
    /// externally presented assignment lease fence.
    pub fn transition_replay_safe(
        &self,
        expected_revision: Revision,
        transition: AssignmentTransition,
    ) -> Result<Self, CoordinationError> {
        let already_applied = self.state == transition.state
            && self.claim == transition.claim
            && self.result_event_id == transition.result_event_id
            && self.block_reason == transition.block_reason;
        if already_applied
            && (expected_revision == self.revision
                || expected_revision.checked_next() == Some(self.revision))
        {
            return Ok(self.clone());
        }
        if expected_revision != self.revision {
            return Err(CoordinationError::RevisionConflict);
        }
        let mut next = self.clone();
        next.revision = expected_revision
            .checked_next()
            .ok_or(CoordinationError::Invalid("assignment revision overflow"))?;
        next.state = transition.state;
        next.claim = transition.claim;
        next.result_event_id = transition.result_event_id;
        next.block_reason = transition.block_reason;
        validate_assignment(&next)?;
        validate_assignment_update(self, &next)?;
        Ok(next)
    }

    /// Atomically describes the assignment half of a typed handoff. Delivery disposition, the
    /// new-owner wake, audit intent, and canonical handoff event remain members of the repository
    /// transaction assembled by the service.
    pub fn transfer_replay_safe(
        &self,
        transfer: OwnershipTransfer,
    ) -> Result<Self, CoordinationError> {
        if self.ownership_history.last() == Some(&transfer)
            && self.owner_profile_id == transfer.to_profile_id
            && self.state == AssignmentState::Transferred
        {
            return Ok(self.clone());
        }
        if transfer.expected_revision != self.revision
            || transfer.from_profile_id != self.owner_profile_id
        {
            return Err(CoordinationError::RevisionConflict);
        }
        let mut next = self.clone();
        next.revision = self
            .revision
            .checked_next()
            .ok_or(CoordinationError::Invalid("assignment revision overflow"))?;
        next.owner_profile_id = transfer.to_profile_id.clone();
        next.state = AssignmentState::Transferred;
        next.claim = None;
        next.block_reason = None;
        next.result_event_id = None;
        next.ownership_history.push(transfer);
        validate_assignment(&next)?;
        validate_assignment_update(self, &next)?;
        Ok(next)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AuditAction {
    DeliveryRecorded,
    RoundRecorded,
    AssignmentRecorded,
    OwnershipTransferred,
    BatchCommitted,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CoordinationAuditRecord {
    pub version: SchemaVersion,
    pub id: AuditId,
    pub stable_key: String,
    pub actor_profile_id: ProfileId,
    pub conversation_id: ConversationId,
    pub action: AuditAction,
    pub subject_key: String,
    pub expected_revision: Option<Revision>,
    pub resulting_revision: Revision,
    pub occurred_at: UtcTimestamp,
    pub safe_detail: Option<String>,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub handoff_event_intent: Option<CanonicalHandoffEventIntent>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum CoordinationWrite {
    PutDelivery(ConversationDelivery, WritePrecondition),
    PutRound(CollaborationRound, WritePrecondition),
    PutAssignment(AssignmentRecord, WritePrecondition),
    AppendAudit(CoordinationAuditRecord),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum WritePrecondition {
    Missing,
    Revision(Revision),
}

#[derive(Clone, Debug, Error, Eq, PartialEq)]
pub enum CoordinationError {
    #[error("coordination record is invalid: {0}")]
    Invalid(&'static str),
    #[error("coordination record is corrupt: {0}")]
    Corrupt(String),
    #[error("stable key already exists")]
    DuplicateStableKey,
    #[error("record already exists")]
    AlreadyExists,
    #[error("record was not found")]
    NotFound,
    #[error("record revision precondition failed")]
    RevisionConflict,
    #[error("coordination batch is too large")]
    BatchTooLarge,
}

#[derive(Clone, Default)]
pub struct CoordinationRepository {
    deliveries: BTreeMap<DeliveryId, Vec<u8>>,
    rounds: BTreeMap<RoundId, Vec<u8>>,
    assignments: BTreeMap<AssignmentId, Vec<u8>>,
    audits: BTreeMap<AuditId, Vec<u8>>,
}

impl CoordinationRepository {
    /// Validates every write and swaps the complete candidate snapshot atomically.
    ///
    /// # Errors
    ///
    /// Returns an error without changing the repository when any record, constraint, or
    /// precondition in the batch is invalid.
    pub fn apply_atomic(
        &mut self,
        writes: Vec<CoordinationWrite>,
    ) -> Result<(), CoordinationError> {
        if writes.len() > MAX_BATCH_OPERATIONS {
            return Err(CoordinationError::BatchTooLarge);
        }
        let mut candidate = self.clone();
        for write in writes {
            candidate.apply(write)?;
        }
        candidate.validate_snapshot()?;
        *self = candidate;
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when stored bytes are corrupt or noncanonical.
    pub fn delivery(
        &self,
        id: &DeliveryId,
    ) -> Result<Option<ConversationDelivery>, CoordinationError> {
        decode_optional(self.deliveries.get(id), validate_delivery)
    }

    /// # Errors
    ///
    /// Returns an error when stored bytes are corrupt or noncanonical.
    pub fn round(&self, id: &RoundId) -> Result<Option<CollaborationRound>, CoordinationError> {
        decode_optional(self.rounds.get(id), validate_round)
    }

    /// # Errors
    ///
    /// Returns an error when stored bytes are corrupt or noncanonical.
    pub fn assignment(
        &self,
        id: &AssignmentId,
    ) -> Result<Option<AssignmentRecord>, CoordinationError> {
        decode_optional(self.assignments.get(id), validate_assignment)
    }

    /// Returns the complete, deterministically ordered assignment snapshot.
    ///
    /// # Errors
    ///
    /// Returns an error when the bounded coordination snapshot is exceeded or any stored
    /// assignment is corrupt or noncanonical.
    pub fn assignments(&self) -> Result<Vec<AssignmentRecord>, CoordinationError> {
        if self.assignments.len() > MAX_BATCH_OPERATIONS {
            return Err(CoordinationError::BatchTooLarge);
        }
        self.assignments
            .values()
            .map(|bytes| decode(bytes, validate_assignment))
            .collect()
    }

    /// # Errors
    ///
    /// Returns an error when any stored audit is corrupt or noncanonical.
    pub fn audits(&self) -> Result<Vec<CoordinationAuditRecord>, CoordinationError> {
        if self.audits.len() > MAX_BATCH_OPERATIONS {
            return Err(CoordinationError::BatchTooLarge);
        }
        self.audits
            .values()
            .map(|bytes| decode(bytes, validate_audit))
            .collect()
    }

    fn apply(&mut self, write: CoordinationWrite) -> Result<(), CoordinationError> {
        match write {
            CoordinationWrite::PutDelivery(value, precondition) => {
                validate_delivery(&value)?;
                if let Some(bytes) = self.deliveries.get(&value.id) {
                    validate_delivery_update(&decode(bytes, validate_delivery)?, &value)?;
                }
                put(
                    &mut self.deliveries,
                    value.id.clone(),
                    value.revision,
                    precondition,
                    &value,
                )
            }
            CoordinationWrite::PutRound(value, precondition) => {
                validate_round(&value)?;
                if let Some(bytes) = self.rounds.get(&value.id) {
                    validate_round_update(&decode(bytes, validate_round)?, &value)?;
                }
                put(
                    &mut self.rounds,
                    value.id.clone(),
                    value.revision,
                    precondition,
                    &value,
                )
            }
            CoordinationWrite::PutAssignment(value, precondition) => {
                validate_assignment(&value)?;
                if matches!(precondition, WritePrecondition::Missing)
                    && !value.ownership_history.is_empty()
                {
                    return Err(CoordinationError::Invalid(
                        "new assignment has ownership history",
                    ));
                }
                if let Some(bytes) = self.assignments.get(&value.id) {
                    validate_assignment_update(&decode(bytes, validate_assignment)?, &value)?;
                }
                put(
                    &mut self.assignments,
                    value.id.clone(),
                    value.revision,
                    precondition,
                    &value,
                )
            }
            CoordinationWrite::AppendAudit(value) => {
                validate_audit(&value)?;
                if self.audits.contains_key(&value.id) {
                    return Err(CoordinationError::AlreadyExists);
                }
                self.audits
                    .insert(value.id.clone(), canonical_bytes(&value)?);
                Ok(())
            }
        }
    }

    fn validate_snapshot(&self) -> Result<(), CoordinationError> {
        let deliveries = self
            .deliveries
            .values()
            .map(|b| decode(b, validate_delivery))
            .collect::<Result<Vec<ConversationDelivery>, _>>()?;
        let rounds = self
            .rounds
            .values()
            .map(|b| decode(b, validate_round))
            .collect::<Result<Vec<CollaborationRound>, _>>()?;
        let assignments = self
            .assignments
            .values()
            .map(|b| decode(b, validate_assignment))
            .collect::<Result<Vec<AssignmentRecord>, _>>()?;
        let audits = self
            .audits
            .values()
            .map(|b| decode(b, validate_audit))
            .collect::<Result<Vec<CoordinationAuditRecord>, _>>()?;
        unique(deliveries.iter().map(|v| v.stable_source_key.as_str()))?;
        unique(rounds.iter().map(|v| v.stable_key.as_str()))?;
        unique(assignments.iter().map(|v| v.stable_key.as_str()))?;
        unique(audits.iter().map(|v| v.stable_key.as_str()))?;
        let assignment_ids: BTreeSet<_> = assignments.iter().map(|v| v.id.clone()).collect();
        let assignments_by_id: BTreeMap<_, _> = assignments
            .iter()
            .map(|assignment| (assignment.id.clone(), assignment))
            .collect();
        for assignment in &assignments {
            if assignment.dependencies.contains(&assignment.id)
                || !assignment
                    .dependencies
                    .iter()
                    .all(|id| assignment_ids.contains(id))
            {
                return Err(CoordinationError::Invalid(
                    "assignment dependency does not resolve",
                ));
            }
        }
        validate_dependency_dag(&assignments)?;
        for assignment in &assignments {
            if matches!(
                assignment.state,
                AssignmentState::Ready | AssignmentState::Claimed | AssignmentState::Active
            ) && assignment.dependencies.iter().any(|dependency| {
                assignments_by_id
                    .get(dependency)
                    .is_none_or(|record| record.state != AssignmentState::Completed)
            }) {
                return Err(CoordinationError::Invalid(
                    "assignment became ready before dependencies completed",
                ));
            }
        }
        Ok(())
    }
}

fn put<K, V>(
    map: &mut BTreeMap<K, Vec<u8>>,
    key: K,
    revision: Revision,
    precondition: WritePrecondition,
    value: &V,
) -> Result<(), CoordinationError>
where
    K: Ord,
    V: Serialize,
{
    match (map.get(&key), precondition) {
        (None, WritePrecondition::Missing) => {}
        (Some(_), WritePrecondition::Missing) => return Err(CoordinationError::AlreadyExists),
        (None, WritePrecondition::Revision(_)) => return Err(CoordinationError::NotFound),
        (Some(bytes), WritePrecondition::Revision(expected)) => {
            let actual = record_revision(bytes)?;
            if actual != expected
                || revision
                    != expected
                        .checked_next()
                        .ok_or(CoordinationError::Invalid("revision overflow"))?
            {
                return Err(CoordinationError::RevisionConflict);
            }
        }
    }
    if matches!(precondition, WritePrecondition::Missing) && revision != Revision::ZERO {
        return Err(CoordinationError::RevisionConflict);
    }
    map.insert(key, canonical_bytes(value)?);
    Ok(())
}

fn record_revision(bytes: &[u8]) -> Result<Revision, CoordinationError> {
    let value: serde_json::Value = serde_json::from_slice(bytes).map_err(corrupt)?;
    let revision = value
        .as_object()
        .and_then(|object| object.get("revision"))
        .cloned()
        .ok_or_else(|| CoordinationError::Corrupt("record has no revision".into()))?;
    serde_json::from_value(revision).map_err(corrupt)
}

fn canonical_bytes<T: Serialize>(value: &T) -> Result<Vec<u8>, CoordinationError> {
    serde_json::to_vec(value).map_err(corrupt)
}

fn decode<T: for<'de> Deserialize<'de> + Serialize>(
    bytes: &[u8],
    validate: fn(&T) -> Result<(), CoordinationError>,
) -> Result<T, CoordinationError> {
    let value: T = serde_json::from_slice(bytes).map_err(corrupt)?;
    validate(&value)?;
    if canonical_bytes(&value)? != bytes {
        return Err(CoordinationError::Corrupt(
            "record is not canonically encoded".into(),
        ));
    }
    Ok(value)
}

fn decode_optional<T: for<'de> Deserialize<'de> + Serialize>(
    bytes: Option<&Vec<u8>>,
    validate: fn(&T) -> Result<(), CoordinationError>,
) -> Result<Option<T>, CoordinationError> {
    bytes.map(|b| decode(b, validate)).transpose()
}

fn corrupt(error: impl std::fmt::Display) -> CoordinationError {
    CoordinationError::Corrupt(error.to_string())
}

fn valid_key(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= MAX_STABLE_KEY_BYTES
        && value
            .bytes()
            .all(|b| b.is_ascii_alphanumeric() || matches!(b, b'-' | b'_' | b':' | b'.' | b'/'))
}

fn bounded(value: &str, max: usize) -> bool {
    !value.trim().is_empty() && value.len() <= max && !value.contains('\0')
}

fn current(version: SchemaVersion) -> bool {
    version == CURRENT_SCHEMA_VERSION
}

fn validate_delivery(value: &ConversationDelivery) -> Result<(), CoordinationError> {
    if !current(value.version)
        || !valid_key(&value.stable_source_key)
        || !valid_key(&value.policy_snapshot_key)
    {
        return Err(CoordinationError::Invalid("delivery version or stable key"));
    }
    if value.source_profile_id == value.destination_profile_id
        && value.purpose != ConversationDeliveryPurpose::CoordinationRound
    {
        return Err(CoordinationError::Invalid(
            "delivery source and destination must differ",
        ));
    }
    if value
        .safe_error
        .as_deref()
        .is_some_and(|v| !bounded(v, MAX_SAFE_DETAIL_BYTES))
    {
        return Err(CoordinationError::Invalid("delivery safe error"));
    }
    if value.supersession.as_ref().is_some_and(|supersession| {
        !bounded(&supersession.reason, MAX_SAFE_DETAIL_BYTES)
            || !matches!(value.state, DeliveryState::Superseded)
    }) || matches!(value.state, DeliveryState::Superseded) != value.supersession.is_some()
    {
        return Err(CoordinationError::Invalid("delivery supersession"));
    }
    if matches!(value.state, DeliveryState::Claimed) != value.claim.is_some() {
        return Err(CoordinationError::Invalid("delivery claim/state mismatch"));
    }
    if value.claim.as_ref().is_some_and(|claim| {
        claim.fence == 0
            || claim.fence != value.last_claim_fence
            || claim.owner_profile_id != value.destination_profile_id
            || claim.attempt != value.attempt_count
            || claim.revision != value.revision
    }) {
        return Err(CoordinationError::Invalid("delivery claim authority"));
    }
    if value.attempt_count == 0 && value.last_claim_fence != 0 {
        return Err(CoordinationError::Invalid(
            "unclaimed delivery has a claim fence",
        ));
    }
    Ok(())
}

fn validate_delivery_update(
    previous: &ConversationDelivery,
    next: &ConversationDelivery,
) -> Result<(), CoordinationError> {
    if previous.stable_source_key != next.stable_source_key
        || previous.conversation_id != next.conversation_id
        || previous.source_event_id != next.source_event_id
        || previous.source_profile_id != next.source_profile_id
        || previous.destination_profile_id != next.destination_profile_id
        || previous.participant_session_id != next.participant_session_id
        || previous.policy_snapshot_key != next.policy_snapshot_key
    {
        return Err(CoordinationError::Invalid("delivery identity changed"));
    }
    if next.attempt_count < previous.attempt_count
        || next.last_claim_fence < previous.last_claim_fence
    {
        return Err(CoordinationError::Invalid(
            "delivery counter moved backward",
        ));
    }
    if !valid_delivery_transition(previous.state, next.state) {
        return Err(CoordinationError::Invalid("delivery state transition"));
    }
    if let Some(next_claim) = &next.claim {
        let newly_claimed = previous
            .claim
            .as_ref()
            .is_none_or(|claim| claim.token != next_claim.token);
        if next_claim.fence <= previous.last_claim_fence {
            return Err(CoordinationError::Invalid(
                "delivery claim fence did not increase",
            ));
        }
        let expected_attempt = if newly_claimed {
            previous
                .attempt_count
                .checked_add(1)
                .ok_or(CoordinationError::Invalid("delivery attempt overflow"))?
        } else {
            previous.attempt_count
        };
        if next.attempt_count != expected_attempt {
            return Err(CoordinationError::Invalid("delivery attempt progression"));
        }
    } else if next.attempt_count != previous.attempt_count {
        return Err(CoordinationError::Invalid(
            "delivery attempt changed without a claim",
        ));
    }
    Ok(())
}

fn valid_delivery_transition(previous: DeliveryState, next: DeliveryState) -> bool {
    previous == next
        || matches!(
            (previous, next),
            (
                DeliveryState::Pending | DeliveryState::Retryable,
                DeliveryState::Claimed
            ) | (
                DeliveryState::Pending | DeliveryState::Claimed | DeliveryState::Retryable,
                DeliveryState::Cancelled | DeliveryState::Superseded | DeliveryState::DeadLetter
            ) | (
                DeliveryState::Claimed,
                DeliveryState::Finalized | DeliveryState::Retryable
            ) | (
                DeliveryState::Finalized,
                DeliveryState::Published | DeliveryState::DeadLetter
            )
        )
}

fn validate_round_update(
    previous: &CollaborationRound,
    next: &CollaborationRound,
) -> Result<(), CoordinationError> {
    if previous.stable_key != next.stable_key
        || previous.conversation_id != next.conversation_id
        || previous.trigger_event_id != next.trigger_event_id
        || previous.eligible_participants != next.eligible_participants
        || previous.mention_policy != next.mention_policy
        || previous.max_depth != next.max_depth
        || previous.max_turns != next.max_turns
    {
        return Err(CoordinationError::Invalid("round identity changed"));
    }
    if next.remaining_depth > previous.remaining_depth
        || next.remaining_turns > previous.remaining_turns
    {
        return Err(CoordinationError::Invalid("round budget increased"));
    }
    if next.revision
        != previous
            .revision
            .checked_next()
            .ok_or(CoordinationError::Invalid("round revision overflow"))?
    {
        return Err(CoordinationError::RevisionConflict);
    }
    if !valid_round_transition(previous.state, next.state) {
        return Err(CoordinationError::Invalid("round state transition"));
    }
    Ok(())
}

const fn valid_round_transition(previous: RoundState, next: RoundState) -> bool {
    matches!(
        (previous, next),
        (RoundState::Open, RoundState::Open)
            | (RoundState::Quiet, RoundState::Quiet)
            | (RoundState::Blocked, RoundState::Blocked)
            | (RoundState::Converged, RoundState::Converged)
            | (RoundState::BudgetClosed, RoundState::BudgetClosed)
            | (RoundState::Cancelled, RoundState::Cancelled)
            | (
                RoundState::Open,
                RoundState::Quiet
                    | RoundState::Blocked
                    | RoundState::Converged
                    | RoundState::BudgetClosed
                    | RoundState::Cancelled
            )
            | (
                RoundState::Quiet,
                RoundState::Open
                    | RoundState::Blocked
                    | RoundState::Converged
                    | RoundState::BudgetClosed
                    | RoundState::Cancelled
            )
            | (
                RoundState::Blocked,
                RoundState::Open
                    | RoundState::Quiet
                    | RoundState::Converged
                    | RoundState::BudgetClosed
                    | RoundState::Cancelled
            )
    )
}

fn validate_round(value: &CollaborationRound) -> Result<(), CoordinationError> {
    if !current(value.version)
        || !valid_key(&value.stable_key)
        || value.eligible_participants.is_empty()
        || value.eligible_participants.len() > MAX_PARTICIPANTS
        || value.active_deliveries.len() > MAX_ACTIVE_DELIVERIES
    {
        return Err(CoordinationError::Invalid(
            "round identity or collection bounds",
        ));
    }
    if value.max_depth == 0
        || value.remaining_depth > value.max_depth
        || value.max_turns == 0
        || value.remaining_turns > value.max_turns
    {
        return Err(CoordinationError::Invalid("round budget"));
    }
    let reason_required = matches!(
        value.state,
        RoundState::Blocked
            | RoundState::Converged
            | RoundState::BudgetClosed
            | RoundState::Cancelled
    );
    if reason_required
        != value
            .terminal_reason
            .as_deref()
            .is_some_and(|v| bounded(v, MAX_SAFE_DETAIL_BYTES))
    {
        return Err(CoordinationError::Invalid("round terminal reason"));
    }
    Ok(())
}

fn validate_assignment(value: &AssignmentRecord) -> Result<(), CoordinationError> {
    if !current(value.version)
        || !valid_key(&value.stable_key)
        || !bounded(&value.objective, MAX_OBJECTIVE_BYTES)
        || value.dependencies.len() > MAX_DEPENDENCIES
        || value.ownership_history.len() > MAX_OWNERSHIP_HISTORY
    {
        return Err(CoordinationError::Invalid("assignment identity or bounds"));
    }
    if matches!(
        value.state,
        AssignmentState::Claimed | AssignmentState::Active
    ) != value.claim.is_some()
    {
        return Err(CoordinationError::Invalid(
            "assignment claim/state mismatch",
        ));
    }
    if value
        .claim
        .as_ref()
        .is_some_and(|claim| claim.claimant != value.owner_profile_id)
    {
        return Err(CoordinationError::Invalid(
            "assignment claimant is not owner",
        ));
    }
    if matches!(value.state, AssignmentState::Blocked) != value.block_reason.is_some() {
        return Err(CoordinationError::Invalid(
            "assignment block state mismatch",
        ));
    }
    if matches!(value.state, AssignmentState::Completed) != value.result_event_id.is_some() {
        return Err(CoordinationError::Invalid(
            "assignment result/state mismatch",
        ));
    }
    if value
        .block_reason
        .as_deref()
        .is_some_and(|v| !bounded(v, MAX_SAFE_DETAIL_BYTES))
    {
        return Err(CoordinationError::Invalid("assignment block reason"));
    }
    let mut seen_keys = BTreeSet::new();
    let mut seen_ids = BTreeSet::new();
    for (index, transfer) in value.ownership_history.iter().enumerate() {
        if !valid_key(&transfer.stable_key)
            || transfer.from_profile_id == transfer.to_profile_id
            || !seen_keys.insert(transfer.stable_key.as_str())
            || !seen_ids.insert(&transfer.id)
            || index > 0
                && value.ownership_history[index - 1].to_profile_id != transfer.from_profile_id
            || transfer.expected_revision >= value.revision
            || index > 0
                && transfer.expected_revision
                    <= value.ownership_history[index - 1].expected_revision
            || index > 0 && transfer.occurred_at < value.ownership_history[index - 1].occurred_at
        {
            return Err(CoordinationError::Invalid("assignment ownership history"));
        }
    }
    if value
        .ownership_history
        .last()
        .is_some_and(|transfer| transfer.to_profile_id != value.owner_profile_id)
    {
        return Err(CoordinationError::Invalid(
            "assignment owner does not match history",
        ));
    }
    Ok(())
}

fn validate_assignment_update(
    previous: &AssignmentRecord,
    next: &AssignmentRecord,
) -> Result<(), CoordinationError> {
    if previous.stable_key != next.stable_key
        || previous.conversation_id != next.conversation_id
        || previous.creator_profile_id != next.creator_profile_id
        || previous.source_event_id != next.source_event_id
        || previous.dependencies != next.dependencies
    {
        return Err(CoordinationError::Invalid("assignment identity changed"));
    }
    if !valid_assignment_transition(previous.state, next.state) {
        return Err(CoordinationError::Invalid("assignment state transition"));
    }
    match (&previous.claim, &next.claim) {
        (Some(previous_claim), Some(next_claim)) => {
            if previous_claim.token != next_claim.token
                || previous_claim.claimant != next_claim.claimant
                || next_claim.expires_at < previous_claim.expires_at
            {
                return Err(CoordinationError::Invalid(
                    "assignment claim was replaced or shortened without release",
                ));
            }
        }
        (None, Some(_))
            if !matches!(
                previous.state,
                AssignmentState::Ready | AssignmentState::Transferred
            ) =>
        {
            return Err(CoordinationError::Invalid(
                "assignment claim did not originate from a claimable state",
            ));
        }
        _ => {}
    }
    if next.revision
        != previous
            .revision
            .checked_next()
            .ok_or(CoordinationError::Invalid("assignment revision overflow"))?
    {
        return Err(CoordinationError::RevisionConflict);
    }
    if !next
        .ownership_history
        .starts_with(&previous.ownership_history)
    {
        return Err(CoordinationError::Invalid(
            "assignment ownership history was rewritten",
        ));
    }
    let appended = &next.ownership_history[previous.ownership_history.len()..];
    if previous.owner_profile_id == next.owner_profile_id {
        if !appended.is_empty() {
            return Err(CoordinationError::Invalid(
                "ownership history changed without owner",
            ));
        }
    } else {
        if appended.len() != 1 || next.state != AssignmentState::Transferred {
            return Err(CoordinationError::Invalid(
                "assignment owner changed without one transfer",
            ));
        }
        let transfer = &appended[0];
        if transfer.from_profile_id != previous.owner_profile_id
            || transfer.to_profile_id != next.owner_profile_id
            || transfer.expected_revision != previous.revision
        {
            return Err(CoordinationError::Invalid(
                "assignment transfer precondition",
            ));
        }
    }
    Ok(())
}

fn valid_assignment_transition(previous: AssignmentState, next: AssignmentState) -> bool {
    previous == next
        || matches!(
            (previous, next),
            (
                AssignmentState::Proposed,
                AssignmentState::Ready | AssignmentState::Blocked | AssignmentState::Cancelled
            ) | (
                AssignmentState::Ready,
                AssignmentState::Claimed
                    | AssignmentState::Blocked
                    | AssignmentState::Cancelled
                    | AssignmentState::Transferred
            ) | (
                AssignmentState::Claimed,
                AssignmentState::Active
                    | AssignmentState::Ready
                    | AssignmentState::Blocked
                    | AssignmentState::Cancelled
                    | AssignmentState::Transferred
            ) | (
                AssignmentState::Active,
                AssignmentState::Blocked
                    | AssignmentState::Completed
                    | AssignmentState::Cancelled
                    | AssignmentState::Transferred
            ) | (
                AssignmentState::Blocked,
                AssignmentState::Ready | AssignmentState::Cancelled | AssignmentState::Transferred
            ) | (
                AssignmentState::Transferred,
                AssignmentState::Ready
                    | AssignmentState::Claimed
                    | AssignmentState::Blocked
                    | AssignmentState::Cancelled
            )
        )
}

fn validate_dependency_dag(assignments: &[AssignmentRecord]) -> Result<(), CoordinationError> {
    let by_id: BTreeMap<_, _> = assignments.iter().map(|value| (&value.id, value)).collect();
    let mut visiting = BTreeSet::new();
    let mut visited = BTreeSet::new();
    for assignment in assignments {
        visit_assignment(&assignment.id, &by_id, &mut visiting, &mut visited)?;
    }
    Ok(())
}

fn visit_assignment<'a>(
    id: &'a AssignmentId,
    assignments: &BTreeMap<&'a AssignmentId, &'a AssignmentRecord>,
    visiting: &mut BTreeSet<&'a AssignmentId>,
    visited: &mut BTreeSet<&'a AssignmentId>,
) -> Result<(), CoordinationError> {
    if visited.contains(id) {
        return Ok(());
    }
    if !visiting.insert(id) {
        return Err(CoordinationError::Invalid("assignment dependency cycle"));
    }
    for dependency in &assignments[id].dependencies {
        visit_assignment(dependency, assignments, visiting, visited)?;
    }
    visiting.remove(id);
    visited.insert(id);
    Ok(())
}

fn validate_audit(value: &CoordinationAuditRecord) -> Result<(), CoordinationError> {
    if !current(value.version)
        || !valid_key(&value.stable_key)
        || !valid_key(&value.subject_key)
        || value
            .safe_detail
            .as_deref()
            .is_some_and(|v| !bounded(v, MAX_SAFE_DETAIL_BYTES))
    {
        return Err(CoordinationError::Invalid("audit identity or bounds"));
    }
    if let Some(intent) = &value.handoff_event_intent
        && (value.action != AuditAction::OwnershipTransferred
            || intent.stable_key != value.stable_key
            || intent.conversation_id != value.conversation_id
            || intent.ownership_revision != value.resulting_revision
            || intent.occurred_at != value.occurred_at
            || intent.from_profile_id == intent.to_profile_id
            || !valid_key(&intent.stable_key))
    {
        return Err(CoordinationError::Invalid(
            "handoff event intent is not bound to its audit",
        ));
    }
    Ok(())
}

fn unique<'a>(values: impl Iterator<Item = &'a str>) -> Result<(), CoordinationError> {
    let mut seen = BTreeSet::new();
    if values.into_iter().all(|value| seen.insert(value)) {
        Ok(())
    } else {
        Err(CoordinationError::DuplicateStableKey)
    }
}

#[derive(Debug, Error)]
pub enum DurableCoordinationError<E: std::error::Error + Send + Sync + 'static> {
    #[error("coordination validation failed: {0}")]
    Coordination(#[from] CoordinationError),
    #[error("state repository failed: {0}")]
    Backend(E),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct StableKeyIndex {
    coordination_index: bool,
    stable_key: String,
    record_id: EntityId,
}

pub struct DurableCoordinationRepository<R> {
    repository: R,
}

impl<R: StateRecordRepository> DurableCoordinationRepository<R> {
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    /// Atomically validates and persists coordination records and their alternate-key indexes.
    ///
    /// # Errors
    /// Returns an error when stored state is corrupt, a coordination invariant fails, or the
    /// backend rejects the single atomic transaction.
    pub fn apply_atomic(
        &self,
        writes: Vec<CoordinationWrite>,
    ) -> Result<(), DurableCoordinationError<R::Error>> {
        if writes.len() > MAX_BATCH_OPERATIONS {
            return Err(CoordinationError::BatchTooLarge.into());
        }
        let mut snapshot = self.load_snapshot()?;
        snapshot.apply_atomic(writes.clone())?;
        let updated_at =
            UtcTimestamp::now().map_err(|error| CoordinationError::Corrupt(error.to_string()))?;
        let mut mutations = Vec::with_capacity(writes.len().saturating_mul(2));
        for write in writes {
            match write {
                CoordinationWrite::PutDelivery(value, precondition) => append_state_put(
                    &mut mutations,
                    Collection::ConversationDeliveries,
                    value.id.as_entity_id(),
                    value.revision,
                    value.version,
                    &value.stable_source_key,
                    precondition,
                    updated_at,
                    &value,
                )?,
                CoordinationWrite::PutRound(value, precondition) => append_state_put(
                    &mut mutations,
                    Collection::CollaborationRounds,
                    value.id.as_entity_id(),
                    value.revision,
                    value.version,
                    &value.stable_key,
                    precondition,
                    updated_at,
                    &value,
                )?,
                CoordinationWrite::PutAssignment(value, precondition) => append_state_put(
                    &mut mutations,
                    Collection::Assignments,
                    value.id.as_entity_id(),
                    value.revision,
                    value.version,
                    &value.stable_key,
                    precondition,
                    updated_at,
                    &value,
                )?,
                CoordinationWrite::AppendAudit(value) => append_state_put(
                    &mut mutations,
                    Collection::TeammateAudits,
                    value.id.as_entity_id(),
                    value.resulting_revision,
                    value.version,
                    &value.stable_key,
                    WritePrecondition::Missing,
                    updated_at,
                    &value,
                )?,
            }
        }
        self.repository
            .transact(&mutations)
            .map_err(DurableCoordinationError::Backend)?;
        Ok(())
    }

    /// # Errors
    /// Returns an error when the backend fails or stored bytes violate the delivery contract.
    pub fn delivery(
        &self,
        id: &DeliveryId,
    ) -> Result<Option<ConversationDelivery>, DurableCoordinationError<R::Error>> {
        self.read_record(
            Collection::ConversationDeliveries,
            id.as_entity_id(),
            validate_delivery,
        )
    }

    /// # Errors
    /// Returns an error when any durable delivery is malformed.
    pub fn deliveries(
        &self,
    ) -> Result<Vec<ConversationDelivery>, DurableCoordinationError<R::Error>> {
        let mut values = self
            .list_data_records(Collection::ConversationDeliveries)?
            .into_iter()
            .map(|record| decode_state_record(record, validate_delivery).map_err(Into::into))
            .collect::<Result<Vec<_>, DurableCoordinationError<R::Error>>>()?;
        values.sort_by(|left, right| left.stable_source_key.cmp(&right.stable_source_key));
        Ok(values)
    }

    /// # Errors
    /// Returns an error when the backend fails or stored bytes violate the assignment contract.
    pub fn assignment(
        &self,
        id: &AssignmentId,
    ) -> Result<Option<AssignmentRecord>, DurableCoordinationError<R::Error>> {
        self.read_record(
            Collection::Assignments,
            id.as_entity_id(),
            validate_assignment,
        )
    }

    /// # Errors
    /// Returns an error when the bounded durable snapshot is exceeded, the backend fails, or any
    /// stored assignment violates the assignment contract.
    pub fn assignments(&self) -> Result<Vec<AssignmentRecord>, DurableCoordinationError<R::Error>> {
        let records = self.list_data_records(Collection::Assignments)?;
        if records.len() > MAX_BATCH_OPERATIONS {
            return Err(CoordinationError::BatchTooLarge.into());
        }
        let mut values = records
            .into_iter()
            .map(|record| decode_state_record(record, validate_assignment).map_err(Into::into))
            .collect::<Result<Vec<_>, DurableCoordinationError<R::Error>>>()?;
        values.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        Ok(values)
    }

    /// # Errors
    /// Returns an error when the bounded durable snapshot is exceeded, the backend fails, or any
    /// stored audit violates the audit contract.
    pub fn audits(
        &self,
    ) -> Result<Vec<CoordinationAuditRecord>, DurableCoordinationError<R::Error>> {
        let records = self.coordination_audit_records()?;
        if records.len() > MAX_BATCH_OPERATIONS {
            return Err(CoordinationError::BatchTooLarge.into());
        }
        let mut values = records
            .into_iter()
            .map(|record| decode_state_record(record, validate_audit).map_err(Into::into))
            .collect::<Result<Vec<_>, DurableCoordinationError<R::Error>>>()?;
        values.sort_by(|left, right| left.stable_key.cmp(&right.stable_key));
        Ok(values)
    }

    fn read_record<T: for<'de> Deserialize<'de> + Serialize + RecordIdentity>(
        &self,
        collection: Collection,
        id: &EntityId,
        validate: fn(&T) -> Result<(), CoordinationError>,
    ) -> Result<Option<T>, DurableCoordinationError<R::Error>> {
        self.repository
            .get_record(collection, id)
            .map_err(DurableCoordinationError::Backend)?
            .map(|record| decode_state_record(record, validate).map_err(Into::into))
            .transpose()
    }

    fn load_snapshot(&self) -> Result<CoordinationRepository, DurableCoordinationError<R::Error>> {
        let mut snapshot = CoordinationRepository::default();
        for record in self.list_data_records(Collection::ConversationDeliveries)? {
            let value: ConversationDelivery = decode_state_record(record, validate_delivery)?;
            let bytes = canonical_bytes(&value)?;
            snapshot.deliveries.insert(value.id, bytes);
        }
        for record in self.list_data_records(Collection::CollaborationRounds)? {
            let value: CollaborationRound = decode_state_record(record, validate_round)?;
            let bytes = canonical_bytes(&value)?;
            snapshot.rounds.insert(value.id, bytes);
        }
        for record in self.list_data_records(Collection::Assignments)? {
            let value: AssignmentRecord = decode_state_record(record, validate_assignment)?;
            let bytes = canonical_bytes(&value)?;
            snapshot.assignments.insert(value.id, bytes);
        }
        for record in self.coordination_audit_records()? {
            let value: CoordinationAuditRecord = decode_state_record(record, validate_audit)?;
            let bytes = canonical_bytes(&value)?;
            snapshot.audits.insert(value.id, bytes);
        }
        snapshot.validate_snapshot()?;
        Ok(snapshot)
    }

    fn coordination_audit_records(
        &self,
    ) -> Result<Vec<VersionedRecord>, DurableCoordinationError<R::Error>> {
        self.list_data_records(Collection::TeammateAudits)?
            .into_iter()
            .filter_map(
                |record| match teammate_audit_payload_kind(&record.payload) {
                    Ok(TeammateAuditPayloadKind::Coordination) => Some(Ok(record)),
                    Ok(
                        TeammateAuditPayloadKind::Conversation
                        | TeammateAuditPayloadKind::CoordinationIndex,
                    ) => None,
                    Err(error) => Some(Err(error.into())),
                },
            )
            .collect()
    }

    fn list_data_records(
        &self,
        collection: Collection,
    ) -> Result<Vec<VersionedRecord>, DurableCoordinationError<R::Error>> {
        let records = self
            .repository
            .list_records(collection)
            .map_err(DurableCoordinationError::Backend)?;
        let mut data = Vec::new();
        for record in records {
            match serde_json::from_value::<StableKeyIndex>(record.payload.clone()) {
                Ok(index) if index.coordination_index => {
                    if record.id != stable_index_id(collection, &index.stable_key)
                        || record.version != CURRENT_SCHEMA_VERSION
                        || record.revision != Revision::ZERO
                    {
                        return Err(CoordinationError::Corrupt(
                            "stable-key index envelope is invalid".into(),
                        )
                        .into());
                    }
                }
                _ => data.push(record),
            }
        }
        Ok(data)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum TeammateAuditPayloadKind {
    Conversation,
    Coordination,
    CoordinationIndex,
}

fn teammate_audit_payload_kind(
    payload: &serde_json::Value,
) -> Result<TeammateAuditPayloadKind, CoordinationError> {
    let object = payload.as_object().ok_or_else(|| {
        CoordinationError::Corrupt("teammate audit payload is not an object".into())
    })?;
    let conversation = object.contains_key("schema_version")
        && object.contains_key("actor")
        && object.contains_key("correlation_key");
    let coordination = object.contains_key("version")
        && object.contains_key("stable_key")
        && object.contains_key("actor_profile_id")
        && object.contains_key("resulting_revision");
    let coordination_index = object.contains_key("coordination_index")
        && object.contains_key("record_id")
        && object.contains_key("stable_key");
    if coordination_index && !conversation && !coordination {
        return Ok(TeammateAuditPayloadKind::CoordinationIndex);
    }
    match (conversation, coordination) {
        (true, false) => Ok(TeammateAuditPayloadKind::Conversation),
        (false, true) => Ok(TeammateAuditPayloadKind::Coordination),
        _ => Err(CoordinationError::Corrupt(
            "unrecognized teammate audit payload".into(),
        )),
    }
}

trait RecordIdentity {
    fn record_id(&self) -> &EntityId;
    fn record_revision(&self) -> Revision;
    fn record_version(&self) -> SchemaVersion;
}

macro_rules! record_identity {
    ($type:ty, $revision:ident) => {
        impl RecordIdentity for $type {
            fn record_id(&self) -> &EntityId {
                self.id.as_entity_id()
            }
            fn record_revision(&self) -> Revision {
                self.$revision
            }
            fn record_version(&self) -> SchemaVersion {
                self.version
            }
        }
    };
}
record_identity!(ConversationDelivery, revision);
record_identity!(CollaborationRound, revision);
record_identity!(AssignmentRecord, revision);
record_identity!(CoordinationAuditRecord, resulting_revision);

fn decode_state_record<T: for<'de> Deserialize<'de> + Serialize + RecordIdentity>(
    record: VersionedRecord,
    validate: fn(&T) -> Result<(), CoordinationError>,
) -> Result<T, CoordinationError> {
    let value: T = serde_json::from_value(record.payload).map_err(corrupt)?;
    validate(&value)?;
    if value.record_id() != &record.id
        || value.record_revision() != record.revision
        || value.record_version() != record.version
    {
        return Err(CoordinationError::Corrupt(
            "state envelope does not match coordination payload".into(),
        ));
    }
    Ok(value)
}

#[allow(clippy::too_many_arguments)]
fn append_state_put<T: Serialize>(
    mutations: &mut Vec<RecordMutation>,
    collection: Collection,
    id: &EntityId,
    revision: Revision,
    version: SchemaVersion,
    stable_key: &str,
    precondition: WritePrecondition,
    updated_at: UtcTimestamp,
    value: &T,
) -> Result<(), CoordinationError> {
    mutations.push(RecordMutation::Put {
        collection,
        record: VersionedRecord {
            version,
            id: id.clone(),
            revision,
            updated_at,
            payload: serde_json::to_value(value).map_err(corrupt)?,
        },
        precondition: match precondition {
            WritePrecondition::Missing => StateWritePrecondition::Missing,
            WritePrecondition::Revision(value) => StateWritePrecondition::Exact(value),
        },
    });
    if matches!(precondition, WritePrecondition::Missing) {
        let index_id = stable_index_id(collection, stable_key);
        mutations.push(RecordMutation::Put {
            collection,
            record: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: index_id,
                revision: Revision::ZERO,
                updated_at,
                payload: serde_json::to_value(StableKeyIndex {
                    coordination_index: true,
                    stable_key: stable_key.into(),
                    record_id: id.clone(),
                })
                .map_err(corrupt)?,
            },
            precondition: StateWritePrecondition::Missing,
        });
    }
    Ok(())
}

fn stable_index_id(collection: Collection, stable_key: &str) -> EntityId {
    let mut digest = Sha256::new();
    digest.update(b"keith-coordination-index-v1\0");
    digest.update(collection.as_str().as_bytes());
    digest.update(b"\0");
    digest.update(stable_key.as_bytes());
    let bytes: [u8; 16] = digest.finalize()[..16].try_into().expect("SHA-256 prefix");
    EntityId::from_u128(u128::from_be_bytes(bytes))
}

#[cfg(test)]
mod tests {
    use super::*;
    use keith_state_store::EmbeddedStore;

    fn profile(value: u128) -> ProfileId {
        ProfileId::from(EntityId::from_u128(value))
    }
    fn event(value: u128) -> EventId {
        EventId::from(EntityId::from_u128(value))
    }
    fn conversation(value: u128) -> ConversationId {
        ConversationId::from(EntityId::from_u128(value))
    }

    fn delivery(key: &str) -> ConversationDelivery {
        ConversationDelivery {
            version: CURRENT_SCHEMA_VERSION,
            id: DeliveryId::from(EntityId::from_u128(10)),
            stable_source_key: key.into(),
            conversation_id: conversation(20),
            source_event_id: event(30),
            source_profile_id: profile(1),
            destination_profile_id: profile(2),
            purpose: ConversationDeliveryPurpose::Peer,
            participant_session_id: SessionId::from(EntityId::from_u128(40)),
            policy_snapshot_key: "policy:1".into(),
            state: DeliveryState::Pending,
            attempt_count: 0,
            last_claim_fence: 0,
            claim: None,
            retry_at: None,
            safe_error: None,
            supersession: None,
            revision: Revision::ZERO,
        }
    }

    fn assignment(id: u128, key: &str, dependencies: BTreeSet<AssignmentId>) -> AssignmentRecord {
        AssignmentRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: AssignmentId::from(EntityId::from_u128(id)),
            stable_key: key.into(),
            conversation_id: conversation(1),
            objective: "Review the durable result".into(),
            owner_profile_id: profile(2),
            creator_profile_id: profile(1),
            dependencies,
            state: AssignmentState::Ready,
            claim: None,
            priority: 0,
            due: None,
            source_event_id: event(2),
            result_event_id: None,
            block_reason: None,
            revision: Revision::ZERO,
            ownership_history: Vec::new(),
        }
    }

    #[test]
    fn domain_round_trips_strict_canonical_records() {
        let value = delivery("source:30:2");
        let bytes = serde_json::to_vec(&value).unwrap();
        assert_eq!(decode(&bytes, validate_delivery).unwrap(), value);
        let mut json: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        json["unknown"] = serde_json::json!(true);
        assert!(serde_json::from_value::<ConversationDelivery>(json).is_err());
    }

    #[test]
    fn domain_batch_is_atomic_and_stable_keys_are_unique() {
        let mut repository = CoordinationRepository::default();
        let first = delivery("source:30:2");
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                first.clone(),
                WritePrecondition::Missing,
            )])
            .unwrap();
        let mut duplicate = delivery("source:30:2");
        duplicate.id = DeliveryId::from(EntityId::from_u128(11));
        let valid_other = {
            let mut item = delivery("source:31:2");
            item.id = DeliveryId::from(EntityId::from_u128(12));
            item
        };
        assert_eq!(
            repository.apply_atomic(vec![
                CoordinationWrite::PutDelivery(valid_other.clone(), WritePrecondition::Missing),
                CoordinationWrite::PutDelivery(duplicate, WritePrecondition::Missing)
            ]),
            Err(CoordinationError::DuplicateStableKey)
        );
        assert!(repository.delivery(&valid_other.id).unwrap().is_none());
        assert_eq!(repository.delivery(&first.id).unwrap(), Some(first));
    }

    #[test]
    fn domain_revision_compare_and_set_rejects_stale_updates() {
        let mut repository = CoordinationRepository::default();
        let mut value = delivery("source:30:2");
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                value.clone(),
                WritePrecondition::Missing,
            )])
            .unwrap();
        value.revision = Revision::new(1);
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                value.clone(),
                WritePrecondition::Revision(Revision::ZERO),
            )])
            .unwrap();
        value.revision = Revision::new(2);
        assert_eq!(
            repository.apply_atomic(vec![CoordinationWrite::PutDelivery(
                value,
                WritePrecondition::Revision(Revision::ZERO)
            )]),
            Err(CoordinationError::RevisionConflict)
        );
    }

    #[test]
    fn domain_claim_fence_must_increase_on_renewal_or_recovery() {
        let mut repository = CoordinationRepository::default();
        let mut value = delivery("source:30:2");
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                value.clone(),
                WritePrecondition::Missing,
            )])
            .unwrap();
        value.revision = Revision::new(1);
        value.attempt_count = 1;
        value.last_claim_fence = 1;
        value.state = DeliveryState::Claimed;
        value.claim = Some(DeliveryClaim {
            token: EntityId::from_u128(50),
            fence: 1,
            owner_profile_id: profile(2),
            attempt: 1,
            revision: Revision::new(1),
            expires_at: UtcTimestamp(100),
        });
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                value.clone(),
                WritePrecondition::Revision(Revision::ZERO),
            )])
            .unwrap();
        value.revision = Revision::new(2);
        value.claim.as_mut().unwrap().revision = Revision::new(2);
        let stale = value.clone();
        assert_eq!(
            repository.apply_atomic(vec![CoordinationWrite::PutDelivery(
                stale,
                WritePrecondition::Revision(Revision::new(1)),
            )]),
            Err(CoordinationError::Invalid(
                "delivery claim fence did not increase"
            ))
        );
        value.claim.as_mut().unwrap().fence = 2;
        value.last_claim_fence = 2;
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                value,
                WritePrecondition::Revision(Revision::new(1)),
            )])
            .unwrap();
    }

    #[test]
    fn domain_rejects_unresolved_dependencies_and_invalid_ownership() {
        let assignment = assignment(
            70,
            "assignment:one",
            BTreeSet::from([AssignmentId::from(EntityId::from_u128(71))]),
        );
        let mut repository = CoordinationRepository::default();
        assert!(matches!(
            repository.apply_atomic(vec![CoordinationWrite::PutAssignment(
                assignment,
                WritePrecondition::Missing
            )]),
            Err(CoordinationError::Invalid(_))
        ));
    }

    #[test]
    fn domain_reacquired_claim_must_advance_persisted_fence_and_attempt() {
        let mut repository = CoordinationRepository::default();
        let mut value = delivery("source:reclaim");
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                value.clone(),
                WritePrecondition::Missing,
            )])
            .unwrap();
        value.revision = Revision::new(1);
        value.state = DeliveryState::Claimed;
        value.attempt_count = 1;
        value.last_claim_fence = 1;
        value.claim = Some(DeliveryClaim {
            token: EntityId::from_u128(80),
            fence: 1,
            owner_profile_id: profile(2),
            attempt: 1,
            revision: Revision::new(1),
            expires_at: UtcTimestamp(100),
        });
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                value.clone(),
                WritePrecondition::Revision(Revision::ZERO),
            )])
            .unwrap();
        value.revision = Revision::new(2);
        value.state = DeliveryState::Retryable;
        value.claim = None;
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                value.clone(),
                WritePrecondition::Revision(Revision::new(1)),
            )])
            .unwrap();
        value.revision = Revision::new(3);
        value.state = DeliveryState::Claimed;
        value.attempt_count = 2;
        value.claim = Some(DeliveryClaim {
            token: EntityId::from_u128(81),
            fence: 1,
            owner_profile_id: profile(2),
            attempt: 2,
            revision: Revision::new(3),
            expires_at: UtcTimestamp(200),
        });
        assert!(
            repository
                .apply_atomic(vec![CoordinationWrite::PutDelivery(
                    value.clone(),
                    WritePrecondition::Revision(Revision::new(2)),
                )])
                .is_err()
        );
        value.last_claim_fence = 2;
        value.claim.as_mut().unwrap().fence = 2;
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                value,
                WritePrecondition::Revision(Revision::new(2)),
            )])
            .unwrap();
    }

    #[test]
    fn domain_updates_freeze_identity_and_reject_terminal_or_counter_regression() {
        let mut repository = CoordinationRepository::default();
        let value = delivery("source:immutable");
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                value.clone(),
                WritePrecondition::Missing,
            )])
            .unwrap();
        let mut changed = value.clone();
        changed.revision = Revision::new(1);
        changed.source_event_id = event(999);
        assert!(
            repository
                .apply_atomic(vec![CoordinationWrite::PutDelivery(
                    changed,
                    WritePrecondition::Revision(Revision::ZERO),
                )])
                .is_err()
        );

        let mut terminal = value;
        terminal.revision = Revision::new(1);
        terminal.state = DeliveryState::Cancelled;
        repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                terminal.clone(),
                WritePrecondition::Revision(Revision::ZERO),
            )])
            .unwrap();
        terminal.revision = Revision::new(2);
        terminal.state = DeliveryState::Pending;
        assert!(
            repository
                .apply_atomic(vec![CoordinationWrite::PutDelivery(
                    terminal,
                    WritePrecondition::Revision(Revision::new(1)),
                )])
                .is_err()
        );
    }

    #[test]
    fn domain_rejects_dependency_cycles() {
        let first_id = AssignmentId::from(EntityId::from_u128(91));
        let second_id = AssignmentId::from(EntityId::from_u128(92));
        let first = assignment(91, "assignment:first", BTreeSet::from([second_id.clone()]));
        let second = assignment(92, "assignment:second", BTreeSet::from([first_id]));
        let mut repository = CoordinationRepository::default();
        assert_eq!(
            repository.apply_atomic(vec![
                CoordinationWrite::PutAssignment(first, WritePrecondition::Missing),
                CoordinationWrite::PutAssignment(second, WritePrecondition::Missing),
            ]),
            Err(CoordinationError::Invalid("assignment dependency cycle"))
        );
    }

    #[test]
    fn domain_ownership_history_is_append_only_and_revision_bound() {
        let mut repository = CoordinationRepository::default();
        let original = assignment(100, "assignment:transfer", BTreeSet::new());
        repository
            .apply_atomic(vec![CoordinationWrite::PutAssignment(
                original.clone(),
                WritePrecondition::Missing,
            )])
            .unwrap();
        let mut transferred = original;
        transferred.revision = Revision::new(1);
        transferred.state = AssignmentState::Transferred;
        transferred.owner_profile_id = profile(3);
        transferred.ownership_history.push(OwnershipTransfer {
            id: OwnershipTransferId(EntityId::from_u128(101)),
            stable_key: "transfer:one".into(),
            from_profile_id: profile(2),
            to_profile_id: profile(3),
            actor_profile_id: profile(1),
            expected_revision: Revision::ZERO,
            source_event_id: event(3),
            occurred_at: UtcTimestamp(100),
        });
        repository
            .apply_atomic(vec![CoordinationWrite::PutAssignment(
                transferred.clone(),
                WritePrecondition::Revision(Revision::ZERO),
            )])
            .unwrap();
        transferred.revision = Revision::new(2);
        transferred.ownership_history.clear();
        assert!(
            repository
                .apply_atomic(vec![CoordinationWrite::PutAssignment(
                    transferred,
                    WritePrecondition::Revision(Revision::new(1)),
                )])
                .is_err()
        );
    }

    #[test]
    fn domain_due_time_zone_rejects_non_iana_input() {
        let mut value =
            serde_json::to_value(assignment(110, "assignment:due", BTreeSet::new())).unwrap();
        value["due"] = serde_json::json!({"due_at": 100, "time_zone": "not a zone"});
        assert!(serde_json::from_value::<AssignmentRecord>(value).is_err());
    }

    #[test]
    fn domain_durable_repository_survives_restart_with_strict_reads() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("coordination.sqlite");
        let value = delivery("source:durable");
        let id = value.id.clone();
        {
            let store = EmbeddedStore::open(&path, None).unwrap();
            let repository = DurableCoordinationRepository::new(store);
            repository
                .apply_atomic(vec![CoordinationWrite::PutDelivery(
                    value.clone(),
                    WritePrecondition::Missing,
                )])
                .unwrap();
        }
        let store = EmbeddedStore::open(&path, None).unwrap();
        let repository = DurableCoordinationRepository::new(store);
        assert_eq!(repository.delivery(&id).unwrap(), Some(value));
    }

    #[test]
    fn domain_durable_stable_key_uniqueness_is_cross_connection_atomic() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("coordination.sqlite");
        let first_store = EmbeddedStore::open(&path, None).unwrap();
        let second_store = EmbeddedStore::open(&path, None).unwrap();
        let first_repository = DurableCoordinationRepository::new(first_store);
        let second_repository = DurableCoordinationRepository::new(second_store);
        first_repository
            .apply_atomic(vec![CoordinationWrite::PutDelivery(
                delivery("source:cross-process"),
                WritePrecondition::Missing,
            )])
            .unwrap();
        let mut duplicate = delivery("source:cross-process");
        duplicate.id = DeliveryId::from(EntityId::from_u128(500));
        assert!(
            second_repository
                .apply_atomic(vec![CoordinationWrite::PutDelivery(
                    duplicate,
                    WritePrecondition::Missing,
                )])
                .is_err()
        );
    }

    #[test]
    fn domain_rejects_noncanonical_and_corrupt_persisted_bytes() {
        let value = delivery("source:30:2");
        let pretty = serde_json::to_vec_pretty(&value).unwrap();
        assert!(matches!(
            decode(&pretty, validate_delivery),
            Err(CoordinationError::Corrupt(_))
        ));
        assert!(matches!(
            decode::<ConversationDelivery>(b"{", validate_delivery),
            Err(CoordinationError::Corrupt(_))
        ));
    }
}
