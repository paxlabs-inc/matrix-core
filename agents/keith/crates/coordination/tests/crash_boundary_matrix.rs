use std::collections::BTreeSet;

use keith_agent_types::{
    ActionId, AssignmentId, CURRENT_SCHEMA_VERSION, ConversationId, DeliveryId, EntityId, EntryId,
    EventId, Generation, ProfileId, Revision, SessionId, StableKey, TurnId, UtcTimestamp, WorkerId,
};
use keith_conversation::{
    CanonicalConversationStore, ConversationContextCursor, ConversationEventKind,
    CreateGroupRequest, GroupMentionPolicy, GroupService, Principal,
};
use keith_coordination::round::{
    RoundCoordinator, RoundCoordinatorConfig, RoundCoordinatorError, RoundMutationStatus,
    RoundTrigger,
};
use keith_coordination::{
    AssignmentHandoff, AssignmentRecord, AssignmentService, AssignmentServiceError,
    AssignmentState, CanonicalHandoffEventIntent, ConversationDelivery,
    ConversationDeliveryCoordinator, ConversationDeliveryEnqueue, ConversationPublicationOutbox,
    ConversationPublicationResult, DeliveryCoordinatorConfig, DeliveryCoordinatorError,
    DeliveryState, DurableCoordinationRepository, MentionPolicy, OwnershipTransfer,
    OwnershipTransferId, PublicationIntentState, publication_result_digest,
};
use keith_session_store::{
    ParticipantPublicationCommit, ParticipantPublicationIntent, ParticipantTerminalFinalization,
    SessionEntry, SessionEntryPayload, WriterIdentity,
};
use keith_state_store::EmbeddedStore;

fn profile(value: u128) -> ProfileId {
    ProfileId::from(EntityId::from_u128(value))
}

fn session(value: u128) -> SessionId {
    SessionId::from(EntityId::from_u128(value))
}

fn conversation(value: u128) -> ConversationId {
    ConversationId::from(EntityId::from_u128(value))
}

fn event(value: u128) -> EventId {
    EventId::from(EntityId::from_u128(value))
}

fn delivery_request(key: &str, destination: u128) -> ConversationDeliveryEnqueue {
    ConversationDeliveryEnqueue {
        stable_source_key: key.to_owned(),
        conversation_id: conversation(10),
        source_event_id: event(20 + destination),
        source_profile_id: profile(1),
        destination_profile_id: profile(destination),
        purpose: keith_coordination::ConversationDeliveryPurpose::Peer,
        participant_session_id: session(100 + destination),
        policy_snapshot_key: format!("policy:{destination}"),
    }
}

