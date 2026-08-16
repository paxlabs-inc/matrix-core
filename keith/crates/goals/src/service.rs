use std::fmt::Display;
use std::sync::{Mutex, MutexGuard};

use keith_action_store::{
    ActionLimits, ActionPayload, ActionPriority, ActionRecord, ActionSource, DeliveryPolicy,
    PersistentActionInbox, SessionAction,
};
use keith_agent_types::{
    ActionId, CURRENT_SCHEMA_VERSION, EntityId, GoalId, Revision, SessionId, UtcTimestamp,
};
use keith_state_store_core::{
    ActionRepository, GoalRepository, VersionedRecord, WritePrecondition,
};

use crate::model::{
    Goal, GoalEdit, GoalError, GoalLimitKind, GoalLimits, GoalProjection, GoalState,
    GoalStopReason, GoalTerminalSummary, GoalUsage, GoalUsageDelta, LinkUpdate,
};

struct StoredGoal {
    goal: Goal,
    storage_revision: Revision,
}

pub struct PersistentGoalService<G, A> {
    goals: G,
    actions: PersistentActionInbox<A>,
    serial: Mutex<()>,
}

impl<G, A> PersistentGoalService<G, A>
where
    G: GoalRepository,
    G::Error: Display,
    A: ActionRepository,
    A::Error: Display,
{
    pub const fn new(goals: G, actions: PersistentActionInbox<A>) -> Self {
        Self {
            goals,
            actions,
            serial: Mutex::new(()),
        }
    }

    /// Creates a durable draft goal.
    ///
    /// # Errors
    ///
    /// Returns an error for an empty objective, invalid limits, or persistence failure.
    pub fn create(
        &self,
        session_id: SessionId,
        objective: impl Into<String>,
        limits: GoalLimits,
        now: UtcTimestamp,
    ) -> Result<Goal, GoalError> {
        limits.validate()?;
        let objective = objective.into();
        validate_objective(&objective)?;
        let _guard = self.lock()?;
        let goal = Goal {
            id: GoalId::new(),
            session_id,
            objective,
            state: GoalState::Draft,
            limits,
            usage: GoalUsage::default(),
            plan_id: None,
            waiting_condition_id: None,
            created_at: now,
            updated_at: now,
            started_at: None,
            terminal_summary: None,
            last_stop: None,
            archived_at: None,
            revision: Revision::ZERO,
            resume_state: None,
        };
        self.goals
            .put_goal(encode(&goal)?, WritePrecondition::Missing)
            .map_err(repository_error)?;
        Ok(goal)
    }

    /// Loads one goal.
    ///
    /// # Errors
    ///
    /// Returns an error for repository or record-integrity failures.
    pub fn get(&self, id: &GoalId) -> Result<Option<Goal>, GoalError> {
        let _guard = self.lock()?;
        self.load(id).map(|stored| stored.map(|value| value.goal))
    }

    /// Lists non-archived goals owned by a session.
    ///
    /// # Errors
    ///
    /// Returns an error for repository or record-integrity failures.
    pub fn list_session(&self, session_id: &SessionId) -> Result<Vec<Goal>, GoalError> {
        let _guard = self.lock()?;
        let mut goals = self
            .load_all()?
            .into_iter()
            .map(|stored| stored.goal)
            .filter(|goal| &goal.session_id == session_id && goal.archived_at.is_none())
            .collect::<Vec<_>>();
        goals.sort_by_key(|goal| (goal.created_at, goal.id.clone()));
        Ok(goals)
    }

    /// Returns the complete projection required by full clients.
    ///
    /// # Errors
    ///
    /// Returns an error when the goal is missing or inaccessible.
    pub fn projection(&self, id: &GoalId) -> Result<GoalProjection, GoalError> {
        let _guard = self.lock()?;
        let stored = self.required(id)?;
        Ok(GoalProjection::from(&stored.goal))
    }

    /// Edits objective, limits, plan, and waiting-condition links in a controllable state.
    ///
    /// # Errors
    ///
    /// Returns an error for terminal/running/archived goals, invalid values, or persistence failure.
    pub fn edit(&self, id: &GoalId, edit: GoalEdit, now: UtcTimestamp) -> Result<Goal, GoalError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        ensure_not_archived(&stored.goal)?;
        if !matches!(
            stored.goal.state,
            GoalState::Draft | GoalState::Ready | GoalState::Paused | GoalState::Blocked
        ) {
            return Err(GoalError::NotActive);
        }
        if let Some(objective) = edit.objective {
            validate_objective(&objective)?;
            stored.goal.objective = objective;
        }
        if let Some(limits) = edit.limits {
            limits.validate()?;
            ensure_usage_within(stored.goal.usage, limits)?;
            stored.goal.limits = limits;
            if stored.goal.state == GoalState::Blocked
                && matches!(
                    stored.goal.last_stop,
                    Some(GoalStopReason::LimitReached { .. })
                )
                && reached_limit(stored.goal.usage, limits).is_none()
            {
                stored.goal.last_stop = None;
            }
        }
        apply_link(&mut stored.goal.plan_id, edit.plan);
        apply_link(
            &mut stored.goal.waiting_condition_id,
            edit.waiting_condition,
        );
        self.persist(&mut stored, now)?;
        Ok(stored.goal)
    }

    /// Moves a goal through a validated state transition.
    ///
    /// # Errors
    ///
    /// Returns an error for illegal transitions, missing required links/summaries, or persistence.
    pub fn transition(
        &self,
        id: &GoalId,
        next: GoalState,
        summary: Option<String>,
        now: UtcTimestamp,
    ) -> Result<Goal, GoalError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        ensure_not_archived(&stored.goal)?;
        if next.is_terminal() {
            self.cancel_continuations(&stored.goal, now)?;
        }
        self.transition_stored(&mut stored, next, summary, now)?;
        Ok(stored.goal)
    }

    /// Pauses a ready or active goal while retaining its valid resume state.
    ///
    /// # Errors
    ///
    /// Returns an error for illegal state or persistence failure.
    pub fn pause(&self, id: &GoalId, now: UtcTimestamp) -> Result<Goal, GoalError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        ensure_not_archived(&stored.goal)?;
        let current = stored.goal.state;
        if !current.allows(GoalState::Paused) {
            return Err(GoalError::IllegalTransition {
                from: current,
                to: GoalState::Paused,
            });
        }
        stored.goal.resume_state = Some(match current {
            GoalState::Ready => GoalState::Ready,
            GoalState::Waiting => GoalState::Waiting,
            GoalState::Reviewing => GoalState::Reviewing,
            GoalState::Running | GoalState::Blocked => GoalState::Running,
            _ => return Err(GoalError::NotActive),
        });
        stored.goal.state = GoalState::Paused;
        self.persist(&mut stored, now)?;
        Ok(stored.goal)
    }

    /// Resumes a paused or recoverable blocked goal.
    ///
    /// # Errors
    ///
    /// Returns an error when the goal remains at a limit or cannot resume.
    pub fn resume(&self, id: &GoalId, now: UtcTimestamp) -> Result<Goal, GoalError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        ensure_not_archived(&stored.goal)?;
        if let Some(limit) = reached_limit(stored.goal.usage, stored.goal.limits) {
            return Err(GoalError::BudgetExceeded(limit));
        }
        let next = match stored.goal.state {
            GoalState::Paused => stored.goal.resume_state.take().unwrap_or(GoalState::Ready),
            GoalState::Blocked => GoalState::Running,
            state => {
                return Err(GoalError::IllegalTransition {
                    from: state,
                    to: GoalState::Running,
                });
            }
        };
        if next == GoalState::Waiting && stored.goal.waiting_condition_id.is_none() {
            return Err(GoalError::Invalid(
                "waiting goals require a waiting-condition link".into(),
            ));
        }
        stored.goal.state = next;
        stored.goal.last_stop = None;
        if next == GoalState::Running && stored.goal.started_at.is_none() {
            stored.goal.started_at = Some(now);
        }
        self.persist(&mut stored, now)?;
        Ok(stored.goal)
    }

    /// Records bounded resource usage and blocks at the exact first reached limit.
    ///
    /// # Errors
    ///
    /// Returns an error if an increment would cross a limit or the goal is inactive.
    pub fn record_usage(
        &self,
        id: &GoalId,
        delta: GoalUsageDelta,
        now: UtcTimestamp,
    ) -> Result<Goal, GoalError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        ensure_not_archived(&stored.goal)?;
        if !matches!(
            stored.goal.state,
            GoalState::Running | GoalState::Waiting | GoalState::Reviewing
        ) {
            return Err(GoalError::NotActive);
        }
        let next_usage = bounded_usage(
            stored.goal.usage,
            delta,
            stored.goal.limits,
            now,
            stored.goal.started_at,
        );
        stored.goal.usage = next_usage;
        if let Some(limit) = reached_limit(next_usage, stored.goal.limits) {
            stored.goal.state = GoalState::Blocked;
            stored.goal.last_stop = Some(GoalStopReason::LimitReached { limit });
        }
        self.persist(&mut stored, now)?;
        Ok(stored.goal)
    }

    /// Cancels a non-terminal goal with a durable terminal summary.
    ///
    /// # Errors
    ///
    /// Returns an error for an illegal transition, empty summary, or persistence failure.
    pub fn cancel(
        &self,
        id: &GoalId,
        summary: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<Goal, GoalError> {
        self.transition(id, GoalState::Cancelled, Some(summary.into()), now)
    }

    /// Blocks an active goal with an inspectable user-facing reason.
    ///
    /// # Errors
    ///
    /// Returns an error for an empty reason, illegal state, or persistence failure.
    pub fn block(
        &self,
        id: &GoalId,
        summary: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<Goal, GoalError> {
        let summary = summary.into();
        if summary.trim().is_empty() {
            return Err(GoalError::Invalid("blocked summary cannot be empty".into()));
        }
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        ensure_not_archived(&stored.goal)?;
        let current = stored.goal.state;
        if !current.allows(GoalState::Blocked) {
            return Err(GoalError::IllegalTransition {
                from: current,
                to: GoalState::Blocked,
            });
        }
        stored.goal.state = GoalState::Blocked;
        stored.goal.last_stop = Some(GoalStopReason::UserBlocked { summary });
        self.persist(&mut stored, now)?;
        Ok(stored.goal)
    }

    /// Archives a terminal goal without deleting its durable audit record.
    ///
    /// # Errors
    ///
    /// Returns an error for a non-terminal/already archived goal or persistence failure.
    pub fn archive(&self, id: &GoalId, now: UtcTimestamp) -> Result<Goal, GoalError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        ensure_not_archived(&stored.goal)?;
        if !stored.goal.state.is_terminal() {
            return Err(GoalError::NotActive);
        }
        stored.goal.archived_at = Some(now);
        self.persist(&mut stored, now)?;
        Ok(stored.goal)
    }

    /// Enqueues continuation through the ordinary durable action inbox. Existing queued
    /// continuation for the same goal is returned, making restart recovery idempotent.
    ///
    /// # Errors
    ///
    /// Returns an error when the goal is not running, reaches a limit, or enqueueing fails.
    pub fn enqueue_continuation(
        &self,
        id: &GoalId,
        now: UtcTimestamp,
    ) -> Result<ActionRecord, GoalError> {
        let _guard = self.lock()?;
        let mut stored = self.required(id)?;
        ensure_not_archived(&stored.goal)?;
        if stored.goal.state != GoalState::Running {
            return Err(GoalError::NotActive);
        }
        refresh_elapsed(&mut stored.goal, now);
        if let Some(limit) = reached_limit(stored.goal.usage, stored.goal.limits) {
            stored.goal.state = GoalState::Blocked;
            stored.goal.last_stop = Some(GoalStopReason::LimitReached { limit });
            self.persist(&mut stored, now)?;
            return Err(GoalError::BudgetExceeded(limit));
        }
        if let Some(existing) = self
            .actions
            .list_session(&stored.goal.session_id)?
            .into_iter()
            .find(|record| {
                !record.state.is_terminal()
                    && matches!(
                        &record.action.payload,
                        ActionPayload::ContinueGoal { goal_id } if goal_id == id
                    )
            })
        {
            return Ok(existing);
        }
        let limits = stored.goal.limits;
        self.actions
            .submit(
                SessionAction {
                    id: ActionId::new(),
                    session_id: stored.goal.session_id,
                    source: ActionSource::AutonomousContinuation {
                        goal_id: id.clone(),
                    },
                    delivery: DeliveryPolicy::WhenIdle,
                    priority: ActionPriority::Background,
                    created_at: now,
                    not_before: None,
                    deadline: elapsed_deadline(stored.goal.started_at, limits.max_elapsed_ms),
                    limits: action_limits(limits, stored.goal.usage),
                    reply_route: None,
                    payload: ActionPayload::ContinueGoal {
                        goal_id: id.clone(),
                    },
                },
                now,
            )
            .map_err(GoalError::from)
    }

    fn transition_stored(
        &self,
        stored: &mut StoredGoal,
        next: GoalState,
        summary: Option<String>,
        now: UtcTimestamp,
    ) -> Result<(), GoalError> {
        let current = stored.goal.state;
        if !current.allows(next) {
            return Err(GoalError::IllegalTransition {
                from: current,
                to: next,
            });
        }
        if next == GoalState::Waiting && stored.goal.waiting_condition_id.is_none() {
            return Err(GoalError::Invalid(
                "waiting goals require a waiting-condition link".into(),
            ));
        }
        if next.is_terminal() {
            let summary = summary.ok_or_else(|| {
                GoalError::Invalid("terminal goals require a completion/failure summary".into())
            })?;
            if summary.trim().is_empty() {
                return Err(GoalError::Invalid(
                    "terminal goal summary cannot be empty".into(),
                ));
            }
            stored.goal.terminal_summary = Some(GoalTerminalSummary {
                state: next,
                summary,
                finished_at: now,
                final_usage: stored.goal.usage,
            });
        } else if summary.is_some() {
            return Err(GoalError::Invalid(
                "summaries are reserved for terminal transitions".into(),
            ));
        }
        stored.goal.state = next;
        stored.goal.last_stop = None;
        stored.goal.resume_state = None;
        if next == GoalState::Running && stored.goal.started_at.is_none() {
            stored.goal.started_at = Some(now);
        }
        self.persist(stored, now)
    }

    fn required(&self, id: &GoalId) -> Result<StoredGoal, GoalError> {
        self.load(id)?
            .ok_or_else(|| GoalError::NotFound(id.clone()))
    }

    fn load(&self, id: &GoalId) -> Result<Option<StoredGoal>, GoalError> {
        self.goals
            .get_goal(id.as_entity_id())
            .map_err(repository_error)?
            .map(decode)
            .transpose()
    }

    fn load_all(&self) -> Result<Vec<StoredGoal>, GoalError> {
        self.goals
            .list_goals()
            .map_err(repository_error)?
            .into_iter()
            .map(decode)
            .collect()
    }

    fn persist(&self, stored: &mut StoredGoal, now: UtcTimestamp) -> Result<(), GoalError> {
        let revision = stored
            .storage_revision
            .checked_next()
            .ok_or(GoalError::RevisionOverflow)?;
        stored.goal.revision = revision;
        stored.goal.updated_at = now;
        self.goals
            .put_goal(
                encode(&stored.goal)?,
                WritePrecondition::Exact(stored.storage_revision),
            )
            .map_err(repository_error)?;
        stored.storage_revision = revision;
        Ok(())
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, GoalError> {
        self.serial.lock().map_err(|_| GoalError::LockPoisoned)
    }

    fn cancel_continuations(&self, goal: &Goal, now: UtcTimestamp) -> Result<(), GoalError> {
        for record in self.actions.list_session(&goal.session_id)? {
            if !record.state.is_terminal()
                && matches!(
                    &record.action.payload,
                    ActionPayload::ContinueGoal { goal_id } if goal_id == &goal.id
                )
            {
                self.actions
                    .cancel(&record.action.id, now, "owning goal became terminal")?;
            }
        }
        Ok(())
    }
}

fn action_limits(limits: GoalLimits, usage: GoalUsage) -> ActionLimits {
    ActionLimits {
        max_turns: Some(limits.max_turns.saturating_sub(usage.turns)),
        max_tokens: Some(limits.max_tokens.saturating_sub(usage.tokens)),
        max_elapsed_ms: Some(limits.max_elapsed_ms.saturating_sub(usage.elapsed_ms)),
        max_tool_calls: Some(limits.max_processes.saturating_sub(usage.processes)),
        max_children: Some(
            u16::try_from(limits.max_children.saturating_sub(usage.children)).unwrap_or(u16::MAX),
        ),
    }
}

fn validate_objective(objective: &str) -> Result<(), GoalError> {
    if objective.trim().is_empty() {
        Err(GoalError::Invalid("goal objective cannot be empty".into()))
    } else {
        Ok(())
    }
}

fn ensure_not_archived(goal: &Goal) -> Result<(), GoalError> {
    if goal.archived_at.is_some() {
        Err(GoalError::Archived)
    } else {
        Ok(())
    }
}

fn apply_link(target: &mut Option<EntityId>, update: LinkUpdate) {
    match update {
        LinkUpdate::Keep => {}
        LinkUpdate::Set(id) => *target = Some(id),
        LinkUpdate::Clear => *target = None,
    }
}

fn bounded_usage(
    usage: GoalUsage,
    delta: GoalUsageDelta,
    limits: GoalLimits,
    now: UtcTimestamp,
    started_at: Option<UtcTimestamp>,
) -> GoalUsage {
    GoalUsage {
        turns: usage
            .turns
            .saturating_add(delta.turns)
            .min(limits.max_turns),
        tokens: usage
            .tokens
            .saturating_add(delta.tokens)
            .min(limits.max_tokens),
        elapsed_ms: usage
            .elapsed_ms
            .max(elapsed_ms(started_at, now))
            .min(limits.max_elapsed_ms),
        reviews: usage
            .reviews
            .saturating_add(delta.reviews)
            .min(limits.max_reviews),
        children: usage
            .children
            .saturating_add(delta.children)
            .min(limits.max_children),
        retries: usage
            .retries
            .saturating_add(delta.retries)
            .min(limits.max_retries),
        processes: usage
            .processes
            .saturating_add(delta.processes)
            .min(limits.max_processes),
        storage_bytes: usage
            .storage_bytes
            .saturating_add(delta.storage_bytes)
            .min(limits.max_storage_bytes),
        cost_microunits: usage
            .cost_microunits
            .saturating_add(delta.cost_microunits)
            .min(limits.max_cost_microunits),
    }
}

fn ensure_usage_within(usage: GoalUsage, limits: GoalLimits) -> Result<(), GoalError> {
    for (kind, used, limit) in usage_limit_pairs(usage, limits) {
        if used > limit {
            return Err(GoalError::BudgetExceeded(kind));
        }
    }
    Ok(())
}

fn reached_limit(usage: GoalUsage, limits: GoalLimits) -> Option<GoalLimitKind> {
    usage_limit_pairs(usage, limits)
        .into_iter()
        .find_map(|(kind, used, limit)| (used >= limit).then_some(kind))
}

fn usage_limit_pairs(usage: GoalUsage, limits: GoalLimits) -> [(GoalLimitKind, u64, u64); 9] {
    [
        (
            GoalLimitKind::Turns,
            u64::from(usage.turns),
            u64::from(limits.max_turns),
        ),
        (GoalLimitKind::Tokens, usage.tokens, limits.max_tokens),
        (
            GoalLimitKind::ElapsedTime,
            usage.elapsed_ms,
            limits.max_elapsed_ms,
        ),
        (
            GoalLimitKind::Reviews,
            u64::from(usage.reviews),
            u64::from(limits.max_reviews),
        ),
        (
            GoalLimitKind::Children,
            u64::from(usage.children),
            u64::from(limits.max_children),
        ),
        (
            GoalLimitKind::Retries,
            u64::from(usage.retries),
            u64::from(limits.max_retries),
        ),
        (
            GoalLimitKind::Processes,
            u64::from(usage.processes),
            u64::from(limits.max_processes),
        ),
        (
            GoalLimitKind::Storage,
            usage.storage_bytes,
            limits.max_storage_bytes,
        ),
        (
            GoalLimitKind::Cost,
            usage.cost_microunits,
            limits.max_cost_microunits,
        ),
    ]
}

fn refresh_elapsed(goal: &mut Goal, now: UtcTimestamp) {
    goal.usage.elapsed_ms = goal
        .usage
        .elapsed_ms
        .max(elapsed_ms(goal.started_at, now))
        .min(goal.limits.max_elapsed_ms);
}

fn elapsed_ms(started_at: Option<UtcTimestamp>, now: UtcTimestamp) -> u64 {
    started_at.map_or(0, |started| {
        u64::try_from(now.unix_millis().saturating_sub(started.unix_millis())).unwrap_or(0)
    })
}

fn elapsed_deadline(started_at: Option<UtcTimestamp>, max_elapsed_ms: u64) -> Option<UtcTimestamp> {
    let started = started_at?.unix_millis();
    let duration = i64::try_from(max_elapsed_ms).ok()?;
    Some(UtcTimestamp::from_unix_millis(
        started.saturating_add(duration),
    ))
}

fn encode(goal: &Goal) -> Result<VersionedRecord, GoalError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: goal.id.as_entity_id().clone(),
        revision: goal.revision,
        updated_at: goal.updated_at,
        payload: serde_json::to_value(goal)
            .map_err(|error| GoalError::Corrupt(error.to_string()))?,
    })
}

