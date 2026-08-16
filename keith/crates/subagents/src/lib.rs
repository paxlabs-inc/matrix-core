#![forbid(unsafe_code)]

mod coordinator;
mod model;

pub use coordinator::ChildCoordinator;
pub use model::{
    ChildCancellation, ChildError, ChildLimits, ChildMessage, ChildMessageKind, ChildMessageSender,
    ChildProjection, ChildRecord, ChildRetention, ChildSpec, ChildStatus, ChildWorkspaceMode,
    ParentAuthority,
};
