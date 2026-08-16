#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, ChildId, EntityId, GoalId, Revision, UtcTimestamp,
};
use keith_session_store::{SessionEntryPayload, SessionStoreError, SessionWriter};
use keith_state_store_core::{PlanRepository, VersionedRecord, WritePrecondition};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ExecutionMode {
    Direct,
    SingleTool,
    MultiStep,
    Research,
    CodingProject,
    Monitoring,
    Recurring,
    Delegated,
}

#[derive(Clone, Debug, Eq, PartialEq)]
#[allow(clippy::struct_excessive_bools)]
pub struct ClassificationInput {
    pub request: String,
    pub available_tool_matches: usize,
    pub explicit_plan: bool,
    pub explicit_delegation: bool,
    pub recurring: bool,
    pub monitoring: bool,
    pub high_risk: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct Classification {
    pub mode: ExecutionMode,
    pub plan_required: bool,
    pub reasons: Vec<String>,
}

pub trait RouterAssistant {
    fn suggest(&self, input: &ClassificationInput) -> Option<ExecutionMode>;
}

pub struct TaskRouter;

impl TaskRouter {
    pub fn classify(input: &ClassificationInput) -> Classification {
        Self::classify_with_assistant(input, None)
    }

    pub fn classify_with_assistant(
        input: &ClassificationInput,
        assistant: Option<&dyn RouterAssistant>,
    ) -> Classification {
        let text = input.request.to_ascii_lowercase();
        let (mode, reason) = if input.recurring
            || contains_any(&text, &["every day", "every week", "recurring", "schedule"])
        {
            (ExecutionMode::Recurring, "request has a recurring cadence")
        } else if input.monitoring
            || contains_any(
                &text,
                &["monitor", "watch for", "alert me", "keep checking"],
            )
        {
            (ExecutionMode::Monitoring, "request waits on changing state")
        } else if input.explicit_delegation
            || contains_any(&text, &["delegate", "subagent", "in parallel"])
        {
            (
                ExecutionMode::Delegated,
                "request explicitly delegates work",
            )
        } else if contains_any(
            &text,
            &[
                "implement",
                "refactor",
                "repository",
                "codebase",
                "migration",
            ],
        ) {
            (
                ExecutionMode::CodingProject,
                "request changes a code project",
            )
        } else if contains_any(
            &text,
            &["research", "compare sources", "investigate", "literature"],
        ) {
            (
                ExecutionMode::Research,
                "request requires evidence synthesis",
            )
        } else if input.available_tool_matches == 1 {
            (
                ExecutionMode::SingleTool,
                "one tool directly satisfies the request",
            )
        } else if input.available_tool_matches > 1
            || input.explicit_plan
            || contains_any(&text, &["then", "multiple steps", "coordinate"])
        {
            (ExecutionMode::MultiStep, "request has dependent operations")
        } else if let Some(suggested) = assistant.and_then(|router| router.suggest(input)) {
            (
                suggested,
                "router assistant classified an ambiguous request",
            )
        } else {
            (ExecutionMode::Direct, "request can be answered directly")
        };
        let complex = !matches!(mode, ExecutionMode::Direct | ExecutionMode::SingleTool);
        Classification {
            mode,
            plan_required: input.explicit_plan || input.high_risk || complex,
            reasons: vec![reason.into()],
        }
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PlanState {
    Draft,
    Active,
    Paused,
    Completed,
    Cancelled,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum StepState {
    Pending,
    InProgress,
    Completed,
    Blocked,
    Skipped,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "assignee", content = "id")]
pub enum Assignee {
    Agent,
    User,
    Child(ChildId),
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ResultCheckKind {
    Assertion,
    Command,
    ArtifactExists,
    UserApproval,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ResultCheck {
    pub kind: ResultCheckKind,
    pub description: String,
    pub command: Option<Vec<String>>,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PlanBudget {
    pub token_limit: Option<u64>,
    pub elapsed_ms_limit: Option<u64>,
    pub cost_micros_limit: Option<u64>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PlanStep {
    pub id: EntityId,
    pub milestone: String,
    pub description: String,
    pub dependencies: Vec<EntityId>,
    pub assignee: Assignee,
    pub checks: Vec<ResultCheck>,
    pub budget: PlanBudget,
    pub state: StepState,
    pub result: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PlanRevision {
    pub revision: Revision,
    pub edited_at: UtcTimestamp,
    pub edited_by: String,
    pub edit_reason: String,
    pub restated_outcome: String,
    pub constraints: Vec<String>,
    pub milestones: Vec<String>,
    pub steps: Vec<PlanStep>,
    pub budget: PlanBudget,
    pub state: PlanState,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Plan {
    pub id: EntityId,
    pub goal_id: Option<GoalId>,
    pub created_at: UtcTimestamp,
    pub current_revision: Revision,
    pub revisions: Vec<PlanRevision>,
}

impl Plan {
    /// # Panics
    ///
    /// Panics only for an unvalidated in-memory plan with no revision history.
    pub fn current(&self) -> &PlanRevision {
        self.revisions
            .last()
            .expect("validated plans always contain a revision")
    }

    pub fn revision(&self, revision: Revision) -> Option<&PlanRevision> {
        self.revisions
            .iter()
            .find(|candidate| candidate.revision == revision)
    }

    /// # Errors
    ///
    /// Returns an error when steps, dependencies, checks, or revision history are invalid.
    pub fn validate(&self) -> Result<(), PlanError> {
        if self.revisions.is_empty()
            || self.current_revision != self.current().revision
            || self.current().restated_outcome.trim().is_empty()
            || self.current().milestones.is_empty()
        {
            return Err(PlanError::Invalid(
                "plan outcome, milestones, or revision is missing".into(),
            ));
        }
        for (index, revision) in self.revisions.iter().enumerate() {
            let expected = u64::try_from(index)
                .ok()
                .and_then(|value| value.checked_add(1))
                .map(Revision::new)
                .ok_or_else(|| PlanError::Invalid("revision count overflowed".into()))?;
            if revision.revision != expected {
                return Err(PlanError::Invalid(
                    "revision history is not contiguous".into(),
                ));
            }
        }
        validate_steps(&self.current().steps)
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NewPlan {
    pub goal_id: Option<GoalId>,
    pub restated_outcome: String,
    pub constraints: Vec<String>,
    pub milestones: Vec<String>,
    pub steps: Vec<PlanStep>,
    pub budget: PlanBudget,
    pub state: PlanState,
    pub created_at: UtcTimestamp,
    pub created_by: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct PlanEdit {
    pub restated_outcome: String,
    pub constraints: Vec<String>,
    pub milestones: Vec<String>,
    pub steps: Vec<PlanStep>,
    pub budget: PlanBudget,
    pub state: PlanState,
    pub edited_at: UtcTimestamp,
    pub edited_by: String,
    pub reason: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PlanContext {
    pub plan_id: EntityId,
    pub goal_id: Option<GoalId>,
    pub revision: Revision,
    pub outcome: String,
    pub state: PlanState,
    pub ready_steps: Vec<PlanStep>,
    pub blocked_steps: Vec<PlanStep>,
}

#[derive(Debug, Error)]
pub enum PlanError {
    #[error("plan is invalid: {0}")]
    Invalid(String),
    #[error("plan was not found: {0}")]
    NotFound(EntityId),
    #[error("plan revision conflict")]
    RevisionConflict,
    #[error("plan repository failed: {0}")]
    Repository(String),
    #[error("plan serialization failed: {0}")]
    Serialization(#[from] serde_json::Error),
    #[error("session history failed: {0}")]
    Session(#[from] SessionStoreError),
}

pub struct PlanService<R> {
    repository: R,
}

impl<R> PlanService<R>
where
    R: PlanRepository,
{
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    /// # Errors
    ///
    /// Returns an error when the plan is invalid or cannot be persisted as a new record.
    pub fn create(&self, new: NewPlan) -> Result<Plan, PlanError> {
        let revision = PlanRevision {
            revision: Revision::new(1),
            edited_at: new.created_at,
            edited_by: new.created_by,
            edit_reason: "initial plan".into(),
            restated_outcome: new.restated_outcome,
            constraints: new.constraints,
            milestones: new.milestones,
            steps: new.steps,
            budget: new.budget,
            state: new.state,
        };
        let plan = Plan {
            id: EntityId::new(),
            goal_id: new.goal_id,
            created_at: new.created_at,
            current_revision: Revision::new(1),
            revisions: vec![revision],
        };
        plan.validate()?;
        self.put(&plan, WritePrecondition::Missing)?;
        Ok(plan)
    }

    /// # Errors
    ///
    /// Returns an error when the plan is missing or its record cannot be decoded.
    pub fn get(&self, id: &EntityId) -> Result<Plan, PlanError> {
        let record = self
            .repository
            .get_plan(id)
            .map_err(repository_error)?
            .ok_or_else(|| PlanError::NotFound(id.clone()))?;
        let plan: Plan = serde_json::from_value(record.payload)?;
        plan.validate()?;
        Ok(plan)
    }

    /// # Errors
    ///
    /// Returns an error when the edit is stale, invalid, or cannot be persisted.
    pub fn edit(
        &self,
        id: &EntityId,
        expected_revision: Revision,
        edit: PlanEdit,
    ) -> Result<Plan, PlanError> {
        let mut plan = self.get(id)?;
        if plan.current_revision != expected_revision {
            return Err(PlanError::RevisionConflict);
        }
        let next = expected_revision
            .checked_next()
            .ok_or_else(|| PlanError::Invalid("revision overflowed".into()))?;
        plan.revisions.push(PlanRevision {
            revision: next,
            edited_at: edit.edited_at,
            edited_by: edit.edited_by,
            edit_reason: edit.reason,
            restated_outcome: edit.restated_outcome,
            constraints: edit.constraints,
            milestones: edit.milestones,
            steps: edit.steps,
            budget: edit.budget,
            state: edit.state,
        });
        plan.current_revision = next;
        plan.validate()?;
        self.put(&plan, WritePrecondition::Exact(expected_revision))?;
        Ok(plan)
    }

    /// # Errors
    ///
    /// Returns an error when the plan cannot be loaded or its dependencies are invalid.
    pub fn context(&self, id: &EntityId) -> Result<PlanContext, PlanError> {
        let plan = self.get(id)?;
        let current = plan.current();
        let completed = current
            .steps
            .iter()
            .filter(|step| step.state == StepState::Completed)
            .map(|step| step.id.clone())
            .collect::<BTreeSet<_>>();
        let ready_steps = current
            .steps
            .iter()
            .filter(|step| {
                step.state == StepState::Pending
                    && step
                        .dependencies
                        .iter()
                        .all(|dependency| completed.contains(dependency))
            })
            .cloned()
            .collect();
        let blocked_steps = current
            .steps
            .iter()
            .filter(|step| {
                step.state == StepState::Blocked
                    || (step.state == StepState::Pending
                        && !step
                            .dependencies
                            .iter()
                            .all(|dependency| completed.contains(dependency)))
            })
            .cloned()
            .collect();
        Ok(PlanContext {
            plan_id: plan.id.clone(),
            goal_id: plan.goal_id.clone(),
            revision: plan.current_revision,
            outcome: current.restated_outcome.clone(),
            state: current.state,
            ready_steps,
            blocked_steps,
        })
    }

    /// # Errors
    ///
    /// Returns an error when the plan marker cannot be appended durably.
    pub fn append_session_marker(
        &self,
        plan: &Plan,
        writer: &mut SessionWriter,
        timestamp: UtcTimestamp,
    ) -> Result<(), PlanError> {
        let parent = writer.manifest().active_leaf.clone();
        writer.append(
            parent,
            timestamp,
            SessionEntryPayload::PlanChanged {
                plan_id: plan.id.clone(),
                revision: plan.current_revision.get(),
            },
        )?;
        Ok(())
    }

    fn put(&self, plan: &Plan, precondition: WritePrecondition) -> Result<(), PlanError> {
        self.repository
            .put_plan(
                VersionedRecord {
                    version: CURRENT_SCHEMA_VERSION,
                    id: plan.id.clone(),
                    revision: plan.current_revision,
                    updated_at: plan.current().edited_at,
                    payload: serde_json::to_value(plan)?,
                },
                precondition,
            )
            .map_err(repository_error)?;
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EvaluationCase {
    pub name: &'static str,
    pub input: ClassificationInput,
    pub expected_mode: ExecutionMode,
    pub persisted_plan_expected: bool,
    pub review_expected: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EvaluationObservation {
    pub name: String,
    pub mode: ExecutionMode,
    pub quality_milliscore: u16,
    pub latency_ms: u64,
    pub cost_micros: u64,
    pub failed: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EvaluationReport {
    pub total: usize,
    pub routing_matches: usize,
    pub observations: Vec<EvaluationObservation>,
}

pub fn fixed_evaluation_corpus() -> Vec<EvaluationCase> {
    vec![
        evaluation("direct", "Explain this error", 0, ExecutionMode::Direct),
        evaluation(
            "single_tool",
            "Read this file",
            1,
            ExecutionMode::SingleTool,
        ),
        evaluation(
            "multi_step",
            "Fetch the data then summarize it",
            2,
            ExecutionMode::MultiStep,
        ),
        evaluation(
            "research",
            "Research and compare sources",
            0,
            ExecutionMode::Research,
        ),
        evaluation(
            "coding",
            "Implement this repository migration",
            0,
            ExecutionMode::CodingProject,
        ),
        evaluation(
            "monitoring",
            "Monitor the endpoint",
            0,
            ExecutionMode::Monitoring,
        ),
        evaluation(
            "recurring",
            "Run this every day",
            0,
            ExecutionMode::Recurring,
        ),
        EvaluationCase {
            name: "delegated",
            input: ClassificationInput {
                request: "Delegate the independent investigation".into(),
                available_tool_matches: 0,
                explicit_plan: false,
                explicit_delegation: true,
                recurring: false,
                monitoring: false,
                high_risk: false,
            },
            expected_mode: ExecutionMode::Delegated,
            persisted_plan_expected: true,
            review_expected: true,
        },
    ]
}

pub fn evaluate_router(observations: Vec<EvaluationObservation>) -> EvaluationReport {
    let corpus = fixed_evaluation_corpus();
    let expected = corpus
        .iter()
        .map(|case| (case.name, case.expected_mode))
        .collect::<BTreeMap<_, _>>();
    let routing_matches = observations
        .iter()
        .filter(|observation| expected.get(observation.name.as_str()) == Some(&observation.mode))
        .count();
    EvaluationReport {
        total: corpus.len(),
        routing_matches,
        observations,
    }
}

fn evaluation(
    name: &'static str,
    request: &str,
    tools: usize,
    expected_mode: ExecutionMode,
) -> EvaluationCase {
    let complex = !matches!(
        expected_mode,
        ExecutionMode::Direct | ExecutionMode::SingleTool
    );
    EvaluationCase {
        name,
        input: ClassificationInput {
            request: request.into(),
            available_tool_matches: tools,
            explicit_plan: false,
            explicit_delegation: false,
            recurring: false,
            monitoring: false,
            high_risk: false,
        },
        expected_mode,
        persisted_plan_expected: complex,
        review_expected: matches!(
            expected_mode,
            ExecutionMode::Research | ExecutionMode::CodingProject | ExecutionMode::Delegated
        ),
    }
}

fn contains_any(text: &str, terms: &[&str]) -> bool {
    terms.iter().any(|term| text.contains(term))
}

fn repository_error(error: impl std::fmt::Display) -> PlanError {
    PlanError::Repository(error.to_string())
}

fn validate_steps(steps: &[PlanStep]) -> Result<(), PlanError> {
    if steps.is_empty() {
        return Err(PlanError::Invalid(
            "a complex plan needs at least one step".into(),
        ));
    }
    let ids = steps
        .iter()
        .map(|step| step.id.clone())
        .collect::<BTreeSet<_>>();
    if ids.len() != steps.len() {
        return Err(PlanError::Invalid("step IDs are not unique".into()));
    }
    if steps.iter().any(|step| {
        step.milestone.trim().is_empty()
            || step.description.trim().is_empty()
            || step.checks.is_empty()
            || step
                .dependencies
                .iter()
                .any(|dependency| !ids.contains(dependency) || *dependency == step.id)
    }) {
        return Err(PlanError::Invalid(
            "steps require descriptions, checks, and valid dependencies".into(),
        ));
    }
    let dependencies = steps
        .iter()
        .map(|step| (step.id.clone(), step.dependencies.clone()))
        .collect::<BTreeMap<_, _>>();
    for id in ids {
        detect_cycle(
            &id,
            &dependencies,
            &mut BTreeSet::new(),
            &mut BTreeSet::new(),
        )?;
    }
    Ok(())
}

fn detect_cycle(
    id: &EntityId,
    dependencies: &BTreeMap<EntityId, Vec<EntityId>>,
    visiting: &mut BTreeSet<EntityId>,
    visited: &mut BTreeSet<EntityId>,
) -> Result<(), PlanError> {
    if visited.contains(id) {
        return Ok(());
    }
    if !visiting.insert(id.clone()) {
        return Err(PlanError::Invalid(
            "step dependencies contain a cycle".into(),
        ));
    }
    if let Some(required) = dependencies.get(id) {
        for dependency in required {
            detect_cycle(dependency, dependencies, visiting, visited)?;
        }
    }
    visiting.remove(id);
    visited.insert(id.clone());
    Ok(())
}

#[cfg(test)]
mod tests {
    use keith_agent_types::{Generation, ProfileId, RootTreeId, SessionId, WorkerId, WorkspaceId};
    use keith_session_store::{NewSession, SessionKind, SessionStore, WriterIdentity};
    use keith_state_store::EmbeddedStore;

    use super::*;

    fn check(description: &str) -> ResultCheck {
        ResultCheck {
            kind: ResultCheckKind::Assertion,
            description: description.into(),
            command: None,
        }
    }

    fn steps() -> Vec<PlanStep> {
        let first = EntityId::new();
        vec![
            PlanStep {
                id: first.clone(),
                milestone: "understand".into(),
                description: "Inspect the inputs".into(),
                dependencies: Vec::new(),
                assignee: Assignee::Agent,
                checks: vec![check("inputs are understood")],
                budget: PlanBudget {
                    token_limit: Some(1_000),
                    ..PlanBudget::default()
                },
                state: StepState::Pending,
                result: None,
            },
            PlanStep {
                id: EntityId::new(),
                milestone: "deliver".into(),
                description: "Produce the checked result".into(),
                dependencies: vec![first],
                assignee: Assignee::Child(ChildId::new()),
                checks: vec![check("result passes")],
                budget: PlanBudget {
                    elapsed_ms_limit: Some(10_000),
                    ..PlanBudget::default()
                },
                state: StepState::Pending,
                result: None,
            },
        ]
    }

    fn new_plan() -> NewPlan {
        NewPlan {
            goal_id: Some(GoalId::new()),
            restated_outcome: "Deliver a verified result".into(),
            constraints: vec!["preserve user data".into()],
            milestones: vec!["understand".into(), "deliver".into()],
            steps: steps(),
            budget: PlanBudget {
                token_limit: Some(5_000),
                elapsed_ms_limit: Some(60_000),
                cost_micros_limit: Some(50_000),
            },
            state: PlanState::Active,
            created_at: UtcTimestamp::from_unix_millis(10),
            created_by: "agent".into(),
        }
    }

    #[test]
    fn fixed_corpus_routes_every_mode_without_plans_for_direct_work() {
        let corpus = fixed_evaluation_corpus();
        let observations = corpus
            .iter()
            .map(|case| {
                let classified = TaskRouter::classify(&case.input);
                assert_eq!(classified.mode, case.expected_mode, "{}", case.name);
                assert_eq!(classified.plan_required, case.persisted_plan_expected);
                EvaluationObservation {
                    name: case.name.into(),
                    mode: classified.mode,
                    quality_milliscore: 900,
                    latency_ms: 10,
                    cost_micros: 20,
                    failed: false,
                }
            })
            .collect();
        let report = evaluate_router(observations);
        assert_eq!(report.routing_matches, report.total);
        let high_risk_direct = TaskRouter::classify(&ClassificationInput {
            request: "Explain this".into(),
            available_tool_matches: 0,
            explicit_plan: false,
            explicit_delegation: false,
            recurring: false,
            monitoring: false,
            high_risk: true,
        });
        assert_eq!(high_risk_direct.mode, ExecutionMode::Direct);
        assert!(high_risk_direct.plan_required);
    }

    #[test]
    fn durable_edits_preserve_revision_history_and_reject_stale_writers() {
        let service = PlanService::new(EmbeddedStore::open_in_memory().unwrap());
        let original = service.create(new_plan()).unwrap();
        assert_eq!(service.get(&original.id).unwrap(), original);
        let original_outcome = original.current().restated_outcome.clone();
        let mut edited_steps = original.current().steps.clone();
        edited_steps[0].state = StepState::Completed;
        edited_steps[0].result = Some("inspected".into());
        let edited = service
            .edit(
                &original.id,
                Revision::new(1),
                PlanEdit {
                    restated_outcome: "Deliver a verified and documented result".into(),
                    constraints: original.current().constraints.clone(),
                    milestones: original.current().milestones.clone(),
                    steps: edited_steps,
                    budget: original.current().budget,
                    state: PlanState::Active,
                    edited_at: UtcTimestamp::from_unix_millis(20),
                    edited_by: "user".into(),
                    reason: "clarified documentation".into(),
                },
            )
            .unwrap();
        assert_eq!(edited.current_revision, Revision::new(2));
        assert_eq!(
            edited.revision(Revision::new(1)).unwrap().restated_outcome,
            original_outcome
        );
        assert_eq!(edited.current().edited_by, "user");
        let stale = service.edit(
            &original.id,
            Revision::new(1),
            PlanEdit {
                restated_outcome: "stale".into(),
                constraints: Vec::new(),
                milestones: vec!["stale".into()],
                steps: steps(),
                budget: PlanBudget::default(),
                state: PlanState::Active,
                edited_at: UtcTimestamp::from_unix_millis(30),
                edited_by: "old client".into(),
                reason: "stale edit".into(),
            },
        );
        assert!(matches!(stale, Err(PlanError::RevisionConflict)));
    }

    #[test]
    fn context_exposes_ready_and_child_dispatch_steps() {
        let service = PlanService::new(EmbeddedStore::open_in_memory().unwrap());
        let plan = service.create(new_plan()).unwrap();
        let initial = service.context(&plan.id).unwrap();
        assert_eq!(initial.ready_steps.len(), 1);
        assert_eq!(initial.blocked_steps.len(), 1);

        let mut completed = plan.current().steps.clone();
        completed[0].state = StepState::Completed;
        completed[0].result = Some("done".into());
        let updated = service
            .edit(
                &plan.id,
                Revision::new(1),
                PlanEdit {
                    restated_outcome: plan.current().restated_outcome.clone(),
                    constraints: plan.current().constraints.clone(),
                    milestones: plan.current().milestones.clone(),
                    steps: completed,
                    budget: plan.current().budget,
                    state: PlanState::Active,
                    edited_at: UtcTimestamp::from_unix_millis(20),
                    edited_by: "agent".into(),
                    reason: "first step completed".into(),
                },
            )
            .unwrap();
        let context = service.context(&updated.id).unwrap();
        assert_eq!(context.ready_steps.len(), 1);
        assert!(matches!(
            context.ready_steps[0].assignee,
            Assignee::Child(_)
        ));
        assert!(context.blocked_steps.is_empty());
    }

    #[test]
    fn session_marker_links_the_durable_plan_revision() {
        let service = PlanService::new(EmbeddedStore::open_in_memory().unwrap());
        let plan = service.create(new_plan()).unwrap();
        let directory = tempfile::tempdir().unwrap();
        let sessions = SessionStore::open(directory.path()).unwrap();
        let session_id = SessionId::new();
        sessions
            .create(NewSession {
                kind: SessionKind::Root,
                session_id: session_id.clone(),
                root_tree_id: RootTreeId::new(),
                parent_session_id: None,
                profile_id: ProfileId::new(),
                workspace_id: WorkspaceId::new(),
                created_at: UtcTimestamp::UNIX_EPOCH,
                label: None,
                profile_snapshot: None,
            })
            .unwrap();
        let mut writer = sessions
            .acquire_writer(
                &session_id,
                WriterIdentity {
                    worker_id: WorkerId::new(),
                    owner_instance: EntityId::new(),
                    generation: Generation::new(1),
                    acquired_at: UtcTimestamp::UNIX_EPOCH,
                },
            )
            .unwrap();
        service
            .append_session_marker(&plan, &mut writer, UtcTimestamp::from_unix_millis(50))
            .unwrap();
        drop(writer);
        let manifest = sessions.manifest(&session_id).unwrap();
        let ancestry = sessions
            .load_index(&session_id)
            .unwrap()
            .ancestry(manifest.active_leaf.as_ref().unwrap())
            .unwrap();
        assert!(matches!(
            &ancestry[0].payload,
            SessionEntryPayload::PlanChanged { plan_id, revision }
                if plan_id == &plan.id && *revision == 1
        ));
    }

    #[test]
    fn cyclic_or_checkless_steps_are_rejected_before_persistence() {
        let service = PlanService::new(EmbeddedStore::open_in_memory().unwrap());
        let mut invalid = new_plan();
        invalid.steps[0].dependencies = vec![invalid.steps[1].id.clone()];
        assert!(matches!(
            service.create(invalid),
            Err(PlanError::Invalid(_))
        ));
        let mut invalid = new_plan();
        invalid.steps[0].checks.clear();
        assert!(matches!(
            service.create(invalid),
            Err(PlanError::Invalid(_))
        ));
    }
}
