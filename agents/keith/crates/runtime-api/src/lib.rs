#![forbid(unsafe_code)]

use std::collections::BTreeSet;

use keith_agent_types::{
    ActionId, ArtifactId, ClientId, CommandId, EntityId, EntryId, Generation, MessageId, ProfileId,
    Revision, RootTreeId, SessionId, ToolCallId, TurnId, UtcTimestamp,
};
use keith_platform_contracts::{
    AuditCorrelationId, CancellationId, ExternalAction, ExternalEffect, LifecycleState,
    RedactedText, ResourceBounds,
};
use keith_protocol::{
    ClientCommand, CommandResult, CreateSession, ModelSelection, ProfileSummary, SessionSnapshot,
    SessionState, SubmitPrompt,
};
use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ExternalServiceKind {
    ChannelAccount,
    AcpConnection,
    Plugin,
    ConnectedApp,
    ComputerSession,
    ControlLease,
    Recording,
    Recipe,
    HarnessRepair,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "state")]
pub enum ServiceAvailability {
    Available,
    Unavailable { safe_reason: RedactedText },
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ServiceControl {
    Restart,
    Cancel,
    Export,
    Delete,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ServiceRegistration {
    pub id: EntityId,
    pub profile_id: ProfileId,
    pub owning_session_id: Option<SessionId>,
    pub service: ExternalServiceKind,
    pub native_resource_key: String,
    pub display_label: RedactedText,
    pub availability: ServiceAvailability,
    pub lifecycle: LifecycleState,
    pub effect: ExternalEffect,
    pub cancellation_id: CancellationId,
    pub audit_correlation: AuditCorrelationId,
    pub bounds: ResourceBounds,
    pub controls: BTreeSet<ServiceControl>,
    pub safe_error: Option<RedactedText>,
    pub revision: Revision,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
}

impl ServiceRegistration {
    /// Validates resource identity, bounds, lifecycle truth, and unavailable-state honesty.
    ///
    /// # Errors
    ///
    /// Returns a safe error when the registration cannot be persisted or projected truthfully.
    pub fn validate(&self) -> Result<(), String> {
        if self.native_resource_key.is_empty()
            || self.native_resource_key.len() > 256
            || self.native_resource_key.chars().any(char::is_control)
        {
            return Err("service resource key is invalid".into());
        }
        self.bounds.validate().map_err(|error| error.to_string())?;
        if matches!(self.availability, ServiceAvailability::Unavailable { .. })
            && matches!(self.lifecycle, LifecycleState::Active)
        {
            return Err("unavailable service cannot report an active lifecycle".into());
        }
        if matches!(
            self.lifecycle,
            LifecycleState::Failed | LifecycleState::Interrupted
        ) != self.safe_error.is_some()
        {
            return Err("service failure and safe error do not agree".into());
        }
        if self.lifecycle.is_terminal() && self.controls.contains(&ServiceControl::Cancel) {
            return Err("terminal service cannot report cancellation availability".into());
        }
        Ok(())
    }

    /// Reconciles an in-flight registration without replaying an uncertain external effect.
    #[must_use]
    pub fn reconcile_after_restart(mut self, now: UtcTimestamp) -> Self {
        let lifecycle = self.lifecycle.reconcile_after_restart(&self.effect);
        if lifecycle != self.lifecycle {
            self.lifecycle = lifecycle;
            self.updated_at = now;
            self.controls.remove(&ServiceControl::Cancel);
            if matches!(lifecycle, LifecycleState::Pending) {
                self.controls.insert(ServiceControl::Cancel);
            }
            self.safe_error = matches!(lifecycle, LifecycleState::Interrupted)
                .then(|| RedactedText::parse("external outcome requires operator review").ok())
                .flatten();
        }
        self
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ActiveServiceOperation {
    pub id: EntityId,
    pub registration_id: EntityId,
    pub profile_id: ProfileId,
    pub action: ExternalAction,
    pub idempotency_key: String,
    pub lifecycle: LifecycleState,
    pub attempt: u32,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub safe_error: Option<RedactedText>,
}

impl ActiveServiceOperation {
    /// Validates profile isolation and truthful terminal/error state.
    ///
    /// # Errors
    ///
    /// Returns an error when the operation widens profile authority or fabricates health.
    pub fn validate(&self) -> Result<(), String> {
        if self.profile_id != self.action.profile_id {
            return Err("service operation profile does not match action authority".into());
        }
        if self.idempotency_key.is_empty()
            || self.idempotency_key.len() > 256
            || self.idempotency_key.chars().any(char::is_control)
        {
            return Err("service operation idempotency key is invalid".into());
        }
        if matches!(
            self.lifecycle,
            LifecycleState::Failed | LifecycleState::Interrupted
        ) != self.safe_error.is_some()
        {
            return Err("service operation failure and safe error do not agree".into());
        }
        Ok(())
    }

    #[must_use]
    pub fn reconcile_after_restart(mut self, now: UtcTimestamp) -> Self {
        let lifecycle = self
            .lifecycle
            .reconcile_after_restart(&self.action.external_effect);
        if lifecycle != self.lifecycle {
            self.lifecycle = lifecycle;
            self.updated_at = now;
            self.safe_error = matches!(lifecycle, LifecycleState::Interrupted)
                .then(|| RedactedText::parse("external outcome requires operator review").ok())
                .flatten();
        }
        self
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RuntimeSession {
    pub session_id: SessionId,
    pub root_tree_id: RootTreeId,
    pub profile_id: ProfileId,
    pub title: Option<String>,
    pub archived: bool,
    pub created_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcceptedPrompt {
    pub acceptance_id: CommandId,
    pub action_id: ActionId,
    pub turn_id: TurnId,
    pub prompt: SubmitPrompt,
    pub accepted_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CandidateCanaryRequest {
    pub corpus_version: u32,
    pub corpus_sha256: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CandidateCanaryOutcome {
    Completed,
    ToolUse,
    Rejected,
    Failed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CandidateCanaryVerdict {
    Improved,
    Equivalent,
    Regressed,
    Inconclusive,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CandidateCanaryMeasurement {
    pub journey_id: String,
    pub outcome: CandidateCanaryOutcome,
    pub output_sha256: String,
    pub tokens: u64,
    pub latency_ms: u64,
    pub operations: u64,
    pub verdict: CandidateCanaryVerdict,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CandidateCanaryReport {
    pub corpus_version: u32,
    pub corpus_sha256: String,
    pub measurements: Vec<CandidateCanaryMeasurement>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "operation", content = "parameters")]
pub enum RuntimeRequest {
    Profiles,
    Sessions,
    CreateDefaultSession {
        session_id: SessionId,
        root_tree_id: RootTreeId,
        title: Option<String>,
    },
    CreateSession {
        session_id: SessionId,
        root_tree_id: RootTreeId,
        request: CreateSession,
    },
    ForkSession {
        source_session_id: SessionId,
        session_id: SessionId,
        root_tree_id: RootTreeId,
        title: Option<String>,
        generation: Generation,
    },
    SelectModel(ModelSelection),
    RunPrompt {
        prompt: SubmitPrompt,
        generation: Generation,
    },
    RunAcceptedPrompt {
        accepted: AcceptedPrompt,
        generation: Generation,
    },
    Snapshot {
        session_id: SessionId,
        generation: Generation,
        state: SessionState,
    },
    ExecuteFeature {
        client_id: ClientId,
        scope_session_id: Option<SessionId>,
        command: ClientCommand,
        generation: Generation,
    },
    Maintain,
    CandidateCanary(CandidateCanaryRequest),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "result", content = "value")]
pub enum RuntimeResponse {
    Profiles(Vec<ProfileSummary>),
    Sessions(Vec<RuntimeSession>),
    Session(RuntimeSession),
    Snapshot(Box<SessionSnapshot>),
    Command(Box<CommandResult>),
    Complete,
    CandidateCanary(CandidateCanaryReport),
    Failed(String),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RuntimeAgentOutcome {
    Completed,
    Cancelled,
    Exhausted,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "event", content = "payload")]
pub enum RuntimeEventKind {
    AgentStarted,
    TurnStarted {
        number: u32,
    },
    AssistantStarted {
        message_id: MessageId,
    },
    AssistantDelta {
        message_id: MessageId,
        text: String,
    },
    AssistantCompleted {
        message_id: MessageId,
        complete: bool,
    },
    AssistantFinalCommitted {
        message_id: MessageId,
        final_id: EntryId,
        text: String,
    },
    ToolStarted {
        call_id: ToolCallId,
        name: String,
    },
    ToolCompleted {
        call_id: ToolCallId,
        name: String,
        is_error: bool,
        artifact_id: Option<ArtifactId>,
    },
    StrategyChanged {
        reason: String,
    },
    TurnEnded,
    AgentEnded {
        outcome: RuntimeAgentOutcome,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RuntimeEvent {
    pub session_id: SessionId,
    pub turn_id: TurnId,
    pub sequence: u64,
    pub kind: RuntimeEventKind,
}

pub trait RuntimeEventSink: Send {
    fn emit(&mut self, event: RuntimeEvent);
}

impl<F> RuntimeEventSink for F
where
    F: FnMut(RuntimeEvent) + Send,
{
    fn emit(&mut self, event: RuntimeEvent) {
        self(event);
    }
}

#[derive(Default)]
pub struct NoRuntimeEvents;

impl RuntimeEventSink for NoRuntimeEvents {
    fn emit(&mut self, _event: RuntimeEvent) {}
}

impl RuntimeRequest {
    pub fn execute(&self, runtime: &dyn CommandRuntime) -> RuntimeResponse {
        self.execute_with_events(runtime, &mut NoRuntimeEvents)
    }

    pub fn execute_with_events(
        &self,
        runtime: &dyn CommandRuntime,
        events: &mut dyn RuntimeEventSink,
    ) -> RuntimeResponse {
        let response = match self {
            Self::Profiles => runtime.profiles().map(RuntimeResponse::Profiles),
            Self::Sessions => runtime.sessions().map(RuntimeResponse::Sessions),
            Self::CreateDefaultSession {
                session_id,
                root_tree_id,
                title,
            } => runtime
                .create_default_session_assigned(session_id, root_tree_id, title.clone())
                .map(RuntimeResponse::Session),
            Self::CreateSession {
                session_id,
                root_tree_id,
                request,
            } => runtime
                .create_session_assigned(session_id, root_tree_id, request)
                .map(RuntimeResponse::Session),
            Self::ForkSession {
                source_session_id,
                session_id,
                root_tree_id,
                title,
                generation,
            } => {
                if source_session_id == session_id {
                    Err("fork source and destination sessions must differ".into())
                } else if !valid_optional_title(title.as_deref()) {
                    Err("fork title is empty, oversized, or contains controls".into())
                } else {
                    runtime
                        .fork_session_assigned(
                            source_session_id,
                            session_id,
                            root_tree_id,
                            title.clone(),
                            *generation,
                        )
                        .map(RuntimeResponse::Session)
                }
            }
            Self::SelectModel(selection) => runtime
                .select_model(selection)
                .map(|()| RuntimeResponse::Complete),
            Self::RunPrompt { prompt, generation } => runtime
                .run_prompt_streaming(prompt, *generation, events)
                .map(|snapshot| RuntimeResponse::Snapshot(Box::new(snapshot))),
            Self::RunAcceptedPrompt {
                accepted,
                generation,
            } => runtime
                .run_accepted_prompt_streaming(accepted, *generation, events)
                .map(|snapshot| RuntimeResponse::Snapshot(Box::new(snapshot))),
            Self::Snapshot {
                session_id,
                generation,
                state,
            } => runtime
                .snapshot(session_id, *generation, *state)
                .map(|snapshot| RuntimeResponse::Snapshot(Box::new(snapshot))),
            Self::ExecuteFeature {
                client_id,
                scope_session_id,
                command,
                generation,
            } => runtime
                .execute_feature(client_id, scope_session_id.as_ref(), command, *generation)
                .map(|result| RuntimeResponse::Command(Box::new(result))),
            Self::Maintain => runtime.maintain().map(|()| RuntimeResponse::Complete),
            Self::CandidateCanary(request) => runtime
                .candidate_canary(request)
                .map(RuntimeResponse::CandidateCanary),
        };
        response.unwrap_or_else(RuntimeResponse::Failed)
    }
}

fn valid_optional_title(title: Option<&str>) -> bool {
    title.is_none_or(|title| {
        !title.trim().is_empty() && title.len() <= 512 && !title.chars().any(char::is_control)
    })
}

#[allow(clippy::missing_errors_doc)]
pub trait CommandRuntime: Send + Sync {
    fn profiles(&self) -> Result<Vec<ProfileSummary>, String>;
    fn sessions(&self) -> Result<Vec<RuntimeSession>, String>;
    fn create_default_session(&self, title: Option<String>) -> Result<RuntimeSession, String>;
    fn create_session(&self, request: &CreateSession) -> Result<RuntimeSession, String>;
    fn create_default_session_assigned(
        &self,
        session_id: &SessionId,
        root_tree_id: &RootTreeId,
        title: Option<String>,
    ) -> Result<RuntimeSession, String>;
    fn create_session_assigned(
        &self,
        session_id: &SessionId,
        root_tree_id: &RootTreeId,
        request: &CreateSession,
    ) -> Result<RuntimeSession, String>;
    fn fork_session_assigned(
        &self,
        source_session_id: &SessionId,
        session_id: &SessionId,
        root_tree_id: &RootTreeId,
        title: Option<String>,
        generation: Generation,
    ) -> Result<RuntimeSession, String>;
    fn select_model(&self, selection: &ModelSelection) -> Result<(), String>;
    fn run_prompt(
        &self,
        prompt: &SubmitPrompt,
        generation: Generation,
    ) -> Result<SessionSnapshot, String>;
    fn run_prompt_streaming(
        &self,
        prompt: &SubmitPrompt,
        generation: Generation,
        events: &mut dyn RuntimeEventSink,
    ) -> Result<SessionSnapshot, String> {
        let _ = events;
        self.run_prompt(prompt, generation)
    }
    fn run_accepted_prompt_streaming(
        &self,
        accepted: &AcceptedPrompt,
        generation: Generation,
        events: &mut dyn RuntimeEventSink,
    ) -> Result<SessionSnapshot, String> {
        self.run_prompt_streaming(&accepted.prompt, generation, events)
    }
    fn cancel_active(&self, session_id: &SessionId) -> Result<bool, String>;
    fn snapshot(
        &self,
        session_id: &SessionId,
        generation: Generation,
        state: SessionState,
    ) -> Result<SessionSnapshot, String>;
    fn execute_feature(
        &self,
        client_id: &ClientId,
        scope_session_id: Option<&SessionId>,
        command: &ClientCommand,
        generation: Generation,
    ) -> Result<CommandResult, String>;
    fn maintain(&self) -> Result<(), String>;
    fn candidate_canary(
        &self,
        _request: &CandidateCanaryRequest,
    ) -> Result<CandidateCanaryReport, String> {
        Err("candidate canary evaluation is unavailable".into())
    }
}

#[cfg(test)]
mod tests {
    use keith_platform_contracts::{
        ActionRisk, ApprovalEnvelope, ApprovalState, Capability, ExternalPrincipalId,
    };

    use super::*;

    fn bounds() -> ResourceBounds {
        ResourceBounds {
            max_concurrency: 1,
            max_duration_ms: 1,
            max_cpu_time_ms: 1,
            max_retries: 0,
            max_input_bytes: 1,
            max_output_bytes: 1,
            max_memory_bytes: 1,
            max_disk_bytes: 1,
            max_events_per_minute: 1,
        }
    }

    fn action(profile_id: ProfileId, effect: ExternalEffect) -> ExternalAction {
        ExternalAction {
            profile_id,
            session_id: SessionId::new(),
            acting_principal: ExternalPrincipalId::new(),
            requested_capability: Capability::LocalWrite,
            risk: ActionRisk::ReversibleLocalWrite,
            approval: ApprovalEnvelope {
                risk: ActionRisk::ReversibleLocalWrite,
                state: ApprovalState::NotRequired,
            },
            target: RedactedText::parse("resource").unwrap(),
            target_digest: RedactedText::parse("digest").unwrap(),
            cancellation_id: CancellationId::new(),
            reply_route: None,
            audit_correlation: AuditCorrelationId::new(),
            external_effect: effect,
        }
    }

    #[test]
    fn restart_reconciliation_never_replays_uncertain_external_effects() {
        let profile_id = ProfileId::new();
        let now = UtcTimestamp::from_unix_millis(10);
        let registration = ServiceRegistration {
            id: EntityId::new(),
            profile_id: profile_id.clone(),
            owning_session_id: None,
            service: ExternalServiceKind::ConnectedApp,
            native_resource_key: "resource".into(),
            display_label: RedactedText::parse("Resource").unwrap(),
            availability: ServiceAvailability::Available,
            lifecycle: LifecycleState::Active,
            effect: ExternalEffect::NonRepeatable,
            cancellation_id: CancellationId::new(),
            audit_correlation: AuditCorrelationId::new(),
            bounds: bounds(),
            controls: [
                ServiceControl::Cancel,
                ServiceControl::Export,
                ServiceControl::Delete,
            ]
            .into_iter()
            .collect(),
            safe_error: None,
            revision: Revision::ZERO,
            created_at: UtcTimestamp::UNIX_EPOCH,
            updated_at: UtcTimestamp::UNIX_EPOCH,
        };
        let interrupted = registration.reconcile_after_restart(now);
        assert_eq!(interrupted.lifecycle, LifecycleState::Interrupted);
        assert!(interrupted.safe_error.is_some());
        assert!(!interrupted.controls.contains(&ServiceControl::Cancel));
        interrupted.validate().unwrap();

        let operation = ActiveServiceOperation {
            id: EntityId::new(),
            registration_id: interrupted.id,
            profile_id: profile_id.clone(),
            action: action(profile_id.clone(), ExternalEffect::NonRepeatable),
            idempotency_key: "operation".into(),
            lifecycle: LifecycleState::Active,
            attempt: 1,
            created_at: UtcTimestamp::UNIX_EPOCH,
            updated_at: UtcTimestamp::UNIX_EPOCH,
            safe_error: None,
        }
        .reconcile_after_restart(now);
        assert_eq!(operation.lifecycle, LifecycleState::Interrupted);
        assert!(operation.safe_error.is_some());
        operation.validate().unwrap();

        let mut cross_profile = operation;
        cross_profile.profile_id = ProfileId::new();
        assert!(cross_profile.validate().is_err());
    }
}
