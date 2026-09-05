use keith_memory::{EvidenceAuthority, EvidenceSourceKind};

use super::memory_intake::{MAX_REPLAY_PAGES, REPLAY_PAGE_BYTES, REPLAY_PAGE_ENTRIES};
use super::tests::{ProviderServer, response, responses_text_stream, seed_provider_credential};
use super::*;

struct Fixture {
    _root: tempfile::TempDir,
    data: PathBuf,
    credentials: PathBuf,
    workspace: PathBuf,
    base_url: String,
}

impl Fixture {
    fn new(base_url: &str) -> Self {
        let root = tempfile::tempdir().unwrap();
        let fixture = Self {
            data: root.path().join("data"),
            credentials: root.path().join("credentials"),
            workspace: root.path().join("workspace"),
            base_url: base_url.into(),
            _root: root,
        };
        seed_provider_credential(
            &fixture.credentials,
            [91; 32],
            "openai",
            "synthetic-intake-test-credential",
        );
        fixture
    }

    fn open(&self, root_scope: Option<RootTreeId>) -> LocalRuntime {
        LocalRuntime::open(LocalRuntimeConfig {
            data_root: self.data.clone(),
            credential_root: self.credentials.clone(),
            credential_key: MasterKey::from_bytes([91; 32]),
            workspace_root: self.workspace.clone(),
            openai_base_url: self.base_url.clone(),
            anthropic_base_url: self.base_url.clone(),
            provider_base_urls: BTreeMap::new(),
            root_scope,
            worker_id: WorkerId::new(),
            owner_instance: EntityId::new(),
        })
        .unwrap()
    }
}

fn session(runtime: &LocalRuntime) -> SessionManifest {
    let profile = runtime.registered_profiles().unwrap().remove(0);
    runtime
        .create_session(&profile.profile.id, &profile.profile.workspace_id, None)
        .unwrap()
}

fn message(role: StoredMessageRole, text: &str) -> StoredMessage {
    StoredMessage {
        role,
        content: vec![StoredContentBlock::Text { text: text.into() }],
        provider_metadata: BTreeMap::new(),
    }
}

fn append_user(runtime: &LocalRuntime, session: &SessionManifest, text: &str) -> SessionEntry {
    let mut writer = runtime
        .sessions
        .acquire_writer(
            &session.session_id,
            runtime.writer_identity(Generation::new(1), UtcTimestamp::UNIX_EPOCH),
        )
        .unwrap();
    writer
        .append(
            writer.manifest().active_leaf.clone(),
            UtcTimestamp::UNIX_EPOCH,
            SessionEntryPayload::UserMessage {
                message: message(StoredMessageRole::User, text),
            },
        )
        .unwrap()
}

fn stored_turn(
    runtime: &LocalRuntime,
    session: &SessionManifest,
    finalize: bool,
) -> (SessionEntry, EntryId, Option<EntryId>) {
    let user = append_user(
        runtime,
        session,
        "Remember the synthetic intake observation.",
    );
    let mut writer = runtime
        .sessions
        .acquire_writer(
            &session.session_id,
            runtime.writer_identity(Generation::new(1), UtcTimestamp::UNIX_EPOCH),
        )
        .unwrap();
    let turn_id = TurnId::new();
    let action_id = ActionId::new();
    writer
        .accept_turn(
            UtcTimestamp::UNIX_EPOCH,
            action_id.clone(),
            turn_id.clone(),
            user.id.clone(),
        )
        .unwrap();
    let candidate = writer
        .append_final_candidate(
            UtcTimestamp::UNIX_EPOCH,
            turn_id.clone(),
            message(
                StoredMessageRole::Assistant,
                "The attributable synthetic answer.",
            ),
            1,
            1,
            0,
        )
        .unwrap();
    let final_id = finalize.then(|| {
        writer
            .append_finalized_turn(
                UtcTimestamp::UNIX_EPOCH,
                &turn_id,
                message(
                    StoredMessageRole::Assistant,
                    "Fallback must not replace candidate.",
                ),
                TurnTerminalStatus::Completed,
                true,
                true,
                Some(action_id),
                Vec::new(),
                None,
            )
            .unwrap()
            .0
            .id
    });
    (user, candidate.id, final_id)
}