#[test]
fn delivery_restart_matrix_fences_stale_claims_rotates_six_profiles_and_cleans_dead_letters() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("coordination.sqlite");
    let config = DeliveryCoordinatorConfig {
        max_queued: 32,
        max_installation_claims: 4,
        max_claims_per_profile: 1,
        max_attempts: 2,
        lease_millis: 10,
        initial_backoff_millis: 1,
        max_backoff_millis: 2,
    };

    let coordinator =
        ConversationDeliveryCoordinator::new(EmbeddedStore::open(&path, None).unwrap(), config)
            .unwrap();
    for destination in 2..=7 {
        let key = format!("delivery:fanout:{destination}");
        let first = coordinator
            .enqueue(delivery_request(&key, destination))
            .unwrap();
        let replay = coordinator
            .enqueue(delivery_request(&key, destination))
            .unwrap();
        assert_eq!(first, replay);
    }
    assert!(matches!(
        coordinator.enqueue(delivery_request("delivery:fanout:2", 3)),
        Err(DeliveryCoordinatorError::Invalid(_))
    ));

    let mut claims = Vec::new();
    for _ in 0..4 {
        claims.push(
            coordinator
                .claim_next(UtcTimestamp::from_unix_millis(1))
                .unwrap()
                .unwrap(),
        );
    }
    assert_eq!(
        claims
            .iter()
            .map(|claim| claim.delivery.destination_profile_id.clone())
            .collect::<BTreeSet<_>>()
            .len(),
        4
    );
    assert!(
        coordinator
            .claim_next(UtcTimestamp::from_unix_millis(1))
            .unwrap()
            .is_none()
    );
    let first_claim = claims[0].clone();
    drop(coordinator);

    let reopened =
        ConversationDeliveryCoordinator::new(EmbeddedStore::open(&path, None).unwrap(), config)
            .unwrap();
    assert!(
        reopened
            .claim_next(UtcTimestamp::from_unix_millis(2))
            .unwrap()
            .is_none()
    );
    let recovered = reopened
        .recover_expired(UtcTimestamp::from_unix_millis(12))
        .unwrap();
    assert_eq!(recovered.len(), 4);
    assert!(recovered.iter().all(|delivery| {
        delivery.state == DeliveryState::Retryable
            && delivery.claim.is_none()
            && delivery.last_claim_fence == 1
    }));
    assert!(matches!(
        reopened.renew(&first_claim, UtcTimestamp::from_unix_millis(12)),
        Err(DeliveryCoordinatorError::StaleClaim)
    ));

    let live = reopened
        .claim_next(UtcTimestamp::from_unix_millis(14))
        .unwrap()
        .unwrap();
    let mut forged = live.clone();
    forged.token = EntityId::new();
    assert!(matches!(
        reopened.finalize(&forged, UtcTimestamp::from_unix_millis(15)),
        Err(DeliveryCoordinatorError::StaleClaim)
    ));

    let mut terminal = reopened
        .retry(
            &live,
            UtcTimestamp::from_unix_millis(15),
            "bounded permanent transport failure",
        )
        .unwrap();
    for step in 0..24 {
        if terminal.state == DeliveryState::DeadLetter {
            break;
        }
        let now = UtcTimestamp::from_unix_millis(1_000 + step * 1_000);
        let Some(claim) = reopened.claim_next(now).unwrap() else {
            continue;
        };
        terminal = reopened
            .retry(&claim, now, "bounded permanent transport failure")
            .unwrap();
    }
    let dead = terminal;
    assert_eq!(dead.state, DeliveryState::DeadLetter);
    assert!(dead.claim.is_none());
    assert!(dead.retry_at.is_none());
    assert!(
        reopened
            .recover_expired(UtcTimestamp::from_unix_millis(1_000))
            .unwrap()
            .iter()
            .all(|delivery| delivery.id != dead.id)
    );
}

fn publication_commit(
    delivery: &ConversationDelivery,
    result: &ConversationPublicationResult,
) -> ParticipantPublicationCommit {
    let stable_publication_key =
        StableKey::parse(format!("publication:{}", delivery.stable_source_key)).unwrap();
    let result_digest = publication_result_digest(result).unwrap();
    let finalization_entry_id = EntryId::new();
    let result_entry_id = EntryId::new();
    let turn_id = TurnId::new();
    let at = UtcTimestamp::from_unix_millis(20);
    let finalization = ParticipantTerminalFinalization {
        stable_publication_key: stable_publication_key.clone(),
        conversation_id: delivery.conversation_id.clone(),
        source_event_id: delivery.source_event_id.clone(),
        participant_session_id: delivery.participant_session_id.clone(),
        participant_profile_id: delivery.destination_profile_id.clone(),
        turn_id: turn_id.clone(),
        result_entry_id: result_entry_id.clone(),
        result_digest: result_digest.clone(),
        finalized_at: at,
        recorded_by: WriterIdentity {
            worker_id: WorkerId::new(),
            owner_instance: EntityId::new(),
            generation: Generation::new(1),
            acquired_at: UtcTimestamp::from_unix_millis(1),
        },
    };
    let finalization_entry = SessionEntry::new(
        finalization_entry_id.clone(),
        None,
        at,
        SessionEntryPayload::ParticipantTerminalFinalization { finalization },
    )
    .unwrap();
    let intent = ParticipantPublicationIntent {
        stable_publication_key,
        conversation_id: delivery.conversation_id.clone(),
        source_event_id: delivery.source_event_id.clone(),
        participant_session_id: delivery.participant_session_id.clone(),
        participant_profile_id: delivery.destination_profile_id.clone(),
        turn_id,
        finalization_entry_id: finalization_entry_id.clone(),
        result_entry_id,
        result_digest,
        created_at: at,
    };
    let publication_intent_entry = SessionEntry::new(
        EntryId::new(),
        Some(finalization_entry_id),
        at,
        SessionEntryPayload::ParticipantPublicationIntent { intent },
    )
    .unwrap();
    ParticipantPublicationCommit {
        finalization_entry,
        publication_intent_entry,
    }
}

