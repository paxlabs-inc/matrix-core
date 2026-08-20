use keith_action_store::ActionStoreError;
use keith_agent_types::{EntityId, GoalId, Revision, SessionId, UtcTimestamp};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GoalState {
    Draft,
    Ready,
    Running,
    Waiting,
    Reviewing,
    Paused,
    Blocked,
    Complete,
    Failed,
    Cancelled,
}

impl GoalState {
    pub const fn is_terminal(self) -> bool {
        matches!(self, Self::Complete | Self::Failed | Self::Cancelled)
    }

    pub(crate) const fn allows(self, next: Self) -> bool {
        match self {
            Self::Draft => matches!(next, Self::Ready | Self::Cancelled),
            Self::Ready => matches!(next, Self::Running | Self::Paused | Self::Cancelled),
            Self::Running => matches!(
                next,
                Self::Waiting
                    | Self::Reviewing
                    | Self::Paused
                    | Self::Blocked
                    | Self::Complete
                    | Self::Failed
                    | Self::Cancelled
            ),
            Self::Waiting => matches!(
                next,
                Self::Running | Self::Paused | Self::Blocked | Self::Failed | Self::Cancelled
            ),
            Self::Reviewing => matches!(
                next,
                Self::Running
                    | Self::Paused
                    | Self::Blocked
                    | Self::Complete
                    | Self::Failed
                    | Self::Cancelled
            ),
            Self::Paused => matches!(
                next,
                Self::Ready | Self::Running | Self::Waiting | Self::Reviewing | Self::Cancelled
            ),
            Self::Blocked => matches!(
                next,
                Self::Running | Self::Paused | Self::Failed | Self::Cancelled
            ),
            Self::Complete | Self::Failed | Self::Cancelled => false,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GoalLimits {
    pub max_turns: u32,
    pub max_tokens: u64,
    pub max_elapsed_ms: u64,
    pub max_reviews: u32,
    pub max_children: u32,
    pub max_retries: u32,
    pub max_processes: u32,
    pub max_storage_bytes: u64,
    pub max_cost_microunits: u64,
}

impl Default for GoalLimits {
    fn default() -> Self {
        Self {
            max_turns: 100,
            max_tokens: 1_000_000,
            max_elapsed_ms: 24 * 60 * 60 * 1_000,
            max_reviews: 20,
            max_children: 16,
            max_retries: 20,
            max_processes: 64,
            max_storage_bytes: 1024 * 1024 * 1024,
            max_cost_microunits: 100_000_000,
        }
    }
}

impl GoalLimits {
    pub(crate) fn validate(self) -> Result<(), GoalError> {
        if self.max_turns == 0
            || self.max_tokens == 0
            || self.max_elapsed_ms == 0
            || self.max_reviews == 0
            || self.max_children == 0
            || self.max_retries == 0
            || self.max_processes == 0
            || self.max_storage_bytes == 0
            || self.max_cost_microunits == 0
        {
            Err(GoalError::Invalid(
                "every autonomous goal limit must be non-zero".into(),
            ))
        } else {
            Ok(())
        }
    }
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GoalUsage {
    pub turns: u32,
    pub tokens: u64,
    pub elapsed_ms: u64,
    pub reviews: u32,
    pub children: u32,
    pub retries: u32,
    pub processes: u32,
    pub storage_bytes: u64,
    pub cost_microunits: u64,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct GoalUsageDelta {
    pub turns: u32,
    pub tokens: u64,
    pub reviews: u32,
    pub children: u32,
    pub retries: u32,
    pub processes: u32,
    pub storage_bytes: u64,
    pub cost_microunits: u64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum GoalLimitKind {
    Turns,
    Tokens,
    ElapsedTime,
    Reviews,
    Children,
    Retries,
    Processes,
    Storage,
    Cost,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "reason")]
pub enum GoalStopReason {
    LimitReached { limit: GoalLimitKind },
    UserBlocked { summary: String },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GoalTerminalSummary {
    pub state: GoalState,
    pub summary: String,
    pub finished_at: UtcTimestamp,
    pub final_usage: GoalUsage,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Goal {
    pub id: GoalId,
    pub session_id: SessionId,
    pub objective: String,
    pub state: GoalState,
    pub limits: GoalLimits,
    pub usage: GoalUsage,
    pub plan_id: Option<EntityId>,
    pub waiting_condition_id: Option<EntityId>,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub started_at: Option<UtcTimestamp>,
    pub terminal_summary: Option<GoalTerminalSummary>,
    pub last_stop: Option<GoalStopReason>,
    pub archived_at: Option<UtcTimestamp>,
    pub revision: Revision,
    pub(crate) resume_state: Option<GoalState>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum LinkUpdate {
    Keep,
    Set(EntityId),
    Clear,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GoalEdit {
    pub objective: Option<String>,
    pub limits: Option<GoalLimits>,
    pub plan: LinkUpdate,
    pub waiting_condition: LinkUpdate,
}

impl Default for GoalEdit {
    fn default() -> Self {
        Self {
            objective: None,
            limits: None,
            plan: LinkUpdate::Keep,
            waiting_condition: LinkUpdate::Keep,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct GoalProjection {
    pub id: GoalId,
    pub session_id: SessionId,
    pub objective: String,
    pub state: GoalState,
    pub limits: GoalLimits,
    pub usage: GoalUsage,
    pub plan_id: Option<EntityId>,
    pub waiting_condition_id: Option<EntityId>,
    pub terminal_summary: Option<GoalTerminalSummary>,
    pub last_stop: Option<GoalStopReason>,
    pub archived: bool,
    pub updated_at: UtcTimestamp,
}

impl From<&Goal> for GoalProjection {
    fn from(goal: &Goal) -> Self {
        Self {
            id: goal.id.clone(),
            session_id: goal.session_id.clone(),
            objective: goal.objective.clone(),
            state: goal.state,
            limits: goal.limits,
            usage: goal.usage,
            plan_id: goal.plan_id.clone(),
            waiting_condition_id: goal.waiting_condition_id.clone(),
            terminal_summary: goal.terminal_summary.clone(),
            last_stop: goal.last_stop.clone(),
            archived: goal.archived_at.is_some(),
            updated_at: goal.updated_at,
        }
    }
}

#[derive(Debug, Error)]
pub enum GoalError {
    #[error("goal repository failed: {0}")]
    Repository(String),
    #[error("action queue failed: {0}")]
    Action(#[from] ActionStoreError),
    #[error("goal {0} was not found")]
    NotFound(GoalId),
    #[error("goal record is corrupt: {0}")]
    Corrupt(String),
    #[error("goal is invalid: {0}")]
    Invalid(String),
    #[error("goal cannot transition from {from:?} to {to:?}")]
    IllegalTransition { from: GoalState, to: GoalState },
    #[error("goal operation is unavailable after archival")]
    Archived,
    #[error("goal operation requires an active state")]
    NotActive,
    #[error("goal usage increment would exceed the {0:?} limit")]
    BudgetExceeded(GoalLimitKind),
    #[error("goal persistence revision overflow")]
    RevisionOverflow,
    #[error("goal state lock was poisoned")]
    LockPoisoned,
}
