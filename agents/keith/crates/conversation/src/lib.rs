#![forbid(unsafe_code)]
#![allow(clippy::missing_errors_doc)]

pub mod context;
pub mod direct;
pub mod group;
pub mod model;
pub mod projection;
pub mod search;
pub mod store;

use std::collections::{BTreeMap, BTreeSet};
use std::sync::{RwLock, RwLockReadGuard, RwLockWriteGuard};

pub use direct::*;
pub use group::*;
use keith_agent_types::{
    AuditId, CURRENT_SCHEMA_VERSION, ConversationId, EntityId, EventId, GrantId, Revision,
    UtcTimestamp,
};
pub use keith_knowledge::{SharedKnowledgePermission, SharedKnowledgeSpace};
use keith_state_store_core::{
    CanonicalConversationAppend, CanonicalConversationRepository,
    CanonicalDirectConversationOutcome, Collection, DirectConversationRepository, RecordMutation,
    StateRecordRepository, VersionedRecord, WritePrecondition,
};
pub use model::*;
use sha2::{Digest, Sha256};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ConversationMutation {
    CreateConversation(ConversationRecord),
    UpdateConversation {
        expected_revision: Revision,
        conversation: ConversationRecord,
    },
    AddParticipant(ConversationParticipant),
    UpdateParticipant {
        expected_revision: Revision,
        participant: ConversationParticipant,
    },
    AppendEvent {
        expected_revision: Revision,
        event: ConversationEvent,
        intents: AppendIntents,
    },
    AdvanceRead {
        expected_revision: Option<Revision>,
        receipt: ReadReceipt,
    },
    PutGrant {
        expected_revision: Option<Revision>,
        grant: SharedKnowledgeGrant,
    },
    AppendAudit(ConversationAuditRecord),
}

#[allow(clippy::struct_excessive_bools)]
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AppendIntents {
    pub participant_projection: bool,
    pub unread_projection: bool,
    pub search_index: bool,
    pub publication: bool,
}

impl AppendIntents {
    pub const REQUIRED: Self = Self {
        participant_projection: true,
        unread_projection: true,
        search_index: true,
        publication: true,
    };
    fn validate(self) -> Result<(), DomainError> {
        if self.participant_projection
            && self.unread_projection
            && self.search_index
            && self.publication
        {
            Ok(())
        } else {
            Err(DomainError::Invalid(
                "canonical append requires every projection intent",
            ))
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
struct RepositoryState {
    conversations: BTreeMap<ConversationId, ConversationRecord>,
    participants: BTreeMap<(ConversationId, ParticipantPrincipal), ConversationParticipant>,
    events: BTreeMap<(ConversationId, u64), ConversationEvent>,
    publication_keys: BTreeMap<keith_agent_types::StableKey, ConversationEvent>,
    event_ids: BTreeMap<EventId, ConversationId>,
    receipts: BTreeMap<(ConversationId, Principal), ReadReceipt>,
    grants: BTreeMap<GrantId, SharedKnowledgeGrant>,
    audits: BTreeMap<AuditId, ConversationAuditRecord>,
}

#[derive(Clone, Debug, serde::Serialize, serde::Deserialize)]
#[serde(tag = "record_kind", rename_all = "snake_case", deny_unknown_fields)]
enum DurableEventRecord {
    Event {
        event: Box<ConversationEvent>,
    },
    PublicationKey {
        key: keith_agent_types::StableKey,
        event_id: EventId,
        event_digest: String,
    },
}

#[derive(Debug, Default)]
pub struct ConversationRepository {
    state: RwLock<RepositoryState>,
}

pub struct DurableConversationRepository<'a, R: StateRecordRepository> {
    store: &'a R,
    local: ConversationRepository,
}

pub struct ConversationStore<'a, R: StateRecordRepository> {
    repository: DurableConversationRepository<'a, R>,
}

pub type CanonicalConversationStore<'a, R> = ConversationStore<'a, R>;

pub struct DurableConversationAccessResolver<'a, R: StateRecordRepository> {
    store: CanonicalConversationStore<'a, R>,
}

impl<'a, R: StateRecordRepository> DurableConversationAccessResolver<'a, R> {
    pub fn open(store: &'a R) -> Result<Self, RepositoryError> {
        Ok(Self {
            store: CanonicalConversationStore::open(store)?,
        })
    }
}

impl<R: StateRecordRepository> keith_artifacts::ArtifactAccessResolver
    for DurableConversationAccessResolver<'_, R>
{
    fn authorize_conversation_actor(
        &self,
        conversation_id: &ConversationId,
        actor: &keith_artifacts::ArtifactActor,
        _operation: keith_artifacts::ArtifactOperation,
        _now: UtcTimestamp,
    ) -> Result<bool, keith_artifacts::ArtifactError> {
        let principal = match actor {
            keith_artifacts::ArtifactActor::HumanOwner => Principal::Human,
            keith_artifacts::ArtifactActor::Agent(profile) => Principal::Agent(profile.clone()),
        };
        self.store
            .is_active_participant(conversation_id, &principal)
            .map_err(|_| keith_artifacts::ArtifactError::Corrupt)
    }

    fn conversation_participants(
        &self,
        conversation_id: &ConversationId,
    ) -> Result<BTreeSet<keith_agent_types::ProfileId>, keith_artifacts::ArtifactError> {
        let state = RepositoryState::load(self.store.repository.store)
            .map_err(|_| keith_artifacts::ArtifactError::Corrupt)?;
        Ok(state
            .participants
            .iter()
            .filter_map(|((id, principal), participant)| {
                if id == conversation_id
                    && participant.left_at.is_none()
                    && let ParticipantPrincipal::Agent(profile) = principal
                {
                    Some(profile.clone())
                } else {
                    None
                }
            })
            .collect())
    }

    fn authorize_artifact_owner(
        &self,
        actor: &keith_artifacts::ArtifactActor,
        owner_profile_id: &keith_agent_types::ProfileId,
        _operation: keith_artifacts::ArtifactOperation,
        _now: UtcTimestamp,
    ) -> Result<bool, keith_artifacts::ArtifactError> {
        Ok(matches!(actor, keith_artifacts::ArtifactActor::HumanOwner)
            || matches!(actor, keith_artifacts::ArtifactActor::Agent(profile) if profile == owner_profile_id))
    }

    fn authorize_source_events(
        &self,
        conversation_id: &ConversationId,
        actor: &keith_artifacts::ArtifactActor,
        source_event_ids: &BTreeSet<EventId>,
        _now: UtcTimestamp,
    ) -> Result<bool, keith_artifacts::ArtifactError> {
        let principal = match actor {
            keith_artifacts::ArtifactActor::HumanOwner => Principal::Human,
            keith_artifacts::ArtifactActor::Agent(profile) => Principal::Agent(profile.clone()),
        };
        Ok(self
            .store
            .validate_canonical_source_events(
                conversation_id,
                &principal,
                &source_event_ids.iter().cloned().collect::<Vec<_>>(),
            )
            .is_ok())
    }

    fn authorize_grant(
        &self,
        grant_id: &GrantId,
        actor: &keith_artifacts::ArtifactActor,
        operation: keith_artifacts::ArtifactOperation,
        now: UtcTimestamp,
    ) -> Result<bool, keith_artifacts::ArtifactError> {
        let keith_artifacts::ArtifactActor::Agent(requester) = actor else {
            return Ok(false);
        };
        let operation = match operation {
            keith_artifacts::ArtifactOperation::Inspect
            | keith_artifacts::ArtifactOperation::Download => GrantOperation::Read,
            keith_artifacts::ArtifactOperation::Append => GrantOperation::Append,
        };
        self.store
            .authorize_grant(grant_id, requester, &operation, now)
            .map_err(|_| keith_artifacts::ArtifactError::Corrupt)
    }
}

