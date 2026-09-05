use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;

use keith_agent_types::{SchemaVersion, UtcTimestamp};
use keith_platform_contracts::{ActionRisk, Capability, DemonstrationId, RecipeId};
use serde::{Deserialize, Serialize};

use crate::{ParameterReference, ParameterSource, Rectangle, TaskRecipeError};

pub const TASK_RECIPE_SCHEMA_VERSION: SchemaVersion = SchemaVersion::new(1, 0);

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind", content = "options")]
pub enum RecipeInputKind {
    Text,
    Url,
    File,
    Credential,
    Choice(Vec<String>),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecipeInput {
    pub name: String,
    pub label: String,
    pub kind: RecipeInputKind,
    pub required: bool,
}

impl RecipeInput {
    pub fn parameter(&self) -> ParameterReference {
        ParameterReference {
            name: self.name.clone(),
            source: if self.kind == RecipeInputKind::Credential {
                ParameterSource::NamedCredential
            } else {
                ParameterSource::RuntimeInput
            },
        }
    }

    fn validate(&self) -> Result<(), TaskRecipeError> {
        if !valid_name(&self.name) || self.label.trim().is_empty() {
            return Err(TaskRecipeError::InvalidRecipe(
                "recipe input is malformed".into(),
            ));
        }
        if let RecipeInputKind::Choice(options) = &self.kind
            && (options.is_empty()
                || options.iter().any(|option| option.trim().is_empty())
                || options.iter().collect::<BTreeSet<_>>().len() != options.len())
        {
            return Err(TaskRecipeError::InvalidRecipe(
                "choice input options are empty or duplicated".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "template", content = "value")]
pub enum TemplateValue {
    Literal(String),
    Parameter(ParameterReference),
}

impl TemplateValue {
    fn parameter_name(&self) -> Option<&str> {
        match self {
            Self::Literal(_) => None,
            Self::Parameter(reference) => Some(&reference.name),
        }
    }

    fn validate(&self) -> Result<(), TaskRecipeError> {
        match self {
            Self::Literal(value) if value.trim().is_empty() => Err(TaskRecipeError::InvalidRecipe(
                "empty literal in recipe template".into(),
            )),
            Self::Parameter(reference) if !valid_name(&reference.name) => Err(
                TaskRecipeError::InvalidRecipe("invalid parameter in recipe template".into()),
            ),
            Self::Literal(_) | Self::Parameter(_) => Ok(()),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SemanticTargetSelector {
    pub role: String,
    pub accessible_name: Option<TemplateValue>,
    pub stable_attributes: BTreeMap<String, String>,
}

impl SemanticTargetSelector {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        if self.role.trim().is_empty() {
            return Err(TaskRecipeError::InvalidRecipe(
                "semantic target role is empty".into(),
            ));
        }
        if let Some(name) = &self.accessible_name {
            name.validate()?;
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct VisualFallback {
    pub source_frame_digest: String,
    pub normalized_bounds: Rectangle,
    pub match_threshold_percent: u8,
}

impl VisualFallback {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        if !valid_digest(&self.source_frame_digest)
            || self.normalized_bounds.width == 0
            || self.normalized_bounds.height == 0
            || !(1..=100).contains(&self.match_threshold_percent)
        {
            return Err(TaskRecipeError::InvalidRecipe(
                "visual fallback is malformed".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecipeTarget {
    pub semantic: SemanticTargetSelector,
    pub visual_fallback: Option<VisualFallback>,
}

impl RecipeTarget {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        self.semantic.validate()?;
        if let Some(fallback) = &self.visual_fallback {
            fallback.validate()?;
        }
        Ok(())
    }

    fn parameter_names(&self) -> impl Iterator<Item = &str> {
        self.semantic
            .accessible_name
            .as_ref()
            .and_then(TemplateValue::parameter_name)
            .into_iter()
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ObservationKind {
    Url,
    Window,
    Application,
    Dom,
    Accessibility,
    VisibleText,
    FileExists,
    DownloadCompleted,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ObservationMatcher {
    pub kind: ObservationKind,
    pub description: String,
    pub expected: TemplateValue,
    pub timeout_ms: u64,
}

impl ObservationMatcher {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        if self.description.trim().is_empty() || self.timeout_ms == 0 {
            return Err(TaskRecipeError::InvalidRecipe(
                "expected observation is malformed".into(),
            ));
        }
        self.expected.validate()
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "action", content = "arguments")]
pub enum RecipeAction {
    Navigate {
        url: TemplateValue,
    },
    Activate {
        target: RecipeTarget,
    },
    EnterText {
        target: RecipeTarget,
        value: TemplateValue,
    },
    Select {
        target: RecipeTarget,
        value: TemplateValue,
    },
    Shortcut {
        keys: Vec<String>,
    },
    Upload {
        target: RecipeTarget,
        file: TemplateValue,
    },
    Download {
        target: RecipeTarget,
    },
    Wait {
        duration_ms: u64,
    },
}

impl RecipeAction {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        match self {
            Self::Navigate { url } => url.validate(),
            Self::Activate { target } | Self::Download { target } => target.validate(),
            Self::EnterText { target, value }
            | Self::Select { target, value }
            | Self::Upload {
                target,
                file: value,
            } => {
                target.validate()?;
                value.validate()
            }
            Self::Shortcut { keys } if keys.is_empty() || keys.iter().any(String::is_empty) => Err(
                TaskRecipeError::InvalidRecipe("shortcut keys are empty".into()),
            ),
            Self::Wait { duration_ms: 0 } => Err(TaskRecipeError::InvalidRecipe(
                "wait duration must be positive".into(),
            )),
            Self::Shortcut { .. } | Self::Wait { .. } => Ok(()),
        }
    }

    fn parameter_names(&self) -> Vec<&str> {
        match self {
            Self::Navigate { url } => url.parameter_name().into_iter().collect(),
            Self::Activate { target } | Self::Download { target } => {
                target.parameter_names().collect()
            }
            Self::EnterText { target, value }
            | Self::Select { target, value }
            | Self::Upload {
                target,
                file: value,
            } => target
                .parameter_names()
                .chain(value.parameter_name())
                .collect(),
            Self::Shortcut { .. } | Self::Wait { .. } => Vec::new(),
        }
    }

    fn target_mut(&mut self) -> Option<&mut RecipeTarget> {
        match self {
            Self::Activate { target }
            | Self::EnterText { target, .. }
            | Self::Select { target, .. }
            | Self::Upload { target, .. }
            | Self::Download { target } => Some(target),
            Self::Navigate { .. } | Self::Shortcut { .. } | Self::Wait { .. } => None,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecipeCheckpoint {
    pub name: String,
    pub description: String,
    pub replayable: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ApprovalRequirement {
    pub capability: Capability,
    pub risk: ActionRisk,
    pub reason: String,
    pub invalidate_on_target_change: bool,
}

impl ApprovalRequirement {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        if self.reason.trim().is_empty() || !self.risk.is_consequential() {
            return Err(TaskRecipeError::InvalidRecipe(
                "approval must describe a consequential action".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecoveryBranch {
    pub when: ObservationMatcher,
    pub retry_step_id: Option<String>,
    pub resume_checkpoint: Option<String>,
    pub max_attempts: u32,
}

impl RecoveryBranch {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        self.when.validate()?;
        if self.max_attempts == 0
            || (self.retry_step_id.is_none() == self.resume_checkpoint.is_none())
        {
            return Err(TaskRecipeError::InvalidRecipe(
                "recovery branch must choose exactly one bounded destination".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecipeStep {
    pub id: String,
    pub title: String,
    pub action: RecipeAction,
    pub expected_observations: Vec<ObservationMatcher>,
    pub checkpoint: Option<RecipeCheckpoint>,
    pub approval: Option<ApprovalRequirement>,
    pub recovery: Vec<RecoveryBranch>,
}

impl RecipeStep {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        if !valid_name(&self.id)
            || self.title.trim().is_empty()
            || self.expected_observations.is_empty()
        {
            return Err(TaskRecipeError::InvalidRecipe(
                "recipe step metadata is malformed".into(),
            ));
        }
        self.action.validate()?;
        for observation in &self.expected_observations {
            observation.validate()?;
        }
        if let Some(checkpoint) = &self.checkpoint
            && (!valid_name(&checkpoint.name) || checkpoint.description.trim().is_empty())
        {
            return Err(TaskRecipeError::InvalidRecipe(
                "checkpoint is malformed".into(),
            ));
        }
        if let Some(approval) = &self.approval {
            approval.validate()?;
        }
        for branch in &self.recovery {
            branch.validate()?;
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecipeCheckResult {
    pub passed: bool,
    pub evidence_digest: String,
    pub checked_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecipeQualification {
    pub declared_checks: BTreeSet<String>,
    pub results: BTreeMap<String, RecipeCheckResult>,
    pub accepted_at: Option<UtcTimestamp>,
}

impl RecipeQualification {
    pub fn new(declared_checks: BTreeSet<String>) -> Result<Self, TaskRecipeError> {
        if declared_checks.is_empty() || declared_checks.iter().any(|check| !valid_name(check)) {
            return Err(TaskRecipeError::InvalidRecipe(
                "recipe must declare valid publication checks".into(),
            ));
        }
        Ok(Self {
            declared_checks,
            results: BTreeMap::new(),
            accepted_at: None,
        })
    }

    pub fn record(
        &mut self,
        check: &str,
        passed: bool,
        evidence_digest: impl Into<String>,
        checked_at: UtcTimestamp,
    ) -> Result<(), TaskRecipeError> {
        if !self.declared_checks.contains(check) {
            return Err(TaskRecipeError::InvalidRecipe(
                "check was not declared by this recipe".into(),
            ));
        }
        let evidence_digest = evidence_digest.into();
        if !valid_digest(&evidence_digest) {
            return Err(TaskRecipeError::InvalidRecipe(
                "check evidence digest is malformed".into(),
            ));
        }
        self.results.insert(
            check.into(),
            RecipeCheckResult {
                passed,
                evidence_digest,
                checked_at,
            },
        );
        self.accepted_at = None;
        Ok(())
    }

    pub fn accept(&mut self, accepted_at: UtcTimestamp) -> Result<(), TaskRecipeError> {
        if !self.all_passed() {
            return Err(TaskRecipeError::PublicationNotReady);
        }
        self.accepted_at = Some(accepted_at);
        Ok(())
    }

    pub fn is_publishable(&self) -> bool {
        self.accepted_at.is_some() && self.all_passed()
    }

    fn all_passed(&self) -> bool {
        self.declared_checks
            .iter()
            .all(|check| self.results.get(check).is_some_and(|result| result.passed))
    }

    fn invalidate(&mut self) {
        self.results.clear();
        self.accepted_at = None;
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PublishedRecipe {
    pub skill_id: String,
    pub published_at: UtcTimestamp,
    pub skill_digest: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TaskRecipe {
    pub schema: SchemaVersion,
    pub id: RecipeId,
    pub source_demonstration_id: DemonstrationId,
    pub revision: u64,
    pub parent_revision: Option<u64>,
    pub rollback_of: Option<u64>,
    pub title: String,
    pub description: String,
    pub inputs: Vec<RecipeInput>,
    pub preconditions: Vec<ObservationMatcher>,
    pub steps: Vec<RecipeStep>,
    pub completion_conditions: Vec<ObservationMatcher>,
    pub qualification: RecipeQualification,
    pub published: Option<PublishedRecipe>,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
}

impl TaskRecipe {
    #[allow(clippy::too_many_arguments)]
    pub fn new(
        source_demonstration_id: DemonstrationId,
        title: impl Into<String>,
        description: impl Into<String>,
        inputs: Vec<RecipeInput>,
        preconditions: Vec<ObservationMatcher>,
        steps: Vec<RecipeStep>,
        completion_conditions: Vec<ObservationMatcher>,
        declared_checks: BTreeSet<String>,
        created_at: UtcTimestamp,
    ) -> Result<Self, TaskRecipeError> {
        let recipe = Self {
            schema: TASK_RECIPE_SCHEMA_VERSION,
            id: RecipeId::new(),
            source_demonstration_id,
            revision: 1,
            parent_revision: None,
            rollback_of: None,
            title: title.into(),
            description: description.into(),
            inputs,
            preconditions,
            steps,
            completion_conditions,
            qualification: RecipeQualification::new(declared_checks)?,
            published: None,
            created_at,
            updated_at: created_at,
        };
        recipe.validate()?;
        Ok(recipe)
    }

    pub fn validate(&self) -> Result<(), TaskRecipeError> {
        if self.schema != TASK_RECIPE_SCHEMA_VERSION
            || self.revision == 0
            || self.title.trim().is_empty()
            || self.description.trim().is_empty()
            || self.steps.is_empty()
            || self.completion_conditions.is_empty()
        {
            return Err(TaskRecipeError::InvalidRecipe(
                "recipe metadata is malformed".into(),
            ));
        }
        let mut input_names = BTreeSet::new();
        for input in &self.inputs {
            input.validate()?;
            if !input_names.insert(input.name.as_str()) {
                return Err(TaskRecipeError::InvalidRecipe(
                    "recipe input names are duplicated".into(),
                ));
            }
        }
        for matcher in self.preconditions.iter().chain(&self.completion_conditions) {
            matcher.validate()?;
            if let Some(name) = matcher.expected.parameter_name()
                && !input_names.contains(name)
            {
                return Err(TaskRecipeError::InvalidRecipe(
                    "observation refers to an undeclared parameter".into(),
                ));
            }
        }
        let mut step_ids = BTreeSet::new();
        let mut checkpoint_names = BTreeSet::new();
        for step in &self.steps {
            step.validate()?;
            if !step_ids.insert(step.id.as_str()) {
                return Err(TaskRecipeError::InvalidRecipe(
                    "recipe step IDs are duplicated".into(),
                ));
            }
            if let Some(checkpoint) = &step.checkpoint
                && !checkpoint_names.insert(checkpoint.name.as_str())
            {
                return Err(TaskRecipeError::InvalidRecipe(
                    "checkpoint names are duplicated".into(),
                ));
            }
            for name in step.action.parameter_names() {
                if !input_names.contains(name) {
                    return Err(TaskRecipeError::InvalidRecipe(
                        "step refers to an undeclared parameter".into(),
                    ));
                }
            }
            for observation in &step.expected_observations {
                if let Some(name) = observation.expected.parameter_name()
                    && !input_names.contains(name)
                {
                    return Err(TaskRecipeError::InvalidRecipe(
                        "step observation refers to an undeclared parameter".into(),
                    ));
                }
            }
        }
        for step in &self.steps {
            for branch in &step.recovery {
                if branch
                    .retry_step_id
                    .as_deref()
                    .is_some_and(|id| !step_ids.contains(id))
                    || branch
                        .resume_checkpoint
                        .as_deref()
                        .is_some_and(|name| !checkpoint_names.contains(name))
                {
                    return Err(TaskRecipeError::InvalidRecipe(
                        "recovery branch destination does not exist".into(),
                    ));
                }
            }
        }
        self.validate_qualification()
    }

    pub fn mark_published(
        &mut self,
        skill_id: impl Into<String>,
        skill_digest: impl Into<String>,
        published_at: UtcTimestamp,
    ) -> Result<(), TaskRecipeError> {
        if !self.qualification.is_publishable() {
            return Err(TaskRecipeError::PublicationNotReady);
        }
        let skill_id = skill_id.into();
        let skill_digest = skill_digest.into();
        if !valid_name(&skill_id) || !valid_digest(&skill_digest) {
            return Err(TaskRecipeError::InvalidRecipe(
                "published skill identity is malformed".into(),
            ));
        }
        self.published = Some(PublishedRecipe {
            skill_id,
            published_at,
            skill_digest,
        });
        Ok(())
    }

    pub fn readable_procedure(&self) -> String {
        let mut output = format!("{}\n\n{}\n", self.title, self.description);
        if !self.preconditions.is_empty() {
            output.push_str("\nPreconditions:\n");
            for condition in &self.preconditions {
                writeln!(output, "- {}", condition.description)
                    .expect("writing to a String cannot fail");
            }
        }
        output.push_str("\nProcedure:\n");
        for (index, step) in self.steps.iter().enumerate() {
            writeln!(output, "{}. {}", index + 1, step.title)
                .expect("writing to a String cannot fail");
            if let Some(approval) = &step.approval {
                writeln!(output, "   Confirmation: {}", approval.reason)
                    .expect("writing to a String cannot fail");
            }
            for observation in &step.expected_observations {
                writeln!(output, "   Check: {}", observation.description)
                    .expect("writing to a String cannot fail");
            }
        }
        output.push_str("\nCompletion:\n");
        for condition in &self.completion_conditions {
            writeln!(output, "- {}", condition.description)
                .expect("writing to a String cannot fail");
        }
        output
    }

    fn validate_qualification(&self) -> Result<(), TaskRecipeError> {
        if self.qualification.declared_checks.is_empty()
            || !self
                .qualification
                .results
                .keys()
                .all(|check| self.qualification.declared_checks.contains(check))
            || self
                .qualification
                .results
                .values()
                .any(|result| !valid_digest(&result.evidence_digest))
            || self.qualification.accepted_at.is_some() && !self.qualification.all_passed()
        {
            return Err(TaskRecipeError::InvalidRecipe(
                "recipe qualification state is malformed".into(),
            ));
        }
        if self.published.is_some() && !self.qualification.is_publishable() {
            return Err(TaskRecipeError::InvalidRecipe(
                "published recipe has no accepted passing qualification".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "correction")]
pub enum RecipeCorrection {
    LabelParameter {
        input_name: String,
        label: String,
    },
    RemoveStep {
        step_id: String,
    },
    CorrectTarget {
        step_id: String,
        target: RecipeTarget,
    },
    AddConfirmation {
        step_id: String,
        approval: ApprovalRequirement,
    },
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TaskRecipeHistory {
    pub recipe_id: RecipeId,
    pub active_revision: u64,
    versions: Vec<TaskRecipe>,
}

impl TaskRecipeHistory {
    pub fn new(recipe: TaskRecipe) -> Result<Self, TaskRecipeError> {
        recipe.validate()?;
        Ok(Self {
            recipe_id: recipe.id.clone(),
            active_revision: recipe.revision,
            versions: vec![recipe],
        })
    }

    pub fn versions(&self) -> &[TaskRecipe] {
        &self.versions
    }

    pub fn active(&self) -> Result<&TaskRecipe, TaskRecipeError> {
        self.version(self.active_revision)
    }

    pub fn active_mut(&mut self) -> Result<&mut TaskRecipe, TaskRecipeError> {
        self.versions
            .iter_mut()
            .find(|recipe| recipe.revision == self.active_revision)
            .ok_or_else(|| TaskRecipeError::InvalidRecipe("active revision is missing".into()))
    }

    pub fn version(&self, revision: u64) -> Result<&TaskRecipe, TaskRecipeError> {
        self.versions
            .iter()
            .find(|recipe| recipe.revision == revision)
            .ok_or(TaskRecipeError::NotFound)
    }

    pub fn apply_correction(
        &mut self,
        correction: RecipeCorrection,
        corrected_at: UtcTimestamp,
    ) -> Result<&TaskRecipe, TaskRecipeError> {
        let mut next = self.active()?.clone();
        let parent = next.revision;
        next.revision = self.next_revision()?;
        next.parent_revision = Some(parent);
        next.rollback_of = None;
        next.updated_at = corrected_at;
        next.published = None;
        next.qualification.invalidate();
        apply_correction(&mut next, correction)?;
        next.validate()?;
        self.active_revision = next.revision;
        self.versions.push(next);
        self.active()
    }

    pub fn rollback(
        &mut self,
        revision: u64,
        rolled_back_at: UtcTimestamp,
    ) -> Result<&TaskRecipe, TaskRecipeError> {
        let mut next = self.version(revision)?.clone();
        let parent = self.active_revision;
        next.revision = self.next_revision()?;
        next.parent_revision = Some(parent);
        next.rollback_of = Some(revision);
        next.updated_at = rolled_back_at;
        next.published = None;
        next.qualification.invalidate();
        next.validate()?;
        self.active_revision = next.revision;
        self.versions.push(next);
        self.active()
    }

    pub fn validate(&self) -> Result<(), TaskRecipeError> {
        if self.versions.is_empty()
            || self
                .versions
                .iter()
                .any(|recipe| recipe.id != self.recipe_id)
            || self.active().is_err()
        {
            return Err(TaskRecipeError::InvalidRecipe(
                "recipe history identity is inconsistent".into(),
            ));
        }
        let mut revisions = BTreeSet::new();
        let mut previous_revision = 0_u64;
        for recipe in &self.versions {
            recipe.validate()?;
            if recipe.revision <= previous_revision || !revisions.insert(recipe.revision) {
                return Err(TaskRecipeError::InvalidRecipe(
                    "recipe revisions are duplicated or unordered".into(),
                ));
            }
            previous_revision = recipe.revision;
        }
        for recipe in &self.versions {
            if recipe
                .parent_revision
                .is_some_and(|parent| parent >= recipe.revision || !revisions.contains(&parent))
                || recipe
                    .rollback_of
                    .is_some_and(|target| target >= recipe.revision || !revisions.contains(&target))
            {
                return Err(TaskRecipeError::InvalidRecipe(
                    "recipe revision lineage is inconsistent".into(),
                ));
            }
        }
        if self.active_revision != previous_revision {
            return Err(TaskRecipeError::InvalidRecipe(
                "active recipe revision is not the newest version".into(),
            ));
        }
        Ok(())
    }

    fn next_revision(&self) -> Result<u64, TaskRecipeError> {
        self.versions
            .iter()
            .map(|recipe| recipe.revision)
            .max()
            .and_then(|revision| revision.checked_add(1))
            .ok_or_else(|| TaskRecipeError::InvalidRecipe("recipe revision exhausted".into()))
    }
}

fn apply_correction(
    recipe: &mut TaskRecipe,
    correction: RecipeCorrection,
) -> Result<(), TaskRecipeError> {
    match correction {
        RecipeCorrection::LabelParameter { input_name, label } => {
            let input = recipe
                .inputs
                .iter_mut()
                .find(|input| input.name == input_name)
                .ok_or(TaskRecipeError::NotFound)?;
            if label.trim().is_empty() {
                return Err(TaskRecipeError::InvalidRecipe(
                    "parameter label is empty".into(),
                ));
            }
            input.label = label;
        }
        RecipeCorrection::RemoveStep { step_id } => {
            let old_len = recipe.steps.len();
            recipe.steps.retain(|step| step.id != step_id);
            if recipe.steps.len() == old_len {
                return Err(TaskRecipeError::NotFound);
            }
        }
        RecipeCorrection::CorrectTarget { step_id, target } => {
            target.validate()?;
            let step = recipe
                .steps
                .iter_mut()
                .find(|step| step.id == step_id)
                .ok_or(TaskRecipeError::NotFound)?;
            let corrected_name = target.semantic.accessible_name.as_ref().map_or_else(
                || target.semantic.role.clone(),
                |value| match value {
                    TemplateValue::Literal(value) => value.clone(),
                    TemplateValue::Parameter(reference) => format!("{{{}}}", reference.name),
                },
            );
            let verb = match &step.action {
                RecipeAction::Activate { .. } => "Activate",
                RecipeAction::EnterText { .. } => "Enter text in",
                RecipeAction::Select { .. } => "Select from",
                RecipeAction::Upload { .. } => "Upload a file using",
                RecipeAction::Download { .. } => "Download from",
                RecipeAction::Navigate { .. }
                | RecipeAction::Shortcut { .. }
                | RecipeAction::Wait { .. } => {
                    return Err(TaskRecipeError::InvalidRecipe(
                        "step has no correctable target".into(),
                    ));
                }
            };
            *step.action.target_mut().ok_or_else(|| {
                TaskRecipeError::InvalidRecipe("step has no correctable target".into())
            })? = target;
            step.title = format!("{verb} {corrected_name}");
        }
        RecipeCorrection::AddConfirmation { step_id, approval } => {
            approval.validate()?;
            let step = recipe
                .steps
                .iter_mut()
                .find(|step| step.id == step_id)
                .ok_or(TaskRecipeError::NotFound)?;
            step.approval = Some(approval);
        }
    }
    Ok(())
}

pub(crate) fn valid_digest(value: &str) -> bool {
    value.len() == 64 && value.bytes().all(|byte| byte.is_ascii_hexdigit())
}

pub(crate) fn valid_name(value: &str) -> bool {
    let mut characters = value.chars();
    characters
        .next()
        .is_some_and(|first| first.is_ascii_alphanumeric())
        && value.len() <= 128
        && characters.all(|character| {
            character.is_ascii_alphanumeric() || matches!(character, '-' | '_' | '.')
        })
}
