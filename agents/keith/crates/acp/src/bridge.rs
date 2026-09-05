use std::fs::OpenOptions;
use std::io::Write;
#[cfg(unix)]
use std::os::unix::fs::{OpenOptionsExt, PermissionsExt};
use std::path::{Path, PathBuf};
use std::sync::Mutex;

use keith_agent_types::{ArtifactId, CommandId, ProfileId, SessionId, UtcTimestamp, WorkspaceId};
use keith_channel_core::AgentConnection;
use keith_connection::{FramedTransport, LocalStream, connect_local};
use keith_protocol::{
    AttachSession, CancelTarget, ClientCommand, CommandResult, CreateSession, DeliveryPolicy,
    EventEnvelope, ForkSession, ResponsePayload, ResumeCursor, StagedAttachment, SubmitPrompt,
    TurnTerminalStatus, WireFormat,
};
use sha2::{Digest, Sha256};

use crate::{
    AcpBinaryContent, AcpClientSessionConfig, AcpContentBlock, AcpPromptOutcome, AcpSessionRecord,
    AcpSessionStatus, AcpSessionStore, AcpUpdate, AcpUpdateProjector, BridgeError, DurablePrompt,
    PromptState,
};

type KeithConnection = AgentConnection<FramedTransport<LocalStream>>;
type SessionEvidence = (
    Option<ResumeCursor>,
    Vec<AcpUpdate>,
    Option<TurnTerminalStatus>,
);

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AcpBridgeConfig {
    pub daemon_endpoint: PathBuf,
    pub state_root: PathBuf,
    pub staging_root: PathBuf,
    pub profile_id: ProfileId,
    pub workspace_id: WorkspaceId,
    pub workspace_roots: Vec<PathBuf>,
    pub max_prompt_bytes: usize,
    pub max_attachments: usize,
    pub max_attachment_bytes: usize,
    pub max_total_attachment_bytes: usize,
}

impl AcpBridgeConfig {
    pub fn local(
        daemon_endpoint: PathBuf,
        state_root: PathBuf,
        profile_id: ProfileId,
        workspace_id: WorkspaceId,
        workspace_root: PathBuf,
    ) -> Self {
        let staging_root = state_root.join("staging");
        Self {
            daemon_endpoint,
            state_root,
            staging_root,
            profile_id,
            workspace_id,
            workspace_roots: vec![workspace_root],
            max_prompt_bytes: 2 * 1_024 * 1_024,
            max_attachments: 16,
            max_attachment_bytes: 25 * 1_024 * 1_024,
            max_total_attachment_bytes: 50 * 1_024 * 1_024,
        }
    }
}

pub struct AcpSessionBridge {
    config: AcpBridgeConfig,
    workspace_roots: Vec<PathBuf>,
    store: Mutex<AcpSessionStore>,
}

impl AcpSessionBridge {
    /// Opens a durable ACP bridge rooted in the configured Keith workspace.
    ///
    /// # Errors
    ///
    /// Returns an error when limits are invalid, workspace roots cannot be admitted, or durable
    /// session state cannot be opened.
    pub fn open(config: AcpBridgeConfig) -> Result<Self, BridgeError> {
        if config.workspace_roots.is_empty()
            || config.max_prompt_bytes == 0
            || config.max_attachments == 0
            || config.max_attachment_bytes == 0
            || config.max_total_attachment_bytes == 0
        {
            return Err(BridgeError::UnexpectedResponse(
                "ACP bridge limits and workspace roots must be non-empty",
            ));
        }
        let workspace_roots = config
            .workspace_roots
            .iter()
            .map(std::fs::canonicalize)
            .collect::<Result<Vec<_>, _>>()?;
        std::fs::create_dir_all(&config.staging_root)?;
        #[cfg(unix)]
        std::fs::set_permissions(&config.staging_root, std::fs::Permissions::from_mode(0o700))?;
        let store = AcpSessionStore::open(&config.state_root)?;
        Ok(Self {
            config,
            workspace_roots,
            store: Mutex::new(store),
        })
    }

