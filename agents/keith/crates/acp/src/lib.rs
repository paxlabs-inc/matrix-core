#![forbid(unsafe_code)]

mod bridge;
mod capabilities;
mod model;
mod permission;
mod projector;
mod protocol_router;
mod store;
mod transport;

pub use bridge::{AcpBridgeConfig, AcpSessionBridge};
pub use capabilities::{
    AcpClientCapabilities, AcpClientPolicy, AcpClientSessionConfig, AcpClientTool,
    AcpClientToolBroker, AcpClientToolKind, AcpCredentialBinding, AcpCredentialPlacement,
    AcpMcpHealth, AcpMcpServer, AcpMcpTransport, AcpTerminalRequest,
};
pub use model::{
    AcpBinaryContent, AcpContentBlock, AcpPromptOutcome, AcpSessionRecord, AcpSessionStatus,
    AcpUpdate, AcpUpdateKind, BridgeError, DurablePrompt, PromptState,
};
pub use permission::{
    AcpPermissionBridge, AcpPermissionChallenge, AcpPermissionDecision, AcpPermissionOption,
    AcpPermissionOptionKind, AcpPermissionRequest,
};
pub use projector::AcpUpdateProjector;
pub use protocol_router::{
    AcpProtocolRoute, AcpProtocolRouteError, AcpProtocolRouter, AcpProtocolVersion,
};
pub use store::AcpSessionStore;
pub use transport::{
    AcpTransport, AcpTransportAuthenticator, AcpTransportConnection, AcpTransportError,
    AcpTransportFrame, AcpTransportFrameId,
};

pub const ACP_PROTOCOL_VERSION: u16 = 1;
