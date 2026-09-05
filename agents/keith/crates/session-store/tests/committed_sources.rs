use std::collections::BTreeMap;
use std::fs::{self, OpenOptions};
use std::io::Write;
use std::path::PathBuf;

use keith_agent_types::{
    EntityId, EntryId, Generation, ProfileId, RootTreeId, SessionId, TurnId, UtcTimestamp,
    WorkerId, WorkspaceId, canonical_json_bytes,
};
use keith_session_store::{
    CommittedSourceCursor, CommittedSourceLimits, ContentBlock, MessageRole, NewSession,
    SessionEntry, SessionEntryPayload, SessionKind, SessionStore, SessionStoreError, StoredMessage,
    WriterIdentity,
};

fn identity() -> WriterIdentity {
    WriterIdentity {
        worker_id: WorkerId::new(),
        owner_instance: EntityId::new(),
        generation: Generation::ZERO,
        acquired_at: UtcTimestamp::UNIX_EPOCH,
    }
}

fn new_session(profile: &ProfileId, workspace: &WorkspaceId) -> NewSession {
    NewSession {
        kind: SessionKind::Root,
        session_id: SessionId::new(),
        root_tree_id: RootTreeId::new(),
        parent_session_id: None,
        profile_id: profile.clone(),
        workspace_id: workspace.clone(),
        created_at: UtcTimestamp::UNIX_EPOCH,
        label: None,
        profile_snapshot: None,
    }
}

fn message(text: &str) -> SessionEntryPayload {
    SessionEntryPayload::UserMessage {
        message: StoredMessage {
            role: MessageRole::User,
            content: vec![ContentBlock::Text { text: text.into() }],
            provider_metadata: BTreeMap::new(),
        },
    }
}

struct Fixture {
    root: tempfile::TempDir,
    store: SessionStore,
    profile: ProfileId,
    workspace: WorkspaceId,
    session: SessionId,
    entries: Vec<SessionEntry>,
}

impl Fixture {
    fn new(count: usize) -> Self {
        let root = tempfile::tempdir().unwrap();
        let store = SessionStore::open(root.path()).unwrap();
        let profile = ProfileId::new();
        let workspace = WorkspaceId::new();
        let manifest = store.create(new_session(&profile, &workspace)).unwrap();
        let session = manifest.session_id;
        let mut writer = store.acquire_writer(&session, identity()).unwrap();
        let mut entries = Vec::new();
        for index in 0..count {
            let parent = writer.manifest().active_leaf.clone();
            entries.push(
                writer
                    .append_committed_source(
                        parent,
                        UtcTimestamp::UNIX_EPOCH,
                        message(&format!("entry-{index}")),
                    )
                    .unwrap()
                    .entry()
                    .clone(),
            );
        }
        drop(writer);
        Self {
            root,
            store,
            profile,
            workspace,
            session,
            entries,
        }
    }

    fn history(&self) -> PathBuf {
        self.root
            .path()
            .join("sessions")
            .join(self.session.to_string())
            .join("history.jsonl")
    }
}

#[test]
fn bounded_pages_reopen_in_append_order_without_rewriting_history() {
    let fixture = Fixture::new(70);
    let original = fs::read(fixture.history()).unwrap();
    let mut cursor = None;
    let mut found = Vec::new();
    let limits = CommittedSourceLimits {
        max_entries: 3,
        max_bytes: 4096,
    };
    loop {
        let store = SessionStore::open(fixture.root.path()).unwrap();
        let page = store
            .committed_source_page(&fixture.profile, &fixture.session, cursor.as_ref(), limits)
            .unwrap();
        assert_eq!(page.profile_id(), &fixture.profile);
        assert_eq!(page.workspace_id(), &fixture.workspace);
        assert_eq!(page.input_cursor(), cursor.as_ref());
        assert!(page.entries().len() <= limits.max_entries);
        assert!(page.bytes_read() <= 3 * (limits.max_bytes + 1));
        found.extend(page.entries().iter().cloned());
        cursor = page.next_cursor().map(|value| {
            serde_json::from_slice::<CommittedSourceCursor>(&serde_json::to_vec(value).unwrap())
                .unwrap()
        });
        if page.caught_up() {
            break;
        }
    }
    assert_eq!(found, fixture.entries);
    let page = fixture
        .store
        .committed_source_page(&fixture.profile, &fixture.session, cursor.as_ref(), limits)
        .unwrap();
    assert!(page.entries().is_empty() && page.caught_up());
    assert_eq!(page.next_cursor(), cursor.as_ref());
    assert_eq!(fs::read(fixture.history()).unwrap(), original);
}