impl<R: StateRecordRepository> keith_knowledge::KnowledgeAccessResolver
    for DurableConversationAccessResolver<'_, R>
{
    fn is_active_participant(
        &self,
        conversation_id: &ConversationId,
        requester: &keith_agent_types::ProfileId,
    ) -> Result<bool, keith_retrieval::RetrievalError> {
        self.store
            .is_active_participant(conversation_id, &Principal::Agent(requester.clone()))
            .map_err(|_| keith_retrieval::RetrievalError::InvalidAuthorization)
    }

    fn authorize_grant(
        &self,
        grant_id: &GrantId,
        space_id: &EntityId,
        requester: &keith_agent_types::ProfileId,
        operation: keith_knowledge::KnowledgeOperation,
        now: UtcTimestamp,
    ) -> Result<Option<keith_retrieval::ResolvedGrantAuthorization>, keith_retrieval::RetrievalError>
    {
        let operation = match operation {
            keith_knowledge::KnowledgeOperation::Search => GrantOperation::Search,
            keith_knowledge::KnowledgeOperation::Read => GrantOperation::Read,
        };
        let state = RepositoryState::load(self.store.repository.store)
            .map_err(|_| keith_retrieval::RetrievalError::InvalidAuthorization)?;
        Ok(state
            .grants
            .get(grant_id)
            .filter(|grant| {
                &grant.grantee == requester
                    && grant.resource_kind == SharedResourceKind::KnowledgeSpace
                    && grant.resource_id == space_id.to_string()
                    && grant.operations.contains(&operation)
            })
            .map(|grant| keith_retrieval::ResolvedGrantAuthorization {
                grant_id: grant.id.clone(),
                grant_revision: grant.revision,
                resource_policy_revision: grant.resource_policy_revision,
                status: if grant.revoked_at.is_some() {
                    keith_retrieval::GrantAuthorizationStatus::Revoked
                } else if grant.expires_at.is_some_and(|expires| expires < now) {
                    keith_retrieval::GrantAuthorizationStatus::Expired
                } else {
                    keith_retrieval::GrantAuthorizationStatus::Active
                },
            }))
    }

    fn authorize_space(
        &self,
        _space_id: &EntityId,
        _observed_permission_revision: Revision,
        _requester: &keith_agent_types::ProfileId,
        _operation: keith_knowledge::KnowledgeOperation,
        _now: UtcTimestamp,
    ) -> Result<Option<keith_retrieval::ResolvedSpaceAuthorization>, keith_retrieval::RetrievalError>
    {
        Ok(None)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum CanonicalAppendOutcome {
    Appended,
    Replayed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
enum TargetAuthority {
    ActiveMember,
    AuthorOrOwner,
}

#[derive(Clone, Debug, PartialEq)]
pub struct PermanentHumanDmPlan {
    pub conversation_id: ConversationId,
    pub mutations: Vec<RecordMutation>,
    pub already_committed: bool,
}

pub fn permanent_human_dm_mutations(
    profile_id: &keith_agent_types::ProfileId,
    now: UtcTimestamp,
) -> Result<Vec<RecordMutation>, RepositoryError> {
    let id = ConversationId::from(compound_id("keith-dm", &profile_id.to_string()));
    let conversation = ConversationRecord {
        schema_version: CURRENT_SCHEMA_VERSION,
        id: id.clone(),
        kind: ConversationKind::HumanAgentDm,
        lifecycle: ConversationLifecycle::Active,
        title: "Keith".into(),
        creator: Principal::System,
        created_at: now,
        updated_at: now,
        revision: Revision::ZERO,
        participant_revision: Revision::ZERO,
        participant_profiles: BTreeSet::from([profile_id.clone()]),
        human_participant: true,
        event_head: None,
    };
    let participant = |principal, role| ConversationParticipant {
        schema_version: CURRENT_SCHEMA_VERSION,
        conversation_id: id.clone(),
        principal,
        role,
        joined_at: now,
        left_at: None,
        revision: Revision::ZERO,
        applied_through_sequence: 0,
        hidden: false,
        muted: false,
        notification_policy: NotificationPolicy {
            mentions_only: false,
            muted: false,
        },
    };
    let logical = vec![
        ConversationMutation::CreateConversation(conversation),
        ConversationMutation::AddParticipant(participant(
            ParticipantPrincipal::Human,
            ParticipantRole::Owner,
        )),
        ConversationMutation::AddParticipant(participant(
            ParticipantPrincipal::Agent(profile_id.clone()),
            ParticipantRole::Member,
        )),
    ];
    let before = RepositoryState::default();
    let mut after = before.clone();
    for mutation in &logical {
        after.apply(mutation.clone())?;
    }
    after.validate_consistency()?;
    let mut mutations = durable_mutations(&before, &after, &logical)?;
    mutations.push(direct::human_dm_key_mutation(profile_id, &id, now)?);
    Ok(mutations)
}

impl<'a, R: StateRecordRepository> ConversationStore<'a, R> {
    pub fn open(store: &'a R) -> Result<Self, RepositoryError> {
        Ok(Self {
            repository: DurableConversationRepository::open(store)?,
        })
    }

    pub fn plan_permanent_human_dm(
        &self,
        profile_id: &keith_agent_types::ProfileId,
        now: UtcTimestamp,
    ) -> Result<PermanentHumanDmPlan, RepositoryError> {
        let id = ConversationId::from(compound_id("keith-dm", &profile_id.to_string()));
        let state = RepositoryState::load(self.repository.store)?;
        let keyed_conversation =
            direct::stored_human_dm_conversation(self.repository.store, profile_id)?;
        let human_key = (id.clone(), ParticipantPrincipal::Human);
        let agent_key = (id.clone(), ParticipantPrincipal::Agent(profile_id.clone()));
        match state.conversations.get(&id) {
            None => {
                if state
                    .participants
                    .keys()
                    .any(|(conversation_id, _)| conversation_id == &id)
                    || keyed_conversation.is_some()
                {
                    return Err(DomainError::Invalid(
                        "permanent human DM is partially provisioned",
                    )
                    .into());
                }
                Ok(PermanentHumanDmPlan {
                    conversation_id: id,
                    mutations: permanent_human_dm_mutations(profile_id, now)?,
                    already_committed: false,
                })
            }
            Some(conversation) => {
                let exact = conversation.kind == ConversationKind::HumanAgentDm
                    && conversation.human_participant
                    && conversation.participant_profiles == BTreeSet::from([profile_id.clone()])
                    && state
                        .participants
                        .get(&human_key)
                        .is_some_and(|participant| {
                            participant.left_at.is_none()
                                && participant.role == ParticipantRole::Owner
                        })
                    && state
                        .participants
                        .get(&agent_key)
                        .is_some_and(|participant| {
                            participant.left_at.is_none()
                                && participant.role == ParticipantRole::Member
                        })
                    && state
                        .participants
                        .keys()
                        .filter(|(conversation_id, _)| conversation_id == &id)
                        .count()
                        == 2
                    && keyed_conversation.as_ref() == Some(&id);
                if !exact {
                    return Err(DomainError::Invalid(
                        "permanent human DM durable state is mismatched",
                    )
                    .into());
                }
                Ok(PermanentHumanDmPlan {
                    conversation_id: id,
                    mutations: Vec::new(),
                    already_committed: true,
                })
            }
        }
    }

    pub fn verify_permanent_human_dm(
        &self,
        profile_id: &keith_agent_types::ProfileId,
    ) -> Result<ConversationId, RepositoryError> {
        let plan = self.plan_permanent_human_dm(profile_id, UtcTimestamp::UNIX_EPOCH)?;
        if !plan.already_committed {
            return Err(RepositoryError::NotFound("permanent human DM"));
        }
        Ok(plan.conversation_id)
    }

    fn apply_command(&self, mutations: &[ConversationMutation]) -> Result<(), RepositoryError> {
        let before = RepositoryState::load(self.repository.store)?;
        let mut after = before.clone();
        for mutation in mutations {
            after.apply(mutation.clone())?;
        }
        after.validate_consistency()?;
        let durable = durable_mutations(&before, &after, mutations)?;
        self.repository
            .store
            .transact(&durable)
            .map_err(|error| RepositoryError::Durable(error.to_string()))?;
        Ok(())
    }

    pub fn advance_read_cursor(
        &self,
        actor: &Principal,
        expected_revision: Option<Revision>,
        receipt: ReadReceipt,
    ) -> Result<(), RepositoryError> {
        if actor != &receipt.reader
            || !self.is_active_participant(&receipt.conversation_id, actor)?
        {
            return Err(DomainError::Invalid("read cursor actor is unauthorized").into());
        }
        self.apply_command(&[ConversationMutation::AdvanceRead {
            expected_revision,
            receipt,
        }])
    }

    pub fn update_participant_projection(
        &self,
        actor: &Principal,
        expected_revision: Revision,
        participant: ConversationParticipant,
    ) -> Result<(), RepositoryError> {
        let actor_principal = match actor {
            Principal::Human => ParticipantPrincipal::Human,
            Principal::Agent(profile) => ParticipantPrincipal::Agent(profile.clone()),
            Principal::System => {
                return Err(
                    DomainError::Invalid("system cannot mutate participant projection").into(),
                );
            }
        };
        let state = RepositoryState::load(self.repository.store)?;
        let actor_row = state
            .participants
            .get(&(participant.conversation_id.clone(), actor_principal));
        let self_update = participant_for(&state, &participant.conversation_id, actor)
            .is_some_and(|row| row.principal == participant.principal);
        let owner = actor_row
            .is_some_and(|row| row.left_at.is_none() && row.role == ParticipantRole::Owner);
        if !self_update && !owner {
            return Err(
                DomainError::Invalid("participant projection actor is unauthorized").into(),
            );
        }
        let current = state
            .participants
            .get(&(
                participant.conversation_id.clone(),
                participant.principal.clone(),
            ))
            .ok_or(RepositoryError::NotFound("participant"))?;
        if current.left_at != participant.left_at || current.role != participant.role {
            return Err(DomainError::Invalid(
                "projection command cannot change membership or role",
            )
            .into());
        }
        self.apply_command(&[ConversationMutation::UpdateParticipant {
            expected_revision,
            participant,
        }])
    }

    pub fn change_membership(
        &self,
        actor: &Principal,
        expected_participant_revision: Revision,
        participant: ConversationParticipant,
        expected_conversation_revision: Revision,
        event: ConversationEvent,
    ) -> Result<(), RepositoryError> {
        if event.kind != ConversationEventKind::MembershipChange
            || &event.author != actor
            || event.conversation_id != participant.conversation_id
        {
            return Err(DomainError::Invalid("membership command event is invalid").into());
        }
        let payload: MembershipEventPayload =
            serde_json::from_str(event.content.as_deref().unwrap_or_default())
                .map_err(|_| DomainError::Invalid("membership payload is malformed"))?;
        let joining = matches!(&payload, MembershipEventPayload::Join { .. });
        let target = match payload {
            MembershipEventPayload::Join { participant, .. }
            | MembershipEventPayload::Leave { participant, .. }
            | MembershipEventPayload::Rejoin { participant, .. } => participant,
        };
        if target != participant.principal {
            return Err(
                DomainError::Invalid("membership target differs from participant row").into(),
            );
        }
        let state = RepositoryState::load(self.repository.store)?;
        require_owner(&state, &participant.conversation_id, actor)?;
        let conversation = state
            .conversations
            .get(&participant.conversation_id)
            .ok_or(RepositoryError::NotFound("conversation"))?;
        if matches!(
            conversation.kind,
            ConversationKind::HumanAgentDm | ConversationKind::AgentAgentDm
        ) {
            return Err(
                DomainError::Invalid("permanent direct-message membership is immutable").into(),
            );
        }
        let event_expected_revision = if joining {
            expected_conversation_revision
                .checked_next()
                .ok_or(DomainError::Invalid("conversation revision overflow"))?
        } else {
            expected_conversation_revision
        };
        let participant_mutation = if joining {
            if state.participants.contains_key(&(
                participant.conversation_id.clone(),
                participant.principal.clone(),
            )) {
                return Err(DomainError::Invalid("membership join target already exists").into());
            }
            if expected_participant_revision != Revision::ZERO {
                return Err(DomainError::Invalid("new participant revision must be zero").into());
            }
            ConversationMutation::AddParticipant(participant)
        } else {
            ConversationMutation::UpdateParticipant {
                expected_revision: expected_participant_revision,
                participant,
            }
        };
        self.apply_command(&[
            participant_mutation,
            ConversationMutation::AppendEvent {
                expected_revision: event_expected_revision,
                event,
                intents: AppendIntents::REQUIRED,
            },
        ])
    }

    pub fn set_archived(
        &self,
        actor: &Principal,
        expected_revision: Revision,
        conversation_id: &ConversationId,
        archived: bool,
        now: UtcTimestamp,
    ) -> Result<(), RepositoryError> {
        let state = RepositoryState::load(self.repository.store)?;
        require_owner(&state, conversation_id, actor)?;
        let mut conversation = state
            .conversations
            .get(conversation_id)
            .cloned()
            .ok_or(RepositoryError::NotFound("conversation"))?;
        check_revision(conversation.revision, expected_revision)?;
        conversation.revision = expected_revision
            .checked_next()
            .ok_or(DomainError::Invalid("revision overflow"))?;
        conversation.updated_at = now;
        conversation.lifecycle = if archived {
            ConversationLifecycle::Archived
        } else {
            ConversationLifecycle::Active
        };
        self.apply_command(&[ConversationMutation::UpdateConversation {
            expected_revision,
            conversation,
        }])
    }

    pub fn put_shared_grant(
        &self,
        actor: &Principal,
        expected_revision: Option<Revision>,
        grant: SharedKnowledgeGrant,
    ) -> Result<(), RepositoryError> {
        if &grant.grantor != actor {
            return Err(DomainError::Invalid("grant actor differs from grantor").into());
        }
        if let Some(conversation) = &grant.provenance.source_conversation_id {
            let state = RepositoryState::load(self.repository.store)?;
            require_owner(&state, conversation, actor)?;
        }
        self.apply_command(&[ConversationMutation::PutGrant {
            expected_revision,
            grant,
        }])
    }

    pub fn revoke_shared_grant(
        &self,
        actor: &Principal,
        grant_id: &GrantId,
        expected_revision: Revision,
        now: UtcTimestamp,
    ) -> Result<(), RepositoryError> {
        let state = RepositoryState::load(self.repository.store)?;
        let mut grant = state
            .grants
            .get(grant_id)
            .cloned()
            .ok_or(RepositoryError::NotFound("grant"))?;
        if &grant.grantor != actor {
            return Err(DomainError::Invalid("grant actor differs from grantor").into());
        }
        check_revision(grant.revision, expected_revision)?;
        grant.revision = expected_revision
            .checked_next()
            .ok_or(DomainError::Invalid("revision overflow"))?;
        grant.revoked_at = Some(now);
        self.put_shared_grant(actor, Some(expected_revision), grant)
    }

    pub fn append(
        &self,
        expected_revision: Revision,
        event: &ConversationEvent,
    ) -> Result<CanonicalAppendOutcome, RepositoryError>
    where
        R: CanonicalConversationRepository,
    {
        if !event.artifacts.is_empty() {
            return Err(
                DomainError::Invalid("attachment append requires authoritative resolver").into(),
            );
        }
        self.append_authorized(expected_revision, event)
    }

    pub fn append_with_attachments(
        &self,
        actor: &Principal,
        expected_revision: Revision,
        event: &ConversationEvent,
        source_event_ids: &[EventId],
        resolver: &impl keith_artifacts::ConversationArtifactResolver,
    ) -> Result<CanonicalAppendOutcome, RepositoryError>
    where
        R: CanonicalConversationRepository,
    {
        if &event.author != actor {
            return Err(DomainError::Invalid("attachment actor differs from event author").into());
        }
        if source_event_ids.is_empty() {
            return Err(
                DomainError::Invalid("attachment provenance event IDs are required").into(),
            );
        }
        if !self.is_active_participant(&event.conversation_id, actor)? {
            return Err(
                DomainError::Invalid("attachment actor is not an active participant").into(),
            );
        }
        let artifact_actor = match actor {
            Principal::Human => keith_artifacts::ArtifactActor::HumanOwner,
            Principal::Agent(profile) => keith_artifacts::ArtifactActor::Agent(profile.clone()),
            Principal::System => {
                return Err(DomainError::Invalid("system cannot append attachments").into());
            }
        };
        for artifact in &event.artifacts {
            resolver
                .validate_reference(
                    &artifact_actor,
                    &event.conversation_id,
                    &artifact.artifact_id,
                    &artifact.digest_sha256,
                    source_event_ids,
                    event.timestamp,
                )
                .map_err(|error| RepositoryError::Durable(error.to_string()))?;
        }
        self.append_authorized(expected_revision, event)
    }

    fn append_authorized(
        &self,
        expected_revision: Revision,
        event: &ConversationEvent,
    ) -> Result<CanonicalAppendOutcome, RepositoryError>
    where
        R: CanonicalConversationRepository,
    {
        let state = RepositoryState::load(self.repository.store)?;
        if let Some(existing) = state.publication_keys.get(&event.publication_key) {
            return if existing == event {
                Ok(CanonicalAppendOutcome::Replayed)
            } else {
                Err(RepositoryError::Conflict("publication key"))
            };
        }
        let mut candidate = state;
        candidate.append_event(expected_revision, event.clone(), AppendIntents::REQUIRED)?;
        let head = candidate
            .conversations
            .get(&event.conversation_id)
            .ok_or(RepositoryError::NotFound("conversation"))?;
        let event_record = VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: event.id.as_entity_id().clone(),
            revision: Revision::ZERO,
            updated_at: event.timestamp,
            payload: serde_json::to_value(DurableEventRecord::Event {
                event: Box::new(event.clone()),
            })
            .map_err(|error| DomainError::Malformed(error.to_string()))?,
        };
        let stable_bytes = keith_agent_types::canonical_json_bytes(&event_record)
            .map_err(|error| DomainError::Malformed(error.to_string()))?;
        let stable_key = VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: publication_sentinel_id(&event.publication_key),
            revision: Revision::ZERO,
            updated_at: event.timestamp,
            payload: serde_json::json!({ "event_id": event.id, "event_digest": hex_sha256(&stable_bytes) }),
        };
        let mut intents = Vec::new();
        for collection in [
            Collection::ConversationProjectionIntents,
            Collection::ConversationUnreadIntents,
            Collection::ConversationSearchIntents,
            Collection::ConversationPublicationIntents,
        ] {
            put(
                &mut intents,
                collection,
                compound_id(collection.as_str(), &event.id.to_string()),
                Revision::ZERO,
                event.timestamp,
                &serde_json::json!({ "conversation_id": event.conversation_id, "event_id": event.id,
                    "sequence": event.sequence, "publication_key": event.publication_key }),
                WritePrecondition::Missing,
            )?;
        }
        let request = CanonicalConversationAppend {
            conversation_id: event.conversation_id.as_entity_id().clone(),
            expected_head_revision: expected_revision,
            expected_next_sequence: event.sequence,
            event: event_record,
            head: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: head.id.as_entity_id().clone(),
                revision: head.revision,
                updated_at: head.updated_at,
                payload: serde_json::to_value(head)
                    .map_err(|error| DomainError::Malformed(error.to_string()))?,
            },
            stable_key,
            intents,
        };
        match self
            .repository
            .store
            .append_canonical_conversation(&request)
            .map_err(|error| RepositoryError::Durable(error.to_string()))?
        {
            keith_state_store_core::CanonicalAppendOutcome::Applied { .. } => {
                Ok(CanonicalAppendOutcome::Appended)
            }
            keith_state_store_core::CanonicalAppendOutcome::Replay { .. } => {
                Ok(CanonicalAppendOutcome::Replayed)
            }
        }
    }

    pub fn append_thread_event(
        &self,
        actor: &Principal,
        expected_revision: Revision,
        event: &ConversationEvent,
    ) -> Result<CanonicalAppendOutcome, RepositoryError>
    where
        R: CanonicalConversationRepository,
    {
        if &event.author != actor || event.thread_parent.is_none() {
            return Err(DomainError::Invalid("thread event actor or parent is invalid").into());
        }
        self.append(expected_revision, event)
    }

    pub fn edit_event(
        &self,
        actor: &Principal,
        expected_revision: Revision,
        mut event: ConversationEvent,
        target: EventId,
        replacement: String,
    ) -> Result<CanonicalAppendOutcome, RepositoryError>
    where
        R: CanonicalConversationRepository,
    {
        self.authorize_target_event(actor, &event, &target, TargetAuthority::AuthorOrOwner)?;
        event.kind = ConversationEventKind::Edit;
        event.reply_to = Some(target.clone());
        event.content = Some(
            serde_json::to_string(&TargetEventPayload::Edit {
                actor: event.author.clone(),
                target,
                replacement,
            })
            .map_err(|error| DomainError::Malformed(error.to_string()))?,
        );
        self.append(expected_revision, &event)
    }

    pub fn redact_event(
        &self,
        actor: &Principal,
        expected_revision: Revision,
        mut event: ConversationEvent,
        target: EventId,
    ) -> Result<CanonicalAppendOutcome, RepositoryError>
    where
        R: CanonicalConversationRepository,
    {
        self.authorize_target_event(actor, &event, &target, TargetAuthority::AuthorOrOwner)?;
        event.kind = ConversationEventKind::Redaction;
        event.reply_to = Some(target.clone());
        event.content = Some(
            serde_json::to_string(&TargetEventPayload::Redact {
                actor: event.author.clone(),
                target,
            })
            .map_err(|error| DomainError::Malformed(error.to_string()))?,
        );
        self.append(expected_revision, &event)
    }

    pub fn react_to_event(
        &self,
        actor: &Principal,
        expected_revision: Revision,
        mut event: ConversationEvent,
        target: EventId,
        reaction: String,
        remove: bool,
    ) -> Result<CanonicalAppendOutcome, RepositoryError>
    where
        R: CanonicalConversationRepository,
    {
        self.authorize_target_event(actor, &event, &target, TargetAuthority::ActiveMember)?;
        event.kind = ConversationEventKind::Reaction;
        event.reply_to = Some(target.clone());
        event.content = Some(
            serde_json::to_string(&TargetEventPayload::React {
                actor: event.author.clone(),
                target,
                reaction,
                remove,
            })
            .map_err(|error| DomainError::Malformed(error.to_string()))?,
        );
        self.append(expected_revision, &event)
    }

    pub fn set_event_pinned(
        &self,
        actor: &Principal,
        expected_revision: Revision,
        mut event: ConversationEvent,
        target: EventId,
        pinned: bool,
    ) -> Result<CanonicalAppendOutcome, RepositoryError>
    where
        R: CanonicalConversationRepository,
    {
        self.authorize_target_event(actor, &event, &target, TargetAuthority::AuthorOrOwner)?;
        event.kind = ConversationEventKind::Pin;
        event.reply_to = Some(target.clone());
        event.content = Some(
            serde_json::to_string(&TargetEventPayload::Pin {
                actor: event.author.clone(),
                target,
                pinned,
            })
            .map_err(|error| DomainError::Malformed(error.to_string()))?,
        );
        self.append(expected_revision, &event)
    }

    fn authorize_target_event(
        &self,
        actor: &Principal,
        command_event: &ConversationEvent,
        target_id: &EventId,
        authority: TargetAuthority,
    ) -> Result<(), RepositoryError> {
        if &command_event.author != actor {
            return Err(
                DomainError::Invalid("target command actor differs from event author").into(),
            );
        }
        let state = RepositoryState::load(self.repository.store)?;
        let participant = participant_for(&state, &command_event.conversation_id, actor)
            .ok_or(RepositoryError::NotFound("participant"))?;
        if participant.left_at.is_some() || participant.role == ParticipantRole::Observer {
            return Err(
                DomainError::Invalid("target command actor is not an active member").into(),
            );
        }
        let target = state
            .event_ids
            .get(target_id)
            .filter(|conversation| *conversation == &command_event.conversation_id)
            .and_then(|_| state.events.values().find(|event| &event.id == target_id))
            .ok_or(RepositoryError::NotFound("target event"))?;
        if target.kind != ConversationEventKind::Message {
            return Err(DomainError::Invalid("target command requires a message event").into());
        }
        let projection = reduce_events(
            state
                .events
                .range(
                    (command_event.conversation_id.clone(), 0)
                        ..=(command_event.conversation_id.clone(), u64::MAX),
                )
                .map(|(_, event)| event),
        )?;
        if !projection.effective_content.contains_key(target_id) {
            return Err(DomainError::Invalid("target event is not currently materialized").into());
        }
        let owns_target = &target.author == actor;
        let owns_conversation = participant.role == ParticipantRole::Owner;
        if authority == TargetAuthority::AuthorOrOwner && !owns_target && !owns_conversation {
            return Err(DomainError::Invalid(
                "target command actor lacks author or owner authority",
            )
            .into());
        }
        Ok(())
    }

    pub fn projection(
        &self,
        id: &ConversationId,
        principal: &Principal,
        after: u64,
        limit: usize,
    ) -> Result<ConversationProjection, RepositoryError> {
        if limit == 0 || limit > 1_000 {
            return Err(DomainError::BoundExceeded("conversation page").into());
        }
        let state = RepositoryState::load(self.repository.store)?;
        ensure_visible(&state, id, principal)?;
        let conversation = state
            .conversations
            .get(id)
            .cloned()
            .ok_or(RepositoryError::NotFound("conversation"))?;
        let participants = state
            .participants
            .range((id.clone(), ParticipantPrincipal::Human)..)
            .take_while(|((conversation_id, _), _)| conversation_id == id)
            .map(|(_, value)| value.clone())
            .collect::<Vec<_>>();
        let events = state
            .events
            .range((id.clone(), after.saturating_add(1))..=(id.clone(), u64::MAX))
            .take(limit)
            .map(|(_, value)| value.clone())
            .collect::<Vec<_>>();
        let read_through_sequence = state
            .receipts
            .get(&(id.clone(), principal.clone()))
            .map_or(0, |receipt| receipt.read_through_sequence);
        let head = conversation
            .event_head
            .as_ref()
            .map_or(0, |head| head.sequence);
        let participant = participant_for(&state, id, principal);
        let materialized = reduce_events(
            state
                .events
                .range((id.clone(), 0)..=(id.clone(), u64::MAX))
                .map(|(_, event)| event),
        )?;
        let pinned = !materialized.pinned.is_empty();
        Ok(ConversationProjection {
            archived: conversation.lifecycle == ConversationLifecycle::Archived,
            hidden: participant.is_some_and(|value| value.hidden),
            conversation,
            participants,
            events,
            read_through_sequence,
            unread_count: head.saturating_sub(read_through_sequence),
            pinned,
            materialized,
        })
    }

    pub fn reconstruct_context(
        &self,
        principal: &Principal,
        cursor: ConversationContextCursor,
        limit: usize,
    ) -> Result<ConversationContext, RepositoryError> {
        let projection = self.projection(
            &cursor.conversation_id,
            principal,
            cursor.applied_through_sequence,
            limit,
        )?;
        let applied = projection
            .events
            .last()
            .map_or(cursor.applied_through_sequence, |event| event.sequence);
        Ok(ConversationContext {
            cursor: ConversationContextCursor {
                conversation_id: cursor.conversation_id,
                applied_through_sequence: applied,
            },
            visible_events: projection.events,
        })
    }

    pub fn search(
        &self,
        principal: &Principal,
        query: &str,
        limit: usize,
    ) -> Result<Vec<ConversationSearchHit>, RepositoryError> {
        if query.trim().is_empty() || query.len() > MAX_KEY_BYTES || limit == 0 || limit > 1_000 {
            return Err(DomainError::Invalid("search query or limit is invalid").into());
        }
        let needle = query.to_lowercase();
        let state = RepositoryState::load(self.repository.store)?;
        let mut hits = Vec::new();
        let mut materialized = BTreeMap::new();
        for conversation in state.conversations.keys() {
            if ensure_visible(&state, conversation, principal).is_ok() {
                materialized.insert(
                    conversation.clone(),
                    reduce_events(
                        state
                            .events
                            .range((conversation.clone(), 0)..=(conversation.clone(), u64::MAX))
                            .map(|(_, event)| event),
                    )?,
                );
            }
        }
        for event in state.events.values() {
            if hits.len() == limit {
                break;
            }
            if let Some(content) = materialized
                .get(&event.conversation_id)
                .and_then(|projection| projection.effective_content.get(&event.id))
                && content.to_lowercase().contains(&needle)
            {
                hits.push(ConversationSearchHit {
                    conversation_id: event.conversation_id.clone(),
                    event_id: event.id.clone(),
                    sequence: event.sequence,
                    author: event.author.clone(),
                    timestamp: event.timestamp,
                    content: content.clone(),
                });
            }
        }
        Ok(hits)
    }

    pub fn is_active_participant(
        &self,
        conversation_id: &ConversationId,
        principal: &Principal,
    ) -> Result<bool, RepositoryError> {
        let state = RepositoryState::load(self.repository.store)?;
        Ok(matches!(principal, Principal::System)
            || participant_for(&state, conversation_id, principal)
                .is_some_and(|participant| participant.left_at.is_none()))
    }

    pub fn authorization_observation(
        &self,
        conversation_id: &ConversationId,
        principal: &Principal,
    ) -> Result<ConversationAuthorizationObservation, RepositoryError> {
        let state = RepositoryState::load(self.repository.store)?;
        ensure_visible(&state, conversation_id, principal)?;
        let conversation = state
            .conversations
            .get(conversation_id)
            .ok_or(RepositoryError::NotFound("conversation"))?;
        let participant_revision = participant_for(&state, conversation_id, principal)
            .map_or(Revision::ZERO, |participant| participant.revision);
        let relevant_grant_revisions = match principal {
            Principal::Agent(profile) => state
                .grants
                .values()
                .filter(|grant| {
                    &grant.grantee == profile
                        && grant.provenance.source_conversation_id.as_ref() == Some(conversation_id)
                        && grant.revoked_at.is_none()
                })
                .map(|grant| (grant.id.clone(), grant.revision))
                .collect(),
            Principal::Human | Principal::System => BTreeMap::new(),
        };
        let grant_evidence = match principal {
            Principal::Agent(profile) => state
                .grants
                .values()
                .filter(|grant| {
                    &grant.grantee == profile
                        && grant.provenance.source_conversation_id.as_ref() == Some(conversation_id)
                })
                .map(|grant| {
                    (
                        grant.id.clone(),
                        GrantAuthorizationEvidence {
                            revision: grant.revision,
                            resource_policy_revision: grant.resource_policy_revision,
                            operations: grant.operations.clone(),
                            expires_at: grant.expires_at,
                            revoked_at: grant.revoked_at,
                        },
                    )
                })
                .collect(),
            Principal::Human | Principal::System => BTreeMap::new(),
        };
        let digest_bytes = keith_agent_types::canonical_json_bytes(&(
            conversation_id,
            principal,
            conversation.revision,
            participant_revision,
            &relevant_grant_revisions,
            &grant_evidence,
        ))
        .map_err(|error| DomainError::Malformed(error.to_string()))?;
        Ok(ConversationAuthorizationObservation {
            conversation_id: conversation_id.clone(),
            principal: principal.clone(),
            conversation_revision: conversation.revision,
            participant_revision,
            relevant_grant_revisions,
            grant_evidence,
            policy_digest_sha256: hex_sha256(&digest_bytes),
        })
    }

    pub fn validate_canonical_source_events(
        &self,
        conversation_id: &ConversationId,
        actor: &Principal,
        source_event_ids: &[EventId],
    ) -> Result<(), RepositoryError> {
        if source_event_ids.is_empty() || source_event_ids.len() > MAX_PROVENANCE_ITEMS {
            return Err(DomainError::Invalid("canonical source event set is invalid").into());
        }
        let state = RepositoryState::load(self.repository.store)?;
        ensure_visible(&state, conversation_id, actor)?;
        let mut unique = BTreeSet::new();
        for event_id in source_event_ids {
            if !unique.insert(event_id.clone())
                || state.event_ids.get(event_id) != Some(conversation_id)
            {
                return Err(DomainError::Invalid(
                    "canonical source event is missing or duplicated",
                )
                .into());
            }
        }
        Ok(())
    }

    pub fn authorize_grant(
        &self,
        grant_id: &GrantId,
        requester: &keith_agent_types::ProfileId,
        operation: &GrantOperation,
        now: UtcTimestamp,
    ) -> Result<bool, RepositoryError> {
        let state = RepositoryState::load(self.repository.store)?;
        Ok(state.grants.get(grant_id).is_some_and(|grant| {
            &grant.grantee == requester
                && grant.operations.contains(operation)
                && grant.revoked_at.is_none()
                && grant.expires_at.is_none_or(|expires| expires >= now)
        }))
    }

    pub fn rebuild_projections(&self) -> Result<Vec<ConversationProjection>, RepositoryError> {
        let state = RepositoryState::load(self.repository.store)?;
        let mut projections = Vec::new();
        for conversation in state.conversations.values() {
            let principal = if conversation.human_participant {
                Principal::Human
            } else if let Some(profile) = conversation.participant_profiles.first() {
                Principal::Agent(profile.clone())
            } else {
                continue;
            };
            projections.push(self.projection(&conversation.id, &principal, 0, 1_000)?);
        }
        Ok(projections)
    }

    pub fn provision_permanent_human_dm(
        &self,
        profile_id: &keith_agent_types::ProfileId,
        now: UtcTimestamp,
    ) -> Result<ConversationRecord, RepositoryError>
    where
        R: DirectConversationRepository,
    {
        let plan = self.plan_permanent_human_dm(profile_id, now)?;
        if !plan.already_committed {
            let binding = direct::direct_binding(&plan.mutations)?;
            match self
                .repository
                .store
                .bind_direct_conversation(&binding)
                .map_err(|error| RepositoryError::Durable(error.to_string()))?
            {
                CanonicalDirectConversationOutcome::Applied { .. }
                | CanonicalDirectConversationOutcome::Replay { .. } => {}
            }
        }
        self.repository
            .conversation(&plan.conversation_id)?
            .ok_or(RepositoryError::NotFound("permanent human DM"))
    }
}

