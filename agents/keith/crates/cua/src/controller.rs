use std::collections::{BTreeMap, VecDeque};
use std::fmt::Write as _;
use std::fs;
use std::io;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::sync::atomic::{AtomicBool, Ordering};

use keith_agent_types::{ProfileId, UtcTimestamp};
use keith_platform_contracts::{
    ActionRisk, AuditEnvelope, AuditOutcome, AuthorityBoundary, CancellationId, Capability,
    ComputerSessionId, ContractError, ExternalAction, RedactedText,
};
use serde::Serialize;
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{
    ActionAttempt, ActionDisposition, ComputerAction, ComputerActionRequest, ComputerActionResult,
    ComputerHealth, ComputerLifecycle, ComputerMode, ComputerObservation, ComputerSession,
    ComputerSessionLayout, ComputerSnapshotId, CreateComputerRequest, FrameId,
    MAX_ALTERNATE_ACTIONS, RuntimeActionResult,
};

pub trait ComputerRuntime {
    type Error: std::error::Error + Send + Sync + 'static;

    /// Starts the owned workstation process tree.
    ///
    /// # Errors
    ///
    /// Returns a runtime-specific launch or isolation error.
    fn start(
        &mut self,
        session: &ComputerSession,
        layout: &ComputerSessionLayout,
    ) -> Result<(), Self::Error>;
    /// Stops the process tree while retaining persistent storage.
    ///
    /// # Errors
    ///
    /// Returns a runtime-specific process-control error.
    fn suspend(&mut self, session: &ComputerSession) -> Result<(), Self::Error>;
    /// Restarts a suspended workstation from retained storage.
    ///
    /// # Errors
    ///
    /// Returns a runtime-specific launch or isolation error.
    fn resume(
        &mut self,
        session: &ComputerSession,
        layout: &ComputerSessionLayout,
    ) -> Result<(), Self::Error>;
    /// Terminates and reaps the owned process tree.
    ///
    /// # Errors
    ///
    /// Returns a runtime-specific process-control error.
    fn terminate(&mut self, session: &ComputerSession) -> Result<(), Self::Error>;
    /// Captures the current multimodal observation.
    ///
    /// # Errors
    ///
    /// Returns a runtime-specific capture or protocol error.
    fn observe(
        &mut self,
        session: &ComputerSession,
        now: UtcTimestamp,
    ) -> Result<ComputerObservation, Self::Error>;
    /// Executes one bounded action against the current workstation state.
    ///
    /// # Errors
    ///
    /// Returns a runtime-specific target, cancellation, timeout, or protocol error.
    fn execute(
        &mut self,
        session: &ComputerSession,
        action: &ComputerAction,
        cancellation: &CancellationToken,
        now: UtcTimestamp,
    ) -> Result<RuntimeActionResult, Self::Error>;
    /// Checks the actual owned process state.
    ///
    /// # Errors
    ///
    /// Returns a runtime-specific process inspection error.
    fn is_running(&mut self, session_id: &ComputerSessionId) -> Result<bool, Self::Error>;
}

#[derive(Clone, Debug, Default)]
pub struct CancellationToken {
    id: Option<CancellationId>,
    cancelled: Arc<AtomicBool>,
}

impl CancellationToken {
    pub fn new(id: CancellationId) -> Self {
        Self {
            id: Some(id),
            cancelled: Arc::new(AtomicBool::new(false)),
        }
    }

    pub fn id(&self) -> Option<&CancellationId> {
        self.id.as_ref()
    }

    pub fn is_cancelled(&self) -> bool {
        self.cancelled.load(Ordering::Acquire)
    }

    pub fn cancel(&self) {
        self.cancelled.store(true, Ordering::Release);
    }
}

pub struct ComputerController<R> {
    root: PathBuf,
    runtime: R,
    sessions: BTreeMap<ComputerSessionId, ComputerSession>,
    cancellations: BTreeMap<CancellationId, CancellationToken>,
    recent_action_times: BTreeMap<ComputerSessionId, VecDeque<UtcTimestamp>>,
}

