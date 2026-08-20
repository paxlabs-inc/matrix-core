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
    ChildCancellation, ChildError, ChildLimits, ChildMessage, ChildMessageKind, ChildMessageSender,
    ChildProjection, ChildRecord, ChildRetention, ChildSpec, ChildStatus, ChildWorkspaceMode,
    ParentAuthority,
};
