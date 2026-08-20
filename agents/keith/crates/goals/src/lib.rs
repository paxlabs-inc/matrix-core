#![forbid(unsafe_code)]

mod model;
mod service;

pub use model::{
    Goal, GoalEdit, GoalError, GoalLimitKind, GoalLimits, GoalProjection, GoalState,
    GoalStopReason, GoalTerminalSummary, GoalUsage, GoalUsageDelta, LinkUpdate,
};
pub use service::PersistentGoalService;
