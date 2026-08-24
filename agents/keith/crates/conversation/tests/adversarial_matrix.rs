use std::collections::BTreeSet;
use std::sync::{Arc, Barrier};
use std::thread;

use keith_agent_types::{
    ArtifactId, CURRENT_SCHEMA_VERSION, ConversationId, EntityId, EventId, GrantId, ProfileId,
    Revision, RootTreeId, SessionId, StableKey, UtcTimestamp,
};
use keith_artifacts::{
    ArtifactLimits, ArtifactReference as StoredArtifactReference, ArtifactScope, ArtifactService,
    ArtifactSource, ConversationArtifactPromotion, ConversationArtifactVerifier, NewArtifact,
    RetentionPolicy,
};
use keith_conversation::{
    ArtifactReference, CanonicalAppendOutcome, ConversationEvent, ConversationEventKind,
    ConversationStore, CreateGroupRequest, DurableConversationAccessResolver, EventProvenance,
    GrantOperation, GrantProvenance, GroupMentionPolicy, GroupMutationStatus, GroupService,
    MAX_CONTENT_BYTES, MAX_PARTICIPANTS, ParticipantPrincipal, PeerMessageReceiptStatus,
    PeerMessageRequest, PermanentDirectMessageService, Principal, RepositoryError,
    SharedDeletionPolicy, SharedKnowledgeGrant, SharedResourceKind,
};
use keith_state_store::{EmbeddedStore, FaultPoint};
use keith_state_store_core::Collection;

fn profile(value: u128) -> ProfileId {
    ProfileId::from(EntityId::from_u128(value))
}

fn session(value: u128) -> SessionId {
    SessionId::from(EntityId::from_u128(value))
}

fn key(value: &str) -> StableKey {
    StableKey::parse(value).expect("fixture stable key is canonical")
}

fn peer_request(
    stable_key: &str,
    conversation_id: ConversationId,
    sender: ProfileId,
    recipient: ProfileId,
    session_id: SessionId,
    content: &str,
    timestamp: i64,
) -> PeerMessageRequest {
    PeerMessageRequest {
        idempotency_key: key(stable_key),
        conversation_id,
        sender_profile_id: sender,
        recipient_profile_id: recipient,
        participant_session_id: session_id,
        policy_snapshot_key: key(&format!("policy:{stable_key}")),
        content: content.into(),
        timestamp: UtcTimestamp::from_unix_millis(timestamp),
    }
}

fn message_event(
    conversation_id: ConversationId,
    sequence: u64,
    author: Principal,
    stable_key: &str,
    content: &str,
    timestamp: i64,
) -> ConversationEvent {
    ConversationEvent {
        schema_version: CURRENT_SCHEMA_VERSION,
        id: EventId::new(),
        conversation_id,
        sequence,
        publication_key: key(stable_key),
        author,
        timestamp: UtcTimestamp::from_unix_millis(timestamp),
        kind: ConversationEventKind::Message,
        content: Some(content.into()),
        artifacts: Vec::new(),
        reply_to: None,
        thread_parent: None,
        provenance: EventProvenance {
            source: "adversarial_matrix".into(),
            source_ids: vec![stable_key.into()],
            migration_version: None,
        },
    }
}