fn decode(record: VersionedRecord) -> Result<StoredGoal, GoalError> {
    let goal: Goal = serde_json::from_value(record.payload)
        .map_err(|error| GoalError::Corrupt(error.to_string()))?;
    if record.version.major != CURRENT_SCHEMA_VERSION.major
        || record.version.minor > CURRENT_SCHEMA_VERSION.minor
        || goal.id.as_entity_id() != &record.id
        || goal.revision != record.revision
    {
        return Err(GoalError::Corrupt(
            "record envelope does not match its goal payload".into(),
        ));
    }
    Ok(StoredGoal {
        goal,
        storage_revision: record.revision,
    })
}

fn repository_error(error: impl Display) -> GoalError {
    GoalError::Repository(error.to_string())
}

#[cfg(test)]
mod tests {
    use std::path::Path;

    use keith_action_store::ActionInboxConfig;
    use keith_action_store::ActionState;
    use keith_state_store::EmbeddedStore;
    use tempfile::tempdir;

    use super::*;

    type Service = PersistentGoalService<EmbeddedStore, EmbeddedStore>;

    fn service(path: &Path) -> Service {
        let goals = EmbeddedStore::open(path, None).unwrap();
        let action_repository = EmbeddedStore::open(path, None).unwrap();
        let inbox =
            PersistentActionInbox::new(action_repository, ActionInboxConfig::default()).unwrap();
        PersistentGoalService::new(goals, inbox)
    }