    /// Creates and durably records a Keith-backed ACP session.
    ///
    /// # Errors
    ///
    /// Returns an error when paths are not admitted, Keith rejects creation, or state cannot be
    /// persisted.
    pub fn create_session(
        &self,
        cwd: &Path,
        additional_directories: &[PathBuf],
    ) -> Result<(AcpSessionRecord, Vec<AcpUpdate>), BridgeError> {
        let cwd = self.admit_path(cwd)?;
        let additional_directories = additional_directories
            .iter()
            .map(|path| self.admit_path(path))
            .collect::<Result<Vec<_>, _>>()?;
        let title = cwd
            .file_name()
            .and_then(|name| name.to_str())
            .map(|name| format!("ACP: {name}"));
        let mut connection = self.connect()?;
        let (result, events) = execute(
            &mut connection,
            ClientCommand::CreateSession(CreateSession {
                profile_id: self.config.profile_id.clone(),
                workspace_id: self.config.workspace_id.clone(),
                title,
            }),
            None,
        )?;
        let snapshot = response_snapshot(result)?;
        let session_id = snapshot.session.session_id.clone();
        let acp_session_id = session_id.to_string();
        let record = AcpSessionRecord {
            acp_session_id: acp_session_id.clone(),
            keith_session_id: session_id,
            profile_id: self.config.profile_id.clone(),
            workspace_id: self.config.workspace_id.clone(),
            cwd,
            additional_directories,
            status: AcpSessionStatus::Ready,
            cursor: Some(snapshot_cursor(&snapshot)),
            next_prompt_ordinal: 0,
            in_flight_prompt: None,
            forked_from: None,
            client_context: None,
            protocol_version: None,
        };
        self.lock_store()?.insert(record.clone())?;
        let mut updates = project_events(&events);
        updates.extend(AcpUpdateProjector::project_snapshot(&snapshot));
        Ok((record, deduplicate_updates(updates)))
    }

    /// Loads a durable ACP session after verifying its workspace boundary.
    ///
    /// # Errors
    ///
    /// Returns an error when the session is unknown, its workspace differs, Keith cannot attach,
    /// or refreshed state cannot be persisted.
    pub fn load_session(
        &self,
        acp_session_id: &str,
        cwd: &Path,
        additional_directories: &[PathBuf],
    ) -> Result<(AcpSessionRecord, Vec<AcpUpdate>), BridgeError> {
        let cwd = self.admit_path(cwd)?;
        let additional_directories = additional_directories
            .iter()
            .map(|path| self.admit_path(path))
            .collect::<Result<Vec<_>, _>>()?;
        let record = self.record(acp_session_id)?;
        if cwd != record.cwd || additional_directories != record.additional_directories {
            return Err(BridgeError::WorkspaceBoundary(cwd));
        }
        let (result, events) = self.attach(&record)?;
        let (cursor, mut updates, terminal) = evidence(result, &events)?;
        let record = self.lock_store()?.update(acp_session_id, |stored| {
            stored.status = terminal.map_or(AcpSessionStatus::Ready, terminal_status);
            stored.cursor = cursor.or_else(|| stored.cursor.clone());
            Ok(())
        })?;
        updates = deduplicate_updates(updates);
        Ok((record, updates))
    }

    /// Resumes a disconnected ACP session and projects recovered Keith events.
    ///
    /// # Errors
    ///
    /// Returns an error when the session is unknown or closed, Keith rejects recovery, or durable
    /// state cannot be updated.
    pub fn resume_session(&self, acp_session_id: &str) -> Result<AcpPromptOutcome, BridgeError> {
        let record = self.record(acp_session_id)?;
        if record.status == AcpSessionStatus::Closed {
            return Err(BridgeError::ClosedSession(acp_session_id.to_owned()));
        }
        let (attach_result, mut events) = self.attach(&record)?;
        let mut snapshots = Vec::new();
        if let CommandResult::Data(payload) = attach_result {
            if let ResponsePayload::Snapshot(snapshot) = *payload {
                snapshots.push(*snapshot);
            }
        } else if let CommandResult::Rejected(error) = attach_result {
            return Err(BridgeError::KeithRejected(error.error.message));
        }
        let mut connection = self.connect()?;
        let (result, resumed_events) = execute(
            &mut connection,
            ClientCommand::ResumeSession {
                session_id: record.keith_session_id.clone(),
            },
            Some(record.keith_session_id.clone()),
        )?;
        events.extend(resumed_events);
        match result {
            CommandResult::Data(payload) => match *payload {
                ResponsePayload::Snapshot(snapshot) => snapshots.push(*snapshot),
                _ => return Err(BridgeError::UnexpectedResponse("resume session payload")),
            },
            CommandResult::Accepted { .. } => {}
            CommandResult::Rejected(error) => {
                return Err(BridgeError::KeithRejected(error.error.message));
            }
        }
        let mut updates = project_events(&events);
        for snapshot in &snapshots {
            updates.extend(AcpUpdateProjector::project_snapshot(snapshot));
        }
        updates = deduplicate_updates(updates);
        let terminal = AcpUpdateProjector::terminal_status(&updates);
        let cursor = newest_cursor(&events)
            .or_else(|| snapshots.last().map(snapshot_cursor))
            .or(record.cursor);
        self.lock_store()?.update(acp_session_id, |stored| {
            stored.cursor = cursor;
            if let Some(status) = terminal {
                stored.status = terminal_status(status);
                if let Some(prompt) = &mut stored.in_flight_prompt {
                    prompt.state = prompt_terminal_state(status);
                }
                stored.in_flight_prompt = None;
            }
            Ok(())
        })?;
        Ok(AcpPromptOutcome { updates, terminal })
    }