fn memory(runtime: &LocalRuntime, session: &SessionManifest) -> Arc<MemoryService> {
    let profile = runtime.profile(&session.profile_id).unwrap();
    Arc::clone(&runtime.profile_modules(&profile).unwrap().memory)
}

fn evidence(runtime: &LocalRuntime, session: &SessionManifest) -> Vec<EvidenceRecord> {
    memory(runtime, session)
        .observatory()
        .evidence_snapshot()
        .unwrap()
        .into_values()
        .collect()
}

fn replay(runtime: &LocalRuntime) -> memory_intake::MemoryReplayProgress {
    runtime.replay_memory_intake(&runtime.sessions().unwrap(), UtcTimestamp::now().unwrap())
}

fn provider_responses(final_text: &str) -> ProviderServer {
    ProviderServer::start(vec![
        response("application/json", r#"{"data":[{"id":"gpt-4.1-mini"}]}"#),
        response(
            "text/event-stream",
            &responses_text_stream(final_text, 5, 3),
        ),
    ])
}

#[test]
fn current_user_receipt_remains_usable_ahead_of_historical_replay() {
    let server = provider_responses("The current user source is available.");
    let fixture = Fixture::new(&server.base_url);
    let runtime = fixture.open(None);
    let session = session(&runtime);
    for _ in 0..=REPLAY_PAGE_ENTRIES {
        append_user(&runtime, &session, "An older independent observation.");
    }
    let snapshot = runtime
        .run_prompt(
            &session.session_id,
            "I prefer concise updates.",
            Generation::new(1),
        )
        .unwrap();
    assert_eq!(
        snapshot.terminal.as_ref().unwrap().status,
        ProjectionTurnTerminalStatus::Completed
    );
    let service = memory(&runtime, &session);
    assert!(
        service
            .committed_source_cursor(&session.session_id)
            .unwrap()
            .is_none()
    );
    let current = evidence(&runtime, &session)
        .into_iter()
        .find(|record| record.text == "User: I prefer concise updates.")
        .unwrap();
    assert_eq!(current.authority, EvidenceAuthority::UserAsserted);
    let profile = runtime.profile(&session.profile_id).unwrap();
    let tools = runtime
        .tool_manager(&profile, &session.session_id, "save preference")
        .unwrap();
    let mut invocation = ToolInvocation {
        call_id: keith_agent_types::ToolCallId::new(),
        name: "memory_create".into(),
        arguments: serde_json::json!({
            "source_entry_id": current.source_entries[0],
            "evidence_quote": "I prefer concise updates.",
            "text": "The user prefers concise updates.",
            "kind": "preference"
        }),
    };
    let created: EvidenceRecord = serde_json::from_slice(
        &keith_tool_core::ToolExecutor::execute(&tools, &invocation, &CancellationToken::default())
            .unwrap(),
    )
    .unwrap();
    assert_eq!(created.authority, EvidenceAuthority::DerivedInference);
    invocation.call_id = keith_agent_types::ToolCallId::new();
    invocation.arguments["source_evidence_id"] = serde_json::json!(created.id);
    invocation.arguments["evidence_quote"] = serde_json::json!(created.text);
    invocation.arguments["text"] = serde_json::json!("A subsequent generated interpretation.");
    assert!(
        keith_tool_core::ToolExecutor::execute(&tools, &invocation, &CancellationToken::default())
            .is_err()
    );
    invocation.call_id = keith_agent_types::ToolCallId::new();
    invocation.arguments["kind"] = serde_json::json!("project_context");
    let interpreted: EvidenceRecord = serde_json::from_slice(
        &keith_tool_core::ToolExecutor::execute(&tools, &invocation, &CancellationToken::default())
            .unwrap(),
    )
    .unwrap();
    assert_eq!(interpreted.authority, EvidenceAuthority::DerivedInference);
    invocation.arguments["source_evidence_id"] = serde_json::json!("malformed-evidence-id");
    assert!(
        keith_tool_core::ToolExecutor::execute(&tools, &invocation, &CancellationToken::default())
            .is_err()
    );
    let final_id = snapshot.terminal.unwrap().final_id;
    for _ in 0..3 {
        let progress = replay(&runtime);
        assert_eq!(progress.failed_sessions, 0);
        assert!(progress.entries_read <= REPLAY_PAGE_ENTRIES);
    }
    assert_eq!(
        evidence(&runtime, &session)
            .iter()
            .filter(|record| record.source_entries.contains(&final_id))
            .count(),
        1
    );
}

#[test]
fn actual_turn_suffix_reaches_memory_without_another_prompt_or_query() {
    let activity_event = serde_json::json!({"type":"response.output_text.delta","delta":"I will write the proof file now."});
    let tool_start = serde_json::json!({"type":"response.output_item.added","output_index":0,"item":{"type":"function_call","name":"write","arguments":""}});
    let tool_event = serde_json::json!({"type":"response.output_item.done","output_index":0,"item":{"type":"function_call","name":"write","arguments":"{\"path\":\"intake-proof.txt\",\"content\":\"actual tool output\"}"}});
    let final_event =
        serde_json::json!({"type":"response.output_text.delta","delta":"The file is written."});
    let completed = serde_json::json!({"type":"response.completed","response":{"usage":{"input_tokens":5,"output_tokens":3}}});
    let server = ProviderServer::start(vec![
        response("application/json", r#"{"data":[{"id":"gpt-4.1-mini"}]}"#),
        response(
            "text/event-stream",
            &format!(
                "data: {activity_event}\n\ndata: {tool_start}\n\ndata: {tool_event}\n\ndata: {completed}\n\n"
            ),
        ),
        response(
            "text/event-stream",
            &format!("data: {final_event}\n\ndata: {completed}\n\n"),
        ),
    ]);
    let fixture = Fixture::new(&server.base_url);
    let runtime = fixture.open(None);
    let session = session(&runtime);
    let snapshot = runtime
        .run_prompt(
            &session.session_id,
            "Write the synthetic intake proof file.",
            Generation::new(1),
        )
        .unwrap();
    let requests = (0..3)
        .map(|_| server.request().lines().next().unwrap().to_string())
        .collect::<Vec<_>>();
    assert_eq!(
        requests,
        [
            "GET /v1/models HTTP/1.1",
            "POST /v1/responses HTTP/1.1",
            "POST /v1/responses HTTP/1.1"
        ]
    );
    assert_eq!(
        snapshot.terminal.as_ref().unwrap().status,
        ProjectionTurnTerminalStatus::Completed,
        "{snapshot:?}; request lines: {requests:?}"
    );
    assert_eq!(
        fs::read(fixture.workspace.join("intake-proof.txt")).unwrap(),
        b"actual tool output"
    );
    runtime.maintain_runtime().unwrap();
    let records = evidence(&runtime, &session);
    let final_id = snapshot.terminal.as_ref().unwrap().final_id.clone();
    let final_record = records
        .iter()
        .find(|record| record.source_entries.contains(&final_id))
        .unwrap();
    assert_eq!(final_record.source_kind, EvidenceSourceKind::AssistantFinal);
    assert_eq!(
        final_record.authority,
        EvidenceAuthority::AssistantGenerated
    );
    assert!(
        records
            .iter()
            .any(|record| record.source_kind == EvidenceSourceKind::ToolResult)
    );
    let current = runtime
        .snapshot(&session.session_id, Generation::new(1), SessionState::Ready)
        .unwrap();
    assert_eq!(current.terminal, snapshot.terminal);
}

#[test]
fn checkpoint_io_failure_after_success_preserves_final_and_retries_intake() {
    let server = provider_responses("The final survives optional intake maintenance.");
    let fixture = Fixture::new(&server.base_url);
    let runtime = fixture.open(None);
    let session = session(&runtime);
    let snapshot = runtime
        .run_prompt(
            &session.session_id,
            "Return the synthetic final.",
            Generation::new(1),
        )
        .unwrap();
    assert_eq!(
        snapshot.terminal.as_ref().unwrap().status,
        ProjectionTurnTerminalStatus::Completed
    );
    assert!(
        snapshot
            .messages
            .iter()
            .any(|message| message.final_id.is_some()
                && message.text == "The final survives optional intake maintenance.")
    );
    let profile = runtime.profile(&session.profile_id).unwrap();
    let modules = runtime.profile_modules(&profile).unwrap();
    let checkpoint = modules
        .workspace
        .layout()
        .root
        .join(".keith/memory-source-cursors.json");
    fs::remove_file(&checkpoint).unwrap();
    fs::create_dir(&checkpoint).unwrap();
    assert_eq!(replay(&runtime).failed_sessions, 1);
    let after_failure = runtime
        .snapshot(&session.session_id, Generation::new(1), SessionState::Ready)
        .unwrap();
    assert_eq!(after_failure.terminal, snapshot.terminal);
    assert_eq!(after_failure.messages, snapshot.messages);
    fs::remove_dir(&checkpoint).unwrap();
    let recovered = replay(&runtime);
    assert_eq!(recovered.failed_sessions, 0);
    assert!(checkpoint.is_file());
    let final_id = snapshot.terminal.unwrap().final_id;
    let before_restart = evidence(&runtime, &session)
        .into_iter()
        .find(|record| record.source_entries.contains(&final_id))
        .unwrap();
    drop(modules);
    drop(runtime);
    let restarted = fixture.open(None);
    let finals = evidence(&restarted, &session)
        .into_iter()
        .filter(|record| record.source_entries.contains(&final_id))
        .collect::<Vec<_>>();
    assert_eq!(finals.len(), 1);
    assert_eq!(finals[0].id, before_restart.id);
}

#[test]
fn startup_replays_committed_history_without_a_surviving_hint() {
    let fixture = Fixture::new("http://127.0.0.1:65535");
    let runtime = fixture.open(None);
    let session = session(&runtime);
    let (_, candidate, final_id) = stored_turn(&runtime, &session, true);
    assert!(evidence(&runtime, &session).is_empty());
    drop(runtime);
    let restarted = fixture.open(None);
    let records = evidence(&restarted, &session);
    assert!(
        records
            .iter()
            .any(|record| record.source_entries.contains(final_id.as_ref().unwrap()))
    );
    assert!(
        !records
            .iter()
            .any(|record| record.source_entries.contains(&candidate))
    );
    assert_eq!(
        restarted
            .snapshot(&session.session_id, Generation::new(2), SessionState::Ready)
            .unwrap()
            .terminal
            .unwrap()
            .final_id,
        final_id.unwrap()
    );
}

#[test]
fn startup_finalization_schedules_the_committed_answer_not_its_candidate() {
    let fixture = Fixture::new("http://127.0.0.1:65535");
    let runtime = fixture.open(None);
    let session = session(&runtime);
    let (_, candidate, _) = stored_turn(&runtime, &session, false);
    drop(runtime);
    let restarted = fixture.open(None);
    let final_id = restarted
        .snapshot(&session.session_id, Generation::new(2), SessionState::Ready)
        .unwrap()
        .terminal
        .unwrap()
        .final_id;
    let records = evidence(&restarted, &session);
    assert!(
        records
            .iter()
            .any(|record| record.source_entries.contains(&final_id))
    );
    assert!(
        !records
            .iter()
            .any(|record| record.source_entries.contains(&candidate))
    );
}

#[test]
fn request_setup_failure_still_schedules_its_committed_fallback() {
    let server = ProviderServer::start(vec![response(
        "application/json",
        r#"{"data":[{"id":"gpt-4.1-mini"}]}"#,
    )]);
    let fixture = Fixture::new(&server.base_url);
    let runtime = fixture.open(None);
    let session = session(&runtime);
    let skills = fixture.workspace.join(".agents/skills");
    fs::create_dir_all(&skills).unwrap();
    fs::write(
        skills.join("invalid-skill-entry"),
        b"a package must be a directory",
    )
    .unwrap();
    let snapshot = runtime
        .run_prompt(
            &session.session_id,
            "The setup failure is synthetic.",
            Generation::new(1),
        )
        .unwrap();
    let terminal = snapshot.terminal.unwrap();
    assert_eq!(terminal.status, ProjectionTurnTerminalStatus::Failed);
    assert_eq!(replay(&runtime).failed_sessions, 0);
    assert!(
        evidence(&runtime, &session)
            .iter()
            .any(|record| record.source_entries.contains(&terminal.final_id))
    );
}

#[test]
fn replay_rotates_bounded_work_past_a_busy_hinted_session() {
    let fixture = Fixture::new("http://127.0.0.1:65535");
    let runtime = fixture.open(None);
    let mut sessions = Vec::new();
    for _ in 0..7 {
        let session = session(&runtime);
        append_user(
            &runtime,
            &session,
            "An independently recorded session observation.",
        );
        sessions.push(session);
    }
    let writer = runtime
        .sessions
        .acquire_writer(
            &sessions[0].session_id,
            runtime.writer_identity(Generation::new(1), UtcTimestamp::UNIX_EPOCH),
        )
        .unwrap();
    for _ in 0..3 {
        runtime.schedule_memory_intake(&sessions[0].session_id);
        let progress = replay(&runtime);
        assert!(progress.sessions_attempted <= MAX_REPLAY_PAGES);
        assert!(progress.pages_read <= MAX_REPLAY_PAGES);
        assert!(progress.entries_read <= MAX_REPLAY_PAGES * REPLAY_PAGE_ENTRIES);
        // Tiny first pages have no checkpoint-validation overhead.
        assert!(progress.bytes_read < MAX_REPLAY_PAGES * REPLAY_PAGE_BYTES);
    }
    for session in &sessions[1..] {
        assert!(
            memory(&runtime, session)
                .committed_source_cursor(&session.session_id)
                .unwrap()
                .is_some()
        );
    }
    assert!(
        memory(&runtime, &sessions[0])
            .committed_source_cursor(&sessions[0].session_id)
            .unwrap()
            .is_none()
    );
    drop(writer);
    runtime.schedule_memory_intake(&sessions[0].session_id);
    assert_eq!(replay(&runtime).failed_sessions, 0);
    assert!(
        memory(&runtime, &sessions[0])
            .committed_source_cursor(&sessions[0].session_id)
            .unwrap()
            .is_some()
    );
}

#[test]
fn startup_replay_respects_worker_root_scope() {
    let fixture = Fixture::new("http://127.0.0.1:65535");
    let runtime = fixture.open(None);
    let owned = session(&runtime);
    let other = session(&runtime);
    let own_entry = append_user(&runtime, &owned, "Only the assigned root is scanned.");
    let other_entry = append_user(&runtime, &other, "Another root must await its owner.");
    drop(runtime);
    let scoped = fixture.open(Some(owned.root_tree_id.clone()));
    let records = evidence(&scoped, &owned);
    assert!(
        records
            .iter()
            .any(|record| record.source_entries.contains(&own_entry.id))
    );
    assert!(
        !records
            .iter()
            .any(|record| record.source_entries.contains(&other_entry.id))
    );
    assert!(
        memory(&scoped, &owned)
            .committed_source_cursor(&other.session_id)
            .unwrap()
            .is_none()
    );
}

#[test]
fn forked_history_preserves_original_observation_roots() {
    let fixture = Fixture::new("http://127.0.0.1:65535");
    let runtime = fixture.open(None);
    let source = session(&runtime);
    let (user, _, _) = stored_turn(&runtime, &source, true);
    assert_eq!(replay(&runtime).failed_sessions, 0);
    let fork = runtime
        .fork_session_assigned(
            &source.session_id,
            &SessionId::new(),
            &RootTreeId::new(),
            None,
            Generation::new(1),
        )
        .unwrap();
    assert_eq!(replay(&runtime).failed_sessions, 0);
    let originals = evidence(&runtime, &source);
    let original = originals
        .iter()
        .find(|record| record.source_entries == vec![user.id.clone()])
        .unwrap();
    let copies = originals
        .iter()
        .filter(|record| {
            record.source_session == fork.session_id
                && record.source_kind == EvidenceSourceKind::UserMessage
        })
        .collect::<Vec<_>>();
    assert_eq!(copies.len(), 1);
    assert_eq!(copies[0].authority, EvidenceAuthority::UserAsserted);
    assert_eq!(
        copies[0].causal.as_ref().unwrap().source_roots,
        original.causal.as_ref().unwrap().source_roots
    );
    let forked = runtime.sessions.load_index(&fork.session_id).unwrap();
    let manifest = runtime.sessions.manifest(&fork.session_id).unwrap();
    let entries = forked
        .ancestry(manifest.active_leaf.as_ref().unwrap())
        .unwrap();
    assert!(entries.iter().any(|entry| {
        entry
            .copied_from
            .as_ref()
            .is_some_and(|reference| reference.entry_id == user.id)
    }));
}

#[test]
fn resumed_ingress_receipt_does_not_append_a_second_user_message() {
    let server = provider_responses("The accepted ingress was resumed.");
    let fixture = Fixture::new(&server.base_url);
    let runtime = fixture.open(None);
    let session = session(&runtime);
    let turn_id = TurnId::new();
    let action_id = ActionId::new();
    let text = "Resume this already accepted source.";
    let mut stored = message(StoredMessageRole::User, text);
    stored.provider_metadata = BTreeMap::from([
        ("accepted_action_id".into(), action_id.to_string()),
        ("turn_id".into(), turn_id.to_string()),
    ]);
    let ingress = {
        let mut writer = runtime
            .sessions
            .acquire_writer(
                &session.session_id,
                runtime.writer_identity(Generation::new(1), UtcTimestamp::UNIX_EPOCH),
            )
            .unwrap();
        let ingress = writer
            .append(
                None,
                UtcTimestamp::UNIX_EPOCH,
                SessionEntryPayload::UserMessage { message: stored },
            )
            .unwrap();
        writer
            .accept_turn(
                UtcTimestamp::UNIX_EPOCH,
                action_id.clone(),
                turn_id.clone(),
                ingress.id.clone(),
            )
            .unwrap();
        ingress
    };
    let snapshot = runtime
        .run_turn(
            &session.session_id,
            text,
            &[],
            Generation::new(1),
            &TurnIngress::User {
                source_id: "accepted-intake-test".into(),
                action_id: Some(action_id),
                turn_id: Some(turn_id),
                accepted_at: Some(UtcTimestamp::UNIX_EPOCH),
            },
            &mut NoRuntimeEvents,
        )
        .unwrap();
    assert_eq!(
        snapshot.terminal.as_ref().unwrap().status,
        ProjectionTurnTerminalStatus::Completed
    );
    assert!(
        evidence(&runtime, &session)
            .iter()
            .any(|record| record.source_entries == vec![ingress.id.clone()])
    );
    let manifest = runtime.sessions.manifest(&session.session_id).unwrap();
    let entries = runtime
        .sessions
        .load_index(&session.session_id)
        .unwrap()
        .ancestry(manifest.active_leaf.as_ref().unwrap())
        .unwrap();
    assert_eq!(
        entries
            .iter()
            .filter(|entry| matches!(entry.payload, SessionEntryPayload::UserMessage { .. }))
            .count(),
        1
    );
}
