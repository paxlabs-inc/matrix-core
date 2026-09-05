use super::*;
use crate::{AgentMemoryKind, MemoryCreateRequest, MemoryPolicy, MemoryWriteSource};
use keith_agent_types::{
    ActionId, Generation, RootTreeId, ToolCallId, TurnId, WorkerId, WorkspaceId,
};
use keith_session_store::{
    CommittedSourceLimits, MessageRole, NewSession, SessionKind, SessionStore, StoredMessage,
    TurnTerminalStatus, WriterIdentity,
};
use keith_workspace::{PersonalWorkspace, PersonalWorkspaceLimits};
use tempfile::{TempDir, tempdir};

const NOW: UtcTimestamp = UtcTimestamp::UNIX_EPOCH;

fn identity() -> WriterIdentity {
    WriterIdentity {
        worker_id: WorkerId::new(),
        owner_instance: EntityId::new(),
        generation: Generation::new(1),
        acquired_at: NOW,
    }
}

fn message(role: MessageRole, text: &str) -> StoredMessage {
    StoredMessage {
        role,
        content: vec![ContentBlock::Text { text: text.into() }],
        provider_metadata: BTreeMap::new(),
    }
}

fn user(text: &str) -> SessionEntryPayload {
    SessionEntryPayload::UserMessage {
        message: message(MessageRole::User, text),
    }
}

struct Fixture {
    root: TempDir,
    store: SessionStore,
    profile: ProfileId,
    workspace: WorkspaceId,
    session: SessionId,
    memory: MemoryService,
}

impl Fixture {
    fn new() -> Self {
        let root = tempdir().unwrap();
        let store = SessionStore::open(root.path().join("sources")).unwrap();
        let profile = ProfileId::new();
        let workspace = WorkspaceId::new();
        let session = create_session(&store, &profile, &workspace);
        let memory = open_memory(root.path(), &profile);
        Self {
            root,
            store,
            profile,
            workspace,
            session,
            memory,
        }
    }

    fn append(&self, payload: SessionEntryPayload) -> CommittedSourceEntry {
        let mut writer = self
            .store
            .acquire_writer(&self.session, identity())
            .unwrap();
        writer
            .append_committed_source(writer.manifest().active_leaf.clone(), NOW, payload)
            .unwrap()
    }

    fn page(&self, count: usize) -> keith_session_store::CommittedSourcePage {
        let cursor = self.memory.committed_source_cursor(&self.session).unwrap();
        self.store
            .committed_source_page(
                &self.profile,
                &self.session,
                cursor.as_ref(),
                CommittedSourceLimits {
                    max_entries: count,
                    max_bytes: 1024 * 1024,
                },
            )
            .unwrap()
    }

    fn evidence(&self, source: &CommittedSourceEntry) -> EvidenceRecord {
        direct_source(
            &self.memory.observatory().evidence_snapshot().unwrap(),
            source.session_id(),
            &source.entry().id,
        )
        .unwrap()
        .clone()
    }

    fn finalize(&self, input: &CommittedSourceEntry, text: &str) -> EntryId {
        let mut writer = self
            .store
            .acquire_writer(&self.session, identity())
            .unwrap();
        let turn = TurnId::new();
        let action = ActionId::new();
        writer
            .accept_turn(NOW, action.clone(), turn.clone(), input.entry().id.clone())
            .unwrap();
        writer
            .append_finalized_turn(
                NOW,
                &turn,
                message(MessageRole::Assistant, text),
                TurnTerminalStatus::Completed,
                true,
                true,
                Some(action),
                vec![],
                None,
            )
            .unwrap()
            .0
            .id
    }
}

fn create_session(store: &SessionStore, profile: &ProfileId, workspace: &WorkspaceId) -> SessionId {
    store
        .create(NewSession {
            kind: SessionKind::Root,
            session_id: SessionId::new(),
            root_tree_id: RootTreeId::new(),
            parent_session_id: None,
            profile_id: profile.clone(),
            workspace_id: workspace.clone(),
            created_at: NOW,
            label: None,
            profile_snapshot: None,
        })
        .unwrap()
        .session_id
}

fn open_memory(root: &Path, profile: &ProfileId) -> MemoryService {
    MemoryService::open(
        PersonalWorkspace::open(root, PersonalWorkspaceLimits::default(), NOW).unwrap(),
        profile,
        MemoryPolicy::default(),
    )
    .unwrap()
}