    /// Submits one durably ordered prompt to an ACP session.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid content, an unavailable session, a conflicting prompt, a Keith
    /// transport or execution failure, or a persistence failure.
    #[allow(clippy::too_many_lines)]
    pub fn prompt(
        &self,
        acp_session_id: &str,
        content: &[AcpContentBlock],
    ) -> Result<AcpPromptOutcome, BridgeError> {
        let prepared = self.prepare_content(content)?;
        let content_sha256 = digest_prompt(&prepared.text, &prepared.attachments);
        let mut record = self.lock_store()?.update(acp_session_id, |stored| {
            if stored.status == AcpSessionStatus::Closed {
                return Err(BridgeError::ClosedSession(acp_session_id.to_owned()));
            }
            match &stored.in_flight_prompt {
                Some(prompt)
                    if prompt.content_sha256 == content_sha256
                        && matches!(
                            prompt.state,
                            PromptState::Prepared | PromptState::Submitted
                        ) => {}
                Some(_) => {
                    return Err(BridgeError::PromptInFlight(acp_session_id.to_owned()));
                }
                None => {
                    stored.in_flight_prompt = Some(DurablePrompt {
                        ordinal: stored.next_prompt_ordinal,
                        command_id: CommandId::new(),
                        content_sha256: content_sha256.clone(),
                        state: PromptState::Prepared,
                        artifact_ids: Vec::new(),
                    });
                    stored.next_prompt_ordinal = stored.next_prompt_ordinal.saturating_add(1);
                }
            }
            stored.status = AcpSessionStatus::Running;
            Ok(())
        })?;
        let (attach_result, recovery_events) = self.attach(&record)?;
        let recovery_snapshot = optional_snapshot(attach_result)?;
        let mut updates = project_events(&recovery_events);
        if let Some(snapshot) = &recovery_snapshot {
            updates.extend(AcpUpdateProjector::project_snapshot(snapshot));
        }
        let recovery_cursor = newest_cursor(&recovery_events)
            .or_else(|| recovery_snapshot.as_ref().map(snapshot_cursor));

        let mut connection = self.connect()?;
        let artifact_ids = if record
            .in_flight_prompt
            .as_ref()
            .is_some_and(|prompt| prompt.artifact_ids.len() == prepared.attachments.len())
        {
            record
                .in_flight_prompt
                .as_ref()
                .map(|prompt| prompt.artifact_ids.clone())
                .unwrap_or_default()
        } else {
            let artifact_ids = self.stage_attachments(
                &mut connection,
                &record.keith_session_id,
                &prepared.attachments,
            )?;
            record = self.lock_store()?.update(acp_session_id, |stored| {
                let prompt = stored
                    .in_flight_prompt
                    .as_mut()
                    .ok_or_else(|| BridgeError::PromptInFlight(acp_session_id.to_owned()))?;
                prompt.artifact_ids.clone_from(&artifact_ids);
                Ok(())
            })?;
            artifact_ids
        };
        let command_id = record
            .in_flight_prompt
            .as_ref()
            .map(|prompt| prompt.command_id.clone())
            .ok_or_else(|| BridgeError::PromptInFlight(acp_session_id.to_owned()))?;
        self.lock_store()?.update(acp_session_id, |stored| {
            if let Some(prompt) = &mut stored.in_flight_prompt {
                prompt.state = PromptState::Submitted;
            }
            Ok(())
        })?;
        let (result, prompt_events) = execute_idempotent(
            &mut connection,
            &command_id,
            ClientCommand::SubmitPrompt(SubmitPrompt {
                session_id: record.keith_session_id.clone(),
                text: prepared.text,
                artifacts: artifact_ids,
                delivery: DeliveryPolicy::Immediate,
                reply_route: None,
            }),
            Some(record.keith_session_id),
        )?;
        updates.extend(project_events(&prompt_events));
        let result_snapshot = optional_snapshot(result)?;
        if let Some(snapshot) = &result_snapshot {
            updates.extend(AcpUpdateProjector::project_snapshot(snapshot));
        }
        updates = deduplicate_updates(updates);
        let terminal = AcpUpdateProjector::terminal_status(&updates);
        let cursor = newest_cursor(&prompt_events)
            .or_else(|| result_snapshot.as_ref().map(snapshot_cursor))
            .or(recovery_cursor);
        self.lock_store()?.update(acp_session_id, |stored| {
            stored.cursor = cursor.or_else(|| stored.cursor.clone());
            if let Some(status) = terminal {
                stored.status = terminal_status(status);
                stored.in_flight_prompt = None;
            } else {
                stored.status = AcpSessionStatus::Running;
            }
            Ok(())
        })?;
        Ok(AcpPromptOutcome { updates, terminal })
    }

