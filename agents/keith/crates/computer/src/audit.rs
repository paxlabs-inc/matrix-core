use keith_agent_types::{
    AuditId, CURRENT_SCHEMA_VERSION, ComputerId, ProfileId, Revision, StableKey, UtcTimestamp,
};
use thiserror::Error;

use crate::model::{
    AuditActor, ComputerAudit, ComputerAuditKind, ComputerError, ComputerRepository,
    ComputerRepositoryBatch, ControlTransition, PolicyDecision, TakeoverTransferMetadata,
};

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NewComputerAudit {
    pub stable_key: StableKey,
    pub owner_profile_id: ProfileId,
    pub computer_id: ComputerId,
    pub actor: AuditActor,
    pub kind: ComputerAuditKind,
    pub task_key: Option<StableKey>,
    pub navigation_origin: Option<String>,
    pub control_transition: Option<ControlTransition>,
    pub policy_decision: Option<PolicyDecision>,
    pub side_effect_summary: Option<String>,
    pub transfer: Option<TakeoverTransferMetadata>,
    pub safe_failure: Option<String>,
    pub recovery_correlation: Option<StableKey>,
    pub safe_summary: String,
    pub occurred_at: UtcTimestamp,
    pub expected_computer_revision: Revision,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComputerAuditContext {
    pub operation_key: StableKey,
    pub owner_profile_id: ProfileId,
    pub computer_id: ComputerId,
    pub actor: AuditActor,
    pub task_key: Option<StableKey>,
    pub occurred_at: UtcTimestamp,
    pub expected_computer_revision: Revision,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ComputerSideEffectKind {
    ConsequentialAction,
    Upload,
    Download,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum TypedComputerAuditEvent {
    Navigation {
        origin: String,
        decision: PolicyDecision,
        safe_summary: String,
    },
    Input {
        transition: Option<ControlTransition>,
        safe_summary: String,
    },
    ControlTransition {
        transition: ControlTransition,
        safe_summary: String,
    },
    Policy {
        decision: PolicyDecision,
        safe_summary: String,
    },
    SideEffect {
        effect: ComputerSideEffectKind,
        decision: PolicyDecision,
        safe_summary: String,
    },
    Failure {
        safe_failure: String,
        safe_summary: String,
    },
    Recovery {
        correlation_key: StableKey,
        safe_summary: String,
    },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComputerAuditReceipt {
    pub audit_id: AuditId,
    pub owner_profile_id: ProfileId,
    pub sequence: u64,
    pub stable_key: StableKey,
    pub duplicate: bool,
}

#[derive(Debug, Error)]
pub enum ComputerAuditServiceError {
    #[error(transparent)]
    Repository(#[from] ComputerError),
    #[error("computer audit subject is missing or stale")]
    StaleSubject,
    #[error("computer audit stable key conflicts with a different event")]
    StableKeyConflict,
    #[error("computer audit origin is invalid")]
    InvalidOrigin,
    #[error("computer audit sequence overflow")]
    SequenceOverflow,
    #[error("computer audit event key is invalid")]
    InvalidEventKey,
}

pub struct ComputerAuditService<R> {
    repository: R,
}

impl<R: ComputerRepository> ComputerAuditService<R> {
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    pub fn append(
        &self,
        mut request: NewComputerAudit,
    ) -> Result<ComputerAuditReceipt, ComputerAuditServiceError> {
        let computer = self
            .repository
            .computer(&request.owner_profile_id)?
            .ok_or(ComputerAuditServiceError::StaleSubject)?;
        if computer.computer_id != request.computer_id
            || computer.revision != request.expected_computer_revision
        {
            return Err(ComputerAuditServiceError::StaleSubject);
        }
        request.navigation_origin = request
            .navigation_origin
            .as_deref()
            .map(canonical_origin)
            .transpose()?;
        let history = self.repository.audit(&request.owner_profile_id)?;
        if let Some(existing) = history
            .iter()
            .find(|event| event.stable_key == request.stable_key)
        {
            if same_event(existing, &request) {
                return Ok(receipt(existing, true));
            }
            return Err(ComputerAuditServiceError::StableKeyConflict);
        }
        let sequence = u64::try_from(history.len())
            .ok()
            .and_then(|value| value.checked_add(1))
            .ok_or(ComputerAuditServiceError::SequenceOverflow)?;
        let event = ComputerAudit {
            version: CURRENT_SCHEMA_VERSION,
            audit_id: AuditId::new(),
            computer_id: request.computer_id,
            owner_profile_id: request.owner_profile_id,
            sequence,
            stable_key: request.stable_key,
            actor: request.actor,
            kind: request.kind,
            task_key: request.task_key,
            navigation_origin: request.navigation_origin,
            control_transition: request.control_transition,
            policy_decision: request.policy_decision,
            side_effect_summary: request.side_effect_summary,
            transfer: request.transfer,
            safe_failure: request.safe_failure,
            recovery_correlation: request.recovery_correlation,
            safe_summary: request.safe_summary,
            occurred_at: request.occurred_at,
            computer_revision: request.expected_computer_revision,
        };
        event.validate()?;
        self.repository
            .transact(&[ComputerRepositoryBatch::AppendAudit(event.clone())])?;
        Ok(receipt(&event, false))
    }

    pub fn append_typed(
        &self,
        context: ComputerAuditContext,
        event: TypedComputerAuditEvent,
    ) -> Result<ComputerAuditReceipt, ComputerAuditServiceError> {
        self.append(NewComputerAudit::from_typed(context, event)?)
    }

    pub fn history(
        &self,
        owner_profile_id: &ProfileId,
    ) -> Result<Vec<ComputerAudit>, ComputerAuditServiceError> {
        Ok(self.repository.audit(owner_profile_id)?)
    }
}

impl NewComputerAudit {
    pub fn from_typed(
        context: ComputerAuditContext,
        event: TypedComputerAuditEvent,
    ) -> Result<Self, ComputerAuditServiceError> {
        let (
            suffix,
            kind,
            navigation_origin,
            control_transition,
            policy_decision,
            side_effect_summary,
            safe_failure,
            recovery_correlation,
            safe_summary,
        ) = match event {
            TypedComputerAuditEvent::Navigation {
                origin,
                decision,
                safe_summary,
            } => (
                "navigation",
                ComputerAuditKind::PolicyDecision,
                Some(origin),
                None,
                Some(decision),
                None,
                None,
                None,
                safe_summary,
            ),
            TypedComputerAuditEvent::Input {
                transition,
                safe_summary,
            } => (
                "input",
                ComputerAuditKind::TaskChanged,
                None,
                transition,
                None,
                None,
                None,
                None,
                safe_summary,
            ),
            TypedComputerAuditEvent::ControlTransition {
                transition,
                safe_summary,
            } => (
                "control",
                ComputerAuditKind::StateChanged,
                None,
                Some(transition),
                None,
                None,
                None,
                None,
                safe_summary,
            ),
            TypedComputerAuditEvent::Policy {
                decision,
                safe_summary,
            } => (
                "policy",
                ComputerAuditKind::PolicyDecision,
                None,
                None,
                Some(decision),
                None,
                None,
                None,
                safe_summary,
            ),
            TypedComputerAuditEvent::SideEffect {
                effect,
                decision,
                safe_summary,
            } => (
                match effect {
                    ComputerSideEffectKind::ConsequentialAction => "consequential",
                    ComputerSideEffectKind::Upload => "upload",
                    ComputerSideEffectKind::Download => "download",
                },
                ComputerAuditKind::PolicyDecision,
                None,
                None,
                Some(decision),
                Some(safe_summary.clone()),
                None,
                None,
                safe_summary,
            ),
            TypedComputerAuditEvent::Failure {
                safe_failure,
                safe_summary,
            } => (
                "failure",
                ComputerAuditKind::StateChanged,
                None,
                None,
                None,
                None,
                Some(safe_failure),
                None,
                safe_summary,
            ),
            TypedComputerAuditEvent::Recovery {
                correlation_key,
                safe_summary,
            } => (
                "recovery",
                ComputerAuditKind::Recovery,
                None,
                None,
                None,
                None,
                None,
                Some(correlation_key),
                safe_summary,
            ),
        };
        let stable_key =
            StableKey::parse(format!("computer-audit/{}/{suffix}", context.operation_key,))
                .map_err(|_| ComputerAuditServiceError::InvalidEventKey)?;
        Ok(Self {
            stable_key,
            owner_profile_id: context.owner_profile_id,
            computer_id: context.computer_id,
            actor: context.actor,
            kind,
            task_key: context.task_key,
            navigation_origin,
            control_transition,
            policy_decision,
            side_effect_summary,
            transfer: None,
            safe_failure,
            recovery_correlation,
            safe_summary,
            occurred_at: context.occurred_at,
            expected_computer_revision: context.expected_computer_revision,
        })
    }
}

fn receipt(event: &ComputerAudit, duplicate: bool) -> ComputerAuditReceipt {
    ComputerAuditReceipt {
        audit_id: event.audit_id.clone(),
        owner_profile_id: event.owner_profile_id.clone(),
        sequence: event.sequence,
        stable_key: event.stable_key.clone(),
        duplicate,
    }
}

fn same_event(event: &ComputerAudit, request: &NewComputerAudit) -> bool {
    event.computer_id == request.computer_id
        && event.owner_profile_id == request.owner_profile_id
        && event.actor == request.actor
        && event.kind == request.kind
        && event.task_key == request.task_key
        && event.navigation_origin == request.navigation_origin
        && event.control_transition == request.control_transition
        && event.policy_decision == request.policy_decision
        && event.side_effect_summary == request.side_effect_summary
        && event.transfer == request.transfer
        && event.safe_failure == request.safe_failure
        && event.recovery_correlation == request.recovery_correlation
        && event.safe_summary == request.safe_summary
        && event.occurred_at == request.occurred_at
        && event.computer_revision == request.expected_computer_revision
}

fn canonical_origin(value: &str) -> Result<String, ComputerAuditServiceError> {
    let (scheme, remainder) = value
        .split_once("://")
        .ok_or(ComputerAuditServiceError::InvalidOrigin)?;
    if !matches!(scheme, "http" | "https") {
        return Err(ComputerAuditServiceError::InvalidOrigin);
    }
    let authority = remainder
        .split(['/', '?', '#'])
        .next()
        .filter(|authority| {
            !authority.is_empty()
                && authority.len() <= 512
                && !authority.contains('@')
                && !authority.chars().any(char::is_whitespace)
        })
        .ok_or(ComputerAuditServiceError::InvalidOrigin)?;
    Ok(format!("{scheme}://{}", authority.to_ascii_lowercase()))
}