fn request(source: &EvidenceRecord, quote: &str, text: &str) -> MemoryCreateRequest {
    MemoryCreateRequest {
        source: MemoryWriteSource {
            evidence_id: Some(source.id.clone()),
            source_entry_id: source.source_entries[0].clone(),
            evidence_quote: quote.into(),
        },
        text: text.into(),
        kind: AgentMemoryKind::ProjectContext,
        facets: vec![],
        sensitivity: Sensitivity::Personal,
    }
}

#[test]
fn bounded_replay_survives_more_than_old_queue_capacity_and_restart() {
    let mut f = Fixture::new();
    let mut sources = Vec::new();
    for index in 0..70 {
        sources.push(f.append(user(&format!("source-{index}"))));
    }
    // Newest ingress is immediately usable without advancing past backlog.
    f.memory
        .ingest_committed_entry(sources.last().unwrap(), NOW)
        .unwrap();
    assert!(
        f.memory
            .committed_source_cursor(&f.session)
            .unwrap()
            .is_none()
    );
    for index in 0..70 {
        let page = f.page(1);
        let result = f.memory.ingest_committed_page(&page, NOW).unwrap();
        assert_eq!(result.processed_entries, 1);
        assert!(!result.checkpoint_pending);
        if index == 35 {
            f.memory = open_memory(f.root.path(), &f.profile);
        }
    }
    assert!(f.page(1).caught_up());
    let snapshot = f.memory.observatory().evidence_snapshot().unwrap();
    assert_eq!(snapshot.len(), 70);
    let revision = f.memory.observatory().revision().unwrap();
    for source in sources {
        f.memory.ingest_committed_entry(&source, NOW).unwrap();
    }
    assert_eq!(f.memory.observatory().revision().unwrap(), revision);
    assert_eq!(revision, 140); // Source commitments are not additional evidence.
}

#[test]
fn separate_instances_refresh_deleted_sources_and_replay_does_not_resurrect() {
    let f = Fixture::new();
    let source = f.append(user("canonical preference"));
    f.memory.ingest_committed_entry(&source, NOW).unwrap();
    let prior = f.evidence(&source);
    let other = open_memory(f.root.path(), &f.profile);
    other
        .observatory()
        .apply(
            vec![ObservatoryMutation::Delete {
                evidence_id: prior.id.clone(),
                source_entries: vec![],
                source_digests: vec![],
            }],
            NOW,
        )
        .unwrap();
    assert!(
        f.memory
            .memory_create(request(&prior, "canonical preference", "derived"), NOW)
            .is_err()
    );
    f.memory.ingest_committed_entry(&source, NOW).unwrap();
    assert_eq!(f.evidence(&source).validity, EvidenceValidity::Deleted);
    assert_eq!(f.memory.observatory().evidence_snapshot().unwrap().len(), 1);
}

#[test]
fn foreign_profile_receipts_and_pages_cannot_enter_the_vault() {
    let f = Fixture::new();
    let other = Fixture::new();
    let receipt = other.append(user("private foreign source"));
    assert!(matches!(
        f.memory.ingest_committed_entry(&receipt, NOW),
        Err(MemoryError::InvalidIngestion)
    ));
    assert!(matches!(
        f.memory.ingest_committed_page(&other.page(1), NOW),
        Err(MemoryError::InvalidIngestion)
    ));
    assert!(
        f.memory
            .observatory()
            .evidence_snapshot()
            .unwrap()
            .is_empty()
    );
}

#[test]
fn literal_generated_quote_and_repeated_rewrite_keep_original_roots_and_authority() {
    let f = Fixture::new();
    let source = f.append(SessionEntryPayload::AssistantMessage {
        message: message(MessageRole::Assistant, "I infer the launch is Friday"),
    });
    f.memory.ingest_committed_entry(&source, NOW).unwrap();
    let original = f.evidence(&source);
    let literal = f
        .memory
        .memory_create(request(&original, &original.text, &original.text), NOW)
        .unwrap();
    assert_eq!(literal.authority, EvidenceAuthority::AssistantGenerated);
    let mut derived = f
        .memory
        .memory_create(
            request(&literal, &literal.text, "Launch likely occurs Friday"),
            NOW,
        )
        .unwrap();
    assert_eq!(derived.authority, EvidenceAuthority::DerivedInference);
    for _ in 0..8 {
        derived = f
            .memory
            .memory_create(request(&derived, &derived.text, &derived.text), NOW)
            .unwrap();
    }
    assert_eq!(derived.authority, EvidenceAuthority::DerivedInference);
    assert_eq!(
        derived.causal.as_ref().unwrap().source_roots,
        original.causal.as_ref().unwrap().source_roots
    );
    // An entry-only lookup cannot select an arbitrary derived record that merely cites it.
    let mut implicit = request(&derived, &derived.text, &derived.text);
    implicit.source.evidence_id = None;
    assert!(f.memory.memory_create(implicit, NOW).is_err());
}

