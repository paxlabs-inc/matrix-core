#![forbid(unsafe_code)]

use keith_agent_types::{
    ActionId, ArtifactId, ClientId, CommandId, ConversationId, EntityId, EntryId, Generation,
    MessageId, ProfileId, RootTreeId, SessionId, ToolCallId, TurnId, UtcTimestamp, WorkerId,
};
use keith_protocol::{
    ClientCommand, CommandResult, CreateSession, ModelSelection, ProfileSummary, SessionSnapshot,
    SessionState, SubmitPrompt,
};
use serde::{Deserialize, Serialize};

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
pub struct ConversationSessionAssignment {
    pub profile_id: ProfileId,
    pub root_tree_id: RootTreeId,
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
#[serde(rename_all = "snake_case", tag = "authority")]
pub enum RuntimeCommandAuthority {
    HumanOwner,
    Agent {
        profile_id: ProfileId,
        session_id: SessionId,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RuntimeWorkerBinding {
    pub root_tree_id: RootTreeId,
    pub worker_id: WorkerId,
    pub generation: Generation,
    pub lease_authentication: EntityId,
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
    ProvisionConversationSession {
        conversation_id: ConversationId,
        profile_id: ProfileId,
        generation: Generation,
        now: UtcTimestamp,
    },
    ProvisionConversationSessions {
        conversation_id: ConversationId,
        assignments: Vec<ConversationSessionAssignment>,
        generation: Generation,
        now: UtcTimestamp,
    },
    DrainConversationActions {
        conversation_id: ConversationId,
        generation: Generation,
    },
    PendingConversationActionSessions {
        conversation_id: ConversationId,
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
        requester_authority: RuntimeCommandAuthority,
        worker_binding: RuntimeWorkerBinding,
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
            Self::ProvisionConversationSession {
                conversation_id,
                profile_id,
                generation,
                now,
            } => runtime
                .provision_conversation_session(conversation_id, profile_id, *generation, *now)
                .map(RuntimeResponse::Session),
            Self::ProvisionConversationSessions {
                conversation_id,
                assignments,
                generation,
                now,
            } => runtime
                .provision_conversation_sessions(conversation_id, assignments, *generation, *now)
                .map(RuntimeResponse::Sessions),
            Self::DrainConversationActions {
                conversation_id,
                generation,
            } => runtime
                .drain_conversation_actions(conversation_id, *generation)
                .map(|()| RuntimeResponse::Complete),
            Self::PendingConversationActionSessions { conversation_id } => runtime
                .pending_conversation_action_sessions(conversation_id)
                .map(RuntimeResponse::Sessions),
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
                requester_authority,
                worker_binding,
                scope_session_id,
                command,
                generation,
            } => runtime
                .execute_feature_authorized(
                    client_id,
                    requester_authority,
                    worker_binding,
                    scope_session_id.as_ref(),
                    command,
                    *generation,
                )
                .map(|result| RuntimeResponse::Command(Box::new(result))),
            Self::Maintain => runtime.maintain().map(|()| RuntimeResponse::Complete),
            Self::CandidateCanary(request) => runtime
                .candidate_canary(request)
                .map(RuntimeResponse::CandidateCanary),
        };
        response.unwrap_or_else(RuntimeResponse::Failed)
    }
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
    fn provision_conversation_session(
        &self,
        conversation_id: &ConversationId,
        profile_id: &ProfileId,
        generation: Generation,
        now: UtcTimestamp,
    ) -> Result<RuntimeSession, String>;
    fn provision_conversation_sessions(
        &self,
        conversation_id: &ConversationId,
        assignments: &[ConversationSessionAssignment],
        generation: Generation,
        now: UtcTimestamp,
    ) -> Result<Vec<RuntimeSession>, String>;
    fn drain_conversation_actions(
        &self,
        conversation_id: &ConversationId,
        generation: Generation,
    ) -> Result<(), String>;
    fn pending_conversation_action_sessions(
        &self,
        _conversation_id: &ConversationId,
    ) -> Result<Vec<RuntimeSession>, String> {
        Err("pending conversation action routing is unavailable".into())
    }
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
    fn execute_feature_authorized(
        &self,
        client_id: &ClientId,
        requester_authority: &RuntimeCommandAuthority,
        worker_binding: &RuntimeWorkerBinding,
        scope_session_id: Option<&SessionId>,
        command: &ClientCommand,
        generation: Generation,
    ) -> Result<CommandResult, String> {
        let _ = requester_authority;
        let _ = worker_binding;
        self.execute_feature(client_id, scope_session_id, command, generation)
    }
    fn maintain(&self) -> Result<(), String>;
    fn candidate_canary(
        &self,
        _request: &CandidateCanaryRequest,
    ) -> Result<CandidateCanaryReport, String> {
        Err("candidate canary evaluation is unavailable".into())
    }
}

#[cfg(test)]
mod authority_tests {
    use super::*;

