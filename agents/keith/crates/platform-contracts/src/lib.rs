#![forbid(unsafe_code)]

use std::collections::BTreeSet;
use std::fmt::{self, Display};
use std::str::FromStr;

use keith_agent_types::{
    EntityId, EntityIdError, ProfileId, ProtocolVersion, SessionId, UtcTimestamp,
};
use schemars::JsonSchema;
use serde::{Deserialize, Serialize};
use thiserror::Error;

pub const PLATFORM_CONTRACT_VERSION: ProtocolVersion = ProtocolVersion::new(1, 0);
pub const MAX_SAFE_TEXT_BYTES: usize = 4 * 1_024;
pub const MAX_TRACE_EVENTS: usize = 4_096;

macro_rules! contract_ids {
    ($($name:ident),+ $(,)?) => {
        $(
            #[derive(Clone, Debug, Eq, Hash, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
            #[serde(transparent)]
            pub struct $name(EntityId);

            impl $name {
                pub fn new() -> Self {
                    Self(EntityId::new())
                }

                pub const fn as_entity_id(&self) -> &EntityId {
                    &self.0
                }
            }

            impl Default for $name {
                fn default() -> Self {
                    Self::new()
                }
            }

            impl Display for $name {
                fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
                    Display::fmt(&self.0, formatter)
                }
            }

            impl FromStr for $name {
                type Err = EntityIdError;

                fn from_str(value: &str) -> Result<Self, Self::Err> {
                    EntityId::from_str(value).map(Self)
                }
            }
        )+
    };
}

contract_ids!(
    AcpConnectionId,
    ApprovalId,
    AuditCorrelationId,
    CancellationId,
    ChannelRouteId,
    ComputerSessionId,
    ConnectedAccountId,
    ControlLeaseId,
    DemonstrationId,
    ExternalPrincipalId,
    HarnessCandidateId,
    HarnessExperimentId,
    RecipeId,
);

#[derive(
    Clone, Copy, Debug, Eq, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize,
)]
#[serde(rename_all = "snake_case")]
pub enum PrincipalKind {
    Human,
    ServiceAccount,
    ChannelSender,
    AcpClient,
    PluginPublisher,
    ConnectorUser,
    ComputerObserver,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExternalPrincipal {
    pub id: ExternalPrincipalId,
    pub kind: PrincipalKind,
    pub profile_id: ProfileId,
    pub display_label: RedactedText,
}

#[derive(
    Clone, Copy, Debug, Eq, Hash, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize,
)]
#[serde(rename_all = "snake_case")]
pub enum Capability {
    Read,
    LocalWrite,
    ExternalCommunication,
    Delete,
    Purchase,
    AccountChange,
    CredentialChange,
    ChannelReceive,
    ChannelSend,
    AcpConnect,
    PluginInvoke,
    PluginInstall,
    ConnectedAppInvoke,
    ComputerObserve,
    ComputerControl,
    DemonstrationRecord,
    RecipeEdit,
    RecipePublish,
    HarnessDiagnose,
    HarnessPropose,
    HarnessPromote,
    HarnessReverse,
}

#[derive(
    Clone, Copy, Debug, Eq, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize,
)]
#[serde(rename_all = "snake_case")]
pub enum ActionRisk {
    ReadOnly,
    ReversibleLocalWrite,
    ExternalCommunication,
    Delete,
    Purchase,
    AccountChange,
    CredentialChange,
    IrreversibleComputerInput,
}

impl ActionRisk {
    pub const fn is_consequential(self) -> bool {
        !matches!(self, Self::ReadOnly | Self::ReversibleLocalWrite)
    }
}

#[derive(Clone, Debug, Eq, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CapabilityGrant {
    pub capability: Capability,
    pub resource: RedactedText,
    pub expires_at: Option<UtcTimestamp>,
}

