#![forbid(unsafe_code)]

use std::collections::BTreeSet;
use std::path::{Component, Path, PathBuf};
use std::{fs, io};

use keith_agent_types::{CURRENT_SCHEMA_VERSION, ProfileId, Revision, UtcTimestamp};
pub use keith_configuration::{
    AgentProfile, AutonomyMode, ModelRoute, ModelSelection, NotificationSettings, ProfileAutonomy,
    ProfileServicePolicy, RefinementSettings, ThinkingLevel, ToolPermission,
};
use keith_state_store_core::{ProfileRepository, VersionedRecord, WritePrecondition};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileResources {
    pub workspace_root: PathBuf,
    pub memory_root: PathBuf,
    pub schedule_root: PathBuf,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RegisteredProfile {
    pub profile: AgentProfile,
    pub resources: ProfileResources,
    pub enabled: bool,
    pub authorized_callers: BTreeSet<String>,
    pub revision: Revision,
    pub updated_at: UtcTimestamp,
}

impl RegisteredProfile {
    pub fn id(&self) -> &ProfileId {
        &self.profile.id
    }

    /// Profile, client, channel, plugin, MCP, skill, kernel, and model state is
    /// never installation authority for self-evolution.
    #[must_use]
    pub const fn can_enable_self_evolution(&self) -> bool {
        false
    }

    #[must_use]
    pub fn authorizes_channel(&self, channel: &str) -> bool {
        self.enabled && self.profile.channels.iter().any(|value| value == channel)
    }

    #[must_use]
    pub fn authorizes_plugin(&self, plugin: &str) -> bool {
        self.enabled
            && self
                .profile
                .enabled_plugins
                .iter()
                .any(|value| value == plugin)
    }

    #[must_use]
    pub fn authorizes_connected_app_toolkit(&self, toolkit: &str) -> bool {
        self.enabled
            && self
                .profile
                .service_policy
                .allowed_connected_app_toolkits
                .contains(toolkit)
    }

    #[must_use]
    pub const fn authorizes_computer(&self) -> bool {
        self.enabled && self.profile.service_policy.allow_computers
    }

    #[must_use]
    pub const fn authorizes_recording(&self) -> bool {
        self.enabled && self.profile.service_policy.allow_recording
    }

    #[must_use]
    pub const fn authorizes_recipe_publication(&self) -> bool {
        self.enabled && self.profile.service_policy.allow_recipe_publication
    }
}

#[derive(Debug, Error)]
pub enum ProfileError {
    #[error("profile {0} already exists")]
    AlreadyExists(ProfileId),
    #[error("profile {0} does not exist")]
    Missing(ProfileId),
    #[error("profile revision is stale")]
    Stale,
    #[error("profile is invalid: {0}")]
    Invalid(String),
    #[error("profile resource I/O failed: {0}")]
    Io(#[from] io::Error),
    #[error("profile repository failed: {0}")]
    Repository(String),
    #[error("profile serialization failed: {0}")]
    Serialize(#[from] serde_json::Error),
}

pub struct ProfileRegistry<R> {
    repository: R,
}

impl<R> ProfileRegistry<R>
where
    R: ProfileRepository,
{
    pub const fn new(repository: R) -> Self {
        Self { repository }
    }

    /// Registers a new durable profile after validating its real resource roots.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid resources, duplicate IDs, or persistence failure.
    pub fn register(
        &self,
        mut profile: RegisteredProfile,
    ) -> Result<RegisteredProfile, ProfileError> {
        if self.get(profile.id())?.is_some() {
            return Err(ProfileError::AlreadyExists(profile.profile.id.clone()));
        }
        profile.revision = Revision::ZERO;
        normalize_and_validate(&mut profile)?;
        self.repository
            .put_profile(encode(&profile)?, WritePrecondition::Missing)
            .map_err(repository_error)?;
        Ok(profile)
    }

    /// Replaces a profile only at the caller-observed revision.
    ///
    /// # Errors
    ///
    /// Returns an error for missing/stale profiles, invalid resources, or persistence failure.
    pub fn update(
        &self,
        mut profile: RegisteredProfile,
        expected: Revision,
    ) -> Result<RegisteredProfile, ProfileError> {
        let current = self
            .get(profile.id())?
            .ok_or_else(|| ProfileError::Missing(profile.profile.id.clone()))?;
        if current.revision != expected || profile.revision != expected {
            return Err(ProfileError::Stale);
        }
        profile.revision = expected.checked_next().ok_or(ProfileError::Stale)?;
        normalize_and_validate(&mut profile)?;
        self.repository
            .put_profile(encode(&profile)?, WritePrecondition::Exact(expected))
            .map_err(repository_error)?;
        Ok(profile)
    }

    /// # Errors
    ///
    /// Returns an error when the durable record cannot be loaded or decoded.
    pub fn get(&self, id: &ProfileId) -> Result<Option<RegisteredProfile>, ProfileError> {
        self.repository
            .get_profile(id.as_entity_id())
            .map_err(repository_error)?
            .map(decode)
            .transpose()
    }

    /// # Errors
    ///
    /// Returns an error when durable records cannot be loaded or decoded.
    pub fn list(&self) -> Result<Vec<RegisteredProfile>, ProfileError> {
        let mut profiles = self
            .repository
            .list_profiles()
            .map_err(repository_error)?
            .into_iter()
            .map(decode)
            .collect::<Result<Vec<_>, _>>()?;
        profiles.sort_by(|left, right| left.profile.id.cmp(&right.profile.id));
        Ok(profiles)
    }

    /// Deletes one already-disabled profile at an exact observed revision.
    ///
    /// Cross-service data deletion must complete before this final catalog removal.
    ///
    /// # Errors
    ///
    /// Returns an error for missing, enabled, stale, or uncommitted profile state.
    pub fn delete(
        &self,
        id: &ProfileId,
        expected: Revision,
    ) -> Result<RegisteredProfile, ProfileError> {
        let current = self
            .get(id)?
            .ok_or_else(|| ProfileError::Missing(id.clone()))?;
        if current.enabled {
            return Err(ProfileError::Invalid(
                "profile must be disabled before service data deletion".into(),
            ));
        }
        if current.revision != expected {
            return Err(ProfileError::Stale);
        }
        self.repository
            .delete_profile(id.as_entity_id(), WritePrecondition::Exact(expected))
            .map_err(repository_error)?;
        Ok(current)
    }
}

fn normalize_and_validate(profile: &mut RegisteredProfile) -> Result<(), ProfileError> {
    if profile.profile.version.major != CURRENT_SCHEMA_VERSION.major
        || profile.profile.version.minor > CURRENT_SCHEMA_VERSION.minor
    {
        return Err(ProfileError::Invalid("unsupported profile version".into()));
    }
    if profile.profile.display_name.trim().is_empty()
        || profile.profile.model_route.provider.trim().is_empty()
        || profile.profile.model_route.model.trim().is_empty()
        || profile.authorized_callers.is_empty()
        || profile
            .authorized_callers
            .iter()
            .any(|caller| caller.trim().is_empty())
    {
        return Err(ProfileError::Invalid(
            "identity, model route, and authorized callers must be non-empty".into(),
        ));
    }
    validate_unique_nonempty("skills", &profile.profile.enabled_skills)?;
    validate_unique_nonempty("MCP servers", &profile.profile.enabled_mcp_servers)?;
    validate_unique_nonempty("plugins", &profile.profile.enabled_plugins)?;
    validate_unique_nonempty("channels", &profile.profile.channels)?;
    if profile
        .profile
        .tool_rules
        .keys()
        .any(|name| name.trim().is_empty())
        || profile.profile.autonomy.max_children == 0
        || profile.profile.autonomy.max_depth == 0
        || profile.profile.notifications.daily_limit == 0
    {
        return Err(ProfileError::Invalid(
            "tool names and profile ceilings must be valid".into(),
        ));
    }
    let workspace = canonical_directory(&profile.resources.workspace_root)?;
    let memory = canonical_directory(&profile.resources.memory_root)?;
    let schedules = canonical_directory(&profile.resources.schedule_root)?;
    if !memory.starts_with(&workspace) || !schedules.starts_with(&workspace) || memory == schedules
    {
        return Err(ProfileError::Invalid(
            "memory and schedule roots must be distinct workspace descendants".into(),
        ));
    }
    validate_profile_file(&workspace, &profile.profile.persona_file)?;
    validate_profile_file(&workspace, &profile.profile.user_file)?;
    for rules in &profile.profile.rule_files {
        validate_profile_file(&workspace, rules)?;
    }
    profile.resources.workspace_root = workspace;
    profile.resources.memory_root = memory;
    profile.resources.schedule_root = schedules;
    Ok(())
}

fn validate_unique_nonempty(field: &str, values: &[String]) -> Result<(), ProfileError> {
    let unique = values.iter().collect::<BTreeSet<_>>();
    if unique.len() != values.len() || values.iter().any(|value| value.trim().is_empty()) {
        Err(ProfileError::Invalid(format!(
            "{field} must be unique and non-empty"
        )))
    } else {
        Ok(())
    }
}

fn canonical_directory(path: &Path) -> Result<PathBuf, ProfileError> {
    let canonical = fs::canonicalize(path)?;
    if !canonical.is_dir() {
        return Err(ProfileError::Invalid(format!(
            "{} is not a directory",
            path.display()
        )));
    }
    Ok(canonical)
}

fn validate_profile_file(workspace: &Path, relative: &Path) -> Result<(), ProfileError> {
    if relative.as_os_str().is_empty()
        || relative.is_absolute()
        || relative.components().any(|component| {
            matches!(
                component,
                Component::ParentDir | Component::RootDir | Component::Prefix(_)
            )
        })
    {
        return Err(ProfileError::Invalid(
            "profile file path escapes workspace".into(),
        ));
    }
    let canonical = fs::canonicalize(workspace.join(relative))?;
    if !canonical.starts_with(workspace) || !canonical.is_file() {
        return Err(ProfileError::Invalid(
            "profile file must be a regular workspace file".into(),
        ));
    }
    Ok(())
}

fn encode(profile: &RegisteredProfile) -> Result<VersionedRecord, ProfileError> {
    Ok(VersionedRecord {
        version: CURRENT_SCHEMA_VERSION,
        id: profile.profile.id.as_entity_id().clone(),
        revision: profile.revision,
        updated_at: profile.updated_at,
        payload: serde_json::to_value(profile)?,
    })
}

fn decode(record: VersionedRecord) -> Result<RegisteredProfile, ProfileError> {
    let profile: RegisteredProfile = serde_json::from_value(record.payload)?;
    if profile.profile.id.as_entity_id() != &record.id || profile.revision != record.revision {
        return Err(ProfileError::Invalid(
            "profile record identity or revision mismatch".into(),
        ));
    }
    Ok(profile)
}

fn repository_error(error: impl std::error::Error) -> ProfileError {
    ProfileError::Repository(error.to_string())
}

#[cfg(test)]
mod tests {
    use std::collections::{BTreeMap, BTreeSet};
    use std::fs;

    use keith_agent_types::{ProfileId, TimeZoneName, WorkspaceId};
    use keith_state_store::EmbeddedStore;
    use tempfile::TempDir;

    use super::*;

    fn registered(root: &TempDir) -> RegisteredProfile {
        fs::write(root.path().join("PERSONA.md"), "precise").unwrap();
        fs::write(root.path().join("USER.md"), "operator").unwrap();
        fs::write(root.path().join("RULES.md"), "stay scoped").unwrap();
        fs::create_dir(root.path().join("memory")).unwrap();
        fs::create_dir(root.path().join("schedules")).unwrap();
        RegisteredProfile {
            profile: AgentProfile {
                version: CURRENT_SCHEMA_VERSION,
                id: ProfileId::new(),
                display_name: "work".into(),
                workspace_id: WorkspaceId::new(),
                persona_file: "PERSONA.md".into(),
                user_file: "USER.md".into(),
                rule_files: vec!["RULES.md".into()],
                model_route: ModelRoute {
                    provider: "openai".into(),
                    model: "model-a".into(),
                    fallbacks: vec![],
                    credential_ref: Some("credential-a".into()),
                },
                thinking: ThinkingLevel::High,
                tool_rules: BTreeMap::from([("read".into(), ToolPermission::Allow)]),
                enabled_skills: vec!["research".into()],
                enabled_mcp_servers: vec!["codegraph".into()],
                enabled_plugins: vec!["source".into()],
                channels: vec!["terminal".into()],
                service_policy: keith_configuration::ProfileServicePolicy::default(),
                autonomy: ProfileAutonomy {
                    mode: AutonomyMode::Bounded,
                    max_children: 2,
                    max_depth: 2,
                    daily_token_budget: 10_000,
                },
                notifications: NotificationSettings {
                    quiet_hours_start: "22:00".into(),
                    quiet_hours_end: "08:00".into(),
                    time_zone: TimeZoneName::parse("Europe/Berlin").unwrap(),
                    daily_limit: 4,
                },
                refinement: RefinementSettings {
                    enabled: true,
                    require_confirmation: true,
                    editable_targets: BTreeSet::from(["persona".into()]),
                },
            },
            resources: ProfileResources {
                workspace_root: root.path().into(),
                memory_root: root.path().join("memory"),
                schedule_root: root.path().join("schedules"),
            },
            enabled: true,
            authorized_callers: BTreeSet::from(["operator-a".into()]),
            revision: Revision::ZERO,
            updated_at: UtcTimestamp::UNIX_EPOCH,
        }
    }

    #[test]
    fn durable_registry_validates_resources_and_rejects_stale_updates() {
        let root = TempDir::new().unwrap();
        let store = EmbeddedStore::open_in_memory().unwrap();
        let registry = ProfileRegistry::new(store);
        let profile = registry.register(registered(&root)).unwrap();
        let mut updated = profile.clone();
        updated.profile.display_name = "updated".into();
        updated.updated_at = UtcTimestamp::from_unix_millis(1);
        let updated = registry.update(updated, Revision::ZERO).unwrap();
        assert_eq!(updated.revision, Revision::new(1));
        assert!(matches!(
            registry.update(profile, Revision::ZERO),
            Err(ProfileError::Stale)
        ));
        assert_eq!(registry.list().unwrap(), vec![updated]);
    }

    #[test]
    fn profile_capabilities_never_grant_self_evolution_authority() {
        let root = TempDir::new().unwrap();
        let mut profile = registered(&root);
        profile.profile.enabled_skills.push("self-evolution".into());
        profile
            .profile
            .enabled_mcp_servers
            .push("self-evolution".into());
        profile
            .profile
            .enabled_plugins
            .push("self-evolution".into());
        profile.profile.channels.push("self-evolution".into());
        assert!(!profile.can_enable_self_evolution());
    }

    #[test]
    fn profile_deletion_requires_disabled_exact_revision_state() {
        let root = TempDir::new().unwrap();
        let registry = ProfileRegistry::new(EmbeddedStore::open_in_memory().unwrap());
        let profile = registry.register(registered(&root)).unwrap();
        assert!(matches!(
            registry.delete(&profile.profile.id, profile.revision),
            Err(ProfileError::Invalid(_))
        ));
        let mut disabled = profile.clone();
        disabled.enabled = false;
        disabled.updated_at = UtcTimestamp::from_unix_millis(1);
        let disabled = registry.update(disabled, profile.revision).unwrap();
        assert!(matches!(
            registry.delete(&disabled.profile.id, profile.revision),
            Err(ProfileError::Stale)
        ));
        registry
            .delete(&disabled.profile.id, disabled.revision)
            .unwrap();
        assert!(registry.get(&disabled.profile.id).unwrap().is_none());
    }
}
