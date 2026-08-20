#![forbid(unsafe_code)]

use keith_agent_types::{EntityId, GoalId, KernelId, SessionId, UtcTimestamp};
use serde::{Deserialize, Serialize};

pub const KERNEL_PROTOCOL_VERSION: u16 = 1;

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GuestRequest {
    pub protocol: u16,
    pub request_id: EntityId,
    pub command: GuestCommand,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "command")]
pub enum GuestCommand {
    Execute { code: String },
    Snapshot,
    Restore { state: serde_json::Value },
    Shutdown,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GuestStream {
    Stdout,
    Stderr,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GuestEvent {
    pub protocol: u16,
    pub request_id: Option<EntityId>,
    pub event: GuestEventKind,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "event")]
pub enum GuestEventKind {
    Ready {
        runtime: String,
    },
    Output {
        stream: GuestStream,
        text: String,
    },
    ExecutionFinished {
        result: Option<serde_json::Value>,
        error: Option<String>,
    },
    Snapshot {
        state: serde_json::Value,
        excluded: Vec<ExcludedState>,
    },
    Restored,
    BridgeRequest {
        bridge_id: EntityId,
        operation: BridgeOperation,
    },
    Shutdown,
    Error {
        code: String,
        message: String,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExcludedState {
    pub name: String,
    pub type_name: String,
    pub reason: String,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BridgeCapability {
    Children,
    Messages,
    Goals,
    Mcp,
    Compaction,
    Artifacts,
    Memory,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MemorySensitivity {
    Public,
    Personal,
    Sensitive,
    Secret,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MemoryBridgeRequest {
    pub expected_revision: Option<u64>,
    pub max_result_bytes: u32,
    pub max_sensitivity: MemorySensitivity,
    pub operation: MemoryBridgeOperation,
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "operation")]
pub enum MemoryBridgeOperation {
    Catalog,
    Search {
        query: String,
        limit: usize,
        include_disputed: bool,
    },
    Timeline {
        session_id: Option<SessionId>,
        from: Option<UtcTimestamp>,
        until: Option<UtcTimestamp>,
        limit: usize,
        include_disputed: bool,
    },
    Expand {
        node_id: String,
        depth: usize,
        max_nodes: usize,
    },
    Compare {
        left_node: String,
        right_node: String,
    },
    Evidence {
        evidence_ids: Vec<EntityId>,
    },
    PlanCapsule {
        query: String,
        evidence_ids: Vec<EntityId>,
        token_budget: u64,
    },
    Recall {
        query: String,
        max_depth: u16,
        max_scouts: u32,
        token_budget: u64,
    },
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum BridgeOperation {
    CreateChild {
        objective: String,
    },
    SendMessage {
        session_id: SessionId,
        text: String,
    },
    UpdateGoal {
        goal_id: GoalId,
        state: String,
        summary: Option<String>,
    },
    CallMcp {
        server: String,
        tool: String,
        arguments: serde_json::Value,
    },
    Compact {
        target_tokens: u64,
    },
    CreateArtifact {
        media_type: String,
        text: String,
    },
    Memory {
        request: MemoryBridgeRequest,
    },
}

impl BridgeOperation {
    pub const fn capability(&self) -> BridgeCapability {
        match self {
            Self::CreateChild { .. } => BridgeCapability::Children,
            Self::SendMessage { .. } => BridgeCapability::Messages,
            Self::UpdateGoal { .. } => BridgeCapability::Goals,
            Self::CallMcp { .. } => BridgeCapability::Mcp,
            Self::Compact { .. } => BridgeCapability::Compaction,
            Self::CreateArtifact { .. } => BridgeCapability::Artifacts,
            Self::Memory { .. } => BridgeCapability::Memory,
        }
    }
}

#[derive(Clone, Debug, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BridgeReply {
    pub protocol: u16,
    pub request_id: EntityId,
    pub bridge_id: EntityId,
    pub result: Option<serde_json::Value>,
    pub error: Option<BridgeFailure>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BridgeFailure {
    pub code: String,
    pub message: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct BridgeContext {
    pub kernel_id: KernelId,
    pub session_id: SessionId,
}

#[cfg(test)]
mod tests {
    use super::*;

    #[test]
    fn bridge_operations_have_one_unambiguous_capability() {
        let operations = [
            (
                BridgeOperation::CreateChild {
                    objective: "child".into(),
                },
                BridgeCapability::Children,
            ),
            (
                BridgeOperation::SendMessage {
                    session_id: SessionId::new(),
                    text: "status".into(),
                },
                BridgeCapability::Messages,
            ),
            (
                BridgeOperation::UpdateGoal {
                    goal_id: GoalId::new(),
                    state: "complete".into(),
                    summary: Some("done".into()),
                },
                BridgeCapability::Goals,
            ),
            (
                BridgeOperation::CallMcp {
                    server: "documents".into(),
                    tool: "search".into(),
                    arguments: serde_json::json!({"query": "test"}),
                },
                BridgeCapability::Mcp,
            ),
            (
                BridgeOperation::Compact { target_tokens: 512 },
                BridgeCapability::Compaction,
            ),
            (
                BridgeOperation::CreateArtifact {
                    media_type: "text/plain".into(),
                    text: "deliverable".into(),
                },
                BridgeCapability::Artifacts,
            ),
            (
                BridgeOperation::Memory {
                    request: MemoryBridgeRequest {
                        expected_revision: Some(7),
                        max_result_bytes: 8_192,
                        max_sensitivity: MemorySensitivity::Personal,
                        operation: MemoryBridgeOperation::Search {
                            query: "routing".into(),
                            limit: 8,
                            include_disputed: false,
                        },
                    },
                },
                BridgeCapability::Memory,
            ),
        ];
        for (operation, capability) in operations {
            assert_eq!(operation.capability(), capability);
            let encoded = serde_json::to_string(&operation).unwrap();
            let decoded: BridgeOperation = serde_json::from_str(&encoded).unwrap();
            assert_eq!(decoded, operation);
        }
    }

    #[test]
    fn wire_protocol_rejects_unknown_fields() {
        let value =
            r#"{"protocol":1,"request_id":null,"event":{"event":"restored"},"authority":"daemon"}"#;
        assert!(serde_json::from_str::<GuestEvent>(value).is_err());
    }
}