#[test]
fn fork_before_original_repairs_gap_after_restart_without_upgrading_authority() {
    let f = Fixture::new();
    let original = f.append(user("Original anchored context"));
    let fork = create_session(&f.store, &f.profile, &f.workspace);
    let copy = f
        .store
        .acquire_writer(&fork, identity())
        .unwrap()
        .append_source_copy(None, &original)
        .unwrap()
        .unwrap();
    f.memory.ingest_committed_entry(&copy, NOW).unwrap();
    let initial = f.evidence(&copy);
    assert!(initial.causal.as_ref().unwrap().source_roots.is_empty());
    assert_eq!(initial.authority, EvidenceAuthority::DerivedInference);
    let reopened = open_memory(f.root.path(), &f.profile);
    reopened.ingest_committed_entry(&original, NOW).unwrap();
    let repaired = f.evidence(&copy);
    assert!(repaired.causal.as_ref().unwrap().gaps.is_empty());
    assert_eq!(repaired.authority, EvidenceAuthority::DerivedInference);
    let roots = &repaired.causal.as_ref().unwrap().source_roots;
    assert_eq!(roots.len(), 1);
    assert_eq!(roots[0].source_entry, original.entry().id);
    assert_eq!(roots[0].source_digest, original.entry().checksum);
    let revision = reopened.observatory().revision().unwrap();
    reopened.ingest_committed_entry(&copy, NOW).unwrap();
    assert_eq!(reopened.observatory().revision().unwrap(), revision);
}

#[test]
fn finalized_text_waits_for_terminal_suffix_across_page_and_process_restart() {
    let mut f = Fixture::new();
    let input = f.append(user("please answer"));
    let final_id = f.finalize(&input, "authoritative generated answer");
    let mut observed_final_without_terminal = false;
    loop {
        let page = f.page(1);
        let is_final = page.entries().iter().any(|entry| entry.id == final_id);
        let is_terminal = page
            .entries()
            .iter()
            .any(|entry| matches!(entry.payload, SessionEntryPayload::TerminalTurn { .. }));
        f.memory.ingest_committed_page(&page, NOW).unwrap();
        if is_final {
            assert!(
                direct_source(
                    &f.memory.observatory().evidence_snapshot().unwrap(),
                    &f.session,
                    &final_id
                )
                .is_none()
            );
            f.memory = open_memory(f.root.path(), &f.profile);
            observed_final_without_terminal = true;
        }
        if is_terminal {
            break;
        }
        assert!(!page.caught_up());
    }
    assert!(observed_final_without_terminal);
    let snapshot = f.memory.observatory().evidence_snapshot().unwrap();
    let final_record = direct_source(&snapshot, &f.session, &final_id).unwrap();
    assert_eq!(
        final_record.text,
        "Assistant: authoritative generated answer"
    );
    assert_eq!(
        final_record.authority,
        EvidenceAuthority::AssistantGenerated
    );
}