#[test]
fn publication_claim_crash_replay_is_fenced_and_eventually_dead_letters_without_leaking_claim() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("publication.sqlite");
    let deliveries = ConversationDeliveryCoordinator::new(
        EmbeddedStore::open(&path, None).unwrap(),
        DeliveryCoordinatorConfig::default(),
    )
    .unwrap();
    let queued = deliveries
        .enqueue(delivery_request("publication:source", 2))
        .unwrap();
    let delivery_claim = deliveries
        .claim_next(UtcTimestamp::from_unix_millis(1))
        .unwrap()
        .unwrap();
    let finalized = deliveries
        .finalize(&delivery_claim, UtcTimestamp::from_unix_millis(2))
        .unwrap();
    assert_eq!(finalized.id, queued.id);

    let result = ConversationPublicationResult {
        kind: ConversationEventKind::Message,
        content: Some("durable participant result".into()),
        artifacts: Vec::new(),
        reply_to: None,
        thread_parent: None,
    };
    let commit = publication_commit(&finalized, &result);
    let outbox = ConversationPublicationOutbox::new(EmbeddedStore::open(&path, None).unwrap());
    let staged = outbox
        .stage(&finalized, ActionId::new(), &commit, result.clone())
        .unwrap();
    assert_eq!(
        outbox
            .stage(&finalized, staged.action_id.clone(), &commit, result)
            .unwrap(),
        staged
    );
    let first_claim = outbox
        .claim_next(UtcTimestamp::from_unix_millis(3))
        .unwrap()
        .unwrap();
    drop(outbox);

    let reopened = ConversationPublicationOutbox::new(EmbeddedStore::open(&path, None).unwrap());
    let recovered = reopened
        .recover_expired(first_claim.lease_expires_at)
        .unwrap();
    assert_eq!(recovered.len(), 1);
    assert_eq!(recovered[0].state, PublicationIntentState::Retryable);
    assert!(recovered[0].claim.is_none());
    assert!(matches!(
        reopened.retry(
            &first_claim,
            first_claim.lease_expires_at,
            "stale publisher must not mutate"
        ),
        Err(keith_coordination::PublicationOutboxError::StaleClaim)
    ));

    let mut last = recovered[0].clone();
    for attempt in 0..8 {
        let now = UtcTimestamp::from_unix_millis(100_000 + attempt * 100_000);
        let Some(claim) = reopened.claim_next(now).unwrap() else {
            continue;
        };
        last = reopened
            .retry(&claim, now, "canonical append unavailable")
            .unwrap();
        if last.state == PublicationIntentState::DeadLetter {
            break;
        }
    }
    assert_eq!(last.state, PublicationIntentState::DeadLetter);
    assert!(last.claim.is_none());
    assert!(last.retry_at.is_none());
}

#[test]
fn six_participant_round_trigger_survives_restart_replays_exactly_and_rejects_forged_coordinator() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("round.sqlite");
    let participants = (1..=6).map(profile).collect::<BTreeSet<_>>();
    let trigger = RoundTrigger {
        stable_key: StableKey::parse("round:restart:six-participants").unwrap(),
        conversation_id: conversation(30),
        trigger_event_id: event(31),
        coordinator_profile_id: profile(1),
        eligible_participants: participants,
        mention_policy: MentionPolicy::AllParticipants,
        max_depth: 3,
        max_turns: 12,
        triggered_at: UtcTimestamp::from_unix_millis(1),
    };
    let coordinator = RoundCoordinator::new(
        EmbeddedStore::open(&path, None).unwrap(),
        RoundCoordinatorConfig {
            max_active_deliveries: 4,
            ..RoundCoordinatorConfig::default()
        },
    )
    .unwrap();
    let applied = coordinator.trigger(&trigger).unwrap();
    assert_eq!(applied.status, RoundMutationStatus::Applied);
    drop(coordinator);

    let reopened = RoundCoordinator::new(
        EmbeddedStore::open(&path, None).unwrap(),
        RoundCoordinatorConfig {
            max_active_deliveries: 4,
            ..RoundCoordinatorConfig::default()
        },
    )
    .unwrap();
    let replay = reopened.trigger(&trigger).unwrap();
    assert_eq!(replay.status, RoundMutationStatus::Duplicate);
    assert_eq!(replay.round, applied.round);

    let mut forged = trigger.clone();
    forged.stable_key = StableKey::parse("round:forged-coordinator").unwrap();
    forged.coordinator_profile_id = profile(99);
    assert!(matches!(
        reopened.trigger(&forged),
        Err(RoundCoordinatorError::Invalid(_))
    ));

    let mut collision = trigger.clone();
    collision.trigger_event_id = event(999);
    assert!(matches!(
        reopened.trigger(&collision),
        Err(RoundCoordinatorError::Invalid(_))
    ));
}

