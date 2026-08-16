#![forbid(unsafe_code)]

use keith_agent_types::{ClientId, Generation, ProfileId, RootTreeId, SessionId, UtcTimestamp};
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
    SelectModel(ModelSelection),
    RunPrompt {
        prompt: SubmitPrompt,
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
    Failed(String),
}

impl RuntimeRequest {
    pub fn execute(&self, runtime: &dyn CommandRuntime) -> RuntimeResponse {
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
            Self::SelectModel(selection) => runtime
                .select_model(selection)
                .map(|()| RuntimeResponse::Complete),
            Self::RunPrompt { prompt, generation } => runtime
                .run_prompt(prompt, *generation)
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
    fn select_model(&self, selection: &ModelSelection) -> Result<(), String>;
    fn run_prompt(
        &self,
        prompt: &SubmitPrompt,
        generation: Generation,
    ) -> Result<SessionSnapshot, String>;
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
}