#[test]
fn canonical_append_crash_replay_and_restart_preserve_one_visible_event() {
    let directory = tempfile::tempdir().unwrap();
    let path = directory.path().join("canonical-faults.sqlite");
    let first = profile(1);
    let second = profile(2);
    let conversation_id = {
        let store = EmbeddedStore::open(&path, None).unwrap();
        PermanentDirectMessageService::new(&store)
            .get_or_create_agent_dm(&first, &second, UtcTimestamp::UNIX_EPOCH)
            .unwrap()
            .id
    };

    let rolled_back = peer_request(
        "fault:before-commit",
        conversation_id.clone(),
        first.clone(),
        second.clone(),
        session(10),
        "must not become visible",
        1,
    );
    {
        let store = EmbeddedStore::open(&path, None).unwrap();
        store.inject_fault_once(FaultPoint::BeforeCommit);
        assert!(matches!(
            PermanentDirectMessageService::new(&store).send_peer_message(&rolled_back),
            Err(RepositoryError::Durable(_))
        ));
    }
    {
        let store = EmbeddedStore::open(&path, None).unwrap();
        let projection = PermanentDirectMessageService::new(&store)
            .projection(
                &conversation_id,
                &keith_conversation::DirectMessageViewer::Agent(first.clone()),
            )
            .unwrap();
        assert!(projection.events.is_empty());
        assert!(
            store
                .list_records(Collection::ConversationDeliveries)
                .unwrap()
                .is_empty()
        );
    }

    let unknown_outcome = peer_request(
        "fault:after-commit",
        conversation_id.clone(),
        first.clone(),
        second.clone(),
        session(11),
        "committed exactly once",
        2,
    );
    {
        let store = EmbeddedStore::open(&path, None).unwrap();
        store.inject_fault_once(FaultPoint::AfterCommit);
        assert!(matches!(
            PermanentDirectMessageService::new(&store).send_peer_message(&unknown_outcome),
            Err(RepositoryError::Durable(_))
        ));
    }

    let reopened = EmbeddedStore::open(&path, None).unwrap();
    let replay = PermanentDirectMessageService::new(&reopened)
        .send_peer_message(&unknown_outcome)
        .unwrap();
    assert_eq!(replay.status, PeerMessageReceiptStatus::Duplicate);
    let projection = PermanentDirectMessageService::new(&reopened)
        .projection(
            &conversation_id,
            &keith_conversation::DirectMessageViewer::Agent(first.clone()),
        )
        .unwrap();
    assert_eq!(projection.events.len(), 1);
    assert_eq!(projection.events[0].sequence, 1);
    assert_eq!(
        projection.events[0].content.as_deref(),
        Some("committed exactly once")
    );
    assert_eq!(
        reopened
            .list_records(Collection::ConversationDeliveries)
            .unwrap()
            .len(),
        1
    );
    for collection in [
        Collection::ConversationProjectionIntents,
        Collection::ConversationUnreadIntents,
        Collection::ConversationSearchIntents,
        Collection::ConversationPublicationIntents,
    ] {
        assert_eq!(reopened.list_records(collection).unwrap().len(), 1);
    }
    let canonical = ConversationStore::open(&reopened).unwrap();
    assert_eq!(
        canonical
            .search(&Principal::Agent(first), "committed exactly once", 10)
            .unwrap()
            .len(),
        1
    );
    assert_eq!(canonical.rebuild_projections().unwrap().len(), 1);
}