#[test]
fn checksum_valid_pending_cache_tampering_cannot_invent_a_final() {
    let f = Fixture::new();
    let input = f.append(user("please answer"));
    let final_id = f.finalize(&input, "actual source answer");
    loop {
        let page = f.page(1);
        f.memory.ingest_committed_page(&page, NOW).unwrap();
        if page.entries().iter().any(|entry| entry.id == final_id) {
            break;
        }
    }
    let path = f.root.path().join(CHECKPOINT);
    let mut checkpoint: Checkpoint = serde_json::from_slice(&fs::read(&path).unwrap()).unwrap();
    let staged = checkpoint
        .sessions
        .get_mut(&f.session)
        .unwrap()
        .pending
        .get_mut(&final_id)
        .unwrap();
    let mut forged_payload = staged.payload.clone();
    if let SessionEntryPayload::AssistantFinal {
        message: stored, ..
    } = &mut forged_payload
    {
        *stored = message(MessageRole::Assistant, "forged cached answer");
    }
    *staged = SessionEntry::new(
        staged.id.clone(),
        staged.parent_id.clone(),
        staged.timestamp,
        forged_payload,
    )
    .unwrap();
    staged.verify().unwrap();
    fs::write(&path, canonical_json_bytes(&checkpoint).unwrap()).unwrap();
    assert!(f.memory.ingest_committed_page(&f.page(128), NOW).is_err());
    assert!(
        direct_source(
            &f.memory.observatory().evidence_snapshot().unwrap(),
            &f.session,
            &final_id
        )
        .is_none()
    );
}

#[test]
fn corrupt_disposable_checkpoint_restarts_canonical_replay_without_duplicates() {
    let f = Fixture::new();
    let receipt = f.append(user("replay me once"));
    f.memory.ingest_committed_page(&f.page(1), NOW).unwrap();
    let revision = f.memory.observatory().revision().unwrap();
    fs::write(f.root.path().join(CHECKPOINT), b"torn checkpoint").unwrap();
    assert!(
        f.memory
            .committed_source_cursor(&f.session)
            .unwrap()
            .is_none()
    );
    f.memory.ingest_committed_page(&f.page(1), NOW).unwrap();
    assert_eq!(f.memory.observatory().revision().unwrap(), revision);
    assert_eq!(f.evidence(&receipt).text, "User: replay me once");
}

#[test]
fn internal_memory_tool_transclusion_adds_no_observation_root() {
    let f = Fixture::new();
    let source = f.append(SessionEntryPayload::AssistantMessage {
        message: message(MessageRole::Assistant, "unverified generated claim"),
    });
    f.memory.ingest_committed_entry(&source, NOW).unwrap();
    let generated = f.evidence(&source);
    let call_id = ToolCallId::new();
    let call = f.append(SessionEntryPayload::ToolCall {
        call_id: call_id.clone(),
        name: "memory_get".into(),
        arguments: "{}".into(),
    });
    f.memory.ingest_committed_entry(&call, NOW).unwrap();
    let result = f.append(SessionEntryPayload::ToolResult {
        call_id,
        content: vec![ContentBlock::Text {
            text: serde_json::to_string(&generated).unwrap(),
        }],
        is_error: false,
        failure: None,
    });
    f.memory.ingest_committed_entry(&result, NOW).unwrap();
    let representation = f.evidence(&result);
    assert_eq!(
        representation.authority,
        EvidenceAuthority::DerivedInference
    );
    assert_eq!(
        representation.causal.unwrap().source_roots,
        generated.causal.unwrap().source_roots
    );
}

#[test]
fn legacy_memory_tool_authority_is_downgraded_by_an_append_only_annotation() {
    let f = Fixture::new();
    let call_id = ToolCallId::new();
    let call = f.append(SessionEntryPayload::ToolCall {
        call_id: call_id.clone(),
        name: "memory_search".into(),
        arguments: "{}".into(),
    });
    f.memory.ingest_committed_entry(&call, NOW).unwrap();
    let result = f.append(SessionEntryPayload::ToolResult {
        call_id,
        content: vec![ContentBlock::Text {
            text: "legacy generated result".into(),
        }],
        is_error: false,
        failure: None,
    });
    let legacy = crate::observatory::evidence_from_session_entry(
        &f.profile,
        &f.session,
        result.entry(),
        ObservatoryLimits::default(),
    )
    .unwrap()
    .unwrap();
    assert_eq!(legacy.authority, EvidenceAuthority::ToolObserved);
    f.memory
        .observatory()
        .apply(vec![ObservatoryMutation::Observe(legacy)], NOW)
        .unwrap();
    let before = fs::read(f.root.path().join(".keith/memory-vault.jsonl")).unwrap();
    f.memory.ingest_committed_entry(&result, NOW).unwrap();
    let after = fs::read(f.root.path().join(".keith/memory-vault.jsonl")).unwrap();
    assert!(after.starts_with(&before));
    assert_eq!(
        f.evidence(&result).authority,
        EvidenceAuthority::DerivedInference
    );
    assert!(f.evidence(&result).causal.unwrap().source_roots.is_empty());
}