    /// Cancels only the active turn belonging to the named ACP session.
    ///
    /// # Errors
    ///
    /// Returns an error when the session is unknown, Keith rejects cancellation, or updated state
    /// cannot be persisted.
    pub fn cancel_session(&self, acp_session_id: &str) -> Result<Vec<AcpUpdate>, BridgeError> {
        let record = self.record(acp_session_id)?;
        let mut connection = self.connect()?;
        let (result, events) = execute(
            &mut connection,
            ClientCommand::Cancel(CancelTarget::Session(record.keith_session_id.clone())),
            Some(record.keith_session_id),
        )?;
        accept_result(result)?;
        let updates = deduplicate_updates(project_events(&events));
        let terminal = AcpUpdateProjector::terminal_status(&updates);
        let cursor = newest_cursor(&events);
        self.lock_store()?.update(acp_session_id, |stored| {
            stored.cursor = cursor.or_else(|| stored.cursor.clone());
            stored.status = terminal.map_or(AcpSessionStatus::Cancelling, terminal_status);
            if terminal.is_some() {
                stored.in_flight_prompt = None;
            }
            Ok(())
        })?;
        Ok(updates)
    }

    /// Cancels outstanding work and closes a durable ACP session.
    ///
    /// # Errors
    ///
    /// Returns an error when the session is unknown, Keith cannot cancel or detach it, or closed
    /// state cannot be persisted.
    pub fn close_session(&self, acp_session_id: &str) -> Result<(), BridgeError> {
        let record = self.record(acp_session_id)?;
        if record.in_flight_prompt.is_some() {
            self.cancel_session(acp_session_id)?;
        }
        let mut connection = self.connect()?;
        let (result, _) = execute(
            &mut connection,
            ClientCommand::DetachSession {
                session_id: record.keith_session_id.clone(),
            },
            Some(record.keith_session_id),
        )?;
        accept_result(result)?;
        self.lock_store()?.update(acp_session_id, |stored| {
            stored.status = AcpSessionStatus::Closed;
            Ok(())
        })?;
        Ok(())
    }