    fn active(service: &Service, session: &SessionId, limits: GoalLimits) -> Goal {
        let goal = service
            .create(
                session.clone(),
                "Finish the durable task",
                limits,
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        service
            .transition(
                &goal.id,
                GoalState::Ready,
                None,
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        service
            .transition(
                &goal.id,
                GoalState::Running,
                None,
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap()
    }

    #[test]
    fn transition_matrix_covers_every_state_pair() {
        let states = [
            GoalState::Draft,
            GoalState::Ready,
            GoalState::Running,
            GoalState::Waiting,
            GoalState::Reviewing,
            GoalState::Paused,
            GoalState::Blocked,
            GoalState::Complete,
            GoalState::Failed,
            GoalState::Cancelled,
        ];
        let expected = [
            (GoalState::Draft, GoalState::Ready),
            (GoalState::Draft, GoalState::Cancelled),
            (GoalState::Ready, GoalState::Running),
            (GoalState::Ready, GoalState::Paused),
            (GoalState::Ready, GoalState::Cancelled),
            (GoalState::Running, GoalState::Waiting),
            (GoalState::Running, GoalState::Reviewing),
            (GoalState::Running, GoalState::Paused),
            (GoalState::Running, GoalState::Blocked),
            (GoalState::Running, GoalState::Complete),
            (GoalState::Running, GoalState::Failed),
            (GoalState::Running, GoalState::Cancelled),
            (GoalState::Waiting, GoalState::Running),
            (GoalState::Waiting, GoalState::Paused),
            (GoalState::Waiting, GoalState::Blocked),
            (GoalState::Waiting, GoalState::Failed),
            (GoalState::Waiting, GoalState::Cancelled),
            (GoalState::Reviewing, GoalState::Running),
            (GoalState::Reviewing, GoalState::Paused),
            (GoalState::Reviewing, GoalState::Blocked),
            (GoalState::Reviewing, GoalState::Complete),
            (GoalState::Reviewing, GoalState::Failed),
            (GoalState::Reviewing, GoalState::Cancelled),
            (GoalState::Paused, GoalState::Ready),
            (GoalState::Paused, GoalState::Running),
            (GoalState::Paused, GoalState::Waiting),
            (GoalState::Paused, GoalState::Reviewing),
            (GoalState::Paused, GoalState::Cancelled),
            (GoalState::Blocked, GoalState::Running),
            (GoalState::Blocked, GoalState::Paused),
            (GoalState::Blocked, GoalState::Failed),
            (GoalState::Blocked, GoalState::Cancelled),
        ];
        for from in states {
            for to in states {
                assert_eq!(
                    from.allows(to),
                    expected.contains(&(from, to)),
                    "unexpected transition {from:?} -> {to:?}"
                );
            }
        }
    }

    #[test]
    fn restart_recovers_state_links_usage_and_idempotent_continuation() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("goals.sqlite");
        let session = SessionId::new();
        let goal_id;
        let action_id;
        {
            let service = service(&path);
            let goal = service
                .create(
                    session.clone(),
                    "Recover after restart",
                    GoalLimits::default(),
                    UtcTimestamp::UNIX_EPOCH,
                )
                .unwrap();
            goal_id = goal.id;
            service
                .edit(
                    &goal_id,
                    GoalEdit {
                        plan: LinkUpdate::Set(EntityId::new()),
                        ..GoalEdit::default()
                    },
                    UtcTimestamp::from_unix_millis(1),
                )
                .unwrap();
            service
                .transition(
                    &goal_id,
                    GoalState::Ready,
                    None,
                    UtcTimestamp::from_unix_millis(2),
                )
                .unwrap();
            service
                .transition(
                    &goal_id,
                    GoalState::Running,
                    None,
                    UtcTimestamp::from_unix_millis(3),
                )
                .unwrap();
            service
                .record_usage(
                    &goal_id,
                    GoalUsageDelta {
                        tokens: 25,
                        ..GoalUsageDelta::default()
                    },
                    UtcTimestamp::from_unix_millis(4),
                )
                .unwrap();
            action_id = service
                .enqueue_continuation(&goal_id, UtcTimestamp::from_unix_millis(5))
                .unwrap()
                .action
                .id;
        }
        let service = service(&path);
        let recovered = service.get(&goal_id).unwrap().unwrap();
        assert_eq!(recovered.state, GoalState::Running);
        assert_eq!(recovered.usage.tokens, 25);
        assert!(recovered.plan_id.is_some());
        let duplicate = service
            .enqueue_continuation(&goal_id, UtcTimestamp::from_unix_millis(6))
            .unwrap();
        assert_eq!(duplicate.action.id, action_id);
        assert_eq!(duplicate.state, ActionState::Queued);
        assert!(matches!(
            duplicate.action.payload,
            ActionPayload::ContinueGoal { goal_id: queued } if queued == goal_id
        ));
    }

    #[test]
    fn each_budget_stops_at_the_exact_limit_without_overshoot() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("limits.sqlite");
        let deltas = [
            GoalUsageDelta {
                turns: 1,
                ..GoalUsageDelta::default()
            },
            GoalUsageDelta {
                tokens: 1,
                ..GoalUsageDelta::default()
            },
            GoalUsageDelta {
                reviews: 1,
                ..GoalUsageDelta::default()
            },
            GoalUsageDelta {
                children: 1,
                ..GoalUsageDelta::default()
            },
            GoalUsageDelta {
                retries: 1,
                ..GoalUsageDelta::default()
            },
            GoalUsageDelta {
                processes: 1,
                ..GoalUsageDelta::default()
            },
            GoalUsageDelta {
                storage_bytes: 1,
                ..GoalUsageDelta::default()
            },
            GoalUsageDelta {
                cost_microunits: 1,
                ..GoalUsageDelta::default()
            },
        ];
        for delta in deltas {
            let service = service(&path);
            let session = SessionId::new();
            let limits = GoalLimits {
                max_turns: if delta.turns == 1 { 1 } else { 10 },
                max_tokens: if delta.tokens == 1 { 1 } else { 10 },
                max_elapsed_ms: 10,
                max_reviews: if delta.reviews == 1 { 1 } else { 10 },
                max_children: if delta.children == 1 { 1 } else { 10 },
                max_retries: if delta.retries == 1 { 1 } else { 10 },
                max_processes: if delta.processes == 1 { 1 } else { 10 },
                max_storage_bytes: if delta.storage_bytes == 1 { 1 } else { 10 },
                max_cost_microunits: if delta.cost_microunits == 1 { 1 } else { 10 },
            };
            let goal = active(&service, &session, limits);
            let stopped = service
                .record_usage(&goal.id, delta, UtcTimestamp::from_unix_millis(2))
                .unwrap();
            assert_eq!(stopped.state, GoalState::Blocked);
            assert!(matches!(
                stopped.last_stop,
                Some(GoalStopReason::LimitReached { .. })
            ));
            assert!(matches!(
                service.enqueue_continuation(&goal.id, UtcTimestamp::from_unix_millis(3)),
                Err(GoalError::NotActive)
            ));
        }
    }

    #[test]
    fn elapsed_limit_pause_resume_edit_cancel_archive_and_projection_are_durable() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("lifecycle.sqlite");
        let service = service(&path);
        let session = SessionId::new();
        let limits = GoalLimits {
            max_elapsed_ms: 5,
            ..GoalLimits::default()
        };
        let goal = active(&service, &session, limits);
        service
            .enqueue_continuation(&goal.id, UtcTimestamp::from_unix_millis(2))
            .unwrap();
        let paused = service
            .pause(&goal.id, UtcTimestamp::from_unix_millis(3))
            .unwrap();
        assert_eq!(paused.state, GoalState::Paused);
        let resumed = service
            .resume(&goal.id, UtcTimestamp::from_unix_millis(4))
            .unwrap();
        assert_eq!(resumed.state, GoalState::Running);
        let blocked = service
            .record_usage(
                &goal.id,
                GoalUsageDelta::default(),
                UtcTimestamp::from_unix_millis(7),
            )
            .unwrap();
        assert_eq!(blocked.state, GoalState::Blocked);
        assert_eq!(
            blocked.last_stop,
            Some(GoalStopReason::LimitReached {
                limit: GoalLimitKind::ElapsedTime
            })
        );
        let edited = service
            .edit(
                &goal.id,
                GoalEdit {
                    limits: Some(GoalLimits {
                        max_elapsed_ms: 50,
                        ..limits
                    }),
                    ..GoalEdit::default()
                },
                UtcTimestamp::from_unix_millis(8),
            )
            .unwrap();
        assert!(edited.last_stop.is_none());
        service
            .resume(&goal.id, UtcTimestamp::from_unix_millis(9))
            .unwrap();
        let user_blocked = service
            .block(
                &goal.id,
                "Owner input is required",
                UtcTimestamp::from_unix_millis(9),
            )
            .unwrap();
        assert_eq!(
            user_blocked.last_stop,
            Some(GoalStopReason::UserBlocked {
                summary: "Owner input is required".into()
            })
        );
        service
            .resume(&goal.id, UtcTimestamp::from_unix_millis(9))
            .unwrap();
        let cancelled = service
            .cancel(
                &goal.id,
                "Stopped by owner",
                UtcTimestamp::from_unix_millis(10),
            )
            .unwrap();
        assert_eq!(cancelled.state, GoalState::Cancelled);
        assert_eq!(
            cancelled
                .terminal_summary
                .as_ref()
                .map(|summary| summary.summary.as_str()),
            Some("Stopped by owner")
        );
        let actions = service.actions.list_session(&session).unwrap();
        assert!(actions.iter().all(|record| record.state.is_terminal()));
        let archived = service
            .archive(&goal.id, UtcTimestamp::from_unix_millis(11))
            .unwrap();
        assert!(archived.archived_at.is_some());
        let projection = service.projection(&goal.id).unwrap();
        assert!(projection.archived);
        assert_eq!(projection.state, GoalState::Cancelled);
        assert!(service.list_session(&session).unwrap().is_empty());
    }

    #[test]
    fn complete_and_failed_goals_require_terminal_summaries() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("summaries.sqlite");
        let service = service(&path);
        let session = SessionId::new();
        let first = active(&service, &session, GoalLimits::default());
        assert!(matches!(
            service.transition(
                &first.id,
                GoalState::Complete,
                None,
                UtcTimestamp::from_unix_millis(3)
            ),
            Err(GoalError::Invalid(_))
        ));
        let completed = service
            .transition(
                &first.id,
                GoalState::Complete,
                Some("Delivered the requested result".into()),
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        assert_eq!(
            completed.terminal_summary.unwrap().state,
            GoalState::Complete
        );

        let second = active(&service, &session, GoalLimits::default());
        let failed = service
            .transition(
                &second.id,
                GoalState::Failed,
                Some("External dependency remained unavailable".into()),
                UtcTimestamp::from_unix_millis(5),
            )
            .unwrap();
        assert_eq!(failed.terminal_summary.unwrap().state, GoalState::Failed);
    }
}
