use std::fmt::Write as _;

use keith_agent_types::UtcTimestamp;
use keith_platform_contracts::RecipeId;
use keith_skills::{SkillManifest, SkillPackage, SkillRegistry};
use serde::{Deserialize, Serialize};

use crate::{TaskRecipe, TaskRecipeError};

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SkillPublicationOptions {
    pub skill_id: String,
    pub triggers: Vec<String>,
    pub required_tools: Vec<String>,
    pub platforms: Vec<String>,
}

impl SkillPublicationOptions {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        if !valid_skill_name(&self.skill_id)
            || self.triggers.is_empty()
            || self
                .triggers
                .iter()
                .any(|trigger| trigger.trim().is_empty())
            || self
                .required_tools
                .iter()
                .any(|tool| !valid_skill_name(tool))
        {
            return Err(TaskRecipeError::InvalidRecipe(
                "skill publication options are malformed".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SkillPublication {
    pub recipe_id: RecipeId,
    pub recipe_revision: u64,
    pub manifest: SkillManifest,
    pub source: String,
    pub origin: String,
}

impl SkillPublication {
    pub fn install(
        &self,
        registry: &SkillRegistry,
        now: UtcTimestamp,
    ) -> Result<SkillPackage, TaskRecipeError> {
        registry
            .install(self.source.clone(), self.origin.clone(), now)
            .map_err(TaskRecipeError::from)
    }

    pub fn update(
        &self,
        registry: &SkillRegistry,
        expected_digest: &str,
        now: UtcTimestamp,
    ) -> Result<SkillPackage, TaskRecipeError> {
        registry
            .update(&self.manifest.id, expected_digest, self.source.clone(), now)
            .map_err(TaskRecipeError::from)
    }
}

impl TaskRecipe {
    pub fn skill_publication(
        &self,
        options: SkillPublicationOptions,
    ) -> Result<SkillPublication, TaskRecipeError> {
        self.validate()?;
        options.validate()?;
        if !self.qualification.is_publishable() {
            return Err(TaskRecipeError::PublicationNotReady);
        }
        let inputs = if self.inputs.is_empty() {
            vec!["no runtime inputs".into()]
        } else {
            self.inputs
                .iter()
                .map(|input| format!("{}: {}", input.name, input.label))
                .collect()
        };
        let known_failures = self
            .steps
            .iter()
            .flat_map(|step| {
                step.recovery
                    .iter()
                    .map(|branch| format!("{}: {}", step.title, branch.when.description))
            })
            .collect::<Vec<_>>();
        let known_failures = if known_failures.is_empty() {
            vec!["observed state differs from the accepted demonstration".into()]
        } else {
            known_failures
        };
        let stop_conditions = self
            .steps
            .iter()
            .filter_map(|step| {
                step.approval
                    .as_ref()
                    .map(|approval| format!("stop for approval: {}", approval.reason))
            })
            .collect::<Vec<_>>();
        let stop_conditions = if stop_conditions.is_empty() {
            vec!["required authority or expected observation is unavailable".into()]
        } else {
            stop_conditions
        };
        let manifest = SkillManifest {
            id: options.skill_id,
            version: self.revision.to_string(),
            description: self.description.clone(),
            triggers: options.triggers,
            inputs,
            steps: self.steps.iter().map(|step| step.title.clone()).collect(),
            required_tools: options.required_tools,
            validation: self.qualification.declared_checks.iter().cloned().collect(),
            known_failures,
            stop_conditions,
            platforms: options.platforms,
        };
        let manifest_source = toml::to_string(&manifest)?;
        let mut source = String::with_capacity(manifest_source.len() + 512);
        source.push_str("+++\n");
        source.push_str(&manifest_source);
        source.push_str("+++\n");
        writeln!(source, "# {}\n", self.title).expect("writing to a String cannot fail");
        source.push_str(&self.readable_procedure());
        Ok(SkillPublication {
            recipe_id: self.id.clone(),
            recipe_revision: self.revision,
            manifest,
            source,
            origin: format!("task-recipe:{}:revision-{}", self.id, self.revision),
        })
    }
}

fn valid_skill_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 96
        && value
            .chars()
            .all(|character| character.is_ascii_alphanumeric() || matches!(character, '-' | '_'))
}