#[test]
fn page_retains_branch_ancestry_and_current_leaf_separately() {
    let fixture = Fixture::new(1);
    let mut writer = fixture
        .store
        .acquire_writer(&fixture.session, identity())
        .unwrap();
    let root = fixture.entries[0].id.clone();
    let left = writer
        .append(
            Some(root.clone()),
            UtcTimestamp::UNIX_EPOCH,
            message("left"),
        )
        .unwrap();
    let right = writer
        .append(
            Some(root.clone()),
            UtcTimestamp::UNIX_EPOCH,
            message("right"),
        )
        .unwrap();
    writer.select_leaf(&left.id).unwrap();
    let page = writer
        .committed_source_page(&fixture.profile, None, CommittedSourceLimits::default())
        .unwrap();
    assert_eq!(
        page.entries()
            .iter()
            .map(|entry| &entry.id)
            .collect::<Vec<_>>(),
        vec![&root, &left.id, &right.id]
    );
    assert_eq!(page.active_leaf(), Some(&left.id));
    assert_eq!(page.entries()[2].parent_id.as_ref(), Some(&root));
}

#[test]
fn scope_busy_quarantine_and_unknown_cursor_versions_fail_closed() {
    let fixture = Fixture::new(2);
    let limits = CommittedSourceLimits {
        max_entries: 1,
        ..CommittedSourceLimits::default()
    };
    assert!(matches!(
        fixture
            .store
            .committed_source_page(&ProfileId::new(), &fixture.session, None, limits),
        Err(SessionStoreError::SourceScopeMismatch)
    ));
    let page = fixture
        .store
        .committed_source_page(&fixture.profile, &fixture.session, None, limits)
        .unwrap();
    let second = fixture
        .store
        .create(new_session(&fixture.profile, &fixture.workspace))
        .unwrap();
    assert!(matches!(
        fixture.store.committed_source_page(
            &fixture.profile,
            &second.session_id,
            page.next_cursor(),
            limits
        ),
        Err(SessionStoreError::InvalidSourceCursor)
    ));
    let mut encoded = serde_json::to_value(page.next_cursor().unwrap()).unwrap();
    encoded["version"] = serde_json::json!({"major": 99, "minor": 0});
    let cursor = serde_json::from_value(encoded).unwrap();
    assert!(matches!(
        fixture.store.committed_source_page(
            &fixture.profile,
            &fixture.session,
            Some(&cursor),
            limits
        ),
        Err(SessionStoreError::InvalidSourceCursor)
    ));
    let writer = fixture
        .store
        .acquire_writer(&fixture.session, identity())
        .unwrap();
    assert!(matches!(
        fixture
            .store
            .committed_source_page(&fixture.profile, &fixture.session, None, limits),
        Err(SessionStoreError::WriterLocked(_))
    ));
    assert!(
        writer
            .committed_source_page(&fixture.profile, None, limits)
            .is_ok()
    );
    drop(writer);
    fs::write(fixture.history().with_file_name("quarantine.json"), b"{}").unwrap();
    assert!(matches!(
        fixture
            .store
            .committed_source_page(&fixture.profile, &fixture.session, None, limits),
        Err(SessionStoreError::Quarantined(_))
    ));
}

#[test]
fn changed_checkpoint_truncation_and_forged_offsets_never_advance() {
    let fixture = Fixture::new(3);
    let original = fs::read(fixture.history()).unwrap();
    let limits = CommittedSourceLimits {
        max_entries: 2,
        ..CommittedSourceLimits::default()
    };
    let page = fixture
        .store
        .committed_source_page(&fixture.profile, &fixture.session, None, limits)
        .unwrap();
    let cursor = page.next_cursor().unwrap();
    for offset in [1, u64::MAX] {
        let mut encoded = serde_json::to_value(cursor).unwrap();
        encoded["offset"] = serde_json::json!(offset);
        let forged = serde_json::from_value(encoded).unwrap();
        assert!(
            fixture
                .store
                .committed_source_page(&fixture.profile, &fixture.session, Some(&forged), limits)
                .is_err()
        );
    }
    let second = &fixture.entries[1];
    let replacement = SessionEntry::new(
        second.id.clone(),
        second.parent_id.clone(),
        second.timestamp,
        message("changed"),
    )
    .unwrap();
    let mut changed = Vec::new();
    for entry in [&fixture.entries[0], &replacement, &fixture.entries[2]] {
        changed.extend(canonical_json_bytes(entry).unwrap());
        changed.push(b'\n');
    }
    fs::write(fixture.history(), changed).unwrap();
    assert!(matches!(
        fixture.store.committed_source_page(
            &fixture.profile,
            &fixture.session,
            Some(cursor),
            limits
        ),
        Err(SessionStoreError::InvalidSourceCursor)
    ));
    fs::write(fixture.history(), &original[..20]).unwrap();
    assert!(
        fixture
            .store
            .committed_source_page(&fixture.profile, &fixture.session, Some(cursor), limits)
            .is_err()
    );
    fs::write(fixture.history(), original).unwrap();
    let resumed = fixture
        .store
        .committed_source_page(&fixture.profile, &fixture.session, Some(cursor), limits)
        .unwrap();
    assert_eq!(resumed.entries(), &fixture.entries[2..]);
}