fn reduce_events<'a>(
    events: impl Iterator<Item = &'a ConversationEvent>,
) -> Result<ConversationMaterializedState, RepositoryError> {
    let mut state = ConversationMaterializedState::default();
    for event in events {
        if event.kind == ConversationEventKind::Message {
            if let Some(content) = &event.content {
                state
                    .effective_content
                    .insert(event.id.clone(), content.clone());
            }
            continue;
        }
        if matches!(
            event.kind,
            ConversationEventKind::Edit
                | ConversationEventKind::Redaction
                | ConversationEventKind::Reaction
                | ConversationEventKind::Pin
        ) {
            let payload: TargetEventPayload =
                serde_json::from_str(event.content.as_deref().unwrap_or_default())
                    .map_err(|_| DomainError::Invalid("target payload is malformed"))?;
            match payload {
                TargetEventPayload::Edit {
                    target,
                    replacement,
                    ..
                } => {
                    if !state.redacted.contains(&target)
                        && state.effective_content.contains_key(&target)
                    {
                        state.effective_content.insert(target, replacement);
                    }
                }
                TargetEventPayload::Redact { target, .. } => {
                    state.effective_content.remove(&target);
                    state.redacted.insert(target);
                }
                TargetEventPayload::React {
                    target,
                    reaction,
                    remove,
                    ..
                } => {
                    let reactions = state.reactions.entry(target).or_default();
                    if remove {
                        reactions.remove(&reaction);
                    } else {
                        reactions.insert(reaction);
                    }
                }
                TargetEventPayload::Pin { target, pinned, .. } => {
                    if pinned {
                        state.pinned.insert(target);
                    } else {
                        state.pinned.remove(&target);
                    }
                }
            }
        }
    }
    Ok(state)
}

