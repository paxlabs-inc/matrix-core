use std::path::PathBuf;

use keith_agent_types::{
    ArtifactId, CommandId, Generation, ProfileId, RootTreeId, Sequence, SessionId, WorkspaceId,
};
use keith_platform_contracts::{AcpConnectionId, CancellationId};
use keith_protocol::{ResumeCursor, TurnTerminalStatus};
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::AcpClientSessionConfig;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AcpSessionStatus {
    Ready,
    Running,
    Cancelling,
    Closed,
    Failed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PromptState {
    Prepared,
    Submitted,
    Completed,
    Cancelled,
    Failed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DurablePrompt {
    pub ordinal: u64,
    pub command_id: CommandId,
    pub content_sha256: String,
    pub state: PromptState,
    pub artifact_ids: Vec<ArtifactId>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcpSessionRecord {
    pub acp_session_id: String,
    pub keith_session_id: SessionId,
    pub profile_id: ProfileId,
    pub workspace_id: WorkspaceId,
    pub cwd: PathBuf,
    pub additional_directories: Vec<PathBuf>,
    pub status: AcpSessionStatus,
    pub cursor: Option<ResumeCursor>,
    pub next_prompt_ordinal: u64,
    pub in_flight_prompt: Option<DurablePrompt>,
    #[serde(default)]
    pub forked_from: Option<String>,
    #[serde(default)]
    pub client_context: Option<AcpClientSessionConfig>,
    #[serde(default)]
    pub protocol_version: Option<u16>,
}

impl AcpSessionRecord {
    pub fn cursor_parts(&self) -> Option<(&RootTreeId, Generation, Sequence)> {
        self.cursor.as_ref().map(|cursor| {
            (
                &cursor.root_tree_id,
                cursor.generation,
                cursor.last_sequence,
            )
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum AcpContentBlock {
    Text(String),
    ResourceLink {
        name: String,
        uri: String,
    },
    EmbeddedText {
        name: String,
        uri: String,
        media_type: String,
        text: String,
    },
    Binary(AcpBinaryContent),
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AcpBinaryContent {
    pub name: String,
    pub media_type: String,
    pub bytes: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind", content = "value")]
pub enum AcpUpdateKind {
    AssistantMessage {
        message_id: String,
        text: String,
        committed: bool,
    },
    Thought {
        text: String,
    },
    Plan {
        plan_id: String,
        summary: String,
        state: String,
        terminal: bool,
    },
    Tool {
        tool_call_id: String,
        title: String,
        state: String,
        terminal: bool,
    },
    ToolOutput {
        message_id: String,
        text: String,
    },
    Diff {
        title: String,
        patch: String,
    },
    Usage {
        input_tokens: u64,
        output_tokens: u64,
        cached_input_tokens: u64,
        estimated_cost_microunits: u64,
    },
    Failure {
        message: String,
        retryable: bool,
    },
    Warning {
        message: String,
    },
    Final {
        status: TurnTerminalStatus,
        detail: Option<String>,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcpUpdate {
    pub event_id: String,
    pub kind: AcpUpdateKind,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AcpPromptOutcome {
    pub updates: Vec<AcpUpdate>,
    pub terminal: Option<TurnTerminalStatus>,
}

#[derive(Debug, Error)]
pub enum BridgeError {
    #[error("ACP connection {0} is not authorized for this session")]
    Unauthorized(AcpConnectionId),
    #[error("ACP session is unknown: {0}")]
    UnknownSession(String),
    #[error("ACP session is closed: {0}")]
    ClosedSession(String),
    #[error("another prompt is already active for ACP session {0}")]
    PromptInFlight(String),
    #[error("ACP path is outside the configured workspace roots: {0}")]
    WorkspaceBoundary(PathBuf),
    #[error("ACP content is unsupported: {0}")]
    UnsupportedContent(String),
    #[error("ACP prompt exceeds the configured content limit")]
    ContentLimit,
    #[error("ACP prompt exceeds the configured attachment limit")]
    AttachmentLimit,
    #[error("Keith rejected the request: {0}")]
    KeithRejected(String),
    #[error("Keith returned an unexpected response: {0}")]
    UnexpectedResponse(&'static str),
    #[error("Keith connection failed: {0}")]
    Connection(#[from] keith_channel_core::AgentConnectionError),
    #[error("Keith transport failed: {0}")]
    Transport(#[from] keith_connection::ConnectionError),
    #[error("ACP persistence failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("ACP persistence is malformed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("host clock is outside Keith's supported range")]
    Clock,
    #[error("ACP cancellation identity is malformed: {0}")]
    Cancellation(CancellationId),
    #[error("ACP durable state lock was poisoned")]
    LockPoisoned,
    #[error("ACP client capability was denied: {0}")]
    ClientCapability(String),
    #[error("ACP MCP policy rejected configuration: {0}")]
    McpPolicy(String),
    #[error("ACP permission was denied: {0}")]
    PermissionDenied(String),
    #[error("ACP protocol version boundary rejected the request: {0}")]
    ProtocolVersion(String),
}