    /// Forks a ready ACP session into an independent Keith root while preserving committed context.
    ///
    /// # Errors
    ///
    /// Returns an error when the source cannot be forked, a workspace path is not admitted, the
    /// daemon rejects the fork, or the new ACP session record cannot be persisted.
    pub fn fork_session(
        &self,
        acp_session_id: &str,
        cwd: &Path,
        additional_directories: &[PathBuf],
    ) -> Result<(AcpSessionRecord, Vec<AcpUpdate>), BridgeError> {
        let source = self.record(acp_session_id)?;
        if source.status == AcpSessionStatus::Closed {
            return Err(BridgeError::ClosedSession(acp_session_id.to_owned()));
        }
        if source.in_flight_prompt.is_some() {
            return Err(BridgeError::PromptInFlight(acp_session_id.to_owned()));
        }
        let cwd = self.admit_path(cwd)?;
        let additional_directories = additional_directories
            .iter()
            .map(|path| self.admit_path(path))
            .collect::<Result<Vec<_>, _>>()?;
        let title = cwd
            .file_name()
            .and_then(|name| name.to_str())
            .map(|name| format!("ACP fork: {name}"));
        let mut connection = self.connect()?;
        let (result, events) = execute(
            &mut connection,
            ClientCommand::ForkSession(ForkSession {
                source_session_id: source.keith_session_id.clone(),
                title,
            }),
            Some(source.keith_session_id.clone()),
        )?;
        let snapshot = response_snapshot(result)?;
        if snapshot.session.session_id == source.keith_session_id
            || source
                .cursor_parts()
                .is_some_and(|(root, _, _)| root == &snapshot.session.root_tree_id)
            || snapshot.session.profile_id != source.profile_id
        {
            return Err(BridgeError::UnexpectedResponse(
                "fork did not create an independent session in the source profile",
            ));
        }
        let forked_session_id = snapshot.session.session_id.clone();
        let record = AcpSessionRecord {
            acp_session_id: forked_session_id.to_string(),
            keith_session_id: forked_session_id,
            profile_id: source.profile_id,
            workspace_id: source.workspace_id,
            cwd,
            additional_directories,
            status: AcpSessionStatus::Ready,
            cursor: Some(snapshot_cursor(&snapshot)),
            next_prompt_ordinal: 0,
            in_flight_prompt: None,
            forked_from: Some(source.acp_session_id),
            client_context: None,
            protocol_version: None,
        };
        self.lock_store()?.insert(record.clone())?;
        let mut updates = project_events(&events);
        updates.extend(AcpUpdateProjector::project_snapshot(&snapshot));
        Ok((record, deduplicate_updates(updates)))
    }

    /// Binds negotiated client tools and an exact ACP protocol version to a durable session.
    ///
    /// Reconnects can only reduce capabilities. Reusing an MCP id with changed configuration or
    /// continuing the session through another protocol version is rejected.
    ///
    /// # Errors
    ///
    /// Returns an error for an unknown session, widened configuration, version crossover, or
    /// persistence failure.
    pub fn bind_client_context(
        &self,
        acp_session_id: &str,
        context: AcpClientSessionConfig,
        protocol_version: u16,
    ) -> Result<AcpSessionRecord, BridgeError> {
        if !matches!(protocol_version, 1 | 2) {
            return Err(BridgeError::ProtocolVersion(format!(
                "ACP v{protocol_version} has no compiled session schema"
            )));
        }
        self.lock_store()?.update(acp_session_id, |stored| {
            if let Some(bound) = stored.protocol_version
                && bound != protocol_version
            {
                return Err(BridgeError::ProtocolVersion(format!(
                    "session is bound to ACP v{bound}, not ACP v{protocol_version}"
                )));
            }
            stored.client_context = Some(match &stored.client_context {
                Some(current) => current.restrict_with(&context)?,
                None => context,
            });
            stored.protocol_version = Some(protocol_version);
            Ok(())
        })
    }

    /// Returns one durable ACP session record.
    ///
    /// # Errors
    ///
    /// Returns an error when the session is unknown or the durable store lock is poisoned.
    pub fn session(&self, acp_session_id: &str) -> Result<AcpSessionRecord, BridgeError> {
        self.record(acp_session_id)
    }

    /// Returns all durable ACP session records.
    ///
    /// # Errors
    ///
    /// Returns an error when the durable store lock is poisoned.
    pub fn sessions(&self) -> Result<Vec<AcpSessionRecord>, BridgeError> {
        Ok(self.lock_store()?.records().cloned().collect())
    }

    fn connect(&self) -> Result<KeithConnection, BridgeError> {
        let stream = connect_local(&self.config.daemon_endpoint)?;
        AgentConnection::connect(FramedTransport::new(stream, WireFormat::Json)).map_err(Into::into)
    }

