//! Unit-test source setup through the actual durable session writer.
use crate::{MemoryPolicy, MemoryService};
use keith_agent_types::{
    EntityId, Generation, ProfileId, RootTreeId, SessionId, UtcTimestamp, WorkerId, WorkspaceId,
};
use keith_session_store::{NewSession, SessionEntry, SessionKind, SessionStore, WriterIdentity};
use keith_workspace::{PersonalWorkspace, PersonalWorkspaceLimits};
use std::path::Path;

pub(crate) fn ingest(
    root: &Path,
    profile: &ProfileId,
    session: &SessionId,
    entries: &[SessionEntry],
    now: UtcTimestamp,
) {
    let store = SessionStore::open(root.join("committed-test-sources")).unwrap();
    store
        .create(NewSession {
            kind: SessionKind::Root,
            session_id: session.clone(),
            root_tree_id: RootTreeId::new(),
            parent_session_id: None,
            profile_id: profile.clone(),
            workspace_id: WorkspaceId::new(),
            created_at: now,
            label: None,
            profile_snapshot: None,
        })
        .unwrap();
    let mut writer = store
        .acquire_writer(
            session,
            WriterIdentity {
                worker_id: WorkerId::new(),
                owner_instance: EntityId::new(),
                generation: Generation::new(1),
                acquired_at: now,
            },
        )
        .unwrap();
    let workspace = PersonalWorkspace::open(root, PersonalWorkspaceLimits::default(), now).unwrap();
    let memory = MemoryService::open(workspace, profile, MemoryPolicy::default()).unwrap();
    for entry in entries {
        let receipt = writer
            .append_committed_source(
                writer.manifest().active_leaf.clone(),
                entry.timestamp,
                entry.payload.clone(),
            )
            .unwrap();
        memory.ingest_committed_entry(&receipt, now).unwrap();
    }
}