impl CapabilityGrant {
    pub fn is_active_at(&self, now: UtcTimestamp) -> bool {
        self.expires_at.is_none_or(|expiry| expiry > now)
    }
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AuthorityBoundary {
    pub profile_id: ProfileId,
    pub allowed: BTreeSet<CapabilityGrant>,
    pub denied: BTreeSet<Capability>,
    pub max_automatic_risk: ActionRisk,
}

impl AuthorityBoundary {
    /// Computes the least authority shared by two boundaries.
    ///
    /// # Errors
    ///
    /// Returns [`ContractError::ProfileMismatch`] when the boundaries belong to different profiles.
    pub fn intersect(&self, other: &Self) -> Result<Self, ContractError> {
        if self.profile_id != other.profile_id {
            return Err(ContractError::ProfileMismatch);
        }
        let allowed = self.allowed.intersection(&other.allowed).cloned().collect();
        let denied = self.denied.union(&other.denied).copied().collect();
        Ok(Self {
            profile_id: self.profile_id.clone(),
            allowed,
            denied,
            max_automatic_risk: self.max_automatic_risk.min(other.max_automatic_risk),
        })
    }

    /// Checks an action against grants, denials, risk, and exact approval state.
    ///
    /// # Errors
    ///
    /// Returns an authority or approval error when any applicable boundary rejects the action.
    pub fn authorizes(
        &self,
        action: &ExternalAction,
        now: UtcTimestamp,
    ) -> Result<(), ContractError> {
        if self.profile_id != action.profile_id {
            return Err(ContractError::ProfileMismatch);
        }
        if self.denied.contains(&action.requested_capability) {
            return Err(ContractError::CapabilityDenied);
        }
        let granted = self.allowed.iter().any(|grant| {
            grant.capability == action.requested_capability
                && grant.resource == action.target
                && grant.is_active_at(now)
        });
        if !granted {
            return Err(ContractError::CapabilityDenied);
        }
        if action.risk > self.max_automatic_risk || action.risk.is_consequential() {
            action.approval.authorize(action, now)?;
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "state")]
pub enum ApprovalState {
    NotRequired,
    Required,
    Granted {
        approval_id: ApprovalId,
        granted_by: ExternalPrincipalId,
        exact_target_digest: RedactedText,
        expires_at: UtcTimestamp,
    },
    Denied {
        safe_reason: RedactedText,
    },
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ApprovalEnvelope {
    pub risk: ActionRisk,
    pub state: ApprovalState,
}

impl ApprovalEnvelope {
    /// Checks whether this envelope authorizes the exact current action.
    ///
    /// # Errors
    ///
    /// Returns an approval error for mismatched risk or target, denial, absence, or expiry.
    pub fn authorize(
        &self,
        action: &ExternalAction,
        now: UtcTimestamp,
    ) -> Result<(), ContractError> {
        if self.risk != action.risk {
            return Err(ContractError::ApprovalMismatch);
        }
        match &self.state {
            ApprovalState::NotRequired if !action.risk.is_consequential() => Ok(()),
            ApprovalState::Granted {
                exact_target_digest,
                expires_at,
                ..
            } if *expires_at > now && exact_target_digest == &action.target_digest => Ok(()),
            ApprovalState::Denied { .. } => Err(ContractError::ApprovalDenied),
            ApprovalState::NotRequired
            | ApprovalState::Required
            | ApprovalState::Granted { .. } => Err(ContractError::ApprovalRequired),
        }
    }
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExternalAction {
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub acting_principal: ExternalPrincipalId,
    pub requested_capability: Capability,
    pub risk: ActionRisk,
    pub approval: ApprovalEnvelope,
    pub target: RedactedText,
    pub target_digest: RedactedText,
    pub cancellation_id: CancellationId,
    pub reply_route: Option<ChannelRouteId>,
    pub audit_correlation: AuditCorrelationId,
    pub external_effect: ExternalEffect,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum ExternalEffect {
    Repeatable,
    Idempotent { delivery_key: RedactedText },
    NonRepeatable,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AuditEnvelope {
    pub correlation_id: AuditCorrelationId,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub acting_principal: ExternalPrincipalId,
    pub capability: Capability,
    pub risk: ActionRisk,
    pub target_digest: RedactedText,
    pub occurred_at: UtcTimestamp,
    pub outcome: AuditOutcome,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AuditOutcome {
    Requested,
    Approved,
    Denied,
    Completed,
    Failed,
    Cancelled,
    Interrupted,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LifecycleState {
    Pending,
    Active,
    Paused,
    Completed,
    Failed,
    Cancelled,
    Interrupted,
}

impl LifecycleState {
    pub const fn is_terminal(self) -> bool {
        matches!(self, Self::Completed | Self::Failed | Self::Cancelled)
    }

    pub const fn can_transition_to(self, next: Self) -> bool {
        matches!(
            (self, next),
            (Self::Pending, Self::Active | Self::Cancelled | Self::Failed)
                | (
                    Self::Active,
                    Self::Paused
                        | Self::Completed
                        | Self::Failed
                        | Self::Cancelled
                        | Self::Interrupted
                )
                | (
                    Self::Paused,
                    Self::Active | Self::Failed | Self::Cancelled | Self::Interrupted
                )
                | (
                    Self::Interrupted,
                    Self::Pending | Self::Failed | Self::Cancelled
                )
        )
    }

    #[must_use]
    pub fn reconcile_after_restart(self, effect: &ExternalEffect) -> Self {
        if self != Self::Active {
            return self;
        }
        match effect {
            ExternalEffect::Repeatable | ExternalEffect::Idempotent { .. } => Self::Pending,
            ExternalEffect::NonRepeatable => Self::Interrupted,
        }
    }
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct HealthProjection {
    pub state: LifecycleState,
    pub last_transition_at: UtcTimestamp,
    pub restartable: bool,
    pub cancellable: bool,
    pub safe_error: Option<RedactedText>,
}

impl HealthProjection {
    /// Validates that terminal and error projections remain truthful.
    ///
    /// # Errors
    ///
    /// Returns an error when a failure omits its safe error or a completion carries one.
    pub fn validate(&self) -> Result<(), ContractError> {
        match (self.state, self.safe_error.is_some()) {
            (LifecycleState::Failed | LifecycleState::Interrupted, false) => {
                Err(ContractError::MissingSafeError)
            }
            (LifecycleState::Completed, true) => Err(ContractError::UnexpectedSafeError),
            _ => Ok(()),
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TraceEventKind {
    ModelProgress,
    ToolCall,
    Observation,
    UserCorrection,
    Retry,
    CompletionDecision,
    Cost,
    Latency,
    Failure,
    DurableTransition,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TraceEvent {
    pub sequence: u64,
    pub occurred_at: UtcTimestamp,
    pub kind: TraceEventKind,
    pub label: RedactedText,
    pub safe_detail: Option<RedactedText>,
    pub payload_digest: Option<RedactedText>,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExecutionTraceBundle {
    pub contract_version: ProtocolVersion,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub audit_correlation: AuditCorrelationId,
    pub events: Vec<TraceEvent>,
    pub redacted: bool,
}

impl ExecutionTraceBundle {
    /// Validates contract compatibility, bounds, ordering, and redaction state.
    ///
    /// # Errors
    ///
    /// Returns an error for an incompatible version, oversized or unordered trace, or missing redaction.
    pub fn validate(&self) -> Result<(), ContractError> {
        negotiate_contract(self.contract_version)?;
        if self.events.len() > MAX_TRACE_EVENTS {
            return Err(ContractError::TraceEventLimit);
        }
        if self
            .events
            .windows(2)
            .any(|window| window[0].sequence >= window[1].sequence)
        {
            return Err(ContractError::TraceOrder);
        }
        if !self.redacted {
            return Err(ContractError::TraceNotRedacted);
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(transparent)]
pub struct RedactedText(String);

impl RedactedText {
    /// Creates bounded text that rejects control characters and common credential markers.
    ///
    /// # Errors
    ///
    /// Returns [`ContractError::UnsafeText`] when the value is empty, oversized, or unsafe.
    pub fn parse(value: impl Into<String>) -> Result<Self, ContractError> {
        let value = value.into();
        let trimmed = value.trim();
        if trimmed.is_empty() || value.len() > MAX_SAFE_TEXT_BYTES {
            return Err(ContractError::UnsafeText);
        }
        let normalized = value.to_ascii_lowercase();
        let secret_marker = [
            "authorization: bearer",
            "access_token",
            "refresh_token",
            "api_key",
            "password=",
            "secret=",
            "sk-",
        ]
        .iter()
        .any(|marker| normalized.contains(marker));
        let invalid_control = value
            .chars()
            .any(|character| character.is_control() && !matches!(character, '\n' | '\r' | '\t'));
        if secret_marker || invalid_control {
            Err(ContractError::UnsafeText)
        } else {
            Ok(Self(value))
        }
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl Display for RedactedText {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl FromStr for RedactedText {
    type Err = ContractError;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        Self::parse(value)
    }
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ControlOwner {
    KeithControl,
    UserControl,
    Paused,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ControlLease {
    pub id: ControlLeaseId,
    pub computer_session_id: ComputerSessionId,
    pub profile_id: ProfileId,
    pub owner: ControlOwner,
    pub holder: Option<ExternalPrincipalId>,
    pub revision: u64,
    pub issued_at: UtcTimestamp,
    pub expires_at: UtcTimestamp,
}

impl ControlLease {
    /// Validates exclusive-holder and time-range invariants.
    ///
    /// # Errors
    ///
    /// Returns [`ContractError::InvalidControlLease`] when the lease is ambiguous or expired at issue.
    pub fn validate(&self) -> Result<(), ContractError> {
        let holder_required = !matches!(self.owner, ControlOwner::Paused);
        if self.expires_at <= self.issued_at || holder_required != self.holder.is_some() {
            return Err(ContractError::InvalidControlLease);
        }
        Ok(())
    }

    pub fn can_inject(&self, principal: &ExternalPrincipalId, now: UtcTimestamp) -> bool {
        now < self.expires_at
            && !matches!(self.owner, ControlOwner::Paused)
            && self.holder.as_ref() == Some(principal)
    }

    /// Drops an expired lease during restart reconciliation so no stale holder can inject input.
    #[must_use]
    pub fn reconcile_after_restart(&self, now: UtcTimestamp) -> Option<Self> {
        (now < self.expires_at).then(|| self.clone())
    }

    /// Replaces the lease under optimistic revision control.
    ///
    /// # Errors
    ///
    /// Returns an error for stale or exhausted revisions or invalid replacement ownership.
    pub fn transfer(
        &self,
        expected_revision: u64,
        owner: ControlOwner,
        holder: Option<ExternalPrincipalId>,
        issued_at: UtcTimestamp,
        expires_at: UtcTimestamp,
    ) -> Result<Self, ContractError> {
        if self.revision != expected_revision {
            return Err(ContractError::StaleControlLease);
        }
        let next_revision = self
            .revision
            .checked_add(1)
            .ok_or(ContractError::RevisionExhausted)?;
        let next = Self {
            id: ControlLeaseId::new(),
            computer_session_id: self.computer_session_id.clone(),
            profile_id: self.profile_id.clone(),
            owner,
            holder,
            revision: next_revision,
            issued_at,
            expires_at,
        };
        next.validate()?;
        Ok(next)
    }
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ResourceBounds {
    pub max_concurrency: u32,
    pub max_duration_ms: u64,
    pub max_cpu_time_ms: u64,
    pub max_retries: u32,
    pub max_input_bytes: u64,
    pub max_output_bytes: u64,
    pub max_memory_bytes: u64,
    pub max_disk_bytes: u64,
    pub max_events_per_minute: u32,
}

impl ResourceBounds {
    /// Validates that every operative ceiling is positive.
    ///
    /// # Errors
    ///
    /// Returns [`ContractError::InvalidResourceBounds`] for a disabled operative ceiling.
    pub fn validate(&self) -> Result<(), ContractError> {
        let valid = self.max_concurrency > 0
            && self.max_duration_ms > 0
            && self.max_cpu_time_ms > 0
            && self.max_input_bytes > 0
            && self.max_output_bytes > 0
            && self.max_memory_bytes > 0
            && self.max_disk_bytes > 0
            && self.max_events_per_minute > 0;
        if valid {
            Ok(())
        } else {
            Err(ContractError::InvalidResourceBounds)
        }
    }
}

pub trait PlatformCapabilityProvider {
    /// Resolves the effective boundary for a profile.
    ///
    /// # Errors
    ///
    /// Returns an implementation-defined contract error when authority cannot be resolved safely.
    fn authority(&self, profile_id: &ProfileId) -> Result<AuthorityBoundary, ContractError>;
}

pub trait AuditedActionSink {
    type Error;

    /// Submits an action carrying its approval for atomic policy enforcement and auditing.
    ///
    /// # Errors
    ///
    /// Returns the implementation error when admission or persistence fails.
    fn submit(&self, action: ExternalAction) -> Result<(), Self::Error>;
}

pub trait PlatformTraceSink {
    type Error;

    /// Records a validated redacted trace bundle.
    ///
    /// # Errors
    ///
    /// Returns the implementation error when validation or persistence fails.
    fn record(&self, bundle: ExecutionTraceBundle) -> Result<(), Self::Error>;
}

pub trait LifecycleProjectionProvider {
    type Subject;
    type Error;

    /// Projects truthful current health for one subject.
    ///
    /// # Errors
    ///
    /// Returns the implementation error when state cannot be read or projected.
    fn health(&self, subject: &Self::Subject) -> Result<HealthProjection, Self::Error>;
    /// Requests cancellation using the action's stable cancellation identity.
    ///
    /// # Errors
    ///
    /// Returns the implementation error when cancellation cannot be admitted or persisted.
    fn cancel(
        &self,
        subject: &Self::Subject,
        cancellation: &CancellationId,
    ) -> Result<bool, Self::Error>;
}

/// Negotiates the current platform contract with a peer.
///
/// # Errors
///
/// Returns [`ContractError::IncompatibleVersion`] for a different major version.
pub fn negotiate_contract(peer: ProtocolVersion) -> Result<ProtocolVersion, ContractError> {
    PLATFORM_CONTRACT_VERSION
        .common_minor(peer)
        .ok_or(ContractError::IncompatibleVersion)
}

#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum ContractError {
    #[error("platform contract version is incompatible")]
    IncompatibleVersion,
    #[error("profile boundary does not match")]
    ProfileMismatch,
    #[error("requested capability is denied or not granted")]
    CapabilityDenied,
    #[error("approval does not match the action")]
    ApprovalMismatch,
    #[error("the action requires an exact unexpired approval")]
    ApprovalRequired,
    #[error("the action was denied")]
    ApprovalDenied,
    #[error("safe text is empty, oversized, contains controls, or resembles a secret")]
    UnsafeText,
    #[error("trace exceeds the event bound")]
    TraceEventLimit,
    #[error("trace events are not strictly ordered")]
    TraceOrder,
    #[error("trace is not marked redacted")]
    TraceNotRedacted,
    #[error("failed or interrupted health must include a safe error")]
    MissingSafeError,
    #[error("completed health cannot include an error")]
    UnexpectedSafeError,
    #[error("control lease owner, holder, or time range is invalid")]
    InvalidControlLease,
    #[error("control lease revision is stale")]
    StaleControlLease,
    #[error("control lease revision is exhausted")]
    RevisionExhausted,
    #[error("resource bounds must be positive")]
    InvalidResourceBounds,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn text(value: &str) -> RedactedText {
        RedactedText::parse(value).expect("safe text")
    }

    fn principal() -> ExternalPrincipalId {
        ExternalPrincipalId::new()
    }

    fn action(profile_id: &ProfileId, capability: Capability, risk: ActionRisk) -> ExternalAction {
        ExternalAction {
            profile_id: profile_id.clone(),
            session_id: SessionId::new(),
            acting_principal: principal(),
            requested_capability: capability,
            risk,
            approval: ApprovalEnvelope {
                risk,
                state: ApprovalState::NotRequired,
            },
            target: text("account:primary"),
            target_digest: text("sha256:target"),
            cancellation_id: CancellationId::new(),
            reply_route: Some(ChannelRouteId::new()),
            audit_correlation: AuditCorrelationId::new(),
            external_effect: ExternalEffect::NonRepeatable,
        }
    }

    fn boundary(
        profile_id: &ProfileId,
        allowed: &[Capability],
        max_risk: ActionRisk,
    ) -> AuthorityBoundary {
        AuthorityBoundary {
            profile_id: profile_id.clone(),
            allowed: allowed
                .iter()
                .copied()
                .map(|capability| CapabilityGrant {
                    capability,
                    resource: text("account:primary"),
                    expires_at: None,
                })
                .collect(),
            denied: BTreeSet::new(),
            max_automatic_risk: max_risk,
        }
    }

    #[test]
    fn contract_version_negotiates_only_within_the_major_version() {
        assert_eq!(
            negotiate_contract(ProtocolVersion::new(1, 7)),
            Ok(PLATFORM_CONTRACT_VERSION)
        );
        assert_eq!(
            negotiate_contract(ProtocolVersion::new(2, 0)),
            Err(ContractError::IncompatibleVersion)
        );
    }

    #[test]
    fn authority_intersection_never_widens_and_denial_wins() {
        let profile = ProfileId::new();
        let mut outer = boundary(
            &profile,
            &[Capability::Read, Capability::ExternalCommunication],
            ActionRisk::ExternalCommunication,
        );
        outer.denied.insert(Capability::Delete);
        let inner = boundary(
            &profile,
            &[Capability::Read, Capability::Delete],
            ActionRisk::ReversibleLocalWrite,
        );
        let combined = outer.intersect(&inner).expect("matching profile");
        assert_eq!(combined.allowed.len(), 1);
        assert!(
            combined
                .allowed
                .iter()
                .any(|grant| grant.capability == Capability::Read)
        );
        assert!(combined.denied.contains(&Capability::Delete));
        assert_eq!(
            combined.max_automatic_risk,
            ActionRisk::ReversibleLocalWrite
        );
    }

    #[test]
    fn consequential_action_requires_exact_unexpired_approval() {
        let profile = ProfileId::new();
        let boundary = boundary(
            &profile,
            &[Capability::ExternalCommunication],
            ActionRisk::ReversibleLocalWrite,
        );
        let mut action = action(
            &profile,
            Capability::ExternalCommunication,
            ActionRisk::ExternalCommunication,
        );
        action.approval = ApprovalEnvelope {
            risk: action.risk,
            state: ApprovalState::Required,
        };
        assert_eq!(
            boundary.authorizes(&action, UtcTimestamp::from_unix_millis(10)),
            Err(ContractError::ApprovalRequired)
        );
        action.approval = ApprovalEnvelope {
            risk: action.risk,
            state: ApprovalState::Granted {
                approval_id: ApprovalId::new(),
                granted_by: principal(),
                exact_target_digest: action.target_digest.clone(),
                expires_at: UtcTimestamp::from_unix_millis(20),
            },
        };
        assert_eq!(
            boundary.authorizes(&action, UtcTimestamp::from_unix_millis(10)),
            Ok(())
        );
        let stale_target = ExternalAction {
            target_digest: text("sha256:changed"),
            ..action
        };
        assert_eq!(
            boundary.authorizes(&stale_target, UtcTimestamp::from_unix_millis(10)),
            Err(ContractError::ApprovalRequired)
        );
    }

    #[test]
    fn profile_mismatch_and_unknown_serialized_capability_fail_closed() {
        let first = boundary(&ProfileId::new(), &[Capability::Read], ActionRisk::ReadOnly);
        let second = boundary(&ProfileId::new(), &[Capability::Read], ActionRisk::ReadOnly);
        assert_eq!(
            first.intersect(&second),
            Err(ContractError::ProfileMismatch)
        );
        let json = r#"{"profile_id":"01ARZ3NDEKTSV4RRFFQ69G5FAV","allowed":[],"denied":["future_unknown"],"max_automatic_risk":"read_only"}"#;
        assert!(serde_json::from_str::<AuthorityBoundary>(json).is_err());
    }

    #[test]
    fn action_round_trip_preserves_cancellation_identity_and_approval() {
        let profile = ProfileId::new();
        let action = action(
            &profile,
            Capability::LocalWrite,
            ActionRisk::ReversibleLocalWrite,
        );
        let expected_cancellation = action.cancellation_id.clone();
        let encoded = serde_json::to_string(&action).expect("serialize external action");
        let decoded: ExternalAction =
            serde_json::from_str(&encoded).expect("deserialize external action");
        assert_eq!(decoded, action);
        assert_eq!(decoded.cancellation_id, expected_cancellation);
        assert_eq!(decoded.approval.state, ApprovalState::NotRequired);
    }

    #[test]
    fn trace_requires_order_bounds_redaction_and_safe_text() {
        assert!(RedactedText::parse("Authorization: Bearer abc").is_err());
        let mut bundle = ExecutionTraceBundle {
            contract_version: PLATFORM_CONTRACT_VERSION,
            profile_id: ProfileId::new(),
            session_id: SessionId::new(),
            audit_correlation: AuditCorrelationId::new(),
            events: vec![
                TraceEvent {
                    sequence: 1,
                    occurred_at: UtcTimestamp::UNIX_EPOCH,
                    kind: TraceEventKind::ToolCall,
                    label: text("tool invoked"),
                    safe_detail: None,
                    payload_digest: Some(text("sha256:event-one")),
                },
                TraceEvent {
                    sequence: 2,
                    occurred_at: UtcTimestamp::from_unix_millis(1),
                    kind: TraceEventKind::CompletionDecision,
                    label: text("completion accepted"),
                    safe_detail: None,
                    payload_digest: Some(text("sha256:event-two")),
                },
            ],
            redacted: true,
        };
        assert_eq!(bundle.validate(), Ok(()));
        bundle.events[1].sequence = 1;
        assert_eq!(bundle.validate(), Err(ContractError::TraceOrder));
        bundle.events[1].sequence = 2;
        bundle.redacted = false;
        assert_eq!(bundle.validate(), Err(ContractError::TraceNotRedacted));
    }

    #[test]
    fn restart_never_blindly_replays_non_repeatable_effects() {
        assert_eq!(
            LifecycleState::Active.reconcile_after_restart(&ExternalEffect::NonRepeatable),
            LifecycleState::Interrupted
        );
        assert_eq!(
            LifecycleState::Active.reconcile_after_restart(&ExternalEffect::Idempotent {
                delivery_key: text("delivery:one")
            }),
            LifecycleState::Pending
        );
        assert!(!LifecycleState::Completed.can_transition_to(LifecycleState::Active));
    }

    #[test]
    fn control_lease_is_exclusive_revisioned_and_pause_has_no_holder() {
        let user = principal();
        let lease = ControlLease {
            id: ControlLeaseId::new(),
            computer_session_id: ComputerSessionId::new(),
            profile_id: ProfileId::new(),
            owner: ControlOwner::UserControl,
            holder: Some(user.clone()),
            revision: 4,
            issued_at: UtcTimestamp::from_unix_millis(10),
            expires_at: UtcTimestamp::from_unix_millis(20),
        };
        assert_eq!(lease.validate(), Ok(()));
        assert!(lease.can_inject(&user, UtcTimestamp::from_unix_millis(15)));
        assert_eq!(
            lease.transfer(
                3,
                ControlOwner::Paused,
                None,
                UtcTimestamp::from_unix_millis(16),
                UtcTimestamp::from_unix_millis(30)
            ),
            Err(ContractError::StaleControlLease)
        );
        let paused = lease
            .transfer(
                4,
                ControlOwner::Paused,
                None,
                UtcTimestamp::from_unix_millis(16),
                UtcTimestamp::from_unix_millis(30),
            )
            .expect("current revision");
        assert!(!paused.can_inject(&user, UtcTimestamp::from_unix_millis(17)));
        assert_eq!(
            lease.reconcile_after_restart(UtcTimestamp::from_unix_millis(20)),
            None
        );
    }

    #[test]
    fn resource_and_health_contracts_reject_fabricated_validity() {
        let bounds = ResourceBounds {
            max_concurrency: 0,
            max_duration_ms: 1,
            max_cpu_time_ms: 1,
            max_retries: 0,
            max_input_bytes: 1,
            max_output_bytes: 1,
            max_memory_bytes: 1,
            max_disk_bytes: 1,
            max_events_per_minute: 1,
        };
        assert_eq!(bounds.validate(), Err(ContractError::InvalidResourceBounds));
        let failed = HealthProjection {
            state: LifecycleState::Failed,
            last_transition_at: UtcTimestamp::UNIX_EPOCH,
            restartable: true,
            cancellable: false,
            safe_error: None,
        };
        assert_eq!(failed.validate(), Err(ContractError::MissingSafeError));
    }
}
