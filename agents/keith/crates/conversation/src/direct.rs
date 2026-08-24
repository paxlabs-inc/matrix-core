use std::collections::BTreeSet;

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, ConversationId, DeliveryId, EventId, ProfileId, Revision,
    SchemaVersion, SessionId, StableKey, UtcTimestamp,
};
use keith_state_store_core::{
    CanonicalConversationAppend, CanonicalConversationRepository,
    CanonicalDirectConversationBinding, CanonicalDirectConversationOutcome, Collection,
    DirectConversationRepository, RecordMutation, StateRecordRepository, VersionedRecord,
    WritePrecondition,
};
use serde::{Deserialize, Serialize};

use super::{
    AppendIntents, ConversationEvent, ConversationEventKind, ConversationKind,
    ConversationLifecycle, ConversationMutation, ConversationParticipant, ConversationRecord,
    DurableEventRecord, EventProvenance, NotificationPolicy, ParticipantPrincipal, ParticipantRole,
    Principal, RepositoryError, RepositoryState, compound_id, durable_mutations, hex_sha256,
    publication_sentinel_id, put,
};

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HumanAgentDmKey {
    pub version: SchemaVersion,
    pub stable_key: StableKey,
    pub profile_id: ProfileId,
    pub conversation_id: ConversationId,
}

