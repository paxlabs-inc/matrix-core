#![forbid(unsafe_code)]

mod coordinator;
mod memory_scout;
mod model;

pub use coordinator::ChildCoordinator;
pub use memory_scout::{
    MemoryScoutCapability, MemoryScoutContractError, MemoryScoutLimits, MemoryScoutPurpose,
    MemoryScoutScopeManifest, MemoryScoutSpec,
};
pub use model::{
    ChildAuthorityCeiling, ChildAutonomyCeiling, ChildCancellation, ChildError, ChildLimits,
    ChildMessage, ChildMessageKind, ChildMessageSender, ChildModelRoute, ChildPrincipal,
    ChildProjection, ChildRecord, ChildRetention, ChildSpec, ChildStatus, ChildWorkspaceMode,
    ParentAuthority,
};
