use std::collections::{BTreeMap, VecDeque};
use std::fmt::Display;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::{Mutex, MutexGuard};

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, ChildId, EntityId, Generation, MessageId, Revision, SessionId,
    UtcTimestamp, WorkerId, WorkspaceId,
};
use keith_artifacts::{ArtifactScope, ArtifactService};
use keith_session::{
    AgentSession, AgentSessionHandle, JsonSessionWriter, LocalSessionServices, SessionCommand,
    SessionIdentity, SessionRunState,
};
use keith_session_store::{
    NewSession, SessionKind as StoreSessionKind, SessionStore, WriterIdentity,
};
use keith_state_store_core::{
    ChildMessageRepository, ChildRepository, VersionedRecord, WritePrecondition,
};

use crate::model::{
    ChildCancellation, ChildError, ChildMessage, ChildMessageKind, ChildMessageSender,
    ChildProjection, ChildRecord, ChildRetention, ChildSpec, ChildStatus, ChildWorkspaceMode,
    ParentAuthority, StoredChild, StoredMessage,
};

pub struct ChildCoordinator<R> {
    root: PathBuf,
    workspaces_root: PathBuf,
    artifacts_root: PathBuf,
    runtime_root: PathBuf,
    sessions: SessionStore,
    artifacts: Arc<ArtifactService>,
    repository: R,
    roots: Mutex<BTreeMap<SessionId, ParentAuthority>>,
    runtimes: Mutex<BTreeMap<ChildId, AgentSessionHandle>>,
    serial: Mutex<()>,
}