fn participant_for<'a>(
    state: &'a RepositoryState,
    id: &ConversationId,
    principal: &Principal,
) -> Option<&'a ConversationParticipant> {
    let principal = match principal {
        Principal::Human => ParticipantPrincipal::Human,
        Principal::Agent(profile) => ParticipantPrincipal::Agent(profile.clone()),
        Principal::System => return None,
    };
    state.participants.get(&(id.clone(), principal))
}

fn ensure_visible(
    state: &RepositoryState,
    id: &ConversationId,
    principal: &Principal,
) -> Result<(), RepositoryError> {
    if matches!(principal, Principal::System)
        || participant_for(state, id, principal).is_some_and(|value| value.left_at.is_none())
    {
        Ok(())
    } else {
        Err(RepositoryError::NotFound("conversation"))
    }
}

fn require_owner(
    state: &RepositoryState,
    id: &ConversationId,
    actor: &Principal,
) -> Result<(), RepositoryError> {
    let principal = match actor {
        Principal::Human => ParticipantPrincipal::Human,
        Principal::Agent(profile) => ParticipantPrincipal::Agent(profile.clone()),
        Principal::System => {
            return Err(DomainError::Invalid("system is not a conversation owner").into());
        }
    };
    if state
        .participants
        .get(&(id.clone(), principal))
        .is_some_and(|participant| {
            participant.left_at.is_none() && participant.role == ParticipantRole::Owner
        })
    {
        Ok(())
    } else {
        Err(DomainError::Invalid("conversation owner authorization failed").into())
    }
}

impl<'a, R: StateRecordRepository> DurableConversationRepository<'a, R> {
    pub fn open(store: &'a R) -> Result<Self, RepositoryError> {
        RepositoryState::load(store)?;
        Ok(Self {
            store,
            local: ConversationRepository::new(),
        })
    }
    #[cfg(test)]
    pub(crate) fn apply(&self, mutations: &[ConversationMutation]) -> Result<(), RepositoryError> {
        self.local.apply_durable(self.store, mutations)
    }
    pub fn conversation(
        &self,
        id: &ConversationId,
    ) -> Result<Option<ConversationRecord>, RepositoryError> {
        Ok(RepositoryState::load(self.store)?
            .conversations
            .get(id)
            .cloned())
    }
    pub fn event(
        &self,
        conversation: &ConversationId,
        sequence: u64,
    ) -> Result<Option<ConversationEvent>, RepositoryError> {
        Ok(RepositoryState::load(self.store)?
            .events
            .get(&(conversation.clone(), sequence))
            .cloned())
    }
    pub fn events_after(
        &self,
        conversation: &ConversationId,
        after: u64,
        limit: usize,
    ) -> Result<Vec<ConversationEvent>, RepositoryError> {
        self.local
            .events_after_durable(self.store, conversation, after, limit)
    }
    pub fn participant(
        &self,
        conversation: &ConversationId,
        principal: &ParticipantPrincipal,
    ) -> Result<Option<ConversationParticipant>, RepositoryError> {
        Ok(RepositoryState::load(self.store)?
            .participants
            .get(&(conversation.clone(), principal.clone()))
            .cloned())
    }
    pub fn read_receipt(
        &self,
        conversation: &ConversationId,
        reader: &Principal,
    ) -> Result<Option<ReadReceipt>, RepositoryError> {
        Ok(RepositoryState::load(self.store)?
            .receipts
            .get(&(conversation.clone(), reader.clone()))
            .cloned())
    }
    pub fn grant(&self, id: &GrantId) -> Result<Option<SharedKnowledgeGrant>, RepositoryError> {
        Ok(RepositoryState::load(self.store)?.grants.get(id).cloned())
    }
    pub fn audits(&self) -> Result<Vec<ConversationAuditRecord>, RepositoryError> {
        Ok(RepositoryState::load(self.store)?
            .audits
            .into_values()
            .collect())
    }
}

#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum RepositoryError {
    #[error(transparent)]
    Domain(#[from] DomainError),
    #[error("record already exists: {0}")]
    Conflict(&'static str),
    #[error("record not found: {0}")]
    NotFound(&'static str),
    #[error("revision conflict: expected {expected}, actual {actual}")]
    RevisionConflict { expected: u64, actual: u64 },
    #[error("event sequence conflict: expected {expected}, actual {actual}")]
    SequenceConflict { expected: u64, actual: u64 },
    #[error("repository lock is poisoned")]
    LockPoisoned,
    #[error("durable repository failed: {0}")]
    Durable(String),
}

impl ConversationRepository {
    pub fn new() -> Self {
        Self::default()
    }

    pub fn apply_atomic(&self, mutations: &[ConversationMutation]) -> Result<(), RepositoryError> {
        let mut guard = self.write()?;
        let mut candidate = guard.clone();
        for mutation in mutations {
            candidate.apply(mutation.clone())?;
        }
        candidate.validate_consistency()?;
        *guard = candidate;
        Ok(())
    }

    #[cfg(test)]
    pub(crate) fn apply_durable<R: StateRecordRepository>(
        &self,
        repository: &R,
        mutations: &[ConversationMutation],
    ) -> Result<(), RepositoryError> {
        let mut guard = self.write()?;
        let before = RepositoryState::load(repository)?;
        let mut candidate = before.clone();
        for mutation in mutations {
            candidate.apply(mutation.clone())?;
        }
        candidate.validate_consistency()?;
        let durable = durable_mutations(&before, &candidate, mutations)?;
        repository
            .transact(&durable)
            .map_err(|error| RepositoryError::Durable(error.to_string()))?;
        *guard = candidate;
        Ok(())
    }

    pub fn conversation(
        &self,
        id: &ConversationId,
    ) -> Result<Option<ConversationRecord>, RepositoryError> {
        Ok(self.read()?.conversations.get(id).cloned())
    }

    pub fn conversation_durable<R: StateRecordRepository>(
        &self,
        repository: &R,
        id: &ConversationId,
    ) -> Result<Option<ConversationRecord>, RepositoryError> {
        Ok(RepositoryState::load(repository)?
            .conversations
            .get(id)
            .cloned())
    }

    pub fn events_after_durable<R: StateRecordRepository>(
        &self,
        repository: &R,
        conversation: &ConversationId,
        after: u64,
        limit: usize,
    ) -> Result<Vec<ConversationEvent>, RepositoryError> {
        let state = RepositoryState::load(repository)?;
        Ok(state
            .events
            .range(
                (conversation.clone(), after.saturating_add(1))..=(conversation.clone(), u64::MAX),
            )
            .take(limit.min(1024))
            .map(|(_, event)| event.clone())
            .collect())
    }

    pub fn event(
        &self,
        conversation: &ConversationId,
        sequence: u64,
    ) -> Result<Option<ConversationEvent>, RepositoryError> {
        Ok(self
            .read()?
            .events
            .get(&(conversation.clone(), sequence))
            .cloned())
    }

    pub fn events_after(
        &self,
        conversation: &ConversationId,
        after: u64,
        limit: usize,
    ) -> Result<Vec<ConversationEvent>, RepositoryError> {
        let limit = limit.min(1024);
        Ok(self
            .read()?
            .events
            .range(
                (conversation.clone(), after.saturating_add(1))..=(conversation.clone(), u64::MAX),
            )
            .take(limit)
            .map(|(_, event)| event.clone())
            .collect())
    }

    pub fn participant(
        &self,
        conversation: &ConversationId,
        principal: &ParticipantPrincipal,
    ) -> Result<Option<ConversationParticipant>, RepositoryError> {
        Ok(self
            .read()?
            .participants
            .get(&(conversation.clone(), principal.clone()))
            .cloned())
    }

    pub fn read_receipt(
        &self,
        conversation: &ConversationId,
        reader: &Principal,
    ) -> Result<Option<ReadReceipt>, RepositoryError> {
        Ok(self
            .read()?
            .receipts
            .get(&(conversation.clone(), reader.clone()))
            .cloned())
    }

    pub fn grant(&self, id: &GrantId) -> Result<Option<SharedKnowledgeGrant>, RepositoryError> {
        Ok(self.read()?.grants.get(id).cloned())
    }

    pub fn audits(&self) -> Result<Vec<ConversationAuditRecord>, RepositoryError> {
        Ok(self.read()?.audits.values().cloned().collect())
    }

    fn read(&self) -> Result<RwLockReadGuard<'_, RepositoryState>, RepositoryError> {
        self.state.read().map_err(|_| RepositoryError::LockPoisoned)
    }
    fn write(&self) -> Result<RwLockWriteGuard<'_, RepositoryState>, RepositoryError> {
        self.state
            .write()
            .map_err(|_| RepositoryError::LockPoisoned)
    }
}

#[allow(clippy::too_many_lines)]
fn durable_mutations(
    before: &RepositoryState,
    after: &RepositoryState,
    mutations: &[ConversationMutation],
) -> Result<Vec<RecordMutation>, RepositoryError> {
    let mut output = Vec::new();
    let mut conversations = BTreeMap::new();
    for mutation in mutations {
        match mutation {
            ConversationMutation::CreateConversation(value) => {
                conversations.insert(value.id.clone(), (value, WritePrecondition::Missing));
            }
            ConversationMutation::UpdateConversation {
                expected_revision,
                conversation,
            } => put(
                &mut output,
                Collection::Conversations,
                conversation.id.as_entity_id().clone(),
                conversation.revision,
                conversation.updated_at,
                conversation,
                WritePrecondition::Exact(*expected_revision),
            )?,
            ConversationMutation::AddParticipant(value) => {
                let conversation = after
                    .conversations
                    .get(&value.conversation_id)
                    .ok_or(RepositoryError::NotFound("conversation"))?;
                conversations.insert(
                    conversation.id.clone(),
                    (
                        conversation,
                        before
                            .conversations
                            .get(&conversation.id)
                            .map_or(WritePrecondition::Missing, |record| {
                                WritePrecondition::Exact(record.revision)
                            }),
                    ),
                );
                put(
                    &mut output,
                    Collection::ConversationParticipants,
                    compound_id(
                        &value.conversation_id.to_string(),
                        &format!("{:?}", value.principal),
                    ),
                    value.revision,
                    value.joined_at,
                    value,
                    WritePrecondition::Missing,
                )?;
            }
            ConversationMutation::UpdateParticipant {
                expected_revision,
                participant,
            } => put(
                &mut output,
                Collection::ConversationParticipants,
                compound_id(
                    &participant.conversation_id.to_string(),
                    &format!("{:?}", participant.principal),
                ),
                participant.revision,
                participant.joined_at,
                participant,
                WritePrecondition::Exact(*expected_revision),
            )?,
            ConversationMutation::AppendEvent { event, .. } => {
                if before.event_ids.contains_key(&event.id) {
                    continue;
                }
                let conversation = after
                    .conversations
                    .get(&event.conversation_id)
                    .ok_or(RepositoryError::NotFound("conversation"))?;
                conversations.insert(
                    conversation.id.clone(),
                    (
                        conversation,
                        before
                            .conversations
                            .get(&conversation.id)
                            .map_or(WritePrecondition::Missing, |record| {
                                WritePrecondition::Exact(record.revision)
                            }),
                    ),
                );
                put(
                    &mut output,
                    Collection::ConversationEvents,
                    event.id.as_entity_id().clone(),
                    Revision::ZERO,
                    event.timestamp,
                    &DurableEventRecord::Event {
                        event: Box::new(event.clone()),
                    },
                    WritePrecondition::Missing,
                )?;
                put(
                    &mut output,
                    Collection::ConversationEvents,
                    publication_sentinel_id(&event.publication_key),
                    Revision::ZERO,
                    event.timestamp,
                    &DurableEventRecord::PublicationKey {
                        key: event.publication_key.clone(),
                        event_id: event.id.clone(),
                        event_digest: event_digest(event)?,
                    },
                    WritePrecondition::Missing,
                )?;
                for collection in [
                    Collection::ConversationProjectionIntents,
                    Collection::ConversationUnreadIntents,
                    Collection::ConversationSearchIntents,
                    Collection::ConversationPublicationIntents,
                ] {
                    put(
                        &mut output,
                        collection,
                        compound_id(collection.as_str(), &event.id.to_string()),
                        Revision::ZERO,
                        event.timestamp,
                        &serde_json::json!({
                            "conversation_id": event.conversation_id,
                            "event_id": event.id,
                            "sequence": event.sequence,
                            "publication_key": event.publication_key,
                        }),
                        WritePrecondition::Missing,
                    )?;
                }
            }
            ConversationMutation::AdvanceRead {
                expected_revision,
                receipt,
            } => put(
                &mut output,
                Collection::ReadReceipts,
                compound_id(
                    &receipt.conversation_id.to_string(),
                    &format!("{:?}", receipt.reader),
                ),
                receipt.revision,
                receipt.updated_at,
                receipt,
                expected_revision.map_or(WritePrecondition::Missing, WritePrecondition::Exact),
            )?,
            ConversationMutation::PutGrant {
                expected_revision,
                grant,
            } => put(
                &mut output,
                Collection::SharedKnowledgeGrants,
                grant.id.as_entity_id().clone(),
                grant.revision,
                grant.created_at,
                grant,
                expected_revision.map_or(WritePrecondition::Missing, WritePrecondition::Exact),
            )?,
            ConversationMutation::AppendAudit(value) => put(
                &mut output,
                Collection::TeammateAudits,
                value.id.as_entity_id().clone(),
                Revision::ZERO,
                value.occurred_at,
                value,
                WritePrecondition::Missing,
            )?,
        }
    }
    for (_, (conversation, precondition)) in conversations {
        put(
            &mut output,
            Collection::Conversations,
            conversation.id.as_entity_id().clone(),
            conversation.revision,
            conversation.updated_at,
            conversation,
            precondition,
        )?;
    }
    Ok(output)
}

fn put<T: serde::Serialize>(
    output: &mut Vec<RecordMutation>,
    collection: Collection,
    id: EntityId,
    revision: Revision,
    timestamp: UtcTimestamp,
    value: &T,
    precondition: WritePrecondition,
) -> Result<(), RepositoryError> {
    output.push(RecordMutation::Put {
        collection,
        record: VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id,
            revision,
            updated_at: timestamp,
            payload: serde_json::to_value(value)
                .map_err(|error| DomainError::Malformed(error.to_string()))?,
        },
        precondition,
    });
    Ok(())
}

fn compound_id(left: &str, right: &str) -> EntityId {
    let digest = Sha256::digest(format!("{left}\0{right}").as_bytes());
    let mut bytes = [0_u8; 16];
    bytes.copy_from_slice(&digest[..16]);
    EntityId::from_u128(u128::from_be_bytes(bytes))
}

fn publication_sentinel_id(key: &keith_agent_types::StableKey) -> EntityId {
    compound_id("conversation-publication-key", key.as_str())
}

