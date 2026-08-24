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
    ActionId, ArtifactId, CURRENT_SCHEMA_VERSION, ConversationId, EntityId, EventId, JobId,
    ProfileId, Revision, SchemaVersion, SessionId, StableKey, UtcTimestamp,
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

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum RoutineTrigger {
    Schedule {
        schedule: ScheduleSpec,
        time_zone: String,
    },
    Event {
        event_kind: String,
        source_conversation_id: Option<ConversationId>,
        bounds: RoutineEventBounds,
    },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RoutineEventBounds {
    pub max_recursion_depth: u16,
    pub min_interval_ms: u64,
    pub max_runs_per_window: u16,
    pub window_ms: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum RoutineInvocation {
    Prompt {
        prompt: String,
    },
    Skill {
        skill_version_id: EntityId,
        inputs: BTreeMap<String, String>,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RoutineApprovalBoundary {
    pub policy_revision: Revision,
    pub allow_unattended: bool,
    pub required_approval_keys: BTreeSet<StableKey>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RoutineApprovalSnapshot {
    pub policy_revision: Revision,
    pub approval_keys: BTreeSet<StableKey>,
    pub captured_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RoutineState {
    Enabled,
    Paused,
    Completed,
    Failed,
    Cancelled,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum RoutineRunSource {
    Schedule {
        scheduled_for: UtcTimestamp,
    },
    Event {
        source_event_id: EventId,
        stable_source_key: StableKey,
        source_conversation_id: ConversationId,
        event_kind: String,
        policy_revision: Revision,
        recursion_depth: u16,
        occurred_at: UtcTimestamp,
    },
    Manual,
    Test,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RoutineRunState {
    Claimed,
    Enqueued,
    Completed,
    Failed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum RoutinePublicationState {
    Pending,
    SuppressedTest,
    Published,
    Failed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RoutinePublication {
    pub stable_key: StableKey,
    pub destination_conversation_id: ConversationId,
    pub state: RoutinePublicationState,
    pub event_id: Option<EventId>,
    pub artifacts: Vec<ArtifactId>,
    pub safe_error: Option<String>,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RoutineRunRecord {
    pub run_id: EntityId,
    pub attempt_id: EntityId,
    pub ordinal: u32,
    pub source: RoutineRunSource,
    pub approval: RoutineApprovalSnapshot,
    pub state: RoutineRunState,
    pub publication: RoutinePublication,
    pub started_at: UtcTimestamp,
    pub finished_at: Option<UtcTimestamp>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct OwnedRoutine {
    pub version: SchemaVersion,
    pub id: JobId,
    pub owner_profile_id: ProfileId,
    pub participant_session_id: SessionId,
    pub destination_conversation_id: ConversationId,
    pub trigger: RoutineTrigger,
    pub invocation: RoutineInvocation,
    pub approval_boundary: RoutineApprovalBoundary,
    pub approval_snapshot: RoutineApprovalSnapshot,
    pub state: RoutineState,
    pub revision: Revision,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub history: Vec<RoutineRunRecord>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NewOwnedRoutine {
    pub owner_profile_id: ProfileId,
    pub participant_session_id: SessionId,
    pub destination_conversation_id: ConversationId,
    pub trigger: RoutineTrigger,
    pub invocation: RoutineInvocation,
    pub approval_boundary: RoutineApprovalBoundary,
    pub approval_snapshot: RoutineApprovalSnapshot,
    pub limits: ActionLimits,
    pub missed_run: MissedRunPolicy,
    pub enabled: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RoutineUpdate {
    pub expected_revision: Revision,
    pub trigger: Option<RoutineTrigger>,
    pub destination_conversation_id: Option<ConversationId>,
    pub invocation: Option<RoutineInvocation>,
    pub approval_boundary: Option<RoutineApprovalBoundary>,
    pub approval_snapshot: Option<RoutineApprovalSnapshot>,
    pub limits: Option<ActionLimits>,
    pub missed_run: Option<MissedRunPolicy>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RoutineEvent {
    pub source_event_id: EventId,
    pub stable_source_key: StableKey,
    pub source_conversation_id: ConversationId,
    pub event_kind: String,
    pub policy_revision: Revision,
    pub recursion_depth: u16,
    pub occurred_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum RoutineRunDisposition {
    Accepted,
    Duplicate,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RoutineRunReceipt {
    pub routine_id: JobId,
    pub run: RoutineRunRecord,
    pub disposition: RoutineRunDisposition,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RoutinePublicationIntent {
    pub routine_id: JobId,
    pub run_id: EntityId,
    pub attempt_id: EntityId,
    pub action_id: ActionId,
    pub owner_profile_id: ProfileId,
    pub participant_session_id: SessionId,
    pub destination_conversation_id: ConversationId,
    pub stable_publication_key: StableKey,
    pub source: RoutineRunSource,
    pub approval: RoutineApprovalSnapshot,
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
    #[serde(default)]
    pub ownership_history: Vec<ScheduleOwnershipTransfer>,
    #[serde(default)]
    pub routine: Option<OwnedRoutine>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ScheduleOwnershipTransfer {
    pub stable_key: String,
    pub from_profile_id: ProfileId,
    pub to_profile_id: ProfileId,
    pub expected_revision: Revision,
    pub resulting_revision: Revision,
    pub authority_snapshot_key: String,
    pub transferred_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileScheduleTransferRequest {
    pub stable_key: String,
    pub from_profile_id: ProfileId,
    pub to_profile_id: ProfileId,
    pub expected_revisions: BTreeMap<JobId, Revision>,
    pub authority_snapshot_key: String,
    pub transferred_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileScheduleTransferReceipt {
    pub stable_key: String,
    pub transferred_jobs: Vec<JobId>,
    pub duplicate: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileScheduleProvision {
    pub profile_id: ProfileId,
    pub created_jobs: BTreeMap<JobId, Revision>,
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
pub struct ProfileScheduleResources {
    pub profile_id: ProfileId,
    pub jobs: Vec<JobId>,
    pub nonterminal_attempts: Vec<EntityId>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileScheduleDeletionInventory {
    pub profile_id: ProfileId,
    pub stable_key: String,
    pub jobs: BTreeMap<JobId, Revision>,
    pub attempts: BTreeMap<EntityId, Revision>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileScheduleEraseReport {
    pub profile_id: ProfileId,
    pub deleted_jobs: usize,
    pub deleted_attempts: usize,
    pub duplicate: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileScheduleLeakScan {
    pub profile_id: ProfileId,
    pub remaining_jobs: Vec<JobId>,
    pub remaining_attempts: Vec<EntityId>,
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
    #[error("routine authority or approval snapshot is not valid")]
    RoutineAuthorityDenied,
    #[error("routine event does not match its durable trigger")]
    RoutineEventMismatch,
    #[error("routine recursion bound was exceeded")]
    RoutineRecursionExceeded,
    #[error("routine event rate bound was exceeded")]
    RoutineRateExceeded,
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
            ownership_history: Vec::new(),
            routine: None,
        };
        self.repository
            .put_schedule(
                job_record(&job, Revision::ZERO)?,
                WritePrecondition::Missing,
            )
            .map_err(repository_error)?;
        Ok(job)
    }

    /// Creates one job and returns an exact compensation token for lifecycle provisioning.
    /// # Errors
    /// Returns the same validation or persistence errors as [`Self::create`].
    pub fn create_provisioned(
        &self,
        request: NewScheduledJob,
        now: UtcTimestamp,
    ) -> Result<(ScheduledJob, ProfileScheduleProvision), SchedulerError> {
        let profile_id = request.profile_id.clone();
        let job = self.create(request, now)?;
        Ok((
            job.clone(),
            ProfileScheduleProvision {
                profile_id,
                created_jobs: BTreeMap::from([(job.id.clone(), Revision::ZERO)]),
            },
        ))
    }

    /// Creates one durable, profile-owned routine in the same repository used by scheduler ticks.
    ///
    /// # Errors
    ///
    /// Rejects invalid triggers, invocations, approval snapshots, or persistence failures.
    pub fn create_routine(
        &self,
        request: NewOwnedRoutine,
        now: UtcTimestamp,
    ) -> Result<OwnedRoutine, SchedulerError> {
        validate_routine_trigger(&request.trigger)?;
        validate_routine_invocation(&request.invocation)?;
        validate_routine_approval(&request.approval_boundary, &request.approval_snapshot)?;
        let id = JobId::new();
        let (schedule, next_run) = match &request.trigger {
            RoutineTrigger::Schedule { schedule, .. } => {
                (schedule.clone(), initial_run(schedule, now)?)
            }
            RoutineTrigger::Event { .. } => (ScheduleSpec::Once { at: now }, None),
        };
        let state = if request.enabled {
            RoutineState::Enabled
        } else {
            RoutineState::Paused
        };
        let routine = OwnedRoutine {
            version: CURRENT_SCHEMA_VERSION,
            id: id.clone(),
            owner_profile_id: request.owner_profile_id.clone(),
            participant_session_id: request.participant_session_id.clone(),
            destination_conversation_id: request.destination_conversation_id,
            trigger: request.trigger,
            invocation: request.invocation,
            approval_boundary: request.approval_boundary,
            approval_snapshot: request.approval_snapshot,
            state,
            revision: Revision::ZERO,
            created_at: now,
            updated_at: now,
            history: Vec::new(),
        };
        let job = ScheduledJob {
            version: CURRENT_SCHEMA_VERSION,
            id,
            profile_id: routine.owner_profile_id.clone(),
            session_id: routine.participant_session_id.clone(),
            schedule,
            action: routine_action(&routine.invocation)?,
            limits: request.limits,
            reply_route: None,
            missed_run: request.missed_run,
            state: if request.enabled {
                JobState::Active
            } else {
                JobState::Paused
            },
            next_run,
            last_run: None,
            attempt_count: 0,
            failure_count: 0,
            safe_error: None,
            created_at: now,
            updated_at: now,
            ownership_history: Vec::new(),
            routine: Some(routine.clone()),
        };
        self.repository
            .put_schedule(
                job_record(&job, Revision::ZERO)?,
                WritePrecondition::Missing,
            )
            .map_err(repository_error)?;
        Ok(routine)
    }

    /// Returns an exact revisioned routine projection from durable storage.
    ///
    /// # Errors
    ///
    /// Returns an error when the job is absent, is not a routine, or is corrupt.
    pub fn routine(&self, routine_id: &JobId) -> Result<OwnedRoutine, SchedulerError> {
        self.required_routine_job(routine_id)?
            .job
            .routine
            .ok_or(SchedulerError::NotFound)
    }

    /// Lists all durable routines in stable identity order.
    ///
    /// # Errors
    ///
    /// Returns an error when any scheduled record is corrupt.
    pub fn routines(&self) -> Result<Vec<OwnedRoutine>, SchedulerError> {
        let mut routines = self
            .load_jobs()?
            .into_iter()
            .filter_map(|stored| stored.job.routine)
            .collect::<Vec<_>>();
        routines.sort_by(|left, right| left.id.cmp(&right.id));
        Ok(routines)
    }

    /// Revisionally edits mutable routine policy while retaining owner and session identity.
    ///
    /// # Errors
    ///
    /// Rejects invalid or stale durable state and invalid trigger, invocation, or approval data.
    pub fn update_routine(
        &self,
        routine_id: &JobId,
        update: RoutineUpdate,
        now: UtcTimestamp,
    ) -> Result<OwnedRoutine, SchedulerError> {
        let _guard = self.lock()?;
        let mut stored = self.required_routine_job(routine_id)?;
        if stored.revision != update.expected_revision {
            return Err(SchedulerError::InvalidTransition);
        }
        if matches!(stored.job.state, JobState::Completed | JobState::Cancelled) {
            return Err(SchedulerError::InvalidTransition);
        }
        let routine = stored
            .job
            .routine
            .as_mut()
            .ok_or(SchedulerError::NotFound)?;
        if let Some(trigger) = update.trigger {
            validate_routine_trigger(&trigger)?;
            match &trigger {
                RoutineTrigger::Schedule { schedule, .. } => {
                    stored.job.schedule = schedule.clone();
                    stored.job.next_run = initial_run(schedule, now)?;
                }
                RoutineTrigger::Event { .. } => stored.job.next_run = None,
            }
            routine.trigger = trigger;
        }
        if let Some(destination) = update.destination_conversation_id {
            routine.destination_conversation_id = destination;
        }
        if let Some(invocation) = update.invocation {
            validate_routine_invocation(&invocation)?;
            stored.job.action = routine_action(&invocation)?;
            routine.invocation = invocation;
        }
        if let Some(boundary) = update.approval_boundary {
            routine.approval_boundary = boundary;
        }
        if let Some(snapshot) = update.approval_snapshot {
            routine.approval_snapshot = snapshot;
        }
        validate_routine_approval(&routine.approval_boundary, &routine.approval_snapshot)?;
        if let Some(limits) = update.limits {
            stored.job.limits = limits;
        }
        if let Some(missed_run) = update.missed_run {
            stored.job.missed_run = missed_run;
        }
        stored.job.updated_at = now;
        self.put_job(&mut stored)?;
        stored.job.routine.ok_or(SchedulerError::NotFound)
    }

    /// Executes a routine immediately and creates a canonical publication intent.
    ///
    /// # Errors
    ///
    /// Rejects paused routines, invalid current approvals, or persistence/delivery failures.
    pub fn run_routine_now(
        &self,
        routine_id: &JobId,
        approval: RoutineApprovalSnapshot,
        now: UtcTimestamp,
    ) -> Result<RoutineRunReceipt, SchedulerError> {
        self.run_routine_manually(routine_id, RoutineRunSource::Manual, approval, now)
    }

    /// Executes the real action path while suppressing canonical publication of its result.
    ///
    /// # Errors
    ///
    /// Rejects paused routines, invalid current approvals, or persistence/delivery failures.
    pub fn test_routine(
        &self,
        routine_id: &JobId,
        approval: RoutineApprovalSnapshot,
        now: UtcTimestamp,
    ) -> Result<RoutineRunReceipt, SchedulerError> {
        self.run_routine_manually(routine_id, RoutineRunSource::Test, approval, now)
    }

    /// Applies one external event to an event-triggered routine with durable source-key dedup.
    ///
    /// # Errors
    ///
    /// Rejects trigger mismatches, recursion/rate violations, revoked authority, or store errors.
    pub fn trigger_routine_event(
        &self,
        routine_id: &JobId,
        event: RoutineEvent,
        now: UtcTimestamp,
    ) -> Result<RoutineRunReceipt, SchedulerError> {
        let _guard = self.lock()?;
        let mut stored = self.required_routine_job(routine_id)?;
        let routine = stored
            .job
            .routine
            .as_ref()
            .ok_or(SchedulerError::NotFound)?;
        if let Some(run) = routine.history.iter().find(|run| {
            matches!(
                &run.source,
                RoutineRunSource::Event { stable_source_key, .. }
                    if stable_source_key == &event.stable_source_key
            )
        }) {
            return Ok(RoutineRunReceipt {
                routine_id: routine_id.clone(),
                run: run.clone(),
                disposition: RoutineRunDisposition::Duplicate,
            });
        }
        if stored.job.state != JobState::Active {
            return Err(SchedulerError::InvalidTransition);
        }
        let (event_kind, source_conversation_id, bounds) = match &routine.trigger {
            RoutineTrigger::Event {
                event_kind,
                source_conversation_id,
                bounds,
            } => (event_kind, source_conversation_id, *bounds),
            RoutineTrigger::Schedule { .. } => return Err(SchedulerError::RoutineEventMismatch),
        };
        if event_kind != &event.event_kind
            || source_conversation_id
                .as_ref()
                .is_some_and(|expected| expected != &event.source_conversation_id)
        {
            return Err(SchedulerError::RoutineEventMismatch);
        }
        if event.recursion_depth > bounds.max_recursion_depth {
            return Err(SchedulerError::RoutineRecursionExceeded);
        }
        enforce_event_rate(routine, bounds, now)?;
        validate_automatic_routine_authority(routine)?;
        let dedup_key = event.stable_source_key.clone();
        let source = RoutineRunSource::Event {
            source_event_id: event.source_event_id,
            stable_source_key: event.stable_source_key,
            source_conversation_id: event.source_conversation_id,
            event_kind: event.event_kind,
            policy_revision: event.policy_revision,
            recursion_depth: event.recursion_depth,
            occurred_at: event.occurred_at,
        };
        let approval = routine.approval_snapshot.clone();
        let attempt = match self.claim_routine_occurrence(&mut stored, source, approval, now) {
            Ok(attempt) => attempt,
            Err(SchedulerError::Repository(error)) => {
                let replay = self.routine(routine_id)?.history.into_iter().find(|run| {
                    matches!(
                        &run.source,
                        RoutineRunSource::Event { stable_source_key, .. }
                            if stable_source_key == &dedup_key
                    )
                });
                if let Some(run) = replay {
                    return Ok(RoutineRunReceipt {
                        routine_id: routine_id.clone(),
                        run,
                        disposition: RoutineRunDisposition::Duplicate,
                    });
                }
                return Err(SchedulerError::Repository(error));
            }
            Err(error) => return Err(error),
        };
        let attempt = self.enqueue_attempt(attempt, now)?;
        self.routine_run_receipt(
            routine_id,
            &attempt.attempt_id,
            RoutineRunDisposition::Accepted,
        )
    }

    /// Enables a paused routine without changing its retained due occurrence.
    ///
    /// # Errors
    ///
    /// Rejects invalid authority, a non-paused routine, or persistence failure.
    pub fn enable_routine(
        &self,
        routine_id: &JobId,
        now: UtcTimestamp,
    ) -> Result<OwnedRoutine, SchedulerError> {
        self.change_routine_state(routine_id, RoutineState::Enabled, now)
    }

    /// Pauses an enabled routine.
    ///
    /// # Errors
    ///
    /// Rejects a non-enabled routine or persistence failure.
    pub fn pause_routine(
        &self,
        routine_id: &JobId,
        now: UtcTimestamp,
    ) -> Result<OwnedRoutine, SchedulerError> {
        self.change_routine_state(routine_id, RoutineState::Paused, now)
    }

    /// Resumes a paused routine.
    ///
    /// # Errors
    ///
    /// Rejects invalid authority, a non-paused routine, or persistence failure.
    pub fn resume_routine(
        &self,
        routine_id: &JobId,
        now: UtcTimestamp,
    ) -> Result<OwnedRoutine, SchedulerError> {
        self.enable_routine(routine_id, now)
    }

    /// Applies an exact-revision enabled/paused transition for remote management callers.
    ///
    /// # Errors
    ///
    /// Rejects stale revisions, terminal/illegal transitions, or persistence failures.
    pub fn transition_routine(
        &self,
        routine_id: &JobId,
        expected_revision: Revision,
        state: RoutineState,
        now: UtcTimestamp,
    ) -> Result<OwnedRoutine, SchedulerError> {
        let _guard = self.lock()?;
        let mut stored = self.required_routine_job(routine_id)?;
        if stored.revision != expected_revision {
            return Err(SchedulerError::InvalidTransition);
        }
        let next = match state {
            RoutineState::Enabled => JobState::Active,
            RoutineState::Paused => JobState::Paused,
            RoutineState::Completed | RoutineState::Failed | RoutineState::Cancelled => {
                return Err(SchedulerError::InvalidTransition);
            }
        };
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
        stored.job.routine.ok_or(SchedulerError::NotFound)
    }

    /// Returns the complete durable run history for one routine.
    ///
    /// # Errors
    ///
    /// Returns an error when the routine is missing or corrupt.
    pub fn routine_history(
        &self,
        routine_id: &JobId,
    ) -> Result<Vec<RoutineRunRecord>, SchedulerError> {
        Ok(self.routine(routine_id)?.history)
    }

    /// Lists restart-safe canonical publication intents whose execution has completed.
    ///
    /// # Errors
    ///
    /// Returns an error when routine or attempt identity is missing or corrupt.
    pub fn pending_routine_publications(
        &self,
    ) -> Result<Vec<RoutinePublicationIntent>, SchedulerError> {
        let attempts = self
            .load_attempts()?
            .into_iter()
            .map(|stored| (stored.attempt.attempt_id.clone(), stored.attempt))
            .collect::<BTreeMap<_, _>>();
        let mut intents = Vec::new();
        for routine in self.routines()? {
            for run in routine.history.iter().filter(|run| {
                run.state == RoutineRunState::Completed
                    && run.publication.state == RoutinePublicationState::Pending
            }) {
                let attempt = attempts.get(&run.attempt_id).ok_or_else(|| {
                    SchedulerError::Corrupt("routine publication attempt is missing".into())
                })?;
                intents.push(RoutinePublicationIntent {
                    routine_id: routine.id.clone(),
                    run_id: run.run_id.clone(),
                    attempt_id: run.attempt_id.clone(),
                    action_id: attempt.action_id.clone(),
                    owner_profile_id: routine.owner_profile_id.clone(),
                    participant_session_id: routine.participant_session_id.clone(),
                    destination_conversation_id: run
                        .publication
                        .destination_conversation_id
                        .clone(),
                    stable_publication_key: run.publication.stable_key.clone(),
                    source: run.source.clone(),
                    approval: run.approval.clone(),
                });
            }
        }
        intents.sort_by(|left, right| {
            left.routine_id
                .cmp(&right.routine_id)
                .then_with(|| left.run_id.cmp(&right.run_id))
        });
        Ok(intents)
    }

    /// Deletes one routine and cancels its nonterminal attempts atomically.
    ///
    /// # Errors
    ///
    /// Returns an error when the routine is missing/corrupt or deletion fails.
    pub fn delete_routine(
        &self,
        routine_id: &JobId,
        now: UtcTimestamp,
    ) -> Result<(), SchedulerError> {
        self.required_routine_job(routine_id)?;
        self.delete(routine_id, now)
    }

    /// Records the canonical event and artifacts produced for one durable routine run.
    ///
    /// # Errors
    ///
    /// Rejects test runs, conflicting publication replay, missing runs, or store failures.
    pub fn record_routine_publication(
        &self,
        routine_id: &JobId,
        run_id: &EntityId,
        event_id: EventId,
        artifacts: Vec<ArtifactId>,
        now: UtcTimestamp,
    ) -> Result<RoutineRunRecord, SchedulerError> {
        let _guard = self.lock()?;
        let mut stored = self.required_routine_job(routine_id)?;
        let routine = stored
            .job
            .routine
            .as_mut()
            .ok_or(SchedulerError::NotFound)?;
        let run = routine
            .history
            .iter_mut()
            .find(|run| &run.run_id == run_id)
            .ok_or(SchedulerError::NotFound)?;
        if run.publication.state == RoutinePublicationState::SuppressedTest {
            return Err(SchedulerError::InvalidTransition);
        }
        if run.publication.state == RoutinePublicationState::Published {
            if run.publication.event_id.as_ref() == Some(&event_id)
                && run.publication.artifacts == artifacts
            {
                return Ok(run.clone());
            }
            return Err(SchedulerError::InvalidTransition);
        }
        run.publication.state = RoutinePublicationState::Published;
        run.publication.event_id = Some(event_id);
        run.publication.artifacts = artifacts;
        run.publication.safe_error = None;
        run.publication.updated_at = now;
        stored.job.updated_at = now;
        let result = run.clone();
        self.put_job(&mut stored)?;
        Ok(result)
    }

    /// Records a bounded canonical-publication failure for durable retry/inspection.
    ///
    /// # Errors
    ///
    /// Rejects test or already-published runs, missing identities, or persistence failures.
    pub fn fail_routine_publication(
        &self,
        routine_id: &JobId,
        run_id: &EntityId,
        error: &str,
        now: UtcTimestamp,
    ) -> Result<RoutineRunRecord, SchedulerError> {
        let _guard = self.lock()?;
        let mut stored = self.required_routine_job(routine_id)?;
        let run = stored
            .job
            .routine
            .as_mut()
            .and_then(|routine| routine.history.iter_mut().find(|run| &run.run_id == run_id))
            .ok_or(SchedulerError::NotFound)?;
        if matches!(
            run.publication.state,
            RoutinePublicationState::SuppressedTest | RoutinePublicationState::Published
        ) {
            return Err(SchedulerError::InvalidTransition);
        }
        run.publication.state = RoutinePublicationState::Failed;
        run.publication.safe_error = Some(safe_error(error));
        run.publication.updated_at = now;
        let result = run.clone();
        stored.job.updated_at = now;
        self.put_job(&mut stored)?;
        Ok(result)
    }

    /// Pauses a routine whose durable authority snapshot was revoked or superseded.
    ///
    /// # Errors
    ///
    /// Rejects stale policy revisions or persistence failures.
    pub fn revoke_routine_authority(
        &self,
        routine_id: &JobId,
        expected_policy_revision: Revision,
        now: UtcTimestamp,
    ) -> Result<OwnedRoutine, SchedulerError> {
        let _guard = self.lock()?;
        let mut stored = self.required_routine_job(routine_id)?;
        let routine = stored
            .job
            .routine
            .as_mut()
            .ok_or(SchedulerError::NotFound)?;
        if routine.approval_boundary.policy_revision != expected_policy_revision {
            return Err(SchedulerError::InvalidTransition);
        }
        stored.job.state = JobState::Paused;
        stored.job.safe_error = Some("routine authority was revoked".into());
        stored.job.updated_at = now;
        routine.state = RoutineState::Paused;
        self.put_job(&mut stored)?;
        stored.job.routine.ok_or(SchedulerError::NotFound)
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
        if stored.job.routine.is_some() {
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

    /// Reconciles the durable schedule inventory owned by one profile without creating ambient jobs.
    /// # Errors
    /// Returns an error when persisted jobs or attempts are corrupt.
    pub fn reconcile_profile_resources(
        &self,
        profile_id: &ProfileId,
    ) -> Result<ProfileScheduleResources, SchedulerError> {
        let _guard = self.lock()?;
        let jobs = self
            .load_jobs()?
            .into_iter()
            .filter(|stored| &stored.job.profile_id == profile_id)
            .map(|stored| stored.job.id)
            .collect::<BTreeSet<_>>();
        let nonterminal_attempts = self
            .load_attempts()?
            .into_iter()
            .filter(|attempt| {
                jobs.contains(&attempt.attempt.job_id) && !attempt_terminal(attempt.attempt.state)
            })
            .map(|attempt| attempt.attempt.attempt_id)
            .collect();
        Ok(ProfileScheduleResources {
            profile_id: profile_id.clone(),
            jobs: jobs.into_iter().collect(),
            nonterminal_attempts,
        })
    }

    /// Enumerates every schedule and run-attempt record owned by one profile.
    /// # Errors
    /// Returns an error when persisted records are corrupt.
    pub fn enumerate_profile_deletion_inventory(
        &self,
        profile_id: &ProfileId,
    ) -> Result<ProfileScheduleDeletionInventory, SchedulerError> {
        let _guard = self.lock()?;
        let jobs = self
            .load_jobs()?
            .into_iter()
            .filter(|stored| &stored.job.profile_id == profile_id)
            .map(|stored| (stored.job.id, stored.revision))
            .collect::<BTreeMap<_, _>>();
        let attempts = self
            .load_attempts()?
            .into_iter()
            .filter(|attempt| jobs.contains_key(&attempt.attempt.job_id))
            .map(|attempt| (attempt.attempt.attempt_id, attempt.revision))
            .collect::<BTreeMap<_, _>>();
        let stable_key = schedule_inventory_key(profile_id, &jobs, &attempts);
        Ok(ProfileScheduleDeletionInventory {
            profile_id: profile_id.clone(),
            stable_key,
            jobs,
            attempts,
        })
    }

    /// Atomically erases the exact revision-bound schedule inventory.
    /// # Errors
    /// Rejects a stale inventory or backend transaction failure.
    pub fn erase_profile_deletion_inventory(
        &self,
        inventory: &ProfileScheduleDeletionInventory,
    ) -> Result<ProfileScheduleEraseReport, SchedulerError> {
        let current = self.enumerate_profile_deletion_inventory(&inventory.profile_id)?;
        if current.jobs.is_empty() && current.attempts.is_empty() {
            return Ok(ProfileScheduleEraseReport {
                profile_id: inventory.profile_id.clone(),
                deleted_jobs: 0,
                deleted_attempts: 0,
                duplicate: true,
            });
        }
        if &current != inventory {
            return Err(SchedulerError::InvalidTransition);
        }
        let mut mutations = Vec::with_capacity(inventory.jobs.len() + inventory.attempts.len());
        for (attempt_id, revision) in &inventory.attempts {
            mutations.push(RecordMutation::Delete {
                collection: Collection::JobAttempts,
                id: attempt_id.clone(),
                precondition: WritePrecondition::Exact(*revision),
            });
        }
        for (job_id, revision) in &inventory.jobs {
            mutations.push(RecordMutation::Delete {
                collection: Collection::ScheduledJobs,
                id: job_id.as_entity_id().clone(),
                precondition: WritePrecondition::Exact(*revision),
            });
        }
        self.repository
            .transact(&mutations)
            .map_err(repository_error)?;
        Ok(ProfileScheduleEraseReport {
            profile_id: inventory.profile_id.clone(),
            deleted_jobs: inventory.jobs.len(),
            deleted_attempts: inventory.attempts.len(),
            duplicate: false,
        })
    }

    /// # Errors
    /// Returns an error when remaining durable records are corrupt.
    pub fn scan_profile_schedule_leaks(
        &self,
        profile_id: &ProfileId,
    ) -> Result<ProfileScheduleLeakScan, SchedulerError> {
        let inventory = self.enumerate_profile_deletion_inventory(profile_id)?;
        Ok(ProfileScheduleLeakScan {
            profile_id: profile_id.clone(),
            remaining_jobs: inventory.jobs.into_keys().collect(),
            remaining_attempts: inventory.attempts.into_keys().collect(),
        })
    }

    /// Atomically deletes every profile-owned schedule and cancels its nonterminal attempts.
    /// Replaying after a committed deletion is a successful no-op.
    /// # Errors
    /// Returns an error when durable state is corrupt or the single transaction fails.
    pub fn delete_profile_resources(
        &self,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<ProfileScheduleResources, SchedulerError> {
        let _guard = self.lock()?;
        let jobs = self
            .load_jobs()?
            .into_iter()
            .filter(|stored| &stored.job.profile_id == profile_id)
            .collect::<Vec<_>>();
        let job_ids = jobs
            .iter()
            .map(|stored| stored.job.id.clone())
            .collect::<BTreeSet<_>>();
        let mut attempts = self
            .load_attempts()?
            .into_iter()
            .filter(|attempt| {
                job_ids.contains(&attempt.attempt.job_id)
                    && !attempt_terminal(attempt.attempt.state)
            })
            .collect::<Vec<_>>();
        let disposition = ProfileScheduleResources {
            profile_id: profile_id.clone(),
            jobs: job_ids.iter().cloned().collect(),
            nonterminal_attempts: attempts
                .iter()
                .map(|attempt| attempt.attempt.attempt_id.clone())
                .collect(),
        };
        let mut mutations = Vec::with_capacity(jobs.len().saturating_add(attempts.len()));
        for attempt in &mut attempts {
            attempt.attempt.state = JobAttemptState::Cancelled;
            attempt.attempt.updated_at = now;
            mutations.push(RecordMutation::Put {
                collection: Collection::JobAttempts,
                record: attempt_record(&attempt.attempt, next_revision(attempt.revision)?)?,
                precondition: WritePrecondition::Exact(attempt.revision),
            });
        }
        for stored in jobs {
            mutations.push(RecordMutation::Delete {
                collection: Collection::ScheduledJobs,
                id: stored.job.id.as_entity_id().clone(),
                precondition: WritePrecondition::Exact(stored.revision),
            });
        }
        if !mutations.is_empty() {
            self.repository
                .transact(&mutations)
                .map_err(repository_error)?;
        }
        Ok(disposition)
    }

    /// Transfers all currently owned schedules with one revisioned, replay-safe transaction.
    /// Only ownership and append-only transfer provenance change; executable authority is frozen.
    /// # Errors
    /// Rejects stale revisions, incomplete job sets, reused keys, self-transfer, or backend failure.
    pub fn transfer_profile_resources(
        &self,
        request: &ProfileScheduleTransferRequest,
    ) -> Result<ProfileScheduleTransferReceipt, SchedulerError> {
        validate_transfer_key(&request.stable_key)?;
        validate_transfer_key(&request.authority_snapshot_key)?;
        if request.from_profile_id == request.to_profile_id {
            return Err(SchedulerError::InvalidTransition);
        }
        let _guard = self.lock()?;
        let all = self.load_jobs()?;
        let replayed = all
            .iter()
            .filter(|stored| {
                stored.job.profile_id == request.to_profile_id
                    && stored.job.ownership_history.last().is_some_and(|transfer| {
                        transfer.stable_key == request.stable_key
                            && transfer.from_profile_id == request.from_profile_id
                            && transfer.to_profile_id == request.to_profile_id
                            && transfer.authority_snapshot_key == request.authority_snapshot_key
                    })
            })
            .map(|stored| stored.job.id.clone())
            .collect::<Vec<_>>();
        let mut owned = all
            .into_iter()
            .filter(|stored| stored.job.profile_id == request.from_profile_id)
            .collect::<Vec<_>>();
        if owned.is_empty() && !replayed.is_empty() {
            return Ok(ProfileScheduleTransferReceipt {
                stable_key: request.stable_key.clone(),
                transferred_jobs: replayed,
                duplicate: true,
            });
        }
        let owned_ids = owned
            .iter()
            .map(|stored| stored.job.id.clone())
            .collect::<BTreeSet<_>>();
        if owned_ids != request.expected_revisions.keys().cloned().collect()
            || owned.iter().any(|stored| {
                request.expected_revisions.get(&stored.job.id) != Some(&stored.revision)
                    || stored
                        .job
                        .ownership_history
                        .iter()
                        .any(|transfer| transfer.stable_key == request.stable_key)
            })
        {
            return Err(SchedulerError::InvalidTransition);
        }
        let mut mutations = Vec::with_capacity(owned.len());
        let mut transferred_jobs = Vec::with_capacity(owned.len());
        for stored in &mut owned {
            let next = next_revision(stored.revision)?;
            stored.job.profile_id = request.to_profile_id.clone();
            stored.job.updated_at = request.transferred_at;
            stored
                .job
                .ownership_history
                .push(ScheduleOwnershipTransfer {
                    stable_key: request.stable_key.clone(),
                    from_profile_id: request.from_profile_id.clone(),
                    to_profile_id: request.to_profile_id.clone(),
                    expected_revision: stored.revision,
                    resulting_revision: next,
                    authority_snapshot_key: request.authority_snapshot_key.clone(),
                    transferred_at: request.transferred_at,
                });
            sync_routine_job(&mut stored.job, next);
            mutations.push(RecordMutation::Put {
                collection: Collection::ScheduledJobs,
                record: job_record(&stored.job, next)?,
                precondition: WritePrecondition::Exact(stored.revision),
            });
            transferred_jobs.push(stored.job.id.clone());
        }
        if !mutations.is_empty() {
            self.repository
                .transact(&mutations)
                .map_err(repository_error)?;
        }
        Ok(ProfileScheduleTransferReceipt {
            stable_key: request.stable_key.clone(),
            transferred_jobs,
            duplicate: false,
        })
    }

    /// Compensates only untouched jobs named by a provisioning token in one transaction.
    /// Replaying after successful compensation is a no-op.
    /// # Errors
    /// Rejects any job whose owner, revision, or run-attempt state changed after provisioning.
    pub fn rollback_profile_schedule_provision(
        &self,
        provision: &ProfileScheduleProvision,
    ) -> Result<usize, SchedulerError> {
        let _guard = self.lock()?;
        let attempts = self.load_attempts()?;
        let mut mutations = Vec::new();
        for (job_id, expected_revision) in &provision.created_jobs {
            let Some(stored) = self
                .repository
                .get_schedule(job_id.as_entity_id())
                .map_err(repository_error)?
                .map(decode_job)
                .transpose()?
            else {
                continue;
            };
            if stored.job.profile_id != provision.profile_id
                || stored.revision != *expected_revision
                || attempts
                    .iter()
                    .any(|attempt| attempt.attempt.job_id == *job_id)
            {
                return Err(SchedulerError::InvalidTransition);
            }
            mutations.push(RecordMutation::Delete {
                collection: Collection::ScheduledJobs,
                id: job_id.as_entity_id().clone(),
                precondition: WritePrecondition::Exact(*expected_revision),
            });
        }
        let removed = mutations.len();
        if !mutations.is_empty() {
            self.repository
                .transact(&mutations)
                .map_err(repository_error)?;
        }
        Ok(removed)
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
        let mut job = self.required_job(&attempt.attempt.job_id)?;
        if let Some(routine) = job.job.routine.as_mut() {
            let run = routine
                .history
                .iter_mut()
                .find(|run| run.attempt_id == attempt.attempt.attempt_id)
                .ok_or_else(|| SchedulerError::Corrupt("routine run history is missing".into()))?;
            run.state = if succeeded {
                RoutineRunState::Completed
            } else {
                RoutineRunState::Failed
            };
            run.finished_at = Some(now);
            if !succeeded {
                run.publication.state = RoutinePublicationState::Failed;
                run.publication.safe_error.clone_from(&safe_error);
                run.publication.updated_at = now;
            }
        }
        if !succeeded {
            job.job.failure_count = job.job.failure_count.saturating_add(1);
            job.job.safe_error = safe_error.clone();
        }
        if !succeeded || job.job.routine.is_some() {
            job.job.updated_at = now;
            let job_revision = next_revision(job.revision)?;
            sync_routine_job(&mut job.job, job_revision);
            mutations.push(RecordMutation::Put {
                collection: Collection::ScheduledJobs,
                record: job_record(&job.job, job_revision)?,
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

    fn run_routine_manually(
        &self,
        routine_id: &JobId,
        source: RoutineRunSource,
        approval: RoutineApprovalSnapshot,
        now: UtcTimestamp,
    ) -> Result<RoutineRunReceipt, SchedulerError> {
        let _guard = self.lock()?;
        let mut stored = self.required_routine_job(routine_id)?;
        let test_run = source == RoutineRunSource::Test;
        if stored.job.state != JobState::Active
            && !(test_run && stored.job.state == JobState::Paused)
        {
            return Err(SchedulerError::InvalidTransition);
        }
        let routine = stored
            .job
            .routine
            .as_ref()
            .ok_or(SchedulerError::NotFound)?;
        validate_routine_approval(&routine.approval_boundary, &approval)?;
        let attempt = self.claim_routine_occurrence(&mut stored, source, approval, now)?;
        let attempt = self.enqueue_attempt(attempt, now)?;
        self.routine_run_receipt(
            routine_id,
            &attempt.attempt_id,
            RoutineRunDisposition::Accepted,
        )
    }

    fn claim_routine_occurrence(
        &self,
        stored: &mut StoredJob,
        source: RoutineRunSource,
        approval: RoutineApprovalSnapshot,
        now: UtcTimestamp,
    ) -> Result<StoredAttempt, SchedulerError> {
        stored.job.attempt_count = stored
            .job
            .attempt_count
            .checked_add(1)
            .ok_or(SchedulerError::TimeOverflow)?;
        let attempt_id = EntityId::new();
        let run_id = EntityId::new();
        let action_id = ActionId::new();
        let ordinal = stored.job.attempt_count;
        let attempt = JobAttempt {
            version: CURRENT_SCHEMA_VERSION,
            job_id: stored.job.id.clone(),
            attempt_id: attempt_id.clone(),
            ordinal,
            scheduled_for: now,
            claimed_by: run_id.clone(),
            claim_expires: add_millis(now, self.config.claim_ttl_ms)?,
            state: JobAttemptState::Claimed,
            action_id,
            retry_count: 0,
            retry_at: None,
            safe_error: None,
            updated_at: now,
        };
        let routine = stored
            .job
            .routine
            .as_mut()
            .ok_or(SchedulerError::NotFound)?;
        let publication_state = if source == RoutineRunSource::Test {
            RoutinePublicationState::SuppressedTest
        } else {
            RoutinePublicationState::Pending
        };
        let publication_key = StableKey::parse(format!("routine:{}:run:{ordinal}", routine.id))
            .map_err(|error| SchedulerError::InvalidSchedule(error.to_string()))?;
        routine.history.push(RoutineRunRecord {
            run_id,
            attempt_id: attempt_id.clone(),
            ordinal,
            source,
            approval,
            state: RoutineRunState::Claimed,
            publication: RoutinePublication {
                stable_key: publication_key,
                destination_conversation_id: routine.destination_conversation_id.clone(),
                state: publication_state,
                event_id: None,
                artifacts: Vec::new(),
                safe_error: None,
                updated_at: now,
            },
            started_at: now,
            finished_at: None,
        });
        stored.job.last_run = Some(now);
        stored.job.updated_at = now;
        let revision = next_revision(stored.revision)?;
        sync_routine_job(&mut stored.job, revision);
        self.repository
            .transact(&[
                RecordMutation::Put {
                    collection: Collection::ScheduledJobs,
                    record: job_record(&stored.job, revision)?,
                    precondition: WritePrecondition::Exact(stored.revision),
                },
                RecordMutation::Put {
                    collection: Collection::JobAttempts,
                    record: attempt_record(&attempt, Revision::ZERO)?,
                    precondition: WritePrecondition::Missing,
                },
            ])
            .map_err(repository_error)?;
        stored.revision = revision;
        Ok(StoredAttempt {
            attempt,
            revision: Revision::ZERO,
        })
    }

    fn routine_run_receipt(
        &self,
        routine_id: &JobId,
        attempt_id: &EntityId,
        disposition: RoutineRunDisposition,
    ) -> Result<RoutineRunReceipt, SchedulerError> {
        let routine = self.routine(routine_id)?;
        let run = routine
            .history
            .into_iter()
            .find(|run| &run.attempt_id == attempt_id)
            .ok_or_else(|| SchedulerError::Corrupt("routine run history is missing".into()))?;
        Ok(RoutineRunReceipt {
            routine_id: routine_id.clone(),
            run,
            disposition,
        })
    }

    fn change_routine_state(
        &self,
        routine_id: &JobId,
        state: RoutineState,
        now: UtcTimestamp,
    ) -> Result<OwnedRoutine, SchedulerError> {
        let job_state = match state {
            RoutineState::Enabled => JobState::Active,
            RoutineState::Paused => JobState::Paused,
            RoutineState::Completed => JobState::Completed,
            RoutineState::Failed => JobState::Failed,
            RoutineState::Cancelled => JobState::Cancelled,
        };
        self.required_routine_job(routine_id)?;
        self.change_state(routine_id, job_state, now)?;
        self.routine(routine_id)
    }

    fn required_routine_job(&self, routine_id: &JobId) -> Result<StoredJob, SchedulerError> {
        let stored = self.required_job(routine_id)?;
        if stored.job.routine.is_none() {
            return Err(SchedulerError::NotFound);
        }
        Ok(stored)
    }

    fn claim_job(
        &self,
        mut stored: StoredJob,
        claimant: &EntityId,
        now: UtcTimestamp,
    ) -> Result<Vec<StoredAttempt>, SchedulerError> {
        if let Some(routine) = stored.job.routine.as_ref() {
            if validate_automatic_routine_authority(routine).is_err() {
                stored.job.state = JobState::Paused;
                stored.job.safe_error =
                    Some("routine approval is not valid for unattended use".into());
                stored.job.updated_at = now;
                self.put_job(&mut stored)?;
                return Ok(Vec::new());
            }
        }
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
            if let Some(routine) = stored.job.routine.as_mut() {
                let approval = routine.approval_snapshot.clone();
                append_routine_run(
                    routine,
                    &attempt,
                    RoutineRunSource::Schedule { scheduled_for },
                    approval,
                    now,
                )?;
            }
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
        let revision = next_revision(stored.revision)?;
        sync_routine_job(&mut stored.job, revision);
        mutations.insert(
            0,
            RecordMutation::Put {
                collection: Collection::ScheduledJobs,
                record: job_record(&stored.job, revision)?,
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
        let mut job = self.required_job(&stored.attempt.job_id)?;
        let action = SessionAction {
            id: stored.attempt.action_id.clone(),
            session_id: job.job.session_id.clone(),
            source: ActionSource::Schedule {
                job_id: job.job.id.clone(),
                attempt: stored.attempt.ordinal,
            },
            delivery: DeliveryPolicy::Immediate,
            priority: ActionPriority::Scheduled,
            created_at: now,
            not_before: None,
            deadline: None,
            limits: job.job.limits,
            reply_route: job.job.reply_route.clone(),
            payload: job.job.action.clone(),
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
        if let Some(routine) = job.job.routine.as_mut() {
            let run = routine
                .history
                .iter_mut()
                .find(|run| run.attempt_id == stored.attempt.attempt_id)
                .ok_or_else(|| SchedulerError::Corrupt("routine run history is missing".into()))?;
            run.state = match stored.attempt.state {
                JobAttemptState::Enqueued => RoutineRunState::Enqueued,
                JobAttemptState::Failed => RoutineRunState::Failed,
                JobAttemptState::Claimed
                | JobAttemptState::RetryScheduled
                | JobAttemptState::Completed
                | JobAttemptState::Cancelled => RoutineRunState::Claimed,
            };
            if stored.attempt.state == JobAttemptState::Failed {
                run.finished_at = Some(now);
                run.publication.state = RoutinePublicationState::Failed;
                run.publication
                    .safe_error
                    .clone_from(&stored.attempt.safe_error);
                run.publication.updated_at = now;
            }
            job.job.updated_at = now;
            let attempt_revision = next_revision(stored.revision)?;
            let job_revision = next_revision(job.revision)?;
            sync_routine_job(&mut job.job, job_revision);
            self.repository
                .transact(&[
                    RecordMutation::Put {
                        collection: Collection::JobAttempts,
                        record: attempt_record(&stored.attempt, attempt_revision)?,
                        precondition: WritePrecondition::Exact(stored.revision),
                    },
                    RecordMutation::Put {
                        collection: Collection::ScheduledJobs,
                        record: job_record(&job.job, job_revision)?,
                        precondition: WritePrecondition::Exact(job.revision),
                    },
                ])
                .map_err(repository_error)?;
            stored.revision = attempt_revision;
        } else {
            self.put_attempt(&mut stored)?;
        }
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
        sync_routine_job(&mut stored.job, revision);
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

fn validate_routine_trigger(trigger: &RoutineTrigger) -> Result<(), SchedulerError> {
    match trigger {
        RoutineTrigger::Schedule {
            schedule,
            time_zone,
        } => {
            validate_schedule(schedule)?;
            Tz::from_str(time_zone)
                .map_err(|_| SchedulerError::InvalidSchedule("invalid IANA time zone".into()))?;
            if let ScheduleSpec::Calendar {
                time_zone: schedule_zone,
                ..
            } = schedule
            {
                if schedule_zone != time_zone {
                    return Err(SchedulerError::InvalidSchedule(
                        "routine and calendar time zones differ".into(),
                    ));
                }
            }
            Ok(())
        }
        RoutineTrigger::Event {
            event_kind, bounds, ..
        } => {
            if event_kind.trim().is_empty()
                || event_kind.len() > 128
                || bounds.max_runs_per_window == 0
                || bounds.window_ms == 0
                || bounds.min_interval_ms > bounds.window_ms
            {
                return Err(SchedulerError::InvalidSchedule(
                    "event trigger bounds are invalid".into(),
                ));
            }
            Ok(())
        }
    }
}

fn validate_routine_invocation(invocation: &RoutineInvocation) -> Result<(), SchedulerError> {
    const MAX_PROMPT_BYTES: usize = 64 * 1024;
    const MAX_SKILL_INPUTS: usize = 128;
    const MAX_SKILL_INPUT_BYTES: usize = 16 * 1024;
    let valid = match invocation {
        RoutineInvocation::Prompt { prompt } => {
            !prompt.trim().is_empty() && prompt.len() <= MAX_PROMPT_BYTES
        }
        RoutineInvocation::Skill {
            skill_version_id: _,
            inputs,
        } => {
            inputs.len() <= MAX_SKILL_INPUTS
                && inputs.iter().all(|(key, value)| {
                    !key.trim().is_empty()
                        && key.len() <= 128
                        && value.len() <= MAX_SKILL_INPUT_BYTES
                })
        }
    };
    if valid {
        Ok(())
    } else {
        Err(SchedulerError::InvalidSchedule(
            "routine invocation is empty or exceeds a bound".into(),
        ))
    }
}

fn routine_action(invocation: &RoutineInvocation) -> Result<ActionPayload, SchedulerError> {
    let instruction = match invocation {
        RoutineInvocation::Prompt { prompt } => prompt.clone(),
        RoutineInvocation::Skill { .. } => format!(
            "keith:routine-skill:v1:{}",
            serde_json::to_string(invocation)
                .map_err(|error| SchedulerError::Corrupt(error.to_string()))?
        ),
    };
    Ok(ActionPayload::Scheduled { instruction })
}

fn validate_routine_approval(
    boundary: &RoutineApprovalBoundary,
    snapshot: &RoutineApprovalSnapshot,
) -> Result<(), SchedulerError> {
    if boundary.policy_revision != snapshot.policy_revision
        || !boundary
            .required_approval_keys
            .is_subset(&snapshot.approval_keys)
    {
        return Err(SchedulerError::RoutineAuthorityDenied);
    }
    Ok(())
}

fn validate_automatic_routine_authority(routine: &OwnedRoutine) -> Result<(), SchedulerError> {
    validate_routine_approval(&routine.approval_boundary, &routine.approval_snapshot)?;
    if !routine.approval_boundary.allow_unattended {
        return Err(SchedulerError::RoutineAuthorityDenied);
    }
    Ok(())
}

fn enforce_event_rate(
    routine: &OwnedRoutine,
    bounds: RoutineEventBounds,
    now: UtcTimestamp,
) -> Result<(), SchedulerError> {
    let window_ms = i64::try_from(bounds.window_ms).map_err(|_| SchedulerError::TimeOverflow)?;
    let window_start = now
        .unix_millis()
        .checked_sub(window_ms)
        .ok_or(SchedulerError::TimeOverflow)?;
    let recent = routine
        .history
        .iter()
        .filter(|run| {
            matches!(&run.source, RoutineRunSource::Event { .. })
                && run.started_at.unix_millis() >= window_start
        })
        .collect::<Vec<_>>();
    if recent.len() >= usize::from(bounds.max_runs_per_window) {
        return Err(SchedulerError::RoutineRateExceeded);
    }
    if recent.last().is_some_and(|run| {
        now.unix_millis()
            .checked_sub(run.started_at.unix_millis())
            .is_none_or(|elapsed| {
                elapsed < i64::try_from(bounds.min_interval_ms).unwrap_or(i64::MAX)
            })
    }) {
        return Err(SchedulerError::RoutineRateExceeded);
    }
    Ok(())
}

fn append_routine_run(
    routine: &mut OwnedRoutine,
    attempt: &JobAttempt,
    source: RoutineRunSource,
    approval: RoutineApprovalSnapshot,
    now: UtcTimestamp,
) -> Result<(), SchedulerError> {
    let publication_state = if source == RoutineRunSource::Test {
        RoutinePublicationState::SuppressedTest
    } else {
        RoutinePublicationState::Pending
    };
    let stable_key = StableKey::parse(format!("routine:{}:run:{}", routine.id, attempt.ordinal))
        .map_err(|error| SchedulerError::InvalidSchedule(error.to_string()))?;
    routine.history.push(RoutineRunRecord {
        run_id: EntityId::new(),
        attempt_id: attempt.attempt_id.clone(),
        ordinal: attempt.ordinal,
        source,
        approval,
        state: RoutineRunState::Claimed,
        publication: RoutinePublication {
            stable_key,
            destination_conversation_id: routine.destination_conversation_id.clone(),
            state: publication_state,
            event_id: None,
            artifacts: Vec::new(),
            safe_error: None,
            updated_at: now,
        },
        started_at: now,
        finished_at: None,
    });
    Ok(())
}

fn sync_routine_job(job: &mut ScheduledJob, revision: Revision) {
    if let Some(routine) = job.routine.as_mut() {
        routine.id = job.id.clone();
        routine.owner_profile_id.clone_from(&job.profile_id);
        routine.participant_session_id.clone_from(&job.session_id);
        routine.state = match job.state {
            JobState::Active => RoutineState::Enabled,
            JobState::Paused => RoutineState::Paused,
            JobState::Completed => RoutineState::Completed,
            JobState::Failed => RoutineState::Failed,
            JobState::Cancelled => RoutineState::Cancelled,
        };
        routine.revision = revision;
        routine.updated_at = job.updated_at;
    }
}

fn validate_persisted_routine(
    job: &ScheduledJob,
    routine: &OwnedRoutine,
    revision: Revision,
) -> Result<(), SchedulerError> {
    validate_routine_trigger(&routine.trigger)?;
    validate_routine_invocation(&routine.invocation)?;
    validate_routine_approval(&routine.approval_boundary, &routine.approval_snapshot)?;
    let expected_state = match job.state {
        JobState::Active => RoutineState::Enabled,
        JobState::Paused => RoutineState::Paused,
        JobState::Completed => RoutineState::Completed,
        JobState::Failed => RoutineState::Failed,
        JobState::Cancelled => RoutineState::Cancelled,
    };
    if routine.version.major != CURRENT_SCHEMA_VERSION.major
        || routine.id != job.id
        || routine.owner_profile_id != job.profile_id
        || routine.participant_session_id != job.session_id
        || routine.state != expected_state
        || routine.revision != revision
        || routine.created_at != job.created_at
        || routine.updated_at != job.updated_at
        || routine_action(&routine.invocation)? != job.action
    {
        return Err(SchedulerError::Corrupt(
            "routine identity, revision, authority, or invocation drifted".into(),
        ));
    }
    match &routine.trigger {
        RoutineTrigger::Schedule { schedule, .. } if schedule != &job.schedule => {
            return Err(SchedulerError::Corrupt(
                "routine schedule differs from its scheduler authority".into(),
            ));
        }
        RoutineTrigger::Event { .. } if job.next_run.is_some() => {
            return Err(SchedulerError::Corrupt(
                "event routine has an ambient time occurrence".into(),
            ));
        }
        RoutineTrigger::Schedule { .. } | RoutineTrigger::Event { .. } => {}
    }
    let mut run_ids = BTreeSet::new();
    let mut attempt_ids = BTreeSet::new();
    let mut ordinals = BTreeSet::new();
    let mut source_keys = BTreeSet::new();
    for run in &routine.history {
        if !run_ids.insert(run.run_id.clone())
            || !attempt_ids.insert(run.attempt_id.clone())
            || !ordinals.insert(run.ordinal)
            || run.ordinal == 0
            || run.ordinal > job.attempt_count
            || run.publication.updated_at < run.started_at
            || run
                .finished_at
                .is_some_and(|finished| finished < run.started_at)
        {
            return Err(SchedulerError::Corrupt(
                "routine run identity or ordering is invalid".into(),
            ));
        }
        if let RoutineRunSource::Event {
            stable_source_key, ..
        } = &run.source
        {
            if !source_keys.insert(stable_source_key.clone()) {
                return Err(SchedulerError::Corrupt(
                    "routine event source key is duplicated".into(),
                ));
            }
        }
        let publication_shape_valid = match run.publication.state {
            RoutinePublicationState::Pending | RoutinePublicationState::SuppressedTest => {
                run.publication.event_id.is_none()
            }
            RoutinePublicationState::Published => run.publication.event_id.is_some(),
            RoutinePublicationState::Failed => true,
        };
        if !publication_shape_valid
            || (run.publication.state == RoutinePublicationState::SuppressedTest
                && run.source != RoutineRunSource::Test)
        {
            return Err(SchedulerError::Corrupt(
                "routine publication state is inconsistent".into(),
            ));
        }
    }
    Ok(())
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

fn validate_transfer_key(value: &str) -> Result<(), SchedulerError> {
    if !value.is_empty()
        && value.len() <= 192
        && value.bytes().all(|byte| {
            byte.is_ascii_alphanumeric() || matches!(byte, b'.' | b'_' | b':' | b'/' | b'-')
        })
    {
        Ok(())
    } else {
        Err(SchedulerError::InvalidTransition)
    }
}

fn schedule_inventory_key(
    profile_id: &ProfileId,
    jobs: &BTreeMap<JobId, Revision>,
    attempts: &BTreeMap<EntityId, Revision>,
) -> String {
    let mut first = 0xcbf2_9ce4_8422_2325_u64;
    let mut second = 0x8422_2325_cbf2_9ce4_u64;
    for byte in profile_id
        .to_string()
        .bytes()
        .chain(jobs.iter().flat_map(|(id, revision)| {
            id.to_string()
                .into_bytes()
                .into_iter()
                .chain(revision.get().to_be_bytes())
        }))
        .chain(attempts.iter().flat_map(|(id, revision)| {
            id.to_string()
                .into_bytes()
                .into_iter()
                .chain(revision.get().to_be_bytes())
        }))
    {
        first = (first ^ u64::from(byte)).wrapping_mul(0x100_0000_01b3);
        second = (second ^ u64::from(byte)).wrapping_mul(0x100_0000_01b3);
    }
    format!("schedule-delete:{first:016x}{second:016x}")
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
    let mut keys = BTreeSet::new();
    if job
        .ownership_history
        .iter()
        .enumerate()
        .any(|(index, transfer)| {
            validate_transfer_key(&transfer.stable_key).is_err()
                || validate_transfer_key(&transfer.authority_snapshot_key).is_err()
                || transfer.from_profile_id == transfer.to_profile_id
                || transfer.expected_revision.checked_next() != Some(transfer.resulting_revision)
                || !keys.insert(transfer.stable_key.as_str())
                || index > 0
                    && job.ownership_history[index - 1].to_profile_id != transfer.from_profile_id
        })
        || job
            .ownership_history
            .last()
            .is_some_and(|transfer| transfer.to_profile_id != job.profile_id)
    {
        return Err(SchedulerError::Corrupt(
            "job ownership history is invalid".into(),
        ));
    }
    if let Some(routine) = &job.routine {
        validate_persisted_routine(&job, routine, record.revision)?;
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
    fn agent_lifecycle_profile_schedule_reconciliation_and_delete_are_restart_safe() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("lifecycle.sqlite");
        let profile = ProfileId::from(EntityId::from_u128(700));
        let scheduler = runtime(&path, SchedulerConfig::default());
        let job = scheduler
            .create(
                NewScheduledJob {
                    profile_id: profile.clone(),
                    session_id: SessionId::new(),
                    schedule: ScheduleSpec::Once {
                        at: UtcTimestamp(100),
                    },
                    action: ActionPayload::Scheduled {
                        instruction: "owned work".into(),
                    },
                    limits: ActionLimits::default(),
                    reply_route: None,
                    missed_run: MissedRunPolicy::RunOnce,
                },
                UtcTimestamp(1),
            )
            .unwrap();
        assert_eq!(
            scheduler
                .reconcile_profile_resources(&profile)
                .unwrap()
                .jobs,
            vec![job.id]
        );
        let removed = scheduler
            .delete_profile_resources(&profile, UtcTimestamp(2))
            .unwrap();
        assert_eq!(removed.jobs.len(), 1);
        assert!(
            scheduler
                .delete_profile_resources(&profile, UtcTimestamp(3))
                .unwrap()
                .jobs
                .is_empty()
        );
        drop(scheduler);
        assert!(
            runtime(&path, SchedulerConfig::default())
                .reconcile_profile_resources(&profile)
                .unwrap()
                .jobs
                .is_empty()
        );
    }

    #[test]
    fn agent_lifecycle_schedule_transfer_is_revisioned_replay_safe_and_authority_frozen() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("transfer.sqlite");
        let source = ProfileId::from(EntityId::from_u128(710));
        let destination = ProfileId::from(EntityId::from_u128(711));
        let scheduler = runtime(&path, SchedulerConfig::default());
        let job = scheduler
            .create(
                NewScheduledJob {
                    profile_id: source.clone(),
                    session_id: SessionId::new(),
                    schedule: ScheduleSpec::Once {
                        at: UtcTimestamp(100),
                    },
                    action: ActionPayload::Scheduled {
                        instruction: "bounded owned work".into(),
                    },
                    limits: ActionLimits::default(),
                    reply_route: None,
                    missed_run: MissedRunPolicy::RunOnce,
                },
                UtcTimestamp(1),
            )
            .unwrap();
        let original_action = job.action.clone();
        let original_limits = job.limits;
        let request = ProfileScheduleTransferRequest {
            stable_key: "profile-delete:710:transfer".into(),
            from_profile_id: source.clone(),
            to_profile_id: destination.clone(),
            expected_revisions: BTreeMap::from([(job.id.clone(), Revision::ZERO)]),
            authority_snapshot_key: "policy:destination:7".into(),
            transferred_at: UtcTimestamp(2),
        };
        let receipt = scheduler.transfer_profile_resources(&request).unwrap();
        assert!(!receipt.duplicate);
        drop(scheduler);
        let restarted = runtime(&path, SchedulerConfig::default());
        let replay = restarted.transfer_profile_resources(&request).unwrap();
        assert!(replay.duplicate);
        let stored = restarted.required_job(&job.id).unwrap().job;
        assert_eq!(stored.profile_id, destination);
        assert_eq!(stored.action, original_action);
        assert_eq!(stored.limits, original_limits);
        assert_eq!(stored.ownership_history.len(), 1);

        let stale = ProfileScheduleTransferRequest {
            stable_key: "profile-delete:711:stale".into(),
            from_profile_id: stored.profile_id,
            to_profile_id: source,
            expected_revisions: BTreeMap::from([(job.id, Revision::ZERO)]),
            authority_snapshot_key: "policy:source:8".into(),
            transferred_at: UtcTimestamp(3),
        };
        assert!(matches!(
            restarted.transfer_profile_resources(&stale),
            Err(SchedulerError::InvalidTransition)
        ));
    }

    #[test]
    fn agent_lifecycle_schedule_create_compensation_is_exact_and_replay_safe() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("compensate.sqlite");
        let profile = ProfileId::from(EntityId::from_u128(720));
        let scheduler = runtime(&path, SchedulerConfig::default());
        let (job, provision) = scheduler
            .create_provisioned(
                NewScheduledJob {
                    profile_id: profile.clone(),
                    session_id: SessionId::new(),
                    schedule: ScheduleSpec::Once {
                        at: UtcTimestamp(100),
                    },
                    action: ActionPayload::Scheduled {
                        instruction: "new resource".into(),
                    },
                    limits: ActionLimits::default(),
                    reply_route: None,
                    missed_run: MissedRunPolicy::RunOnce,
                },
                UtcTimestamp(1),
            )
            .unwrap();
        assert_eq!(
            scheduler
                .rollback_profile_schedule_provision(&provision)
                .unwrap(),
            1
        );
        assert_eq!(
            scheduler
                .rollback_profile_schedule_provision(&provision)
                .unwrap(),
            0
        );
        assert!(scheduler.required_job(&job.id).is_err());

        let (changed, changed_provision) = scheduler
            .create_provisioned(
                NewScheduledJob {
                    profile_id: profile,
                    session_id: SessionId::new(),
                    schedule: ScheduleSpec::Once {
                        at: UtcTimestamp(200),
                    },
                    action: ActionPayload::Scheduled {
                        instruction: "changed resource".into(),
                    },
                    limits: ActionLimits::default(),
                    reply_route: None,
                    missed_run: MissedRunPolicy::RunOnce,
                },
                UtcTimestamp(2),
            )
            .unwrap();
        scheduler.pause(&changed.id, UtcTimestamp(3)).unwrap();
        assert!(matches!(
            scheduler.rollback_profile_schedule_provision(&changed_provision),
            Err(SchedulerError::InvalidTransition)
        ));
    }

    #[test]
    fn agent_lifecycle_schedule_inventory_erases_runs_and_proves_no_restart_leak() {
        let directory = tempdir().unwrap();
        let path = directory.path().join("erase-inventory.sqlite");
        let profile = ProfileId::from(EntityId::from_u128(730));
        let scheduler = runtime(&path, SchedulerConfig::default());
        scheduler
            .create(
                NewScheduledJob {
                    profile_id: profile.clone(),
                    session_id: SessionId::new(),
                    schedule: ScheduleSpec::Once {
                        at: UtcTimestamp::UNIX_EPOCH,
                    },
                    action: ActionPayload::Scheduled {
                        instruction: "erase me".into(),
                    },
                    limits: ActionLimits::default(),
                    reply_route: None,
                    missed_run: MissedRunPolicy::RunOnce,
                },
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        scheduler
            .tick(&EntityId::new(), UtcTimestamp::UNIX_EPOCH)
            .unwrap();
        let inventory = scheduler
            .enumerate_profile_deletion_inventory(&profile)
            .unwrap();
        assert_eq!(inventory.jobs.len(), 1);
        assert_eq!(inventory.attempts.len(), 1);
        let report = scheduler
            .erase_profile_deletion_inventory(&inventory)
            .unwrap();
        assert_eq!((report.deleted_jobs, report.deleted_attempts), (1, 1));
        assert!(
            scheduler
                .erase_profile_deletion_inventory(&inventory)
                .unwrap()
                .duplicate
        );
        drop(scheduler);
        let restarted = runtime(&path, SchedulerConfig::default());
        let scan = restarted.scan_profile_schedule_leaks(&profile).unwrap();
        assert!(scan.remaining_jobs.is_empty() && scan.remaining_attempts.is_empty());
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
