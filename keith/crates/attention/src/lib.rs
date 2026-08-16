#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Display;
use std::fs;
use std::path::{Path, PathBuf};

use chrono::{Datelike, Timelike, Utc};
use chrono_tz::Tz;
use keith_action_store::{
    ActionLimits, ActionPayload, ActionPriority, ActionRecord, ActionSource, DeliveryPolicy,
    PersistentActionInbox, SessionAction,
};
use keith_agent_types::{ActionId, ProfileId, SessionId, UtcTimestamp};
use keith_initiative::{CandidateError, InitiativeCandidate};
use keith_state_store_core::ActionRepository;
use serde::{Deserialize, Serialize};
use thiserror::Error;

const LEDGER_FILE: &str = "attention.json";

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AutonomyMode {
    Disabled,
    RememberOnly,
    Suggest,
    Bounded,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum Workload {
    Idle,
    Background,
    Interactive,
    Saturated,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct QuietHours {
    pub time_zone: String,
    pub start_minute: u16,
    pub end_minute: u16,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AttentionConfig {
    pub quiet_hours: Option<QuietHours>,
    pub duplicate_window_ms: i64,
    pub notification_budgets: BTreeMap<String, u32>,
    pub user_priorities: BTreeMap<String, i32>,
    pub max_candidates: usize,
    pub max_history: usize,
    pub action_limits: ActionLimits,
}

impl Default for AttentionConfig {
    fn default() -> Self {
        Self {
            quiet_hours: None,
            duplicate_window_ms: 24 * 60 * 60 * 1_000,
            notification_budgets: BTreeMap::new(),
            user_priorities: BTreeMap::new(),
            max_candidates: 256,
            max_history: 512,
            action_limits: ActionLimits {
                max_turns: Some(1),
                max_tokens: Some(8_000),
                max_elapsed_ms: Some(5 * 60 * 1_000),
                max_tool_calls: Some(8),
                max_children: Some(0),
            },
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AttentionDecision {
    Ignore,
    Remember,
    Batch,
    Schedule,
    Ask,
    StartBoundedWork,
    Notify,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DecisionRecord {
    pub candidate_id: keith_agent_types::EntityId,
    pub awareness_event_id: keith_agent_types::EntityId,
    pub decision: AttentionDecision,
    pub score: i64,
    pub reasons: Vec<String>,
    pub decided_at: UtcTimestamp,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct AttentionLedger {
    profile_id: ProfileId,
    candidates: Vec<InitiativeCandidate>,
    decisions: Vec<DecisionRecord>,
    notification_counts: BTreeMap<String, u32>,
}

#[derive(Debug, Error)]
pub enum AttentionError {
    #[error("initiative candidate is invalid: {0}")]
    Candidate(#[from] CandidateError),
    #[error("attention configuration is invalid")]
    InvalidConfiguration,
    #[error("attention candidate belongs to another profile")]
    ProfileMismatch,
    #[error("attention action submission failed: {0}")]
    Action(String),
    #[error("attention persistence failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("attention state JSON failed: {0}")]
    Json(#[from] serde_json::Error),
}

pub struct AttentionService<R> {
    root: PathBuf,
    profile_id: ProfileId,
    config: AttentionConfig,
    quiet_zone: Option<Tz>,
    inbox: PersistentActionInbox<R>,
    ledger: AttentionLedger,
}

impl<R> AttentionService<R>
where
    R: ActionRepository,
    R::Error: Display,
{
    /// Opens bounded decision history and recovers any recorded work submission.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid policy, profile mismatch, or persistence failure.
    pub fn open(
        root: impl AsRef<Path>,
        profile_id: ProfileId,
        config: AttentionConfig,
        inbox: PersistentActionInbox<R>,
        now: UtcTimestamp,
    ) -> Result<Self, AttentionError> {
        let quiet_zone = validate_config(&config)?;
        fs::create_dir_all(root.as_ref())?;
        let root = fs::canonicalize(root.as_ref())?;
        let path = root.join(LEDGER_FILE);
        let ledger = if path.exists() {
            let ledger: AttentionLedger = serde_json::from_slice(&fs::read(path)?)?;
            if ledger.profile_id != profile_id {
                return Err(AttentionError::ProfileMismatch);
            }
            ledger
        } else {
            AttentionLedger {
                profile_id: profile_id.clone(),
                candidates: Vec::new(),
                decisions: Vec::new(),
                notification_counts: BTreeMap::new(),
            }
        };
        let mut service = Self {
            root,
            profile_id,
            config,
            quiet_zone,
            inbox,
            ledger,
        };
        service.recover_started(now)?;
        Ok(service)
    }

    pub fn recent_candidates(&self) -> &[InitiativeCandidate] {
        &self.ledger.candidates
    }

    pub fn decision_history(&self) -> &[DecisionRecord] {
        &self.ledger.decisions
    }

    /// Evaluates only supplied real candidates; empty input creates no history.
    ///
    /// # Errors
    ///
    /// Returns an error if any candidate or resulting durable action fails validation.
    pub fn evaluate(
        &mut self,
        candidates: Vec<InitiativeCandidate>,
        autonomy: AutonomyMode,
        workload: Workload,
        now: UtcTimestamp,
    ) -> Result<Vec<DecisionRecord>, AttentionError> {
        let mut decisions = Vec::with_capacity(candidates.len());
        for candidate in candidates {
            decisions.push(self.evaluate_one(&candidate, autonomy, workload, now)?);
        }
        Ok(decisions)
    }

    /// Lists the real ordinary actions queued for a session.
    ///
    /// # Errors
    ///
    /// Returns an error if the durable action repository cannot be read.
    pub fn queued_actions(
        &self,
        session_id: &SessionId,
    ) -> Result<Vec<ActionRecord>, AttentionError> {
        self.inbox
            .list_session(session_id)
            .map_err(|error| AttentionError::Action(error.to_string()))
    }

    fn evaluate_one(
        &mut self,
        candidate: &InitiativeCandidate,
        autonomy: AutonomyMode,
        workload: Workload,
        now: UtcTimestamp,
    ) -> Result<DecisionRecord, AttentionError> {
        candidate.validate()?;
        if candidate.profile_id != self.profile_id {
            return Err(AttentionError::ProfileMismatch);
        }
        if let Some(existing) = self
            .ledger
            .decisions
            .iter()
            .find(|decision| decision.candidate_id == candidate.id)
        {
            return Ok(existing.clone());
        }
        let duplicate = self.ledger.candidates.iter().any(|previous| {
            let elapsed = now
                .unix_millis()
                .saturating_sub(previous.created_at.unix_millis());
            previous.topic == candidate.topic
                && previous.proposed_action == candidate.proposed_action
                && elapsed >= 0
                && elapsed <= self.config.duplicate_window_ms
        });
        let priority = i64::from(
            self.config
                .user_priorities
                .get(&candidate.topic)
                .copied()
                .unwrap_or_default(),
        );
        let mut score = candidate.base_score().saturating_add(priority);
        let mut reasons = vec![format!("base score {}", candidate.base_score())];
        if priority != 0 {
            reasons.push(format!("user priority {priority:+}"));
        }
        if duplicate {
            score = score.saturating_sub(500);
            reasons.push("recent duplicate penalty".to_owned());
        }
        let quiet = self.is_quiet(now);
        let budget_exhausted = self.budget_exhausted(&candidate.channel, now);
        let decision = if now >= candidate.expires_at {
            reasons.push("candidate expired".to_owned());
            AttentionDecision::Ignore
        } else if autonomy == AutonomyMode::Disabled {
            reasons.push("autonomy disabled".to_owned());
            AttentionDecision::Remember
        } else if duplicate && candidate.signals.urgency < 950 {
            reasons.push("duplicate suppressed".to_owned());
            AttentionDecision::Ignore
        } else if quiet && candidate.signals.urgency < 950 {
            reasons.push("quiet hours".to_owned());
            AttentionDecision::Batch
        } else if budget_exhausted && candidate.signals.urgency < 975 {
            reasons.push("daily channel budget exhausted".to_owned());
            AttentionDecision::Batch
        } else if matches!(workload, Workload::Interactive | Workload::Saturated)
            && candidate.signals.urgency < 950
        {
            reasons.push("interactive workload protected".to_owned());
            AttentionDecision::Remember
        } else {
            score_decision(score, autonomy)
        };
        reasons.push(format!("decision {decision:?}"));
        let record = DecisionRecord {
            candidate_id: candidate.id.clone(),
            awareness_event_id: candidate.awareness_event_id.clone(),
            decision,
            score,
            reasons,
            decided_at: now,
        };
        self.ledger.candidates.push(candidate.clone());
        self.ledger.decisions.push(record.clone());
        if matches!(decision, AttentionDecision::Ask | AttentionDecision::Notify) {
            let key = budget_key(&candidate.channel, now, self.quiet_zone);
            let count = self.ledger.notification_counts.entry(key).or_default();
            *count = count.saturating_add(1);
        }
        bound(&mut self.ledger.candidates, self.config.max_candidates);
        bound(&mut self.ledger.decisions, self.config.max_history);
        self.persist()?;
        if decision == AttentionDecision::StartBoundedWork {
            self.submit_candidate(candidate, now)?;
        }
        Ok(record)
    }

    fn submit_candidate(
        &self,
        candidate: &InitiativeCandidate,
        now: UtcTimestamp,
    ) -> Result<(), AttentionError> {
        let action = action_for(candidate, self.config.action_limits);
        if self
            .inbox
            .get(&action.id)
            .map_err(|error| AttentionError::Action(error.to_string()))?
            .is_none()
        {
            self.inbox
                .submit(action, now)
                .map_err(|error| AttentionError::Action(error.to_string()))?;
        }
        Ok(())
    }

    fn recover_started(&mut self, now: UtcTimestamp) -> Result<(), AttentionError> {
        let started = self
            .ledger
            .decisions
            .iter()
            .filter(|decision| decision.decision == AttentionDecision::StartBoundedWork)
            .map(|decision| decision.candidate_id.clone())
            .collect::<BTreeSet<_>>();
        for candidate in self
            .ledger
            .candidates
            .iter()
            .filter(|candidate| started.contains(&candidate.id))
        {
            self.submit_candidate(candidate, now)?;
        }
        Ok(())
    }

    fn is_quiet(&self, now: UtcTimestamp) -> bool {
        let Some(quiet) = &self.config.quiet_hours else {
            return false;
        };
        let Some(zone) = self.quiet_zone else {
            return false;
        };
        let Some(utc) = chrono::DateTime::<Utc>::from_timestamp_millis(now.unix_millis()) else {
            return false;
        };
        let local = utc.with_timezone(&zone);
        let minute = u16::try_from(local.hour() * 60 + local.minute()).unwrap_or(u16::MAX);
        if quiet.start_minute <= quiet.end_minute {
            minute >= quiet.start_minute && minute < quiet.end_minute
        } else {
            minute >= quiet.start_minute || minute < quiet.end_minute
        }
    }

    fn budget_exhausted(&self, channel: &str, now: UtcTimestamp) -> bool {
        let Some(limit) = self.config.notification_budgets.get(channel) else {
            return false;
        };
        self.ledger
            .notification_counts
            .get(&budget_key(channel, now, self.quiet_zone))
            .copied()
            .unwrap_or_default()
            >= *limit
    }

    fn persist(&self) -> Result<(), AttentionError> {
        let temporary = self.root.join(format!(".{LEDGER_FILE}.tmp"));
        fs::write(&temporary, serde_json::to_vec_pretty(&self.ledger)?)?;
        keith_platform::replace_file(&temporary, &self.root.join(LEDGER_FILE))?;
        Ok(())
    }
}

fn validate_config(config: &AttentionConfig) -> Result<Option<Tz>, AttentionError> {
    if config.duplicate_window_ms < 0
        || config.max_candidates == 0
        || config.max_history == 0
        || config.action_limits.max_turns.is_none()
        || config.action_limits.max_tokens.is_none()
        || config.action_limits.max_elapsed_ms.is_none()
        || config.action_limits.max_tool_calls.is_none()
        || config.action_limits.max_children.is_none()
    {
        return Err(AttentionError::InvalidConfiguration);
    }
    config
        .quiet_hours
        .as_ref()
        .map(|quiet| {
            if quiet.start_minute >= 24 * 60 || quiet.end_minute >= 24 * 60 {
                return Err(AttentionError::InvalidConfiguration);
            }
            quiet
                .time_zone
                .parse::<Tz>()
                .map_err(|_| AttentionError::InvalidConfiguration)
        })
        .transpose()
}

fn score_decision(score: i64, autonomy: AutonomyMode) -> AttentionDecision {
    if (740..=849).contains(&score) && autonomy == AutonomyMode::Bounded {
        return AttentionDecision::StartBoundedWork;
    }
    match score {
        ..=199 => AttentionDecision::Ignore,
        200..=349 => AttentionDecision::Remember,
        350..=499 => AttentionDecision::Batch,
        500..=619 => AttentionDecision::Schedule,
        620..=849 => AttentionDecision::Ask,
        _ => AttentionDecision::Notify,
    }
}

fn action_for(candidate: &InitiativeCandidate, limits: ActionLimits) -> SessionAction {
    SessionAction {
        id: ActionId(candidate.id.clone()),
        session_id: candidate.session_id.clone(),
        source: ActionSource::Awareness {
            event_id: candidate.awareness_event_id.clone(),
        },
        delivery: DeliveryPolicy::WhenIdle,
        priority: ActionPriority::Background,
        created_at: candidate.created_at,
        not_before: None,
        deadline: Some(candidate.expires_at),
        limits,
        reply_route: None,
        payload: ActionPayload::Awareness {
            event_id: candidate.awareness_event_id.clone(),
            summary: candidate.proposed_action.clone(),
        },
    }
}

fn budget_key(channel: &str, now: UtcTimestamp, zone: Option<Tz>) -> String {
    let date = chrono::DateTime::<Utc>::from_timestamp_millis(now.unix_millis()).map(|utc| {
        if let Some(zone) = zone {
            let local = utc.with_timezone(&zone);
            format!(
                "{:04}-{:02}-{:02}",
                local.year(),
                local.month(),
                local.day()
            )
        } else {
            format!("{:04}-{:02}-{:02}", utc.year(), utc.month(), utc.day())
        }
    });
    format!("{channel}:{}", date.unwrap_or_else(|| "invalid".to_owned()))
}

fn bound<T>(items: &mut Vec<T>, maximum: usize) {
    if items.len() > maximum {
        items.drain(..items.len() - maximum);
    }
}

#[cfg(test)]
mod tests {
    use keith_action_store::ActionInboxConfig;
    use keith_agent_types::EntityId;
    use keith_initiative::InitiativeSignals;
    use keith_state_store::EmbeddedStore;
    use tempfile::TempDir;

    use super::*;

    fn candidate(
        profile_id: &ProfileId,
        session_id: &SessionId,
        topic: &str,
        signals: InitiativeSignals,
        at: i64,
    ) -> InitiativeCandidate {
        InitiativeCandidate {
            id: EntityId::new(),
            awareness_event_id: EntityId::new(),
            profile_id: profile_id.clone(),
            session_id: session_id.clone(),
            channel: "desktop".to_owned(),
            topic: topic.to_owned(),
            proposed_action: format!("help with {topic}"),
            signals,
            created_at: UtcTimestamp::from_unix_millis(at),
            expires_at: UtcTimestamp::from_unix_millis(at + 86_400_000),
        }
    }

    fn service(
        root: &Path,
        profile_id: ProfileId,
        config: AttentionConfig,
    ) -> AttentionService<EmbeddedStore> {
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open_in_memory().expect("store"),
            ActionInboxConfig::default(),
        )
        .expect("inbox");
        AttentionService::open(root, profile_id, config, inbox, UtcTimestamp::UNIX_EPOCH)
            .expect("attention")
    }

    fn signals(score: u16) -> InitiativeSignals {
        InitiativeSignals {
            urgency: score,
            expected_value: score,
            confidence: score,
            interruption_cost: 0,
            resource_cost: 0,
            duplication_penalty: 0,
        }
    }

    #[test]
    fn quiet_hours_duplicates_workload_budget_and_urgency_override_prevent_spam() {
        let root = TempDir::new().expect("root");
        let profile = ProfileId::new();
        let session = SessionId::new();
        let config = AttentionConfig {
            quiet_hours: Some(QuietHours {
                time_zone: "UTC".to_owned(),
                start_minute: 0,
                end_minute: 60,
            }),
            notification_budgets: BTreeMap::from([("desktop".to_owned(), 1)]),
            ..AttentionConfig::default()
        };
        let mut service = service(root.path(), profile.clone(), config);
        let quiet = service
            .evaluate(
                vec![candidate(&profile, &session, "routine", signals(700), 1)],
                AutonomyMode::Bounded,
                Workload::Idle,
                UtcTimestamp::from_unix_millis(1),
            )
            .expect("quiet decision");
        assert_eq!(quiet[0].decision, AttentionDecision::Batch);
        let duplicate = service
            .evaluate(
                vec![candidate(&profile, &session, "routine", signals(700), 2)],
                AutonomyMode::Bounded,
                Workload::Idle,
                UtcTimestamp::from_unix_millis(2),
            )
            .expect("duplicate decision");
        assert_eq!(duplicate[0].decision, AttentionDecision::Ignore);
        let urgent = service
            .evaluate(
                vec![candidate(&profile, &session, "deadline", signals(1_000), 3)],
                AutonomyMode::Bounded,
                Workload::Saturated,
                UtcTimestamp::from_unix_millis(3),
            )
            .expect("urgent override");
        assert_eq!(urgent[0].decision, AttentionDecision::Notify);
        let limited = service
            .evaluate(
                vec![candidate(&profile, &session, "another", signals(900), 4)],
                AutonomyMode::Bounded,
                Workload::Idle,
                UtcTimestamp::from_unix_millis(4),
            )
            .expect("budget decision");
        assert_eq!(limited[0].decision, AttentionDecision::Batch);
    }

    #[test]
    fn bounded_work_uses_ordinary_inbox_and_restart_deduplicates_submission() {
        let root = TempDir::new().expect("root");
        let profile = ProfileId::new();
        let session = SessionId::new();
        let mut config = AttentionConfig::default();
        config.user_priorities.insert("priority".to_owned(), 50);
        let state_path = root.path().join("actions.db");
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open(&state_path, None).expect("durable store"),
            ActionInboxConfig::default(),
        )
        .expect("inbox");
        let mut service = AttentionService::open(
            root.path(),
            profile.clone(),
            config.clone(),
            inbox,
            UtcTimestamp::UNIX_EPOCH,
        )
        .expect("attention");
        let work = candidate(&profile, &session, "priority", signals(780), 1);
        let decision = service
            .evaluate(
                vec![work],
                AutonomyMode::Bounded,
                Workload::Idle,
                UtcTimestamp::from_unix_millis(1),
            )
            .expect("work decision");
        assert_eq!(decision[0].decision, AttentionDecision::StartBoundedWork);
        let actions = service.queued_actions(&session).expect("actions");
        assert_eq!(actions.len(), 1);
        assert_eq!(actions[0].action.limits.max_turns, Some(1));
        assert!(
            service
                .evaluate(
                    Vec::new(),
                    AutonomyMode::Bounded,
                    Workload::Idle,
                    UtcTimestamp::from_unix_millis(2),
                )
                .expect("no event")
                .is_empty()
        );
        assert_eq!(service.decision_history().len(), 1);
        drop(service);
        let inbox = PersistentActionInbox::new(
            EmbeddedStore::open(&state_path, None).expect("restart durable store"),
            ActionInboxConfig::default(),
        )
        .expect("restart inbox");
        let restarted = AttentionService::open(
            root.path(),
            profile,
            config,
            inbox,
            UtcTimestamp::from_unix_millis(2),
        )
        .expect("restart attention");
        assert_eq!(
            restarted.queued_actions(&session).expect("actions").len(),
            1
        );
    }

    #[test]
    fn all_score_decisions_and_user_controls_are_explainable() {
        assert_eq!(
            score_decision(100, AutonomyMode::Bounded),
            AttentionDecision::Ignore
        );
        assert_eq!(
            score_decision(250, AutonomyMode::Bounded),
            AttentionDecision::Remember
        );
        assert_eq!(
            score_decision(400, AutonomyMode::Bounded),
            AttentionDecision::Batch
        );
        assert_eq!(
            score_decision(550, AutonomyMode::Bounded),
            AttentionDecision::Schedule
        );
        assert_eq!(
            score_decision(700, AutonomyMode::Bounded),
            AttentionDecision::Ask
        );
        assert_eq!(
            score_decision(800, AutonomyMode::Bounded),
            AttentionDecision::StartBoundedWork
        );
        assert_eq!(
            score_decision(900, AutonomyMode::Bounded),
            AttentionDecision::Notify
        );
    }
}