impl HumanAgentDmKey {
    pub fn new(
        profile_id: &ProfileId,
        conversation_id: &ConversationId,
    ) -> Result<Self, RepositoryError> {
        Ok(Self {
            version: CURRENT_SCHEMA_VERSION,
            stable_key: StableKey::parse(format!("human-dm:{profile_id}"))
                .map_err(|error| RepositoryError::Durable(error.to_string()))?,
            profile_id: profile_id.clone(),
            conversation_id: conversation_id.clone(),
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentPairKey {
    pub version: SchemaVersion,
    pub stable_key: StableKey,
    pub first_profile_id: ProfileId,
    pub second_profile_id: ProfileId,
    pub conversation_id: ConversationId,
}

impl AgentPairKey {
    pub fn new(first: &ProfileId, second: &ProfileId) -> Result<Self, RepositoryError> {
        if first == second {
            return Err(RepositoryError::Conflict(
                "agent DM requires distinct profiles",
            ));
        }
        let (first_profile_id, second_profile_id) = if first < second {
            (first.clone(), second.clone())
        } else {
            (second.clone(), first.clone())
        };
        let stable_key =
            StableKey::parse(format!("agent-dm:{first_profile_id}:{second_profile_id}"))
                .map_err(|error| RepositoryError::Durable(error.to_string()))?;
        let conversation_id = ConversationId::from(compound_id("agent-dm", stable_key.as_str()));
        Ok(Self {
            version: CURRENT_SCHEMA_VERSION,
            stable_key,
            first_profile_id,
            second_profile_id,
            conversation_id,
        })
    }

    pub fn contains(&self, profile_id: &ProfileId) -> bool {
        &self.first_profile_id == profile_id || &self.second_profile_id == profile_id
    }
}

#[derive(Clone, Debug, PartialEq)]
pub struct DirectMessagePlan {
    pub conversation_id: ConversationId,
    pub mutations: Vec<RecordMutation>,
    pub already_committed: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PeerMessageRequest {
    pub idempotency_key: StableKey,
    pub conversation_id: ConversationId,
    pub sender_profile_id: ProfileId,
    pub recipient_profile_id: ProfileId,
    pub participant_session_id: SessionId,
    pub policy_snapshot_key: StableKey,
    pub content: String,
    pub timestamp: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PeerMessageReceiptStatus {
    Accepted,
    Duplicate,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PeerMessageReceipt {
    pub status: PeerMessageReceiptStatus,
    pub delivery_id: DeliveryId,
    pub stable_source_key: StableKey,
    pub conversation_id: ConversationId,
    pub source_event_id: EventId,
    pub sender_profile_id: ProfileId,
    pub recipient_profile_id: ProfileId,
    pub participant_session_id: SessionId,
    pub context_cursor: super::ConversationContextCursor,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum DirectMessageViewer {
    HumanOwner,
    Agent(ProfileId),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct DirectMessageProjection {
    pub conversation: ConversationRecord,
    pub participants: Vec<ConversationParticipant>,
    pub events: Vec<ConversationEvent>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct PendingPeerDelivery {
    version: SchemaVersion,
    id: DeliveryId,
    stable_source_key: String,
    conversation_id: ConversationId,
    source_event_id: EventId,
    source_profile_id: ProfileId,
    destination_profile_id: ProfileId,
    participant_session_id: SessionId,
    policy_snapshot_key: String,
    state: PendingDeliveryState,
    attempt_count: u32,
    last_claim_fence: u64,
    claim: Option<serde_json::Value>,
    retry_at: Option<UtcTimestamp>,
    safe_error: Option<String>,
    revision: Revision,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
enum PendingDeliveryState {
    Pending,
}

pub struct PermanentDirectMessageService<'a, R: StateRecordRepository> {
    store: &'a R,
}

impl<'a, R: StateRecordRepository> PermanentDirectMessageService<'a, R> {
    pub const fn new(store: &'a R) -> Self {
        Self { store }
    }

    pub fn plan_agent_dm(
        &self,
        first: &ProfileId,
        second: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<DirectMessagePlan, RepositoryError> {
        let key = AgentPairKey::new(first, second)?;
        if let Some(stored) = stored_agent_pair_key(self.store, &key)? {
            verify_agent_dm(&RepositoryState::load(self.store)?, &stored)?;
            return Ok(DirectMessagePlan {
                conversation_id: stored.conversation_id,
                mutations: Vec::new(),
                already_committed: true,
            });
        }
        let before = RepositoryState::load(self.store)?;
        if before.conversations.contains_key(&key.conversation_id) {
            return Err(RepositoryError::Conflict(
                "agent DM is missing its pair key",
            ));
        }
        let logical = agent_dm_mutations(&key, now);
        let mut after = before.clone();
        for mutation in &logical {
            after.apply(mutation.clone())?;
        }
        after.validate_consistency()?;
        let mut mutations = durable_mutations(&before, &after, &logical)?;
        mutations.push(agent_pair_key_mutation(&key, now)?);
        Ok(DirectMessagePlan {
            conversation_id: key.conversation_id,
            mutations,
            already_committed: false,
        })
    }

    pub fn get_or_create_agent_dm(
        &self,
        first: &ProfileId,
        second: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ConversationRecord, RepositoryError>
    where
        R: DirectConversationRepository,
    {
        let plan = self.plan_agent_dm(first, second, now)?;
        if !plan.already_committed {
            let binding = direct_binding(&plan.mutations)?;
            match self
                .store
                .bind_direct_conversation(&binding)
                .map_err(|error| RepositoryError::Durable(error.to_string()))?
            {
                CanonicalDirectConversationOutcome::Applied { .. }
                | CanonicalDirectConversationOutcome::Replay { .. } => {}
            }
        }
        RepositoryState::load(self.store)?
            .conversations
            .get(&plan.conversation_id)
            .cloned()
            .ok_or(RepositoryError::NotFound("agent DM"))
    }

    pub fn send_peer_message(
        &self,
        request: &PeerMessageRequest,
    ) -> Result<PeerMessageReceipt, RepositoryError>
    where
        R: CanonicalConversationRepository,
    {
        let state = RepositoryState::load(self.store)?;
        let pair = AgentPairKey::new(&request.sender_profile_id, &request.recipient_profile_id)?;
        if pair.conversation_id != request.conversation_id {
            return Err(RepositoryError::Conflict(
                "peer message conversation binding",
            ));
        }
        let stored_pair = stored_agent_pair_key(self.store, &pair)?
            .ok_or(RepositoryError::NotFound("agent pair key"))?;
        verify_agent_dm(&state, &stored_pair)?;
        let conversation = state
            .conversations
            .get(&request.conversation_id)
            .ok_or(RepositoryError::NotFound("agent DM"))?;
        if let Some(existing) = state.publication_keys.get(&request.idempotency_key) {
            let expected_event_id = EventId::from(compound_id(
                "peer-source-event",
                request.idempotency_key.as_str(),
            ));
            let delivery_id = DeliveryId::from(compound_id(
                "peer-delivery",
                request.idempotency_key.as_str(),
            ));
            let exact = existing.id == expected_event_id
                && existing.conversation_id == request.conversation_id
                && existing.author == Principal::Agent(request.sender_profile_id.clone())
                && existing.kind == ConversationEventKind::Message
                && existing.content.as_deref() == Some(request.content.as_str())
                && existing.provenance.source == "peer_message"
                && existing.provenance.source_ids == vec![request.idempotency_key.to_string()]
                && delivery_matches(self.store, &request.idempotency_key, &delivery_id)?;
            if !exact {
                return Err(RepositoryError::Conflict("peer message idempotency key"));
            }
            return Ok(peer_receipt(
                request,
                delivery_id,
                expected_event_id,
                existing.sequence,
                PeerMessageReceiptStatus::Duplicate,
            ));
        }
        let sequence = conversation
            .event_head
            .as_ref()
            .map_or(1, |head| head.sequence + 1);
        let expected_head_revision = conversation.revision;
        let event_id = EventId::from(compound_id(
            "peer-source-event",
            request.idempotency_key.as_str(),
        ));
        let delivery_id = DeliveryId::from(compound_id(
            "peer-delivery",
            request.idempotency_key.as_str(),
        ));
        let event = ConversationEvent {
            schema_version: CURRENT_SCHEMA_VERSION,
            id: event_id.clone(),
            conversation_id: request.conversation_id.clone(),
            sequence,
            publication_key: request.idempotency_key.clone(),
            author: Principal::Agent(request.sender_profile_id.clone()),
            timestamp: request.timestamp,
            kind: ConversationEventKind::Message,
            content: Some(request.content.clone()),
            artifacts: Vec::new(),
            reply_to: None,
            thread_parent: None,
            provenance: EventProvenance {
                source: "peer_message".into(),
                source_ids: vec![request.idempotency_key.to_string()],
                migration_version: None,
            },
        };
        let mut candidate = state;
        candidate.append_event(
            expected_head_revision,
            event.clone(),
            AppendIntents::REQUIRED,
        )?;
        let head = candidate
            .conversations
            .get(&event.conversation_id)
            .ok_or(RepositoryError::NotFound("agent DM"))?;
        let event_record = VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id: event.id.as_entity_id().clone(),
            revision: Revision::ZERO,
            updated_at: event.timestamp,
            payload: serde_json::to_value(DurableEventRecord::Event {
                event: Box::new(event.clone()),
            })
            .map_err(|error| RepositoryError::Durable(error.to_string()))?,
        };
        let stable_bytes = keith_agent_types::canonical_json_bytes(&event_record)
            .map_err(|error| RepositoryError::Durable(error.to_string()))?;
        let stable_key_record = VersionedRecord {
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
        let delivery = pending_delivery(request, delivery_id.clone(), event_id.clone());
        put(
            &mut intents,
            Collection::ConversationDeliveries,
            delivery_id.as_entity_id().clone(),
            Revision::ZERO,
            request.timestamp,
            &delivery,
            WritePrecondition::Missing,
        )?;
        let append = CanonicalConversationAppend {
            conversation_id: event.conversation_id.as_entity_id().clone(),
            expected_head_revision,
            expected_next_sequence: event.sequence,
            event: event_record,
            head: VersionedRecord {
                version: CURRENT_SCHEMA_VERSION,
                id: head.id.as_entity_id().clone(),
                revision: head.revision,
                updated_at: head.updated_at,
                payload: serde_json::to_value(head)
                    .map_err(|error| RepositoryError::Durable(error.to_string()))?,
            },
            stable_key: stable_key_record,
            intents,
        };
        let status = match self
            .store
            .append_canonical_conversation(&append)
            .map_err(|error| RepositoryError::Durable(error.to_string()))?
        {
            keith_state_store_core::CanonicalAppendOutcome::Applied { .. } => {
                PeerMessageReceiptStatus::Accepted
            }
            keith_state_store_core::CanonicalAppendOutcome::Replay { .. } => {
                PeerMessageReceiptStatus::Duplicate
            }
        };
        Ok(peer_receipt(
            request,
            delivery_id,
            event_id,
            sequence,
            status,
        ))
    }

    pub fn projection(
        &self,
        conversation_id: &ConversationId,
        viewer: &DirectMessageViewer,
    ) -> Result<DirectMessageProjection, RepositoryError> {
        let state = RepositoryState::load(self.store)?;
        let conversation = state
            .conversations
            .get(conversation_id)
            .filter(|record| {
                matches!(
                    record.kind,
                    ConversationKind::HumanAgentDm | ConversationKind::AgentAgentDm
                )
            })
            .cloned()
            .ok_or(RepositoryError::NotFound("direct message"))?;
        if let DirectMessageViewer::Agent(profile_id) = viewer {
            let active = state
                .participants
                .get(&(
                    conversation_id.clone(),
                    ParticipantPrincipal::Agent(profile_id.clone()),
                ))
                .is_some_and(|participant| participant.left_at.is_none());
            if !active {
                return Err(RepositoryError::Conflict("direct message viewer"));
            }
        }
        let participants = state
            .participants
            .iter()
            .filter(|((id, _), _)| id == conversation_id)
            .map(|(_, participant)| participant.clone())
            .collect();
        let events = state
            .events
            .range((conversation_id.clone(), 0)..=(conversation_id.clone(), u64::MAX))
            .map(|(_, event)| event.clone())
            .collect();
        Ok(DirectMessageProjection {
            conversation,
            participants,
            events,
        })
    }
}

pub(crate) fn direct_binding(
    mutations: &[RecordMutation],
) -> Result<CanonicalDirectConversationBinding, RepositoryError> {
    let mut key = None;
    let mut conversation = None;
    let mut participants = Vec::new();
    for mutation in mutations {
        let RecordMutation::Put {
            collection,
            record,
            precondition: WritePrecondition::Missing,
        } = mutation
        else {
            return Err(RepositoryError::Conflict(
                "direct conversation creation must contain missing-only records",
            ));
        };
        match collection {
            Collection::DirectMessageKeys => key = Some(record.clone()),
            Collection::Conversations => conversation = Some(record.clone()),
            Collection::ConversationParticipants => participants.push(record.clone()),
            _ => {
                return Err(RepositoryError::Conflict(
                    "direct conversation creation contains an unrelated record",
                ));
            }
        }
    }
    if participants.len() != 2 {
        return Err(RepositoryError::Conflict(
            "agent direct conversation requires exactly two participants",
        ));
    }
    Ok(CanonicalDirectConversationBinding {
        key: key.ok_or(RepositoryError::NotFound("agent pair key mutation"))?,
        conversation: conversation.ok_or(RepositoryError::NotFound("agent DM mutation"))?,
        participants,
    })
}

pub(crate) fn human_dm_key_mutation(
    profile_id: &ProfileId,
    conversation_id: &ConversationId,
    now: UtcTimestamp,
) -> Result<RecordMutation, RepositoryError> {
    let key = HumanAgentDmKey::new(profile_id, conversation_id)?;
    direct_key_mutation(
        compound_id("human-agent-dm-key", &profile_id.to_string()),
        &key,
        now,
    )
}

pub(crate) fn stored_human_dm_conversation<R: StateRecordRepository>(
    store: &R,
    profile_id: &ProfileId,
) -> Result<Option<ConversationId>, RepositoryError> {
    let id = compound_id("human-agent-dm-key", &profile_id.to_string());
    let Some(record) = store
        .get_record(Collection::DirectMessageKeys, &id)
        .map_err(|error| RepositoryError::Durable(error.to_string()))?
    else {
        return Ok(None);
    };
    let key: HumanAgentDmKey = serde_json::from_value(record.payload)
        .map_err(|error| RepositoryError::Durable(error.to_string()))?;
    if key.version != CURRENT_SCHEMA_VERSION
        || key.profile_id != *profile_id
        || record.id != id
        || record.revision != Revision::ZERO
    {
        return Err(RepositoryError::Conflict("human DM key corruption"));
    }
    Ok(Some(key.conversation_id))
}

fn agent_dm_mutations(key: &AgentPairKey, now: UtcTimestamp) -> Vec<ConversationMutation> {
    let profiles = BTreeSet::from([key.first_profile_id.clone(), key.second_profile_id.clone()]);
    let conversation = ConversationRecord {
        schema_version: CURRENT_SCHEMA_VERSION,
        id: key.conversation_id.clone(),
        kind: ConversationKind::AgentAgentDm,
        lifecycle: ConversationLifecycle::Active,
        title: "Direct message".into(),
        creator: Principal::System,
        created_at: now,
        updated_at: now,
        revision: Revision::ZERO,
        participant_revision: Revision::ZERO,
        participant_profiles: profiles.clone(),
        human_participant: false,
        event_head: None,
    };
    let mut mutations = vec![ConversationMutation::CreateConversation(conversation)];
    mutations.extend(profiles.into_iter().map(|profile| {
        ConversationMutation::AddParticipant(ConversationParticipant {
            schema_version: CURRENT_SCHEMA_VERSION,
            conversation_id: key.conversation_id.clone(),
            principal: ParticipantPrincipal::Agent(profile),
            role: ParticipantRole::Member,
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
        })
    }));
    mutations
}

fn agent_pair_key_mutation(
    key: &AgentPairKey,
    now: UtcTimestamp,
) -> Result<RecordMutation, RepositoryError> {
    direct_key_mutation(
        compound_id("agent-pair-dm-key", key.stable_key.as_str()),
        key,
        now,
    )
}

fn direct_key_mutation(
    id: keith_agent_types::EntityId,
    key: &impl Serialize,
    now: UtcTimestamp,
) -> Result<RecordMutation, RepositoryError> {
    Ok(RecordMutation::Put {
        collection: Collection::DirectMessageKeys,
        record: VersionedRecord {
            version: CURRENT_SCHEMA_VERSION,
            id,
            revision: Revision::ZERO,
            updated_at: now,
            payload: serde_json::to_value(key)
                .map_err(|error| RepositoryError::Durable(error.to_string()))?,
        },
        precondition: WritePrecondition::Missing,
    })
}

fn stored_agent_pair_key<R: StateRecordRepository>(
    store: &R,
    expected: &AgentPairKey,
) -> Result<Option<AgentPairKey>, RepositoryError> {
    let id = compound_id("agent-pair-dm-key", expected.stable_key.as_str());
    let Some(record) = store
        .get_record(Collection::DirectMessageKeys, &id)
        .map_err(|error| RepositoryError::Durable(error.to_string()))?
    else {
        return Ok(None);
    };
    let key: AgentPairKey = serde_json::from_value(record.payload)
        .map_err(|error| RepositoryError::Durable(error.to_string()))?;
    if &key != expected || record.id != id || record.revision != Revision::ZERO {
        return Err(RepositoryError::Conflict("agent pair key corruption"));
    }
    Ok(Some(key))
}

fn verify_agent_dm(state: &RepositoryState, key: &AgentPairKey) -> Result<(), RepositoryError> {
    let conversation = state
        .conversations
        .get(&key.conversation_id)
        .ok_or(RepositoryError::NotFound("agent DM"))?;
    if conversation.kind != ConversationKind::AgentAgentDm
        || conversation.human_participant
        || conversation.participant_profiles
            != BTreeSet::from([key.first_profile_id.clone(), key.second_profile_id.clone()])
    {
        return Err(RepositoryError::Conflict("agent DM key mismatch"));
    }
    for profile in [&key.first_profile_id, &key.second_profile_id] {
        if state
            .participants
            .get(&(
                key.conversation_id.clone(),
                ParticipantPrincipal::Agent(profile.clone()),
            ))
            .is_none_or(|participant| {
                participant.left_at.is_some() || participant.role != ParticipantRole::Member
            })
        {
            return Err(RepositoryError::Conflict("agent DM participant mismatch"));
        }
    }
    Ok(())
}

fn pending_delivery(
    request: &PeerMessageRequest,
    delivery_id: DeliveryId,
    event_id: EventId,
) -> PendingPeerDelivery {
    PendingPeerDelivery {
        version: CURRENT_SCHEMA_VERSION,
        id: delivery_id,
        stable_source_key: request.idempotency_key.to_string(),
        conversation_id: request.conversation_id.clone(),
        source_event_id: event_id,
        source_profile_id: request.sender_profile_id.clone(),
        destination_profile_id: request.recipient_profile_id.clone(),
        participant_session_id: request.participant_session_id.clone(),
        policy_snapshot_key: request.policy_snapshot_key.to_string(),
        state: PendingDeliveryState::Pending,
        attempt_count: 0,
        last_claim_fence: 0,
        claim: None,
        retry_at: None,
        safe_error: None,
        revision: Revision::ZERO,
    }
}

fn delivery_matches<R: StateRecordRepository>(
    store: &R,
    stable_key: &StableKey,
    delivery_id: &DeliveryId,
) -> Result<bool, RepositoryError> {
    let Some(record) = store
        .get_record(
            Collection::ConversationDeliveries,
            delivery_id.as_entity_id(),
        )
        .map_err(|error| RepositoryError::Durable(error.to_string()))?
    else {
        return Ok(false);
    };
    let delivery: PendingPeerDelivery = serde_json::from_value(record.payload)
        .map_err(|error| RepositoryError::Durable(error.to_string()))?;
    Ok(delivery.id == *delivery_id
        && delivery.stable_source_key == stable_key.as_str()
        && delivery.revision == record.revision)
}

fn peer_receipt(
    request: &PeerMessageRequest,
    delivery_id: DeliveryId,
    event_id: EventId,
    sequence: u64,
    status: PeerMessageReceiptStatus,
) -> PeerMessageReceipt {
    PeerMessageReceipt {
        status,
        delivery_id,
        stable_source_key: request.idempotency_key.clone(),
        conversation_id: request.conversation_id.clone(),
        source_event_id: event_id,
        sender_profile_id: request.sender_profile_id.clone(),
        recipient_profile_id: request.recipient_profile_id.clone(),
        participant_session_id: request.participant_session_id.clone(),
        context_cursor: super::ConversationContextCursor {
            conversation_id: request.conversation_id.clone(),
            applied_through_sequence: sequence,
        },
    }
}

#[cfg(test)]
mod tests {
    use keith_agent_types::{EntityId, ProfileId, StableKey};
    use keith_state_store::EmbeddedStore;

    use super::*;

    fn profile(value: u128) -> ProfileId {
        ProfileId::from(EntityId::from_u128(value))
    }

    #[test]
    fn direct_message_pair_order_is_canonical_and_replay_does_not_duplicate() {
        let store = EmbeddedStore::open_in_memory().unwrap();
        let service = PermanentDirectMessageService::new(&store);
        let first = profile(1);
        let second = profile(2);
        let created = service
            .get_or_create_agent_dm(&first, &second, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let replayed = service
            .get_or_create_agent_dm(&second, &first, UtcTimestamp::from_unix_millis(1))
            .unwrap();
        assert_eq!(created.id, replayed.id);
        assert_eq!(
            created.participant_profiles,
            BTreeSet::from([first, second])
        );
    }

    #[test]
    fn direct_message_send_is_async_durable_and_old_replay_stays_duplicate() {
        let directory = tempfile::tempdir().unwrap();
        let path = directory.path().join("direct.sqlite");
        let first = profile(10);
        let second = profile(11);
        let conversation_id = {
            let store = EmbeddedStore::open(&path, None).unwrap();
            PermanentDirectMessageService::new(&store)
                .get_or_create_agent_dm(&first, &second, UtcTimestamp::UNIX_EPOCH)
                .unwrap()
                .id
        };
        let first_request = PeerMessageRequest {
            idempotency_key: StableKey::parse("peer-message:first").unwrap(),
            conversation_id: conversation_id.clone(),
            sender_profile_id: first.clone(),
            recipient_profile_id: second.clone(),
            participant_session_id: SessionId::from(EntityId::from_u128(20)),
            policy_snapshot_key: StableKey::parse("policy:first").unwrap(),
            content: "Please review this asynchronously".into(),
            timestamp: UtcTimestamp::from_unix_millis(1),
        };
        let store = EmbeddedStore::open(&path, None).unwrap();
        let service = PermanentDirectMessageService::new(&store);
        assert_eq!(
            service.send_peer_message(&first_request).unwrap().status,
            PeerMessageReceiptStatus::Accepted
        );
        let second_request = PeerMessageRequest {
            idempotency_key: StableKey::parse("peer-message:second").unwrap(),
            conversation_id,
            sender_profile_id: second,
            recipient_profile_id: first,
            participant_session_id: SessionId::from(EntityId::from_u128(21)),
            policy_snapshot_key: StableKey::parse("policy:second").unwrap(),
            content: "Acknowledged as a separate source event".into(),
            timestamp: UtcTimestamp::from_unix_millis(2),
        };
        assert_eq!(
            service.send_peer_message(&second_request).unwrap().status,
            PeerMessageReceiptStatus::Accepted
        );
        let replay = service.send_peer_message(&first_request).unwrap();
        assert_eq!(replay.status, PeerMessageReceiptStatus::Duplicate);
        assert_eq!(replay.context_cursor.applied_through_sequence, 1);
    }
}