    fn attach(
        &self,
        record: &AcpSessionRecord,
    ) -> Result<(CommandResult, Vec<EventEnvelope>), BridgeError> {
        let mut connection = self.connect()?;
        execute(
            &mut connection,
            ClientCommand::AttachSession(AttachSession {
                session_id: record.keith_session_id.clone(),
                resume: record.cursor.clone(),
            }),
            Some(record.keith_session_id.clone()),
        )
    }

    fn stage_attachments(
        &self,
        connection: &mut KeithConnection,
        session_id: &SessionId,
        attachments: &[AcpBinaryContent],
    ) -> Result<Vec<ArtifactId>, BridgeError> {
        let directory = self.config.staging_root.join(session_id.to_string());
        std::fs::create_dir_all(&directory)?;
        #[cfg(unix)]
        std::fs::set_permissions(&directory, std::fs::Permissions::from_mode(0o700))?;
        let mut artifact_ids = Vec::with_capacity(attachments.len());
        for attachment in attachments {
            let digest = hex_digest(&attachment.bytes);
            let staging_file = directory.join(&digest);
            if !staging_file.exists() {
                let mut options = OpenOptions::new();
                options.write(true).create_new(true);
                #[cfg(unix)]
                options.mode(0o600);
                let mut file = options.open(&staging_file)?;
                file.write_all(&attachment.bytes)?;
                file.sync_all()?;
            }
            let (result, _) = execute(
                connection,
                ClientCommand::StageAttachment(StagedAttachment {
                    session_id: session_id.clone(),
                    staging_file: staging_file.to_string_lossy().into_owned(),
                    file_name: safe_file_name(&attachment.name),
                    media_type: attachment.media_type.clone(),
                    byte_length: u64::try_from(attachment.bytes.len())
                        .map_err(|_| BridgeError::AttachmentLimit)?,
                    sha256: digest,
                }),
                Some(session_id.clone()),
            )?;
            match result {
                CommandResult::Data(payload) => match *payload {
                    ResponsePayload::Artifact(artifact_id) => artifact_ids.push(artifact_id),
                    _ => {
                        return Err(BridgeError::UnexpectedResponse("stage attachment payload"));
                    }
                },
                CommandResult::Rejected(error) => {
                    return Err(BridgeError::KeithRejected(error.error.message));
                }
                CommandResult::Accepted { .. } => {
                    return Err(BridgeError::UnexpectedResponse(
                        "stage attachment acknowledgement",
                    ));
                }
            }
        }
        Ok(artifact_ids)
    }

    fn prepare_content(&self, content: &[AcpContentBlock]) -> Result<PreparedPrompt, BridgeError> {
        let mut text = String::new();
        let mut attachments = Vec::new();
        for block in content {
            if !text.is_empty() {
                text.push_str("\n\n");
            }
            match block {
                AcpContentBlock::Text(value) => text.push_str(value),
                AcpContentBlock::ResourceLink { name, uri } => {
                    text.push('[');
                    text.push_str(name);
                    text.push_str("](");
                    text.push_str(uri);
                    text.push(')');
                }
                AcpContentBlock::EmbeddedText {
                    name,
                    uri,
                    media_type,
                    text: resource_text,
                } => {
                    text.push_str("[Attached resource: ");
                    text.push_str(name);
                    text.push_str(" (");
                    text.push_str(uri);
                    text.push_str(")]");
                    attachments.push(AcpBinaryContent {
                        name: name.clone(),
                        media_type: media_type.clone(),
                        bytes: resource_text.as_bytes().to_vec(),
                    });
                }
                AcpContentBlock::Binary(binary) => {
                    text.push_str("[Attached content: ");
                    text.push_str(&binary.name);
                    text.push_str(" (");
                    text.push_str(&binary.media_type);
                    text.push_str(")]");
                    attachments.push(binary.clone());
                }
            }
        }
        if text.trim().is_empty() && attachments.is_empty() {
            return Err(BridgeError::UnsupportedContent(
                "prompt contains no supported content blocks".to_owned(),
            ));
        }
        if text.len() > self.config.max_prompt_bytes {
            return Err(BridgeError::ContentLimit);
        }
        if attachments.len() > self.config.max_attachments
            || attachments
                .iter()
                .any(|attachment| attachment.bytes.len() > self.config.max_attachment_bytes)
            || attachments
                .iter()
                .try_fold(0_usize, |total, attachment| {
                    total.checked_add(attachment.bytes.len())
                })
                .is_none_or(|total| total > self.config.max_total_attachment_bytes)
        {
            return Err(BridgeError::AttachmentLimit);
        }
        Ok(PreparedPrompt { text, attachments })
    }