#[test]
fn oversized_corrupt_duplicate_and_torn_records_do_not_produce_receipts() {
    let fixture = Fixture::new(2);
    let original = fs::read(fixture.history()).unwrap();
    let limits = CommittedSourceLimits::default();
    assert!(matches!(
        fixture.store.committed_source_page(
            &fixture.profile,
            &fixture.session,
            None,
            CommittedSourceLimits {
                max_entries: 1,
                max_bytes: 1
            }
        ),
        Err(SessionStoreError::SourceReadLimit)
    ));
    OpenOptions::new()
        .append(true)
        .open(fixture.history())
        .unwrap()
        .write_all(b"{unfinished")
        .unwrap();
    assert!(
        fixture
            .store
            .committed_source_page(&fixture.profile, &fixture.session, None, limits)
            .is_err()
    );
    assert!(
        fs::read(fixture.history())
            .unwrap()
            .ends_with(b"{unfinished")
    );
    fs::write(fixture.history(), &original).unwrap();
    let mut corrupt = fixture.entries[1].clone();
    corrupt.payload = message("unchecked replacement");
    let mut bytes = canonical_json_bytes(&fixture.entries[0]).unwrap();
    bytes.push(b'\n');
    bytes.extend(canonical_json_bytes(&corrupt).unwrap());
    bytes.push(b'\n');
    fs::write(fixture.history(), bytes).unwrap();
    assert!(matches!(
        fixture
            .store
            .committed_source_page(&fixture.profile, &fixture.session, None, limits),
        Err(SessionStoreError::ChecksumMismatch(_))
    ));
    let mut duplicate = canonical_json_bytes(&fixture.entries[0]).unwrap();
    duplicate.push(b'\n');
    duplicate.extend(duplicate.clone());
    fs::write(fixture.history(), duplicate).unwrap();
    assert!(matches!(
        fixture
            .store
            .committed_source_page(&fixture.profile, &fixture.session, None, limits),
        Err(SessionStoreError::DuplicateEntry(_))
    ));
}

#[test]
fn exact_tail_lookup_prioritizes_newest_entries_and_exposes_bounded_absence() {
    let fixture = Fixture::new(70);
    let limits = CommittedSourceLimits {
        max_entries: 1,
        ..CommittedSourceLimits::default()
    };
    let last = fixture.entries.last().unwrap();
    let receipt = fixture
        .store
        .committed_source_entry(&fixture.profile, &fixture.session, &last.id, limits)
        .unwrap();
    assert_eq!(receipt.entry(), last);
    assert_eq!(receipt.reference().checksum, last.checksum);
    assert!(matches!(
        fixture.store.committed_source_entry(
            &fixture.profile,
            &fixture.session,
            &fixture.entries[0].id,
            limits
        ),
        Err(SessionStoreError::SourceLookupLimit)
    ));
    assert!(matches!(
        fixture.store.committed_source_entry(
            &fixture.profile,
            &fixture.session,
            &EntryId::new(),
            CommittedSourceLimits::default()
        ),
        Err(SessionStoreError::MissingEntry(_))
    ));
    let writer = fixture
        .store
        .acquire_writer(&fixture.session, identity())
        .unwrap();
    assert_eq!(
        writer
            .committed_source_entry(&fixture.profile, &last.id, limits)
            .unwrap()
            .entry(),
        last
    );
}

