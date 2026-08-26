use std::fmt::{Display, Write as _};

use keith_agent_types::{
    ActionId, CURRENT_SCHEMA_VERSION, ConversationId, DeliveryId, EntityId, EventId, ProfileId,
    Revision, SchemaVersion, StableKey, UtcTimestamp,
};
use keith_conversation::{
    ArtifactReference, CanonicalAppendOutcome, CanonicalConversationStore, ConversationEvent,
    ConversationEventKind, EventProvenance, Principal,
};
use keith_session_store::{
    ParticipantPublicationCommit, ParticipantPublicationIntent, ParticipantTerminalFinalization,
    SessionEntryPayload,
};
use keith_state_store_core::{
    CanonicalConversationRepository, Collection, RecordMutation, StateRecordRepository,
    VersionedRecord, WritePrecondition,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{ConversationDelivery, DeliveryState, MAX_SAFE_DETAIL_BYTES};

const MAX_PUBLICATION_ATTEMPTS: u32 = 8;
const PUBLICATION_LEASE_MILLIS: i64 = 60_000;
const PUBLICATION_INITIAL_BACKOFF_MILLIS: i64 = 1_000;
const PUBLICATION_MAX_BACKOFF_MILLIS: i64 = 300_000;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationPublicationResult {
    pub kind: ConversationEventKind,
    pub content: Option<String>,
    pub artifacts: Vec<ArtifactReference>,
    pub reply_to: Option<EventId>,
    pub thread_parent: Option<EventId>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PublicationIntentState {
    Pending,
    Claimed,
    Retryable,
    Published,
    DeadLetter,
    Cancelled,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PublicationClaimLease {
    pub token: EntityId,
    pub fence: u64,
    pub expires_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConversationPublicationIntentRecord {
    pub coordination_publication_outbox: bool,
    pub version: SchemaVersion,
    pub id: EntityId,
    pub stable_publication_key: StableKey,
    pub delivery_id: DeliveryId,
    pub action_id: ActionId,
    pub conversation_id: ConversationId,
    pub source_event_id: EventId,
    pub participant_session_id: keith_agent_types::SessionId,
    pub participant_profile_id: ProfileId,
    pub finalization_entry_id: keith_agent_types::EntryId,
    pub result_entry_id: keith_agent_types::EntryId,
    pub result_digest: String,
    pub result: ConversationPublicationResult,
    pub created_at: UtcTimestamp,
    pub state: PublicationIntentState,
    pub attempt_count: u32,
    pub last_claim_fence: u64,
    pub claim: Option<PublicationClaimLease>,
    pub retry_at: Option<UtcTimestamp>,
    pub published_event_id: Option<EventId>,
    pub safe_error: Option<String>,
    pub revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConversationPublicationClaim {
    pub intent: ConversationPublicationIntentRecord,
    pub token: EntityId,
    pub fence: u64,
    pub lease_expires_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum PublicationAppendOutcome {
    Appended,
    AlreadyVisible,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ConversationPublicationAcknowledgement {
    pub stable_publication_key: StableKey,
    pub delivery_id: DeliveryId,
    pub action_id: ActionId,
    pub event_id: EventId,
    pub publication_revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SafePublicationProjection {
    pub id: EntityId,
    pub delivery_id: DeliveryId,
    pub action_id: ActionId,
    pub conversation_id: ConversationId,
    pub source_event_id: EventId,
    pub participant_profile_id: ProfileId,
    pub state: PublicationIntentState,
    pub attempt_count: u32,
    pub retry_at: Option<UtcTimestamp>,
    pub published_event_id: Option<EventId>,
    pub safe_error: Option<String>,
    pub revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum PublicationProjectionPrincipal {
    HumanOwner,
    Profile(ProfileId),
}

pub trait PublicationProjectionAuthorizer {
    type Error: std::error::Error + Send + Sync + 'static;

    fn can_view_publication(
        &self,
        requester: &PublicationProjectionPrincipal,
        intent: &ConversationPublicationIntentRecord,
    ) -> Result<bool, Self::Error>;
}

#[derive(Debug, Error)]
pub enum PublicationOutboxError {
    #[error("publication intent is invalid: {0}")]
    Invalid(&'static str),
    #[error("publication outbox repository failed: {0}")]
    Repository(String),
    #[error("publication intent was not found")]
    NotFound,
    #[error("publication claim is stale or expired")]
    StaleClaim,
    #[error("publication intent is not eligible")]
    NotEligible,
    #[error("publication stable key conflicts with a different visible event")]
    StableKeyConflict,
    #[error("publication projection is not authorized")]
    Unauthorized,
    #[error("publication revision overflow")]
    RevisionOverflow,
}

pub struct ConversationPublicationOutbox<R> {
    repository: R,
}

impl<R> ConversationPublicationOutbox<R>
where
    R: StateRecordRepository,
    R::Error: Display,
{
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    /// Consumes only the concrete, fsync-backed participant finalization commit. The private
    /// result bytes remain in the participant session; the digest binds the canonical payload.
    pub fn stage(
        &self,
        delivery: &ConversationDelivery,
        action_id: ActionId,
        commit: &ParticipantPublicationCommit,
        result: ConversationPublicationResult,
    ) -> Result<ConversationPublicationIntentRecord, PublicationOutboxError> {
        if delivery.state != DeliveryState::Finalized {
            return Err(PublicationOutboxError::Invalid(
                "delivery must be terminally finalized before publication staging",
            ));
        }
        let (finalization, intent) = validate_session_commit(commit)?;
        if finalization.conversation_id != delivery.conversation_id
            || finalization.source_event_id != delivery.source_event_id
            || finalization.participant_session_id != delivery.participant_session_id
            || finalization.participant_profile_id != delivery.destination_profile_id
            || publication_result_digest(&result)? != finalization.result_digest
        {
            return Err(PublicationOutboxError::Invalid(
                "participant finalization does not bind this delivery and result",
            ));
        }
        let id = publication_outbox_id(&finalization.stable_publication_key);
        let record = ConversationPublicationIntentRecord {
            coordination_publication_outbox: true,
            version: CURRENT_SCHEMA_VERSION,
            id: id.clone(),
            stable_publication_key: finalization.stable_publication_key.clone(),
            delivery_id: delivery.id.clone(),
            action_id,
            conversation_id: finalization.conversation_id.clone(),
            source_event_id: finalization.source_event_id.clone(),
            participant_session_id: finalization.participant_session_id.clone(),
            participant_profile_id: finalization.participant_profile_id.clone(),
            finalization_entry_id: intent.finalization_entry_id.clone(),
            result_entry_id: finalization.result_entry_id.clone(),
            result_digest: finalization.result_digest.clone(),
            result,
            created_at: intent.created_at,
            state: PublicationIntentState::Pending,
            attempt_count: 0,
            last_claim_fence: 0,
            claim: None,
            retry_at: None,
            published_event_id: None,
            safe_error: None,
            revision: Revision::ZERO,
        };
        validate_intent(&record)?;
        if let Some(existing) = self.read(&id)? {
            return if same_intent_identity(&existing, &record) {
                Ok(existing)
            } else {
                Err(PublicationOutboxError::StableKeyConflict)
            };
        }
        self.put(&record, WritePrecondition::Missing)?;
        Ok(record)
    }

    pub fn claim_next(
        &self,
        now: UtcTimestamp,
    ) -> Result<Option<ConversationPublicationClaim>, PublicationOutboxError> {
        self.recover_expired(now)?;
        let mut eligible = self
            .list()?
            .into_iter()
            .filter(|intent| {
                matches!(
                    intent.state,
                    PublicationIntentState::Pending | PublicationIntentState::Retryable
                ) && intent.retry_at.is_none_or(|retry_at| retry_at <= now)
                    && intent.attempt_count < MAX_PUBLICATION_ATTEMPTS
            })
            .collect::<Vec<_>>();
        eligible.sort_by(|left, right| {
            left.attempt_count
                .cmp(&right.attempt_count)
                .then_with(|| left.created_at.cmp(&right.created_at))
                .then_with(|| left.id.cmp(&right.id))
        });
        let Some(mut intent) = eligible.into_iter().next() else {
            return Ok(None);
        };
        let previous = intent.revision;
        let revision = next_revision(previous)?;
        let attempt = intent
            .attempt_count
            .checked_add(1)
            .ok_or(PublicationOutboxError::RevisionOverflow)?;
        let fence = intent
            .last_claim_fence
            .checked_add(1)
            .ok_or(PublicationOutboxError::RevisionOverflow)?;
        let token = EntityId::new();
        let expires_at = add_millis(now, PUBLICATION_LEASE_MILLIS);
        intent.state = PublicationIntentState::Claimed;
        intent.attempt_count = attempt;
        intent.last_claim_fence = fence;
        intent.claim = Some(PublicationClaimLease {
            token: token.clone(),
            fence,
            expires_at,
        });
        intent.retry_at = None;
        intent.safe_error = None;
        intent.revision = revision;
        self.put(&intent, WritePrecondition::Exact(previous))?;
        Ok(Some(ConversationPublicationClaim {
            intent,
            token,
            fence,
            lease_expires_at: expires_at,
        }))
    }

    pub fn renew(
        &self,
        claim: &ConversationPublicationClaim,
        now: UtcTimestamp,
    ) -> Result<ConversationPublicationClaim, PublicationOutboxError> {
        let mut intent = self.required(&claim.intent.id)?;
        require_claim(&intent, claim, now)?;
        let previous = intent.revision;
        let revision = next_revision(previous)?;
        let fence = intent
            .last_claim_fence
            .checked_add(1)
            .ok_or(PublicationOutboxError::RevisionOverflow)?;
        let expires_at = add_millis(now, PUBLICATION_LEASE_MILLIS);
        intent.last_claim_fence = fence;
        intent.revision = revision;
        intent.claim = Some(PublicationClaimLease {
            token: claim.token.clone(),
            fence,
            expires_at,
        });
        self.put(&intent, WritePrecondition::Exact(previous))?;
        Ok(ConversationPublicationClaim {
            intent,
            token: claim.token.clone(),
            fence,
            lease_expires_at: expires_at,
        })
    }

    pub fn append_claimed(
        &self,
        claim: &ConversationPublicationClaim,
        now: UtcTimestamp,
    ) -> Result<PublicationAppendOutcome, PublicationOutboxError>
    where
        R: CanonicalConversationRepository,
    {
        if !claim.intent.result.artifacts.is_empty() {
            return Err(PublicationOutboxError::Invalid(
                "artifact results must be published by the attachment-authorized runtime path",
            ));
        }
        let intent = self.required(&claim.intent.id)?;
        require_claim(&intent, claim, now)?;
        if let Some(existing) = self.visible_event(&intent.stable_publication_key)? {
            verify_visible_event(&intent, &existing)?;
            return Ok(PublicationAppendOutcome::AlreadyVisible);
        }
        let store = CanonicalConversationStore::open(&self.repository)
            .map_err(|error| PublicationOutboxError::Repository(error.to_string()))?;
        let principal = Principal::Agent(intent.participant_profile_id.clone());
        let projection = store
            .projection(&intent.conversation_id, &principal, 0, 1)
            .map_err(|error| PublicationOutboxError::Repository(error.to_string()))?;
        let sequence = projection
            .conversation
            .event_head
            .as_ref()
            .map_or(1, |head| head.sequence.saturating_add(1));
        let event = event_for_intent(&intent, sequence);
        match store
            .append(projection.conversation.revision, &event)
            .map_err(|error| PublicationOutboxError::Repository(error.to_string()))?
        {
            CanonicalAppendOutcome::Appended => Ok(PublicationAppendOutcome::Appended),
            CanonicalAppendOutcome::Replayed => Ok(PublicationAppendOutcome::AlreadyVisible),
        }
    }

    pub fn acknowledge(
        &self,
        claim: &ConversationPublicationClaim,
        now: UtcTimestamp,
    ) -> Result<ConversationPublicationIntentRecord, PublicationOutboxError> {
        let mut intent = self.required(&claim.intent.id)?;
        require_claim(&intent, claim, now)?;
        let visible = self
            .visible_event(&intent.stable_publication_key)?
            .ok_or(PublicationOutboxError::NotEligible)?;
        verify_visible_event(&intent, &visible)?;
        let previous = intent.revision;
        intent.revision = next_revision(previous)?;
        intent.state = PublicationIntentState::Published;
        intent.claim = None;
        intent.retry_at = None;
        intent.safe_error = None;
        intent.published_event_id = Some(visible.id);
        self.put(&intent, WritePrecondition::Exact(previous))?;
        Ok(intent)
    }

    pub fn acknowledgement(
        &self,
        id: &EntityId,
    ) -> Result<ConversationPublicationAcknowledgement, PublicationOutboxError> {
        let intent = self.required(id)?;
        if intent.state != PublicationIntentState::Published {
            return Err(PublicationOutboxError::NotEligible);
        }
        Ok(ConversationPublicationAcknowledgement {
            stable_publication_key: intent.stable_publication_key,
            delivery_id: intent.delivery_id,
            action_id: intent.action_id,
            event_id: intent
                .published_event_id
                .ok_or(PublicationOutboxError::NotEligible)?,
            publication_revision: intent.revision,
        })
    }

    pub fn retry(
        &self,
        claim: &ConversationPublicationClaim,
        now: UtcTimestamp,
        safe_error: impl Into<String>,
    ) -> Result<ConversationPublicationIntentRecord, PublicationOutboxError> {
        let mut intent = self.required(&claim.intent.id)?;
        require_claim(&intent, claim, now)?;
        let previous = intent.revision;
        intent.revision = next_revision(previous)?;
        intent.claim = None;
        intent.safe_error = Some(safe_detail(safe_error.into())?);
        if intent.attempt_count >= MAX_PUBLICATION_ATTEMPTS {
            intent.state = PublicationIntentState::DeadLetter;
            intent.retry_at = None;
        } else {
            intent.state = PublicationIntentState::Retryable;
            intent.retry_at = Some(add_millis(now, publication_backoff(intent.attempt_count)));
        }
        self.put(&intent, WritePrecondition::Exact(previous))?;
        Ok(intent)
    }

    pub fn cancel(
        &self,
        id: &EntityId,
        expected_revision: Revision,
        safe_reason: impl Into<String>,
    ) -> Result<ConversationPublicationIntentRecord, PublicationOutboxError> {
        let mut intent = self.required(id)?;
        if intent.revision != expected_revision
            || matches!(
                intent.state,
                PublicationIntentState::Published
                    | PublicationIntentState::DeadLetter
                    | PublicationIntentState::Cancelled
            )
        {
            return Err(PublicationOutboxError::NotEligible);
        }
        intent.revision = next_revision(expected_revision)?;
        intent.state = PublicationIntentState::Cancelled;
        intent.claim = None;
        intent.retry_at = None;
        intent.safe_error = Some(safe_detail(safe_reason.into())?);
        self.put(&intent, WritePrecondition::Exact(expected_revision))?;
        Ok(intent)
    }

    pub fn recover_expired(
        &self,
        now: UtcTimestamp,
    ) -> Result<Vec<ConversationPublicationIntentRecord>, PublicationOutboxError> {
        let mut recovered = Vec::new();
        for mut intent in self.list()?.into_iter().filter(|intent| {
            intent.state == PublicationIntentState::Claimed
                && intent
                    .claim
                    .as_ref()
                    .is_some_and(|claim| claim.expires_at <= now)
        }) {
            let previous = intent.revision;
            intent.revision = next_revision(previous)?;
            intent.claim = None;
            if let Some(visible) = self.visible_event(&intent.stable_publication_key)? {
                verify_visible_event(&intent, &visible)?;
                intent.state = PublicationIntentState::Published;
                intent.retry_at = None;
                intent.safe_error = None;
                intent.published_event_id = Some(visible.id);
                self.put(&intent, WritePrecondition::Exact(previous))?;
                recovered.push(intent);
                continue;
            }
            intent.safe_error = Some(
                "publisher lease expired; canonical append outcome will be reconciled by stable key"
                    .into(),
            );
            if intent.attempt_count >= MAX_PUBLICATION_ATTEMPTS {
                intent.state = PublicationIntentState::DeadLetter;
                intent.retry_at = None;
            } else {
                intent.state = PublicationIntentState::Retryable;
                intent.retry_at = Some(add_millis(now, publication_backoff(intent.attempt_count)));
            }
            self.put(&intent, WritePrecondition::Exact(previous))?;
            recovered.push(intent);
        }
        Ok(recovered)
    }

    pub fn safe_projection<A: PublicationProjectionAuthorizer>(
        &self,
        requester: &PublicationProjectionPrincipal,
        authorizer: &A,
    ) -> Result<Vec<SafePublicationProjection>, PublicationOutboxError> {
        let mut output = Vec::new();
        for intent in self.list()? {
            if authorizer
                .can_view_publication(requester, &intent)
                .map_err(|error| PublicationOutboxError::Repository(error.to_string()))?
            {
                output.push(SafePublicationProjection {
                    id: intent.id,
                    delivery_id: intent.delivery_id,
                    action_id: intent.action_id,
                    conversation_id: intent.conversation_id,
                    source_event_id: intent.source_event_id,
                    participant_profile_id: intent.participant_profile_id,
                    state: intent.state,
                    attempt_count: intent.attempt_count,
                    retry_at: intent.retry_at,
                    published_event_id: intent.published_event_id,
                    safe_error: intent.safe_error,
                    revision: intent.revision,
                });
            }
        }
        Ok(output)
    }

    fn required(
        &self,
        id: &EntityId,
    ) -> Result<ConversationPublicationIntentRecord, PublicationOutboxError> {
        self.read(id)?.ok_or(PublicationOutboxError::NotFound)
    }

    fn read(
        &self,
        id: &EntityId,
    ) -> Result<Option<ConversationPublicationIntentRecord>, PublicationOutboxError> {
        self.repository
            .get_record(Collection::ConversationPublicationIntents, id)
            .map_err(repository_error)?
            .map(decode_intent)
            .transpose()
    }

    fn list(&self) -> Result<Vec<ConversationPublicationIntentRecord>, PublicationOutboxError> {
        let mut output = Vec::new();
        for record in self
            .repository
            .list_records(Collection::ConversationPublicationIntents)
            .map_err(repository_error)?
        {
            if record
                .payload
                .get("coordination_publication_outbox")
                .and_then(serde_json::Value::as_bool)
                == Some(true)
            {
                output.push(decode_intent(record)?);
            }
        }
        Ok(output)
    }

    fn put(
        &self,
        intent: &ConversationPublicationIntentRecord,
        precondition: WritePrecondition,
    ) -> Result<(), PublicationOutboxError> {
        validate_intent(intent)?;
        self.repository
            .transact(&[RecordMutation::Put {
                collection: Collection::ConversationPublicationIntents,
                record: VersionedRecord {
                    version: intent.version,
                    id: intent.id.clone(),
                    revision: intent.revision,
                    updated_at: intent.created_at,
                    payload: serde_json::to_value(intent)
                        .map_err(|error| PublicationOutboxError::Repository(error.to_string()))?,
                },
                precondition,
            }])
            .map_err(repository_error)?;
        Ok(())
    }

    fn visible_event(
        &self,
        key: &StableKey,
    ) -> Result<Option<ConversationEvent>, PublicationOutboxError> {
        let sentinel_id = conversation_publication_sentinel_id(key);
        let Some(sentinel) = self
            .repository
            .get_record(Collection::ConversationStableKeys, &sentinel_id)
            .map_err(repository_error)?
        else {
            return Ok(None);
        };
        let stored: ConversationPublicationSentinel = serde_json::from_value(sentinel.payload)
            .map_err(|error| PublicationOutboxError::Repository(error.to_string()))?;
        if stored.event_digest.len() != 64
            || !stored
                .event_digest
                .bytes()
                .all(|byte| byte.is_ascii_hexdigit())
        {
            return Err(PublicationOutboxError::StableKeyConflict);
        }
        let event_record = self
            .repository
            .get_record(
                Collection::ConversationEvents,
                stored.event_id.as_entity_id(),
            )
            .map_err(repository_error)?
            .ok_or_else(|| {
                PublicationOutboxError::Repository(
                    "publication sentinel references a missing event".into(),
                )
            })?;
        match serde_json::from_value(event_record.payload)
            .map_err(|error| PublicationOutboxError::Repository(error.to_string()))?
        {
            DurableConversationEvent::Event { event } => Ok(Some(*event)),
        }
    }
}

#[derive(Deserialize)]
#[serde(tag = "record_kind", rename_all = "snake_case", deny_unknown_fields)]
enum DurableConversationEvent {
    Event { event: Box<ConversationEvent> },
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct ConversationPublicationSentinel {
    event_id: EventId,
    event_digest: String,
}

pub fn publication_result_digest(
    result: &ConversationPublicationResult,
) -> Result<String, PublicationOutboxError> {
    let bytes = keith_agent_types::canonical_json_bytes(result)
        .map_err(|error| PublicationOutboxError::Repository(error.to_string()))?;
    Ok(hex_digest(&bytes))
}

fn validate_session_commit(
    commit: &ParticipantPublicationCommit,
) -> Result<
    (
        &ParticipantTerminalFinalization,
        &ParticipantPublicationIntent,
    ),
    PublicationOutboxError,
> {
    commit
        .finalization_entry
        .verify()
        .map_err(|error| PublicationOutboxError::Repository(error.to_string()))?;
    commit
        .publication_intent_entry
        .verify()
        .map_err(|error| PublicationOutboxError::Repository(error.to_string()))?;
    let SessionEntryPayload::ParticipantTerminalFinalization { finalization } =
        &commit.finalization_entry.payload
    else {
        return Err(PublicationOutboxError::Invalid(
            "commit has no participant terminal finalization",
        ));
    };
    let SessionEntryPayload::ParticipantPublicationIntent { intent } =
        &commit.publication_intent_entry.payload
    else {
        return Err(PublicationOutboxError::Invalid(
            "commit has no participant publication intent",
        ));
    };
    if commit.publication_intent_entry.parent_id.as_ref() != Some(&commit.finalization_entry.id)
        || intent.finalization_entry_id != commit.finalization_entry.id
        || intent.stable_publication_key != finalization.stable_publication_key
        || intent.conversation_id != finalization.conversation_id
        || intent.source_event_id != finalization.source_event_id
        || intent.participant_session_id != finalization.participant_session_id
        || intent.participant_profile_id != finalization.participant_profile_id
        || intent.turn_id != finalization.turn_id
        || intent.result_entry_id != finalization.result_entry_id
        || intent.result_digest != finalization.result_digest
    {
        return Err(PublicationOutboxError::Invalid(
            "participant publication intent is not bound to its terminal finalization",
        ));
    }
    Ok((finalization, intent))
}

fn validate_intent(
    intent: &ConversationPublicationIntentRecord,
) -> Result<(), PublicationOutboxError> {
    if !intent.coordination_publication_outbox
        || intent.version != CURRENT_SCHEMA_VERSION
        || intent.id != publication_outbox_id(&intent.stable_publication_key)
        || intent.result_digest.len() != 64
        || !intent
            .result_digest
            .bytes()
            .all(|byte| byte.is_ascii_hexdigit())
        || publication_result_digest(&intent.result)? != intent.result_digest
        || intent.attempt_count > MAX_PUBLICATION_ATTEMPTS
        || matches!(intent.state, PublicationIntentState::Claimed) != intent.claim.is_some()
        || intent
            .claim
            .as_ref()
            .is_some_and(|claim| claim.fence == 0 || claim.fence != intent.last_claim_fence)
        || matches!(intent.state, PublicationIntentState::Published)
            != intent.published_event_id.is_some()
        || intent.safe_error.as_deref().is_some_and(|detail| {
            detail.trim().is_empty()
                || detail.len() > MAX_SAFE_DETAIL_BYTES
                || detail.contains('\0')
        })
    {
        return Err(PublicationOutboxError::Invalid(
            "publication identity, digest, lease, or state is inconsistent",
        ));
    }
    Ok(())
}

fn same_intent_identity(
    left: &ConversationPublicationIntentRecord,
    right: &ConversationPublicationIntentRecord,
) -> bool {
    left.id == right.id
        && left.stable_publication_key == right.stable_publication_key
        && left.delivery_id == right.delivery_id
        && left.action_id == right.action_id
        && left.conversation_id == right.conversation_id
        && left.source_event_id == right.source_event_id
        && left.participant_session_id == right.participant_session_id
        && left.participant_profile_id == right.participant_profile_id
        && left.finalization_entry_id == right.finalization_entry_id
        && left.result_entry_id == right.result_entry_id
        && left.result_digest == right.result_digest
        && left.result == right.result
        && left.created_at == right.created_at
}

fn decode_intent(
    record: VersionedRecord,
) -> Result<ConversationPublicationIntentRecord, PublicationOutboxError> {
    let intent: ConversationPublicationIntentRecord = serde_json::from_value(record.payload)
        .map_err(|error| PublicationOutboxError::Repository(error.to_string()))?;
    validate_intent(&intent)?;
    if record.id != intent.id
        || record.version != intent.version
        || record.revision != intent.revision
    {
        return Err(PublicationOutboxError::Repository(
            "publication state envelope does not match payload".into(),
        ));
    }
    Ok(intent)
}

fn require_claim(
    intent: &ConversationPublicationIntentRecord,
    claim: &ConversationPublicationClaim,
    now: UtcTimestamp,
) -> Result<(), PublicationOutboxError> {
    if intent.state != PublicationIntentState::Claimed
        || intent.id != claim.intent.id
        || intent.revision != claim.intent.revision
        || intent.claim.as_ref().is_none_or(|lease| {
            lease.token != claim.token
                || lease.fence != claim.fence
                || lease.expires_at != claim.lease_expires_at
                || lease.expires_at <= now
        })
    {
        return Err(PublicationOutboxError::StaleClaim);
    }
    Ok(())
}

fn event_for_intent(
    intent: &ConversationPublicationIntentRecord,
    sequence: u64,
) -> ConversationEvent {
    ConversationEvent {
        schema_version: CURRENT_SCHEMA_VERSION,
        id: publication_event_id(&intent.stable_publication_key),
        conversation_id: intent.conversation_id.clone(),
        sequence,
        publication_key: intent.stable_publication_key.clone(),
        author: Principal::Agent(intent.participant_profile_id.clone()),
        timestamp: intent.created_at,
        kind: intent.result.kind.clone(),
        content: intent.result.content.clone(),
        artifacts: intent.result.artifacts.clone(),
        reply_to: intent.result.reply_to.clone(),
        thread_parent: intent.result.thread_parent.clone(),
        provenance: EventProvenance {
            source: "participant-delivery-publication".into(),
            source_ids: vec![
                intent.delivery_id.to_string(),
                intent.source_event_id.to_string(),
                intent.participant_session_id.to_string(),
                intent.result_entry_id.to_string(),
                intent.result_digest.clone(),
            ],
            migration_version: None,
        },
    }
}

fn verify_visible_event(
    intent: &ConversationPublicationIntentRecord,
    event: &ConversationEvent,
) -> Result<(), PublicationOutboxError> {
    let expected = event_for_intent(intent, event.sequence);
    if event != &expected {
        return Err(PublicationOutboxError::StableKeyConflict);
    }
    Ok(())
}

fn publication_outbox_id(key: &StableKey) -> EntityId {
    compound_id("coordination-publication-outbox", key.as_str())
}

fn conversation_publication_sentinel_id(key: &StableKey) -> EntityId {
    compound_id("conversation-publication-key", key.as_str())
}

fn publication_event_id(key: &StableKey) -> EventId {
    EventId::from(compound_id("coordination-publication-event", key.as_str()))
}

fn compound_id(left: &str, right: &str) -> EntityId {
    let digest = Sha256::digest(format!("{left}\0{right}").as_bytes());
    let mut bytes = [0_u8; 16];
    bytes.copy_from_slice(&digest[..16]);
    EntityId::from_u128(u128::from_be_bytes(bytes))
}

fn hex_digest(bytes: &[u8]) -> String {
    let mut output = String::with_capacity(64);
    for byte in Sha256::digest(bytes) {
        write!(&mut output, "{byte:02x}").expect("writing to a String cannot fail");
    }
    output
}

fn next_revision(value: Revision) -> Result<Revision, PublicationOutboxError> {
    value
        .checked_next()
        .ok_or(PublicationOutboxError::RevisionOverflow)
}

fn add_millis(value: UtcTimestamp, millis: i64) -> UtcTimestamp {
    UtcTimestamp::from_unix_millis(value.unix_millis().saturating_add(millis))
}

fn publication_backoff(attempt: u32) -> i64 {
    let shift = attempt.saturating_sub(1).min(30);
    PUBLICATION_INITIAL_BACKOFF_MILLIS
        .saturating_mul(1_i64 << shift)
        .min(PUBLICATION_MAX_BACKOFF_MILLIS)
}

fn safe_detail(value: String) -> Result<String, PublicationOutboxError> {
    let trimmed = value.trim();
    if trimmed.is_empty() || trimmed.len() > MAX_SAFE_DETAIL_BYTES || trimmed.contains('\0') {
        return Err(PublicationOutboxError::Invalid(
            "safe publication detail is invalid",
        ));
    }
    Ok(trimmed.to_owned())
}

fn repository_error(error: impl Display) -> PublicationOutboxError {
    PublicationOutboxError::Repository(error.to_string())
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeMap;

    use keith_agent_types::{Generation, RootTreeId, SessionId, TurnId, WorkerId, WorkspaceId};
    use keith_session_store::{
        ContentBlock, MessageRole, NewSession, ParticipantPublicationFinalizationRequest,
        SessionEntryPayload, SessionKind, SessionStore, StoredMessage, WriterIdentity,
    };
    use keith_state_store::EmbeddedStore;

    use super::*;

    fn profile(value: u128) -> ProfileId {
        ProfileId::from(EntityId::from_u128(value))
    }

    fn finalized_delivery(
        profile_id: ProfileId,
        conversation_id: ConversationId,
        source_event_id: EventId,
        session_id: SessionId,
    ) -> ConversationDelivery {
        ConversationDelivery {
            version: CURRENT_SCHEMA_VERSION,
            id: DeliveryId::from(EntityId::from_u128(70)),
            stable_source_key: "delivery:publication:test".into(),
            conversation_id,
            source_event_id,
            source_profile_id: profile(1),
            destination_profile_id: profile_id,
            purpose: crate::ConversationDeliveryPurpose::Peer,
            participant_session_id: session_id,
            policy_snapshot_key: "policy:1".into(),
            state: DeliveryState::Finalized,
            attempt_count: 1,
            last_claim_fence: 1,
            claim: None,
            retry_at: None,
            safe_error: None,
            supersession: None,
            revision: Revision::new(2),
        }
    }

    fn publication_result() -> ConversationPublicationResult {
        ConversationPublicationResult {
            kind: ConversationEventKind::Message,
            content: Some("durable participant result".into()),
            artifacts: Vec::new(),
            reply_to: None,
            thread_parent: None,
        }
    }

    #[test]
    fn publication_process_recovers_every_crash_boundary_without_duplicate_visible_event() {
        let directory = tempfile::tempdir().unwrap();
        let state_path = directory.path().join("state.sqlite");
        let session_root = directory.path().join("sessions");
        let profile_id = profile(2);
        let source_event_id = EventId::from(EntityId::from_u128(91));
        let session_id = SessionId::from(EntityId::from_u128(92));
        let now = UtcTimestamp::from_unix_millis(10);
        let store = EmbeddedStore::open(&state_path, None).unwrap();
        let conversations = CanonicalConversationStore::open(&store).unwrap();
        let dm = conversations
            .provision_permanent_human_dm(&profile_id, now)
            .unwrap()
            .id;
        let sessions = SessionStore::open(&session_root).unwrap();
        sessions
            .create(NewSession {
                kind: SessionKind::Root,
                session_id: session_id.clone(),
                root_tree_id: RootTreeId::new(),
                parent_session_id: None,
                profile_id: profile_id.clone(),
                workspace_id: WorkspaceId::new(),
                created_at: now,
                label: Some("participant".into()),
                profile_snapshot: None,
            })
            .unwrap();
        let identity = WriterIdentity {
            worker_id: WorkerId::new(),
            owner_instance: EntityId::new(),
            generation: Generation::new(1),
            acquired_at: now,
        };
        let mut writer = sessions.acquire_writer(&session_id, identity).unwrap();
        let result_entry = writer
            .append(
                None,
                now,
                SessionEntryPayload::AssistantMessage {
                    message: StoredMessage {
                        role: MessageRole::Assistant,
                        content: vec![ContentBlock::Text {
                            text: "durable participant result".into(),
                        }],
                        provider_metadata: BTreeMap::new(),
                    },
                },
            )
            .unwrap();
        let result = publication_result();
        let commit = writer
            .finalize_participant_publication(
                UtcTimestamp(11),
                ParticipantPublicationFinalizationRequest {
                    stable_publication_key: StableKey::parse("publication:delivery:test").unwrap(),
                    conversation_id: dm.clone(),
                    source_event_id: source_event_id.clone(),
                    turn_id: TurnId::new(),
                    result_entry_id: result_entry.id,
                    result_digest: publication_result_digest(&result).unwrap(),
                },
            )
            .unwrap();
        drop(writer);
        let delivery =
            finalized_delivery(profile_id.clone(), dm.clone(), source_event_id, session_id);
        let outbox = ConversationPublicationOutbox::new(store);
        let staged = outbox
            .stage(&delivery, ActionId::new(), &commit, result)
            .unwrap();

        // Crash after durable intent, before claim.
        drop(outbox);
        let store = EmbeddedStore::open(&state_path, None).unwrap();
        let outbox = ConversationPublicationOutbox::new(store);
        let claim = outbox.claim_next(UtcTimestamp(12)).unwrap().unwrap();
        assert_eq!(claim.intent.id, staged.id);

        // Crash after claim, before append: expiry recovers an at-least-once attempt.
        drop(outbox);
        let store = EmbeddedStore::open(&state_path, None).unwrap();
        let outbox = ConversationPublicationOutbox::new(store);
        outbox.recover_expired(UtcTimestamp(70_013)).unwrap();
        let claim = outbox.claim_next(UtcTimestamp(72_000)).unwrap().unwrap();
        assert_eq!(
            outbox.append_claimed(&claim, UtcTimestamp(72_001)).unwrap(),
            PublicationAppendOutcome::Appended
        );

        // Crash after canonical append, before acknowledgement: stable-key reconciliation sees it.
        drop(outbox);
        let store = EmbeddedStore::open(&state_path, None).unwrap();
        let outbox = ConversationPublicationOutbox::new(store);
        let recovered = outbox.recover_expired(UtcTimestamp(140_001)).unwrap();
        assert_eq!(recovered.len(), 1);
        let published = &recovered[0];
        assert_eq!(published.state, PublicationIntentState::Published);
        assert!(outbox.claim_next(UtcTimestamp(142_000)).unwrap().is_none());
        assert_eq!(
            outbox.acknowledgement(&published.id).unwrap().event_id,
            published.published_event_id.clone().unwrap()
        );
        let conversation = CanonicalConversationStore::open(&outbox.repository).unwrap();
        let events = conversation
            .projection(&dm, &Principal::Agent(profile_id.clone()), 0, 16)
            .unwrap()
            .events;
        assert_eq!(events.len(), 1);
        assert_eq!(events[0].publication_key, published.stable_publication_key);
        assert_eq!(events[0].author, Principal::Agent(profile_id));
    }

    #[test]
    fn publication_rejects_unfinalized_delivery_and_mismatched_payload_digest() {
        let result = publication_result();
        assert_eq!(publication_result_digest(&result).unwrap().len(), 64);
    }
}