#[test]
fn group_round_audit_does_not_corrupt_conversation_reads_after_reopen() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("group-round-conversation.sqlite");
    let store = EmbeddedStore::open(&path, None).unwrap();
    let members = BTreeSet::from([profile(1), profile(2)]);
    let group = GroupService::open(&store)
        .unwrap()
        .create_group(&CreateGroupRequest {
            operation_key: StableKey::parse("group:round-reopen").unwrap(),
            title: "Round durability".into(),
            creator: Principal::Human,
            initial_profile_ids: members.clone(),
            mention_policy: GroupMentionPolicy::default(),
            now: UtcTimestamp::from_unix_millis(1),
        })
        .unwrap();
    RoundCoordinator::new(store, RoundCoordinatorConfig::default())
        .unwrap()
        .trigger(&RoundTrigger {
            stable_key: StableKey::parse("round:conversation-reopen").unwrap(),
            conversation_id: group.conversation_id.clone(),
            trigger_event_id: group.event_id,
            coordinator_profile_id: profile(1),
            eligible_participants: members,
            mention_policy: MentionPolicy::AllParticipants,
            max_depth: 2,
            max_turns: 4,
            triggered_at: UtcTimestamp::from_unix_millis(2),
        })
        .unwrap();

    let reopened = EmbeddedStore::open(&path, None).unwrap();
    let conversations = CanonicalConversationStore::open(&reopened).unwrap();
    let projection = conversations
        .projection(&group.conversation_id, &Principal::Human, 0, 100)
        .unwrap();
    assert_eq!(projection.conversation.id, group.conversation_id);
    let context = conversations
        .reconstruct_context(
            &Principal::Human,
            ConversationContextCursor {
                conversation_id: projection.conversation.id,
                applied_through_sequence: 0,
            },
            100,
        )
        .unwrap();
    assert_eq!(context.visible_events.len(), 1);
}

fn assignment_record(owner: ProfileId) -> AssignmentRecord {
    AssignmentRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: AssignmentId::new(),
        stable_key: "assignment:restart:handoff".into(),
        conversation_id: conversation(40),
        objective: "Produce the bounded handoff result".into(),
        owner_profile_id: owner,
        creator_profile_id: profile(1),
        dependencies: BTreeSet::new(),
        state: AssignmentState::Proposed,
        claim: None,
        priority: 0,
        due: None,
        source_event_id: event(41),
        result_event_id: None,
        block_reason: None,
        revision: Revision::ZERO,
        ownership_history: Vec::new(),
    }
}

