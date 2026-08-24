use keith_agent_types::{
    AuditId, CURRENT_SCHEMA_VERSION, ComputerId, ProfileId, Revision, StableKey, TakeoverLeaseId,
    UtcTimestamp,
};
use thiserror::Error;

use crate::{
    AuditActor, ComputerAudit, ComputerAuditKind, ComputerError, ComputerRecord,
    ComputerRepository, ComputerRepositoryBatch, ComputerState, ControlState, ControlTransition,
    TakeoverLease, TakeoverState, TakeoverTransferMetadata,
};

pub const MIN_TAKEOVER_LEASE_MILLIS: u64 = 1_000;
pub const MAX_TAKEOVER_LEASE_MILLIS: u64 = 15 * 60 * 1_000;
const MAX_SAFE_BOUNDARY_REASON_BYTES: usize = 512;

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TakeoverAcquireRequest {
    pub owner_profile_id: ProfileId,
    pub expected_computer_revision: Revision,
    pub task_key: StableKey,
    pub token_digest_hex: String,
    pub lease_millis: u64,
    pub operation_key: StableKey,
    pub now: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TakeoverClaim {
    pub takeover_lease_id: TakeoverLeaseId,
    pub computer_id: ComputerId,
    pub owner_profile_id: ProfileId,
    pub task_key: StableKey,
    pub fencing_token: u64,
    pub lease_revision: Revision,
    pub expires_at: UtcTimestamp,
}

impl From<&TakeoverLease> for TakeoverClaim {
    fn from(lease: &TakeoverLease) -> Self {
        Self {
            takeover_lease_id: lease.takeover_lease_id.clone(),
            computer_id: lease.computer_id.clone(),
            owner_profile_id: lease.owner_profile_id.clone(),
            task_key: lease.task_key.clone(),
            fencing_token: lease.fencing_token,
            lease_revision: lease.revision,
            expires_at: lease.expires_at,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TakeoverRenewRequest {
    pub claim: TakeoverClaim,
    pub presented_token_digest_hex: String,
    pub replacement_token_digest_hex: String,
    pub lease_millis: u64,
    pub operation_key: StableKey,
    pub now: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TakeoverHandbackRequest {
    pub claim: TakeoverClaim,
    pub presented_token_digest_hex: String,
    pub operation_key: StableKey,
    pub now: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum UserActedKind {
    Navigation,
    Input,
    Authentication,
    Upload,
    Download,
    ConsequentialAction,
    Other,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TakeoverUserActedNote {
    pub operation_key: StableKey,
    pub kind: UserActedKind,
    pub safe_summary: String,
    pub acted_at: UtcTimestamp,
}

impl TakeoverUserActedNote {
    pub fn new(
        operation_key: StableKey,
        kind: UserActedKind,
        safe_summary: impl Into<String>,
        acted_at: UtcTimestamp,
    ) -> Result<Self, TakeoverServiceError> {
        let safe_summary = safe_summary.into();
        validate_safe_note(&safe_summary)?;
        Ok(Self {
            operation_key,
            kind,
            safe_summary,
            acted_at,
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TakeoverPauseBoundary {
    pub owner_profile_id: ProfileId,
    pub computer_id: ComputerId,
    pub task_key: StableKey,
    pub expected_computer_revision: Revision,
    pub operation_key: StableKey,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TakeoverResolutionBoundary {
    pub owner_profile_id: ProfileId,
    pub computer_id: ComputerId,
    pub task_key: StableKey,
    pub takeover_lease_id: TakeoverLeaseId,
    pub fencing_token: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RefreshedComputerObservation {
    pub observation_key: StableKey,
    pub observed_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TakeoverExpiryDeadline {
    pub owner_profile_id: ProfileId,
    pub takeover_lease_id: TakeoverLeaseId,
    pub lease_revision: Revision,
    pub fencing_token: u64,
    pub expires_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum OwningTaskResumeOrFail {
    Resumed {
        observation: RefreshedComputerObservation,
    },
    Failed {
        safe_reason: String,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TakeoverHandbackReceipt {
    pub claim: TakeoverClaim,
    pub user_acted: TakeoverUserActedNote,
    pub owning_task: OwningTaskResumeOrFail,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum TakeoverDueRecovery {
    NoLease,
    NotDue(TakeoverExpiryDeadline),
    Recovered(TakeoverResolution),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TakeoverBoundaryError {
    safe_reason: String,
}

impl TakeoverBoundaryError {
    pub fn new(safe_reason: impl Into<String>) -> Result<Self, TakeoverServiceError> {
        let safe_reason = safe_reason.into();
        if safe_reason.is_empty()
            || safe_reason.len() > MAX_SAFE_BOUNDARY_REASON_BYTES
            || safe_reason
                .chars()
                .any(|character| character.is_control() && !matches!(character, '\n' | '\t'))
        {
            return Err(TakeoverServiceError::InvalidBoundaryReason);
        }
        Ok(Self { safe_reason })
    }

    pub fn safe_reason(&self) -> &str {
        &self.safe_reason
    }
}

pub trait TakeoverTaskBoundary: Send + Sync {
    fn pause_agent_input(
        &self,
        boundary: &TakeoverPauseBoundary,
    ) -> Result<(), TakeoverBoundaryError>;

    fn release_uncommitted_pause(
        &self,
        boundary: &TakeoverPauseBoundary,
    ) -> Result<(), TakeoverBoundaryError>;

    fn refresh_observation(
        &self,
        boundary: &TakeoverResolutionBoundary,
    ) -> Result<RefreshedComputerObservation, TakeoverBoundaryError>;

    fn resume_owning_task(
        &self,
        boundary: &TakeoverResolutionBoundary,
        observation: &RefreshedComputerObservation,
    ) -> Result<(), TakeoverBoundaryError>;

    fn fail_owning_task(
        &self,
        boundary: &TakeoverResolutionBoundary,
        safe_reason: &str,
    ) -> Result<(), TakeoverBoundaryError>;
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum TakeoverResolution {
    Resumed {
        claim: TakeoverClaim,
        observation: RefreshedComputerObservation,
    },
    OwningTaskFailed {
        claim: TakeoverClaim,
        safe_reason: String,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum TakeoverRecovery {
    NoLease,
    Active(TakeoverClaim),
    Resolved(TakeoverResolution),
}

#[derive(Debug, Error)]
pub enum TakeoverServiceError {
    #[error(transparent)]
    Computer(#[from] ComputerError),
    #[error("takeover requires a ready computer with an active owning task")]
    NoOwningTask,
    #[error("another controller already owns the active takeover lease")]
    ControllerBusy,
    #[error("takeover controller token, fence, identity, or revision is stale")]
    StaleController,
    #[error("takeover lease duration is outside the allowed range")]
    InvalidLeaseDuration,
    #[error("takeover token digest is not canonical lowercase SHA-256")]
    InvalidTokenDigest,
    #[error("takeover boundary returned an unsafe reason")]
    InvalidBoundaryReason,
    #[error("takeover user-acted note is invalid or does not match the handback")]
    InvalidUserActedNote,
    #[error("takeover boundary failed: {0}")]
    Boundary(String),
    #[error("takeover state is inconsistent with the durable computer boundary")]
    InconsistentBoundary,
}

pub struct TakeoverLeaseService<R> {
    repository: R,
}

impl<R> TakeoverLeaseService<R>
where
    R: ComputerRepository,
{
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    pub const fn repository(&self) -> &R {
        &self.repository
    }

    pub fn into_repository(self) -> R {
        self.repository
    }

    pub fn acquire<B>(
        &self,
        request: TakeoverAcquireRequest,
        boundary: &B,
    ) -> Result<TakeoverClaim, TakeoverServiceError>
    where
        B: TakeoverTaskBoundary,
    {
        validate_duration(request.lease_millis)?;
        validate_digest(&request.token_digest_hex)?;
        let current = self.required_computer(&request.owner_profile_id)?;
        if current.revision != request.expected_computer_revision {
            return Err(ComputerError::RevisionConflict {
                expected: request.expected_computer_revision,
                actual: current.revision,
            }
            .into());
        }
        if current.state != ComputerState::Ready
            || current.control_state != ControlState::Agent
            || current.current_task_key.as_ref() != Some(&request.task_key)
        {
            return Err(TakeoverServiceError::NoOwningTask);
        }
        let previous = self.repository.lease(&request.owner_profile_id)?;
        if let Some(lease) = &previous {
            if lease.state == TakeoverState::Active && lease.expires_at > request.now {
                if lease.task_key == request.task_key
                    && lease.token_digest_hex == request.token_digest_hex
                {
                    return Ok(TakeoverClaim::from(lease));
                }
                return Err(TakeoverServiceError::ControllerBusy);
            }
            if lease.state == TakeoverState::Active {
                return Err(TakeoverServiceError::InconsistentBoundary);
            }
        }

        let pause = TakeoverPauseBoundary {
            owner_profile_id: request.owner_profile_id.clone(),
            computer_id: current.computer_id.clone(),
            task_key: request.task_key.clone(),
            expected_computer_revision: current.revision,
            operation_key: request.operation_key,
        };
        boundary.pause_agent_input(&pause).map_err(boundary_error)?;

        let next_computer_revision = next_revision(current.revision)?;
        let mut computer = current.clone();
        computer.control_state = ControlState::UserTakeover;
        computer.updated_at = request.now;
        computer.revision = next_computer_revision;

        let (lease_revision, fencing_token) = match &previous {
            Some(lease) => (
                next_revision(next_revision(lease.revision)?)?,
                next_fence(lease.fencing_token)?,
            ),
            None => (Revision::ZERO, 1),
        };
        let expires_at = lease_expiry(request.now, request.lease_millis)?;
        let lease = TakeoverLease {
            version: CURRENT_SCHEMA_VERSION,
            takeover_lease_id: TakeoverLeaseId::new(),
            computer_id: computer.computer_id.clone(),
            owner_profile_id: computer.owner_profile_id.clone(),
            task_key: request.task_key,
            token_digest_hex: request.token_digest_hex,
            fencing_token,
            acquired_at: request.now,
            renewed_at: request.now,
            expires_at,
            state: TakeoverState::Active,
            revision: lease_revision,
        };
        let audit = self.takeover_audit(
            &computer,
            &lease,
            ComputerAuditKind::TakeoverAcquired,
            ControlState::Agent,
            ControlState::UserTakeover,
            AuditActor::Owner,
            "owner acquired durable computer control",
            None,
            request.now,
        )?;
        let mut changes = vec![ComputerRepositoryBatch::ReplaceComputer {
            expected_revision: current.revision,
            record: computer,
        }];
        if let Some(previous) = previous {
            changes.push(ComputerRepositoryBatch::RemoveLease {
                owner_profile_id: previous.owner_profile_id,
                expected_revision: previous.revision,
            });
        }
        changes.push(ComputerRepositoryBatch::PutLease {
            expected_revision: None,
            lease: lease.clone(),
        });
        changes.push(ComputerRepositoryBatch::AppendAudit(audit));
        if let Err(error) = self.repository.transact(&changes) {
            let durable = self.repository.computer(&request.owner_profile_id)?;
            if durable.is_some_and(|record| record.control_state != ControlState::UserTakeover) {
                boundary
                    .release_uncommitted_pause(&pause)
                    .map_err(boundary_error)?;
            }
            return Err(error.into());
        }
        Ok(TakeoverClaim::from(&lease))
    }

    pub fn renew(
        &self,
        request: TakeoverRenewRequest,
    ) -> Result<TakeoverClaim, TakeoverServiceError> {
        validate_duration(request.lease_millis)?;
        validate_digest(&request.presented_token_digest_hex)?;
        validate_digest(&request.replacement_token_digest_hex)?;
        let current = self.required_active_lease(&request.claim.owner_profile_id)?;
        if current.token_digest_hex == request.replacement_token_digest_hex
            && current.fencing_token == request.claim.fencing_token.saturating_add(1)
            && current.revision > request.claim.lease_revision
        {
            return Ok(TakeoverClaim::from(&current));
        }
        validate_claim(
            &current,
            &request.claim,
            &request.presented_token_digest_hex,
        )?;
        if request.now >= current.expires_at {
            return Err(TakeoverServiceError::StaleController);
        }
        if request.presented_token_digest_hex == request.replacement_token_digest_hex {
            return Err(TakeoverServiceError::InvalidTokenDigest);
        }
        let computer = self.required_computer(&current.owner_profile_id)?;
        if computer.control_state != ControlState::UserTakeover
            || computer.current_task_key.as_ref() != Some(&current.task_key)
        {
            return Err(TakeoverServiceError::InconsistentBoundary);
        }
        let expected_revision = current.revision;
        let mut renewed = current;
        renewed.token_digest_hex = request.replacement_token_digest_hex;
        renewed.fencing_token = next_fence(renewed.fencing_token)?;
        renewed.renewed_at = request.now;
        renewed.expires_at = lease_expiry(request.now, request.lease_millis)?;
        renewed.revision = next_revision(expected_revision)?;
        let audit = self.takeover_audit(
            &computer,
            &renewed,
            ComputerAuditKind::TakeoverRenewed,
            ControlState::UserTakeover,
            ControlState::UserTakeover,
            AuditActor::Owner,
            "owner renewed durable computer control",
            None,
            request.now,
        )?;
        self.repository.transact(&[
            ComputerRepositoryBatch::PutLease {
                expected_revision: Some(expected_revision),
                lease: renewed.clone(),
            },
            ComputerRepositoryBatch::AppendAudit(audit),
        ])?;
        Ok(TakeoverClaim::from(&renewed))
    }

    pub fn handback<B>(
        &self,
        request: TakeoverHandbackRequest,
        boundary: &B,
    ) -> Result<TakeoverResolution, TakeoverServiceError>
    where
        B: TakeoverTaskBoundary,
    {
        self.handback_internal(request, None, boundary)
    }

    pub fn handback_user_acted<B>(
        &self,
        request: TakeoverHandbackRequest,
        user_acted: TakeoverUserActedNote,
        boundary: &B,
    ) -> Result<TakeoverHandbackReceipt, TakeoverServiceError>
    where
        B: TakeoverTaskBoundary,
    {
        if user_acted.operation_key != request.operation_key || user_acted.acted_at > request.now {
            return Err(TakeoverServiceError::InvalidUserActedNote);
        }
        validate_safe_note(&user_acted.safe_summary)?;
        let resolution = self.handback_internal(request, Some(&user_acted), boundary)?;
        let (claim, owning_task) = match resolution {
            TakeoverResolution::Resumed { claim, observation } => {
                (claim, OwningTaskResumeOrFail::Resumed { observation })
            }
            TakeoverResolution::OwningTaskFailed { claim, safe_reason } => {
                (claim, OwningTaskResumeOrFail::Failed { safe_reason })
            }
        };
        Ok(TakeoverHandbackReceipt {
            claim,
            user_acted,
            owning_task,
        })
    }

    fn handback_internal<B>(
        &self,
        request: TakeoverHandbackRequest,
        user_acted: Option<&TakeoverUserActedNote>,
        boundary: &B,
    ) -> Result<TakeoverResolution, TakeoverServiceError>
    where
        B: TakeoverTaskBoundary,
    {
        validate_digest(&request.presented_token_digest_hex)?;
        let lease = self
            .repository
            .lease(&request.claim.owner_profile_id)?
            .ok_or_else(|| ComputerError::MissingLease(request.claim.owner_profile_id.clone()))?;
        if lease.state != TakeoverState::Active {
            validate_terminal_replay_claim(&lease, &request.claim)?;
            return self.complete_resolution(lease, boundary, request.now);
        }
        validate_claim(&lease, &request.claim, &request.presented_token_digest_hex)?;
        let state = if request.now >= lease.expires_at {
            TakeoverState::Expired
        } else {
            TakeoverState::HandedBack
        };
        let actor = if state == TakeoverState::Expired {
            AuditActor::System
        } else {
            AuditActor::Owner
        };
        let terminal = self.pause_and_invalidate(lease, state, actor, user_acted, request.now)?;
        self.complete_resolution(terminal, boundary, request.now)
    }

    pub fn expiry_deadline(
        &self,
        owner: &ProfileId,
    ) -> Result<Option<TakeoverExpiryDeadline>, TakeoverServiceError> {
        Ok(self.repository.lease(owner)?.and_then(|lease| {
            (lease.state == TakeoverState::Active).then(|| TakeoverExpiryDeadline {
                owner_profile_id: lease.owner_profile_id,
                takeover_lease_id: lease.takeover_lease_id,
                lease_revision: lease.revision,
                fencing_token: lease.fencing_token,
                expires_at: lease.expires_at,
            })
        }))
    }

    pub fn recover_due<B>(
        &self,
        owner: &ProfileId,
        boundary: &B,
        now: UtcTimestamp,
    ) -> Result<TakeoverDueRecovery, TakeoverServiceError>
    where
        B: TakeoverTaskBoundary,
    {
        let Some(deadline) = self.expiry_deadline(owner)? else {
            return match self.expire_and_recover(owner, boundary, now)? {
                TakeoverRecovery::NoLease => Ok(TakeoverDueRecovery::NoLease),
                TakeoverRecovery::Resolved(resolution) => {
                    Ok(TakeoverDueRecovery::Recovered(resolution))
                }
                TakeoverRecovery::Active(_) => Err(TakeoverServiceError::InconsistentBoundary),
            };
        };
        if now < deadline.expires_at {
            return Ok(TakeoverDueRecovery::NotDue(deadline));
        }
        match self.expire_and_recover(owner, boundary, now)? {
            TakeoverRecovery::Resolved(resolution) => {
                Ok(TakeoverDueRecovery::Recovered(resolution))
            }
            TakeoverRecovery::NoLease => Ok(TakeoverDueRecovery::NoLease),
            TakeoverRecovery::Active(_) => Err(TakeoverServiceError::InconsistentBoundary),
        }
    }

    pub fn expire_and_recover<B>(
        &self,
        owner: &ProfileId,
        boundary: &B,
        now: UtcTimestamp,
    ) -> Result<TakeoverRecovery, TakeoverServiceError>
    where
        B: TakeoverTaskBoundary,
    {
        let Some(lease) = self.repository.lease(owner)? else {
            let computer = self.required_computer(owner)?;
            if computer.control_state == ControlState::Paused {
                return Err(TakeoverServiceError::InconsistentBoundary);
            }
            return Ok(TakeoverRecovery::NoLease);
        };
        if lease.state == TakeoverState::Active && now < lease.expires_at {
            let computer = self.required_computer(owner)?;
            if computer.control_state != ControlState::UserTakeover
                || computer.current_task_key.as_ref() != Some(&lease.task_key)
            {
                return Err(TakeoverServiceError::InconsistentBoundary);
            }
            return Ok(TakeoverRecovery::Active(TakeoverClaim::from(&lease)));
        }
        let terminal = if lease.state == TakeoverState::Active {
            self.pause_and_invalidate(lease, TakeoverState::Expired, AuditActor::System, None, now)?
        } else {
            lease
        };
        Ok(TakeoverRecovery::Resolved(
            self.complete_resolution(terminal, boundary, now)?,
        ))
    }

    fn pause_and_invalidate(
        &self,
        current: TakeoverLease,
        state: TakeoverState,
        actor: AuditActor,
        user_acted: Option<&TakeoverUserActedNote>,
        now: UtcTimestamp,
    ) -> Result<TakeoverLease, TakeoverServiceError> {
        let mut computer = self.required_computer(&current.owner_profile_id)?;
        if computer.control_state != ControlState::UserTakeover
            || computer.current_task_key.as_ref() != Some(&current.task_key)
        {
            return Err(TakeoverServiceError::InconsistentBoundary);
        }
        let expected_computer_revision = computer.revision;
        computer.control_state = ControlState::Paused;
        computer.updated_at = now;
        computer.revision = next_revision(expected_computer_revision)?;

        let expected_lease_revision = current.revision;
        let mut terminal = current;
        terminal.token_digest_hex = invalidated_digest(&terminal.token_digest_hex);
        terminal.fencing_token = next_fence(terminal.fencing_token)?;
        terminal.state = state;
        terminal.renewed_at = terminal.renewed_at.max(now.min(terminal.expires_at));
        if terminal.expires_at <= terminal.renewed_at {
            terminal.expires_at =
                UtcTimestamp::from_unix_millis(terminal.renewed_at.unix_millis().saturating_add(1));
        }
        terminal.revision = next_revision(expected_lease_revision)?;
        let kind = if state == TakeoverState::Expired {
            ComputerAuditKind::TakeoverExpired
        } else {
            ComputerAuditKind::TakeoverHandedBack
        };
        let summary = if state == TakeoverState::Expired {
            "takeover expired and agent input remained paused"
        } else {
            "owner acted and handed computer control back"
        };
        let audit = self.takeover_audit(
            &computer,
            &terminal,
            kind,
            ControlState::UserTakeover,
            ControlState::Paused,
            actor,
            summary,
            user_acted,
            now,
        )?;
        self.repository.transact(&[
            ComputerRepositoryBatch::ReplaceComputer {
                expected_revision: expected_computer_revision,
                record: computer,
            },
            ComputerRepositoryBatch::PutLease {
                expected_revision: Some(expected_lease_revision),
                lease: terminal.clone(),
            },
            ComputerRepositoryBatch::AppendAudit(audit),
        ])?;
        Ok(terminal)
    }

    fn complete_resolution<B>(
        &self,
        lease: TakeoverLease,
        boundary: &B,
        now: UtcTimestamp,
    ) -> Result<TakeoverResolution, TakeoverServiceError>
    where
        B: TakeoverTaskBoundary,
    {
        if lease.state == TakeoverState::Active {
            return Err(TakeoverServiceError::ControllerBusy);
        }
        let computer = self.required_computer(&lease.owner_profile_id)?;
        if computer.control_state == ControlState::Agent
            && computer.current_task_key.as_ref() == Some(&lease.task_key)
        {
            let observation = RefreshedComputerObservation {
                observation_key: resolution_key(&lease, "recovered")?,
                observed_at: computer.updated_at,
            };
            return Ok(TakeoverResolution::Resumed {
                claim: TakeoverClaim::from(&lease),
                observation,
            });
        }
        if computer.control_state == ControlState::Idle && computer.current_task_key.is_none() {
            return Ok(TakeoverResolution::OwningTaskFailed {
                claim: TakeoverClaim::from(&lease),
                safe_reason: "owning task was safely failed after takeover".into(),
            });
        }
        if computer.control_state != ControlState::Paused
            || computer.current_task_key.as_ref() != Some(&lease.task_key)
        {
            return Err(TakeoverServiceError::InconsistentBoundary);
        }
        let resolution = TakeoverResolutionBoundary {
            owner_profile_id: lease.owner_profile_id.clone(),
            computer_id: lease.computer_id.clone(),
            task_key: lease.task_key.clone(),
            takeover_lease_id: lease.takeover_lease_id.clone(),
            fencing_token: lease.fencing_token,
        };
        let observation = match boundary.refresh_observation(&resolution) {
            Ok(observation) => observation,
            Err(error) => {
                let reason = error.safe_reason().to_owned();
                boundary
                    .fail_owning_task(&resolution, &reason)
                    .map_err(boundary_error)?;
                return self.persist_resolution(lease, computer, None, Some(reason), now);
            }
        };
        match boundary.resume_owning_task(&resolution, &observation) {
            Ok(()) => self.persist_resolution(lease, computer, Some(observation), None, now),
            Err(error) => {
                let reason = error.safe_reason().to_owned();
                boundary
                    .fail_owning_task(&resolution, &reason)
                    .map_err(boundary_error)?;
                self.persist_resolution(lease, computer, None, Some(reason), now)
            }
        }
    }

    fn persist_resolution(
        &self,
        lease: TakeoverLease,
        mut computer: ComputerRecord,
        observation: Option<RefreshedComputerObservation>,
        safe_failure: Option<String>,
        now: UtcTimestamp,
    ) -> Result<TakeoverResolution, TakeoverServiceError> {
        let expected_revision = computer.revision;
        let from = computer.control_state;
        computer.control_state = if safe_failure.is_some() {
            computer.current_task_key = None;
            ControlState::Idle
        } else {
            ControlState::Agent
        };
        computer.updated_at = now;
        computer.revision = next_revision(expected_revision)?;
        let audit = self.resolution_audit(
            &computer,
            &lease,
            from,
            safe_failure.clone(),
            observation.as_ref(),
            now,
        )?;
        self.repository.transact(&[
            ComputerRepositoryBatch::ReplaceComputer {
                expected_revision,
                record: computer,
            },
            ComputerRepositoryBatch::AppendAudit(audit),
        ])?;
        let claim = TakeoverClaim::from(&lease);
        match (observation, safe_failure) {
            (Some(observation), None) => Ok(TakeoverResolution::Resumed { claim, observation }),
            (None, Some(safe_reason)) => {
                Ok(TakeoverResolution::OwningTaskFailed { claim, safe_reason })
            }
            _ => Err(TakeoverServiceError::InconsistentBoundary),
        }
    }

    fn required_computer(&self, owner: &ProfileId) -> Result<ComputerRecord, ComputerError> {
        self.repository
            .computer(owner)?
            .ok_or_else(|| ComputerError::MissingComputer(owner.clone()))
    }

    fn required_active_lease(&self, owner: &ProfileId) -> Result<TakeoverLease, ComputerError> {
        self.repository
            .lease(owner)?
            .filter(|lease| lease.state == TakeoverState::Active)
            .ok_or_else(|| ComputerError::MissingLease(owner.clone()))
    }

    #[allow(clippy::too_many_arguments)]
    fn takeover_audit(
        &self,
        computer: &ComputerRecord,
        lease: &TakeoverLease,
        kind: ComputerAuditKind,
        from: ControlState,
        to: ControlState,
        actor: AuditActor,
        summary: &str,
        user_acted: Option<&TakeoverUserActedNote>,
        now: UtcTimestamp,
    ) -> Result<ComputerAudit, TakeoverServiceError> {
        let sequence = next_audit_sequence(&self.repository, &computer.owner_profile_id)?;
        Ok(ComputerAudit {
            version: CURRENT_SCHEMA_VERSION,
            audit_id: AuditId::new(),
            computer_id: computer.computer_id.clone(),
            owner_profile_id: computer.owner_profile_id.clone(),
            sequence,
            stable_key: if let Some(note) = user_acted {
                StableKey::parse(format!("takeover/user-acted/{}", note.operation_key))
                    .map_err(|_| ComputerError::Malformed("user acted audit stable key"))?
            } else {
                audit_key(
                    lease,
                    match kind {
                        ComputerAuditKind::TakeoverAcquired => "acquire",
                        ComputerAuditKind::TakeoverRenewed => "renew",
                        ComputerAuditKind::TakeoverHandedBack => "handback",
                        ComputerAuditKind::TakeoverExpired => "expire",
                        _ => "takeover",
                    },
                )?
            },
            actor,
            kind,
            task_key: Some(lease.task_key.clone()),
            navigation_origin: None,
            control_transition: Some(ControlTransition { from, to }),
            policy_decision: None,
            side_effect_summary: user_acted
                .map(|note| format!("user acted ({:?}): {}", note.kind, note.safe_summary)),
            transfer: Some(TakeoverTransferMetadata {
                lease_id: lease.takeover_lease_id.clone(),
                fencing_token: lease.fencing_token,
                from,
                to,
            }),
            safe_failure: None,
            recovery_correlation: None,
            safe_summary: summary.into(),
            occurred_at: now,
            computer_revision: computer.revision,
        })
    }

    fn resolution_audit(
        &self,
        computer: &ComputerRecord,
        lease: &TakeoverLease,
        from: ControlState,
        safe_failure: Option<String>,
        observation: Option<&RefreshedComputerObservation>,
        now: UtcTimestamp,
    ) -> Result<ComputerAudit, TakeoverServiceError> {
        let sequence = next_audit_sequence(&self.repository, &computer.owner_profile_id)?;
        Ok(ComputerAudit {
            version: CURRENT_SCHEMA_VERSION,
            audit_id: AuditId::new(),
            computer_id: computer.computer_id.clone(),
            owner_profile_id: computer.owner_profile_id.clone(),
            sequence,
            stable_key: resolution_key(
                lease,
                if safe_failure.is_some() {
                    "failed"
                } else {
                    "resumed"
                },
            )?,
            actor: AuditActor::System,
            kind: ComputerAuditKind::TaskChanged,
            task_key: Some(lease.task_key.clone()),
            navigation_origin: None,
            control_transition: Some(ControlTransition {
                from,
                to: computer.control_state,
            }),
            policy_decision: None,
            side_effect_summary: observation
                .map(|value| format!("refreshed observation {}", value.observation_key)),
            transfer: None,
            safe_failure,
            recovery_correlation: None,
            safe_summary: if computer.control_state == ControlState::Agent {
                "refreshed observation and resumed owning task".into()
            } else {
                "owning task safely failed after takeover".into()
            },
            occurred_at: now,
            computer_revision: computer.revision,
        })
    }
}

fn validate_claim(
    lease: &TakeoverLease,
    claim: &TakeoverClaim,
    token_digest_hex: &str,
) -> Result<(), TakeoverServiceError> {
    if lease.takeover_lease_id != claim.takeover_lease_id
        || lease.computer_id != claim.computer_id
        || lease.owner_profile_id != claim.owner_profile_id
        || lease.task_key != claim.task_key
        || lease.fencing_token != claim.fencing_token
        || lease.revision != claim.lease_revision
        || lease.token_digest_hex != token_digest_hex
    {
        return Err(TakeoverServiceError::StaleController);
    }
    Ok(())
}

fn validate_terminal_replay_claim(
    lease: &TakeoverLease,
    claim: &TakeoverClaim,
) -> Result<(), TakeoverServiceError> {
    if lease.takeover_lease_id != claim.takeover_lease_id
        || lease.computer_id != claim.computer_id
        || lease.owner_profile_id != claim.owner_profile_id
        || lease.task_key != claim.task_key
        || lease.fencing_token != claim.fencing_token.saturating_add(1)
        || lease.revision
            != claim
                .lease_revision
                .checked_next()
                .unwrap_or(claim.lease_revision)
    {
        return Err(TakeoverServiceError::StaleController);
    }
    Ok(())
}

fn validate_duration(duration: u64) -> Result<(), TakeoverServiceError> {
    if !(MIN_TAKEOVER_LEASE_MILLIS..=MAX_TAKEOVER_LEASE_MILLIS).contains(&duration) {
        return Err(TakeoverServiceError::InvalidLeaseDuration);
    }
    Ok(())
}

fn validate_digest(digest: &str) -> Result<(), TakeoverServiceError> {
    if digest.len() != 64
        || !digest
            .bytes()
            .all(|byte| byte.is_ascii_digit() || matches!(byte, b'a'..=b'f'))
    {
        return Err(TakeoverServiceError::InvalidTokenDigest);
    }
    Ok(())
}

fn validate_safe_note(note: &str) -> Result<(), TakeoverServiceError> {
    if note.trim().is_empty()
        || note.len() > MAX_SAFE_BOUNDARY_REASON_BYTES
        || note
            .chars()
            .any(|character| character.is_control() && !matches!(character, '\n' | '\t'))
    {
        return Err(TakeoverServiceError::InvalidUserActedNote);
    }
    Ok(())
}

fn lease_expiry(
    now: UtcTimestamp,
    duration_millis: u64,
) -> Result<UtcTimestamp, TakeoverServiceError> {
    let duration =
        i64::try_from(duration_millis).map_err(|_| TakeoverServiceError::InvalidLeaseDuration)?;
    let millis = now
        .unix_millis()
        .checked_add(duration)
        .ok_or(TakeoverServiceError::InvalidLeaseDuration)?;
    Ok(UtcTimestamp::from_unix_millis(millis))
}

fn next_revision(revision: Revision) -> Result<Revision, ComputerError> {
    revision
        .checked_next()
        .ok_or(ComputerError::RevisionOverflow)
}

fn next_fence(fence: u64) -> Result<u64, ComputerError> {
    fence.checked_add(1).ok_or(ComputerError::RevisionOverflow)
}

fn invalidated_digest(current: &str) -> String {
    if current == "0".repeat(64) {
        "1".repeat(64)
    } else {
        "0".repeat(64)
    }
}

fn audit_key(lease: &TakeoverLease, phase: &str) -> Result<StableKey, ComputerError> {
    StableKey::parse(format!(
        "takeover/{}/{}/{phase}",
        lease.takeover_lease_id, lease.fencing_token
    ))
    .map_err(|_| ComputerError::Malformed("takeover audit stable key"))
}

fn resolution_key(lease: &TakeoverLease, phase: &str) -> Result<StableKey, ComputerError> {
    StableKey::parse(format!(
        "takeover/{}/{}/{phase}",
        lease.takeover_lease_id, lease.fencing_token
    ))
    .map_err(|_| ComputerError::Malformed("takeover resolution stable key"))
}

fn next_audit_sequence<R>(repository: &R, owner: &ProfileId) -> Result<u64, ComputerError>
where
    R: ComputerRepository,
{
    u64::try_from(repository.audit(owner)?.len())
        .ok()
        .and_then(|sequence| sequence.checked_add(1))
        .ok_or(ComputerError::AuditConflict)
}

fn boundary_error(error: TakeoverBoundaryError) -> TakeoverServiceError {
    TakeoverServiceError::Boundary(error.safe_reason)
}
