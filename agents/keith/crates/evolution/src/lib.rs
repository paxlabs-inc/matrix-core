#![forbid(unsafe_code)]

mod refinement;

pub use refinement::*;

use std::collections::{BTreeMap, BTreeSet};

use keith_agent_types::{CURRENT_SCHEMA_VERSION, EntityId, ProfileId, Revision, UtcTimestamp};
use keith_state_store_core::{ToolExperienceRepository, VersionedRecord, WritePrecondition};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum TaskCategory {
    Conversation,
    Research,
    Coding,
    FileOperation,
    DataAnalysis,
    Communication,
    Monitoring,
    Automation,
}

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "subject")]
pub enum ExperienceSubject {
    Provider { provider: String, model: String },
    Tool { name: String },
    Skill { name: String },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum FailureCategory {
    Authentication,
    InvalidInput,
    Unavailable,
    RateLimited,
    Environment,
    Permission,
    MalformedOutput,
    Verification,
    Cancelled,
    Internal,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CorrectionKind {
    RetryWithFallback,
    IncreaseTimeout,
    RefreshReadiness,
    ChooseDifferentTool,
    AskUser,
    Stop,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "outcome")]
pub enum ExperienceOutcome {
    Success,
    Failure { category: FailureCategory },
    Timeout,
    Corrected { strategy: CorrectionKind },
    Recovered,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExperienceRecord {
    pub id: EntityId,
    pub profile_id: ProfileId,
    pub task_category: TaskCategory,
    pub subject: ExperienceSubject,
    pub outcome: ExperienceOutcome,
    pub latency_ms: u64,
    pub observed_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct HistoryControl {
    profile_id: ProfileId,
    enabled: bool,
    changed_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "record")]
enum StoredExperience {
    Observation(ExperienceRecord),
    Control(HistoryControl),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ExperienceConfig {
    pub max_records_per_profile: usize,
    pub routing_window: usize,
    pub success_bonus: i32,
    pub failure_penalty: i32,
    pub timeout_penalty: i32,
    pub recovery_bonus: i32,
    pub minimum_timeout_ms: u64,
    pub maximum_timeout_ms: u64,
}

impl Default for ExperienceConfig {
    fn default() -> Self {
        Self {
            max_records_per_profile: 512,
            routing_window: 64,
            success_bonus: 8,
            failure_penalty: 24,
            timeout_penalty: 32,
            recovery_bonus: 16,
            minimum_timeout_ms: 1_000,
            maximum_timeout_ms: 300_000,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RouteCandidate {
    pub subject: ExperienceSubject,
    pub base_priority: i32,
    pub ready: bool,
    pub default_timeout_ms: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RoutingConstraints {
    pub allowed: BTreeSet<ExperienceSubject>,
    pub forced: Option<ExperienceSubject>,
}

impl RoutingConstraints {
    pub fn allows(&self, subject: &ExperienceSubject) -> bool {
        self.allowed.is_empty() || self.allowed.contains(subject)
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RankedCandidate {
    pub subject: ExperienceSubject,
    pub score: i32,
    pub suggested_timeout_ms: u64,
    pub forced_by_profile: bool,
    pub evidence_records: usize,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RoutingDecision {
    pub history_enabled: bool,
    pub ranked: Vec<RankedCandidate>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExperienceExport {
    pub profile_id: ProfileId,
    pub enabled: bool,
    pub records: Vec<ExperienceRecord>,
}

#[derive(Debug, Error)]
pub enum ExperienceError {
    #[error("experience repository failed: {0}")]
    Repository(String),
    #[error("experience record encoding failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("experience configuration is invalid")]
    InvalidConfig,
    #[error("experience subject identity is invalid")]
    InvalidSubject,
}

pub struct ExperienceService<R> {
    repository: R,
    config: ExperienceConfig,
}

impl<R> ExperienceService<R>
where
    R: ToolExperienceRepository,
{
    /// # Errors
    ///
    /// Returns an error when history or timeout bounds are invalid.
    pub fn new(repository: R, config: ExperienceConfig) -> Result<Self, ExperienceError> {
        if config.max_records_per_profile == 0
            || config.routing_window == 0
            || config.minimum_timeout_ms == 0
            || config.minimum_timeout_ms > config.maximum_timeout_ms
        {
            return Err(ExperienceError::InvalidConfig);
        }
        Ok(Self { repository, config })
    }

    /// # Errors
    ///
    /// Returns an error when identity validation, persistence, or bounded pruning fails.
    pub fn record(&self, record: ExperienceRecord) -> Result<(), ExperienceError> {
        validate_subject(&record.subject)?;
        self.put(
            record.id.clone(),
            record.observed_at,
            StoredExperience::Observation(record),
        )?;
        self.prune_profile()?;
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when persistent history cannot be read or decoded.
    pub fn inspect(
        &self,
        profile_id: &ProfileId,
    ) -> Result<Vec<ExperienceRecord>, ExperienceError> {
        let mut records = self
            .stored()?
            .into_iter()
            .filter_map(|(_, stored)| match stored {
                StoredExperience::Observation(record) if &record.profile_id == profile_id => {
                    Some(record)
                }
                StoredExperience::Observation(_) | StoredExperience::Control(_) => None,
            })
            .collect::<Vec<_>>();
        records.sort_by(|left, right| {
            left.observed_at
                .cmp(&right.observed_at)
                .then_with(|| left.id.cmp(&right.id))
        });
        Ok(records)
    }

    /// Resolves an experience identity against the authoritative durable repository.
    ///
    /// # Errors
    /// Returns an error when persistent history cannot be read or decoded.
    pub fn resolve_evidence(
        &self,
        profile_id: &ProfileId,
        record_id: &EntityId,
    ) -> Result<Option<ExperienceRecord>, ExperienceError> {
        Ok(self
            .inspect(profile_id)?
            .into_iter()
            .find(|record| &record.id == record_id))
    }

    /// # Errors
    ///
    /// Returns an error when persistent controls cannot be inspected.
    pub fn enabled(&self, profile_id: &ProfileId) -> Result<bool, ExperienceError> {
        Ok(self
            .stored()?
            .into_iter()
            .filter_map(|(_, stored)| match stored {
                StoredExperience::Control(control) if &control.profile_id == profile_id => {
                    Some(control)
                }
                StoredExperience::Control(_) | StoredExperience::Observation(_) => None,
            })
            .max_by_key(|control| control.changed_at)
            .is_none_or(|control| control.enabled))
    }

    /// # Errors
    ///
    /// Returns an error when the auditable enable/disable control cannot be persisted.
    pub fn set_enabled(
        &self,
        profile_id: ProfileId,
        enabled: bool,
        changed_at: UtcTimestamp,
    ) -> Result<(), ExperienceError> {
        self.put(
            EntityId::new(),
            changed_at,
            StoredExperience::Control(HistoryControl {
                profile_id,
                enabled,
                changed_at,
            }),
        )?;
        self.prune_controls()?;
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when profile history cannot be read, ranked, or decoded.
    pub fn rank(
        &self,
        profile_id: &ProfileId,
        task_category: TaskCategory,
        candidates: &[RouteCandidate],
        constraints: &RoutingConstraints,
    ) -> Result<RoutingDecision, ExperienceError> {
        for candidate in candidates {
            validate_subject(&candidate.subject)?;
        }
        let enabled = self.enabled(profile_id)?;
        let records = if enabled {
            let matching = self
                .inspect(profile_id)?
                .into_iter()
                .filter(|record| record.task_category == task_category)
                .collect::<Vec<_>>();
            let start = matching.len().saturating_sub(self.config.routing_window);
            matching.into_iter().skip(start).collect::<Vec<_>>()
        } else {
            Vec::new()
        };
        let mut ranked = candidates
            .iter()
            .filter(|candidate| candidate.ready && constraints.allows(&candidate.subject))
            .map(|candidate| {
                let subject_records = records
                    .iter()
                    .filter(|record| record.subject == candidate.subject)
                    .collect::<Vec<_>>();
                let score = if enabled {
                    adaptive_score(candidate.base_priority, &subject_records, self.config)
                } else {
                    candidate.base_priority
                };
                RankedCandidate {
                    subject: candidate.subject.clone(),
                    score,
                    suggested_timeout_ms: suggested_timeout(
                        candidate.default_timeout_ms,
                        &subject_records,
                        self.config,
                    ),
                    forced_by_profile: constraints.forced.as_ref() == Some(&candidate.subject),
                    evidence_records: subject_records.len(),
                }
            })
            .collect::<Vec<_>>();
        ranked.sort_by(|left, right| {
            right
                .forced_by_profile
                .cmp(&left.forced_by_profile)
                .then_with(|| right.score.cmp(&left.score))
                .then_with(|| left.subject.cmp(&right.subject))
        });
        Ok(RoutingDecision {
            history_enabled: enabled,
            ranked,
        })
    }

    /// # Errors
    ///
    /// Returns an error when profile observations or controls cannot be exported.
    pub fn export(&self, profile_id: &ProfileId) -> Result<ExperienceExport, ExperienceError> {
        Ok(ExperienceExport {
            profile_id: profile_id.clone(),
            enabled: self.enabled(profile_id)?,
            records: self.inspect(profile_id)?,
        })
    }

    /// # Errors
    ///
    /// Returns an error when any profile history record cannot be deleted explicitly.
    pub fn reset(&self, profile_id: &ProfileId) -> Result<usize, ExperienceError> {
        let records = self.stored()?;
        let ids = records
            .into_iter()
            .filter_map(|(id, stored)| match stored {
                StoredExperience::Observation(record) if &record.profile_id == profile_id => {
                    Some(id)
                }
                StoredExperience::Control(control) if &control.profile_id == profile_id => Some(id),
                StoredExperience::Observation(_) | StoredExperience::Control(_) => None,
            })
            .collect::<Vec<_>>();
        for id in &ids {
            self.repository
                .delete_tool_experience(id, WritePrecondition::Any)
                .map_err(repository_error)?;
        }
        Ok(ids.len())
    }

    fn prune_profile(&self) -> Result<(), ExperienceError> {
        let mut by_profile = BTreeMap::<ProfileId, Vec<(EntityId, UtcTimestamp)>>::new();
        for (id, stored) in self.stored()? {
            if let StoredExperience::Observation(record) = stored {
                by_profile
                    .entry(record.profile_id)
                    .or_default()
                    .push((id, record.observed_at));
            }
        }
        for records in by_profile.values_mut() {
            records.sort_by_key(|(_, timestamp)| *timestamp);
            let excess = records
                .len()
                .saturating_sub(self.config.max_records_per_profile);
            for (id, _) in records.iter().take(excess) {
                self.repository
                    .delete_tool_experience(id, WritePrecondition::Any)
                    .map_err(repository_error)?;
            }
        }
        Ok(())
    }

    fn prune_controls(&self) -> Result<(), ExperienceError> {
        let mut by_profile = BTreeMap::<ProfileId, Vec<(EntityId, UtcTimestamp)>>::new();
        for (id, stored) in self.stored()? {
            if let StoredExperience::Control(control) = stored {
                by_profile
                    .entry(control.profile_id)
                    .or_default()
                    .push((id, control.changed_at));
            }
        }
        for controls in by_profile.values_mut() {
            controls.sort_by_key(|(_, timestamp)| *timestamp);
            let stale = controls.len().saturating_sub(1);
            for (id, _) in controls.iter().take(stale) {
                self.repository
                    .delete_tool_experience(id, WritePrecondition::Any)
                    .map_err(repository_error)?;
            }
        }
        Ok(())
    }

    fn stored(&self) -> Result<Vec<(EntityId, StoredExperience)>, ExperienceError> {
        self.repository
            .list_tool_experience()
            .map_err(repository_error)?
            .into_iter()
            .map(|record| {
                let stored = serde_json::from_value(record.payload)?;
                Ok((record.id, stored))
            })
            .collect()
    }

    fn put(
        &self,
        id: EntityId,
        timestamp: UtcTimestamp,
        stored: StoredExperience,
    ) -> Result<(), ExperienceError> {
        self.repository
            .put_tool_experience(
                VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id,
                    revision: Revision::new(1),
                    updated_at: timestamp,
                    payload: serde_json::to_value(stored)?,
                },
                WritePrecondition::Missing,
            )
            .map_err(repository_error)?;
        Ok(())
    }
}

fn adaptive_score(base: i32, records: &[&ExperienceRecord], config: ExperienceConfig) -> i32 {
    let mut adjustment = 0_i32;
    for record in records {
        match record.outcome {
            ExperienceOutcome::Success => {
                adjustment = adjustment.saturating_add(config.success_bonus);
            }
            ExperienceOutcome::Failure { .. } => {
                adjustment = adjustment.saturating_sub(config.failure_penalty);
            }
            ExperienceOutcome::Timeout => {
                adjustment = adjustment.saturating_sub(config.timeout_penalty);
            }
            ExperienceOutcome::Recovered => {
                adjustment = config.recovery_bonus;
            }
            ExperienceOutcome::Corrected { .. } => {}
        }
    }
    base.saturating_add(adjustment)
}

fn suggested_timeout(
    default_timeout_ms: u64,
    records: &[&ExperienceRecord],
    config: ExperienceConfig,
) -> u64 {
    let mut successful = records
        .iter()
        .filter(|record| record.outcome == ExperienceOutcome::Success)
        .map(|record| record.latency_ms)
        .collect::<Vec<_>>();
    if successful.is_empty() {
        return default_timeout_ms.clamp(config.minimum_timeout_ms, config.maximum_timeout_ms);
    }
    successful.sort_unstable();
    successful[successful.len() / 2]
        .saturating_mul(3)
        .clamp(config.minimum_timeout_ms, config.maximum_timeout_ms)
}

fn validate_subject(subject: &ExperienceSubject) -> Result<(), ExperienceError> {
    let valid = match subject {
        ExperienceSubject::Provider { provider, model } => {
            valid_identity(provider) && valid_identity(model)
        }
        ExperienceSubject::Tool { name } | ExperienceSubject::Skill { name } => {
            valid_identity(name)
        }
    };
    if valid {
        Ok(())
    } else {
        Err(ExperienceError::InvalidSubject)
    }
}

fn valid_identity(value: &str) -> bool {
    !value.trim().is_empty() && value.len() <= 256 && !value.chars().any(char::is_control)
}

fn repository_error(error: impl std::fmt::Display) -> ExperienceError {
    ExperienceError::Repository(error.to_string())
}

#[cfg(test)]
mod tests {
    use keith_state_store::EmbeddedStore;

    use super::*;

    fn tool(name: &str) -> ExperienceSubject {
        ExperienceSubject::Tool { name: name.into() }
    }

    fn observation(
        profile_id: &ProfileId,
        subject: ExperienceSubject,
        outcome: ExperienceOutcome,
        at: i64,
        latency_ms: u64,
    ) -> ExperienceRecord {
        ExperienceRecord {
            id: EntityId::new(),
            profile_id: profile_id.clone(),
            task_category: TaskCategory::Coding,
            subject,
            outcome,
            latency_ms,
            observed_at: UtcTimestamp::from_unix_millis(at),
        }
    }

    fn candidates(ready_a: bool) -> Vec<RouteCandidate> {
        vec![
            RouteCandidate {
                subject: tool("workspace_search"),
                base_priority: 100,
                ready: ready_a,
                default_timeout_ms: 30_000,
            },
            RouteCandidate {
                subject: tool("fallback_search"),
                base_priority: 90,
                ready: true,
                default_timeout_ms: 30_000,
            },
        ]
    }

    fn constraints(forced: Option<ExperienceSubject>) -> RoutingConstraints {
        RoutingConstraints {
            allowed: BTreeSet::new(),
            forced,
        }
    }

    #[test]
    fn repeated_environment_failure_changes_route_but_recovery_restores_selection() {
        let profile_id = ProfileId::new();
        let service = ExperienceService::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ExperienceConfig::default(),
        )
        .unwrap();
        for at in 1..=2 {
            service
                .record(observation(
                    &profile_id,
                    tool("workspace_search"),
                    ExperienceOutcome::Failure {
                        category: FailureCategory::Environment,
                    },
                    at,
                    20,
                ))
                .unwrap();
        }
        let changed = service
            .rank(
                &profile_id,
                TaskCategory::Coding,
                &candidates(true),
                &constraints(None),
            )
            .unwrap();
        assert_eq!(changed.ranked[0].subject, tool("fallback_search"));
        assert!(changed.ranked.iter().any(|candidate| {
            candidate.subject == tool("workspace_search") && candidate.evidence_records == 2
        }));

        let unavailable = service
            .rank(
                &profile_id,
                TaskCategory::Coding,
                &candidates(false),
                &constraints(None),
            )
            .unwrap();
        assert!(
            unavailable
                .ranked
                .iter()
                .all(|candidate| candidate.subject != tool("workspace_search"))
        );
        service
            .record(observation(
                &profile_id,
                tool("workspace_search"),
                ExperienceOutcome::Recovered,
                3,
                0,
            ))
            .unwrap();
        let recovered = service
            .rank(
                &profile_id,
                TaskCategory::Coding,
                &candidates(true),
                &constraints(None),
            )
            .unwrap();
        assert_eq!(recovered.ranked[0].subject, tool("workspace_search"));
    }

    #[test]
    fn profile_override_is_authoritative_and_disabled_history_uses_base_order() {
        let profile_id = ProfileId::new();
        let service = ExperienceService::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ExperienceConfig::default(),
        )
        .unwrap();
        for at in 0..5 {
            service
                .record(observation(
                    &profile_id,
                    tool("fallback_search"),
                    ExperienceOutcome::Failure {
                        category: FailureCategory::Verification,
                    },
                    at,
                    10,
                ))
                .unwrap();
        }
        let forced = service
            .rank(
                &profile_id,
                TaskCategory::Coding,
                &candidates(true),
                &constraints(Some(tool("fallback_search"))),
            )
            .unwrap();
        assert_eq!(forced.ranked[0].subject, tool("fallback_search"));
        assert!(forced.ranked[0].forced_by_profile);

        service
            .set_enabled(
                profile_id.clone(),
                false,
                UtcTimestamp::from_unix_millis(10),
            )
            .unwrap();
        let disabled = service
            .rank(
                &profile_id,
                TaskCategory::Coding,
                &candidates(true),
                &constraints(None),
            )
            .unwrap();
        assert!(!disabled.history_enabled);
        assert_eq!(disabled.ranked[0].subject, tool("workspace_search"));
    }

    #[test]
    fn successful_latency_adapts_timeout_within_profile_bounds() {
        let profile_id = ProfileId::new();
        let service = ExperienceService::new(
            EmbeddedStore::open_in_memory().unwrap(),
            ExperienceConfig {
                minimum_timeout_ms: 100,
                maximum_timeout_ms: 2_000,
                ..ExperienceConfig::default()
            },
        )
        .unwrap();
        for (at, latency) in [(1, 200), (2, 300), (3, 400)] {
            service
                .record(observation(
                    &profile_id,
                    ExperienceSubject::Provider {
                        provider: "openai".into(),
                        model: "model-a".into(),
                    },
                    ExperienceOutcome::Success,
                    at,
                    latency,
                ))
                .unwrap();
        }
        let subject = ExperienceSubject::Provider {
            provider: "openai".into(),
            model: "model-a".into(),
        };
        let decision = service
            .rank(
                &profile_id,
                TaskCategory::Coding,
                &[RouteCandidate {
                    subject,
                    base_priority: 1,
                    ready: true,
                    default_timeout_ms: 1_500,
                }],
                &constraints(None),
            )
            .unwrap();
        assert_eq!(decision.ranked[0].suggested_timeout_ms, 900);
    }

    #[test]
    fn bounded_history_persists_and_operator_can_inspect_export_reset_and_disable() {
        let directory = tempfile::tempdir().unwrap();
        let database = directory.path().join("experience.sqlite");
        let profile_id = ProfileId::new();
        {
            let service = ExperienceService::new(
                EmbeddedStore::open(&database, None).unwrap(),
                ExperienceConfig {
                    max_records_per_profile: 3,
                    ..ExperienceConfig::default()
                },
            )
            .unwrap();
            for at in 0..5 {
                service
                    .record(observation(
                        &profile_id,
                        ExperienceSubject::Skill {
                            name: "repository-inspection".into(),
                        },
                        ExperienceOutcome::Corrected {
                            strategy: CorrectionKind::ChooseDifferentTool,
                        },
                        at,
                        5,
                    ))
                    .unwrap();
            }
            service
                .set_enabled(profile_id.clone(), false, UtcTimestamp::from_unix_millis(6))
                .unwrap();
        }
        let service = ExperienceService::new(
            EmbeddedStore::open(&database, None).unwrap(),
            ExperienceConfig {
                max_records_per_profile: 3,
                ..ExperienceConfig::default()
            },
        )
        .unwrap();
        let records = service.inspect(&profile_id).unwrap();
        assert_eq!(records.len(), 3);
        assert_eq!(records[0].observed_at, UtcTimestamp::from_unix_millis(2));
        let exported = service.export(&profile_id).unwrap();
        assert!(!exported.enabled);
        let serialized = serde_json::to_string(&exported).unwrap();
        assert!(!serialized.contains("reasoning"));
        assert!(serialized.contains("choose_different_tool"));
        assert_eq!(service.reset(&profile_id).unwrap(), 4);
        assert!(service.inspect(&profile_id).unwrap().is_empty());
        assert!(service.enabled(&profile_id).unwrap());
    }
}