impl<R> ChildCoordinator<R>
where
    R: ChildRepository + ChildMessageRepository,
    <R as ChildRepository>::Error: Display,
    <R as ChildMessageRepository>::Error: Display,
{
    /// Opens durable child, runtime, artifact, and workspace roots.
    ///
    /// # Errors
    ///
    /// Returns an error when a root cannot be created or canonicalized.
    pub fn open(
        root: impl AsRef<Path>,
        repository: R,
        artifacts: Arc<ArtifactService>,
    ) -> Result<Self, ChildError> {
        fs::create_dir_all(root.as_ref())?;
        let root = fs::canonicalize(root.as_ref())?;
        let workspaces_root = create_owned_root(&root, "workspaces")?;
        let artifacts_root = create_owned_root(&root, "artifacts")?;
        let runtime_root = create_owned_root(&root, "runtime")?;
        let sessions = SessionStore::open(root.join("session-store"))?;
        Ok(Self {
            root,
            workspaces_root,
            artifacts_root,
            runtime_root,
            sessions,
            artifacts,
            repository,
            roots: Mutex::new(BTreeMap::new()),
            runtimes: Mutex::new(BTreeMap::new()),
            serial: Mutex::new(()),
        })
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    /// Registers daemon-authenticated authority for a root parent session.
    ///
    /// # Errors
    ///
    /// Returns an error for an invalid workspace or poisoned authority state.
    pub fn register_root(&self, mut authority: ParentAuthority) -> Result<(), ChildError> {
        let workspace = fs::canonicalize(&authority.workspace_root)?;
        if !workspace.is_dir() {
            return Err(ChildError::WorkspaceDenied);
        }
        authority.workspace_root = workspace;
        self.roots
            .lock()
            .map_err(|_| ChildError::LockPoisoned)?
            .insert(authority.session_id.clone(), authority);
        Ok(())
    }

    /// Creates an independent durable child session through the ordinary `AgentSession` actor.
    ///
    /// # Errors
    ///
    /// Returns an error for authority, recursion, workspace, persistence, or runtime failure.
    #[allow(clippy::too_many_lines)]
    pub fn create(&self, spec: ChildSpec, now: UtcTimestamp) -> Result<ChildRecord, ChildError> {
        spec.limits.validate()?;
        validate_spec(&spec)?;
        let _guard = self.lock()?;
        let existing = self.load_children()?;
        let authority = self.resolve_authority(&spec.parent_session_id, &existing)?;
        if !spec.requested_tools.is_subset(&authority.allowed_tools) {
            return Err(ChildError::ToolEscalation);
        }
        let parent_depth = existing
            .iter()
            .find(|child| child.record.session_id == spec.parent_session_id)
            .map_or(0, |child| child.record.depth);
        let depth = parent_depth
            .checked_add(1)
            .ok_or(ChildError::RecursiveLimit)?;
        if depth > spec.limits.max_depth {
            return Err(ChildError::RecursiveLimit);
        }
        let direct = existing
            .iter()
            .filter(|child| {
                child.record.parent_session_id == spec.parent_session_id
                    && child.record.status != ChildStatus::Archived
            })
            .count();
        let descendants = existing
            .iter()
            .filter(|child| child.record.root_tree_id == authority.root_tree_id)
            .count();
        if direct >= usize::from(spec.limits.max_direct_children)
            || descendants >= usize::try_from(spec.limits.max_descendants).unwrap_or(usize::MAX)
        {
            return Err(ChildError::RecursiveLimit);
        }
        let id = ChildId::new();
        let session_id = SessionId::new();
        let (workspace_id, workspace_root) = self.prepare_workspace(&id, &authority, &spec)?;
        let artifact_directory = self.artifacts_root.join(id.to_string());
        if let Err(error) = fs::create_dir(&artifact_directory) {
            if matches!(
                spec.workspace_mode,
                ChildWorkspaceMode::IsolatedCopy | ChildWorkspaceMode::DedicatedWorkspace
            ) {
                let _ = remove_owned_directory(&self.workspaces_root, &workspace_root);
            }
            return Err(error.into());
        }
        let mut record = ChildRecord {
            id: id.clone(),
            session_id: session_id.clone(),
            parent_session_id: spec.parent_session_id.clone(),
            origin_parent_session_id: spec.parent_session_id,
            root_tree_id: authority.root_tree_id.clone(),
            profile_id: authority.profile_id.clone(),
            workspace_id: workspace_id.clone(),
            objective: spec.objective,
            goal_id: keith_agent_types::GoalId::new(),
            provider: spec.provider,
            model: spec.model,
            artifact_directory,
            workspace_mode: spec.workspace_mode,
            workspace_root,
            allowed_tools: spec.requested_tools,
            limits: spec.limits,
            depth,
            status: ChildStatus::Starting,
            heartbeat_at: now,
            created_at: now,
            updated_at: now,
            terminal_at: None,
            terminal_summary: None,
            orphaned_at: None,
            cancellation: spec.cancellation,
            retention: spec.retention,
            revision: Revision::ZERO,
        };
        self.put_new_child(&record)?;
        let startup = (|| {
            self.sessions.create(NewSession {
                kind: StoreSessionKind::DurableChild,
                session_id: session_id.clone(),
                root_tree_id: authority.root_tree_id,
                parent_session_id: Some(record.parent_session_id.clone()),
                profile_id: authority.profile_id,
                workspace_id,
                created_at: now,
                label: Some(record.objective.clone()),
                profile_snapshot: None,
            })?;
            let identity = child_identity(&record);
            let handle = AgentSession::spawn(identity, self.services(&record), 64)?;
            handle.dispatch(SessionCommand::Transition(SessionRunState::Ready))?;
            handle.dispatch(SessionCommand::Transition(SessionRunState::Running))?;
            self.runtimes
                .lock()
                .map_err(|_| ChildError::LockPoisoned)?
                .insert(id.clone(), handle);
            Ok::<(), ChildError>(())
        })();
        let mut stored = StoredChild {
            record: record.clone(),
            storage_revision: Revision::ZERO,
        };
        match startup {
            Ok(()) => stored.record.status = ChildStatus::Running,
            Err(error) => {
                stored.record.status = ChildStatus::Failed;
                stored.record.terminal_at = Some(now);
                stored.record.terminal_summary = Some(format!("child startup failed: {error}"));
                self.persist_child(&mut stored, now)?;
                return Err(error);
            }
        }
        self.persist_child(&mut stored, now)?;
        record = stored.record;
        Ok(record)
    }

    /// Recovers every non-terminal child actor from its durable runtime snapshot.
    ///
    /// # Errors
    ///
    /// Returns an error for corrupt records, incompatible snapshots, or inaccessible state.
    pub fn recover_active(&self) -> Result<Vec<ChildId>, ChildError> {
        let _guard = self.lock()?;
        let children = self.load_children()?;
        let mut recovered = Vec::new();
        for child in children
            .into_iter()
            .filter(|child| !child.record.status.is_terminal())
        {
            let handle = AgentSession::recover(
                &child_identity(&child.record),
                self.services(&child.record),
                64,
            )?;
            self.runtimes
                .lock()
                .map_err(|_| ChildError::LockPoisoned)?
                .insert(child.record.id.clone(), handle);
            recovered.push(child.record.id);
        }
        Ok(recovered)
    }

    /// Returns one full client-safe projection without local filesystem paths.
    ///
    /// # Errors
    ///
    /// Returns an error for missing or corrupt records.
    pub fn projection(&self, id: &ChildId) -> Result<ChildProjection, ChildError> {
        let _guard = self.lock()?;
        let child = self.required_child(id)?;
        Ok(ChildProjection::from(&child.record))
    }

    /// Lists children directly owned by a parent.
    ///
    /// # Errors
    ///
    /// Returns an error for repository failures.
    pub fn list_parent(&self, parent: &SessionId) -> Result<Vec<ChildProjection>, ChildError> {
        let _guard = self.lock()?;
        let mut children = self
            .load_children()?
            .into_iter()
            .filter(|child| &child.record.parent_session_id == parent)
            .map(|child| ChildProjection::from(&child.record))
            .collect::<Vec<_>>();
        children.sort_by(|left, right| left.id.cmp(&right.id));
        Ok(children)
    }

    /// Dispatches through the same bounded actor used by root sessions.
    ///
    /// # Errors
    ///
    /// Returns an error for missing runtime or actor rejection.
    pub fn dispatch(&self, id: &ChildId, command: SessionCommand) -> Result<(), ChildError> {
        let runtime = self
            .runtimes
            .lock()
            .map_err(|_| ChildError::LockPoisoned)?
            .get(id)
            .cloned()
            .ok_or_else(|| ChildError::NotFound(id.clone()))?;
        runtime.dispatch(command)?;
        Ok(())
    }

    /// Updates a live child's durable heartbeat.
    ///
    /// # Errors
    ///
    /// Returns an error for missing/terminal child or persistence failure.
    pub fn heartbeat(&self, id: &ChildId, now: UtcTimestamp) -> Result<ChildRecord, ChildError> {
        let _guard = self.lock()?;
        let mut child = self.required_child(id)?;
        if child.record.status.is_terminal() {
            return Err(ChildError::InvalidState);
        }
        child.record.heartbeat_at = now;
        self.persist_child(&mut child, now)?;
        Ok(child.record)
    }

    /// Moves a live child between running and waiting without changing its actor identity.
    ///
    /// # Errors
    ///
    /// Returns an error for missing/terminal children or persistence failure.
    pub fn set_waiting(
        &self,
        id: &ChildId,
        waiting: bool,
        now: UtcTimestamp,
    ) -> Result<ChildRecord, ChildError> {
        let _guard = self.lock()?;
        let mut child = self.required_child(id)?;
        if !matches!(
            child.record.status,
            ChildStatus::Running | ChildStatus::Waiting
        ) {
            return Err(ChildError::InvalidState);
        }
        child.record.status = if waiting {
            ChildStatus::Waiting
        } else {
            ChildStatus::Running
        };
        self.persist_child(&mut child, now)?;
        Ok(child.record)
    }

    /// Stores a typed, stable, rate-limited parent/child message.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid scope, payload, artifact, rate, count, or persistence.
    pub fn send_message(
        &self,
        id: &ChildId,
        sender: ChildMessageSender,
        kind: ChildMessageKind,
        now: UtcTimestamp,
    ) -> Result<ChildMessage, ChildError> {
        let _guard = self.lock()?;
        let child = self.required_child(id)?.record;
        if child.status.is_terminal() {
            return Err(ChildError::InvalidState);
        }
        validate_message(&child, &kind)?;
        if let ChildMessageKind::Artifacts { references } = &kind {
            self.artifacts.child_deliverable(
                &ArtifactScope {
                    root_tree_id: child.root_tree_id.clone(),
                    session_id: child.session_id.clone(),
                    profile_id: child.profile_id.clone(),
                },
                child.id.clone(),
                references.clone(),
            )?;
        }
        let messages = self.load_messages(id)?;
        if messages.len() >= usize::try_from(child.limits.max_messages).unwrap_or(usize::MAX) {
            return Err(ChildError::MessageLimit);
        }
        let window_start = now
            .unix_millis()
            .saturating_sub(i64::try_from(child.limits.message_window_ms).unwrap_or(i64::MAX));
        let recent = messages
            .iter()
            .filter(|message| message.message.created_at.unix_millis() >= window_start)
            .count();
        if recent >= usize::try_from(child.limits.max_messages_per_window).unwrap_or(usize::MAX) {
            return Err(ChildError::MessageLimit);
        }
        let sequence = messages
            .iter()
            .map(|message| message.message.sequence)
            .max()
            .unwrap_or(0)
            .checked_add(1)
            .ok_or(ChildError::MessageLimit)?;
        let message = ChildMessage {
            id: MessageId::new(),
            child_id: child.id,
            parent_session_id: child.parent_session_id,
            child_session_id: child.session_id,
            sender,
            sequence,
            created_at: now,
            kind,
            revision: Revision::ZERO,
        };
        self.repository
            .put_child_message(encode_message(&message)?, WritePrecondition::Missing)
            .map_err(message_repository_error)?;
        Ok(message)
    }

    /// Lists durable messages in stable sequence order.
    ///
    /// # Errors
    ///
    /// Returns an error for repository or record-integrity failures.
    pub fn messages(&self, id: &ChildId) -> Result<Vec<ChildMessage>, ChildError> {
        let _guard = self.lock()?;
        self.required_child(id)?;
        Ok(self
            .load_messages(id)?
            .into_iter()
            .map(|stored| stored.message)
            .collect())
    }

    /// Marks a child complete or failed and stops its live actor.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid terminal state, empty summary, or persistence failure.
    pub fn finish(
        &self,
        id: &ChildId,
        status: ChildStatus,
        summary: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<ChildRecord, ChildError> {
        if !matches!(status, ChildStatus::Complete | ChildStatus::Failed) {
            return Err(ChildError::InvalidState);
        }
        let summary = summary.into();
        if summary.trim().is_empty() {
            return Err(ChildError::Invalid(
                "terminal summary cannot be empty".into(),
            ));
        }
        let _guard = self.lock()?;
        self.stop_runtime(id);
        let mut child = self.required_child(id)?;
        if child.record.status.is_terminal() {
            return Err(ChildError::InvalidState);
        }
        child.record.status = status;
        child.record.terminal_at = Some(now);
        child.record.terminal_summary = Some(summary);
        self.persist_child(&mut child, now)?;
        Ok(child.record)
    }

    /// Cancels a nonterminal child and stops its live actor.
    ///
    /// # Errors
    ///
    /// Returns an error for a missing/terminal child, empty summary, or persistence failure.
    pub fn cancel(
        &self,
        id: &ChildId,
        summary: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<ChildRecord, ChildError> {
        let summary = summary.into();
        if summary.trim().is_empty() {
            return Err(ChildError::Invalid(
                "cancellation summary cannot be empty".into(),
            ));
        }
        let _guard = self.lock()?;
        self.stop_runtime(id);
        let mut child = self.required_child(id)?;
        if child.record.status.is_terminal() {
            return Err(ChildError::InvalidState);
        }
        child.record.status = ChildStatus::Cancelled;
        child.record.terminal_at = Some(now);
        child.record.terminal_summary = Some(summary);
        self.persist_child(&mut child, now)?;
        Ok(child.record)
    }

    /// Applies parent cancellation policies recursively.
    ///
    /// # Errors
    ///
    /// Returns an error for repository or persistence failures.
    pub fn parent_unavailable(
        &self,
        parent: &SessionId,
        now: UtcTimestamp,
    ) -> Result<Vec<ChildId>, ChildError> {
        let _guard = self.lock()?;
        let mut changed = Vec::new();
        let mut queue = VecDeque::from([parent.clone()]);
        while let Some(parent_session) = queue.pop_front() {
            let direct = self
                .load_children()?
                .into_iter()
                .filter(|child| {
                    child.record.parent_session_id == parent_session
                        && !child.record.status.is_terminal()
                })
                .collect::<Vec<_>>();
            for mut child in direct {
                match child.record.cancellation {
                    ChildCancellation::Propagate => {
                        self.stop_runtime(&child.record.id);
                        child.record.status = ChildStatus::Cancelled;
                        child.record.terminal_at = Some(now);
                        child.record.terminal_summary = Some("parent became unavailable".into());
                        queue.push_back(child.record.session_id.clone());
                    }
                    ChildCancellation::DetachAsOrphan => {
                        child.record.status = ChildStatus::Orphaned;
                        child.record.orphaned_at = Some(now);
                    }
                }
                self.persist_child(&mut child, now)?;
                changed.push(child.record.id);
            }
        }
        Ok(changed)
    }

    /// Adopts a visible orphan under another authorized parent in the same profile/tree.
    ///
    /// # Errors
    ///
    /// Returns an error for non-orphans, scope mismatch, or recursion limit.
    pub fn adopt(
        &self,
        id: &ChildId,
        new_parent: &SessionId,
        now: UtcTimestamp,
    ) -> Result<ChildRecord, ChildError> {
        let _guard = self.lock()?;
        let all = self.load_children()?;
        let authority = self.resolve_authority(new_parent, &all)?;
        let mut child = all
            .into_iter()
            .find(|child| &child.record.id == id)
            .ok_or_else(|| ChildError::NotFound(id.clone()))?;
        if child.record.status != ChildStatus::Orphaned {
            return Err(ChildError::InvalidState);
        }
        if child.record.root_tree_id != authority.root_tree_id
            || child.record.profile_id != authority.profile_id
            || !child
                .record
                .allowed_tools
                .is_subset(&authority.allowed_tools)
        {
            return Err(ChildError::ScopeDenied);
        }
        let parent_depth = self
            .load_children()?
            .into_iter()
            .find(|candidate| candidate.record.session_id == *new_parent)
            .map_or(0, |candidate| candidate.record.depth);
        let depth = parent_depth
            .checked_add(1)
            .ok_or(ChildError::RecursiveLimit)?;
        if depth > child.record.limits.max_depth {
            return Err(ChildError::RecursiveLimit);
        }
        child.record.parent_session_id = new_parent.clone();
        child.record.depth = depth;
        child.record.status = ChildStatus::Running;
        child.record.orphaned_at = None;
        self.persist_child(&mut child, now)?;
        Ok(child.record)
    }

    /// Cancels orphans after their configured recovery grace period.
    ///
    /// # Errors
    ///
    /// Returns an error for repository or persistence failures.
    pub fn reap_orphans(&self, now: UtcTimestamp) -> Result<Vec<ChildId>, ChildError> {
        let _guard = self.lock()?;
        let mut reaped = Vec::new();
        for mut child in self.load_children()?.into_iter().filter(|child| {
            child.record.status == ChildStatus::Orphaned
                && child.record.orphaned_at.is_some_and(|orphaned| {
                    elapsed_ms(orphaned, now) >= child.record.limits.orphan_grace_ms
                })
        }) {
            self.stop_runtime(&child.record.id);
            child.record.status = ChildStatus::Cancelled;
            child.record.terminal_at = Some(now);
            child.record.terminal_summary = Some("orphan recovery grace elapsed".into());
            self.persist_child(&mut child, now)?;
            reaped.push(child.record.id);
        }
        Ok(reaped)
    }

    /// Fails live children whose heartbeat or total runtime limit elapsed.
    ///
    /// # Errors
    ///
    /// Returns an error for repository or persistence failures.
    pub fn enforce_liveness(&self, now: UtcTimestamp) -> Result<Vec<ChildId>, ChildError> {
        let _guard = self.lock()?;
        let mut failed = Vec::new();
        for mut child in self.load_children()?.into_iter().filter(|child| {
            !child.record.status.is_terminal()
                && (elapsed_ms(child.record.heartbeat_at, now)
                    >= child.record.limits.heartbeat_timeout_ms
                    || elapsed_ms(child.record.created_at, now)
                        >= child.record.limits.max_runtime_ms)
        }) {
            self.stop_runtime(&child.record.id);
            child.record.status = ChildStatus::Failed;
            child.record.terminal_at = Some(now);
            child.record.terminal_summary = Some("child liveness limit elapsed".into());
            self.persist_child(&mut child, now)?;
            failed.push(child.record.id);
        }
        Ok(failed)
    }

    /// Archives a terminal child and its independent session manifest.
    ///
    /// # Errors
    ///
    /// Returns an error for a live child or persistence failure.
    pub fn archive(&self, id: &ChildId, now: UtcTimestamp) -> Result<ChildRecord, ChildError> {
        let _guard = self.lock()?;
        let mut child = self.required_child(id)?;
        if !child.record.status.is_terminal() || child.record.status == ChildStatus::Archived {
            return Err(ChildError::InvalidState);
        }
        self.sessions
            .archive_session(&child.record.session_id, writer_identity(now))?;
        child.record.status = ChildStatus::Archived;
        self.persist_child(&mut child, now)?;
        Ok(child.record)
    }

    /// Permanently deletes an explicitly archived child, messages, and owned directories.
    ///
    /// # Errors
    ///
    /// Returns an error for non-archived children, unsafe paths, or persistence failure.
    pub fn delete(&self, id: &ChildId) -> Result<(), ChildError> {
        let _guard = self.lock()?;
        let child = self.required_child(id)?;
        if child.record.status != ChildStatus::Archived {
            return Err(ChildError::InvalidState);
        }
        for message in self.load_messages(id)? {
            self.repository
                .delete_child_message(
                    message.message.id.as_entity_id(),
                    WritePrecondition::Exact(message.message.revision),
                )
                .map_err(message_repository_error)?;
        }
        self.sessions.delete_archived(&child.record.session_id)?;
        remove_owned_directory(&self.artifacts_root, &child.record.artifact_directory)?;
        if matches!(
            child.record.workspace_mode,
            ChildWorkspaceMode::IsolatedCopy | ChildWorkspaceMode::DedicatedWorkspace
        ) {
            remove_owned_directory(&self.workspaces_root, &child.record.workspace_root)?;
        }
        let runtime_snapshot = self
            .runtime_root
            .join(format!("{}.json", child.record.session_id));
        match fs::remove_file(runtime_snapshot) {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
        self.repository
            .delete_child(
                child.record.id.as_entity_id(),
                WritePrecondition::Exact(child.storage_revision),
            )
            .map_err(child_repository_error)?;
        Ok(())
    }

    /// Applies configured archive/delete retention to terminal children.
    ///
    /// # Errors
    ///
    /// Returns an error for archive, deletion, or persistence failure.
    pub fn apply_retention(&self, now: UtcTimestamp) -> Result<Vec<ChildId>, ChildError> {
        let candidates = {
            let _guard = self.lock()?;
            self.load_children()?
                .into_iter()
                .filter_map(|child| {
                    let terminal = child.record.terminal_at?;
                    let elapsed = elapsed_ms(terminal, now);
                    let due = match child.record.retention {
                        ChildRetention::Retain => false,
                        ChildRetention::ArchiveAfter(delay)
                        | ChildRetention::DeleteAfter(delay) => elapsed >= delay,
                    };
                    due.then_some((child.record.id, child.record.retention, child.record.status))
                })
                .collect::<Vec<_>>()
        };
        let mut changed = Vec::new();
        for (id, retention, status) in candidates {
            match retention {
                ChildRetention::ArchiveAfter(_) if status != ChildStatus::Archived => {
                    self.archive(&id, now)?;
                    changed.push(id);
                }
                ChildRetention::DeleteAfter(_) => {
                    if status != ChildStatus::Archived {
                        self.archive(&id, now)?;
                    }
                    self.delete(&id)?;
                    changed.push(id);
                }
                ChildRetention::Retain | ChildRetention::ArchiveAfter(_) => {}
            }
        }
        Ok(changed)
    }

    fn resolve_authority(
        &self,
        session_id: &SessionId,
        children: &[StoredChild],
    ) -> Result<ParentAuthority, ChildError> {
        if let Some(child) = children.iter().find(|child| {
            &child.record.session_id == session_id && !child.record.status.is_terminal()
        }) {
            return Ok(ParentAuthority {
                session_id: session_id.clone(),
                root_tree_id: child.record.root_tree_id.clone(),
                profile_id: child.record.profile_id.clone(),
                workspace_id: child.record.workspace_id.clone(),
                workspace_root: child.record.workspace_root.clone(),
                allowed_tools: child.record.allowed_tools.clone(),
            });
        }
        self.roots
            .lock()
            .map_err(|_| ChildError::LockPoisoned)?
            .get(session_id)
            .cloned()
            .ok_or(ChildError::ScopeDenied)
    }

    fn prepare_workspace(
        &self,
        id: &ChildId,
        authority: &ParentAuthority,
        spec: &ChildSpec,
    ) -> Result<(WorkspaceId, PathBuf), ChildError> {
        match spec.workspace_mode {
            ChildWorkspaceMode::ReadOnlyParent | ChildWorkspaceMode::SharedParent => Ok((
                authority.workspace_id.clone(),
                authority.workspace_root.clone(),
            )),
            ChildWorkspaceMode::DedicatedWorkspace => {
                let path = self.workspaces_root.join(id.to_string());
                fs::create_dir(&path)?;
                Ok((WorkspaceId::new(), path))
            }
            ChildWorkspaceMode::IsolatedCopy => {
                let path = self.workspaces_root.join(id.to_string());
                fs::create_dir(&path)?;
                let copied = copy_tree_bounded(
                    &authority.workspace_root,
                    &path,
                    spec.limits.max_copy_bytes,
                    spec.limits.max_copy_files,
                );
                if let Err(error) = copied {
                    let _ = fs::remove_dir_all(&path);
                    return Err(error);
                }
                Ok((WorkspaceId::new(), path))
            }
        }
    }

    fn services(&self, child: &ChildRecord) -> keith_session::SessionServices {
        LocalSessionServices::new(
            JsonSessionWriter::new(&self.runtime_root),
            child.provider.clone(),
            child.model.clone(),
            child.allowed_tools.clone(),
            BTreeMap::new(),
        )
        .into_services()
    }

    fn stop_runtime(&self, id: &ChildId) {
        if let Ok(mut runtimes) = self.runtimes.lock()
            && let Some(runtime) = runtimes.remove(id)
        {
            let _ = runtime.dispatch(SessionCommand::Shutdown);
        }
    }

    fn required_child(&self, id: &ChildId) -> Result<StoredChild, ChildError> {
        self.repository
            .get_child(id.as_entity_id())
            .map_err(child_repository_error)?
            .map(decode_child)
            .transpose()?
            .ok_or_else(|| ChildError::NotFound(id.clone()))
    }

    fn load_children(&self) -> Result<Vec<StoredChild>, ChildError> {
        self.repository
            .list_children()
            .map_err(child_repository_error)?
            .into_iter()
            .map(decode_child)
            .collect()
    }

    fn load_messages(&self, id: &ChildId) -> Result<Vec<StoredMessage>, ChildError> {
        let mut messages = self
            .repository
            .list_child_messages()
            .map_err(message_repository_error)?
            .into_iter()
            .map(decode_message)
            .collect::<Result<Vec<_>, _>>()?
            .into_iter()
            .filter(|message| &message.message.child_id == id)
            .collect::<Vec<_>>();
        messages.sort_by_key(|message| message.message.sequence);
        Ok(messages)
    }

    fn put_new_child(&self, child: &ChildRecord) -> Result<(), ChildError> {
        self.repository
            .put_child(encode_child(child)?, WritePrecondition::Missing)
            .map_err(child_repository_error)?;
        Ok(())
    }

    fn persist_child(&self, child: &mut StoredChild, now: UtcTimestamp) -> Result<(), ChildError> {
        let revision = child
            .storage_revision
            .checked_next()
            .ok_or(ChildError::RevisionOverflow)?;
        child.record.revision = revision;
        child.record.updated_at = now;
        self.repository
            .put_child(
                encode_child(&child.record)?,
                WritePrecondition::Exact(child.storage_revision),
            )
            .map_err(child_repository_error)?;
        child.storage_revision = revision;
        Ok(())
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, ChildError> {
        self.serial.lock().map_err(|_| ChildError::LockPoisoned)
    }
}

fn validate_spec(spec: &ChildSpec) -> Result<(), ChildError> {
    if spec.objective.trim().is_empty()
        || spec.provider.trim().is_empty()
        || spec.model.trim().is_empty()
        || spec
            .requested_tools
            .iter()
            .any(|tool| tool.trim().is_empty())
    {
        Err(ChildError::Invalid(
            "objective, model route, and tool names must be non-empty".into(),
        ))
    } else {
        Ok(())
    }
}

fn validate_message(child: &ChildRecord, kind: &ChildMessageKind) -> Result<(), ChildError> {
    let bytes = serde_json::to_vec(kind)?;
    if bytes.len() > child.limits.max_message_bytes {
        return Err(ChildError::MessageLimit);
    }
    match kind {
        ChildMessageKind::Text { text }
        | ChildMessageKind::Request { request: text }
        | ChildMessageKind::Status { status: text }
            if text.trim().is_empty() =>
        {
            Err(ChildError::Invalid(
                "child message text cannot be empty".into(),
            ))
        }
        ChildMessageKind::Artifacts { references } => {
            if references.is_empty()
                || references.len() > child.limits.max_artifacts_per_message
                || references.iter().any(|reference| {
                    reference.root_tree_id != child.root_tree_id
                        || reference.profile_id != child.profile_id
                })
            {
                Err(ChildError::ScopeDenied)
            } else {
                Ok(())
            }
        }
        _ => Ok(()),
    }
}

fn child_identity(child: &ChildRecord) -> SessionIdentity {
    SessionIdentity::durable_child(
        child.session_id.clone(),
        child.root_tree_id.clone(),
        child.origin_parent_session_id.clone(),
        child.profile_id.clone(),
    )
}

fn writer_identity(now: UtcTimestamp) -> WriterIdentity {
    WriterIdentity {
        worker_id: WorkerId::new(),
        owner_instance: EntityId::new(),
        generation: Generation::ZERO,
        acquired_at: now,
    }
}

fn elapsed_ms(start: UtcTimestamp, now: UtcTimestamp) -> u64 {
    u64::try_from(now.unix_millis().saturating_sub(start.unix_millis())).unwrap_or(0)
}

fn create_owned_root(root: &Path, name: &str) -> Result<PathBuf, ChildError> {
    let path = root.join(name);
    fs::create_dir_all(&path)?;
    let path = fs::canonicalize(path)?;
    if path.parent() != Some(root) {
        return Err(ChildError::WorkspaceDenied);
    }
    Ok(path)
}

fn copy_tree_bounded(
    source: &Path,
    destination: &Path,
    max_bytes: u64,
    max_files: u32,
) -> Result<(), ChildError> {
    let mut pending = VecDeque::from([(source.to_path_buf(), destination.to_path_buf())]);
    let mut bytes = 0_u64;
    let mut files = 0_u32;
    while let Some((from, to)) = pending.pop_front() {
        for entry in fs::read_dir(from)? {
            let entry = entry?;
            let file_type = entry.file_type()?;
            if file_type.is_symlink() {
                return Err(ChildError::WorkspaceDenied);
            }
            let target = to.join(entry.file_name());
            if file_type.is_dir() {
                fs::create_dir(&target)?;
                pending.push_back((entry.path(), target));
            } else if file_type.is_file() {
                files = files.checked_add(1).ok_or(ChildError::WorkspaceDenied)?;
                bytes = bytes
                    .checked_add(entry.metadata()?.len())
                    .ok_or(ChildError::WorkspaceDenied)?;
                if files > max_files || bytes > max_bytes {
                    return Err(ChildError::WorkspaceDenied);
                }
                fs::copy(entry.path(), target)?;
            } else {
                return Err(ChildError::WorkspaceDenied);
            }
        }
    }
    Ok(())
}

fn remove_owned_directory(parent: &Path, path: &Path) -> Result<(), ChildError> {
    let canonical = fs::canonicalize(path)?;
    if canonical.parent() != Some(parent) {
        return Err(ChildError::WorkspaceDenied);
    }
    fs::remove_dir_all(canonical)?;
    Ok(())
}

fn encode_child(child: &ChildRecord) -> Result<VersionedRecord, ChildError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: child.id.as_entity_id().clone(),
        revision: child.revision,
        updated_at: child.updated_at,
        payload: serde_json::to_value(child)?,
    })
}

fn decode_child(record: VersionedRecord) -> Result<StoredChild, ChildError> {
    let child: ChildRecord = serde_json::from_value(record.payload)?;
    if child.id.as_entity_id() != &record.id
        || child.revision != record.revision
        || record.version.major != CURRENT_SCHEMA_VERSION.major
        || record.version.minor > CURRENT_SCHEMA_VERSION.minor
    {
        return Err(ChildError::Corrupt("child record envelope mismatch".into()));
    }
    Ok(StoredChild {
        record: child,
        storage_revision: record.revision,
    })
}

fn encode_message(message: &ChildMessage) -> Result<VersionedRecord, ChildError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: message.id.as_entity_id().clone(),
        revision: message.revision,
        updated_at: message.created_at,
        payload: serde_json::to_value(message)?,
    })
}