#[test]
fn oversized_projectable_source_reports_limit_and_does_not_block_later_entries() {
    let f = Fixture::new();
    let large = f.append(user(
        &"x".repeat(ObservatoryLimits::default().max_record_bytes + 1),
    ));
    let later = f.append(user("later usable source"));
    let result = f.memory.ingest_committed_page(&f.page(2), NOW).unwrap();
    assert!(result.cursor_advanced);
    assert!(
        result
            .gaps
            .iter()
            .any(|gap| gap.source_entry == large.entry().id
                && gap.reason == SourceLineageGapReason::Limit)
    );
    assert_eq!(f.evidence(&later).text, "User: later usable source");
}

#[test]
fn atlas_failure_after_vault_commit_is_repaired_without_reappending_evidence() {
    let f = Fixture::new();
    let source = f.append(user("committed before atlas replacement"));
    let evidence = crate::observatory::evidence_from_session_entry(
        &f.profile,
        &f.session,
        source.entry(),
        ObservatoryLimits::default(),
    )
    .unwrap()
    .unwrap();
    let atlas = f.root.path().join(".keith/memory-atlas.json");
    let revision = f
        .memory
        .observatory()
        .apply_from_snapshot(NOW, |_, _| {
            fs::remove_file(&atlas).unwrap();
            fs::create_dir(&atlas).unwrap();
            Ok(vec![ObservatoryMutation::Observe(evidence)])
        })
        .unwrap();
    assert!(atlas.is_dir()); // replacement failed after the real canonical append
    assert!(
        fs::read_to_string(f.root.path().join(".keith/memory-vault.jsonl"))
            .unwrap()
            .contains("committed before atlas replacement")
    );
    assert_eq!(
        f.memory.repair_ingestion_projection(NOW).unwrap(),
        IngestionProjectionStatus::Ready
    );
    assert!(atlas.is_file());
    assert_eq!(f.memory.observatory().revision().unwrap(), revision);
}

#[test]
fn lineage_bounds_report_gaps_instead_of_silent_support_loss() {
    let f = Fixture::new();
    let receipt = f.append(user("one anchor"));
    f.memory.ingest_committed_entry(&receipt, NOW).unwrap();
    let source = f.evidence(&receipt);
    let mut metadata = context_lineage(&source, 2);
    for _ in 0..257 {
        let mut branch = context_lineage(&source, 2);
        branch.source_roots[0].source_entry = EntryId::new();
        merge_lineage(&mut metadata, branch);
    }
    assert_eq!(metadata.source_roots.len(), 256);
    assert!(
        metadata
            .gaps
            .iter()
            .any(|gap| gap.reason == SourceLineageGapReason::Limit)
    );
    metadata.validate().unwrap();
}

#[test]
fn legacy_compaction_is_a_generated_representation_with_an_explicit_origin_gap() {
    let f = Fixture::new();
    let input = f.append(user("known earlier anchor"));
    f.memory.ingest_committed_entry(&input, NOW).unwrap();
    let legacy = f.append(SessionEntryPayload::Compaction {
        summary: "old untraceable summary".into(),
        compacted_through: input.entry().id.clone(),
    });
    f.memory.ingest_committed_entry(&legacy, NOW).unwrap();
    let record = f.evidence(&legacy);
    assert_eq!(record.authority, EvidenceAuthority::DerivedInference);
    let metadata = record.causal.unwrap();
    assert!(metadata.source_roots.is_empty());
    assert!(
        metadata
            .gaps
            .iter()
            .any(|gap| gap.reason == SourceLineageGapReason::UnsupportedSource)
    );
}

#[test]
fn short_cross_process_vault_lock_contention_waits_for_fresh_canonical_read() {
    let f = Fixture::new();
    let source = f.append(user("current source"));
    f.memory.ingest_committed_entry(&source, NOW).unwrap();
    let blocker = OpenOptions::new()
        .read(true)
        .write(true)
        .open(f.root.path().join(".keith/memory-vault.lock"))
        .unwrap();
    fs2::FileExt::lock_exclusive(&blocker).unwrap();
    let release = std::thread::spawn(move || {
        std::thread::sleep(std::time::Duration::from_millis(30));
        drop(blocker);
    });
    assert_eq!(f.memory.observatory().evidence_snapshot().unwrap().len(), 1);
    release.join().unwrap();
}