    fn record(&self, acp_session_id: &str) -> Result<AcpSessionRecord, BridgeError> {
        self.lock_store()?
            .get(acp_session_id)
            .cloned()
            .ok_or_else(|| BridgeError::UnknownSession(acp_session_id.to_owned()))
    }

    fn lock_store(&self) -> Result<std::sync::MutexGuard<'_, AcpSessionStore>, BridgeError> {
        self.store.lock().map_err(|_| BridgeError::LockPoisoned)
    }

    fn admit_path(&self, path: &Path) -> Result<PathBuf, BridgeError> {
        let canonical = std::fs::canonicalize(path)?;
        if self
            .workspace_roots
            .iter()
            .any(|root| canonical.starts_with(root))
        {
            Ok(canonical)
        } else {
            Err(BridgeError::WorkspaceBoundary(canonical))
        }
    }
}

struct PreparedPrompt {
    text: String,
    attachments: Vec<AcpBinaryContent>,
}

fn execute(
    connection: &mut KeithConnection,
    command: ClientCommand,
    session_id: Option<SessionId>,
) -> Result<(CommandResult, Vec<EventEnvelope>), BridgeError> {
    let result = connection.execute(command, session_id, now()?)?;
    Ok((result, drain_events(connection)))
}

fn execute_idempotent(
    connection: &mut KeithConnection,
    command_id: &CommandId,
    command: ClientCommand,
    session_id: Option<SessionId>,
) -> Result<(CommandResult, Vec<EventEnvelope>), BridgeError> {
    let result = connection.execute_idempotent(command_id, command, session_id, now()?)?;
    Ok((result, drain_events(connection)))
}

fn drain_events(connection: &mut KeithConnection) -> Vec<EventEnvelope> {
    let mut events = Vec::new();
    while let Some(event) = connection.take_event() {
        events.push(event);
    }
    events
}

fn now() -> Result<UtcTimestamp, BridgeError> {
    UtcTimestamp::now().map_err(|_| BridgeError::Clock)
}

fn response_snapshot(
    result: CommandResult,
) -> Result<keith_protocol::SessionSnapshot, BridgeError> {
    optional_snapshot(result)?.ok_or(BridgeError::UnexpectedResponse("session snapshot"))
}

fn optional_snapshot(
    result: CommandResult,
) -> Result<Option<keith_protocol::SessionSnapshot>, BridgeError> {
    match result {
        CommandResult::Data(payload) => match *payload {
            ResponsePayload::Snapshot(snapshot) => Ok(Some(*snapshot)),
            _ => Err(BridgeError::UnexpectedResponse("session payload")),
        },
        CommandResult::Accepted { .. } => Ok(None),
        CommandResult::Rejected(error) => Err(BridgeError::KeithRejected(error.error.message)),
    }
}

fn accept_result(result: CommandResult) -> Result<(), BridgeError> {
    match result {
        CommandResult::Accepted { .. } | CommandResult::Data(_) => Ok(()),
        CommandResult::Rejected(error) => Err(BridgeError::KeithRejected(error.error.message)),
    }
}

fn evidence(
    result: CommandResult,
    events: &[EventEnvelope],
) -> Result<SessionEvidence, BridgeError> {
    let snapshot = optional_snapshot(result)?;
    let mut updates = project_events(events);
    if let Some(snapshot) = &snapshot {
        updates.extend(AcpUpdateProjector::project_snapshot(snapshot));
    }
    let terminal = AcpUpdateProjector::terminal_status(&updates);
    let cursor = newest_cursor(events).or_else(|| snapshot.as_ref().map(snapshot_cursor));
    Ok((cursor, updates, terminal))
}

fn project_events(events: &[EventEnvelope]) -> Vec<AcpUpdate> {
    events
        .iter()
        .flat_map(AcpUpdateProjector::project_event)
        .collect()
}

fn newest_cursor(events: &[EventEnvelope]) -> Option<ResumeCursor> {
    events.last().map(|event| ResumeCursor {
        root_tree_id: event.root_tree_id.clone(),
        generation: event.generation,
        last_sequence: event.sequence,
    })
}