fn event_digest(event: &ConversationEvent) -> Result<String, RepositoryError> {
    let bytes = canonical_json(event)?;
    let mut output = String::with_capacity(64);
    for byte in Sha256::digest(bytes) {
        use std::fmt::Write as _;
        write!(&mut output, "{byte:02x}").expect("writing to String cannot fail");
    }
    Ok(output)
}

fn durable_event_record_digest(event: &ConversationEvent) -> Result<String, RepositoryError> {
    let record = VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: event.id.as_entity_id().clone(),
        revision: Revision::ZERO,
        updated_at: event.timestamp,
        payload: serde_json::to_value(DurableEventRecord::Event {
            event: Box::new(event.clone()),
        })
        .map_err(|error| DomainError::Malformed(error.to_string()))?,
    };
    let bytes = keith_agent_types::canonical_json_bytes(&record)
        .map_err(|error| DomainError::Malformed(error.to_string()))?;
    Ok(hex_sha256(&bytes))
}

fn hex_sha256(bytes: &[u8]) -> String {
    let mut output = String::with_capacity(64);
    for byte in Sha256::digest(bytes) {
        use std::fmt::Write as _;
        write!(&mut output, "{byte:02x}").expect("writing to String cannot fail");
    }
    output
}

impl RepositoryState {
    #[allow(clippy::too_many_lines)]
    fn load<R: StateRecordRepository>(repository: &R) -> Result<Self, RepositoryError> {
        let mut state = Self::default();
        for stored in list(repository, Collection::Conversations)? {
            let value: ConversationRecord = decode_stored(&stored)?;
            check_metadata(
                &stored,
                value.id.as_entity_id(),
                value.revision,
                value.updated_at,
            )?;
            if state
                .conversations
                .insert(value.id.clone(), value)
                .is_some()
            {
                return Err(RepositoryError::Conflict("conversation ID"));
            }
        }
        for stored in list(repository, Collection::ConversationParticipants)? {
            let value: ConversationParticipant = decode_stored(&stored)?;
            value.validate()?;
            let expected = compound_id(
                &value.conversation_id.to_string(),
                &format!("{:?}", value.principal),
            );
            check_metadata(&stored, &expected, value.revision, value.joined_at)?;
            let key = (value.conversation_id.clone(), value.principal.clone());
            if state.participants.insert(key, value).is_some() {
                return Err(RepositoryError::Conflict("conversation participant"));
            }
        }
        let mut sentinels = BTreeMap::new();
        for stored in list(repository, Collection::ConversationEvents)? {
            let value: DurableEventRecord = serde_json::from_value(stored.payload.clone())
                .map_err(|error| DomainError::Malformed(error.to_string()))?;
            match value {
                DurableEventRecord::Event { event } => {
                    let event = *event;
                    event.validate()?;
                    check_metadata(
                        &stored,
                        event.id.as_entity_id(),
                        Revision::ZERO,
                        event.timestamp,
                    )?;
                    if state
                        .event_ids
                        .insert(event.id.clone(), event.conversation_id.clone())
                        .is_some()
                    {
                        return Err(RepositoryError::Conflict("event ID"));
                    }
                    if state
                        .events
                        .insert(
                            (event.conversation_id.clone(), event.sequence),
                            event.clone(),
                        )
                        .is_some()
                    {
                        return Err(RepositoryError::Conflict("event sequence"));
                    }
                    if state
                        .publication_keys
                        .insert(event.publication_key.clone(), event)
                        .is_some()
                    {
                        return Err(RepositoryError::Conflict("publication key"));
                    }
                }
                DurableEventRecord::PublicationKey {
                    key,
                    event_id,
                    event_digest,
                } => {
                    let expected = publication_sentinel_id(&key);
                    check_metadata(&stored, &expected, Revision::ZERO, stored.updated_at)?;
                    if sentinels.insert(key, (event_id, event_digest)).is_some() {
                        return Err(RepositoryError::Conflict("publication sentinel"));
                    }
                }
            }
        }
        for stored in list(repository, Collection::ConversationStableKeys)? {
            let event_id = stored
                .payload
                .get("event_id")
                .and_then(serde_json::Value::as_str)
                .ok_or(DomainError::Invalid("stable key event ID is missing"))?;
            let event = state
                .publication_keys
                .values()
                .find(|event| event.id.to_string() == event_id)
                .ok_or(DomainError::Invalid(
                    "stable key references an absent event",
                ))?;
            let digest = stored
                .payload
                .get("event_digest")
                .and_then(serde_json::Value::as_str)
                .ok_or(DomainError::Invalid("stable key digest is missing"))?
                .to_owned();
            let expected = publication_sentinel_id(&event.publication_key);
            check_metadata(&stored, &expected, Revision::ZERO, event.timestamp)?;
            sentinels.insert(event.publication_key.clone(), (event.id.clone(), digest));
        }
        for (key, event) in &state.publication_keys {
            let Some((event_id, digest)) = sentinels.remove(key) else {
                return Err(DomainError::Invalid("publication sentinel is missing").into());
            };
            if event_id != event.id
                || (digest != event_digest(event)? && digest != durable_event_record_digest(event)?)
            {
                return Err(DomainError::Invalid("publication sentinel is corrupt").into());
            }
        }
        if !sentinels.is_empty() {
            return Err(DomainError::Invalid("orphan publication sentinel").into());
        }
        for stored in list(repository, Collection::ReadReceipts)? {
            let value: ReadReceipt = decode_stored(&stored)?;
            let expected = compound_id(
                &value.conversation_id.to_string(),
                &format!("{:?}", value.reader),
            );
            check_metadata(&stored, &expected, value.revision, value.updated_at)?;
            state
                .receipts
                .insert((value.conversation_id.clone(), value.reader.clone()), value);
        }
        for stored in list(repository, Collection::SharedKnowledgeGrants)? {
            let value: SharedKnowledgeGrant = decode_stored(&stored)?;
            check_metadata(
                &stored,
                value.id.as_entity_id(),
                value.revision,
                value.created_at,
            )?;
            state.grants.insert(value.id.clone(), value);
        }
        for stored in list(repository, Collection::TeammateAudits)? {
            let value: ConversationAuditRecord = decode_stored(&stored)?;
            check_metadata(
                &stored,
                value.id.as_entity_id(),
                Revision::ZERO,
                value.occurred_at,
            )?;
            state.audits.insert(value.id.clone(), value);
        }
        state.validate_consistency()?;
        Ok(state)
    }

    fn validate_consistency(&self) -> Result<(), RepositoryError> {
        for conversation in self.conversations.values() {
            self.validate_conversation_membership(conversation)?;
            self.validate_conversation_events(conversation)?;
        }
        for ((conversation, reader), receipt) in &self.receipts {
            let principal = match reader {
                Principal::Human => ParticipantPrincipal::Human,
                Principal::Agent(profile) => ParticipantPrincipal::Agent(profile.clone()),
                Principal::System => {
                    return Err(DomainError::Invalid("system read receipt is invalid").into());
                }
            };
            if self
                .participants
                .get(&(conversation.clone(), principal))
                .is_none_or(|participant| participant.left_at.is_some())
            {
                return Err(DomainError::Invalid(
                    "durable read receipt owner is not a participant",
                )
                .into());
            }
            let head = self
                .conversations
                .get(conversation)
                .and_then(|record| record.event_head.as_ref())
                .map_or(0, |head| head.sequence);
            if receipt.read_through_sequence > head {
                return Err(DomainError::Invalid("durable read cursor exceeds event head").into());
            }
        }
        Ok(())
    }

    fn validate_conversation_membership(
        &self,
        conversation: &ConversationRecord,
    ) -> Result<(), RepositoryError> {
        for profile in &conversation.participant_profiles {
            if !self.participants.contains_key(&(
                conversation.id.clone(),
                ParticipantPrincipal::Agent(profile.clone()),
            )) {
                return Err(DomainError::Invalid("conversation participant row is missing").into());
            }
        }
        if conversation.human_participant
            && !self
                .participants
                .contains_key(&(conversation.id.clone(), ParticipantPrincipal::Human))
        {
            return Err(DomainError::Invalid("human participant row is missing").into());
        }
        let rows = self
            .participants
            .iter()
            .filter(|((conversation_id, _), _)| conversation_id == &conversation.id)
            .map(|(_, participant)| participant)
            .collect::<Vec<_>>();
        match conversation.kind {
            ConversationKind::HumanAgentDm => {
                let profile = conversation
                    .participant_profiles
                    .first()
                    .ok_or(DomainError::Invalid("human DM profile is missing"))?;
                let human = self
                    .participants
                    .get(&(conversation.id.clone(), ParticipantPrincipal::Human));
                let agent = self.participants.get(&(
                    conversation.id.clone(),
                    ParticipantPrincipal::Agent(profile.clone()),
                ));
                if rows.len() != 2
                    || !human.is_some_and(|row| {
                        row.left_at.is_none() && row.role == ParticipantRole::Owner
                    })
                    || !agent.is_some_and(|row| {
                        row.left_at.is_none() && row.role == ParticipantRole::Member
                    })
                {
                    return Err(
                        DomainError::Invalid("human DM durable membership is not exact").into(),
                    );
                }
            }
            ConversationKind::AgentAgentDm => {
                let exact = rows.len() == 2
                    && !conversation.human_participant
                    && conversation.participant_profiles.len() == 2
                    && rows.iter().all(|row| {
                        matches!(row.principal, ParticipantPrincipal::Agent(_))
                            && row.left_at.is_none()
                            && row.role == ParticipantRole::Member
                    });
                if !exact {
                    return Err(
                        DomainError::Invalid("agent DM durable membership is not exact").into(),
                    );
                }
            }
            ConversationKind::Group | ConversationKind::Thread => {}
        }
        Ok(())
    }

    fn validate_conversation_events(
        &self,
        conversation: &ConversationRecord,
    ) -> Result<(), RepositoryError> {
        let events: Vec<_> = self
            .events
            .range((conversation.id.clone(), 0)..=(conversation.id.clone(), u64::MAX))
            .map(|(_, event)| event)
            .collect();
        for (index, event) in events.iter().enumerate() {
            if event.sequence
                != u64::try_from(index + 1)
                    .map_err(|_| DomainError::Invalid("event sequence overflow"))?
            {
                return Err(DomainError::Invalid("durable event sequence has a gap").into());
            }
            let authorized = match &event.author {
                Principal::System => true,
                Principal::Human => self
                    .participants
                    .get(&(conversation.id.clone(), ParticipantPrincipal::Human))
                    .is_some_and(|participant| participant.left_at.is_none()),
                Principal::Agent(profile) => self
                    .participants
                    .get(&(
                        conversation.id.clone(),
                        ParticipantPrincipal::Agent(profile.clone()),
                    ))
                    .is_some_and(|participant| participant.left_at.is_none()),
            };
            if !authorized {
                return Err(
                    DomainError::Invalid("durable event author is not a participant").into(),
                );
            }
        }
        match (&conversation.event_head, events.last()) {
            (None, None) => Ok(()),
            (Some(head), Some(event))
                if head.sequence == event.sequence && head.event_id == event.id =>
            {
                Ok(())
            }
            _ => {
                Err(DomainError::Invalid("conversation head does not match durable events").into())
            }
        }
    }

    fn apply(&mut self, mutation: ConversationMutation) -> Result<(), RepositoryError> {
        match mutation {
            ConversationMutation::CreateConversation(record) => self.create_conversation(record),
            ConversationMutation::UpdateConversation {
                expected_revision,
                conversation,
            } => self.update_conversation(expected_revision, conversation),
            ConversationMutation::AddParticipant(record) => self.add_participant(record),
            ConversationMutation::UpdateParticipant {
                expected_revision,
                participant,
            } => self.update_participant(expected_revision, participant),
            ConversationMutation::AppendEvent {
                expected_revision,
                event,
                intents,
            } => self.append_event(expected_revision, event, intents),
            ConversationMutation::AdvanceRead {
                expected_revision,
                receipt,
            } => self.advance_read(expected_revision, receipt),
            ConversationMutation::PutGrant {
                expected_revision,
                grant,
            } => self.put_grant(expected_revision, grant),
            ConversationMutation::AppendAudit(record) => self.append_audit(record),
        }
    }

    fn update_conversation(
        &mut self,
        expected: Revision,
        record: ConversationRecord,
    ) -> Result<(), RepositoryError> {
        record.validate()?;
        let current = self
            .conversations
            .get(&record.id)
            .ok_or(RepositoryError::NotFound("conversation"))?;
        check_revision(current.revision, expected)?;
        if record.revision
            != expected
                .checked_next()
                .ok_or(DomainError::Invalid("revision overflow"))?
            || record.created_at != current.created_at
            || record.kind != current.kind
            || record.creator != current.creator
            || record.event_head != current.event_head
            || record.participant_profiles != current.participant_profiles
            || record.human_participant != current.human_participant
        {
            return Err(DomainError::Invalid(
                "conversation update changed canonical identity or transcript state",
            )
            .into());
        }
        self.conversations.insert(record.id.clone(), record);
        Ok(())
    }

    fn update_participant(
        &mut self,
        expected: Revision,
        record: ConversationParticipant,
    ) -> Result<(), RepositoryError> {
        record.validate()?;
        let key = (record.conversation_id.clone(), record.principal.clone());
        let current = self
            .participants
            .get(&key)
            .ok_or(RepositoryError::NotFound("participant"))?;
        check_revision(current.revision, expected)?;
        if record.revision
            != expected
                .checked_next()
                .ok_or(DomainError::Invalid("revision overflow"))?
            || record.joined_at != current.joined_at
            || record.role != current.role
            || record.applied_through_sequence < current.applied_through_sequence
        {
            return Err(DomainError::Invalid("participant projection update is invalid").into());
        }
        self.participants.insert(key, record);
        Ok(())
    }

    fn create_conversation(&mut self, record: ConversationRecord) -> Result<(), RepositoryError> {
        record.validate()?;
        if record.revision != Revision::ZERO || record.event_head.is_some() {
            return Err(RepositoryError::Domain(DomainError::Invalid(
                "new conversation must have zero revision and no event head",
            )));
        }
        if record.participant_revision != Revision::ZERO {
            return Err(DomainError::Invalid("new participant revision must be zero").into());
        }
        if self.conversations.contains_key(&record.id) {
            return Err(RepositoryError::Conflict("conversation ID"));
        }
        self.conversations.insert(record.id.clone(), record);
        Ok(())
    }

