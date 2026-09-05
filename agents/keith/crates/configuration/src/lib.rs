#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::path::PathBuf;
use std::sync::mpsc::{self, Receiver, Sender};

use keith_agent_types::{
    CURRENT_SCHEMA_VERSION, ProfileId, Revision, SchemaVersion, TimeZoneName, WorkspaceId,
    canonical_json_bytes,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum LayerKind {
    Global,
    Profile,
    Workspace,
    Session,
    Action,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConfigLayer {
    pub version: SchemaVersion,
    pub kind: LayerKind,
    pub patch: ConfigPatch,
}

impl ConfigLayer {
    pub const fn new(kind: LayerKind, patch: ConfigPatch) -> Self {
        Self {
            version: CURRENT_SCHEMA_VERSION,
            kind,
            patch,
        }
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConfigPatch {
    pub data_root: Option<PathBuf>,
    pub daemon: Option<DaemonPatch>,
    pub execution: Option<ExecutionPatch>,
    pub autonomy: Option<AutonomyPatch>,
    pub retrieval: Option<RetrievalPatch>,
    pub telemetry: Option<TelemetryPatch>,
    pub services: Option<ServiceEnablementPatch>,
    pub profile: Option<ProfilePatch>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ServiceEnablementPatch {
    pub channels: Option<bool>,
    pub acp: Option<bool>,
    pub plugins: Option<bool>,
    pub connected_apps: Option<bool>,
    pub computers: Option<bool>,
    pub teaching: Option<bool>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DaemonPatch {
    pub worker_idle_seconds: Option<u64>,
    pub max_workers: Option<u16>,
    pub event_replay_limit: Option<usize>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExecutionPatch {
    pub workspace_confinement: Option<bool>,
    pub protect_local_network: Option<bool>,
    pub max_processes: Option<u16>,
    pub default_timeout_seconds: Option<u64>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AutonomyPatch {
    pub default_mode: Option<AutonomyMode>,
    pub max_background_actions_per_hour: Option<u16>,
    pub max_notifications_per_day: Option<u16>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RetrievalPatch {
    pub lexical: Option<bool>,
    pub trigram: Option<bool>,
    pub vector: Option<bool>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TelemetryPatch {
    pub local_metrics: Option<bool>,
    pub export: Option<bool>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfilePatch {
    pub replace: Option<AgentProfile>,
    pub model_route: Option<ModelRoute>,
    pub thinking: Option<ThinkingLevel>,
    pub tool_rules: Option<MapOperation<ToolPermission>>,
    pub enabled_skills: Option<ListOperation>,
    pub enabled_mcp_servers: Option<ListOperation>,
    pub enabled_plugins: Option<ListOperation>,
    pub channels: Option<ListOperation>,
    pub service_policy: Option<ProfileServicePolicy>,
    pub autonomy: Option<ProfileAutonomyPatch>,
    pub notifications: Option<NotificationPatch>,
    pub refinement: Option<RefinementPatch>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "operation", content = "values")]
pub enum ListOperation {
    Replace(Vec<String>),
    AppendUnique(Vec<String>),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "operation", content = "values")]
pub enum MapOperation<T> {
    Replace(BTreeMap<String, T>),
    Merge(BTreeMap<String, T>),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RuntimeConfig {
    pub version: SchemaVersion,
    pub data_root: PathBuf,
    pub daemon: DaemonConfig,
    pub execution: ExecutionConfig,
    pub autonomy: AutonomyConfig,
    pub retrieval: RetrievalConfig,
    pub telemetry: TelemetryConfig,
    #[serde(default)]
    pub services: ServiceEnablementConfig,
    pub self_evolution: SelfEvolutionConfig,
    pub profile: Option<AgentProfile>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SelfEvolutionConfig {
    enabled: bool,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ServiceEnablementConfig {
    pub enabled: BTreeSet<PlatformServiceGroup>,
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PlatformServiceGroup {
    Channels,
    Acp,
    Plugins,
    ConnectedApps,
    Computers,
    Teaching,
}

impl ServiceEnablementConfig {
    pub fn set_enabled(&mut self, service: PlatformServiceGroup, enabled: bool) {
        if enabled {
            self.enabled.insert(service);
        } else {
            self.enabled.remove(&service);
        }
    }

    #[must_use]
    pub fn is_enabled(&self, service: PlatformServiceGroup) -> bool {
        self.enabled.contains(&service)
    }
}

impl SelfEvolutionConfig {
    #[must_use]
    pub const fn enabled(&self) -> bool {
        self.enabled
    }
}

impl RuntimeConfig {
    pub fn secure_defaults() -> Self {
        Self {
            version: CURRENT_SCHEMA_VERSION,
            data_root: PathBuf::from("keith-data"),
            daemon: DaemonConfig {
                worker_idle_seconds: 900,
                max_workers: 8,
                event_replay_limit: 10_000,
            },
            execution: ExecutionConfig {
                workspace_confinement: true,
                protect_local_network: true,
                max_processes: 16,
                default_timeout_seconds: 120,
            },
            autonomy: AutonomyConfig {
                default_mode: AutonomyMode::Suggest,
                max_background_actions_per_hour: 4,
                max_notifications_per_day: 12,
            },
            retrieval: RetrievalConfig {
                lexical: true,
                trigram: true,
                vector: false,
            },
            telemetry: TelemetryConfig {
                local_metrics: true,
                export: false,
            },
            services: ServiceEnablementConfig::default(),
            self_evolution: SelfEvolutionConfig { enabled: false },
            profile: None,
        }
    }

    /// # Errors
    ///
    /// Returns an error if the resolved configuration cannot be serialized for inspection.
    pub fn redacted_inspection(&self) -> Result<serde_json::Value, ConfigError> {
        let mut value = serde_json::to_value(self).map_err(ConfigError::Serialize)?;
        value["data_root"] = serde_json::Value::String("<redacted-path>".into());
        if let Some(profile) = value
            .get_mut("profile")
            .and_then(serde_json::Value::as_object_mut)
            && let Some(model) = profile
                .get_mut("model_route")
                .and_then(serde_json::Value::as_object_mut)
            && model
                .get("credential_ref")
                .is_some_and(|value| !value.is_null())
        {
            model.insert(
                "credential_ref".into(),
                serde_json::Value::String("<redacted-reference>".into()),
            );
        }
        Ok(value)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DaemonConfig {
    pub worker_idle_seconds: u64,
    pub max_workers: u16,
    pub event_replay_limit: usize,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExecutionConfig {
    pub workspace_confinement: bool,
    pub protect_local_network: bool,
    pub max_processes: u16,
    pub default_timeout_seconds: u64,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AutonomyConfig {
    pub default_mode: AutonomyMode,
    pub max_background_actions_per_hour: u16,
    pub max_notifications_per_day: u16,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AutonomyMode {
    Off,
    Suggest,
    ConfirmSelected,
    Bounded,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RetrievalConfig {
    pub lexical: bool,
    pub trigram: bool,
    pub vector: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TelemetryConfig {
    pub local_metrics: bool,
    pub export: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AgentProfile {
    pub version: SchemaVersion,
    pub id: ProfileId,
    pub display_name: String,
    pub workspace_id: WorkspaceId,
    pub persona_file: PathBuf,
    pub user_file: PathBuf,
    pub rule_files: Vec<PathBuf>,
    pub model_route: ModelRoute,
    pub thinking: ThinkingLevel,
    pub tool_rules: BTreeMap<String, ToolPermission>,
    pub enabled_skills: Vec<String>,
    pub enabled_mcp_servers: Vec<String>,
    pub enabled_plugins: Vec<String>,
    pub channels: Vec<String>,
    #[serde(default)]
    pub service_policy: ProfileServicePolicy,
    pub autonomy: ProfileAutonomy,
    pub notifications: NotificationSettings,
    pub refinement: RefinementSettings,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ModelRoute {
    pub provider: String,
    pub model: String,
    pub fallbacks: Vec<ModelSelection>,
    pub credential_ref: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ModelSelection {
    pub provider: String,
    pub model: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ThinkingLevel {
    Minimal,
    Low,
    Medium,
    High,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ToolPermission {
    Deny,
    Confirm,
    Allow,
}

impl ToolPermission {
    const fn authority(self) -> u8 {
        match self {
            Self::Deny => 0,
            Self::Confirm => 1,
            Self::Allow => 2,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileAutonomy {
    pub mode: AutonomyMode,
    pub max_children: u16,
    pub max_depth: u16,
    pub daily_token_budget: u64,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileServicePolicy {
    pub allowed_connected_app_toolkits: BTreeSet<String>,
    pub allow_computers: bool,
    pub allow_recording: bool,
    pub allow_recipe_publication: bool,
    pub max_computers: u16,
    pub max_recording_bytes: u64,
    pub max_recipe_steps: u32,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileAutonomyPatch {
    pub mode: Option<AutonomyMode>,
    pub max_children: Option<u16>,
    pub max_depth: Option<u16>,
    pub daily_token_budget: Option<u64>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NotificationSettings {
    pub quiet_hours_start: String,
    pub quiet_hours_end: String,
    pub time_zone: TimeZoneName,
    pub daily_limit: u16,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NotificationPatch {
    pub quiet_hours_start: Option<String>,
    pub quiet_hours_end: Option<String>,
    pub time_zone: Option<TimeZoneName>,
    pub daily_limit: Option<u16>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RefinementSettings {
    pub enabled: bool,
    pub require_confirmation: bool,
    pub editable_targets: BTreeSet<String>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RefinementPatch {
    pub enabled: Option<bool>,
    pub require_confirmation: Option<bool>,
    pub editable_targets: Option<ListOperation>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "status")]
pub enum ConfigNotification {
    Applied {
        revision: Revision,
        layer: LayerKind,
        changed_sections: BTreeSet<String>,
    },
    Rejected {
        revision: Revision,
        layer: LayerKind,
        error: String,
    },
}

#[derive(Debug, Error)]
pub enum ConfigError {
    #[error("unsupported configuration schema version {0}")]
    UnsupportedVersion(SchemaVersion),
    #[error("{field} cannot relax the active safety ceiling")]
    SafetyCeiling { field: &'static str },
    #[error("{field} cannot be changed at the {layer:?} layer")]
    Prohibited {
        field: &'static str,
        layer: LayerKind,
    },
    #[error("profile replacement is required before profile overrides")]
    MissingProfile,
    #[error("invalid configuration: {0}")]
    Invalid(String),
    #[error("configuration serialization failed: {0}")]
    Serialize(serde_json::Error),
    #[error("configuration TOML failed: {0}")]
    Toml(String),
    #[error("persisted resolved configuration does not match its layers")]
    SnapshotMismatch,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct ConfigurationSnapshot {
    version: SchemaVersion,
    revision: Revision,
    layers: BTreeMap<LayerKind, ConfigLayer>,
    active: RuntimeConfig,
}

pub struct ConfigManager {
    defaults: RuntimeConfig,
    active: RuntimeConfig,
    layers: BTreeMap<LayerKind, ConfigLayer>,
    revision: Revision,
    subscribers: Vec<Sender<ConfigNotification>>,
}

impl ConfigManager {
    /// # Errors
    ///
    /// Returns an error when the supplied installation defaults are invalid.
    pub fn new(defaults: RuntimeConfig) -> Result<Self, ConfigError> {
        validate_config(&defaults)?;
        Ok(Self {
            active: defaults.clone(),
            defaults,
            layers: BTreeMap::new(),
            revision: Revision::ZERO,
            subscribers: Vec::new(),
        })
    }

    pub fn active(&self) -> &RuntimeConfig {
        &self.active
    }

    pub const fn revision(&self) -> Revision {
        self.revision
    }

    pub fn subscribe(&mut self) -> Receiver<ConfigNotification> {
        let (sender, receiver) = mpsc::channel();
        self.subscribers.push(sender);
        receiver
    }

    /// # Errors
    ///
    /// Returns an error when the layer is incompatible, invalid, or relaxes a safety ceiling.
    pub fn apply(&mut self, layer: ConfigLayer) -> Result<ConfigNotification, ConfigError> {
        let kind = layer.kind;
        let mut candidate_layers = self.layers.clone();
        candidate_layers.insert(kind, layer);
        let candidate = match resolve_layers(&self.defaults, &candidate_layers) {
            Ok(candidate) => candidate,
            Err(error) => {
                let notification = ConfigNotification::Rejected {
                    revision: self.revision,
                    layer: kind,
                    error: error.to_string(),
                };
                self.notify(&notification);
                return Err(error);
            }
        };
        let changed_sections = changed_sections(&self.active, &candidate);
        self.revision = self
            .revision
            .checked_next()
            .ok_or_else(|| ConfigError::Invalid("configuration revision exhausted".into()))?;
        self.layers = candidate_layers;
        self.active = candidate;
        let notification = ConfigNotification::Applied {
            revision: self.revision,
            layer: kind,
            changed_sections,
        };
        self.notify(&notification);
        Ok(notification)
    }

    /// # Errors
    ///
    /// Returns an error if the active state cannot be serialized canonically.
    pub fn snapshot_bytes(&self) -> Result<Vec<u8>, ConfigError> {
        canonical_json_bytes(&ConfigurationSnapshot {
            version: CURRENT_SCHEMA_VERSION,
            revision: self.revision,
            layers: self.layers.clone(),
            active: self.active.clone(),
        })
        .map_err(ConfigError::Serialize)
    }

    /// # Errors
    ///
    /// Returns an error for invalid, incompatible, or internally inconsistent snapshot bytes.
    pub fn restore(defaults: RuntimeConfig, bytes: &[u8]) -> Result<Self, ConfigError> {
        let snapshot: ConfigurationSnapshot =
            serde_json::from_slice(bytes).map_err(ConfigError::Serialize)?;
        check_version(snapshot.version)?;
        let active = resolve_layers(&defaults, &snapshot.layers)?;
        if active != snapshot.active {
            return Err(ConfigError::SnapshotMismatch);
        }
        Ok(Self {
            defaults,
            active,
            layers: snapshot.layers,
            revision: snapshot.revision,
            subscribers: Vec::new(),
        })
    }

    fn notify(&mut self, notification: &ConfigNotification) {
        self.subscribers
            .retain(|subscriber| subscriber.send(notification.clone()).is_ok());
    }
}

/// # Errors
///
/// Returns an error when the TOML is invalid or has an unsupported schema version.
pub fn parse_or_migrate_toml(input: &str) -> Result<ConfigLayer, ConfigError> {
    if let Ok(layer) = toml::from_str::<ConfigLayer>(input) {
        check_version(layer.version)?;
        return Ok(layer);
    }
    let legacy: LegacyConfigV0 =
        toml::from_str(input).map_err(|error| ConfigError::Toml(error.to_string()))?;
    if legacy.config_version != 0 {
        return Err(ConfigError::UnsupportedVersion(SchemaVersion::new(
            legacy.config_version,
            0,
        )));
    }
    Ok(ConfigLayer::new(
        LayerKind::Global,
        ConfigPatch {
            data_root: Some(legacy.data_root),
            daemon: Some(DaemonPatch {
                max_workers: Some(legacy.max_workers),
                ..DaemonPatch::default()
            }),
            execution: Some(ExecutionPatch {
                max_processes: Some(legacy.max_processes),
                ..ExecutionPatch::default()
            }),
            ..ConfigPatch::default()
        },
    ))
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct LegacyConfigV0 {
    config_version: u16,
    data_root: PathBuf,
    max_workers: u16,
    max_processes: u16,
}

fn check_version(version: SchemaVersion) -> Result<(), ConfigError> {
    if version == CURRENT_SCHEMA_VERSION {
        Ok(())
    } else {
        Err(ConfigError::UnsupportedVersion(version))
    }
}

fn resolve_layers(
    defaults: &RuntimeConfig,
    layers: &BTreeMap<LayerKind, ConfigLayer>,
) -> Result<RuntimeConfig, ConfigError> {
    let mut resolved = defaults.clone();
    for (kind, layer) in layers {
        check_version(layer.version)?;
        if kind != &layer.kind {
            return Err(ConfigError::Invalid(
                "layer map key does not match layer kind".into(),
            ));
        }
        apply_patch(&mut resolved, *kind, &layer.patch)?;
    }
    validate_config(&resolved)?;
    Ok(resolved)
}

fn apply_patch(
    config: &mut RuntimeConfig,
    kind: LayerKind,
    patch: &ConfigPatch,
) -> Result<(), ConfigError> {
    if let Some(data_root) = &patch.data_root {
        if kind > LayerKind::Global {
            return Err(ConfigError::Prohibited {
                field: "data_root",
                layer: kind,
            });
        }
        config.data_root.clone_from(data_root);
    }
    if let Some(daemon) = &patch.daemon {
        lower_ceiling(
            &mut config.daemon.max_workers,
            daemon.max_workers,
            "daemon.max_workers",
        )?;
        lower_ceiling(
            &mut config.daemon.event_replay_limit,
            daemon.event_replay_limit,
            "daemon.event_replay_limit",
        )?;
        if let Some(value) = daemon.worker_idle_seconds {
            config.daemon.worker_idle_seconds = value;
        }
    }
    if let Some(execution) = &patch.execution {
        enforce_true(
            &mut config.execution.workspace_confinement,
            execution.workspace_confinement,
            "execution.workspace_confinement",
        )?;
        enforce_true(
            &mut config.execution.protect_local_network,
            execution.protect_local_network,
            "execution.protect_local_network",
        )?;
        lower_ceiling(
            &mut config.execution.max_processes,
            execution.max_processes,
            "execution.max_processes",
        )?;
        lower_ceiling(
            &mut config.execution.default_timeout_seconds,
            execution.default_timeout_seconds,
            "execution.default_timeout_seconds",
        )?;
    }
    if let Some(autonomy) = &patch.autonomy {
        if let Some(value) = autonomy.default_mode {
            config.autonomy.default_mode = value;
        }
        lower_ceiling(
            &mut config.autonomy.max_background_actions_per_hour,
            autonomy.max_background_actions_per_hour,
            "autonomy.max_background_actions_per_hour",
        )?;
        lower_ceiling(
            &mut config.autonomy.max_notifications_per_day,
            autonomy.max_notifications_per_day,
            "autonomy.max_notifications_per_day",
        )?;
    }
    if let Some(retrieval) = &patch.retrieval {
        replace_if_some(&mut config.retrieval.lexical, retrieval.lexical);
        replace_if_some(&mut config.retrieval.trigram, retrieval.trigram);
        replace_if_some(&mut config.retrieval.vector, retrieval.vector);
    }
    if let Some(telemetry) = &patch.telemetry {
        replace_if_some(&mut config.telemetry.local_metrics, telemetry.local_metrics);
        if telemetry.export == Some(true) && kind > LayerKind::Global {
            return Err(ConfigError::Prohibited {
                field: "telemetry.export",
                layer: kind,
            });
        }
        replace_if_some(&mut config.telemetry.export, telemetry.export);
    }
    if let Some(services) = &patch.services {
        apply_service_patch(&mut config.services, services, kind)?;
    }
    if let Some(profile) = &patch.profile {
        apply_profile_patch(&mut config.profile, kind, profile)?;
    }
    Ok(())
}

fn apply_service_patch(
    config: &mut ServiceEnablementConfig,
    patch: &ServiceEnablementPatch,
    kind: LayerKind,
) -> Result<(), ConfigError> {
    if kind != LayerKind::Global {
        return Err(ConfigError::Prohibited {
            field: "services",
            layer: kind,
        });
    }
    for (service, enabled) in [
        (PlatformServiceGroup::Channels, patch.channels),
        (PlatformServiceGroup::Acp, patch.acp),
        (PlatformServiceGroup::Plugins, patch.plugins),
        (PlatformServiceGroup::ConnectedApps, patch.connected_apps),
        (PlatformServiceGroup::Computers, patch.computers),
        (PlatformServiceGroup::Teaching, patch.teaching),
    ] {
        replace_service_if_some(config, service, enabled);
    }
    Ok(())
}

fn replace_service_if_some(
    config: &mut ServiceEnablementConfig,
    service: PlatformServiceGroup,
    enabled: Option<bool>,
) {
    if let Some(enabled) = enabled {
        config.set_enabled(service, enabled);
    }
}

fn apply_profile_patch(
    target: &mut Option<AgentProfile>,
    kind: LayerKind,
    patch: &ProfilePatch,
) -> Result<(), ConfigError> {
    if let Some(replacement) = &patch.replace {
        if kind != LayerKind::Profile {
            return Err(ConfigError::Prohibited {
                field: "profile.replace",
                layer: kind,
            });
        }
        *target = Some(replacement.clone());
    }
    let profile = target.as_mut().ok_or(ConfigError::MissingProfile)?;
    if let Some(model_route) = &patch.model_route {
        profile.model_route.clone_from(model_route);
    }
    replace_if_some(&mut profile.thinking, patch.thinking);
    if let Some(operation) = &patch.tool_rules {
        apply_tool_rules(&mut profile.tool_rules, operation, kind)?;
    }
    apply_list(&mut profile.enabled_skills, patch.enabled_skills.as_ref());
    apply_list(
        &mut profile.enabled_mcp_servers,
        patch.enabled_mcp_servers.as_ref(),
    );
    apply_list(&mut profile.enabled_plugins, patch.enabled_plugins.as_ref());
    apply_list(&mut profile.channels, patch.channels.as_ref());
    if let Some(service_policy) = &patch.service_policy {
        profile.service_policy.clone_from(service_policy);
    }
    if let Some(autonomy) = &patch.autonomy {
        replace_if_some(&mut profile.autonomy.mode, autonomy.mode);
        lower_ceiling(
            &mut profile.autonomy.max_children,
            autonomy.max_children,
            "profile.autonomy.max_children",
        )?;
        lower_ceiling(
            &mut profile.autonomy.max_depth,
            autonomy.max_depth,
            "profile.autonomy.max_depth",
        )?;
        lower_ceiling(
            &mut profile.autonomy.daily_token_budget,
            autonomy.daily_token_budget,
            "profile.autonomy.daily_token_budget",
        )?;
    }
    if let Some(notifications) = &patch.notifications {
        if let Some(value) = &notifications.quiet_hours_start {
            profile.notifications.quiet_hours_start.clone_from(value);
        }
        if let Some(value) = &notifications.quiet_hours_end {
            profile.notifications.quiet_hours_end.clone_from(value);
        }
        if let Some(value) = &notifications.time_zone {
            profile.notifications.time_zone.clone_from(value);
        }
        lower_ceiling(
            &mut profile.notifications.daily_limit,
            notifications.daily_limit,
            "profile.notifications.daily_limit",
        )?;
    }
    if let Some(refinement) = &patch.refinement {
        if refinement.enabled == Some(true)
            && !profile.refinement.enabled
            && kind > LayerKind::Profile
        {
            return Err(ConfigError::Prohibited {
                field: "profile.refinement.enabled",
                layer: kind,
            });
        }
        replace_if_some(&mut profile.refinement.enabled, refinement.enabled);
        enforce_true(
            &mut profile.refinement.require_confirmation,
            refinement.require_confirmation,
            "profile.refinement.require_confirmation",
        )?;
        if let Some(operation) = &refinement.editable_targets {
            let mut values: Vec<_> = profile
                .refinement
                .editable_targets
                .iter()
                .cloned()
                .collect();
            apply_list(&mut values, Some(operation));
            profile.refinement.editable_targets = values.into_iter().collect();
        }
    }
    Ok(())
}

fn apply_tool_rules(
    target: &mut BTreeMap<String, ToolPermission>,
    operation: &MapOperation<ToolPermission>,
    kind: LayerKind,
) -> Result<(), ConfigError> {
    let values = match operation {
        MapOperation::Replace(values) => {
            if kind > LayerKind::Profile {
                return Err(ConfigError::Prohibited {
                    field: "profile.tool_rules.replace",
                    layer: kind,
                });
            }
            target.clear();
            values
        }
        MapOperation::Merge(values) => values,
    };
    for (tool, permission) in values {
        if target
            .get(tool)
            .is_some_and(|current| permission.authority() > current.authority())
            && kind > LayerKind::Profile
        {
            return Err(ConfigError::SafetyCeiling {
                field: "profile.tool_rules",
            });
        }
        target.insert(tool.clone(), *permission);
    }
    Ok(())
}

fn apply_list(target: &mut Vec<String>, operation: Option<&ListOperation>) {
    match operation {
        Some(ListOperation::Replace(values)) => target.clone_from(values),
        Some(ListOperation::AppendUnique(values)) => {
            for value in values {
                if !target.contains(value) {
                    target.push(value.clone());
                }
            }
        }
        None => {}
    }
}

fn replace_if_some<T: Copy>(target: &mut T, value: Option<T>) {
    if let Some(value) = value {
        *target = value;
    }
}

fn lower_ceiling<T: Copy + PartialOrd>(
    target: &mut T,
    value: Option<T>,
    field: &'static str,
) -> Result<(), ConfigError> {
    if let Some(value) = value {
        if value > *target {
            return Err(ConfigError::SafetyCeiling { field });
        }
        *target = value;
    }
    Ok(())
}

fn enforce_true(
    target: &mut bool,
    value: Option<bool>,
    field: &'static str,
) -> Result<(), ConfigError> {
    if value == Some(false) && *target {
        return Err(ConfigError::SafetyCeiling { field });
    }
    replace_if_some(target, value);
    Ok(())
}

fn validate_config(config: &RuntimeConfig) -> Result<(), ConfigError> {
    if config.version != CURRENT_SCHEMA_VERSION {
        return Err(ConfigError::UnsupportedVersion(config.version));
    }
    if config.daemon.max_workers == 0
        || config.execution.max_processes == 0
        || config.execution.default_timeout_seconds == 0
        || config.daemon.event_replay_limit == 0
    {
        return Err(ConfigError::Invalid(
            "resource ceilings must be non-zero".into(),
        ));
    }
    if !config.retrieval.lexical && !config.retrieval.trigram && !config.retrieval.vector {
        return Err(ConfigError::Invalid(
            "at least one retrieval mode must remain enabled".into(),
        ));
    }
    if let Some(profile) = &config.profile {
        validate_profile(profile)?;
    }
    Ok(())
}

fn validate_profile(profile: &AgentProfile) -> Result<(), ConfigError> {
    if profile.version != CURRENT_SCHEMA_VERSION
        || profile.display_name.trim().is_empty()
        || profile.model_route.provider.trim().is_empty()
        || profile.model_route.model.trim().is_empty()
        || profile.persona_file.as_os_str().is_empty()
        || profile.user_file.as_os_str().is_empty()
        || profile.rule_files.is_empty()
        || profile.autonomy.max_children == 0
        || profile.autonomy.max_depth == 0
        || profile.autonomy.daily_token_budget == 0
        || profile.notifications.daily_limit == 0
        || (profile.service_policy.allow_computers && profile.service_policy.max_computers == 0)
        || (profile.service_policy.allow_recording
            && profile.service_policy.max_recording_bytes == 0)
        || (profile.service_policy.allow_recipe_publication
            && profile.service_policy.max_recipe_steps == 0)
        || profile
            .service_policy
            .allowed_connected_app_toolkits
            .iter()
            .any(|toolkit| toolkit.trim().is_empty())
    {
        return Err(ConfigError::Invalid(
            "profile fields are incomplete or invalid".into(),
        ));
    }
    validate_clock(&profile.notifications.quiet_hours_start)?;
    validate_clock(&profile.notifications.quiet_hours_end)
}

fn validate_clock(value: &str) -> Result<(), ConfigError> {
    let (hour, minute) = value
        .split_once(':')
        .ok_or_else(|| ConfigError::Invalid(format!("invalid clock value {value}")))?;
    let hour = hour
        .parse::<u8>()
        .map_err(|_| ConfigError::Invalid(format!("invalid clock value {value}")))?;
    let minute = minute
        .parse::<u8>()
        .map_err(|_| ConfigError::Invalid(format!("invalid clock value {value}")))?;
    if hour < 24 && minute < 60 {
        Ok(())
    } else {
        Err(ConfigError::Invalid(format!("invalid clock value {value}")))
    }
}

fn changed_sections(before: &RuntimeConfig, after: &RuntimeConfig) -> BTreeSet<String> {
    let mut sections = BTreeSet::new();
    for (name, changed) in [
        ("data_root", before.data_root != after.data_root),
        ("daemon", before.daemon != after.daemon),
        ("execution", before.execution != after.execution),
        ("autonomy", before.autonomy != after.autonomy),
        ("retrieval", before.retrieval != after.retrieval),
        ("telemetry", before.telemetry != after.telemetry),
        ("services", before.services != after.services),
        (
            "self_evolution",
            before.self_evolution != after.self_evolution,
        ),
        ("profile", before.profile != after.profile),
    ] {
        if changed {
            sections.insert(name.to_owned());
        }
    }
    sections
}

#[cfg(test)]
mod tests {
    use super::*;
    use keith_agent_types::EntityId;

    fn profile() -> AgentProfile {
        AgentProfile {
            version: CURRENT_SCHEMA_VERSION,
            id: ProfileId::from(EntityId::parse("01ARZ3NDEKTSV4RRFFQ69G5FAV").unwrap()),
            display_name: "Personal Assistant".into(),
            workspace_id: WorkspaceId::from(EntityId::parse("01ARZ3NDEKTSV4RRFFQ69G5FAW").unwrap()),
            persona_file: "AGENT.md".into(),
            user_file: "USER.md".into(),
            rule_files: vec!["RULE.md".into()],
            model_route: ModelRoute {
                provider: "provider-a".into(),
                model: "model-main".into(),
                fallbacks: vec![ModelSelection {
                    provider: "provider-b".into(),
                    model: "model-fallback".into(),
                }],
                credential_ref: Some("provider-a-personal".into()),
            },
            thinking: ThinkingLevel::Medium,
            tool_rules: BTreeMap::from([
                ("read".into(), ToolPermission::Allow),
                ("shell".into(), ToolPermission::Confirm),
            ]),
            enabled_skills: vec!["coding".into()],
            enabled_mcp_servers: vec!["project".into()],
            enabled_plugins: vec!["built-in".into()],
            channels: vec!["terminal".into()],
            service_policy: ProfileServicePolicy::default(),
            autonomy: ProfileAutonomy {
                mode: AutonomyMode::ConfirmSelected,
                max_children: 3,
                max_depth: 2,
                daily_token_budget: 250_000,
            },
            notifications: NotificationSettings {
                quiet_hours_start: "22:00".into(),
                quiet_hours_end: "07:00".into(),
                time_zone: TimeZoneName::parse("Europe/Berlin").unwrap(),
                daily_limit: 8,
            },
            refinement: RefinementSettings {
                enabled: false,
                require_confirmation: true,
                editable_targets: BTreeSet::from(["memory".into(), "persona".into()]),
            },
        }
    }

    fn profile_layer() -> ConfigLayer {
        ConfigLayer::new(
            LayerKind::Profile,
            ConfigPatch {
                profile: Some(ProfilePatch {
                    replace: Some(profile()),
                    ..ProfilePatch::default()
                }),
                ..ConfigPatch::default()
            },
        )
    }

    #[test]
    fn merge_is_deterministic_and_list_semantics_are_explicit() {
        let mut first = ConfigManager::new(RuntimeConfig::secure_defaults()).unwrap();
        let mut second = ConfigManager::new(RuntimeConfig::secure_defaults()).unwrap();
        for manager in [&mut first, &mut second] {
            manager.apply(profile_layer()).unwrap();
            manager
                .apply(ConfigLayer::new(
                    LayerKind::Workspace,
                    ConfigPatch {
                        profile: Some(ProfilePatch {
                            enabled_skills: Some(ListOperation::AppendUnique(vec![
                                "research".into(),
                                "coding".into(),
                            ])),
                            channels: Some(ListOperation::Replace(vec!["local".into()])),
                            ..ProfilePatch::default()
                        }),
                        ..ConfigPatch::default()
                    },
                ))
                .unwrap();
        }
        assert_eq!(first.active(), second.active());
        let resolved = first.active().profile.as_ref().unwrap();
        assert_eq!(resolved.enabled_skills, ["coding", "research"]);
        assert_eq!(resolved.channels, ["local"]);
    }

    #[test]
    fn self_evolution_is_default_off_and_absent_from_configuration_patches() {
        let manager = ConfigManager::new(RuntimeConfig::secure_defaults()).unwrap();
        assert!(!manager.active().self_evolution.enabled());
        let attempted = r#"
version = { major = 1, minor = 0 }
kind = "global"
[patch.self_evolution]
enabled = true
"#;
        assert!(parse_or_migrate_toml(attempted).is_err());
    }

    #[test]
    fn external_services_are_default_off_and_only_global_configuration_can_enable_them() {
        let mut manager = ConfigManager::new(RuntimeConfig::secure_defaults()).unwrap();
        assert_eq!(
            manager.active().services,
            ServiceEnablementConfig::default()
        );
        manager
            .apply(ConfigLayer::new(
                LayerKind::Global,
                ConfigPatch {
                    services: Some(ServiceEnablementPatch {
                        channels: Some(true),
                        acp: Some(true),
                        ..ServiceEnablementPatch::default()
                    }),
                    ..ConfigPatch::default()
                },
            ))
            .unwrap();
        assert!(
            manager
                .active()
                .services
                .is_enabled(PlatformServiceGroup::Channels)
        );
        assert!(
            manager
                .active()
                .services
                .is_enabled(PlatformServiceGroup::Acp)
        );
        assert!(
            !manager
                .active()
                .services
                .is_enabled(PlatformServiceGroup::Computers)
        );
        let before = manager.active().clone();
        assert!(matches!(
            manager.apply(ConfigLayer::new(
                LayerKind::Session,
                ConfigPatch {
                    services: Some(ServiceEnablementPatch {
                        computers: Some(true),
                        ..ServiceEnablementPatch::default()
                    }),
                    ..ConfigPatch::default()
                },
            )),
            Err(ConfigError::Prohibited {
                field: "services",
                layer: LayerKind::Session
            })
        ));
        assert_eq!(manager.active(), &before);
    }

    #[test]
    fn invalid_narrow_layer_keeps_last_known_good_and_notifies() {
        let mut manager = ConfigManager::new(RuntimeConfig::secure_defaults()).unwrap();
        let changes = manager.subscribe();
        manager.apply(profile_layer()).unwrap();
        let before = manager.active().clone();
        let error = manager
            .apply(ConfigLayer::new(
                LayerKind::Action,
                ConfigPatch {
                    execution: Some(ExecutionPatch {
                        workspace_confinement: Some(false),
                        ..ExecutionPatch::default()
                    }),
                    ..ConfigPatch::default()
                },
            ))
            .unwrap_err();
        assert!(matches!(error, ConfigError::SafetyCeiling { .. }));
        assert_eq!(manager.active(), &before);
        assert!(matches!(
            changes.recv().unwrap(),
            ConfigNotification::Applied { .. }
        ));
        assert!(matches!(
            changes.recv().unwrap(),
            ConfigNotification::Rejected { .. }
        ));
    }

    #[test]
    fn ceilings_and_prohibitions_cannot_be_relaxed() {
        let mut manager = ConfigManager::new(RuntimeConfig::secure_defaults()).unwrap();
        manager
            .apply(ConfigLayer::new(
                LayerKind::Global,
                ConfigPatch {
                    execution: Some(ExecutionPatch {
                        max_processes: Some(8),
                        ..ExecutionPatch::default()
                    }),
                    ..ConfigPatch::default()
                },
            ))
            .unwrap();
        let error = manager
            .apply(ConfigLayer::new(
                LayerKind::Session,
                ConfigPatch {
                    execution: Some(ExecutionPatch {
                        max_processes: Some(9),
                        ..ExecutionPatch::default()
                    }),
                    ..ConfigPatch::default()
                },
            ))
            .unwrap_err();
        assert!(matches!(error, ConfigError::SafetyCeiling { .. }));

        let error = manager
            .apply(ConfigLayer::new(
                LayerKind::Session,
                ConfigPatch {
                    data_root: Some("elsewhere".into()),
                    ..ConfigPatch::default()
                },
            ))
            .unwrap_err();
        assert!(matches!(error, ConfigError::Prohibited { .. }));
    }

    #[test]
    fn narrower_tool_rules_can_reduce_but_not_expand_authority() {
        let mut manager = ConfigManager::new(RuntimeConfig::secure_defaults()).unwrap();
        manager.apply(profile_layer()).unwrap();
        manager
            .apply(ConfigLayer::new(
                LayerKind::Workspace,
                ConfigPatch {
                    profile: Some(ProfilePatch {
                        tool_rules: Some(MapOperation::Merge(BTreeMap::from([(
                            "shell".into(),
                            ToolPermission::Deny,
                        )]))),
                        ..ProfilePatch::default()
                    }),
                    ..ConfigPatch::default()
                },
            ))
            .unwrap();
        let error = manager
            .apply(ConfigLayer::new(
                LayerKind::Action,
                ConfigPatch {
                    profile: Some(ProfilePatch {
                        tool_rules: Some(MapOperation::Merge(BTreeMap::from([(
                            "shell".into(),
                            ToolPermission::Allow,
                        )]))),
                        ..ProfilePatch::default()
                    }),
                    ..ConfigPatch::default()
                },
            ))
            .unwrap_err();
        assert!(matches!(error, ConfigError::SafetyCeiling { .. }));
    }

    #[test]
    fn snapshot_restart_reconstructs_equivalent_active_state() {
        let defaults = RuntimeConfig::secure_defaults();
        let mut manager = ConfigManager::new(defaults.clone()).unwrap();
        manager.apply(profile_layer()).unwrap();
        let bytes = manager.snapshot_bytes().unwrap();
        let restored = ConfigManager::restore(defaults, &bytes).unwrap();
        assert_eq!(restored.active(), manager.active());
        assert_eq!(restored.revision(), manager.revision());
    }

    #[test]
    fn legacy_configuration_migrates_to_versioned_global_layer() {
        let legacy = r#"
config_version = 0
data_root = "legacy-data"
max_workers = 4
max_processes = 6
"#;
        let migrated = parse_or_migrate_toml(legacy).unwrap();
        assert_eq!(migrated.version, CURRENT_SCHEMA_VERSION);
        assert_eq!(migrated.kind, LayerKind::Global);
        assert_eq!(migrated.patch.daemon.unwrap().max_workers, Some(4));
        assert_eq!(migrated.patch.execution.unwrap().max_processes, Some(6));
    }

    #[test]
    fn inspection_redacts_paths_and_credential_references() {
        let mut manager = ConfigManager::new(RuntimeConfig::secure_defaults()).unwrap();
        manager.apply(profile_layer()).unwrap();
        let inspection = manager.active().redacted_inspection().unwrap();
        assert_eq!(inspection["data_root"], "<redacted-path>");
        assert_eq!(
            inspection["profile"]["model_route"]["credential_ref"],
            "<redacted-reference>"
        );
        let text = inspection.to_string();
        assert!(!text.contains("provider-a-personal"));
        assert!(!text.contains("keith-data"));
    }
}