#[test]
fn copied_sources_preserve_original_identity_and_legacy_checksum_bytes() {
    let fixture = Fixture::new(2);
    let original = fs::read(fixture.history()).unwrap();
    let sources = fixture
        .store
        .committed_ancestry(&fixture.profile, &fixture.session, &fixture.entries[1].id)
        .unwrap();
    let fork = fixture
        .store
        .create(new_session(&fixture.profile, &fixture.workspace))
        .unwrap();
    let mut writer = fixture
        .store
        .acquire_writer(&fork.session_id, identity())
        .unwrap();
    for source in &sources {
        let copy = writer
            .append_source_copy(writer.manifest().active_leaf.clone(), source)
            .unwrap()
            .unwrap();
        assert_eq!(copy.entry().copied_from.as_ref(), Some(&source.reference()));
        assert_ne!(copy.entry().id, source.entry().id);
        assert_eq!(copy.entry().payload, source.entry().payload);
        copy.entry().verify().unwrap();
        let mut changed = copy.entry().clone();
        changed.copied_from = None;
        assert!(matches!(
            changed.verify(),
            Err(SessionStoreError::ChecksumMismatch(_))
        ));
    }
    drop(writer);
    assert_eq!(fixture.store.load_index(&fork.session_id).unwrap().len(), 2);
    assert_eq!(fs::read(fixture.history()).unwrap(), original);
    assert!(fixture.entries.iter().all(|entry| {
        !String::from_utf8(canonical_json_bytes(entry).unwrap())
            .unwrap()
            .contains("copied_from")
    }));
    let foreign = fixture
        .store
        .create(new_session(&ProfileId::new(), &fixture.workspace))
        .unwrap();
    let mut writer = fixture
        .store
        .acquire_writer(&foreign.session_id, identity())
        .unwrap();
    assert!(matches!(
        writer.append_source_copy(None, &sources[0]),
        Err(SessionStoreError::SourceScopeMismatch)
    ));
}

#[cfg(unix)]
#[test]
fn source_reader_rejects_symlink_history_and_lock_files() {
    let fixture = Fixture::new(1);
    let history = fixture.history();
    let renamed = history.with_file_name("original.jsonl");
    fs::rename(&history, &renamed).unwrap();
    std::os::unix::fs::symlink(&renamed, &history).unwrap();
    assert!(matches!(
        fixture.store.committed_source_page(
            &fixture.profile,
            &fixture.session,
            None,
            CommittedSourceLimits::default()
        ),
        Err(SessionStoreError::PathEscape)
    ));
    fs::remove_file(&history).unwrap();
    fs::rename(&renamed, &history).unwrap();
    let lock = history.with_file_name(".writer.lock");
    fs::remove_file(&lock).unwrap();
    std::os::unix::fs::symlink(&history, &lock).unwrap();
    assert!(matches!(
        fixture.store.committed_source_page(
            &fixture.profile,
            &fixture.session,
            None,
            CommittedSourceLimits::default()
        ),
        Err(SessionStoreError::PathEscape)
    ));
}

#[test]
fn fork_conversion_preserves_generated_content_and_excludes_final_candidates() {
    let fixture = Fixture::new(1);
    let mut source_writer = fixture
        .store
        .acquire_writer(&fixture.session, identity())
        .unwrap();
    let message = StoredMessage {
        role: MessageRole::Assistant,
        content: vec![ContentBlock::Text {
            text: "Generated interpretation, not an observation".into(),
        }],
        provider_metadata: BTreeMap::new(),
    };
    let activity = source_writer
        .append_committed_source(
            source_writer.manifest().active_leaf.clone(),
            UtcTimestamp::UNIX_EPOCH,
            SessionEntryPayload::AssistantActivity {
                turn_id: TurnId::new(),
                message: message.clone(),
            },
        )
        .unwrap();
    let candidate = source_writer
        .append_final_candidate(
            UtcTimestamp::UNIX_EPOCH,
            TurnId::new(),
            message.clone(),
            1,
            1,
            0,
        )
        .unwrap();
    let candidate = source_writer
        .committed_source_entry(
            &fixture.profile,
            &candidate.id,
            CommittedSourceLimits::default(),
        )
        .unwrap();
    let fork = fixture
        .store
        .create(new_session(&fixture.profile, &fixture.workspace))
        .unwrap();
    let mut writer = fixture
        .store
        .acquire_writer(&fork.session_id, identity())
        .unwrap();
    let copy = writer.append_source_copy(None, &activity).unwrap().unwrap();
    assert_eq!(
        copy.entry().payload,
        SessionEntryPayload::AssistantMessage { message }
    );
    assert_eq!(
        copy.entry().copied_from.as_ref(),
        Some(&activity.reference())
    );
    assert!(
        writer
            .append_source_copy(writer.manifest().active_leaf.clone(), &candidate)
            .unwrap()
            .is_none()
    );
    let page = writer
        .committed_source_page(&fixture.profile, None, CommittedSourceLimits::default())
        .unwrap();
    assert_eq!(page.entries().len(), 1);
    assert!(!format!("{page:?}").contains("Generated interpretation"));
    assert!(!format!("{copy:?}").contains("Generated interpretation"));
}