    fn worker_binding() -> RuntimeWorkerBinding {
        RuntimeWorkerBinding {
            root_tree_id: RootTreeId::new(),
            worker_id: WorkerId::new(),
            generation: Generation::ZERO,
            lease_authentication: EntityId::new(),
        }
    }

    #[test]
    fn execution_request_preserves_requester_authority_separately_from_route() {
        let client_id = ClientId::new();
        let profile_id = ProfileId::new();
        let requester_session = SessionId::new();
        let target_session = SessionId::new();
        let request = RuntimeRequest::ExecuteFeature {
            client_id,
            requester_authority: RuntimeCommandAuthority::Agent {
                profile_id,
                session_id: requester_session,
            },
            worker_binding: worker_binding(),
            scope_session_id: Some(target_session.clone()),
            command: ClientCommand::AgentLifecycle(keith_protocol::AgentLifecycleCommand::List),
            generation: Generation::ZERO,
        };
        let RuntimeRequest::ExecuteFeature {
            requester_authority,
            scope_session_id,
            ..
        } = request
        else {
            panic!("expected feature request")
        };
        assert!(matches!(
            requester_authority,
            RuntimeCommandAuthority::Agent { .. }
        ));
        assert_eq!(scope_session_id, Some(target_session));
    }

    #[test]
    fn unscoped_owner_authority_is_explicit_on_the_wire() {
        let request = RuntimeRequest::ExecuteFeature {
            client_id: ClientId::new(),
            requester_authority: RuntimeCommandAuthority::HumanOwner,
            worker_binding: worker_binding(),
            scope_session_id: None,
            command: ClientCommand::AgentLifecycle(keith_protocol::AgentLifecycleCommand::List),
            generation: Generation::ZERO,
        };
        let RuntimeRequest::ExecuteFeature {
            requester_authority,
            scope_session_id,
            ..
        } = request
        else {
            panic!("expected feature request")
        };
        assert_eq!(requester_authority, RuntimeCommandAuthority::HumanOwner);
        assert!(scope_session_id.is_none());
    }

    #[test]
    fn conversation_session_assignments_preserve_exact_profile_roots_on_the_wire() {
        let conversation_id = ConversationId::new();
        let assignments = vec![
            ConversationSessionAssignment {
                profile_id: ProfileId::new(),
                root_tree_id: RootTreeId::new(),
            },
            ConversationSessionAssignment {
                profile_id: ProfileId::new(),
                root_tree_id: RootTreeId::new(),
            },
        ];
        let request = RuntimeRequest::ProvisionConversationSessions {
            conversation_id,
            assignments,
            generation: Generation::ZERO,
            now: UtcTimestamp::UNIX_EPOCH,
        };
        let encoded = serde_json::to_vec(&request).unwrap();
        assert_eq!(
            serde_json::from_slice::<RuntimeRequest>(&encoded).unwrap(),
            request
        );
    }
}