fn snapshot_cursor(snapshot: &keith_protocol::SessionSnapshot) -> ResumeCursor {
    ResumeCursor {
        root_tree_id: snapshot.session.root_tree_id.clone(),
        generation: snapshot.generation,
        last_sequence: snapshot.through_sequence,
    }
}

fn terminal_status(status: TurnTerminalStatus) -> AcpSessionStatus {
    match status {
        TurnTerminalStatus::Completed | TurnTerminalStatus::Cancelled => AcpSessionStatus::Ready,
        TurnTerminalStatus::Failed | TurnTerminalStatus::Exhausted => AcpSessionStatus::Failed,
    }
}

fn prompt_terminal_state(status: TurnTerminalStatus) -> PromptState {
    match status {
        TurnTerminalStatus::Completed => PromptState::Completed,
        TurnTerminalStatus::Cancelled => PromptState::Cancelled,
        TurnTerminalStatus::Failed | TurnTerminalStatus::Exhausted => PromptState::Failed,
    }
}

fn digest_prompt(text: &str, attachments: &[AcpBinaryContent]) -> String {
    let mut hasher = Sha256::new();
    hasher.update(b"keith-acp-prompt-v1\0");
    hasher.update(text.len().to_be_bytes());
    hasher.update(text.as_bytes());
    for attachment in attachments {
        hasher.update(attachment.name.len().to_be_bytes());
        hasher.update(attachment.name.as_bytes());
        hasher.update(attachment.media_type.len().to_be_bytes());
        hasher.update(attachment.media_type.as_bytes());
        hasher.update(attachment.bytes.len().to_be_bytes());
        hasher.update(&attachment.bytes);
    }
    format!("{:x}", hasher.finalize())
}

fn hex_digest(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn safe_file_name(value: &str) -> String {
    Path::new(value)
        .file_name()
        .and_then(|name| name.to_str())
        .filter(|name| !name.is_empty())
        .unwrap_or("attachment")
        .to_owned()
}

fn deduplicate_updates(updates: Vec<AcpUpdate>) -> Vec<AcpUpdate> {
    let streamed_messages = updates
        .iter()
        .filter_map(|update| match &update.kind {
            crate::AcpUpdateKind::AssistantMessage {
                message_id,
                committed: false,
                ..
            } => Some(message_id.clone()),
            _ => None,
        })
        .collect::<std::collections::BTreeSet<_>>();
    let mut seen = std::collections::BTreeSet::new();
    updates
        .into_iter()
        .filter(|update| {
            !matches!(
                &update.kind,
                crate::AcpUpdateKind::AssistantMessage {
                    message_id,
                    committed: true,
                    ..
                } if streamed_messages.contains(message_id)
            ) && seen.insert(update.event_id.clone())
        })
        .collect()
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn prompt_digest_has_unambiguous_block_boundaries() {
        let left = vec![AcpBinaryContent {
            name: "a".to_owned(),
            media_type: "bc".to_owned(),
            bytes: vec![1, 2],
        }];
        let right = vec![AcpBinaryContent {
            name: "ab".to_owned(),
            media_type: "c".to_owned(),
            bytes: vec![1, 2],
        }];
        assert_ne!(
            digest_prompt("prompt", &left),
            digest_prompt("prompt", &right)
        );
    }

    #[test]
    fn attachment_names_cannot_escape_staging_directory() {
        assert_eq!(safe_file_name("../../secret.txt"), "secret.txt");
        assert_eq!(safe_file_name(""), "attachment");
    }

    #[test]
    fn streamed_assistant_message_is_not_repeated_as_a_committed_snapshot() {
        let streaming = AcpUpdate {
            event_id: "event:stream".to_owned(),
            kind: crate::AcpUpdateKind::AssistantMessage {
                message_id: "message".to_owned(),
                text: "hel".to_owned(),
                committed: false,
            },
        };
        let committed = AcpUpdate {
            event_id: "event:committed".to_owned(),
            kind: crate::AcpUpdateKind::AssistantMessage {
                message_id: "message".to_owned(),
                text: "hello".to_owned(),
                committed: true,
            },
        };
        assert_eq!(
            deduplicate_updates(vec![streaming.clone(), committed]),
            vec![streaming]
        );
    }
}
