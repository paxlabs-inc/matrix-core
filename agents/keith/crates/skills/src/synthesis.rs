use std::collections::BTreeSet;
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Path, PathBuf};

use keith_agent_types::{CURRENT_SCHEMA_VERSION, EntityId, Revision, SchemaVersion, UtcTimestamp};
use keith_workspace::{PersonalWorkspace, PersonalWorkspaceLimits};
use serde::{Deserialize, Serialize};
use thiserror::Error;

use super::{
    SkillLimits, SkillManifest, SkillRegistry, SkillRoots, digest, parse_skill, valid_name,
};

const CANDIDATE_ROOT: &str = ".keith/skill-candidates";

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CommittedWorkflowOutcome {
    pub outcome_id: EntityId,
    pub session_id: EntityId,
    pub entry_id: EntityId,
    pub workflow_key: String,
    pub description: String,
    pub trigger: String,
    pub inputs: Vec<String>,
    pub steps: Vec<String>,
    pub required_tools: Vec<String>,
    pub validation: Vec<String>,
    pub known_failures: Vec<String>,
    pub stop_conditions: Vec<String>,
    pub platforms: Vec<String>,
    pub committed: bool,
    pub successful: bool,
    pub contains_private_data: bool,
    pub improvement_basis_points: u16,
    pub observed_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CandidateValidationCases {
    pub applicability: Vec<String>,
    pub regressions: Vec<String>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SkillSynthesisPolicy {
    pub minimum_successes: usize,
    pub minimum_improvement_basis_points: u16,
    pub maximum_candidate_bytes: usize,
    pub require_review: bool,
    pub blocked_tools: BTreeSet<String>,
    pub sensitive_markers: BTreeSet<String>,
}

impl Default for SkillSynthesisPolicy {
    fn default() -> Self {
        Self {
            minimum_successes: 3,
            minimum_improvement_basis_points: 100,
            maximum_candidate_bytes: 64 * 1_024,
            require_review: true,
            blocked_tools: BTreeSet::new(),
            sensitive_markers: BTreeSet::from([
                "api_key".into(),
                "authorization:".into(),
                "bearer ".into(),
                "password".into(),
                "private_key".into(),
                "secret".into(),
            ]),
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SkillCandidateState {
    AwaitingReview,
    Ready,
    Rejected,
    Activated,
    RolledBack,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SkillCandidateReview {
    pub reviewer: String,
    pub approved: bool,
    pub note: String,
    pub reviewed_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SkillCandidate {
    pub version: SchemaVersion,
    pub id: EntityId,
    pub skill_id: String,
    pub state: SkillCandidateState,
    pub source: String,
    pub source_digest: String,
    pub evidence: Vec<EntityId>,
    pub validation_cases: CandidateValidationCases,
    pub created_at: UtcTimestamp,
    pub review: Option<SkillCandidateReview>,
    pub activated_revision: Option<Revision>,
    pub activated_digest: Option<String>,
}

#[derive(Debug, Error)]
pub enum SkillSynthesisError {
    #[error("candidate synthesis policy is invalid")]
    InvalidPolicy,
    #[error("workflow evidence is insufficient or inconsistent")]
    InsufficientEvidence,
    #[error("workflow evidence includes an uncommitted or failed outcome")]
    FailedOutcome,
    #[error("workflow evidence or candidate contains private or secret data")]
    PrivateData,
    #[error("candidate requests unsafe authority")]
    Unsafe,
    #[error("candidate package is malformed")]
    Malformed,
    #[error("candidate does not improve on an installed procedure")]
    NonImproving,
    #[error("candidate requires unavailable tools: {0}")]
    UnavailableTools(String),
    #[error("candidate failed clean-workspace applicability")]
    Applicability,
    #[error("candidate failed a regression case")]
    Regression,
    #[error("candidate review is required")]
    ReviewRequired,
    #[error("candidate was rejected")]
    Rejected,
    #[error("candidate state does not allow this operation")]
    InvalidState,
    #[error("candidate was not found")]
    NotFound,
    #[error("candidate storage failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("candidate encoding failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("skill lifecycle failed: {0}")]
    Skill(#[from] super::SkillError),
    #[error("clean validation workspace failed: {0}")]
    Workspace(#[from] keith_workspace::PersonalWorkspaceError),
}

pub struct SkillSynthesisService<'a> {
    registry: &'a SkillRegistry,
    policy: SkillSynthesisPolicy,
    candidate_root: PathBuf,
}

impl<'a> SkillSynthesisService<'a> {
    /// Opens the persistent candidate staging area with bounded synthesis policy.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid policy or an unsafe candidate directory.
    pub fn new(
        registry: &'a SkillRegistry,
        policy: SkillSynthesisPolicy,
    ) -> Result<Self, SkillSynthesisError> {
        if policy.minimum_successes < 2
            || policy.minimum_improvement_basis_points == 0
            || policy.maximum_candidate_bytes == 0
            || policy.sensitive_markers.iter().any(String::is_empty)
        {
            return Err(SkillSynthesisError::InvalidPolicy);
        }
        let candidate_root = registry.workspace.layout().root.join(CANDIDATE_ROOT);
        create_private_directory(&candidate_root)?;
        Ok(Self {
            registry,
            policy,
            candidate_root,
        })
    }

    /// Builds and validates an inactive candidate from repeated committed outcomes.
    ///
    /// # Errors
    ///
    /// Returns a typed rejection for insufficient, private, unsafe, unavailable,
    /// malformed, non-improving, or regression-producing evidence.
    pub fn synthesize(
        &self,
        outcomes: &[CommittedWorkflowOutcome],
        validation_cases: CandidateValidationCases,
        ready_tools: &BTreeSet<String>,
        now: UtcTimestamp,
    ) -> Result<SkillCandidate, SkillSynthesisError> {
        let representative = self.validate_evidence(outcomes)?;
        validate_cases(&validation_cases, &self.policy)?;
        let missing = representative
            .required_tools
            .iter()
            .filter(|tool| !ready_tools.contains(*tool))
            .cloned()
            .collect::<Vec<_>>();
        if !missing.is_empty() {
            return Err(SkillSynthesisError::UnavailableTools(missing.join(", ")));
        }
        if representative
            .required_tools
            .iter()
            .any(|tool| self.policy.blocked_tools.contains(tool))
        {
            return Err(SkillSynthesisError::Unsafe);
        }
        let skill_id = learned_skill_id(&representative.workflow_key)?;
        let manifest = SkillManifest {
            id: skill_id.clone(),
            version: "1.0.0".into(),
            description: representative.description.clone(),
            triggers: vec![representative.trigger.clone()],
            inputs: representative.inputs.clone(),
            steps: representative.steps.clone(),
            required_tools: representative.required_tools.clone(),
            validation: representative.validation.clone(),
            known_failures: representative.known_failures.clone(),
            stop_conditions: representative.stop_conditions.clone(),
            platforms: representative.platforms.clone(),
        };
        let source = render_candidate(&manifest, outcomes.len())?;
        if source.len() > self.policy.maximum_candidate_bytes
            || parse_skill(&source, source.len()).is_err()
        {
            return Err(SkillSynthesisError::Malformed);
        }
        self.reject_sensitive(&source)?;
        self.reject_duplicate(&manifest, now)?;
        self.validate_clean_workspace(&source, &skill_id, &validation_cases, ready_tools, now)?;
        let candidate = SkillCandidate {
            version: CURRENT_SCHEMA_VERSION,
            id: EntityId::new(),
            skill_id,
            state: if self.policy.require_review {
                SkillCandidateState::AwaitingReview
            } else {
                SkillCandidateState::Ready
            },
            source_digest: digest(&source),
            source,
            evidence: outcomes
                .iter()
                .map(|outcome| outcome.outcome_id.clone())
                .collect(),
            validation_cases,
            created_at: now,
            review: None,
            activated_revision: None,
            activated_digest: None,
        };
        self.persist(&candidate)?;
        Ok(candidate)
    }

    /// Records the configured owner review without activating the candidate.
    ///
    /// # Errors
    ///
    /// Returns an error for missing candidates, invalid state, private review data,
    /// or durable storage failure.
    pub fn review(
        &self,
        id: &EntityId,
        reviewer: impl Into<String>,
        approved: bool,
        note: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<SkillCandidate, SkillSynthesisError> {
        let mut candidate = self.load(id)?;
        if candidate.state != SkillCandidateState::AwaitingReview {
            return Err(SkillSynthesisError::InvalidState);
        }
        let review = SkillCandidateReview {
            reviewer: reviewer.into(),
            approved,
            note: note.into(),
            reviewed_at: now,
        };
        self.reject_sensitive(&format!("{}\n{}", review.reviewer, review.note))?;
        if review.reviewer.trim().is_empty() {
            return Err(SkillSynthesisError::Rejected);
        }
        candidate.state = if approved {
            SkillCandidateState::Ready
        } else {
            SkillCandidateState::Rejected
        };
        candidate.review = Some(review);
        self.persist(&candidate)?;
        Ok(candidate)
    }

    /// Atomically installs a validated and approved candidate through the skill registry.
    ///
    /// # Errors
    ///
    /// Returns an error when review is required, the candidate was rejected or
    /// changed, or the skill lifecycle cannot commit the package.
    pub fn activate(
        &self,
        id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<SkillCandidate, SkillSynthesisError> {
        let mut candidate = self.load(id)?;
        if candidate.state == SkillCandidateState::AwaitingReview {
            return Err(SkillSynthesisError::ReviewRequired);
        }
        if candidate.state == SkillCandidateState::Rejected {
            return Err(SkillSynthesisError::Rejected);
        }
        if candidate.state != SkillCandidateState::Ready
            || digest(&candidate.source) != candidate.source_digest
        {
            return Err(SkillSynthesisError::InvalidState);
        }
        let package = self.registry.install(
            candidate.source.clone(),
            format!("validated candidate {}", candidate.id),
            now,
        )?;
        candidate.state = SkillCandidateState::Activated;
        candidate.activated_revision = package.provenance.revision;
        candidate.activated_digest = Some(package.provenance.digest);
        self.persist(&candidate)?;
        Ok(candidate)
    }

    /// Removes the exact activated candidate while retaining its lifecycle history.
    ///
    /// # Errors
    ///
    /// Returns an error when the candidate is not active, its installed content
    /// changed, or the registry cannot complete the rollback.
    pub fn rollback_activation(
        &self,
        id: &EntityId,
        now: UtcTimestamp,
    ) -> Result<SkillCandidate, SkillSynthesisError> {
        let mut candidate = self.load(id)?;
        if candidate.state != SkillCandidateState::Activated {
            return Err(SkillSynthesisError::InvalidState);
        }
        let inspection = self.registry.inspect(&candidate.skill_id, now)?;
        if inspection.effective.as_ref().is_none_or(|package| {
            package.provenance.digest != candidate.activated_digest.clone().unwrap_or_default()
        }) {
            return Err(SkillSynthesisError::InvalidState);
        }
        self.registry.delete(&candidate.skill_id, now)?;
        candidate.state = SkillCandidateState::RolledBack;
        self.persist(&candidate)?;
        Ok(candidate)
    }

    /// Loads a restart-safe candidate record.
    ///
    /// # Errors
    ///
    /// Returns an error for a missing, corrupt, or incompatible candidate record.
    pub fn load(&self, id: &EntityId) -> Result<SkillCandidate, SkillSynthesisError> {
        let path = self.candidate_path(id);
        let bytes = fs::read(&path).map_err(|error| {
            if error.kind() == std::io::ErrorKind::NotFound {
                SkillSynthesisError::NotFound
            } else {
                SkillSynthesisError::Io(error)
            }
        })?;
        let candidate = serde_json::from_slice::<SkillCandidate>(&bytes)?;
        if candidate.id != *id
            || candidate.version.major != CURRENT_SCHEMA_VERSION.major
            || candidate.version.minor > CURRENT_SCHEMA_VERSION.minor
        {
            return Err(SkillSynthesisError::Malformed);
        }
        Ok(candidate)
    }

    fn validate_evidence<'b>(
        &self,
        outcomes: &'b [CommittedWorkflowOutcome],
    ) -> Result<&'b CommittedWorkflowOutcome, SkillSynthesisError> {
        if outcomes.len() < self.policy.minimum_successes {
            return Err(SkillSynthesisError::InsufficientEvidence);
        }
        if outcomes
            .iter()
            .any(|outcome| !outcome.committed || !outcome.successful)
        {
            return Err(SkillSynthesisError::FailedOutcome);
        }
        if outcomes.iter().any(|outcome| outcome.contains_private_data) {
            return Err(SkillSynthesisError::PrivateData);
        }
        let representative = &outcomes[0];
        let unique = outcomes
            .iter()
            .map(|outcome| &outcome.outcome_id)
            .collect::<BTreeSet<_>>();
        let consistent = outcomes.iter().all(|outcome| {
            outcome.workflow_key == representative.workflow_key
                && outcome.description == representative.description
                && outcome.trigger == representative.trigger
                && outcome.inputs == representative.inputs
                && outcome.steps == representative.steps
                && outcome.required_tools == representative.required_tools
                && outcome.validation == representative.validation
                && outcome.known_failures == representative.known_failures
                && outcome.stop_conditions == representative.stop_conditions
                && outcome.platforms == representative.platforms
        });
        let improvement = outcomes
            .iter()
            .map(|outcome| u64::from(outcome.improvement_basis_points))
            .sum::<u64>()
            / u64::try_from(outcomes.len()).unwrap_or(u64::MAX);
        if unique.len() != outcomes.len()
            || !consistent
            || improvement < u64::from(self.policy.minimum_improvement_basis_points)
        {
            return Err(SkillSynthesisError::NonImproving);
        }
        for value in evidence_strings(representative) {
            self.reject_sensitive(value)?;
        }
        Ok(representative)
    }

    fn reject_sensitive(&self, value: &str) -> Result<(), SkillSynthesisError> {
        let normalized = value.to_ascii_lowercase();
        if has_disallowed_control(value)
            || self
                .policy
                .sensitive_markers
                .iter()
                .any(|marker| normalized.contains(marker))
            || contains_secret_shape(value)
        {
            Err(SkillSynthesisError::PrivateData)
        } else {
            Ok(())
        }
    }

    fn reject_duplicate(
        &self,
        manifest: &SkillManifest,
        now: UtcTimestamp,
    ) -> Result<(), SkillSynthesisError> {
        let duplicate = self.registry.discover(now)?.iter().any(|package| {
            package.manifest.id == manifest.id
                || (package.manifest.triggers == manifest.triggers
                    && package.manifest.steps == manifest.steps
                    && package.manifest.required_tools == manifest.required_tools)
        });
        if duplicate {
            Err(SkillSynthesisError::NonImproving)
        } else {
            Ok(())
        }
    }

    fn validate_clean_workspace(
        &self,
        source: &str,
        skill_id: &str,
        cases: &CandidateValidationCases,
        ready_tools: &BTreeSet<String>,
        now: UtcTimestamp,
    ) -> Result<(), SkillSynthesisError> {
        let directory = tempfile::tempdir()?;
        let workspace = PersonalWorkspace::open(
            directory.path().join("workspace"),
            PersonalWorkspaceLimits::default(),
            now,
        )?;
        let clean_registry = SkillRegistry::open(
            workspace,
            SkillRoots {
                built_in: directory.path().join("built-in"),
                global: directory.path().join("global"),
                project: directory.path().join("project"),
            },
            SkillLimits {
                max_skill_bytes: self.policy.maximum_candidate_bytes,
                ..SkillLimits::default()
            },
        )?;
        clean_registry.install(source, "clean-workspace validation", now)?;
        for task in &cases.applicability {
            let selection = clean_registry.select(
                &super::SkillSelectionRequest {
                    task: task.clone(),
                    platform: current_platform().into(),
                    ready_tools: ready_tools.clone(),
                    max_prompt_bytes: self.policy.maximum_candidate_bytes,
                    max_skills: 8,
                },
                now,
            )?;
            if !selection
                .selected
                .iter()
                .any(|selected| selected.id == skill_id)
            {
                return Err(SkillSynthesisError::Applicability);
            }
        }
        for task in &cases.regressions {
            let selection = clean_registry.select(
                &super::SkillSelectionRequest {
                    task: task.clone(),
                    platform: current_platform().into(),
                    ready_tools: ready_tools.clone(),
                    max_prompt_bytes: self.policy.maximum_candidate_bytes,
                    max_skills: 8,
                },
                now,
            )?;
            if selection
                .selected
                .iter()
                .any(|selected| selected.id == skill_id)
            {
                return Err(SkillSynthesisError::Regression);
            }
        }
        Ok(())
    }

    fn candidate_path(&self, id: &EntityId) -> PathBuf {
        self.candidate_root.join(format!("{id}.json"))
    }

    fn persist(&self, candidate: &SkillCandidate) -> Result<(), SkillSynthesisError> {
        let path = self.candidate_path(&candidate.id);
        let temporary = path.with_extension(format!("{}.tmp", EntityId::new()));
        let mut file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temporary)?;
        file.write_all(&keith_agent_types::canonical_json_bytes(candidate)?)?;
        file.sync_all()?;
        keith_platform::replace_file(&temporary, &path)?;
        File::open(&self.candidate_root)?.sync_all()?;
        Ok(())
    }
}

fn evidence_strings(outcome: &CommittedWorkflowOutcome) -> Vec<&str> {
    let mut values = vec![
        outcome.workflow_key.as_str(),
        outcome.description.as_str(),
        outcome.trigger.as_str(),
    ];
    for list in [
        &outcome.inputs,
        &outcome.steps,
        &outcome.required_tools,
        &outcome.validation,
        &outcome.known_failures,
        &outcome.stop_conditions,
        &outcome.platforms,
    ] {
        values.extend(list.iter().map(String::as_str));
    }
    values
}

fn validate_cases(
    cases: &CandidateValidationCases,
    policy: &SkillSynthesisPolicy,
) -> Result<(), SkillSynthesisError> {
    if cases.applicability.is_empty() || cases.regressions.is_empty() {
        return Err(SkillSynthesisError::Applicability);
    }
    for value in cases.applicability.iter().chain(&cases.regressions) {
        let normalized = value.to_ascii_lowercase();
        if value.trim().is_empty()
            || has_disallowed_control(value)
            || policy
                .sensitive_markers
                .iter()
                .any(|marker| normalized.contains(marker))
            || contains_secret_shape(value)
        {
            return Err(SkillSynthesisError::PrivateData);
        }
    }
    Ok(())
}

fn learned_skill_id(workflow_key: &str) -> Result<String, SkillSynthesisError> {
    let slug = workflow_key
        .chars()
        .flat_map(char::to_lowercase)
        .map(|character| {
            if character.is_ascii_alphanumeric() {
                character
            } else {
                '-'
            }
        })
        .collect::<String>()
        .split('-')
        .filter(|part| !part.is_empty())
        .collect::<Vec<_>>()
        .join("-");
    let id = format!("learned-{slug}");
    if valid_name(&id) {
        Ok(id)
    } else {
        Err(SkillSynthesisError::Malformed)
    }
}

fn render_candidate(
    manifest: &SkillManifest,
    evidence_count: usize,
) -> Result<String, SkillSynthesisError> {
    let header = toml::to_string(manifest).map_err(|_| SkillSynthesisError::Malformed)?;
    Ok(format!(
        "+++\n{header}+++\n# {}\nProcedure validated from {evidence_count} committed successful outcomes.\n",
        manifest.id
    ))
}

fn contains_secret_shape(value: &str) -> bool {
    value.split_whitespace().any(|token| {
        let trimmed = token.trim_matches(|character: char| {
            !character.is_ascii_alphanumeric() && character != '-' && character != '_'
        });
        (trimmed.starts_with("sk-") && trimmed.len() >= 20)
            || (trimmed.len() >= 40
                && trimmed.chars().all(|character| {
                    character.is_ascii_alphanumeric() || matches!(character, '-' | '_')
                }))
    })
}

fn has_disallowed_control(value: &str) -> bool {
    value
        .chars()
        .any(|character| character.is_control() && !matches!(character, '\n' | '\r' | '\t'))
}

fn create_private_directory(path: &Path) -> Result<(), std::io::Error> {
    fs::create_dir_all(path)?;
    let metadata = fs::symlink_metadata(path)?;
    if metadata.file_type().is_symlink() || !metadata.is_dir() {
        return Err(std::io::Error::other("candidate root is unsafe"));
    }
    Ok(())
}

fn current_platform() -> &'static str {
    std::env::consts::OS
}

#[cfg(test)]
mod tests {
    use super::*;

    fn registry(root: &Path) -> SkillRegistry {
        let workspace = PersonalWorkspace::open(
            root.join("workspace"),
            PersonalWorkspaceLimits::default(),
            UtcTimestamp::UNIX_EPOCH,
        )
        .unwrap();
        SkillRegistry::open(
            workspace,
            SkillRoots {
                built_in: root.join("built-in"),
                global: root.join("global"),
                project: root.join("project"),
            },
            SkillLimits::default(),
        )
        .unwrap()
    }

    fn outcome(at: i64) -> CommittedWorkflowOutcome {
        CommittedWorkflowOutcome {
            outcome_id: EntityId::new(),
            session_id: EntityId::new(),
            entry_id: EntityId::new(),
            workflow_key: "release-check".into(),
            description: "Validate a release candidate".into(),
            trigger: "validate release candidate".into(),
            inputs: vec!["release artifact".into()],
            steps: vec!["inspect manifest".into(), "run release checks".into()],
            required_tools: vec!["shell".into()],
            validation: vec!["all release checks pass".into()],
            known_failures: vec!["artifact is incompatible".into()],
            stop_conditions: vec!["release authority is unavailable".into()],
            platforms: vec![current_platform().into()],
            committed: true,
            successful: true,
            contains_private_data: false,
            improvement_basis_points: 500,
            observed_at: UtcTimestamp::from_unix_millis(at),
        }
    }

    fn cases() -> CandidateValidationCases {
        CandidateValidationCases {
            applicability: vec!["validate release candidate before publishing".into()],
            regressions: vec!["translate poetry into French".into()],
        }
    }

    #[test]
    fn repeated_committed_workflows_require_review_then_activate_and_roll_back() {
        let directory = tempfile::tempdir().unwrap();
        let registry = registry(directory.path());
        let service =
            SkillSynthesisService::new(&registry, SkillSynthesisPolicy::default()).unwrap();
        let outcomes = [outcome(1), outcome(2), outcome(3)];
        let ready_tools = BTreeSet::from(["shell".into()]);
        let candidate = service
            .synthesize(
                &outcomes,
                cases(),
                &ready_tools,
                UtcTimestamp::from_unix_millis(4),
            )
            .unwrap();
        assert_eq!(candidate.state, SkillCandidateState::AwaitingReview);
        assert!(
            !candidate
                .source
                .contains(&outcomes[0].session_id.to_string())
        );
        assert!(matches!(
            service.activate(&candidate.id, UtcTimestamp::from_unix_millis(5)),
            Err(SkillSynthesisError::ReviewRequired)
        ));
        let approved = service
            .review(
                &candidate.id,
                "workspace owner",
                true,
                "validated against release fixtures",
                UtcTimestamp::from_unix_millis(6),
            )
            .unwrap();
        assert_eq!(approved.state, SkillCandidateState::Ready);
        let activated = service
            .activate(&candidate.id, UtcTimestamp::from_unix_millis(7))
            .unwrap();
        assert_eq!(activated.state, SkillCandidateState::Activated);
        let inspection = registry
            .inspect(&candidate.skill_id, UtcTimestamp::from_unix_millis(8))
            .unwrap();
        assert!(
            inspection
                .effective
                .unwrap()
                .provenance
                .origin
                .contains(&candidate.id.to_string())
        );
        let rolled_back = service
            .rollback_activation(&candidate.id, UtcTimestamp::from_unix_millis(9))
            .unwrap();
        assert_eq!(rolled_back.state, SkillCandidateState::RolledBack);
        assert!(
            registry
                .inspect(&candidate.skill_id, UtcTimestamp::from_unix_millis(10))
                .unwrap()
                .effective
                .is_none()
        );
        assert_eq!(service.load(&candidate.id).unwrap(), rolled_back);
    }

    #[test]
    fn failed_private_unsafe_non_improving_and_unavailable_candidates_are_rejected() {
        let directory = tempfile::tempdir().unwrap();
        let registry = registry(directory.path());
        let mut policy = SkillSynthesisPolicy::default();
        policy.blocked_tools.insert("dangerous-delete".into());
        let service = SkillSynthesisService::new(&registry, policy).unwrap();
        let ready_tools = BTreeSet::from(["shell".into(), "dangerous-delete".into()]);

        let mut failed = [outcome(1), outcome(2), outcome(3)];
        failed[1].successful = false;
        assert!(matches!(
            service.synthesize(&failed, cases(), &ready_tools, UtcTimestamp::UNIX_EPOCH),
            Err(SkillSynthesisError::FailedOutcome)
        ));

        let mut private = [outcome(1), outcome(2), outcome(3)];
        private[0].steps[0] = "use password from request".into();
        private[1].steps[0] = "use password from request".into();
        private[2].steps[0] = "use password from request".into();
        assert!(matches!(
            service.synthesize(&private, cases(), &ready_tools, UtcTimestamp::UNIX_EPOCH),
            Err(SkillSynthesisError::PrivateData)
        ));

        let mut unsafe_outcomes = [outcome(1), outcome(2), outcome(3)];
        for value in &mut unsafe_outcomes {
            value.required_tools = vec!["dangerous-delete".into()];
        }
        assert!(matches!(
            service.synthesize(
                &unsafe_outcomes,
                cases(),
                &ready_tools,
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(SkillSynthesisError::Unsafe)
        ));

        assert!(matches!(
            service.synthesize(
                &[outcome(1), outcome(2), outcome(3)],
                cases(),
                &BTreeSet::new(),
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(SkillSynthesisError::UnavailableTools(_))
        ));

        let mut no_gain = [outcome(1), outcome(2), outcome(3)];
        for value in &mut no_gain {
            value.improvement_basis_points = 0;
        }
        assert!(matches!(
            service.synthesize(&no_gain, cases(), &ready_tools, UtcTimestamp::UNIX_EPOCH),
            Err(SkillSynthesisError::NonImproving)
        ));
    }

    #[test]
    fn clean_workspace_regressions_and_duplicate_procedures_block_activation() {
        let directory = tempfile::tempdir().unwrap();
        let registry = registry(directory.path());
        let service =
            SkillSynthesisService::new(&registry, SkillSynthesisPolicy::default()).unwrap();
        let outcomes = [outcome(1), outcome(2), outcome(3)];
        let ready_tools = BTreeSet::from(["shell".into()]);
        let broad_cases = CandidateValidationCases {
            applicability: vec!["validate release candidate".into()],
            regressions: vec!["validate release candidate for poetry".into()],
        };
        assert!(matches!(
            service.synthesize(
                &outcomes,
                broad_cases,
                &ready_tools,
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(SkillSynthesisError::Regression)
        ));

        let first = service
            .synthesize(
                &outcomes,
                cases(),
                &ready_tools,
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        service
            .review(
                &first.id,
                "owner",
                true,
                "approved",
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        service
            .activate(&first.id, UtcTimestamp::from_unix_millis(3))
            .unwrap();
        assert!(matches!(
            service.synthesize(
                &outcomes,
                cases(),
                &ready_tools,
                UtcTimestamp::from_unix_millis(4)
            ),
            Err(SkillSynthesisError::NonImproving)
        ));
    }
}