    fn add_participant(&mut self, record: ConversationParticipant) -> Result<(), RepositoryError> {
        record.validate()?;
        let conversation = self
            .conversations
            .get_mut(&record.conversation_id)
            .ok_or(RepositoryError::NotFound("conversation"))?;
        let key = (record.conversation_id.clone(), record.principal.clone());
        if self.participants.contains_key(&key) {
            return Err(RepositoryError::Conflict("conversation participant"));
        }
        match &record.principal {
            ParticipantPrincipal::Human => conversation.human_participant = true,
            ParticipantPrincipal::Agent(profile) => {
                conversation.participant_profiles.insert(profile.clone());
            }
        }
        conversation.participant_revision = conversation
            .participant_revision
            .checked_next()
            .ok_or(DomainError::Invalid("participant revision overflow"))?;
        conversation.revision = conversation
            .revision
            .checked_next()
            .ok_or(DomainError::Invalid("revision overflow"))?;
        self.participants.insert(key, record);
        Ok(())
    }

    fn append_event(
        &mut self,
        expected_revision: Revision,
        event: ConversationEvent,
        intents: AppendIntents,
    ) -> Result<(), RepositoryError> {
        event.validate()?;
        intents.validate()?;
        if let Some(existing) = self.publication_keys.get(&event.publication_key) {
            return if existing == &event {
                Ok(())
            } else {
                Err(RepositoryError::Conflict("publication key"))
            };
        }
        if self.event_ids.contains_key(&event.id) {
            return Err(RepositoryError::Conflict("event ID"));
        }
        let conversation = self
            .conversations
            .get_mut(&event.conversation_id)
            .ok_or(RepositoryError::NotFound("conversation"))?;
        check_revision(conversation.revision, expected_revision)?;
        match &event.author {
            Principal::Human
                if self
                    .participants
                    .get(&(event.conversation_id.clone(), ParticipantPrincipal::Human))
                    .is_none_or(|participant| participant.left_at.is_some()) =>
            {
                return Err(DomainError::Invalid("human author is not a participant").into());
            }
            Principal::Agent(profile)
                if self
                    .participants
                    .get(&(
                        event.conversation_id.clone(),
                        ParticipantPrincipal::Agent(profile.clone()),
                    ))
                    .is_none_or(|participant| participant.left_at.is_some()) =>
            {
                return Err(DomainError::Invalid("agent author is not a participant").into());
            }
            _ => {}
        }
        let expected_sequence = conversation
            .event_head
            .as_ref()
            .map_or(1, |head| head.sequence.saturating_add(1));
        if event.sequence != expected_sequence {
            return Err(RepositoryError::SequenceConflict {
                expected: expected_sequence,
                actual: event.sequence,
            });
        }
        if event.reply_to.as_ref().is_some_and(|id| {
            !self
                .events
                .values()
                .any(|prior| prior.conversation_id == event.conversation_id && &prior.id == id)
        }) {
            return Err(RepositoryError::NotFound("reply event"));
        }
        if event.thread_parent.as_ref().is_some_and(|id| {
            !self
                .events
                .values()
                .any(|prior| prior.conversation_id == event.conversation_id && &prior.id == id)
        }) {
            return Err(RepositoryError::NotFound("thread parent event"));
        }
        let next_revision = conversation
            .revision
            .checked_next()
            .ok_or(RepositoryError::Domain(DomainError::Invalid(
                "revision overflow",
            )))?;
        conversation.revision = next_revision;
        conversation.updated_at = event.timestamp.max(conversation.updated_at);
        conversation.event_head = Some(EventHead {
            sequence: event.sequence,
            event_id: event.id.clone(),
        });
        self.publication_keys
            .insert(event.publication_key.clone(), event.clone());
        self.event_ids
            .insert(event.id.clone(), event.conversation_id.clone());
        self.events
            .insert((event.conversation_id.clone(), event.sequence), event);
        Ok(())
    }

    fn advance_read(
        &mut self,
        expected_revision: Option<Revision>,
        receipt: ReadReceipt,
    ) -> Result<(), RepositoryError> {
        if receipt.schema_version != CONVERSATION_SCHEMA_VERSION {
            return Err(DomainError::UnsupportedVersion(receipt.schema_version.major).into());
        }
        let conversation = self
            .conversations
            .get(&receipt.conversation_id)
            .ok_or(RepositoryError::NotFound("conversation"))?;
        let participant = match &receipt.reader {
            Principal::Human => ParticipantPrincipal::Human,
            Principal::Agent(profile) => ParticipantPrincipal::Agent(profile.clone()),
            Principal::System => {
                return Err(DomainError::Invalid("system cannot own a read receipt").into());
            }
        };
        if self
            .participants
            .get(&(receipt.conversation_id.clone(), participant))
            .is_none_or(|participant| participant.left_at.is_some())
        {
            return Err(DomainError::Invalid("reader is not a participant").into());
        }
        let head = conversation
            .event_head
            .as_ref()
            .map_or(0, |value| value.sequence);
        if receipt.read_through_sequence > head {
            return Err(DomainError::Invalid("read cursor exceeds event head").into());
        }
        let key = (receipt.conversation_id.clone(), receipt.reader.clone());
        if let Some(current) = self.receipts.get(&key) {
            check_revision(
                current.revision,
                expected_revision.ok_or(RepositoryError::RevisionConflict {
                    expected: current.revision.get(),
                    actual: receipt.revision.get(),
                })?,
            )?;
            if receipt.read_through_sequence < current.read_through_sequence {
                return Err(DomainError::Invalid("read cursor cannot move backward").into());
            }
            let expected_next = current
                .revision
                .checked_next()
                .ok_or(DomainError::Invalid("revision overflow"))?;
            if receipt.revision != expected_next {
                return Err(RepositoryError::RevisionConflict {
                    expected: expected_next.get(),
                    actual: receipt.revision.get(),
                });
            }
        } else if expected_revision.is_some() || receipt.revision != Revision::ZERO {
            return Err(RepositoryError::RevisionConflict {
                expected: 0,
                actual: receipt.revision.get(),
            });
        }
        self.receipts.insert(key, receipt);
        Ok(())
    }

    fn put_grant(
        &mut self,
        expected_revision: Option<Revision>,
        grant: SharedKnowledgeGrant,
    ) -> Result<(), RepositoryError> {
        grant.validate()?;
        if let Some(current) = self.grants.get(&grant.id) {
            check_revision(
                current.revision,
                expected_revision.ok_or(RepositoryError::RevisionConflict {
                    expected: current.revision.get(),
                    actual: grant.revision.get(),
                })?,
            )?;
            let expected_next = current
                .revision
                .checked_next()
                .ok_or(DomainError::Invalid("revision overflow"))?;
            if grant.revision != expected_next {
                return Err(RepositoryError::RevisionConflict {
                    expected: expected_next.get(),
                    actual: grant.revision.get(),
                });
            }
        } else if expected_revision.is_some() || grant.revision != Revision::ZERO {
            return Err(RepositoryError::RevisionConflict {
                expected: 0,
                actual: grant.revision.get(),
            });
        }
        self.grants.insert(grant.id.clone(), grant);
        Ok(())
    }

    fn append_audit(&mut self, record: ConversationAuditRecord) -> Result<(), RepositoryError> {
        record.validate()?;
        if self.audits.contains_key(&record.id) {
            return Err(RepositoryError::Conflict("audit ID"));
        }
        self.audits.insert(record.id.clone(), record);
        Ok(())
    }
}

fn list<R: StateRecordRepository>(
    repository: &R,
    collection: Collection,
) -> Result<Vec<VersionedRecord>, RepositoryError> {
    repository
        .list_records(collection)
        .map_err(|error| RepositoryError::Durable(error.to_string()))
}

fn decode_stored<T: serde::de::DeserializeOwned + ValidateRecord>(
    stored: &VersionedRecord,
) -> Result<T, RepositoryError> {
    let value: T = serde_json::from_value(stored.payload.clone())
        .map_err(|error| DomainError::Malformed(error.to_string()))?;
    value.validate_record()?;
    Ok(value)
}

fn check_metadata(
    stored: &VersionedRecord,
    id: &EntityId,
    revision: Revision,
    updated_at: UtcTimestamp,
) -> Result<(), RepositoryError> {
    if stored.version != CURRENT_SCHEMA_VERSION
        || &stored.id != id
        || stored.revision != revision
        || stored.updated_at != updated_at
    {
        return Err(DomainError::Invalid("durable record metadata mismatch").into());
    }
    Ok(())
}

fn check_revision(actual: Revision, expected: Revision) -> Result<(), RepositoryError> {
    if actual == expected {
        Ok(())
    } else {
        Err(RepositoryError::RevisionConflict {
            expected: expected.get(),
            actual: actual.get(),
        })
    }
}

#[cfg(test)]
mod domain_tests {
    use super::*;
    use keith_agent_types::{EntityId, ProfileId, StableKey};
    use keith_knowledge::KnowledgeAccessResolver as _;
    use keith_state_store_core::AtomicStateRepository as _;
    use std::collections::BTreeSet;

    struct DenyAttachments;
    impl keith_artifacts::ConversationArtifactResolver for DenyAttachments {
        fn validate_reference(
            &self,
            _: &keith_artifacts::ArtifactActor,
            _: &ConversationId,
            _: &keith_agent_types::ArtifactId,
            _: &str,
            _: &[EventId],
            _: UtcTimestamp,
        ) -> Result<(), keith_artifacts::ArtifactError> {
            Err(keith_artifacts::ArtifactError::AccessDenied)
        }
    }
    struct AllowHumanAttachments;
    impl keith_artifacts::ConversationArtifactResolver for AllowHumanAttachments {
        fn validate_reference(
            &self,
            actor: &keith_artifacts::ArtifactActor,
            _: &ConversationId,
            _: &keith_agent_types::ArtifactId,
            _: &str,
            source_event_ids: &[EventId],
            _: UtcTimestamp,
        ) -> Result<(), keith_artifacts::ArtifactError> {
            if actor == &keith_artifacts::ArtifactActor::HumanOwner && !source_event_ids.is_empty()
            {
                Ok(())
            } else {
                Err(keith_artifacts::ArtifactError::AccessDenied)
            }
        }
    }

    fn profile(value: u128) -> ProfileId {
        ProfileId(EntityId::from_u128(value))
    }
    fn conversation(value: u128, profiles: &[ProfileId]) -> ConversationRecord {
        ConversationRecord {
            schema_version: CONVERSATION_SCHEMA_VERSION,
            id: ConversationId(EntityId::from_u128(value)),
            kind: ConversationKind::Group,
            lifecycle: ConversationLifecycle::Active,
            title: "work".into(),
            creator: Principal::Human,
            created_at: UtcTimestamp::from_unix_millis(10),
            updated_at: UtcTimestamp::from_unix_millis(10),
            revision: Revision::ZERO,
            participant_revision: Revision::ZERO,
            participant_profiles: profiles.iter().cloned().collect(),
            human_participant: false,
            event_head: None,
        }
    }
    fn event(conversation_id: ConversationId, sequence: u64, key: &str) -> ConversationEvent {
        ConversationEvent {
            schema_version: CURRENT_SCHEMA_VERSION,
            id: EventId(EntityId::from_u128(100 + u128::from(sequence))),
            conversation_id,
            sequence,
            publication_key: StableKey::parse(key).unwrap(),
            author: Principal::Agent(profile(1)),
            timestamp: UtcTimestamp::from_unix_millis(20 + i64::try_from(sequence).unwrap()),
            kind: ConversationEventKind::Message,
            content: Some("hello".into()),
            artifacts: vec![],
            reply_to: None,
            thread_parent: None,
            provenance: EventProvenance {
                source: "native".into(),
                source_ids: vec![],
                migration_version: None,
            },
        }
    }
    fn participant(id: ConversationId, owner: ProfileId) -> ConversationParticipant {
        ConversationParticipant {
            schema_version: CURRENT_SCHEMA_VERSION,
            conversation_id: id,
            principal: ParticipantPrincipal::Agent(owner),
            role: ParticipantRole::Member,
            joined_at: UtcTimestamp::from_unix_millis(10),
            left_at: None,
            revision: Revision::ZERO,
            applied_through_sequence: 0,
            hidden: false,
            muted: false,
            notification_policy: NotificationPolicy {
                mentions_only: false,
                muted: false,
            },
        }
    }

    #[test]
    fn domain_atomic_batch_rolls_back_and_enforces_ordered_immutable_events() {
        let repository = ConversationRepository::new();
        let record = conversation(1, &[profile(1)]);
        let id = record.id.clone();
        let invalid = event(id.clone(), 2, "bad-order");
        assert!(
            repository
                .apply_atomic(&[
                    ConversationMutation::CreateConversation(record),
                    ConversationMutation::AddParticipant(participant(id.clone(), profile(1))),
                    ConversationMutation::AppendEvent {
                        expected_revision: Revision::ZERO,
                        event: invalid,
                        intents: AppendIntents::REQUIRED,
                    }
                ])
                .is_err()
        );
        assert_eq!(repository.conversation(&id).unwrap(), None);

        repository
            .apply_atomic(&[
                ConversationMutation::CreateConversation(conversation(1, &[profile(1)])),
                ConversationMutation::AddParticipant(participant(id.clone(), profile(1))),
                ConversationMutation::AppendEvent {
                    expected_revision: Revision::new(1),
                    event: event(id.clone(), 1, "publish-1"),
                    intents: AppendIntents::REQUIRED,
                },
            ])
            .unwrap();
        assert_eq!(
            repository
                .conversation(&id)
                .unwrap()
                .unwrap()
                .event_head
                .unwrap()
                .sequence,
            1
        );
        assert_eq!(
            repository
                .event(&id, 1)
                .unwrap()
                .unwrap()
                .content
                .as_deref(),
            Some("hello")
        );
    }

    #[test]
    fn domain_publication_replay_is_idempotent_but_key_reuse_is_rejected() {
        let repository = ConversationRepository::new();
        let record = conversation(2, &[profile(1)]);
        let id = record.id.clone();
        repository
            .apply_atomic(&[
                ConversationMutation::CreateConversation(record),
                ConversationMutation::AddParticipant(participant(id.clone(), profile(1))),
            ])
            .unwrap();
        let first = event(id.clone(), 1, "stable");
        repository
            .apply_atomic(&[ConversationMutation::AppendEvent {
                expected_revision: Revision::new(1),
                event: first.clone(),
                intents: AppendIntents::REQUIRED,
            }])
            .unwrap();
        repository
            .apply_atomic(&[ConversationMutation::AppendEvent {
                expected_revision: Revision::new(1),
                event: first.clone(),
                intents: AppendIntents::REQUIRED,
            }])
            .unwrap();
        let mut altered_replay = first;
        altered_replay.content = Some("different bytes".into());
        assert_eq!(
            repository.apply_atomic(&[ConversationMutation::AppendEvent {
                expected_revision: Revision::new(1),
                event: altered_replay,
                intents: AppendIntents::REQUIRED,
            }]),
            Err(RepositoryError::Conflict("publication key"))
        );
        let mut collision = event(id.clone(), 2, "stable");
        collision.id = EventId(EntityId::from_u128(999));
        assert_eq!(
            repository.apply_atomic(&[ConversationMutation::AppendEvent {
                expected_revision: Revision::new(2),
                event: collision,
                intents: AppendIntents::REQUIRED,
            }]),
            Err(RepositoryError::Conflict("publication key"))
        );
        assert_eq!(repository.events_after(&id, 0, 10).unwrap().len(), 1);
    }