#[test]
fn assignment_claim_and_atomic_handoff_survive_restart_and_reject_stale_authority() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("assignment.sqlite");
    let old_owner = profile(2);
    let new_owner = profile(3);
    let repository = DurableCoordinationRepository::new(EmbeddedStore::open(&path, None).unwrap());
    let mut assignments = AssignmentService::new(repository);
    let created = assignments
        .create(assignment_record(old_owner.clone()))
        .unwrap();
    let lease = assignments
        .claim(
            &created.id,
            created.revision,
            old_owner.clone(),
            UtcTimestamp::from_unix_millis(100),
            UtcTimestamp::from_unix_millis(1),
        )
        .unwrap();
    let mut forged = lease.clone();
    forged.claimant = profile(99);
    assert!(matches!(
        assignments.activate(&forged, UtcTimestamp::from_unix_millis(2)),
        Err(AssignmentServiceError::LeaseLost)
    ));
    let active = assignments
        .activate(&lease, UtcTimestamp::from_unix_millis(2))
        .unwrap();

    let deliveries = ConversationDeliveryCoordinator::new(
        EmbeddedStore::open(&path, None).unwrap(),
        DeliveryCoordinatorConfig::default(),
    )
    .unwrap();
    let mut obsolete_request = delivery_request("assignment:obsolete", 2);
    obsolete_request.conversation_id = active.conversation_id.clone();
    obsolete_request.source_event_id = active.source_event_id.clone();
    obsolete_request.source_profile_id = active.creator_profile_id.clone();
    obsolete_request.destination_profile_id = active.owner_profile_id.clone();
    let obsolete = deliveries.enqueue(obsolete_request).unwrap();
    let new_delivery = ConversationDelivery {
        version: CURRENT_SCHEMA_VERSION,
        id: DeliveryId::new(),
        stable_source_key: "assignment:new-owner-delivery".into(),
        conversation_id: active.conversation_id.clone(),
        source_event_id: event(44),
        source_profile_id: old_owner.clone(),
        destination_profile_id: new_owner.clone(),
        purpose: keith_coordination::ConversationDeliveryPurpose::Assignment,
        participant_session_id: session(103),
        policy_snapshot_key: "policy:new-owner".into(),
        state: DeliveryState::Pending,
        attempt_count: 0,
        last_claim_fence: 0,
        claim: None,
        retry_at: None,
        safe_error: None,
        supersession: None,
        revision: Revision::ZERO,
    };
    let occurred_at = UtcTimestamp::from_unix_millis(3);
    let next_revision = active.revision.checked_next().unwrap();
    let transfer = OwnershipTransfer {
        id: OwnershipTransferId::new(),
        stable_key: "ownership-transfer:restart".into(),
        from_profile_id: old_owner.clone(),
        to_profile_id: new_owner.clone(),
        actor_profile_id: old_owner.clone(),
        expected_revision: active.revision,
        source_event_id: active.source_event_id.clone(),
        occurred_at,
    };
    let intent = CanonicalHandoffEventIntent {
        stable_key: "handoff-event:restart".into(),
        event_id: event(44),
        assignment_id: active.id.clone(),
        conversation_id: active.conversation_id.clone(),
        from_profile_id: old_owner,
        to_profile_id: new_owner.clone(),
        source_event_id: active.source_event_id.clone(),
        ownership_revision: next_revision,
        occurred_at,
    };
    let request = AssignmentHandoff {
        transfer,
        obsolete_delivery_id: obsolete.id.clone(),
        new_owner_delivery: new_delivery,
        event_intent: intent,
    };
    let receipt = assignments.handoff(&active.id, request.clone()).unwrap();
    assert_eq!(receipt.assignment.owner_profile_id, new_owner);
    assert_eq!(receipt.assignment.revision, next_revision);
    assert_eq!(receipt.superseded_delivery.state, DeliveryState::Superseded);
    assert_eq!(receipt.new_owner_delivery.state, DeliveryState::Pending);
    drop(assignments);
    drop(deliveries);

    let reopened = DurableCoordinationRepository::new(EmbeddedStore::open(&path, None).unwrap());
    let mut assignments = AssignmentService::new(reopened);
    let replay = assignments.handoff(&active.id, request.clone()).unwrap();
    assert_eq!(replay, receipt);

    let mut conflict = request;
    conflict.event_intent.to_profile_id = profile(77);
    assert!(matches!(
        assignments.handoff(&active.id, conflict),
        Err(AssignmentServiceError::Invalid("handoff replay conflict"))
    ));
    assert!(matches!(
        assignments.claim(
            &active.id,
            active.revision,
            profile(2),
            UtcTimestamp::from_unix_millis(200),
            UtcTimestamp::from_unix_millis(4),
        ),
        Err(AssignmentServiceError::RevisionConflict)
    ));
}
