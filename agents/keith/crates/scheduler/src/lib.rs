#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Display;
use std::str::FromStr;
use std::sync::{Arc, Mutex, MutexGuard};

use chrono::{Datelike, TimeZone, Timelike, Utc};
use chrono_tz::Tz;
use keith_action_store::{
    ActionLimits, ActionPayload, ActionPriority, ActionSource, DeliveryPolicy,
    PersistentActionInbox, ReplyRoute, SessionAction,
};
use keith_agent_types::{
    ActionId, CURRENT_SCHEMA_VERSION, EntityId, JobId, ProfileId, Revision, SchemaVersion,
    SessionId, UtcTimestamp,
};
use keith_state_store_core::{
    AtomicStateRepository, Collection, JobAttemptRepository, RecordMutation, ScheduleRepository,
    VersionedRecord, WritePrecondition,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum ScheduleSpec {
    Once {
        at: UtcTimestamp,
    },
    Interval {
        every_ms: u64,
        anchor: UtcTimestamp,
    },
    Calendar {
        expression: String,
        time_zone: String,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "policy")]
pub enum MissedRunPolicy {
    Skip,
    RunOnce,
    ReplayBounded { max_runs: u16 },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum JobState {
    Active,
    Paused,
    Completed,
    Failed,
    Cancelled,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ScheduledJob {
    pub version: SchemaVersion,
    pub id: JobId,
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub schedule: ScheduleSpec,
    pub action: ActionPayload,
    pub limits: ActionLimits,
    pub reply_route: Option<ReplyRoute>,
    pub missed_run: MissedRunPolicy,
    pub state: JobState,
    pub next_run: Option<UtcTimestamp>,
    pub last_run: Option<UtcTimestamp>,
    pub attempt_count: u32,
    pub failure_count: u32,
    pub safe_error: Option<String>,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NewScheduledJob {
    pub profile_id: ProfileId,
    pub session_id: SessionId,
    pub schedule: ScheduleSpec,
    pub action: ActionPayload,
    pub limits: ActionLimits,
    pub reply_route: Option<ReplyRoute>,
    pub missed_run: MissedRunPolicy,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum JobAttemptState {
    Claimed,
    RetryScheduled,
    Enqueued,
    Completed,
    Failed,
    Cancelled,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct JobAttempt {
    pub version: SchemaVersion,
    pub job_id: JobId,
    pub attempt_id: EntityId,
    pub ordinal: u32,
    pub scheduled_for: UtcTimestamp,
    pub claimed_by: EntityId,
    pub claim_expires: UtcTimestamp,
    pub state: JobAttemptState,
    pub action_id: ActionId,
    pub retry_count: u16,
    pub retry_at: Option<UtcTimestamp>,
    pub safe_error: Option<String>,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ScheduleProjection {
    pub job_id: JobId,
    pub state: JobState,
    pub schedule: ScheduleSpec,
    pub next_run: Option<UtcTimestamp>,
    pub last_run: Option<UtcTimestamp>,
    pub attempts: u32,
    pub failures: u32,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct JobUpdate {
    pub schedule: Option<ScheduleSpec>,
    pub action: Option<ActionPayload>,
    pub limits: Option<ActionLimits>,
    pub reply_route: Option<Option<ReplyRoute>>,
    pub missed_run: Option<MissedRunPolicy>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SchedulerConfig {
    pub claim_ttl_ms: u64,
    pub retry_backoff_ms: u64,
    pub max_enqueue_retries: u16,
    pub max_claims_per_tick: usize,
    pub on_time_grace_ms: u64,
}

impl Default for SchedulerConfig {
    fn default() -> Self {
        Self {
            claim_ttl_ms: 30_000,
            retry_backoff_ms: 5_000,
            max_enqueue_retries: 3,
            max_claims_per_tick: 64,
            on_time_grace_ms: 1_000,
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum EnqueueStatus {
    Accepted,
    AlreadyPresent,
}

pub trait ScheduledActionSink: Send + Sync {
    /// # Errors
    ///
    /// Returns a safe error when the persistent session action inbox rejects the action.
    fn enqueue(&self, action: SessionAction, now: UtcTimestamp) -> Result<EnqueueStatus, String>;
}

impl<R> ScheduledActionSink for PersistentActionInbox<R>
where
    R: keith_state_store_core::ActionRepository,
    R::Error: Display,
{
    fn enqueue(&self, action: SessionAction, now: UtcTimestamp) -> Result<EnqueueStatus, String> {
        if self
            .get(&action.id)
            .map_err(|error| error.to_string())?
            .is_some()
        {
            return Ok(EnqueueStatus::AlreadyPresent);
        }
        match self.submit(action.clone(), now) {
            Ok(_) => Ok(EnqueueStatus::Accepted),
            Err(error) => {
                if self
                    .get(&action.id)
                    .map_err(|lookup| lookup.to_string())?
                    .is_some()
                {
                    Ok(EnqueueStatus::AlreadyPresent)
                } else {
                    Err(error.to_string())
                }
            }
        }
    }
}

#[derive(Debug, Error)]
pub enum SchedulerError {
    #[error("scheduler repository failed: {0}")]
    Repository(String),
    #[error("scheduler record is corrupt: {0}")]
    Corrupt(String),
    #[error("scheduler state lock was poisoned")]
    LockPoisoned,
    #[error("schedule is invalid: {0}")]
    InvalidSchedule(String),
    #[error("scheduled job was not found")]
    NotFound,
    #[error("scheduled job transition is invalid")]
    InvalidTransition,
    #[error("time arithmetic overflowed")]
    TimeOverflow,
}

struct StoredJob {
    job: ScheduledJob,
    revision: Revision,
}

struct StoredAttempt {
    attempt: JobAttempt,
    revision: Revision,
}

pub struct Scheduler<R, S> {
    repository: Arc<R>,
    sink: Arc<S>,
    config: SchedulerConfig,
    serial: Mutex<()>,
}

impl<R, S> Scheduler<R, S>
where
    R: AtomicStateRepository
        + ScheduleRepository<Error = <R as AtomicStateRepository>::Error>
        + JobAttemptRepository<Error = <R as AtomicStateRepository>::Error>,
    <R as AtomicStateRepository>::Error: Display,
    S: ScheduledActionSink,
{
    /// Creates a scheduler over the transactional job store and persistent action inbox.
    ///
    /// # Errors
    ///
    /// Returns an error when claim, retry, or tick bounds are invalid.
    pub fn new(
        repository: Arc<R>,
        sink: Arc<S>,
        config: SchedulerConfig,
    ) -> Result<Self, SchedulerError> {
        if config.claim_ttl_ms == 0
            || config.retry_backoff_ms == 0
            || config.max_claims_per_tick == 0
        {
            return Err(SchedulerError::InvalidSchedule(
                "scheduler bounds must be non-zero".into(),
            ));
        }
        Ok(Self {
            repository,
            sink,
            config,
            serial: Mutex::new(()),
        })
    }

    /// Creates and durably schedules a one-time, interval, or IANA-zone calendar job.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid schedule/action data or persistence failure.
    pub fn create(
        &self,
        request: NewScheduledJob,
        now: UtcTimestamp,
    ) -> Result<ScheduledJob, SchedulerError> {
        validate_schedule(&request.schedule)?;
        validate_action(&request.action)?;
        let next_run = initial_run(&request.schedule, now)?;
        let job = ScheduledJob {
            version: CURRENT_SCHEMA_VERSION,
            id: JobId::new(),
            profile_id: request.profile_id,
            session_id: request.session_id,
            schedule: request.schedule,
            action: request.action,
            limits: request.limits,
            reply_route: request.reply_route,
            missed_run: request.missed_run,
            state: JobState::Active,
            next_run,
            last_run: None,
            attempt_count: 0,
            failure_count: 0,
            safe_error: None,
            created_at: now,
            updated_at: now,
        };
        self.repository
            .put_schedule(
                job_record(&job, Revision::ZERO)?,
                WritePrecondition::Missing,
            )
            .map_err(repository_error)?;
        Ok(job)
    }

    /// Claims due occurrences, advances jobs transactionally, and enqueues stable actions.
    ///
    /// # Errors
    ///
    /// Returns an error for corrupt persisted state, time overflow, or repository failure.
    pub fn tick(
        &self,
        claimant: &EntityId,
        now: UtcTimestamp,
    ) -> Result<Vec<JobAttempt>, SchedulerError> {
        self.tick_filtered(claimant, now, None)
    }

    /// Claims due work only for sessions retained beneath one worker-owned root tree.
    ///
    /// # Errors
    ///
    /// Returns an error for corrupt persisted state, time overflow, or repository failure.
    pub fn tick_sessions(
        &self,
        claimant: &EntityId,
        now: UtcTimestamp,
        session_ids: &BTreeSet<SessionId>,
    ) -> Result<Vec<JobAttempt>, SchedulerError> {
        self.tick_filtered(claimant, now, Some(session_ids))
    }

    fn tick_filtered(
        &self,
        claimant: &EntityId,
        now: UtcTimestamp,
        session_ids: Option<&BTreeSet<SessionId>>,
    ) -> Result<Vec<JobAttempt>, SchedulerError> {
        let _guard = self.lock()?;
        let mut processed = self.retry_due(now, session_ids)?;
        let jobs = self.load_jobs()?;
        for stored in jobs
            .into_iter()
            .filter(|stored| {
                stored.job.state == JobState::Active
                    && stored.job.next_run.is_some_and(|next| next <= now)
                    && session_ids.is_none_or(|sessions| sessions.contains(&stored.job.session_id))
            })
            .take(
                self.config
                    .max_claims_per_tick
                    .saturating_sub(processed.len()),
            )
        {
            let attempts = match self.claim_job(stored, claimant, now) {
                Ok(attempts) => attempts,
                Err(SchedulerError::Repository(_)) => continue,
                Err(error) => return Err(error),
            };
            for attempt in attempts {
                processed.push(self.enqueue_attempt(attempt, now)?);
            }
        }
        Ok(processed)
    }

    /// Pauses an active job without discarding its exact next due occurrence.
    ///
    /// # Errors
    ///
    /// Returns an error for missing/terminal jobs or persistence failure.
    pub fn pause(&self, job_id: &JobId, now: UtcTimestamp) -> Result<ScheduledJob, SchedulerError> {
        self.change_state(job_id, JobState::Paused, now)
    }

    /// Resumes a paused job so its missed-run policy is applied on the next tick.
    ///
    /// # Errors
    ///
    /// Returns an error for missing/non-paused jobs or persistence failure.
    pub fn resume(
        &self,
        job_id: &JobId,
        now: UtcTimestamp,
    ) -> Result<ScheduledJob, SchedulerError> {
        self.change_state(job_id, JobState::Active, now)
    }

    /// Updates mutable job configuration and deliberately recalculates changed schedules.
    ///
    /// # Errors
    ///
    /// Returns an error for missing/terminal jobs, invalid changes, or persistence failure.
    pub fn update(
        &self,
        job_id: &JobId,
        update: JobUpdate,
        now: UtcTimestamp,
    ) -> Result<ScheduledJob, SchedulerError> {
        let _guard = self.lock()?;
        let mut stored = self.required_job(job_id)?;
        if matches!(stored.job.state, JobState::Completed | JobState::Cancelled) {
            return Err(SchedulerError::InvalidTransition);
        }
        if let Some(schedule) = update.schedule {
            validate_schedule(&schedule)?;
            stored.job.next_run = initial_run(&schedule, now)?;
            stored.job.schedule = schedule;
        }
        if let Some(action) = update.action {
            validate_action(&action)?;
            stored.job.action = action;
        }
        if let Some(limits) = update.limits {
            stored.job.limits = limits;
        }
        if let Some(reply_route) = update.reply_route {
            stored.job.reply_route = reply_route;
        }
        if let Some(missed_run) = update.missed_run {
            stored.job.missed_run = missed_run;
        }
        stored.job.updated_at = now;
        self.put_job(&mut stored)?;
        Ok(stored.job)
    }

    /// Deletes a job and cancels every nonterminal attempt in one transaction.
    ///
    /// # Errors
    ///
    /// Returns an error for missing jobs, corrupt attempts, or persistence failure.
    pub fn delete(&self, job_id: &JobId, now: UtcTimestamp) -> Result<(), SchedulerError> {
        let _guard = self.lock()?;
        let stored = self.required_job(job_id)?;
        let mut mutations = vec![RecordMutation::Delete {
            collection: Collection::ScheduledJobs,
            id: job_id.as_entity_id().clone(),
            precondition: WritePrecondition::Exact(stored.revision),
        }];
        for mut attempt in self.load_attempts()?.into_iter().filter(|attempt| {
            attempt.attempt.job_id == *job_id && !attempt_terminal(attempt.attempt.state)
        }) {
            attempt.attempt.state = JobAttemptState::Cancelled;
            attempt.attempt.updated_at = now;
            mutations.push(RecordMutation::Put {
                collection: Collection::JobAttempts,
                record: attempt_record(&attempt.attempt, next_revision(attempt.revision)?)?,
                precondition: WritePrecondition::Exact(attempt.revision),
            });
        }
        self.repository
            .transact(&mutations)
            .map_err(repository_error)?;
        Ok(())
    }

    /// Records the terminal result of an enqueued attempt for operator projections.
    ///
    /// # Errors
    ///
    /// Returns an error for missing/non-enqueued attempts or persistence failure.
    pub fn finish_attempt(
        &self,
        attempt_id: &EntityId,
        succeeded: bool,
        safe_error: Option<String>,
        now: UtcTimestamp,
    ) -> Result<JobAttempt, SchedulerError> {
        let _guard = self.lock()?;
        let mut attempt = self.required_attempt(attempt_id)?;
        if attempt.attempt.state != JobAttemptState::Enqueued {
            return Err(SchedulerError::InvalidTransition);
        }
        attempt.attempt.state = if succeeded {
            JobAttemptState::Completed
        } else {
            JobAttemptState::Failed
        };
        attempt.attempt.safe_error.clone_from(&safe_error);
        attempt.attempt.updated_at = now;
        let mut mutations = vec![RecordMutation::Put {
            collection: Collection::JobAttempts,
            record: attempt_record(&attempt.attempt, next_revision(attempt.revision)?)?,
            precondition: WritePrecondition::Exact(attempt.revision),
        }];
        if !succeeded {
            let mut job = self.required_job(&attempt.attempt.job_id)?;
            job.job.failure_count = job.job.failure_count.saturating_add(1);
            job.job.safe_error = safe_error;
            job.job.updated_at = now;
            mutations.push(RecordMutation::Put {
                collection: Collection::ScheduledJobs,
                record: job_record(&job.job, next_revision(job.revision)?)?,
                precondition: WritePrecondition::Exact(job.revision),
            });
        }
        self.repository
            .transact(&mutations)
            .map_err(repository_error)?;
        Ok(attempt.attempt)
    }

    /// Lists stable client projections for every persisted schedule.
    ///
    /// # Errors
    ///
    /// Returns an error when persisted jobs cannot be decoded.
    pub fn projections(&self) -> Result<Vec<ScheduleProjection>, SchedulerError> {
        let mut projections = self
            .load_jobs()?
            .into_iter()
            .map(|stored| ScheduleProjection {
                job_id: stored.job.id,
                state: stored.job.state,
                schedule: stored.job.schedule,
                next_run: stored.job.next_run,
                last_run: stored.job.last_run,
                attempts: stored.job.attempt_count,
                failures: stored.job.failure_count,
                safe_error: stored.job.safe_error,
            })
            .collect::<Vec<_>>();
        projections.sort_by(|left, right| left.job_id.cmp(&right.job_id));
        Ok(projections)
    }

    /// Lists stable projections owned by one session.
    ///
    /// # Errors
    ///
    /// Returns an error when persisted jobs cannot be decoded.
    pub fn projections_for_session(
        &self,
        session_id: &SessionId,
    ) -> Result<Vec<ScheduleProjection>, SchedulerError> {
        let mut projections = self
            .load_jobs()?
            .into_iter()
            .filter(|stored| &stored.job.session_id == session_id)
            .map(|stored| ScheduleProjection {
                job_id: stored.job.id,
                state: stored.job.state,
                schedule: stored.job.schedule,
                next_run: stored.job.next_run,
                last_run: stored.job.last_run,
                attempts: stored.job.attempt_count,
                failures: stored.job.failure_count,
                safe_error: stored.job.safe_error,
            })
            .collect::<Vec<_>>();
        projections.sort_by(|left, right| left.job_id.cmp(&right.job_id));
        Ok(projections)
    }

    /// Returns the session that owns a scheduled job.
    ///
    /// # Errors
    ///
    /// Returns an error when the job is missing or corrupt.
    pub fn session_id(&self, job_id: &JobId) -> Result<SessionId, SchedulerError> {
        Ok(self.required_job(job_id)?.job.session_id)
    }

    fn claim_job(
        &self,
        mut stored: StoredJob,
        claimant: &EntityId,
        now: UtcTimestamp,
    ) -> Result<Vec<StoredAttempt>, SchedulerError> {
        let decision = due_decision(&stored.job, now, self.config.on_time_grace_ms)?;
        let mut mutations = Vec::new();
        let mut attempts = Vec::new();
        for scheduled_for in decision.run {
            stored.job.attempt_count = stored
                .job
                .attempt_count
                .checked_add(1)
                .ok_or(SchedulerError::TimeOverflow)?;
            let attempt = JobAttempt {
                version: CURRENT_SCHEMA_VERSION,
                job_id: stored.job.id.clone(),
                attempt_id: EntityId::new(),
                ordinal: stored.job.attempt_count,
                scheduled_for,
                claimed_by: claimant.clone(),
                claim_expires: add_millis(now, self.config.claim_ttl_ms)?,
                state: JobAttemptState::Claimed,
                action_id: ActionId::new(),
                retry_count: 0,
                retry_at: None,
                safe_error: None,
                updated_at: now,
            };
            mutations.push(RecordMutation::Put {
                collection: Collection::JobAttempts,
                record: attempt_record(&attempt, Revision::ZERO)?,
                precondition: WritePrecondition::Missing,
            });
            attempts.push(StoredAttempt {
                attempt,
                revision: Revision::ZERO,
            });
        }
        stored.job.next_run = decision.next_run;
        stored.job.last_run = decision.last_run.or(stored.job.last_run);
        stored.job.updated_at = now;
        if stored.job.next_run.is_none() {
            stored.job.state = JobState::Completed;
        }
        mutations.insert(
            0,
            RecordMutation::Put {
                collection: Collection::ScheduledJobs,
                record: job_record(&stored.job, next_revision(stored.revision)?)?,
                precondition: WritePrecondition::Exact(stored.revision),
            },
        );
        self.repository
            .transact(&mutations)
            .map_err(repository_error)?;
        Ok(attempts)
    }

    fn retry_due(
        &self,
        now: UtcTimestamp,
        session_ids: Option<&BTreeSet<SessionId>>,
    ) -> Result<Vec<JobAttempt>, SchedulerError> {
        let attempts = self.load_attempts()?;
        let job_sessions = if session_ids.is_some() {
            self.load_jobs()?
                .into_iter()
                .map(|stored| (stored.job.id, stored.job.session_id))
                .collect::<BTreeMap<_, _>>()
        } else {
            BTreeMap::new()
        };
        let mut processed = Vec::new();
        for mut stored in attempts.into_iter().filter(|stored| {
            let due = (stored.attempt.state == JobAttemptState::Claimed
                && stored.attempt.claim_expires <= now)
                || (stored.attempt.state == JobAttemptState::RetryScheduled
                    && stored.attempt.retry_at.is_some_and(|retry| retry <= now));
            due && session_ids.is_none_or(|sessions| {
                job_sessions
                    .get(&stored.attempt.job_id)
                    .is_some_and(|session_id| sessions.contains(session_id))
            })
        }) {
            if stored.attempt.state == JobAttemptState::Claimed {
                stored.attempt.state = JobAttemptState::RetryScheduled;
                stored.attempt.retry_at = Some(now);
                stored.attempt.safe_error = Some("expired scheduler claim recovered".into());
                stored.attempt.updated_at = now;
                self.put_attempt(&mut stored)?;
            }
            processed.push(self.enqueue_attempt(stored, now)?);
        }
        Ok(processed)
    }

    fn enqueue_attempt(
        &self,
        mut stored: StoredAttempt,
        now: UtcTimestamp,
    ) -> Result<JobAttempt, SchedulerError> {
        let job = self.required_job(&stored.attempt.job_id)?.job;
        let action = SessionAction {
            id: stored.attempt.action_id.clone(),
            session_id: job.session_id,
            source: ActionSource::Schedule {
                job_id: job.id,
                attempt: stored.attempt.ordinal,
            },
            delivery: DeliveryPolicy::Immediate,
            priority: ActionPriority::Scheduled,
            created_at: now,
            not_before: None,
            deadline: None,
            limits: job.limits,
            reply_route: job.reply_route,
            payload: job.action,
        };
        match self.sink.enqueue(action, now) {
            Ok(EnqueueStatus::Accepted | EnqueueStatus::AlreadyPresent) => {
                stored.attempt.state = JobAttemptState::Enqueued;
                stored.attempt.retry_at = None;
                stored.attempt.safe_error = None;
            }
            Err(error) if stored.attempt.retry_count < self.config.max_enqueue_retries => {
                stored.attempt.retry_count = stored.attempt.retry_count.saturating_add(1);
                stored.attempt.state = JobAttemptState::RetryScheduled;
                stored.attempt.retry_at = Some(add_millis(now, self.config.retry_backoff_ms)?);
                stored.attempt.safe_error = Some(safe_error(&error));
            }
            Err(error) => {
                stored.attempt.state = JobAttemptState::Failed;
                stored.attempt.retry_at = None;
                stored.attempt.safe_error = Some(safe_error(&error));
            }
        }
        stored.attempt.updated_at = now;
        self.put_attempt(&mut stored)?;
        Ok(stored.attempt)
    }

    fn change_state(
        &self,
        job_id: &JobId,
        next: JobState,
        now: UtcTimestamp,
    ) -> Result<ScheduledJob, SchedulerError> {
        let _guard = self.lock()?;
        let mut stored = self.required_job(job_id)?;
        let allowed = matches!(
            (stored.job.state, next),
            (JobState::Active, JobState::Paused) | (JobState::Paused, JobState::Active)
        );
        if !allowed {
            return Err(SchedulerError::InvalidTransition);
        }
        stored.job.state = next;
        stored.job.updated_at = now;
        self.put_job(&mut stored)?;
        Ok(stored.job)
    }

    fn put_job(&self, stored: &mut StoredJob) -> Result<(), SchedulerError> {
        let revision = next_revision(stored.revision)?;
        self.repository
            .put_schedule(
                job_record(&stored.job, revision)?,
                WritePrecondition::Exact(stored.revision),
            )
            .map_err(repository_error)?;
        stored.revision = revision;
        Ok(())
    }

    fn put_attempt(&self, stored: &mut StoredAttempt) -> Result<(), SchedulerError> {
        let revision = next_revision(stored.revision)?;
        self.repository
            .put_job_attempt(
                attempt_record(&stored.attempt, revision)?,
                WritePrecondition::Exact(stored.revision),
            )
            .map_err(repository_error)?;
        stored.revision = revision;
        Ok(())
    }

    fn required_job(&self, job_id: &JobId) -> Result<StoredJob, SchedulerError> {
        self.repository
            .get_schedule(job_id.as_entity_id())
            .map_err(repository_error)?
            .map(decode_job)
            .transpose()?
            .ok_or(SchedulerError::NotFound)
    }

    fn required_attempt(&self, attempt_id: &EntityId) -> Result<StoredAttempt, SchedulerError> {
        self.repository
            .get_job_attempt(attempt_id)
            .map_err(repository_error)?
            .map(decode_attempt)
            .transpose()?
            .ok_or(SchedulerError::NotFound)
    }

    fn load_jobs(&self) -> Result<Vec<StoredJob>, SchedulerError> {
        self.repository
            .list_schedules()
            .map_err(repository_error)?
            .into_iter()
            .map(decode_job)
            .collect()
    }

    fn load_attempts(&self) -> Result<Vec<StoredAttempt>, SchedulerError> {
        self.repository
            .list_job_attempts()
            .map_err(repository_error)?
            .into_iter()
            .map(decode_attempt)
            .collect()
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, SchedulerError> {
        self.serial.lock().map_err(|_| SchedulerError::LockPoisoned)
    }
}

struct DueDecision {
    run: Vec<UtcTimestamp>,
    next_run: Option<UtcTimestamp>,
    last_run: Option<UtcTimestamp>,
}

fn due_decision(
    job: &ScheduledJob,
    now: UtcTimestamp,
    on_time_grace_ms: u64,
) -> Result<DueDecision, SchedulerError> {
    let first = job.next_run.ok_or(SchedulerError::InvalidTransition)?;
    let occurrences = due_occurrences(&job.schedule, first, now)?;
    let grace = i64::try_from(on_time_grace_ms).map_err(|_| SchedulerError::TimeOverflow)?;
    let on_time = now
        .unix_millis()
        .checked_sub(first.unix_millis())
        .is_some_and(|delay| delay <= grace);
    let run = if on_time {
        occurrences.first().copied().into_iter().collect()
    } else {
        match job.missed_run {
            MissedRunPolicy::Skip => Vec::new(),
            MissedRunPolicy::RunOnce => occurrences.last().copied().into_iter().collect(),
            MissedRunPolicy::ReplayBounded { max_runs } => occurrences
                .iter()
                .take(usize::from(max_runs))
                .copied()
                .collect(),
        }
    };
    let last_run = occurrences.last().copied();
    let next_run = next_after(&job.schedule, now)?;
    Ok(DueDecision {
        run,
        next_run,
        last_run,
    })
}

fn due_occurrences(
    schedule: &ScheduleSpec,
    first: UtcTimestamp,
    now: UtcTimestamp,
) -> Result<Vec<UtcTimestamp>, SchedulerError> {
    let mut occurrences = Vec::new();
    let mut candidate = Some(first);
    while let Some(value) = candidate {
        if value > now {
            break;
        }
        occurrences.push(value);
        if occurrences.len() >= 65_536 {
            break;
        }
        candidate = next_after(schedule, value)?;
    }
    Ok(occurrences)
}

fn initial_run(
    schedule: &ScheduleSpec,
    now: UtcTimestamp,
) -> Result<Option<UtcTimestamp>, SchedulerError> {
    match schedule {
        ScheduleSpec::Once { at } => Ok(Some(*at)),
        ScheduleSpec::Interval { every_ms, anchor } => {
            if *anchor >= now {
                Ok(Some(*anchor))
            } else {
                let elapsed = now
                    .unix_millis()
                    .checked_sub(anchor.unix_millis())
                    .ok_or(SchedulerError::TimeOverflow)?;
                let every = i64::try_from(*every_ms).map_err(|_| SchedulerError::TimeOverflow)?;
                let periods = elapsed
                    .checked_add(every - 1)
                    .and_then(|value| value.checked_div(every))
                    .ok_or(SchedulerError::TimeOverflow)?;
                anchor
                    .unix_millis()
                    .checked_add(
                        periods
                            .checked_mul(every)
                            .ok_or(SchedulerError::TimeOverflow)?,
                    )
                    .map(UtcTimestamp::from_unix_millis)
                    .map(Some)
                    .ok_or(SchedulerError::TimeOverflow)
            }
        }
        ScheduleSpec::Calendar {
            expression,
            time_zone,
        } => next_calendar(expression, time_zone, now).map(Some),
    }
}

fn next_after(
    schedule: &ScheduleSpec,
    after: UtcTimestamp,
) -> Result<Option<UtcTimestamp>, SchedulerError> {
    match schedule {
        ScheduleSpec::Once { .. } => Ok(None),
        ScheduleSpec::Interval { every_ms, anchor } => {
            let every = i64::try_from(*every_ms).map_err(|_| SchedulerError::TimeOverflow)?;
            let elapsed = after
                .unix_millis()
                .checked_sub(anchor.unix_millis())
                .ok_or(SchedulerError::TimeOverflow)?;
            let periods = if elapsed < 0 {
                0
            } else {
                elapsed
                    .checked_div(every)
                    .and_then(|value| value.checked_add(1))
                    .ok_or(SchedulerError::TimeOverflow)?
            };
            anchor
                .unix_millis()
                .checked_add(
                    periods
                        .checked_mul(every)
                        .ok_or(SchedulerError::TimeOverflow)?,
                )
                .map(UtcTimestamp::from_unix_millis)
                .map(Some)
                .ok_or(SchedulerError::TimeOverflow)
        }
        ScheduleSpec::Calendar {
            expression,
            time_zone,
        } => next_calendar(expression, time_zone, after).map(Some),
    }
}

fn next_calendar(
    expression: &str,
    time_zone: &str,
    after: UtcTimestamp,
) -> Result<UtcTimestamp, SchedulerError> {
    let calendar = CalendarExpression::parse(expression)?;
    let zone = Tz::from_str(time_zone)
        .map_err(|_| SchedulerError::InvalidSchedule("invalid IANA time zone".into()))?;
    let first_minute = after
        .unix_millis()
        .div_euclid(60_000)
        .checked_add(1)
        .and_then(|minute| minute.checked_mul(60_000))
        .ok_or(SchedulerError::TimeOverflow)?;
    let max_minutes = 60_i64 * 24 * 366 * 5;
    for offset in 0..max_minutes {
        let millis = first_minute
            .checked_add(
                offset
                    .checked_mul(60_000)
                    .ok_or(SchedulerError::TimeOverflow)?,
            )
            .ok_or(SchedulerError::TimeOverflow)?;
        let utc = Utc
            .timestamp_millis_opt(millis)
            .single()
            .ok_or(SchedulerError::TimeOverflow)?;
        let local = utc.with_timezone(&zone);
        if calendar.matches(&local) {
            return Ok(UtcTimestamp::from_unix_millis(millis));
        }
    }
    Err(SchedulerError::InvalidSchedule(
        "calendar has no occurrence within five years".into(),
    ))
}

struct CalendarExpression {
    minute: CronField,
    hour: CronField,
    day: CronField,
    month: CronField,
    weekday: CronField,
}

impl CalendarExpression {
    fn parse(expression: &str) -> Result<Self, SchedulerError> {
        let fields = expression.split_whitespace().collect::<Vec<_>>();
        if fields.len() != 5 {
            return Err(SchedulerError::InvalidSchedule(
                "calendar expression must have five fields".into(),
            ));
        }
        Ok(Self {
            minute: CronField::parse(fields[0], 0, 59, false)?,
            hour: CronField::parse(fields[1], 0, 23, false)?,
            day: CronField::parse(fields[2], 1, 31, false)?,
            month: CronField::parse(fields[3], 1, 12, false)?,
            weekday: CronField::parse(fields[4], 0, 7, true)?,
        })
    }

    fn matches<T: TimeZone>(&self, date: &chrono::DateTime<T>) -> bool {
        self.minute.contains(date.minute())
            && self.hour.contains(date.hour())
            && self.day.contains(date.day())
            && self.month.contains(date.month())
            && self.weekday.contains(date.weekday().num_days_from_sunday())
    }
}

struct CronField {
    values: BTreeSet<u32>,
}

impl CronField {
    fn parse(
        value: &str,
        minimum: u32,
        maximum: u32,
        sunday_alias: bool,
    ) -> Result<Self, SchedulerError> {
        let mut values = BTreeSet::new();
        for part in value.split(',') {
            let (base, step) = part.split_once('/').map_or((part, 1), |(base, step)| {
                (base, step.parse::<u32>().unwrap_or(0))
            });
            if step == 0 {
                return Err(SchedulerError::InvalidSchedule(
                    "invalid calendar step".into(),
                ));
            }
            let (start, end) = if base == "*" {
                (minimum, maximum)
            } else if let Some((start, end)) = base.split_once('-') {
                (
                    parse_cron_value(start, minimum, maximum)?,
                    parse_cron_value(end, minimum, maximum)?,
                )
            } else {
                let value = parse_cron_value(base, minimum, maximum)?;
                (value, value)
            };
            if start > end {
                return Err(SchedulerError::InvalidSchedule(
                    "descending calendar range".into(),
                ));
            }
            for mut candidate in (start..=end)
                .step_by(usize::try_from(step).map_err(|_| SchedulerError::TimeOverflow)?)
            {
                if sunday_alias && candidate == 7 {
                    candidate = 0;
                }
                values.insert(candidate);
            }
        }
        if values.is_empty() {
            Err(SchedulerError::InvalidSchedule(
                "empty calendar field".into(),
            ))
        } else {
            Ok(Self { values })
        }
    }

    fn contains(&self, value: u32) -> bool {
        self.values.contains(&value)
    }
}

fn parse_cron_value(value: &str, minimum: u32, maximum: u32) -> Result<u32, SchedulerError> {
    let value = value
        .parse::<u32>()
        .map_err(|_| SchedulerError::InvalidSchedule("invalid calendar value".into()))?;
    if (minimum..=maximum).contains(&value) {
        Ok(value)
    } else {
        Err(SchedulerError::InvalidSchedule(
            "calendar value is outside its field range".into(),
        ))
    }
}

fn validate_schedule(schedule: &ScheduleSpec) -> Result<(), SchedulerError> {
    match schedule {
        ScheduleSpec::Once { .. } => Ok(()),
        ScheduleSpec::Interval { every_ms, .. } if *every_ms > 0 => Ok(()),
        ScheduleSpec::Interval { .. } => Err(SchedulerError::InvalidSchedule(
            "interval must be non-zero".into(),
        )),
        ScheduleSpec::Calendar {
            expression,
            time_zone,
        } => {
            CalendarExpression::parse(expression)?;
            Tz::from_str(time_zone)
                .map(|_| ())
                .map_err(|_| SchedulerError::InvalidSchedule("invalid IANA time zone".into()))
        }
    }
}

fn validate_action(action: &ActionPayload) -> Result<(), SchedulerError> {
    let valid = match action {
        ActionPayload::Scheduled { instruction }
        | ActionPayload::SystemMaintenance {
            operation: instruction,
        } => !instruction.trim().is_empty(),
        _ => true,
    };
    if valid {
        Ok(())
    } else {
        Err(SchedulerError::InvalidSchedule(
            "scheduled action text must be non-empty".into(),
        ))
    }
}

fn add_millis(time: UtcTimestamp, millis: u64) -> Result<UtcTimestamp, SchedulerError> {
    let millis = i64::try_from(millis).map_err(|_| SchedulerError::TimeOverflow)?;
    time.unix_millis()
        .checked_add(millis)
        .map(UtcTimestamp::from_unix_millis)
        .ok_or(SchedulerError::TimeOverflow)
}

fn next_revision(revision: Revision) -> Result<Revision, SchedulerError> {
    revision.checked_next().ok_or(SchedulerError::TimeOverflow)
}

fn safe_error(error: &str) -> String {
    error.chars().take(512).collect()
}

fn repository_error(error: impl Display) -> SchedulerError {
    SchedulerError::Repository(safe_error(&error.to_string()))
}

fn job_record(job: &ScheduledJob, revision: Revision) -> Result<VersionedRecord, SchedulerError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: job.id.as_entity_id().clone(),
        revision,
        updated_at: job.updated_at,
        payload: serde_json::to_value(job)
            .map_err(|error| SchedulerError::Corrupt(error.to_string()))?,
    })
}

fn attempt_record(
    attempt: &JobAttempt,
    revision: Revision,
) -> Result<VersionedRecord, SchedulerError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: attempt.attempt_id.clone(),
        revision,
        updated_at: attempt.updated_at,
        payload: serde_json::to_value(attempt)
            .map_err(|error| SchedulerError::Corrupt(error.to_string()))?,
    })
}

fn decode_job(record: VersionedRecord) -> Result<StoredJob, SchedulerError> {
    let job = serde_json::from_value::<ScheduledJob>(record.payload)
        .map_err(|error| SchedulerError::Corrupt(error.to_string()))?;
    if job.id.as_entity_id() != &record.id || job.version.major != CURRENT_SCHEMA_VERSION.major {
        return Err(SchedulerError::Corrupt(
            "job identity or schema mismatch".into(),
        ));
    }
    Ok(StoredJob {
        job,
        revision: record.revision,
    })
}

fn decode_attempt(record: VersionedRecord) -> Result<StoredAttempt, SchedulerError> {
    let attempt = serde_json::from_value::<JobAttempt>(record.payload)
        .map_err(|error| SchedulerError::Corrupt(error.to_string()))?;
    if attempt.attempt_id != record.id || attempt.version.major != CURRENT_SCHEMA_VERSION.major {
        return Err(SchedulerError::Corrupt(
            "attempt identity or schema mismatch".into(),
        ));
    }
    Ok(StoredAttempt {
        attempt,
        revision: record.revision,
    })
}

const fn attempt_terminal(state: JobAttemptState) -> bool {
    matches!(
        state,
        JobAttemptState::Completed
            | JobAttemptState::Failed
            | JobAttemptState::Cancelled
            | JobAttemptState::Enqueued
    )
}

#[cfg(test)]
mod tests {
    use std::path::Path;
    use std::thread;

    use keith_action_store::{ActionInboxConfig, ActionState};
    use keith_state_store::EmbeddedStore;
    use tempfile::tempdir;

    use super::*;

    type TestScheduler = Scheduler<EmbeddedStore, PersistentActionInbox<EmbeddedStore>>;

    fn runtime(path: &Path, config: SchedulerConfig) -> TestScheduler {
        runtime_with_inbox(path, config, ActionInboxConfig::default())
    }

    fn runtime_with_inbox(
        path: &Path,
        config: SchedulerConfig,
        inbox_config: ActionInboxConfig,
    ) -> TestScheduler {
        let schedules = Arc::new(EmbeddedStore::open(path, None).unwrap());
        let actions = Arc::new(
            PersistentActionInbox::new(EmbeddedStore::open(path, None).unwrap(), inbox_config)
                .unwrap(),
        );
        Scheduler::new(schedules, actions, config).unwrap()
    }

    fn create_job(
        scheduler: &TestScheduler,
        schedule: ScheduleSpec,
        missed: MissedRunPolicy,
        now: i64,
    ) -> ScheduledJob {
        scheduler
            .create(
                NewScheduledJob {
                    profile_id: ProfileId::new(),
                    session_id: SessionId::new(),
                    schedule,
                    action: ActionPayload::Scheduled {
                        instruction: "run durable work".into(),
                    },
                    limits: ActionLimits::default(),
                    reply_route: Some(ReplyRoute::Session {
                        session_id: SessionId::new(),
                    }),
                    missed_run: missed,
                },
                UtcTimestamp::from_unix_millis(now),
            )
            .unwrap()
    }

    #[test]
    fn interval_claim_is_atomic_stable_and_restart_safe() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let scheduler = runtime(&path, SchedulerConfig::default());
        let job = create_job(
            &scheduler,
            ScheduleSpec::Interval {
                every_ms: 1_000,
                anchor: UtcTimestamp::UNIX_EPOCH,
            },
            MissedRunPolicy::ReplayBounded { max_runs: 2 },
            0,
        );
        drop(scheduler);
        let restarted = runtime(&path, SchedulerConfig::default());
        let attempts = restarted
            .tick(&EntityId::new(), UtcTimestamp::from_unix_millis(3_500))
            .unwrap();
        assert_eq!(attempts.len(), 2);
        assert!(
            attempts
                .iter()
                .all(|attempt| attempt.state == JobAttemptState::Enqueued)
        );
        let action_ids = attempts
            .iter()
            .map(|attempt| attempt.action_id.clone())
            .collect::<BTreeSet<_>>();
        assert_eq!(action_ids.len(), 2);
        let projection = restarted
            .projections()
            .unwrap()
            .into_iter()
            .find(|projection| projection.job_id == job.id)
            .unwrap();
        assert_eq!(
            projection.next_run,
            Some(UtcTimestamp::from_unix_millis(4_000))
        );
        assert_eq!(
            projection.last_run,
            Some(UtcTimestamp::from_unix_millis(3_000))
        );
        assert!(
            restarted
                .tick(&EntityId::new(), UtcTimestamp::from_unix_millis(3_500))
                .unwrap()
                .is_empty()
        );
    }

    #[test]
    fn duplicate_claim_race_enqueues_one_action() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let scheduler = runtime(&path, SchedulerConfig::default());
        let job = create_job(
            &scheduler,
            ScheduleSpec::Once {
                at: UtcTimestamp::UNIX_EPOCH,
            },
            MissedRunPolicy::RunOnce,
            0,
        );
        drop(scheduler);
        let first = runtime(&path, SchedulerConfig::default());
        let second = runtime(&path, SchedulerConfig::default());
        let first = thread::spawn(move || first.tick(&EntityId::new(), UtcTimestamp::UNIX_EPOCH));
        let second = thread::spawn(move || second.tick(&EntityId::new(), UtcTimestamp::UNIX_EPOCH));
        let count = first.join().unwrap().unwrap().len() + second.join().unwrap().unwrap().len();
        assert_eq!(count, 1);
        let inspector = runtime(&path, SchedulerConfig::default());
        let attempts = inspector.load_attempts().unwrap();
        assert_eq!(attempts.len(), 1);
        assert_eq!(attempts[0].attempt.job_id, job.id);
    }

    #[test]
    fn missed_skip_run_once_and_bounded_replay_do_not_storm() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let scheduler = runtime(&path, SchedulerConfig::default());
        let schedule = ScheduleSpec::Interval {
            every_ms: 1_000,
            anchor: UtcTimestamp::UNIX_EPOCH,
        };
        let skipped = create_job(&scheduler, schedule.clone(), MissedRunPolicy::Skip, 0);
        let once = create_job(&scheduler, schedule.clone(), MissedRunPolicy::RunOnce, 0);
        let replay = create_job(
            &scheduler,
            schedule,
            MissedRunPolicy::ReplayBounded { max_runs: 3 },
            0,
        );
        let attempts = scheduler
            .tick(&EntityId::new(), UtcTimestamp::from_unix_millis(10_500))
            .unwrap();
        assert_eq!(attempts.len(), 4);
        assert!(!attempts.iter().any(|attempt| attempt.job_id == skipped.id));
        assert_eq!(
            attempts
                .iter()
                .filter(|attempt| attempt.job_id == once.id)
                .count(),
            1
        );
        assert_eq!(
            attempts
                .iter()
                .filter(|attempt| attempt.job_id == replay.id)
                .count(),
            3
        );
    }

    #[test]
    fn calendar_time_zones_handle_dst_gap_and_overlap_monotonically() {
        let spring = Utc.with_ymd_and_hms(2026, 3, 7, 7, 0, 0).single().unwrap();
        let next = next_calendar(
            "30 2 * * *",
            "America/New_York",
            UtcTimestamp::from_unix_millis(spring.timestamp_millis()),
        )
        .unwrap();
        let next_utc = Utc
            .timestamp_millis_opt(next.unix_millis())
            .single()
            .unwrap();
        assert_eq!(
            next_utc,
            Utc.with_ymd_and_hms(2026, 3, 7, 7, 30, 0).unwrap()
        );
        let after = next_calendar("30 2 * * *", "America/New_York", next).unwrap();
        let after_utc = Utc
            .timestamp_millis_opt(after.unix_millis())
            .single()
            .unwrap();
        assert_eq!(
            after_utc,
            Utc.with_ymd_and_hms(2026, 3, 9, 6, 30, 0).unwrap()
        );

        let fall = Utc.with_ymd_and_hms(2026, 11, 1, 4, 0, 0).single().unwrap();
        let first = next_calendar(
            "30 1 * * *",
            "America/New_York",
            UtcTimestamp::from_unix_millis(fall.timestamp_millis()),
        )
        .unwrap();
        let second = next_calendar("30 1 * * *", "America/New_York", first).unwrap();
        assert_eq!(second.unix_millis() - first.unix_millis(), 3_600_000);
    }

    #[test]
    fn pause_update_delete_failure_projection_and_action_delivery_are_real() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let scheduler = runtime(&path, SchedulerConfig::default());
        let job = create_job(
            &scheduler,
            ScheduleSpec::Once {
                at: UtcTimestamp::from_unix_millis(1_000),
            },
            MissedRunPolicy::RunOnce,
            0,
        );
        scheduler
            .pause(&job.id, UtcTimestamp::from_unix_millis(100))
            .unwrap();
        assert!(
            scheduler
                .tick(&EntityId::new(), UtcTimestamp::from_unix_millis(2_000))
                .unwrap()
                .is_empty()
        );
        scheduler
            .update(
                &job.id,
                JobUpdate {
                    schedule: None,
                    action: Some(ActionPayload::Scheduled {
                        instruction: "updated work".into(),
                    }),
                    limits: None,
                    reply_route: None,
                    missed_run: None,
                },
                UtcTimestamp::from_unix_millis(2_000),
            )
            .unwrap();
        scheduler
            .resume(&job.id, UtcTimestamp::from_unix_millis(2_000))
            .unwrap();
        let attempt = scheduler
            .tick(&EntityId::new(), UtcTimestamp::from_unix_millis(2_000))
            .unwrap()
            .pop()
            .unwrap();
        let inbox_record = scheduler.sink.get(&attempt.action_id).unwrap().unwrap();
        assert_eq!(inbox_record.state, ActionState::Queued);
        scheduler
            .finish_attempt(
                &attempt.attempt_id,
                false,
                Some("provider unavailable".into()),
                UtcTimestamp::from_unix_millis(2_100),
            )
            .unwrap();
        let projection = scheduler.projections().unwrap().pop().unwrap();
        assert_eq!(projection.failures, 1);
        assert_eq!(
            projection.safe_error.as_deref(),
            Some("provider unavailable")
        );
        scheduler
            .delete(&job.id, UtcTimestamp::from_unix_millis(2_200))
            .unwrap();
        assert!(scheduler.projections().unwrap().is_empty());
    }

    #[test]
    fn enqueue_failure_retries_the_same_action_after_backoff() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("runtime.sqlite");
        let scheduler = runtime_with_inbox(
            &path,
            SchedulerConfig::default(),
            ActionInboxConfig {
                max_queued_per_session: 1,
                max_background_queued_per_session: 1,
            },
        );
        let job = create_job(
            &scheduler,
            ScheduleSpec::Once {
                at: UtcTimestamp::UNIX_EPOCH,
            },
            MissedRunPolicy::RunOnce,
            0,
        );
        let occupying = SessionAction {
            id: ActionId::new(),
            session_id: job.session_id.clone(),
            source: ActionSource::FollowUp,
            delivery: DeliveryPolicy::Immediate,
            priority: ActionPriority::User,
            created_at: UtcTimestamp::UNIX_EPOCH,
            not_before: None,
            deadline: None,
            limits: ActionLimits::default(),
            reply_route: None,
            payload: ActionPayload::FollowUp {
                text: "occupy queue".into(),
            },
        };
        scheduler
            .sink
            .submit(occupying.clone(), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let first = scheduler
            .tick(&EntityId::new(), UtcTimestamp::UNIX_EPOCH)
            .unwrap()
            .pop()
            .unwrap();
        assert_eq!(first.state, JobAttemptState::RetryScheduled);
        assert_eq!(first.retry_count, 1);
        let stable_action = first.action_id.clone();
        scheduler
            .sink
            .cancel(
                &occupying.id,
                UtcTimestamp::from_unix_millis(1),
                "queue released",
            )
            .unwrap();
        let retried = scheduler
            .tick(&EntityId::new(), UtcTimestamp::from_unix_millis(5_000))
            .unwrap()
            .pop()
            .unwrap();
        assert_eq!(retried.state, JobAttemptState::Enqueued);
        assert_eq!(retried.action_id, stable_action);
        assert!(scheduler.sink.get(&stable_action).unwrap().is_some());
    }
}