impl<R> ComputerController<R>
where
    R: ComputerRuntime,
{
    /// Opens a durable controller and reconciles sessions that were running when the prior process stopped.
    ///
    /// # Errors
    ///
    /// Returns a storage or serialization error when durable state cannot be read safely.
    pub fn open(
        root: impl Into<PathBuf>,
        runtime: R,
        now: UtcTimestamp,
    ) -> Result<Self, ComputerError> {
        let root = root.into();
        fs::create_dir_all(root.join("profiles"))?;
        let mut controller = Self {
            root,
            runtime,
            sessions: BTreeMap::new(),
            cancellations: BTreeMap::new(),
            recent_action_times: BTreeMap::new(),
        };
        controller.load_sessions(now)?;
        Ok(controller)
    }

    pub fn runtime(&self) -> &R {
        &self.runtime
    }

    pub fn runtime_mut(&mut self) -> &mut R {
        &mut self.runtime
    }

    pub fn session(&self, session_id: &ComputerSessionId) -> Option<&ComputerSession> {
        self.sessions.get(session_id)
    }

    /// Creates durable, profile-owned computer storage without starting a process.
    ///
    /// # Errors
    ///
    /// Returns a validation or storage error.
    pub fn create(
        &mut self,
        request: CreateComputerRequest,
        now: UtcTimestamp,
    ) -> Result<ComputerSession, ComputerError> {
        request.viewport.validate()?;
        request.limits.validate()?;
        let session = ComputerSession {
            id: ComputerSessionId::new(),
            profile_id: request.profile_id,
            mode: request.mode,
            lifecycle: ComputerLifecycle::Created,
            isolation: request.isolation,
            network: request.network,
            viewport: request.viewport,
            limits: request.limits,
            created_at: now,
            updated_at: now,
            last_activity_at: now,
            active_snapshot: None,
            generation: 0,
            safe_error: None,
        };
        self.create_layout(&session)?;
        self.persist(&session)?;
        self.sessions.insert(session.id.clone(), session.clone());
        Ok(session)
    }

    /// Starts a created, suspended, or interrupted computer.
    ///
    /// # Errors
    ///
    /// Returns an ownership, lifecycle, runtime, or persistence error.
    pub fn start(
        &mut self,
        session_id: &ComputerSessionId,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ComputerSession, ComputerError> {
        self.start_or_resume(session_id, profile_id, now, false)
    }

    /// Suspends a running computer and releases its processes while retaining authorized state.
    ///
    /// # Errors
    ///
    /// Returns an ownership, lifecycle, runtime, or persistence error.
    pub fn suspend(
        &mut self,
        session_id: &ComputerSessionId,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ComputerSession, ComputerError> {
        let snapshot = self.owned_session(session_id, profile_id)?.clone();
        if snapshot.lifecycle != ComputerLifecycle::Running {
            return Err(ComputerError::InvalidLifecycle);
        }
        self.runtime
            .suspend(&snapshot)
            .map_err(|error| ComputerError::Runtime(error.to_string()))?;
        self.update_session(session_id, |session| {
            session.lifecycle = ComputerLifecycle::Suspended;
            session.updated_at = now;
            session.safe_error = None;
        })
    }

    /// Resumes a suspended computer.
    ///
    /// # Errors
    ///
    /// Returns an ownership, lifecycle, runtime, or persistence error.
    pub fn resume(
        &mut self,
        session_id: &ComputerSessionId,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ComputerSession, ComputerError> {
        self.start_or_resume(session_id, profile_id, now, true)
    }

    /// Snapshots a non-running computer's profile, workspace, and downloads.
    ///
    /// # Errors
    ///
    /// Returns an ownership, lifecycle, or storage error.
    pub fn snapshot(
        &mut self,
        session_id: &ComputerSessionId,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ComputerSnapshotId, ComputerError> {
        let session = self.owned_session(session_id, profile_id)?.clone();
        if !matches!(
            session.lifecycle,
            ComputerLifecycle::Created
                | ComputerLifecycle::Suspended
                | ComputerLifecycle::Interrupted
                | ComputerLifecycle::Terminated
        ) {
            return Err(ComputerError::InvalidLifecycle);
        }
        let snapshot_id = ComputerSnapshotId::new();
        let layout = self.layout(&session);
        let target = layout.snapshots.join(snapshot_id.as_str());
        fs::create_dir_all(&target)?;
        let snapshot_result = (|| {
            let mut remaining = session.limits.disk_bytes.saturating_sub(4_096);
            copy_tree(&layout.profile, &target.join("profile"), &mut remaining)?;
            copy_tree(&layout.workspace, &target.join("workspace"), &mut remaining)?;
            copy_tree(&layout.downloads, &target.join("downloads"), &mut remaining)?;
            write_json_atomic(
                &target.join("snapshot.json"),
                &SnapshotManifest {
                    id: snapshot_id.clone(),
                    computer_session_id: session.id.clone(),
                    profile_id: session.profile_id.clone(),
                    created_at: now,
                    generation: session.generation,
                },
            )
        })();
        if let Err(error) = snapshot_result {
            let _ = remove_dir_if_exists(&target);
            return Err(error);
        }
        self.update_session(session_id, |session| {
            session.active_snapshot = Some(snapshot_id.clone());
            session.updated_at = now;
        })?;
        Ok(snapshot_id)
    }

    /// Restores an owned snapshot only while no workstation process is running.
    ///
    /// # Errors
    ///
    /// Returns an ownership, lifecycle, snapshot, or storage error.
    pub fn restore(
        &mut self,
        session_id: &ComputerSessionId,
        profile_id: &ProfileId,
        snapshot_id: &ComputerSnapshotId,
        now: UtcTimestamp,
    ) -> Result<ComputerSession, ComputerError> {
        let session = self.owned_session(session_id, profile_id)?.clone();
        if !matches!(
            session.lifecycle,
            ComputerLifecycle::Created
                | ComputerLifecycle::Suspended
                | ComputerLifecycle::Interrupted
                | ComputerLifecycle::Terminated
        ) {
            return Err(ComputerError::InvalidLifecycle);
        }
        let layout = self.layout(&session);
        let source = layout.snapshots.join(snapshot_id.as_str());
        let manifest: SnapshotManifest = read_json(&source.join("snapshot.json"))?;
        if manifest.computer_session_id != session.id
            || manifest.profile_id != session.profile_id
            || manifest.id != *snapshot_id
        {
            return Err(ComputerError::SnapshotOwnership);
        }
        let mut remaining = session.limits.disk_bytes;
        replace_tree(&source.join("profile"), &layout.profile, &mut remaining)?;
        replace_tree(&source.join("workspace"), &layout.workspace, &mut remaining)?;
        replace_tree(&source.join("downloads"), &layout.downloads, &mut remaining)?;
        self.update_session(session_id, |session| {
            session.active_snapshot = Some(snapshot_id.clone());
            session.generation = session.generation.saturating_add(1);
            session.updated_at = now;
            session.last_activity_at = now;
            session.safe_error = None;
        })
    }

    /// Clears all mutable workstation state and returns the session to Created.
    ///
    /// # Errors
    ///
    /// Returns an ownership, runtime, or storage error.
    pub fn reset(
        &mut self,
        session_id: &ComputerSessionId,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ComputerSession, ComputerError> {
        let session = self.owned_session(session_id, profile_id)?.clone();
        if session.lifecycle == ComputerLifecycle::Deleted {
            return Err(ComputerError::InvalidLifecycle);
        }
        if self
            .runtime
            .is_running(session_id)
            .map_err(|error| ComputerError::Runtime(error.to_string()))?
        {
            self.runtime
                .terminate(&session)
                .map_err(|error| ComputerError::Runtime(error.to_string()))?;
        }
        self.erase_mutable_state(&session)?;
        self.create_layout(&session)?;
        self.update_session(session_id, |session| {
            session.lifecycle = ComputerLifecycle::Created;
            session.active_snapshot = None;
            session.generation = session.generation.saturating_add(1);
            session.updated_at = now;
            session.last_activity_at = now;
            session.safe_error = None;
        })
    }

    /// Terminates a computer process. Ephemeral sessions lose all mutable state immediately.
    ///
    /// # Errors
    ///
    /// Returns an ownership, runtime, or storage error.
    pub fn terminate(
        &mut self,
        session_id: &ComputerSessionId,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ComputerSession, ComputerError> {
        let session = self.owned_session(session_id, profile_id)?.clone();
        if session.lifecycle == ComputerLifecycle::Deleted {
            return Err(ComputerError::InvalidLifecycle);
        }
        if self
            .runtime
            .is_running(session_id)
            .map_err(|error| ComputerError::Runtime(error.to_string()))?
        {
            self.runtime
                .terminate(&session)
                .map_err(|error| ComputerError::Runtime(error.to_string()))?;
        }
        if session.mode == ComputerMode::Ephemeral {
            self.erase_mutable_state(&session)?;
        }
        self.update_session(session_id, |session| {
            session.lifecycle = ComputerLifecycle::Terminated;
            session.updated_at = now;
            session.safe_error = None;
        })
    }

    /// Completely deletes every computer owned by a profile.
    ///
    /// # Errors
    ///
    /// Returns a runtime or storage error. Other profiles are never touched.
    pub fn delete_profile(
        &mut self,
        profile_id: &ProfileId,
        _now: UtcTimestamp,
    ) -> Result<usize, ComputerError> {
        let owned = self
            .sessions
            .values()
            .filter(|session| &session.profile_id == profile_id)
            .cloned()
            .collect::<Vec<_>>();
        for session in &owned {
            if self
                .runtime
                .is_running(&session.id)
                .map_err(|error| ComputerError::Runtime(error.to_string()))?
            {
                self.runtime
                    .terminate(session)
                    .map_err(|error| ComputerError::Runtime(error.to_string()))?;
            }
        }
        let profile_root = self.root.join("profiles").join(profile_id.to_string());
        remove_dir_if_exists(&profile_root)?;
        for session in &owned {
            self.sessions.remove(&session.id);
            self.recent_action_times.remove(&session.id);
        }
        Ok(owned.len())
    }

    /// Reclaims computers whose explicit idle deadlines passed.
    ///
    /// Persistent sessions suspend; ephemeral sessions terminate and erase mutable state.
    ///
    /// # Errors
    ///
    /// Returns a runtime or storage error.
    pub fn reclaim_idle(
        &mut self,
        now: UtcTimestamp,
    ) -> Result<Vec<ComputerSessionId>, ComputerError> {
        let eligible = self
            .sessions
            .values()
            .filter(|session| {
                session.lifecycle == ComputerLifecycle::Running
                    && elapsed_ms(session.last_activity_at, now) >= session.limits.idle_timeout_ms
            })
            .map(|session| (session.id.clone(), session.profile_id.clone(), session.mode))
            .collect::<Vec<_>>();
        for (session_id, profile_id, mode) in &eligible {
            if *mode == ComputerMode::Persistent {
                self.suspend(session_id, profile_id, now)?;
            } else {
                self.terminate(session_id, profile_id, now)?;
            }
        }
        Ok(eligible.into_iter().map(|(id, _, _)| id).collect())
    }

    /// Returns a real observation from a running owned workstation.
    ///
    /// # Errors
    ///
    /// Returns an ownership, lifecycle, runtime, or validation error.
    pub fn observe(
        &mut self,
        session_id: &ComputerSessionId,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ComputerObservation, ComputerError> {
        let session = self.owned_session(session_id, profile_id)?.clone();
        self.ensure_running(&session)?;
        let observation = self
            .runtime
            .observe(&session, now)
            .map_err(|error| ComputerError::Runtime(error.to_string()))?;
        validate_observation_ownership(&observation, &session)?;
        observation.validate()?;
        self.update_session(session_id, |session| {
            session.last_activity_at = now;
            session.updated_at = now;
        })?;
        Ok(observation)
    }

    /// Executes an authority-checked action with post-action observation, bounded retries,
    /// alternate strategies, cancellation checks, stale-frame refusal, and audit output.
    ///
    /// # Errors
    ///
    /// Returns an ownership, lifecycle, authority, approval, cancellation, runtime, or
    /// progress error. Failed attempts remain visible through returned audit envelopes where
    /// execution reaches a terminal result.
    pub fn execute_action(
        &mut self,
        request: &ComputerActionRequest,
        boundary: &AuthorityBoundary,
        now: UtcTimestamp,
    ) -> Result<ComputerActionResult, ComputerError> {
        if request.alternates.len() > MAX_ALTERNATE_ACTIONS {
            return Err(ComputerError::TooManyAlternates);
        }
        let session = self
            .owned_session(&request.computer_session_id, &request.profile_id)?
            .clone();
        self.ensure_running(&session)?;
        request.progress.validate(&session.limits)?;
        self.admit_rate(&session, now)?;
        let cancellation_id = request.cancellation_id().clone();
        let mut observation = self.observe(&session.id, &session.profile_id, now)?;
        self.cancellations
            .entry(cancellation_id.clone())
            .or_insert_with(|| CancellationToken::new(cancellation_id.clone()));
        let mut audits = Vec::new();
        let mut attempts = 0_u8;
        let candidates = std::iter::once(&request.primary).chain(request.alternates.iter());
        for candidate in candidates {
            for _ in 0..=session.limits.max_retries {
                attempts = attempts.saturating_add(1);
                if self.is_cancelled(&cancellation_id) {
                    audits.push(audit(&candidate.authority, now, AuditOutcome::Cancelled));
                    self.cancellations.remove(&cancellation_id);
                    return Ok(ComputerActionResult {
                        disposition: ActionDisposition::Cancelled,
                        attempts,
                        observation,
                        audits,
                    });
                }
                if let Err(error) =
                    Self::authorize_attempt(candidate, boundary, &session, &observation, now)
                {
                    self.cancellations.remove(&cancellation_id);
                    return Err(error);
                }
                audits.push(audit(&candidate.authority, now, AuditOutcome::Approved));
                let token = self
                    .cancellations
                    .get(&cancellation_id)
                    .cloned()
                    .ok_or(ComputerError::Cancelled)?;
                let execution = self
                    .runtime
                    .execute(&session, &candidate.action, &token, now);
                let _execution = match execution {
                    Ok(execution) => execution,
                    Err(error) => {
                        audits.push(audit(&candidate.authority, now, AuditOutcome::Failed));
                        if self.is_cancelled(&cancellation_id) {
                            self.cancellations.remove(&cancellation_id);
                            return Ok(ComputerActionResult {
                                disposition: ActionDisposition::Cancelled,
                                attempts,
                                observation,
                                audits,
                            });
                        }
                        let _safe_error = safe_error(&error.to_string());
                        continue;
                    }
                };
                let after = match self.observe(&session.id, &session.profile_id, now) {
                    Ok(observation) => observation,
                    Err(error) => {
                        self.cancellations.remove(&cancellation_id);
                        return Err(error);
                    }
                };
                if request.progress.is_satisfied(&observation, &after) {
                    audits.push(audit(&candidate.authority, now, AuditOutcome::Completed));
                    self.cancellations.remove(&cancellation_id);
                    return Ok(ComputerActionResult {
                        disposition: ActionDisposition::Completed,
                        attempts,
                        observation: after,
                        audits,
                    });
                }
                audits.push(audit(&candidate.authority, now, AuditOutcome::Failed));
                observation = after;
            }
        }
        self.cancellations.remove(&cancellation_id);
        Ok(ComputerActionResult {
            disposition: ActionDisposition::NoProgress,
            attempts,
            observation,
            audits,
        })
    }

    pub fn cancel(&self, cancellation_id: &CancellationId) -> bool {
        if let Some(token) = self.cancellations.get(cancellation_id) {
            token.cancel();
            true
        } else {
            false
        }
    }

    /// Creates or returns a shared cancellation handle before an action is dispatched.
    /// A supervisor can cancel this handle from another thread while the runtime is executing.
    pub fn cancellation_handle(&mut self, cancellation_id: CancellationId) -> CancellationToken {
        self.cancellations
            .entry(cancellation_id.clone())
            .or_insert_with(|| CancellationToken::new(cancellation_id))
            .clone()
    }

    pub fn release_cancellation(&mut self, cancellation_id: &CancellationId) {
        self.cancellations.remove(cancellation_id);
    }

    /// Projects health from durable state and the actual owned process.
    ///
    /// # Errors
    ///
    /// Returns an ownership or runtime error.
    pub fn health(
        &mut self,
        session_id: &ComputerSessionId,
        profile_id: &ProfileId,
    ) -> Result<ComputerHealth, ComputerError> {
        let session = self.owned_session(session_id, profile_id)?.clone();
        let process_running = self
            .runtime
            .is_running(session_id)
            .map_err(|error| ComputerError::Runtime(error.to_string()))?;
        Ok(ComputerHealth {
            computer_session_id: session.id,
            profile_id: session.profile_id,
            lifecycle: session.lifecycle,
            process_running,
            restartable: matches!(
                session.lifecycle,
                ComputerLifecycle::Created
                    | ComputerLifecycle::Suspended
                    | ComputerLifecycle::Interrupted
            ),
            safe_error: session.safe_error,
            updated_at: session.updated_at,
        })
    }

    fn authorize_attempt(
        attempt: &ActionAttempt,
        boundary: &AuthorityBoundary,
        session: &ComputerSession,
        observation: &ComputerObservation,
        now: UtcTimestamp,
    ) -> Result<(), ComputerError> {
        attempt.action.validate(&session.limits)?;
        if attempt.authority.profile_id != session.profile_id
            || attempt.authority.requested_capability != Capability::ComputerControl
        {
            return Err(ComputerError::Authority);
        }
        if attempt.action.requires_consequential_approval()
            && attempt.authority.risk != ActionRisk::IrreversibleComputerInput
        {
            return Err(ComputerError::ApprovalRequired);
        }
        for source_frame in attempt.action.coordinate_frames() {
            if source_frame != &observation.screenshot.frame_id {
                return Err(ComputerError::StaleFrame {
                    expected: source_frame.clone(),
                    actual: observation.screenshot.frame_id.clone(),
                });
            }
        }
        let expected_digest = action_target_digest(&session.id, &attempt.action, observation)?;
        if attempt.authority.target_digest.as_str() != expected_digest {
            return Err(ComputerError::ApprovalTargetChanged);
        }
        boundary.authorizes(&attempt.authority, now)?;
        if let ComputerAction::CredentialFill { grant, .. } = &attempt.action {
            if grant.profile_id != session.profile_id || grant.expires_at <= now {
                return Err(ComputerError::InvalidCredentialGrant);
            }
            grant.validate()?;
            if observation
                .url
                .as_ref()
                .is_none_or(|url| !same_origin(url, grant.allowed_origin.as_str()))
            {
                return Err(ComputerError::InvalidCredentialGrant);
            }
        }
        Ok(())
    }

    fn start_or_resume(
        &mut self,
        session_id: &ComputerSessionId,
        profile_id: &ProfileId,
        now: UtcTimestamp,
        resume_only: bool,
    ) -> Result<ComputerSession, ComputerError> {
        let snapshot = self.owned_session(session_id, profile_id)?.clone();
        if !snapshot.lifecycle.can_start()
            || (resume_only && snapshot.lifecycle != ComputerLifecycle::Suspended)
        {
            return Err(ComputerError::InvalidLifecycle);
        }
        self.update_session(session_id, |session| {
            session.lifecycle = ComputerLifecycle::Starting;
            session.updated_at = now;
            session.safe_error = None;
        })?;
        let layout = self.layout(&snapshot);
        let result = if resume_only {
            self.runtime.resume(&snapshot, &layout)
        } else {
            self.runtime.start(&snapshot, &layout)
        };
        if let Err(error) = result {
            let redacted = safe_error(&error.to_string());
            self.update_session(session_id, |session| {
                session.lifecycle = ComputerLifecycle::Failed;
                session.updated_at = now;
                session.safe_error = Some(redacted.clone());
            })?;
            return Err(ComputerError::Runtime(error.to_string()));
        }
        self.update_session(session_id, |session| {
            session.lifecycle = ComputerLifecycle::Running;
            session.updated_at = now;
            session.last_activity_at = now;
            session.safe_error = None;
        })
    }

    fn ensure_running(&mut self, session: &ComputerSession) -> Result<(), ComputerError> {
        if session.lifecycle != ComputerLifecycle::Running {
            return Err(ComputerError::InvalidLifecycle);
        }
        if !self
            .runtime
            .is_running(&session.id)
            .map_err(|error| ComputerError::Runtime(error.to_string()))?
        {
            return Err(ComputerError::ProcessUnavailable);
        }
        Ok(())
    }

    fn admit_rate(
        &mut self,
        session: &ComputerSession,
        now: UtcTimestamp,
    ) -> Result<(), ComputerError> {
        let times = self
            .recent_action_times
            .entry(session.id.clone())
            .or_default();
        while times
            .front()
            .is_some_and(|timestamp| elapsed_ms(*timestamp, now) >= 60_000)
        {
            times.pop_front();
        }
        if times.len() >= session.limits.max_actions_per_minute as usize {
            return Err(ComputerError::ActionRateLimit);
        }
        times.push_back(now);
        Ok(())
    }

    fn is_cancelled(&self, cancellation_id: &CancellationId) -> bool {
        self.cancellations
            .get(cancellation_id)
            .is_some_and(CancellationToken::is_cancelled)
    }

    fn owned_session(
        &self,
        session_id: &ComputerSessionId,
        profile_id: &ProfileId,
    ) -> Result<&ComputerSession, ComputerError> {
        let session = self
            .sessions
            .get(session_id)
            .ok_or(ComputerError::NotFound)?;
        if !session.is_owned_by(profile_id) {
            return Err(ComputerError::ProfileIsolation);
        }
        Ok(session)
    }

    fn update_session(
        &mut self,
        session_id: &ComputerSessionId,
        update: impl FnOnce(&mut ComputerSession),
    ) -> Result<ComputerSession, ComputerError> {
        let updated = {
            let session = self
                .sessions
                .get_mut(session_id)
                .ok_or(ComputerError::NotFound)?;
            update(session);
            session.clone()
        };
        self.persist(&updated)?;
        Ok(updated)
    }

    fn layout(&self, session: &ComputerSession) -> ComputerSessionLayout {
        let root = self
            .root
            .join("profiles")
            .join(session.profile_id.to_string())
            .join(session.id.to_string());
        ComputerSessionLayout {
            profile: root.join("profile"),
            workspace: root.join("workspace"),
            downloads: root.join("downloads"),
            runtime: root.join("runtime"),
            snapshots: root.join("snapshots"),
            root,
        }
    }

    fn create_layout(&self, session: &ComputerSession) -> Result<(), ComputerError> {
        let layout = self.layout(session);
        for path in [
            &layout.profile,
            &layout.workspace,
            &layout.downloads,
            &layout.runtime,
            &layout.snapshots,
        ] {
            fs::create_dir_all(path)?;
        }
        Ok(())
    }

    fn erase_mutable_state(&self, session: &ComputerSession) -> Result<(), ComputerError> {
        let layout = self.layout(session);
        for path in [
            &layout.profile,
            &layout.workspace,
            &layout.downloads,
            &layout.runtime,
        ] {
            remove_dir_if_exists(path)?;
        }
        Ok(())
    }

    fn persist(&self, session: &ComputerSession) -> Result<(), ComputerError> {
        write_json_atomic(&self.layout(session).root.join("session.json"), session)
    }

    fn load_sessions(&mut self, now: UtcTimestamp) -> Result<(), ComputerError> {
        let profiles = self.root.join("profiles");
        for profile_entry in fs::read_dir(profiles)? {
            let profile_entry = profile_entry?;
            if !profile_entry.file_type()?.is_dir() {
                continue;
            }
            for session_entry in fs::read_dir(profile_entry.path())? {
                let session_entry = session_entry?;
                if !session_entry.file_type()?.is_dir() {
                    continue;
                }
                let path = session_entry.path().join("session.json");
                if !path.is_file() {
                    continue;
                }
                let mut session: ComputerSession = read_json(&path)?;
                if matches!(
                    session.lifecycle,
                    ComputerLifecycle::Running | ComputerLifecycle::Starting
                ) {
                    session.lifecycle = ComputerLifecycle::Interrupted;
                    session.updated_at = now;
                    session.safe_error = Some(safe_error(
                        "workstation process ended before reconciliation",
                    ));
                    write_json_atomic(&path, &session)?;
                }
                self.sessions.insert(session.id.clone(), session);
            }
        }
        Ok(())
    }
}

/// Computes the exact approval target for an action against the observed visual state.
///
/// # Errors
///
/// Returns a serialization error when the bounded action cannot be encoded.
pub fn action_target_digest(
    session_id: &ComputerSessionId,
    action: &ComputerAction,
    observation: &ComputerObservation,
) -> Result<String, ComputerError> {
    #[derive(Serialize)]
    struct DigestInput<'a> {
        session_id: &'a ComputerSessionId,
        coordinate_frames: Vec<&'a FrameId>,
        visual_digest: &'a str,
        url: &'a Option<String>,
        action: &'a ComputerAction,
    }
    let bytes = serde_json::to_vec(&DigestInput {
        session_id,
        coordinate_frames: action.coordinate_frames(),
        visual_digest: &observation.screenshot.content_digest,
        url: &observation.url,
        action,
    })?;
    let digest = Sha256::digest(bytes);
    let mut encoded = String::with_capacity(digest.len() * 2);
    for byte in digest {
        write!(encoded, "{byte:02x}").expect("writing to a string cannot fail");
    }
    Ok(encoded)
}

fn audit(action: &ExternalAction, now: UtcTimestamp, outcome: AuditOutcome) -> AuditEnvelope {
    AuditEnvelope {
        correlation_id: action.audit_correlation.clone(),
        profile_id: action.profile_id.clone(),
        session_id: action.session_id.clone(),
        acting_principal: action.acting_principal.clone(),
        capability: action.requested_capability,
        risk: action.risk,
        target_digest: action.target_digest.clone(),
        occurred_at: now,
        outcome,
    }
}

fn validate_observation_ownership(
    observation: &ComputerObservation,
    session: &ComputerSession,
) -> Result<(), ComputerError> {
    if observation.computer_session_id != session.id || observation.profile_id != session.profile_id
    {
        return Err(ComputerError::ProfileIsolation);
    }
    Ok(())
}

fn elapsed_ms(from: UtcTimestamp, to: UtcTimestamp) -> u64 {
    to.unix_millis()
        .saturating_sub(from.unix_millis())
        .try_into()
        .unwrap_or(0)
}

fn same_origin(url: &str, allowed_origin: &str) -> bool {
    let Ok(url) = url::Url::parse(url) else {
        return false;
    };
    let Ok(allowed) = url::Url::parse(allowed_origin) else {
        return false;
    };
    url.scheme() == allowed.scheme()
        && url.host_str() == allowed.host_str()
        && url.port_or_known_default() == allowed.port_or_known_default()
}

fn safe_error(error: &str) -> RedactedText {
    let sanitized = error
        .chars()
        .filter(|character| !character.is_control())
        .take(512)
        .collect::<String>();
    RedactedText::parse(if sanitized.trim().is_empty() {
        "computer operation failed".to_string()
    } else {
        sanitized
    })
    .unwrap_or_else(|_| RedactedText::parse("computer operation failed").expect("constant is safe"))
}

fn write_json_atomic(path: &Path, value: &impl Serialize) -> Result<(), ComputerError> {
    let parent = path.parent().ok_or(ComputerError::InvalidStorage)?;
    fs::create_dir_all(parent)?;
    let temporary = parent.join(format!(
        ".{}.{}.tmp",
        path.file_name()
            .and_then(|name| name.to_str())
            .unwrap_or("state"),
        std::process::id()
    ));
    let bytes = serde_json::to_vec_pretty(value)?;
    fs::write(&temporary, bytes)?;
    fs::rename(&temporary, path)?;
    Ok(())
}

fn read_json<T>(path: &Path) -> Result<T, ComputerError>
where
    T: serde::de::DeserializeOwned,
{
    Ok(serde_json::from_slice(&fs::read(path)?)?)
}

fn copy_tree(source: &Path, target: &Path, remaining: &mut u64) -> Result<(), ComputerError> {
    fs::create_dir_all(target)?;
    for entry in fs::read_dir(source)? {
        let entry = entry?;
        let file_type = entry.file_type()?;
        let destination = target.join(entry.file_name());
        if file_type.is_symlink() {
            return Err(ComputerError::UnsafeFilesystemEntry);
        }
        if file_type.is_dir() {
            copy_tree(&entry.path(), &destination, remaining)?;
        } else if file_type.is_file() {
            let file_bytes = entry.metadata()?.len();
            *remaining = remaining
                .checked_sub(file_bytes)
                .ok_or(ComputerError::DiskLimit)?;
            fs::copy(entry.path(), destination)?;
        } else {
            return Err(ComputerError::UnsafeFilesystemEntry);
        }
    }
    Ok(())
}

fn replace_tree(source: &Path, target: &Path, remaining: &mut u64) -> Result<(), ComputerError> {
    if !source.is_dir() {
        return Err(ComputerError::SnapshotNotFound);
    }
    remove_dir_if_exists(target)?;
    copy_tree(source, target, remaining)
}

fn remove_dir_if_exists(path: &Path) -> io::Result<()> {
    match fs::remove_dir_all(path) {
        Ok(()) => Ok(()),
        Err(error) if error.kind() == io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error),
    }
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct SnapshotManifest {
    id: ComputerSnapshotId,
    computer_session_id: ComputerSessionId,
    profile_id: ProfileId,
    created_at: UtcTimestamp,
    generation: u64,
}

#[derive(Debug, Error)]
pub enum ComputerError {
    #[error("computer session was not found")]
    NotFound,
    #[error("computer session belongs to another profile")]
    ProfileIsolation,
    #[error("computer lifecycle transition is invalid")]
    InvalidLifecycle,
    #[error("computer resource limits are invalid")]
    InvalidLimits,
    #[error("computer viewport is invalid")]
    InvalidViewport,
    #[error("computer action is invalid or outside its bounds")]
    InvalidAction,
    #[error("computer observation is malformed or exceeds bounds")]
    InvalidObservation,
    #[error("computer credential grant is invalid, expired, or out of scope")]
    InvalidCredentialGrant,
    #[error("computer action authority is invalid")]
    Authority,
    #[error("computer action requires exact approval")]
    ApprovalRequired,
    #[error("computer approval target changed after approval")]
    ApprovalTargetChanged,
    #[error("coordinate action frame is stale")]
    StaleFrame { expected: FrameId, actual: FrameId },
    #[error("computer action was cancelled")]
    Cancelled,
    #[error("computer action rate limit was reached")]
    ActionRateLimit,
    #[error("too many alternate computer actions were supplied")]
    TooManyAlternates,
    #[error("computer process is unavailable")]
    ProcessUnavailable,
    #[error("computer snapshot was not found")]
    SnapshotNotFound,
    #[error("computer snapshot belongs to another session or profile")]
    SnapshotOwnership,
    #[error("computer storage path is invalid")]
    InvalidStorage,
    #[error("computer state contains a symlink or unsupported filesystem entry")]
    UnsafeFilesystemEntry,
    #[error("computer state exceeds its disk limit")]
    DiskLimit,
    #[error("computer runtime failed: {0}")]
    Runtime(String),
    #[error(transparent)]
    Contract(#[from] ContractError),
    #[error(transparent)]
    Io(#[from] io::Error),
    #[error(transparent)]
    Json(#[from] serde_json::Error),
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    use keith_agent_types::{SessionId, UtcTimestamp};
    use keith_platform_contracts::{
        ActionRisk, ApprovalEnvelope, ApprovalState, AuditCorrelationId, CancellationId,
        Capability, CapabilityGrant, ExternalEffect, ExternalPrincipalId,
    };
    use tempfile::TempDir;

    use super::*;
    use crate::{
        ActionTarget, ComputerResourceLimits, IsolationRequirement, MouseButton, NetworkPolicy,
        Point, ProgressExpectation, Screenshot, Viewport,
    };

    #[derive(Default)]
    struct ProcessBackedRuntime {
        processes: BTreeMap<ComputerSessionId, std::process::Child>,
        frames: BTreeMap<ComputerSessionId, u128>,
    }

    impl Drop for ProcessBackedRuntime {
        fn drop(&mut self) {
            for process in self.processes.values_mut() {
                let _ = process.kill();
                let _ = process.wait();
            }
        }
    }

    #[derive(Debug, Error)]
    #[error("process runtime error")]
    struct ProcessRuntimeError;

    impl ComputerRuntime for ProcessBackedRuntime {
        type Error = ProcessRuntimeError;

        fn start(
            &mut self,
            session: &ComputerSession,
            _layout: &ComputerSessionLayout,
        ) -> Result<(), Self::Error> {
            let process = std::process::Command::new("/bin/sh")
                .args(["-c", "exec sleep 3600"])
                .stdin(std::process::Stdio::null())
                .stdout(std::process::Stdio::null())
                .stderr(std::process::Stdio::null())
                .spawn()
                .map_err(|_| ProcessRuntimeError)?;
            self.processes.insert(session.id.clone(), process);
            Ok(())
        }

        fn suspend(&mut self, session: &ComputerSession) -> Result<(), Self::Error> {
            stop_process(&mut self.processes, &session.id)?;
            Ok(())
        }

        fn resume(
            &mut self,
            session: &ComputerSession,
            layout: &ComputerSessionLayout,
        ) -> Result<(), Self::Error> {
            self.start(session, layout)
        }

        fn terminate(&mut self, session: &ComputerSession) -> Result<(), Self::Error> {
            stop_process(&mut self.processes, &session.id)
        }

        fn observe(
            &mut self,
            session: &ComputerSession,
            now: UtcTimestamp,
        ) -> Result<ComputerObservation, Self::Error> {
            let counter = self.frames.entry(session.id.clone()).or_insert(1);
            Ok(observation(session, *counter, now))
        }

        fn execute(
            &mut self,
            session: &ComputerSession,
            action: &ComputerAction,
            cancellation: &CancellationToken,
            _now: UtcTimestamp,
        ) -> Result<RuntimeActionResult, Self::Error> {
            assert!(!cancellation.is_cancelled());
            if let ComputerAction::Wait { duration_ms } = action {
                std::thread::sleep(std::time::Duration::from_millis(*duration_ms));
            } else {
                *self.frames.entry(session.id.clone()).or_insert(1) += 1;
            }
            Ok(RuntimeActionResult {
                description: "real process boundary action".into(),
            })
        }

        fn is_running(&mut self, session_id: &ComputerSessionId) -> Result<bool, Self::Error> {
            let Some(process) = self.processes.get_mut(session_id) else {
                return Ok(false);
            };
            Ok(process
                .try_wait()
                .map_err(|_| ProcessRuntimeError)?
                .is_none())
        }
    }

    fn stop_process(
        processes: &mut BTreeMap<ComputerSessionId, std::process::Child>,
        session_id: &ComputerSessionId,
    ) -> Result<(), ProcessRuntimeError> {
        if let Some(mut process) = processes.remove(session_id) {
            process.kill().map_err(|_| ProcessRuntimeError)?;
            process.wait().map_err(|_| ProcessRuntimeError)?;
        }
        Ok(())
    }

    fn request(profile_id: ProfileId, mode: ComputerMode) -> CreateComputerRequest {
        CreateComputerRequest {
            profile_id,
            mode,
            isolation: IsolationRequirement::ReducedExplicitlyAllowed,
            network: NetworkPolicy::Denied,
            viewport: Viewport::default(),
            limits: ComputerResourceLimits::default(),
        }
    }

    fn observation(
        session: &ComputerSession,
        frame: u128,
        now: UtcTimestamp,
    ) -> ComputerObservation {
        ComputerObservation {
            computer_session_id: session.id.clone(),
            profile_id: session.profile_id.clone(),
            captured_at: now,
            screenshot: Screenshot {
                frame_id: FrameId::from_u128(frame),
                content_digest: format!("frame-{frame}"),
                media_type: "image/png".into(),
                base64_data: "AA==".into(),
                width: session.viewport.width,
                height: session.viewport.height,
            },
            dom: None,
            accessibility: Vec::new(),
            focused_window: None,
            url: Some("about:blank".into()),
            viewport: session.viewport,
            cursor: Point::default(),
            dialogs: Vec::new(),
            downloads: Vec::new(),
            applications: Vec::new(),
            recent_actions: Vec::new(),
        }
    }

    #[test]
    fn persistent_lifecycle_snapshot_reset_restore_and_restart_reconcile() {
        let temp = TempDir::new().unwrap();
        let profile = ProfileId::new();
        let now = UtcTimestamp::from_unix_millis(10_000);
        let mut controller =
            ComputerController::open(temp.path(), ProcessBackedRuntime::default(), now).unwrap();
        let session = controller
            .create(request(profile.clone(), ComputerMode::Persistent), now)
            .unwrap();
        let running = controller.start(&session.id, &profile, now).unwrap();
        assert_eq!(running.lifecycle, ComputerLifecycle::Running);
        let suspended = controller.suspend(&session.id, &profile, now).unwrap();
        assert_eq!(suspended.lifecycle, ComputerLifecycle::Suspended);
        let layout = controller.layout(&suspended);
        fs::write(layout.workspace.join("proof.txt"), b"before").unwrap();
        let snapshot = controller.snapshot(&session.id, &profile, now).unwrap();
        fs::write(layout.workspace.join("proof.txt"), b"after").unwrap();
        controller
            .restore(&session.id, &profile, &snapshot, now)
            .unwrap();
        assert_eq!(
            fs::read(layout.workspace.join("proof.txt")).unwrap(),
            b"before"
        );
        controller.resume(&session.id, &profile, now).unwrap();
        drop(controller);
        let reopened =
            ComputerController::open(temp.path(), ProcessBackedRuntime::default(), now).unwrap();
        assert_eq!(
            reopened.session(&session.id).unwrap().lifecycle,
            ComputerLifecycle::Interrupted
        );
    }

    #[test]
    fn ephemeral_termination_erases_state_and_profile_delete_is_isolated() {
        let temp = TempDir::new().unwrap();
        let first = ProfileId::new();
        let second = ProfileId::new();
        let now = UtcTimestamp::from_unix_millis(20_000);
        let mut controller =
            ComputerController::open(temp.path(), ProcessBackedRuntime::default(), now).unwrap();
        let ephemeral = controller
            .create(request(first.clone(), ComputerMode::Ephemeral), now)
            .unwrap();
        let retained = controller
            .create(request(second.clone(), ComputerMode::Persistent), now)
            .unwrap();
        controller.start(&ephemeral.id, &first, now).unwrap();
        let layout = controller.layout(&ephemeral);
        fs::write(layout.workspace.join("secret.txt"), b"temporary").unwrap();
        controller.terminate(&ephemeral.id, &first, now).unwrap();
        assert!(!layout.workspace.exists());
        assert_eq!(controller.delete_profile(&first, now).unwrap(), 1);
        assert!(controller.session(&retained.id).is_some());
        assert!(matches!(
            controller.start(&retained.id, &first, now),
            Err(ComputerError::ProfileIsolation)
        ));
    }

    #[test]
    fn stale_coordinate_is_refused_and_exact_semantic_action_is_audited() {
        let temp = TempDir::new().unwrap();
        let profile = ProfileId::new();
        let now = UtcTimestamp::from_unix_millis(30_000);
        let mut controller =
            ComputerController::open(temp.path(), ProcessBackedRuntime::default(), now).unwrap();
        let session = controller
            .create(request(profile.clone(), ComputerMode::Persistent), now)
            .unwrap();
        controller.start(&session.id, &profile, now).unwrap();
        let initial = controller.observe(&session.id, &profile, now).unwrap();
        let action = ComputerAction::Click {
            target: ActionTarget::Coordinate {
                point: Point { x: 10, y: 10 },
                source_frame: FrameId::from_u128(999),
            },
            button: MouseButton::Left,
        };
        let attempt = approved_attempt(&session, &initial, action, now);
        let request = ComputerActionRequest {
            computer_session_id: session.id.clone(),
            profile_id: profile.clone(),
            primary: attempt,
            alternates: Vec::new(),
            progress: ProgressExpectation::FrameChanged,
        };
        let stale_boundary = boundary(&profile, &request.primary.authority.target);
        assert!(matches!(
            controller.execute_action(&request, &stale_boundary, now),
            Err(ComputerError::StaleFrame { .. })
        ));

        let current = controller.observe(&session.id, &profile, now).unwrap();
        let semantic = ComputerAction::Click {
            target: ActionTarget::Semantic {
                target: crate::SemanticTarget::Text {
                    text: "Continue".into(),
                },
            },
            button: MouseButton::Left,
        };
        let attempt = approved_attempt(&session, &current, semantic, now);
        let boundary = boundary(&profile, &attempt.authority.target);
        let result = controller
            .execute_action(
                &ComputerActionRequest {
                    computer_session_id: session.id.clone(),
                    profile_id: profile,
                    primary: attempt,
                    alternates: Vec::new(),
                    progress: ProgressExpectation::FrameChanged,
                },
                &boundary,
                now,
            )
            .unwrap();
        assert_eq!(result.disposition, ActionDisposition::Completed);
        assert_eq!(
            result.audits.last().unwrap().outcome,
            AuditOutcome::Completed
        );
    }

    #[test]
    fn idle_reclaim_alternates_and_prepared_cancellation_are_bounded() {
        let temp = TempDir::new().unwrap();
        let profile = ProfileId::new();
        let now = UtcTimestamp::from_unix_millis(40_000);
        let mut create = request(profile.clone(), ComputerMode::Persistent);
        create.limits.idle_timeout_ms = 100;
        create.limits.max_retries = 1;
        let mut controller =
            ComputerController::open(temp.path(), ProcessBackedRuntime::default(), now).unwrap();
        let session = controller.create(create, now).unwrap();
        controller.start(&session.id, &profile, now).unwrap();
        let initial = controller.observe(&session.id, &profile, now).unwrap();
        let primary = approved_attempt(
            &session,
            &initial,
            ComputerAction::Wait { duration_ms: 1 },
            now,
        );
        let alternate = approved_attempt(
            &session,
            &initial,
            ComputerAction::Move {
                target: ActionTarget::Semantic {
                    target: crate::SemanticTarget::Text {
                        text: "fallback".into(),
                    },
                },
            },
            now,
        );
        let alternate_boundary = boundary(&profile, &primary.authority.target);
        let result = controller
            .execute_action(
                &ComputerActionRequest {
                    computer_session_id: session.id.clone(),
                    profile_id: profile.clone(),
                    primary,
                    alternates: vec![alternate],
                    progress: ProgressExpectation::FrameChanged,
                },
                &alternate_boundary,
                now,
            )
            .unwrap();
        assert_eq!(result.disposition, ActionDisposition::Completed);
        assert_eq!(result.attempts, 3);

        let current = controller.observe(&session.id, &profile, now).unwrap();
        let cancelled = approved_attempt(
            &session,
            &current,
            ComputerAction::Move {
                target: ActionTarget::Semantic {
                    target: crate::SemanticTarget::Text {
                        text: "cancelled".into(),
                    },
                },
            },
            now,
        );
        controller
            .cancellation_handle(cancelled.authority.cancellation_id.clone())
            .cancel();
        let boundary = boundary(&profile, &cancelled.authority.target);
        let result = controller
            .execute_action(
                &ComputerActionRequest {
                    computer_session_id: session.id.clone(),
                    profile_id: profile.clone(),
                    primary: cancelled,
                    alternates: Vec::new(),
                    progress: ProgressExpectation::FrameChanged,
                },
                &boundary,
                now,
            )
            .unwrap();
        assert_eq!(result.disposition, ActionDisposition::Cancelled);

        let reclaimed = controller
            .reclaim_idle(UtcTimestamp::from_unix_millis(now.unix_millis() + 101))
            .unwrap();
        assert_eq!(reclaimed, vec![session.id.clone()]);
        assert_eq!(
            controller.session(&session.id).unwrap().lifecycle,
            ComputerLifecycle::Suspended
        );
    }

    fn approved_attempt(
        session: &ComputerSession,
        observation: &ComputerObservation,
        action: ComputerAction,
        now: UtcTimestamp,
    ) -> ActionAttempt {
        let digest = action_target_digest(&session.id, &action, observation).unwrap();
        let target = RedactedText::parse("computer-session").unwrap();
        ActionAttempt {
            action,
            authority: ExternalAction {
                profile_id: session.profile_id.clone(),
                session_id: SessionId::new(),
                acting_principal: ExternalPrincipalId::new(),
                requested_capability: Capability::ComputerControl,
                risk: ActionRisk::IrreversibleComputerInput,
                approval: ApprovalEnvelope {
                    risk: ActionRisk::IrreversibleComputerInput,
                    state: ApprovalState::Granted {
                        approval_id: keith_platform_contracts::ApprovalId::new(),
                        granted_by: ExternalPrincipalId::new(),
                        exact_target_digest: RedactedText::parse(digest.clone()).unwrap(),
                        expires_at: UtcTimestamp::from_unix_millis(now.unix_millis() + 1_000),
                    },
                },
                target,
                target_digest: RedactedText::parse(digest).unwrap(),
                cancellation_id: CancellationId::new(),
                reply_route: None,
                audit_correlation: AuditCorrelationId::new(),
                external_effect: ExternalEffect::NonRepeatable,
            },
        }
    }

    fn boundary(profile: &ProfileId, target: &RedactedText) -> AuthorityBoundary {
        AuthorityBoundary {
            profile_id: profile.clone(),
            allowed: BTreeSet::from([CapabilityGrant {
                capability: Capability::ComputerControl,
                resource: target.clone(),
                expires_at: None,
            }]),
            denied: BTreeSet::new(),
            max_automatic_risk: ActionRisk::IrreversibleComputerInput,
        }
    }
}
