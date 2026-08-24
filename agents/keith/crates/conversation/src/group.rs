use std::collections::BTreeSet;

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, ConversationId, EventId, ProfileId, Revision, SessionId, StableKey,
    UtcTimestamp,
};
use keith_state_store_core::{
    CanonicalConversationAppend, Collection, GroupMembershipRepository, GroupMembershipTransition,
    RecordMutation, VersionedRecord, WritePrecondition,
};
use keith_state_store_core::{CanonicalConversationRepository, StateRecordRepository};
use serde::{Deserialize, Serialize};

use super::*;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GroupMentionMode {
    AllActive,
    ExplicitOnly,
    ExplicitOrOwners,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GroupMentionPolicy {
    pub mode: GroupMentionMode,
    pub allow_human_trigger: bool,
}

impl Default for GroupMentionPolicy {
    fn default() -> Self {
        Self {
            mode: GroupMentionMode::ExplicitOnly,
            allow_human_trigger: true,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GroupMembershipAction {
    Join,
    Leave,
    Rejoin,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct CreateGroupRequest {
    pub operation_key: StableKey,
    pub title: String,
    pub creator: Principal,
    pub initial_profile_ids: BTreeSet<ProfileId>,
    pub mention_policy: GroupMentionPolicy,
    pub now: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ChangeGroupMembershipRequest {
    pub operation_key: StableKey,
    pub conversation_id: ConversationId,
    pub actor: Principal,
    pub target: ParticipantPrincipal,
    pub action: GroupMembershipAction,
    pub role: ParticipantRole,
    pub expected_conversation_revision: Revision,
    pub expected_participant_revision: Option<Revision>,
    pub now: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct UpdateGroupMentionPolicyRequest {
    pub operation_key: StableKey,
    pub conversation_id: ConversationId,
    pub actor: Principal,
    pub expected_conversation_revision: Revision,
    pub policy: GroupMentionPolicy,
    pub now: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum GroupMutationStatus {
    Applied,
    Duplicate,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GroupMutationReceipt {
    pub status: GroupMutationStatus,
    pub conversation_id: ConversationId,
    pub event_id: EventId,
    pub conversation_revision: Revision,
    pub participant_revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(tag = "type", rename_all = "snake_case", deny_unknown_fields)]
enum GroupEventPayload {
    MentionPolicyChanged { policy: GroupMentionPolicy },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GroupParticipantSession {
    pub profile_id: ProfileId,
    pub participant_revision: Revision,
    pub session_id: SessionId,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GroupTriggerSnapshot {
    pub conversation_id: ConversationId,
    pub trigger_event_id: EventId,
    pub trigger_sequence: u64,
    pub group_revision: Revision,
    pub policy_digest_sha256: String,
    pub eligible_participants: Vec<GroupParticipantSession>,
    pub explicit_mentions: BTreeSet<ProfileId>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuthorizedGroupSession {
    pub session_id: SessionId,
    pub authorization: ConversationAuthorizationObservation,
    pub projection: ConversationProjection,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GroupMentionPolicyObservation {
    pub policy: GroupMentionPolicy,
    pub source_event_id: EventId,
    pub source_sequence: u64,
    pub digest_sha256: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GroupAuthorityObservation {
    pub conversation_id: ConversationId,
    pub conversation_revision: Revision,
    pub participant_revision: Revision,
    pub authenticated_participant: ConversationParticipant,
    pub target_participant: Option<ConversationParticipant>,
    pub mention_policy: GroupMentionPolicyObservation,
}

pub trait GroupParticipantSessionResolver {
    fn resolve_session(
        &self,
        conversation_id: &ConversationId,
        profile_id: &ProfileId,
    ) -> Result<Option<SessionId>, RepositoryError>;

    fn authorize_session(
        &self,
        conversation_id: &ConversationId,
        profile_id: &ProfileId,
        session_id: &SessionId,
    ) -> Result<bool, RepositoryError>;
}

pub struct GroupService<'a, R: StateRecordRepository> {
    conversations: ConversationStore<'a, R>,
}

impl<'a, R> GroupService<'a, R>
where
    R: StateRecordRepository + CanonicalConversationRepository + GroupMembershipRepository,
{
    pub fn open(store: &'a R) -> Result<Self, RepositoryError> {
        Ok(Self {
            conversations: ConversationStore::open(store)?,
        })
    }

    pub fn create_group(
        &self,
        request: &CreateGroupRequest,
    ) -> Result<GroupMutationReceipt, RepositoryError> {
        let replay_content = policy_content(&request.mention_policy)?;
        if let Some(receipt) = self.replayed_receipt(
            &ConversationId(compound_id("group", request.operation_key.as_str())),
            &request.operation_key,
            &request.creator,
            ConversationEventKind::SystemNotice,
            &replay_content,
        )? {
            return Ok(receipt);
        }
        let conversation_id = ConversationId(compound_id("group", request.operation_key.as_str()));
        let mut profiles = request.initial_profile_ids.clone();
        if let Principal::Agent(profile_id) = &request.creator {
            profiles.insert(profile_id.clone());
        }
        if profiles.is_empty() {
            return Err(DomainError::Invalid("group requires at least one agent profile").into());
        }
        if profiles.len() > MAX_PARTICIPANTS {
            return Err(DomainError::BoundExceeded("participants").into());
        }
        let mut mutations = vec![ConversationMutation::CreateConversation(
            ConversationRecord {
                schema_version: CURRENT_SCHEMA_VERSION,
                id: conversation_id.clone(),
                kind: ConversationKind::Group,
                lifecycle: ConversationLifecycle::Active,
                title: request.title.clone(),
                creator: request.creator.clone(),
                created_at: request.now,
                updated_at: request.now,
                revision: Revision::ZERO,
                participant_revision: Revision::ZERO,
                participant_profiles: BTreeSet::new(),
                human_participant: false,
                event_head: None,
            },
        )];
        if request.creator == Principal::Human {
            mutations.push(ConversationMutation::AddParticipant(group_participant(
                conversation_id.clone(),
                ParticipantPrincipal::Human,
                ParticipantRole::Owner,
                request.now,
            )));
        }
        for profile_id in profiles {
            let role = if request.creator == Principal::Agent(profile_id.clone()) {
                ParticipantRole::Owner
            } else {
                ParticipantRole::Member
            };
            mutations.push(ConversationMutation::AddParticipant(group_participant(
                conversation_id.clone(),
                ParticipantPrincipal::Agent(profile_id),
                role,
                request.now,
            )));
        }
        let mut simulated = RepositoryState::default();
        for mutation in &mutations {
            simulated.apply(mutation.clone())?;
        }
        let expected_revision = simulated
            .conversations
            .get(&conversation_id)
            .ok_or(RepositoryError::NotFound("group"))?
            .revision;
        let event = group_policy_event(
            &conversation_id,
            &request.creator,
            &request.operation_key,
            1,
            &request.mention_policy,
            request.now,
        )?;
        mutations.push(ConversationMutation::AppendEvent {
            expected_revision,
            event: event.clone(),
            intents: AppendIntents::REQUIRED,
        });
        self.conversations.apply_command(&mutations)?;
        self.applied_receipt(&conversation_id, event.id)
    }

    pub fn change_membership(
        &self,
        request: &ChangeGroupMembershipRequest,
    ) -> Result<GroupMutationReceipt, RepositoryError> {
        let replay_payload = membership_payload(request);
        let replay_content = serde_json::to_string(&replay_payload)
            .map_err(|error| DomainError::Malformed(error.to_string()))?;
        if let Some(receipt) = self.replayed_receipt(
            &request.conversation_id,
            &request.operation_key,
            &request.actor,
            ConversationEventKind::MembershipChange,
            &replay_content,
        )? {
            return Ok(receipt);
        }
        let state = RepositoryState::load(self.conversations.repository.store)?;
        let conversation = require_group(&state, &request.conversation_id)?;
        require_owner(&state, &request.conversation_id, &request.actor)?;
        if conversation.revision != request.expected_conversation_revision {
            return Err(DomainError::Invalid("group conversation revision is stale").into());
        }
        let key = (request.conversation_id.clone(), request.target.clone());
        let current = state.participants.get(&key).cloned();
        let participant_exists = current.is_some();
        let (participant, payload) = match request.action {
            GroupMembershipAction::Join => {
                if current.is_some() || request.expected_participant_revision.is_some() {
                    return Err(DomainError::Invalid("group join target already exists").into());
                }
                (
                    group_participant(
                        request.conversation_id.clone(),
                        request.target.clone(),
                        request.role,
                        request.now,
                    ),
                    MembershipEventPayload::Join {
                        actor: request.actor.clone(),
                        participant: request.target.clone(),
                    },
                )
            }
            GroupMembershipAction::Leave | GroupMembershipAction::Rejoin => {
                let mut participant = current.ok_or(RepositoryError::NotFound("participant"))?;
                if Some(participant.revision) != request.expected_participant_revision {
                    return Err(DomainError::Invalid("group participant revision is stale").into());
                }
                participant.revision = participant
                    .revision
                    .checked_next()
                    .ok_or(DomainError::Invalid("participant revision overflow"))?;
                let payload = if request.action == GroupMembershipAction::Leave {
                    if participant.left_at.is_some() {
                        return Err(DomainError::Invalid("participant already left group").into());
                    }
                    participant.left_at = Some(request.now);
                    MembershipEventPayload::Leave {
                        actor: request.actor.clone(),
                        participant: request.target.clone(),
                    }
                } else {
                    if participant.left_at.is_none() {
                        return Err(DomainError::Invalid("participant is already active").into());
                    }
                    participant.left_at = None;
                    participant.joined_at = request.now;
                    participant.role = request.role;
                    MembershipEventPayload::Rejoin {
                        actor: request.actor.clone(),
                        participant: request.target.clone(),
                        expected_revision: request
                            .expected_participant_revision
                            .ok_or(DomainError::Invalid("rejoin requires participant revision"))?,
                    }
                };
                (participant, payload)
            }
        };
        let participant_mutation = if !participant_exists {
            ConversationMutation::AddParticipant(participant.clone())
        } else {
            ConversationMutation::UpdateParticipant {
                expected_revision: request
                    .expected_participant_revision
                    .ok_or(DomainError::Invalid("participant revision is required"))?,
                participant: participant.clone(),
            }
        };
        let mut simulated = state.clone();
        simulated.apply(participant_mutation.clone())?;
        let expected_event_revision = simulated
            .conversations
            .get(&request.conversation_id)
            .ok_or(RepositoryError::NotFound("group"))?
            .revision;
        let sequence = conversation
            .event_head
            .as_ref()
            .map_or(1, |head| head.sequence + 1);
        let event = ConversationEvent {
            schema_version: CURRENT_SCHEMA_VERSION,
            id: EventId(compound_id(
                "group-membership-event",
                request.operation_key.as_str(),
            )),
            conversation_id: request.conversation_id.clone(),
            sequence,
            publication_key: request.operation_key.clone(),
            author: request.actor.clone(),
            timestamp: request.now,
            kind: ConversationEventKind::MembershipChange,
            content: Some(
                serde_json::to_string(&payload)
                    .map_err(|error| DomainError::Malformed(error.to_string()))?,
            ),
            artifacts: Vec::new(),
            reply_to: None,
            thread_parent: None,
            provenance: EventProvenance {
                source: "group_membership".into(),
                source_ids: Vec::new(),
                migration_version: None,
            },
        };
        let _ = expected_event_revision;
        self.transition_membership(
            conversation,
            participant,
            request.expected_participant_revision,
            !participant_exists,
            &event,
        )?;
        self.applied_receipt(&request.conversation_id, event.id)
    }

    pub fn update_mention_policy(
        &self,
        request: &UpdateGroupMentionPolicyRequest,
    ) -> Result<GroupMutationReceipt, RepositoryError> {
        let replay_content = policy_content(&request.policy)?;
        if let Some(receipt) = self.replayed_receipt(
            &request.conversation_id,
            &request.operation_key,
            &request.actor,
            ConversationEventKind::SystemNotice,
            &replay_content,
        )? {
            return Ok(receipt);
        }
        let state = RepositoryState::load(self.conversations.repository.store)?;
        let conversation = require_group(&state, &request.conversation_id)?;
        require_owner(&state, &request.conversation_id, &request.actor)?;
        if conversation.revision != request.expected_conversation_revision {
            return Err(DomainError::Invalid("group conversation revision is stale").into());
        }
        let sequence = conversation
            .event_head
            .as_ref()
            .map_or(1, |head| head.sequence + 1);
        let event = group_policy_event(
            &request.conversation_id,
            &request.actor,
            &request.operation_key,
            sequence,
            &request.policy,
            request.now,
        )?;
        self.conversations
            .append(request.expected_conversation_revision, &event)?;
        self.applied_receipt(&request.conversation_id, event.id)
    }

    pub fn authorized_session_transcript<A: GroupParticipantSessionResolver>(
        &self,
        conversation_id: &ConversationId,
        profile_id: &ProfileId,
        session_id: &SessionId,
        after_sequence: u64,
        limit: usize,
        sessions: &A,
    ) -> Result<AuthorizedGroupSession, RepositoryError> {
        let state = RepositoryState::load(self.conversations.repository.store)?;
        require_group(&state, conversation_id)?;
        if !sessions.authorize_session(conversation_id, profile_id, session_id)?
            || !self
                .conversations
                .is_active_participant(conversation_id, &Principal::Agent(profile_id.clone()))?
        {
            return Err(DomainError::Invalid("group participant session is unauthorized").into());
        }
        let principal = Principal::Agent(profile_id.clone());
        Ok(AuthorizedGroupSession {
            session_id: session_id.clone(),
            authorization: self
                .conversations
                .authorization_observation(conversation_id, &principal)?,
            projection: self.conversations.projection(
                conversation_id,
                &principal,
                after_sequence,
                limit,
            )?,
        })
    }

    pub fn transcript(
        &self,
        conversation_id: &ConversationId,
        viewer: &Principal,
        after_sequence: u64,
        limit: usize,
    ) -> Result<ConversationProjection, RepositoryError> {
        let state = RepositoryState::load(self.conversations.repository.store)?;
        require_group(&state, conversation_id)?;
        self.conversations
            .projection(conversation_id, viewer, after_sequence, limit)
    }

    pub fn authority_observation(
        &self,
        conversation_id: &ConversationId,
        authenticated: &Principal,
        target: Option<&ParticipantPrincipal>,
    ) -> Result<GroupAuthorityObservation, RepositoryError> {
        let state = RepositoryState::load(self.conversations.repository.store)?;
        let conversation = require_group(&state, conversation_id)?;
        let authenticated_principal = match authenticated {
            Principal::Human => ParticipantPrincipal::Human,
            Principal::Agent(profile_id) => ParticipantPrincipal::Agent(profile_id.clone()),
            Principal::System => {
                return Err(
                    DomainError::Invalid("system has no group participant authority").into(),
                );
            }
        };
        let authenticated_participant = state
            .participants
            .get(&(conversation_id.clone(), authenticated_principal))
            .filter(|participant| participant.left_at.is_none())
            .cloned()
            .ok_or(DomainError::Invalid(
                "authenticated principal is not an active group member",
            ))?;
        let target_participant = target
            .map(|principal| {
                state
                    .participants
                    .get(&(conversation_id.clone(), principal.clone()))
                    .filter(|participant| participant.left_at.is_none())
                    .cloned()
                    .ok_or_else(|| {
                        RepositoryError::from(DomainError::Invalid(
                            "target is not an active group member",
                        ))
                    })
            })
            .transpose()?;
        Ok(GroupAuthorityObservation {
            conversation_id: conversation_id.clone(),
            conversation_revision: conversation.revision,
            participant_revision: conversation.participant_revision,
            authenticated_participant,
            target_participant,
            mention_policy: self.current_mention_policy(conversation_id)?,
        })
    }

    pub fn trigger_snapshot<A: GroupParticipantSessionResolver>(
        &self,
        conversation_id: &ConversationId,
        trigger_event_id: &EventId,
        explicit_mentions: &BTreeSet<ProfileId>,
        sessions: &A,
    ) -> Result<GroupTriggerSnapshot, RepositoryError> {
        let state = RepositoryState::load(self.conversations.repository.store)?;
        let conversation = require_group(&state, conversation_id)?;
        let events = self
            .conversations
            .repository
            .events_after(conversation_id, 0, usize::MAX)?;
        let trigger = events
            .iter()
            .find(|event| &event.id == trigger_event_id)
            .ok_or(RepositoryError::NotFound("group trigger event"))?;
        let policy = events
            .iter()
            .rev()
            .filter(|event| event.kind == ConversationEventKind::SystemNotice)
            .filter_map(|event| event.content.as_deref())
            .find_map(|content| {
                serde_json::from_str::<GroupEventPayload>(content)
                    .ok()
                    .map(|payload| match payload {
                        GroupEventPayload::MentionPolicyChanged { policy } => policy,
                    })
            })
            .ok_or(DomainError::Invalid("group mention policy is missing"))?;
        let active = state
            .participants
            .iter()
            .filter_map(|((id, principal), participant)| {
                if id != conversation_id
                    || participant.left_at.is_some()
                    || participant.role == ParticipantRole::Observer
                {
                    return None;
                }
                let ParticipantPrincipal::Agent(profile_id) = principal else {
                    return None;
                };
                Some((profile_id.clone(), participant))
            })
            .collect::<Vec<_>>();
        if explicit_mentions
            .iter()
            .any(|profile_id| !active.iter().any(|(active, _)| active == profile_id))
        {
            return Err(DomainError::Invalid("explicit mention names a nonparticipant").into());
        }
        let selected = active
            .into_iter()
            .filter(|(profile_id, participant)| match policy.mode {
                GroupMentionMode::AllActive => true,
                GroupMentionMode::ExplicitOnly => explicit_mentions.contains(profile_id),
                GroupMentionMode::ExplicitOrOwners => {
                    explicit_mentions.contains(profile_id)
                        || participant.role == ParticipantRole::Owner
                }
            });
        let mut eligible_participants = Vec::new();
        for (profile_id, participant) in selected {
            let session_id = sessions
                .resolve_session(conversation_id, &profile_id)?
                .ok_or(RepositoryError::NotFound("group participant session"))?;
            if !sessions.authorize_session(conversation_id, &profile_id, &session_id)? {
                return Err(DomainError::Invalid(
                    "resolved group participant session is unauthorized",
                )
                .into());
            }
            eligible_participants.push(GroupParticipantSession {
                profile_id,
                participant_revision: participant.revision,
                session_id,
            });
        }
        let policy_bytes = keith_agent_types::canonical_json_bytes(&policy)
            .map_err(|error| DomainError::Malformed(error.to_string()))?;
        Ok(GroupTriggerSnapshot {
            conversation_id: conversation_id.clone(),
            trigger_event_id: trigger_event_id.clone(),
            trigger_sequence: trigger.sequence,
            group_revision: conversation.revision,
            policy_digest_sha256: hex_sha256(&policy_bytes),
            eligible_participants,
            explicit_mentions: explicit_mentions.clone(),
        })
    }

    fn replayed_receipt(
        &self,
        conversation_id: &ConversationId,
        operation_key: &StableKey,
        expected_author: &Principal,
        expected_kind: ConversationEventKind,
        expected_content: &str,
    ) -> Result<Option<GroupMutationReceipt>, RepositoryError> {
        for event in self
            .conversations
            .repository
            .events_after(conversation_id, 0, usize::MAX)?
        {
            if &event.publication_key == operation_key {
                if &event.author != expected_author
                    || event.kind != expected_kind
                    || event.content.as_deref() != Some(expected_content)
                {
                    return Err(DomainError::Invalid(
                        "group operation key was reused with different input",
                    )
                    .into());
                }
                let mut receipt = self.applied_receipt(conversation_id, event.id)?;
                receipt.status = GroupMutationStatus::Duplicate;
                return Ok(Some(receipt));
            }
        }
        Ok(None)
    }

    fn applied_receipt(
        &self,
        conversation_id: &ConversationId,
        event_id: EventId,
    ) -> Result<GroupMutationReceipt, RepositoryError> {
        let state = RepositoryState::load(self.conversations.repository.store)?;
        let conversation = require_group(&state, conversation_id)?;
        Ok(GroupMutationReceipt {
            status: GroupMutationStatus::Applied,
            conversation_id: conversation_id.clone(),
            event_id,
            conversation_revision: conversation.revision,
            participant_revision: conversation.participant_revision,
        })
    }

    fn current_mention_policy(
        &self,
        conversation_id: &ConversationId,
    ) -> Result<GroupMentionPolicyObservation, RepositoryError> {
        self.conversations
            .repository
            .events_after(conversation_id, 0, usize::MAX)?
            .into_iter()
            .rev()
            .filter(|event| event.kind == ConversationEventKind::SystemNotice)
            .find_map(|event| {
                let content = event.content.as_deref()?;
                let GroupEventPayload::MentionPolicyChanged { policy } =
                    serde_json::from_str(content).ok()?;
                let bytes = keith_agent_types::canonical_json_bytes(&policy).ok()?;
                Some(GroupMentionPolicyObservation {
                    policy,
                    source_event_id: event.id,
                    source_sequence: event.sequence,
                    digest_sha256: hex_sha256(&bytes),
                })
            })
            .ok_or(DomainError::Invalid("group mention policy is missing").into())
    }

    fn transition_membership(
        &self,
        conversation: &ConversationRecord,
        participant: ConversationParticipant,
        expected_participant_row_revision: Option<Revision>,
        joining: bool,
        event: &ConversationEvent,
    ) -> Result<(), RepositoryError> {
        let next_revision = conversation
            .revision
            .checked_next()
            .ok_or(DomainError::Invalid("conversation revision overflow"))?;
        let next_participant_revision = conversation
            .participant_revision
            .checked_next()
            .ok_or(DomainError::Invalid("participant revision overflow"))?;
        let mut head = conversation.clone();
        head.revision = next_revision;
        head.participant_revision = next_participant_revision;
        head.updated_at = event.timestamp;
        head.event_head = Some(EventHead {
            sequence: event.sequence,
            event_id: event.id.clone(),
        });
        match &participant.principal {
            ParticipantPrincipal::Human => head.human_participant = participant.left_at.is_none(),
            ParticipantPrincipal::Agent(profile_id) => {
                if participant.left_at.is_none() {
                    head.participant_profiles.insert(profile_id.clone());
                } else {
                    head.participant_profiles.remove(profile_id);
                }
            }
        }
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
        let digest_bytes = keith_agent_types::canonical_json_bytes(&event_record)
            .map_err(|error| DomainError::Malformed(error.to_string()))?;
        let intent_payload = serde_json::json!({
            "conversation_id": event.conversation_id,
            "event_id": event.id,
            "sequence": event.sequence,
            "publication_key": event.publication_key,
        });
        let intents = [
            Collection::ConversationProjectionIntents,
            Collection::ConversationUnreadIntents,
            Collection::ConversationSearchIntents,
            Collection::ConversationPublicationIntents,
        ]
        .into_iter()
        .map(|collection| RecordMutation::Put {
            collection,
            record: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: compound_id(collection.as_str(), &event.id.to_string()),
                revision: Revision::ZERO,
                updated_at: event.timestamp,
                payload: intent_payload.clone(),
            },
            precondition: WritePrecondition::Missing,
        })
        .collect();
        let transition = GroupMembershipTransition {
            canonical_append: CanonicalConversationAppend {
                conversation_id: event.conversation_id.as_entity_id().clone(),
                expected_head_revision: conversation.revision,
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
                stable_key: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: publication_sentinel_id(&event.publication_key),
                    revision: Revision::ZERO,
                    updated_at: event.timestamp,
                    payload: serde_json::json!({
                        "event_id": event.id,
                        "event_digest": hex_sha256(&digest_bytes),
                    }),
                },
                intents,
            },
            expected_participant_revision: conversation.participant_revision,
            participant_mutations: vec![RecordMutation::Put {
                collection: Collection::ConversationParticipants,
                record: VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: compound_id(
                        &participant.conversation_id.to_string(),
                        &format!("{:?}", participant.principal),
                    ),
                    revision: participant.revision,
                    updated_at: event.timestamp,
                    payload: serde_json::to_value(participant)
                        .map_err(|error| DomainError::Malformed(error.to_string()))?,
                },
                precondition: if joining {
                    WritePrecondition::Missing
                } else {
                    WritePrecondition::Exact(
                        expected_participant_row_revision
                            .ok_or(DomainError::Invalid("participant revision is required"))?,
                    )
                },
            }],
        };
        self.conversations
            .repository
            .store
            .transition_group_membership(&transition)
            .map_err(|error| RepositoryError::Durable(error.to_string()))?;
        Ok(())
    }
}

fn require_group<'a>(
    state: &'a RepositoryState,
    conversation_id: &ConversationId,
) -> Result<&'a ConversationRecord, RepositoryError> {
    let conversation = state
        .conversations
        .get(conversation_id)
        .ok_or(RepositoryError::NotFound("group"))?;
    if conversation.kind != ConversationKind::Group {
        return Err(DomainError::Invalid("conversation is not a group").into());
    }
    Ok(conversation)
}

fn group_participant(
    conversation_id: ConversationId,
    principal: ParticipantPrincipal,
    role: ParticipantRole,
    now: UtcTimestamp,
) -> ConversationParticipant {
    ConversationParticipant {
        schema_version: CURRENT_SCHEMA_VERSION,
        conversation_id,
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
    }
}

fn group_policy_event(
    conversation_id: &ConversationId,
    actor: &Principal,
    operation_key: &StableKey,
    sequence: u64,
    policy: &GroupMentionPolicy,
    now: UtcTimestamp,
) -> Result<ConversationEvent, RepositoryError> {
    Ok(ConversationEvent {
        schema_version: CURRENT_SCHEMA_VERSION,
        id: EventId(compound_id("group-policy-event", operation_key.as_str())),
        conversation_id: conversation_id.clone(),
        sequence,
        publication_key: operation_key.clone(),
        author: actor.clone(),
        timestamp: now,
        kind: ConversationEventKind::SystemNotice,
        content: Some(
            serde_json::to_string(&GroupEventPayload::MentionPolicyChanged {
                policy: policy.clone(),
            })
            .map_err(|error| DomainError::Malformed(error.to_string()))?,
        ),
        artifacts: Vec::new(),
        reply_to: None,
        thread_parent: None,
        provenance: EventProvenance {
            source: "group_policy".into(),
            source_ids: Vec::new(),
            migration_version: None,
        },
    })
}

fn policy_content(policy: &GroupMentionPolicy) -> Result<String, RepositoryError> {
    serde_json::to_string(&GroupEventPayload::MentionPolicyChanged {
        policy: policy.clone(),
    })
    .map_err(|error| DomainError::Malformed(error.to_string()).into())
}

fn membership_payload(request: &ChangeGroupMembershipRequest) -> MembershipEventPayload {
    match request.action {
        GroupMembershipAction::Join => MembershipEventPayload::Join {
            actor: request.actor.clone(),
            participant: request.target.clone(),
        },
        GroupMembershipAction::Leave => MembershipEventPayload::Leave {
            actor: request.actor.clone(),
            participant: request.target.clone(),
        },
        GroupMembershipAction::Rejoin => MembershipEventPayload::Rejoin {
            actor: request.actor.clone(),
            participant: request.target.clone(),
            expected_revision: request
                .expected_participant_revision
                .unwrap_or(Revision::ZERO),
        },
    }
}