fn decode_message(record: VersionedRecord) -> Result<StoredMessage, ChildError> {
    let message: ChildMessage = serde_json::from_value(record.payload)?;
    if message.id.as_entity_id() != &record.id
        || message.revision != record.revision
        || record.version.major != CURRENT_SCHEMA_VERSION.major
        || record.version.minor > CURRENT_SCHEMA_VERSION.minor
    {
        return Err(ChildError::Corrupt(
            "child message envelope mismatch".into(),
        ));
    }
    Ok(StoredMessage { message })
}

fn child_repository_error(error: impl Display) -> ChildError {
    ChildError::Repository(error.to_string())
}

fn message_repository_error(error: impl Display) -> ChildError {
    ChildError::Repository(error.to_string())
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;
    use std::sync::Arc;
    use std::thread;

    use keith_artifacts::{ArtifactLimits, ArtifactSource, NewArtifact, RetentionPolicy};
    use keith_state_store::EmbeddedStore;
    use tempfile::TempDir;

    use super::*;
    use crate::model::ChildLimits;

    type Coordinator = ChildCoordinator<EmbeddedStore>;

    fn tools(values: &[&str]) -> BTreeSet<String> {
        values.iter().map(|value| (*value).to_owned()).collect()
    }

    fn artifact_service(root: &TempDir) -> Arc<ArtifactService> {
        Arc::new(ArtifactService::open(root.path(), ArtifactLimits::default()).unwrap())
    }

    fn coordinator(root: &TempDir, artifacts: Arc<ArtifactService>) -> Coordinator {
        let repository = EmbeddedStore::open(&root.path().join("children.sqlite"), None).unwrap();
        ChildCoordinator::open(root.path().join("children"), repository, artifacts).unwrap()
    }

    fn authority(workspace: &TempDir) -> ParentAuthority {
        ParentAuthority {
            session_id: SessionId::new(),
            root_tree_id: keith_agent_types::RootTreeId::new(),
            profile_id: keith_agent_types::ProfileId::new(),
            workspace_id: WorkspaceId::new(),
            workspace_root: workspace.path().to_path_buf(),
            allowed_tools: tools(&["read", "search", "write"]),
        }
    }

    fn spec(parent: &SessionId, mode: ChildWorkspaceMode) -> ChildSpec {
        ChildSpec {
            parent_session_id: parent.clone(),
            objective: "Complete an independent workstream".into(),
            workspace_mode: mode,
            requested_tools: tools(&["read", "search"]),
            provider: "local".into(),
            model: "test-model".into(),
            limits: ChildLimits::default(),
            cancellation: ChildCancellation::Propagate,
            retention: ChildRetention::Retain,
        }
    }

    #[test]
    fn all_workspace_modes_nested_sessions_and_authority_limits_are_real() {
        let root = TempDir::new().unwrap();
        let artifact_root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        fs::write(workspace.path().join("source.txt"), b"independent copy").unwrap();
        let coordinator = coordinator(&root, artifact_service(&artifact_root));
        let root_authority = authority(&workspace);
        coordinator.register_root(root_authority.clone()).unwrap();

        let isolated = coordinator
            .create(
                spec(&root_authority.session_id, ChildWorkspaceMode::IsolatedCopy),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        assert_ne!(isolated.workspace_root, root_authority.workspace_root);
        assert_eq!(
            fs::read(isolated.workspace_root.join("source.txt")).unwrap(),
            b"independent copy"
        );
        let read_only = coordinator
            .create(
                spec(
                    &root_authority.session_id,
                    ChildWorkspaceMode::ReadOnlyParent,
                ),
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        assert_eq!(read_only.workspace_root, root_authority.workspace_root);
        let shared = coordinator
            .create(
                spec(&root_authority.session_id, ChildWorkspaceMode::SharedParent),
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert_eq!(shared.workspace_id, root_authority.workspace_id);
        let dedicated = coordinator
            .create(
                spec(
                    &root_authority.session_id,
                    ChildWorkspaceMode::DedicatedWorkspace,
                ),
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        assert!(
            dedicated
                .workspace_root
                .read_dir()
                .unwrap()
                .next()
                .is_none()
        );

        let nested = coordinator
            .create(
                spec(&isolated.session_id, ChildWorkspaceMode::ReadOnlyParent),
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        assert_eq!(nested.depth, 2);
        assert_eq!(nested.root_tree_id, root_authority.root_tree_id);
        let mut escalation = spec(&nested.session_id, ChildWorkspaceMode::DedicatedWorkspace);
        escalation.requested_tools.insert("write".into());
        assert!(matches!(
            coordinator.create(escalation, UtcTimestamp::from_unix_millis(5)),
            Err(ChildError::ToolEscalation)
        ));
        let mut too_deep = spec(&nested.session_id, ChildWorkspaceMode::DedicatedWorkspace);
        too_deep.limits.max_depth = 2;
        assert!(matches!(
            coordinator.create(too_deep, UtcTimestamp::from_unix_millis(6)),
            Err(ChildError::RecursiveLimit)
        ));
        assert_eq!(read_only.status, ChildStatus::Running);
    }

    #[cfg(unix)]
    #[test]
    fn isolated_copy_rejects_symlinks() {
        use std::os::unix::fs::symlink;

        let root = TempDir::new().unwrap();
        let artifact_root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        symlink("/etc/passwd", workspace.path().join("escape")).unwrap();
        let coordinator = coordinator(&root, artifact_service(&artifact_root));
        let authority = authority(&workspace);
        coordinator.register_root(authority.clone()).unwrap();
        assert!(matches!(
            coordinator.create(
                spec(&authority.session_id, ChildWorkspaceMode::IsolatedCopy),
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(ChildError::WorkspaceDenied)
        ));
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn typed_messages_artifacts_rate_limits_and_parallel_results_are_durable() {
        let root = TempDir::new().unwrap();
        let artifact_root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        let artifacts = artifact_service(&artifact_root);
        let coordinator = Arc::new(coordinator(&root, Arc::clone(&artifacts)));
        let authority = authority(&workspace);
        coordinator.register_root(authority.clone()).unwrap();
        let mut first_spec = spec(
            &authority.session_id,
            ChildWorkspaceMode::DedicatedWorkspace,
        );
        first_spec.limits.max_messages_per_window = 4;
        let first = coordinator
            .create(first_spec, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let second = coordinator
            .create(
                spec(
                    &authority.session_id,
                    ChildWorkspaceMode::DedicatedWorkspace,
                ),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let scope = ArtifactScope {
            root_tree_id: first.root_tree_id.clone(),
            session_id: first.session_id.clone(),
            profile_id: first.profile_id.clone(),
        };
        let artifact = artifacts
            .create(NewArtifact {
                scope: scope.clone(),
                source: ArtifactSource::Child(first.id.clone()),
                media_type: "text/plain",
                bytes: b"child deliverable",
                created_at: UtcTimestamp::UNIX_EPOCH,
                display: None,
                retention: RetentionPolicy::Retain,
            })
            .unwrap();
        coordinator
            .send_message(
                &first.id,
                ChildMessageSender::Parent,
                ChildMessageKind::Request {
                    request: "Return a concise result".into(),
                },
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        coordinator
            .send_message(
                &first.id,
                ChildMessageSender::Child,
                ChildMessageKind::Text {
                    text: "Work is complete".into(),
                },
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        coordinator
            .send_message(
                &first.id,
                ChildMessageSender::Child,
                ChildMessageKind::Status {
                    status: "ready".into(),
                },
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        coordinator
            .send_message(
                &first.id,
                ChildMessageSender::Child,
                ChildMessageKind::Artifacts {
                    references: vec![keith_artifacts::ArtifactReference::from(&artifact)],
                },
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        assert!(matches!(
            coordinator.send_message(
                &first.id,
                ChildMessageSender::Child,
                ChildMessageKind::Text {
                    text: "fifth".into()
                },
                UtcTimestamp::from_unix_millis(5)
            ),
            Err(ChildError::MessageLimit)
        ));
        let messages = coordinator.messages(&first.id).unwrap();
        assert_eq!(messages.len(), 4);
        assert_eq!(messages[0].sequence, 1);
        assert_eq!(messages[3].sequence, 4);

        thread::scope(|scope| {
            let first_coordinator = Arc::clone(&coordinator);
            let first_id = first.id.clone();
            let first_finish = scope.spawn(move || {
                first_coordinator.finish(
                    &first_id,
                    ChildStatus::Complete,
                    "Delivered an artifact",
                    UtcTimestamp::from_unix_millis(10),
                )
            });
            let second_coordinator = Arc::clone(&coordinator);
            let second_id = second.id.clone();
            let second_finish = scope.spawn(move || {
                second_coordinator.finish(
                    &second_id,
                    ChildStatus::Failed,
                    "Dependency failed",
                    UtcTimestamp::from_unix_millis(10),
                )
            });
            assert_eq!(
                first_finish.join().unwrap().unwrap().status,
                ChildStatus::Complete
            );
            assert_eq!(
                second_finish.join().unwrap().unwrap().status,
                ChildStatus::Failed
            );
        });
    }

    #[test]
    fn parent_restart_recovers_the_same_child_actor_and_history() {
        let root = TempDir::new().unwrap();
        let artifact_root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        let artifacts = artifact_service(&artifact_root);
        let authority = authority(&workspace);
        let child_id;
        {
            let coordinator = coordinator(&root, Arc::clone(&artifacts));
            coordinator.register_root(authority.clone()).unwrap();
            let child = coordinator
                .create(
                    spec(
                        &authority.session_id,
                        ChildWorkspaceMode::DedicatedWorkspace,
                    ),
                    UtcTimestamp::UNIX_EPOCH,
                )
                .unwrap();
            child_id = child.id.clone();
            coordinator
                .send_message(
                    &child.id,
                    ChildMessageSender::Parent,
                    ChildMessageKind::Text {
                        text: "persist this".into(),
                    },
                    UtcTimestamp::from_unix_millis(1),
                )
                .unwrap();
        }
        let coordinator = coordinator(&root, artifacts);
        coordinator.register_root(authority).unwrap();
        let recovered = coordinator.recover_active().unwrap();
        assert_eq!(recovered.as_slice(), std::slice::from_ref(&child_id));
        coordinator
            .dispatch(&child_id, SessionCommand::Snapshot)
            .unwrap();
        assert_eq!(coordinator.messages(&child_id).unwrap().len(), 1);
        assert_eq!(
            coordinator.projection(&child_id).unwrap().status,
            ChildStatus::Running
        );
    }

    #[test]
    fn cancellation_orphan_adoption_grace_and_liveness_are_bounded() {
        let root = TempDir::new().unwrap();
        let artifact_root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        let artifacts = artifact_service(&artifact_root);
        let coordinator = coordinator(&root, Arc::clone(&artifacts));
        let first_root = authority(&workspace);
        let mut second_root = first_root.clone();
        second_root.session_id = SessionId::new();
        coordinator.register_root(first_root.clone()).unwrap();
        coordinator.register_root(second_root.clone()).unwrap();
        let propagated = coordinator
            .create(
                spec(
                    &first_root.session_id,
                    ChildWorkspaceMode::DedicatedWorkspace,
                ),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let nested = coordinator
            .create(
                spec(
                    &propagated.session_id,
                    ChildWorkspaceMode::DedicatedWorkspace,
                ),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let mut detached_spec = spec(
            &first_root.session_id,
            ChildWorkspaceMode::DedicatedWorkspace,
        );
        detached_spec.cancellation = ChildCancellation::DetachAsOrphan;
        detached_spec.limits.orphan_grace_ms = 5;
        let detached = coordinator
            .create(detached_spec, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let changed = coordinator
            .parent_unavailable(&first_root.session_id, UtcTimestamp::from_unix_millis(1))
            .unwrap();
        assert!(changed.contains(&propagated.id));
        assert_eq!(
            coordinator.projection(&nested.id).unwrap().status,
            ChildStatus::Cancelled
        );
        assert_eq!(
            coordinator.projection(&detached.id).unwrap().status,
            ChildStatus::Orphaned
        );
        let adopted = coordinator
            .adopt(
                &detached.id,
                &second_root.session_id,
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert_eq!(adopted.status, ChildStatus::Running);
        drop(coordinator);
        let coordinator = crate::coordinator::tests::coordinator(&root, artifacts);
        coordinator.register_root(first_root.clone()).unwrap();
        coordinator.register_root(second_root.clone()).unwrap();
        assert!(coordinator.recover_active().unwrap().contains(&detached.id));
        coordinator
            .parent_unavailable(&second_root.session_id, UtcTimestamp::from_unix_millis(3))
            .unwrap();
        let reaped = coordinator
            .reap_orphans(UtcTimestamp::from_unix_millis(8))
            .unwrap();
        assert_eq!(reaped.as_slice(), std::slice::from_ref(&detached.id));

        let mut stale_spec = spec(
            &first_root.session_id,
            ChildWorkspaceMode::DedicatedWorkspace,
        );
        stale_spec.limits.heartbeat_timeout_ms = 5;
        let stale = coordinator
            .create(stale_spec, UtcTimestamp::from_unix_millis(10))
            .unwrap();
        assert_eq!(
            coordinator
                .enforce_liveness(UtcTimestamp::from_unix_millis(15))
                .unwrap(),
            [stale.id]
        );
    }

    #[test]
    fn explicit_retention_archives_and_deletes_independent_session_data() {
        let root = TempDir::new().unwrap();
        let artifact_root = TempDir::new().unwrap();
        let workspace = TempDir::new().unwrap();
        let coordinator = coordinator(&root, artifact_service(&artifact_root));
        let authority = authority(&workspace);
        coordinator.register_root(authority.clone()).unwrap();
        let mut archive_spec = spec(
            &authority.session_id,
            ChildWorkspaceMode::DedicatedWorkspace,
        );
        archive_spec.retention = ChildRetention::ArchiveAfter(0);
        let archived = coordinator
            .create(archive_spec, UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        coordinator
            .finish(
                &archived.id,
                ChildStatus::Complete,
                "archive me",
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        coordinator
            .apply_retention(UtcTimestamp::from_unix_millis(1))
            .unwrap();
        assert_eq!(
            coordinator.projection(&archived.id).unwrap().status,
            ChildStatus::Archived
        );

        let mut delete_spec = spec(
            &authority.session_id,
            ChildWorkspaceMode::DedicatedWorkspace,
        );
        delete_spec.retention = ChildRetention::DeleteAfter(0);
        let deleted = coordinator
            .create(delete_spec, UtcTimestamp::from_unix_millis(2))
            .unwrap();
        coordinator
            .finish(
                &deleted.id,
                ChildStatus::Complete,
                "delete me",
                UtcTimestamp::from_unix_millis(3),
            )
            .unwrap();
        coordinator
            .apply_retention(UtcTimestamp::from_unix_millis(3))
            .unwrap();
        assert!(matches!(
            coordinator.projection(&deleted.id),
            Err(ChildError::NotFound(_))
        ));
        assert!(!deleted.workspace_root.exists());
    }
}