    #[test]
    fn domain_rejects_nonparticipant_authors_and_unbounded_artifacts() {
        let repository = ConversationRepository::new();
        let owner = profile(1);
        let id = ConversationId(EntityId::from_u128(88));
        repository
            .apply_atomic(&[
                ConversationMutation::CreateConversation(conversation(
                    88,
                    std::slice::from_ref(&owner),
                )),
                ConversationMutation::AddParticipant(participant(id.clone(), owner)),
            ])
            .unwrap();
        let mut forged = event(id.clone(), 1, "forged-author");
        forged.author = Principal::Agent(profile(99));
        assert!(
            repository
                .apply_atomic(&[ConversationMutation::AppendEvent {
                    expected_revision: Revision::new(1),
                    event: forged,
                    intents: AppendIntents::REQUIRED
                }])
                .is_err()
        );
        let mut oversized = event(id, 1, "artifact-bound");
        oversized.artifacts = (0..=MAX_ARTIFACTS)
            .map(|index| ArtifactReference {
                artifact_id: keith_agent_types::ArtifactId(EntityId::from_u128(index as u128 + 1)),
                digest_sha256: "00".repeat(32),
            })
            .collect();
        assert_eq!(
            oversized.validate(),
            Err(DomainError::BoundExceeded("event artifacts"))
        );
    }

    #[test]
    fn domain_durable_batch_survives_embedded_store_restart() {
        use keith_state_store::EmbeddedStore;
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("conversation.sqlite");
        let owner = profile(1);
        let id = ConversationId(EntityId::from_u128(90));
        {
            let store = EmbeddedStore::open(&path, None).unwrap();
            let repository = ConversationRepository::new();
            repository
                .apply_durable(
                    &store,
                    &[
                        ConversationMutation::CreateConversation(conversation(
                            90,
                            std::slice::from_ref(&owner),
                        )),
                        ConversationMutation::AddParticipant(participant(id.clone(), owner)),
                        ConversationMutation::AppendEvent {
                            expected_revision: Revision::new(1),
                            event: event(id.clone(), 1, "durable-event"),
                            intents: AppendIntents::REQUIRED,
                        },
                    ],
                )
                .unwrap();
        }
        let reopened = EmbeddedStore::open(&path, None).unwrap();
        let conversation = reopened
            .get_record(Collection::Conversations, id.as_entity_id())
            .unwrap()
            .unwrap();
        let decoded: ConversationRecord = serde_json::from_value(conversation.payload).unwrap();
        assert_eq!(decoded.event_head.unwrap().sequence, 1);
        assert_eq!(
            reopened
                .list_records(Collection::ConversationEvents)
                .unwrap()
                .len(),
            2
        );
        assert_eq!(
            reopened
                .list_records(Collection::ConversationParticipants)
                .unwrap()
                .len(),
            1
        );
        let reopened_repository = DurableConversationRepository::open(&reopened).unwrap();
        reopened_repository
            .apply(&[ConversationMutation::AppendEvent {
                expected_revision: Revision::new(2),
                event: event(id.clone(), 2, "durable-event-2"),
                intents: AppendIntents::REQUIRED,
            }])
            .unwrap();
        DurableConversationRepository::open(&reopened)
            .unwrap()
            .apply(&[ConversationMutation::AppendEvent {
                expected_revision: Revision::ZERO,
                event: event(id.clone(), 1, "durable-event"),
                intents: AppendIntents::REQUIRED,
            }])
            .unwrap();
        assert_eq!(
            DurableConversationRepository::open(&reopened)
                .unwrap()
                .events_after(&id, 0, 10)
                .unwrap()
                .len(),
            2
        );
    }

    #[test]
    fn domain_two_live_connections_reject_conflicting_publication_key() {
        use keith_state_store::EmbeddedStore;
        let root = tempfile::tempdir().unwrap();
        let path = root.path().join("shared.sqlite");
        let first_store = EmbeddedStore::open(&path, None).unwrap();
        let second_store = EmbeddedStore::open(&path, None).unwrap();
        let owner = profile(1);
        let id = ConversationId(EntityId::from_u128(91));
        ConversationRepository::new()
            .apply_durable(
                &first_store,
                &[
                    ConversationMutation::CreateConversation(conversation(
                        91,
                        std::slice::from_ref(&owner),
                    )),
                    ConversationMutation::AddParticipant(participant(id.clone(), owner)),
                ],
            )
            .unwrap();
        ConversationRepository::new()
            .apply_durable(
                &first_store,
                &[ConversationMutation::AppendEvent {
                    expected_revision: Revision::new(1),
                    event: event(id.clone(), 1, "cross-process-key"),
                    intents: AppendIntents::REQUIRED,
                }],
            )
            .unwrap();
        let mut conflicting = event(id, 2, "cross-process-key");
        conflicting.id = EventId(EntityId::from_u128(9_999));
        assert_eq!(
            ConversationRepository::new().apply_durable(
                &second_store,
                &[ConversationMutation::AppendEvent {
                    expected_revision: Revision::new(2),
                    event: conflicting,
                    intents: AppendIntents::REQUIRED,
                }],
            ),
            Err(RepositoryError::Conflict("publication key"))
        );
    }

    #[test]
    fn domain_canonical_encoding_rejects_unknown_fields_and_noncanonical_bytes() {
        let record = conversation(3, &[profile(1)]);
        let bytes = canonical_json(&record).unwrap();
        assert_eq!(
            decode_canonical::<ConversationRecord>(&bytes).unwrap(),
            record
        );
        let mut value: serde_json::Value = serde_json::from_slice(&bytes).unwrap();
        value
            .as_object_mut()
            .unwrap()
            .insert("agent_id".into(), serde_json::json!("forged"));
        assert!(
            decode_canonical::<ConversationRecord>(&serde_json::to_vec(&value).unwrap()).is_err()
        );
        let pretty = serde_json::to_vec_pretty(&record).unwrap();
        assert!(decode_canonical::<ConversationRecord>(&pretty).is_err());
    }