#[test]
fn cross_profile_authorship_search_grants_and_real_attachments_fail_closed() {
    let directory = tempfile::tempdir().unwrap();
    let store_path = directory.path().join("authority.sqlite");
    let store = EmbeddedStore::open(&store_path, None).unwrap();
    let owner = profile(20);
    let member = profile(21);
    let outsider = profile(22);
    let group = GroupService::open(&store)
        .unwrap()
        .create_group(&CreateGroupRequest {
            operation_key: key("group:authority-matrix"),
            title: "Authority matrix".into(),
            creator: Principal::Agent(owner.clone()),
            initial_profile_ids: BTreeSet::from([member.clone()]),
            mention_policy: GroupMentionPolicy::default(),
            now: UtcTimestamp::UNIX_EPOCH,
        })
        .unwrap();
    assert_eq!(group.status, GroupMutationStatus::Applied);

    let canonical = ConversationStore::open(&store).unwrap();
    let source = message_event(
        group.conversation_id.clone(),
        2,
        Principal::Agent(owner.clone()),
        "authority:source",
        "visible-to-members-only needle-473",
        1,
    );
    assert_eq!(
        canonical
            .append(group.conversation_revision, &source)
            .unwrap(),
        CanonicalAppendOutcome::Appended
    );
    assert_eq!(
        canonical
            .search(&Principal::Agent(owner.clone()), "needle-473", 10)
            .unwrap()
            .len(),
        1
    );
    assert!(
        canonical
            .search(&Principal::Agent(outsider.clone()), "needle-473", 10)
            .unwrap()
            .is_empty()
    );
    assert!(
        canonical
            .projection(
                &group.conversation_id,
                &Principal::Agent(outsider.clone()),
                0,
                10,
            )
            .is_err()
    );
    let forged = message_event(
        group.conversation_id.clone(),
        3,
        Principal::Agent(outsider.clone()),
        "authority:forged",
        "forged cross-profile authorship",
        2,
    );
    assert!(
        canonical
            .append(group.conversation_revision.checked_next().unwrap(), &forged)
            .is_err()
    );

    let grant_id = GrantId::new();
    let grant = SharedKnowledgeGrant {
        schema_version: CURRENT_SCHEMA_VERSION,
        id: grant_id.clone(),
        resource_kind: SharedResourceKind::KnowledgeSpace,
        resource_id: EntityId::new().to_string(),
        grantor: Principal::Agent(owner.clone()),
        grantee: member.clone(),
        purpose: "review the shared result".into(),
        provenance: GrantProvenance {
            source_actor: Principal::Agent(owner.clone()),
            source_conversation_id: Some(group.conversation_id.clone()),
            source_event_ids: vec![source.id.clone()],
        },
        resource_policy_revision: Revision::ZERO,
        deletion_policy: SharedDeletionPolicy::RetainUntilExplicitDelete,
        operations: BTreeSet::from([GrantOperation::Read, GrantOperation::Search]),
        created_at: UtcTimestamp::from_unix_millis(2),
        expires_at: None,
        revoked_at: None,
        revision: Revision::ZERO,
    };
    canonical
        .put_shared_grant(&Principal::Agent(owner.clone()), None, grant.clone())
        .unwrap();
    assert!(
        canonical
            .authorize_grant(
                &grant_id,
                &member,
                &GrantOperation::Search,
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap()
    );
    assert!(
        !canonical
            .authorize_grant(
                &grant_id,
                &outsider,
                &GrantOperation::Search,
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap()
    );
    let mut forged_grant = grant.clone();
    forged_grant.id = GrantId::new();
    forged_grant.grantor = Principal::Agent(outsider.clone());
    forged_grant.provenance.source_actor = Principal::Agent(outsider.clone());
    assert!(
        canonical
            .put_shared_grant(&Principal::Agent(outsider.clone()), None, forged_grant)
            .is_err()
    );
    canonical
        .revoke_shared_grant(
            &Principal::Agent(owner.clone()),
            &grant_id,
            Revision::ZERO,
            UtcTimestamp::from_unix_millis(4),
        )
        .unwrap();
    assert!(
        !canonical
            .authorize_grant(
                &grant_id,
                &member,
                &GrantOperation::Read,
                UtcTimestamp::from_unix_millis(5),
            )
            .unwrap()
    );

    let artifact_service = ArtifactService::open(
        directory.path().join("artifacts"),
        ArtifactLimits {
            max_artifact_bytes: 1_024,
            max_preview_bytes: 1_024,
            max_artifacts_per_tree: 16,
        },
    )
    .unwrap();
    let metadata = artifact_service
        .create(NewArtifact {
            scope: ArtifactScope {
                root_tree_id: RootTreeId::new(),
                session_id: session(30),
                profile_id: owner.clone(),
            },
            source: ArtifactSource::Tool,
            media_type: "text/plain",
            bytes: b"durable attachment",
            created_at: UtcTimestamp::from_unix_millis(5),
            display: None,
            retention: RetentionPolicy::Retain,
        })
        .unwrap();
    let stored_reference = StoredArtifactReference::from(&metadata);
    let access = DurableConversationAccessResolver::open(&store).unwrap();
    let promoted = artifact_service
        .promote_to_conversation(
            &stored_reference,
            ConversationArtifactPromotion {
                operation_key: key("artifact:promote:authority"),
                actor: keith_artifacts::ArtifactActor::Agent(owner.clone()),
                conversation_id: group.conversation_id.clone(),
                expected_revision: Revision::ZERO,
                source_event_ids: BTreeSet::from([source.id.clone()]),
                now: UtcTimestamp::from_unix_millis(6),
            },
            &access,
        )
        .unwrap();
    let verifier = ConversationArtifactVerifier::new(&artifact_service, &access);
    let attachment = ConversationEvent {
        artifacts: vec![ArtifactReference {
            artifact_id: promoted.id.clone(),
            digest_sha256: promoted.sha256.clone(),
        }],
        ..message_event(
            group.conversation_id.clone(),
            3,
            Principal::Agent(owner.clone()),
            "authority:attachment",
            "published attachment",
            7,
        )
    };
    assert_eq!(
        canonical
            .append_with_attachments(
                &Principal::Agent(owner.clone()),
                group.conversation_revision.checked_next().unwrap(),
                &attachment,
                &[source.id.clone()],
                &verifier,
            )
            .unwrap(),
        CanonicalAppendOutcome::Appended
    );
    let forged_attachment = ConversationEvent {
        artifacts: vec![ArtifactReference {
            artifact_id: promoted.id,
            digest_sha256: promoted.sha256,
        }],
        ..message_event(
            group.conversation_id,
            4,
            Principal::Agent(outsider.clone()),
            "authority:attachment-forged",
            "must be rejected",
            8,
        )
    };
    assert!(
        canonical
            .append_with_attachments(
                &Principal::Agent(outsider),
                group
                    .conversation_revision
                    .checked_next()
                    .and_then(Revision::checked_next)
                    .unwrap(),
                &forged_attachment,
                &[source.id],
                &verifier,
            )
            .is_err()
    );
}

#[test]
fn eight_way_dm_and_group_contention_converges_after_restart() {
    const WORKERS: usize = 8;
    let directory = tempfile::tempdir().unwrap();
    let dm_path = directory.path().join("dm-concurrency.sqlite");
    EmbeddedStore::open(&dm_path, None).unwrap();
    let first = profile(40);
    let second = profile(41);
    let barrier = Arc::new(Barrier::new(WORKERS));
    let mut workers = Vec::new();
    for _ in 0..WORKERS {
        let path = dm_path.clone();
        let barrier = Arc::clone(&barrier);
        let first = first.clone();
        let second = second.clone();
        workers.push(thread::spawn(move || {
            barrier.wait();
            for _ in 0..32 {
                let store = EmbeddedStore::open(&path, None).unwrap();
                match PermanentDirectMessageService::new(&store).get_or_create_agent_dm(
                    &first,
                    &second,
                    UtcTimestamp::UNIX_EPOCH,
                ) {
                    Ok(conversation) => return conversation.id,
                    Err(RepositoryError::Durable(_) | RepositoryError::Conflict(_)) => {
                        thread::yield_now();
                    }
                    Err(error) => panic!("unexpected DM contention error: {error}"),
                }
            }
            panic!("DM contention did not converge")
        }));
    }
    let conversation_ids = workers
        .into_iter()
        .map(|worker| worker.join().unwrap())
        .collect::<BTreeSet<_>>();
    assert_eq!(conversation_ids.len(), 1);
    let reopened = EmbeddedStore::open(&dm_path, None).unwrap();
    assert_eq!(
        reopened
            .list_records(Collection::DirectMessageKeys)
            .unwrap()
            .len(),
        1
    );
    assert_eq!(
        reopened
            .list_records(Collection::Conversations)
            .unwrap()
            .len(),
        1
    );
    assert_eq!(
        reopened
            .list_records(Collection::ConversationParticipants)
            .unwrap()
            .len(),
        2
    );

    let group_path = directory.path().join("group-concurrency.sqlite");
    EmbeddedStore::open(&group_path, None).unwrap();
    let barrier = Arc::new(Barrier::new(WORKERS));
    let mut workers = Vec::new();
    for _ in 0..WORKERS {
        let path = group_path.clone();
        let barrier = Arc::clone(&barrier);
        let first = first.clone();
        let second = second.clone();
        workers.push(thread::spawn(move || {
            barrier.wait();
            let request = CreateGroupRequest {
                operation_key: key("group:eight-way-contention"),
                title: "Eight-way contention".into(),
                creator: Principal::Agent(first),
                initial_profile_ids: BTreeSet::from([second]),
                mention_policy: GroupMentionPolicy::default(),
                now: UtcTimestamp::UNIX_EPOCH,
            };
            for _ in 0..32 {
                let store = EmbeddedStore::open(&path, None).unwrap();
                match GroupService::open(&store).unwrap().create_group(&request) {
                    Ok(receipt) => return receipt,
                    Err(RepositoryError::Durable(_) | RepositoryError::Conflict(_)) => {
                        thread::yield_now();
                    }
                    Err(error) => panic!("unexpected group contention error: {error}"),
                }
            }
            panic!("group contention did not converge")
        }));
    }
    let receipts = workers
        .into_iter()
        .map(|worker| worker.join().unwrap())
        .collect::<Vec<_>>();
    assert_eq!(
        receipts
            .iter()
            .filter(|receipt| receipt.status == GroupMutationStatus::Applied)
            .count(),
        1
    );
    assert!(receipts.iter().skip(1).all(|receipt| {
        receipt.conversation_id == receipts[0].conversation_id
            && receipt.event_id == receipts[0].event_id
    }));
    let reopened = EmbeddedStore::open(&group_path, None).unwrap();
    let projection = ConversationStore::open(&reopened)
        .unwrap()
        .projection(
            &receipts[0].conversation_id,
            &Principal::Agent(first),
            0,
            100,
        )
        .unwrap();
    assert_eq!(projection.events.len(), 1);
    assert_eq!(projection.events[0].sequence, 1);
    assert_eq!(projection.conversation.event_head.unwrap().sequence, 1);
    assert_eq!(projection.participants.len(), 2);
}

#[test]
fn hard_budgets_leave_no_conversation_or_artifact_resource_leaks() {
    let directory = tempfile::tempdir().unwrap();
    let store = EmbeddedStore::open(&directory.path().join("budgets.sqlite"), None).unwrap();
    let first = profile(60);
    let second = profile(61);
    let conversation = PermanentDirectMessageService::new(&store)
        .get_or_create_agent_dm(&first, &second, UtcTimestamp::UNIX_EPOCH)
        .unwrap();
    let oversized = peer_request(
        "budget:oversized-message",
        conversation.id.clone(),
        first.clone(),
        second,
        session(60),
        &"x".repeat(MAX_CONTENT_BYTES + 1),
        1,
    );
    assert!(
        PermanentDirectMessageService::new(&store)
            .send_peer_message(&oversized)
            .is_err()
    );
    assert!(
        PermanentDirectMessageService::new(&store)
            .projection(
                &conversation.id,
                &keith_conversation::DirectMessageViewer::Agent(first.clone()),
            )
            .unwrap()
            .events
            .is_empty()
    );
    assert!(
        store
            .list_records(Collection::ConversationDeliveries)
            .unwrap()
            .is_empty()
    );
    assert!(
        ConversationStore::open(&store)
            .unwrap()
            .search(&Principal::Agent(first.clone()), "x", 1_001)
            .is_err()
    );

    let too_many_profiles = (0..=MAX_PARTICIPANTS)
        .map(|offset| profile(1_000 + u128::try_from(offset).unwrap()))
        .collect::<BTreeSet<_>>();
    assert!(
        GroupService::open(&store)
            .unwrap()
            .create_group(&CreateGroupRequest {
                operation_key: key("budget:too-many-participants"),
                title: "Must not persist".into(),
                creator: Principal::Human,
                initial_profile_ids: too_many_profiles,
                mention_policy: GroupMentionPolicy::default(),
                now: UtcTimestamp::from_unix_millis(2),
            })
            .is_err()
    );
    assert_eq!(
        store.list_records(Collection::Conversations).unwrap().len(),
        1
    );

    let artifact_service = ArtifactService::open(
        directory.path().join("bounded-artifacts"),
        ArtifactLimits {
            max_artifact_bytes: 4,
            max_preview_bytes: 4,
            max_artifacts_per_tree: 1,
        },
    )
    .unwrap();
    let scope = ArtifactScope {
        root_tree_id: RootTreeId::new(),
        session_id: session(61),
        profile_id: first.clone(),
    };
    assert!(
        artifact_service
            .create(NewArtifact {
                scope: scope.clone(),
                source: ArtifactSource::Tool,
                media_type: "text/plain",
                bytes: b"12345",
                created_at: UtcTimestamp::from_unix_millis(3),
                display: None,
                retention: RetentionPolicy::Retain,
            })
            .is_err()
    );
    assert!(
        artifact_service
            .inventory_profile_deletion(&first)
            .unwrap()
            .records
            .is_empty()
    );
    artifact_service
        .create(NewArtifact {
            scope,
            source: ArtifactSource::Tool,
            media_type: "text/plain",
            bytes: b"1234",
            created_at: UtcTimestamp::from_unix_millis(4),
            display: None,
            retention: RetentionPolicy::Retain,
        })
        .unwrap();
    let inventory = artifact_service.inventory_profile_deletion(&first).unwrap();
    assert_eq!(inventory.records.len(), 1);
    let receipt = artifact_service
        .erase_profile_inventory(&inventory)
        .unwrap();
    assert_eq!(receipt.erased_stable_keys.len(), 1);
    assert!(
        artifact_service
            .scan_profile_deletion_leaks(&first)
            .unwrap()
            .leaked_private_keys
            .is_empty()
    );
}