    #[test]
    fn domain_participants_receipts_grants_and_audit_are_revisioned() {
        let repository = ConversationRepository::new();
        let owner = profile(1);
        let record = conversation(4, std::slice::from_ref(&owner));
        let id = record.id.clone();
        repository
            .apply_atomic(&[
                ConversationMutation::CreateConversation(record),
                ConversationMutation::AddParticipant(ConversationParticipant {
                    schema_version: CURRENT_SCHEMA_VERSION,
                    conversation_id: id.clone(),
                    principal: ParticipantPrincipal::Agent(owner.clone()),
                    role: ParticipantRole::Owner,
                    joined_at: UtcTimestamp::from_unix_millis(10),
                    left_at: None,
                    revision: Revision::ZERO,
                    applied_through_sequence: 0,
                    hidden: false,
                    muted: false,
                    notification_policy: NotificationPolicy {
                        mentions_only: false,
                        muted: false,
                    },
                }),
                ConversationMutation::AppendEvent {
                    expected_revision: Revision::new(1),
                    event: event(id.clone(), 1, "receipt-source"),
                    intents: AppendIntents::REQUIRED,
                },
            ])
            .unwrap();
        let receipt = ReadReceipt {
            schema_version: CURRENT_SCHEMA_VERSION,
            conversation_id: id.clone(),
            reader: Principal::Agent(owner.clone()),
            read_through_sequence: 1,
            updated_at: UtcTimestamp::from_unix_millis(30),
            revision: Revision::ZERO,
        };
        let grant = SharedKnowledgeGrant {
            schema_version: CURRENT_SCHEMA_VERSION,
            id: GrantId(EntityId::from_u128(55)),
            resource_kind: SharedResourceKind::Conversation,
            resource_id: id.to_string(),
            grantor: Principal::Human,
            grantee: owner.clone(),
            purpose: "coordinate".into(),
            provenance: GrantProvenance {
                source_actor: Principal::Human,
                source_conversation_id: Some(id.clone()),
                source_event_ids: vec![],
            },
            resource_policy_revision: Revision::ZERO,
            deletion_policy: SharedDeletionPolicy::RetainUntilExplicitDelete,
            operations: BTreeSet::from([GrantOperation::Read]),
            created_at: UtcTimestamp::from_unix_millis(30),
            expires_at: None,
            revoked_at: None,
            revision: Revision::ZERO,
        };
        let audit = ConversationAuditRecord {
            schema_version: CURRENT_SCHEMA_VERSION,
            id: AuditId(EntityId::from_u128(56)),
            actor: Principal::Agent(owner.clone()),
            action: "read".into(),
            conversation_id: Some(id.clone()),
            event_id: None,
            correlation_key: "corr-1".into(),
            occurred_at: UtcTimestamp::from_unix_millis(30),
            outcome: "allowed".into(),
        };
        repository
            .apply_atomic(&[
                ConversationMutation::AdvanceRead {
                    expected_revision: None,
                    receipt,
                },
                ConversationMutation::PutGrant {
                    expected_revision: None,
                    grant: grant.clone(),
                },
                ConversationMutation::AppendAudit(audit),
            ])
            .unwrap();
        assert_eq!(
            repository
                .read_receipt(&id, &Principal::Agent(owner))
                .unwrap()
                .unwrap()
                .read_through_sequence,
            1
        );
        assert_eq!(repository.grant(&grant.id).unwrap(), Some(grant));
        assert_eq!(repository.audits().unwrap().len(), 1);
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn store_context_search_and_permanent_dm_survive_restart() {
        let durable = keith_state_store::EmbeddedStore::open_in_memory().unwrap();
        let owner = profile(91);
        let store = CanonicalConversationStore::open(&durable).unwrap();
        let initial = store
            .plan_permanent_human_dm(&owner, UtcTimestamp::from_unix_millis(1))
            .unwrap();
        assert!(!initial.already_committed);
        assert!(!initial.mutations.is_empty());
        let dm = store
            .provision_permanent_human_dm(&owner, UtcTimestamp::from_unix_millis(1))
            .unwrap();
        assert_eq!(
            store
                .provision_permanent_human_dm(&owner, UtcTimestamp::from_unix_millis(2))
                .unwrap()
                .id,
            dm.id
        );
        let committed = store
            .plan_permanent_human_dm(&owner, UtcTimestamp::from_unix_millis(2))
            .unwrap();
        assert!(committed.already_committed);
        assert!(committed.mutations.is_empty());
        let mut message = event(dm.id.clone(), 1, "permanent-dm-message");
        message.content = Some("bounded canonical context".into());
        message.author = Principal::Human;
        assert_eq!(
            store.append(dm.revision, &message).unwrap(),
            CanonicalAppendOutcome::Appended
        );
        assert_eq!(
            store.append(dm.revision, &message).unwrap(),
            CanonicalAppendOutcome::Replayed
        );
        let projection = store
            .projection(&dm.id, &Principal::Agent(owner.clone()), 0, 10)
            .unwrap();
        assert_eq!(projection.unread_count, 1);
        assert_eq!(projection.events.len(), 1);
        let context = store
            .reconstruct_context(
                &Principal::Agent(owner.clone()),
                ConversationContextCursor {
                    conversation_id: dm.id.clone(),
                    applied_through_sequence: 0,
                },
                10,
            )
            .unwrap();
        assert_eq!(context.cursor.applied_through_sequence, 1);
        assert_eq!(
            store
                .search(&Principal::Agent(owner.clone()), "canonical", 10)
                .unwrap()
                .len(),
            1
        );
        assert!(
            store
                .search(&Principal::Agent(profile(92)), "canonical", 10)
                .unwrap()
                .is_empty()
        );
        let mut edit = event(dm.id.clone(), 2, "edit-message");
        edit.author = Principal::Human;
        let mut forged_edit = edit.clone();
        forged_edit.author = Principal::Agent(owner.clone());
        assert!(
            store
                .edit_event(
                    &Principal::Agent(owner.clone()),
                    Revision::new(3),
                    forged_edit,
                    message.id.clone(),
                    "forged replacement".into()
                )
                .is_err()
        );
        store
            .edit_event(
                &Principal::Human,
                Revision::new(3),
                edit,
                message.id.clone(),
                "updated projection".into(),
            )
            .unwrap();
        assert!(
            store
                .search(&Principal::Agent(owner.clone()), "canonical", 10)
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            store
                .search(&Principal::Agent(owner.clone()), "updated", 10)
                .unwrap()
                .len(),
            1
        );
        let mut pin = event(dm.id.clone(), 3, "pin-message");
        pin.author = Principal::Human;
        store
            .set_event_pinned(
                &Principal::Human,
                Revision::new(4),
                pin,
                message.id.clone(),
                true,
            )
            .unwrap();
        assert!(
            store
                .projection(&dm.id, &Principal::Human, 0, 10)
                .unwrap()
                .pinned
        );
        let mut unpin = event(dm.id.clone(), 4, "unpin-message");
        unpin.author = Principal::Human;
        store
            .set_event_pinned(
                &Principal::Human,
                Revision::new(5),
                unpin,
                message.id.clone(),
                false,
            )
            .unwrap();
        assert!(
            !store
                .projection(&dm.id, &Principal::Human, 0, 10)
                .unwrap()
                .pinned
        );
        let mut redact = event(dm.id.clone(), 5, "redact-message");
        redact.author = Principal::Human;
        store
            .redact_event(
                &Principal::Human,
                Revision::new(6),
                redact,
                message.id.clone(),
            )
            .unwrap();
        assert!(
            store
                .search(&Principal::Agent(owner.clone()), "updated", 10)
                .unwrap()
                .is_empty()
        );
        let mut forged_attachment = event(dm.id.clone(), 6, "forged-attachment");
        forged_attachment.author = Principal::Agent(owner.clone());
        forged_attachment.artifacts.push(ArtifactReference {
            artifact_id: keith_agent_types::ArtifactId(EntityId::from_u128(707)),
            digest_sha256: "ab".repeat(32),
        });
        assert!(
            store
                .append_with_attachments(
                    &Principal::Agent(owner.clone()),
                    Revision::new(7),
                    &forged_attachment,
                    std::slice::from_ref(&message.id),
                    &DenyAttachments
                )
                .is_err()
        );
        assert_eq!(
            store
                .projection(&dm.id, &Principal::Human, 0, 10)
                .unwrap()
                .conversation
                .event_head
                .unwrap()
                .sequence,
            5
        );
        let mut human_attachment = forged_attachment.clone();
        human_attachment.author = Principal::Human;
        assert_eq!(
            store
                .append_with_attachments(
                    &Principal::Human,
                    Revision::new(7),
                    &human_attachment,
                    std::slice::from_ref(&message.id),
                    &AllowHumanAttachments
                )
                .unwrap(),
            CanonicalAppendOutcome::Appended
        );
        let space_id = EntityId::from_u128(808);
        let grant = SharedKnowledgeGrant {
            schema_version: CURRENT_SCHEMA_VERSION,
            id: GrantId(EntityId::from_u128(909)),
            resource_kind: SharedResourceKind::KnowledgeSpace,
            resource_id: space_id.to_string(),
            grantor: Principal::Human,
            grantee: owner.clone(),
            purpose: "authorized search".into(),
            provenance: GrantProvenance {
                source_actor: Principal::Human,
                source_conversation_id: Some(dm.id.clone()),
                source_event_ids: vec![],
            },
            resource_policy_revision: Revision::ZERO,
            deletion_policy: SharedDeletionPolicy::RetainUntilExplicitDelete,
            operations: BTreeSet::from([GrantOperation::Search]),
            created_at: UtcTimestamp::from_unix_millis(4),
            expires_at: Some(UtcTimestamp::from_unix_millis(10)),
            revoked_at: None,
            revision: Revision::ZERO,
        };
        store
            .repository
            .apply(&[ConversationMutation::PutGrant {
                expected_revision: None,
                grant: grant.clone(),
            }])
            .unwrap();
        let resolver = DurableConversationAccessResolver::open(&durable).unwrap();
        assert_eq!(
            resolver
                .authorize_grant(
                    &grant.id,
                    &space_id,
                    &owner,
                    keith_knowledge::KnowledgeOperation::Search,
                    UtcTimestamp::from_unix_millis(5)
                )
                .unwrap()
                .unwrap()
                .status,
            keith_retrieval::GrantAuthorizationStatus::Active
        );
        assert!(
            resolver
                .authorize_grant(
                    &grant.id,
                    &space_id,
                    &owner,
                    keith_knowledge::KnowledgeOperation::Read,
                    UtcTimestamp::from_unix_millis(5)
                )
                .unwrap()
                .is_none()
        );
        assert_eq!(
            resolver
                .authorize_grant(
                    &grant.id,
                    &space_id,
                    &owner,
                    keith_knowledge::KnowledgeOperation::Search,
                    UtcTimestamp::from_unix_millis(11)
                )
                .unwrap()
                .unwrap()
                .status,
            keith_retrieval::GrantAuthorizationStatus::Expired
        );
        let mut revoked = grant.clone();
        revoked.revoked_at = Some(UtcTimestamp::from_unix_millis(6));
        revoked.revision = Revision::new(1);
        store
            .repository
            .apply(&[ConversationMutation::PutGrant {
                expected_revision: Some(Revision::ZERO),
                grant: revoked,
            }])
            .unwrap();
        assert_eq!(
            resolver
                .authorize_grant(
                    &grant.id,
                    &space_id,
                    &owner,
                    keith_knowledge::KnowledgeOperation::Search,
                    UtcTimestamp::from_unix_millis(7)
                )
                .unwrap()
                .unwrap()
                .status,
            keith_retrieval::GrantAuthorizationStatus::Revoked
        );
        let mut departed = store
            .repository
            .participant(&dm.id, &ParticipantPrincipal::Agent(owner.clone()))
            .unwrap()
            .unwrap();
        departed.left_at = Some(UtcTimestamp::from_unix_millis(3));
        departed.revision = Revision::new(1);
        let mut leave = event(dm.id.clone(), 7, "leave-permanent-dm");
        leave.author = Principal::Human;
        leave.kind = ConversationEventKind::MembershipChange;
        leave.content = Some(
            serde_json::to_string(&MembershipEventPayload::Leave {
                actor: Principal::Human,
                participant: ParticipantPrincipal::Agent(owner.clone()),
            })
            .unwrap(),
        );
        assert!(
            store
                .change_membership(
                    &Principal::Human,
                    Revision::ZERO,
                    departed.clone(),
                    Revision::new(8),
                    leave
                )
                .is_err()
        );
        drop(store);
        let reopened = CanonicalConversationStore::open(&durable).unwrap();
        assert!(
            reopened
                .plan_permanent_human_dm(&profile(91), UtcTimestamp::from_unix_millis(9))
                .unwrap()
                .already_committed
        );
        assert_eq!(reopened.rebuild_projections().unwrap().len(), 1);
        assert!(
            !reopened
                .projection(&dm.id, &Principal::Human, 0, 10)
                .unwrap()
                .pinned
        );
        assert!(reopened.repository.conversation(&dm.id).unwrap().is_some());
    }

    #[test]
    fn store_permanent_dm_plan_rejects_partial_durable_state() {
        let durable = keith_state_store::EmbeddedStore::open_in_memory().unwrap();
        let owner = profile(93);
        let mutations = permanent_human_dm_mutations(&owner, UtcTimestamp::UNIX_EPOCH).unwrap();
        durable.transact(&[mutations[1].clone()]).unwrap();
        let store = CanonicalConversationStore::open(&durable).unwrap();
        assert!(
            store
                .plan_permanent_human_dm(&owner, UtcTimestamp::UNIX_EPOCH)
                .is_err()
        );
        assert!(
            durable
                .list_records(Collection::Conversations)
                .unwrap()
                .is_empty()
        );
    }

    #[test]
    fn store_authorization_observation_distinguishes_revoked_grant() {
        let durable = keith_state_store::EmbeddedStore::open_in_memory().unwrap();
        let owner = profile(193);
        let store = ConversationStore::open(&durable).unwrap();
        let dm = store
            .provision_permanent_human_dm(&owner, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let grant = SharedKnowledgeGrant {
            schema_version: CURRENT_SCHEMA_VERSION,
            id: GrantId(EntityId::from_u128(193_001)),
            resource_kind: SharedResourceKind::Conversation,
            resource_id: dm.id.to_string(),
            grantor: Principal::Human,
            grantee: owner.clone(),
            purpose: "cursor authorization evidence".into(),
            provenance: GrantProvenance {
                source_actor: Principal::Human,
                source_conversation_id: Some(dm.id.clone()),
                source_event_ids: Vec::new(),
            },
            resource_policy_revision: Revision::new(4),
            deletion_policy: SharedDeletionPolicy::RetainUntilExplicitDelete,
            operations: BTreeSet::from([GrantOperation::Read]),
            created_at: UtcTimestamp::from_unix_millis(1),
            expires_at: None,
            revoked_at: None,
            revision: Revision::ZERO,
        };
        store
            .repository
            .apply(&[ConversationMutation::PutGrant {
                expected_revision: None,
                grant: grant.clone(),
            }])
            .unwrap();
        let active = store
            .authorization_observation(&dm.id, &Principal::Agent(owner.clone()))
            .unwrap();
        assert_eq!(
            active.relevant_grant_revisions.get(&grant.id),
            Some(&Revision::ZERO)
        );
        assert_eq!(
            active.grant_evidence.get(&grant.id).unwrap().revoked_at,
            None
        );

        let revoked_at = UtcTimestamp::from_unix_millis(2);
        let mut revoked = grant.clone();
        revoked.revision = Revision::new(1);
        revoked.revoked_at = Some(revoked_at);
        store
            .repository
            .apply(&[ConversationMutation::PutGrant {
                expected_revision: Some(Revision::ZERO),
                grant: revoked,
            }])
            .unwrap();
        let observation = store
            .authorization_observation(&dm.id, &Principal::Agent(owner))
            .unwrap();
        assert!(!observation.relevant_grant_revisions.contains_key(&grant.id));
        let evidence = observation.grant_evidence.get(&grant.id).unwrap();
        assert_eq!(evidence.revision, Revision::new(1));
        assert_eq!(evidence.resource_policy_revision, Revision::new(4));
        assert_eq!(evidence.revoked_at, Some(revoked_at));
        assert_ne!(
            active.policy_digest_sha256,
            observation.policy_digest_sha256
        );
    }

    #[test]
    fn store_agent_dm_required_pair_membership_survives_restart() {
        let durable = keith_state_store::EmbeddedStore::open_in_memory().unwrap();
        let first = profile(94);
        let second = profile(95);
        let id = ConversationId(EntityId::from_u128(9495));
        let mut direct = conversation(9495, &[first.clone(), second.clone()]);
        direct.kind = ConversationKind::AgentAgentDm;
        direct.creator = Principal::Agent(first.clone());
        let owner = participant(id.clone(), first.clone());
        let member = participant(id.clone(), second.clone());
        let repository = ConversationRepository::new();
        repository
            .apply_durable(
                &durable,
                &[
                    ConversationMutation::CreateConversation(direct),
                    ConversationMutation::AddParticipant(owner),
                    ConversationMutation::AddParticipant(member.clone()),
                ],
            )
            .unwrap();
        let store = ConversationStore::open(&durable).unwrap();
        let mut departed = member;
        departed.left_at = Some(UtcTimestamp::from_unix_millis(20));
        departed.revision = Revision::new(1);
        let mut leave = event(id.clone(), 1, "leave-agent-dm");
        leave.author = Principal::Agent(first.clone());
        leave.kind = ConversationEventKind::MembershipChange;
        leave.content = Some(
            serde_json::to_string(&MembershipEventPayload::Leave {
                actor: Principal::Agent(first.clone()),
                participant: ParticipantPrincipal::Agent(second.clone()),
            })
            .unwrap(),
        );
        assert!(
            store
                .change_membership(
                    &Principal::Agent(first),
                    Revision::ZERO,
                    departed,
                    Revision::new(2),
                    leave
                )
                .is_err()
        );
        drop(store);
        let reopened = ConversationStore::open(&durable).unwrap();
        assert!(
            reopened
                .is_active_participant(&id, &Principal::Agent(second))
                .unwrap()
        );
    }

    #[test]
    fn store_restart_rejects_corrupt_permanent_dm_membership_rows() {
        for corruption in 0..3 {
            let durable = keith_state_store::EmbeddedStore::open_in_memory().unwrap();
            let owner = profile(96 + corruption);
            let store = ConversationStore::open(&durable).unwrap();
            let dm = store
                .provision_permanent_human_dm(&owner, UtcTimestamp::UNIX_EPOCH)
                .unwrap();
            drop(store);
            if corruption == 2 {
                let extra = participant(dm.id.clone(), profile(199));
                durable
                    .transact(&[RecordMutation::Put {
                        collection: Collection::ConversationParticipants,
                        record: VersionedRecord {
                            version: CURRENT_SCHEMA_VERSION,
                            id: compound_id(&dm.id.to_string(), &format!("{:?}", extra.principal)),
                            revision: Revision::ZERO,
                            updated_at: extra.joined_at,
                            payload: serde_json::to_value(extra).unwrap(),
                        },
                        precondition: WritePrecondition::Missing,
                    }])
                    .unwrap();
                assert!(ConversationStore::open(&durable).is_err());
                continue;
            }
            let id = compound_id(
                &dm.id.to_string(),
                &format!("{:?}", ParticipantPrincipal::Agent(owner)),
            );
            let mut stored = durable
                .get_record(Collection::ConversationParticipants, &id)
                .unwrap()
                .unwrap();
            let mut participant: ConversationParticipant =
                serde_json::from_value(stored.payload).unwrap();
            participant.revision = Revision::new(1);
            if corruption == 1 {
                participant.left_at = Some(UtcTimestamp::from_unix_millis(1));
            } else {
                participant.role = ParticipantRole::Observer;
            }
            stored.revision = participant.revision;
            stored.payload = serde_json::to_value(participant).unwrap();
            durable
                .transact(&[RecordMutation::Put {
                    collection: Collection::ConversationParticipants,
                    record: stored,
                    precondition: WritePrecondition::Exact(Revision::ZERO),
                }])
                .unwrap();
            assert!(ConversationStore::open(&durable).is_err());
        }
    }

    #[test]
    fn store_restart_rejects_corrupt_agent_dm_membership_rows() {
        for corruption in 0..3 {
            let durable = keith_state_store::EmbeddedStore::open_in_memory().unwrap();
            let first = profile(201 + corruption * 2);
            let second = profile(202 + corruption * 2);
            let id = ConversationId(EntityId::from_u128(20_100 + corruption));
            let mut direct = conversation(20_100 + corruption, &[first.clone(), second.clone()]);
            direct.kind = ConversationKind::AgentAgentDm;
            direct.creator = Principal::Agent(first.clone());
            ConversationRepository::new()
                .apply_durable(
                    &durable,
                    &[
                        ConversationMutation::CreateConversation(direct),
                        ConversationMutation::AddParticipant(participant(
                            id.clone(),
                            first.clone(),
                        )),
                        ConversationMutation::AddParticipant(participant(
                            id.clone(),
                            second.clone(),
                        )),
                    ],
                )
                .unwrap();
            ConversationStore::open(&durable).unwrap();
            if corruption == 2 {
                let mut extra = participant(id.clone(), profile(299));
                extra.principal = ParticipantPrincipal::Human;
                durable
                    .transact(&[RecordMutation::Put {
                        collection: Collection::ConversationParticipants,
                        record: VersionedRecord {
                            version: CURRENT_SCHEMA_VERSION,
                            id: compound_id(&id.to_string(), &format!("{:?}", extra.principal)),
                            revision: Revision::ZERO,
                            updated_at: extra.joined_at,
                            payload: serde_json::to_value(extra).unwrap(),
                        },
                        precondition: WritePrecondition::Missing,
                    }])
                    .unwrap();
                assert!(ConversationStore::open(&durable).is_err());
                continue;
            }
            let row_id = compound_id(
                &id.to_string(),
                &format!("{:?}", ParticipantPrincipal::Agent(second)),
            );
            let mut stored = durable
                .get_record(Collection::ConversationParticipants, &row_id)
                .unwrap()
                .unwrap();
            let mut row: ConversationParticipant = serde_json::from_value(stored.payload).unwrap();
            row.revision = Revision::new(1);
            if corruption == 1 {
                row.left_at = Some(UtcTimestamp::from_unix_millis(1));
            } else {
                row.role = ParticipantRole::Observer;
            }
            stored.revision = row.revision;
            stored.payload = serde_json::to_value(row).unwrap();
            durable
                .transact(&[RecordMutation::Put {
                    collection: Collection::ConversationParticipants,
                    record: stored,
                    precondition: WritePrecondition::Exact(Revision::ZERO),
                }])
                .unwrap();
            assert!(ConversationStore::open(&durable).is_err());
        }
    }
}
