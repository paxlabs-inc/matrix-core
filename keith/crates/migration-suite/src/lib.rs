#![forbid(unsafe_code)]

use std::collections::BTreeSet;

use serde::{Deserialize, Serialize};

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MigrationCase {
    LegacyConfiguration,
    ConfigurationLayering,
    InvalidConfigurationFallback,
    LegacyStateBackup,
    FailedStateRollback,
    ProtocolNegotiation,
    LegacySessionExport,
    WorkspaceSchema,
    DerivedIndexRebuild,
    PluginMismatch,
    PluginRollback,
}

impl MigrationCase {
    pub const ALL: [Self; 11] = [
        Self::LegacyConfiguration,
        Self::ConfigurationLayering,
        Self::InvalidConfigurationFallback,
        Self::LegacyStateBackup,
        Self::FailedStateRollback,
        Self::ProtocolNegotiation,
        Self::LegacySessionExport,
        Self::WorkspaceSchema,
        Self::DerivedIndexRebuild,
        Self::PluginMismatch,
        Self::PluginRollback,
    ];
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct MigrationMatrix {
    passed: BTreeSet<MigrationCase>,
}

impl MigrationMatrix {
    pub fn record(&mut self, case: MigrationCase) {
        self.passed.insert(case);
    }

    pub fn missing(&self) -> Vec<MigrationCase> {
        MigrationCase::ALL
            .into_iter()
            .filter(|case| !self.passed.contains(case))
            .collect()
    }

    pub fn is_complete(&self) -> bool {
        self.missing().is_empty()
    }
}

#[cfg(test)]
mod tests {
    use std::collections::{BTreeMap, BTreeSet};
    use std::fs;
    use std::io;
    use std::path::{Path, PathBuf};

    use keith_agent_types::{
        CURRENT_PROTOCOL_VERSION, CURRENT_SCHEMA_VERSION, ClientId, EntityId, EntryId, ProfileId,
        ProtocolVersion, RootTreeId, SessionId, UtcTimestamp, WorkspaceId,
    };
    use keith_configuration::{
        ConfigLayer, ConfigManager, ConfigPatch, ExecutionPatch, LayerKind, RuntimeConfig,
        parse_or_migrate_toml,
    };
    use keith_plugin_host::{PluginHost, PluginHostError};
    use keith_plugin_sdk::{
        MANIFEST_FILE, MODULE_FILE, ManifestError, PluginHook, PluginKind, PluginManifest,
        ResourceGrants,
    };
    use keith_protocol::{
        ClientHello, Feature, ProtocolError, WireFormat, WireMessage, decode_negotiated_bounded,
        encode, negotiate,
    };
    use keith_retrieval::{RankWeights, RetrievalLimits, RetrievalService};
    use keith_session_store::{
        LegacySessionExportLimits, SessionKind, SessionStore, SessionStoreError,
    };
    use keith_state_store::{BackupHook, EmbeddedStore, FileBackupHook, StoreError};
    use keith_workspace::{PersonalWorkspace, PersonalWorkspaceLimits};
    use rusqlite::Connection;
    use tempfile::tempdir;

    use super::*;

    struct FailingBackup;

    impl BackupHook for FailingBackup {
        fn before_migration(
            &self,
            _source: &Path,
            _destination: &Path,
            _from_version: u32,
            _to_version: u32,
        ) -> io::Result<()> {
            Err(io::Error::other("matrix backup failure"))
        }
    }

    fn legacy_database(path: &Path) {
        let connection = Connection::open(path).unwrap();
        connection
            .execute_batch(
                "CREATE TABLE legacy_data(value TEXT NOT NULL);
                 INSERT INTO legacy_data(value) VALUES('preserve-me');
                 PRAGMA user_version = 0;",
            )
            .unwrap();
    }

    fn plugin_package(root: &Path, version: &str, migrate_status: i32) -> PathBuf {
        let package = root.join(format!("matrix-{version}"));
        fs::create_dir_all(&package).unwrap();
        let manifest = PluginManifest {
            manifest_version: 1,
            id: "matrix".into(),
            name: "matrix".into(),
            version: version.into(),
            host_api_min: 1,
            host_api_max: 1,
            kind: PluginKind::WasiComponent,
            hooks: BTreeSet::from([PluginHook::Activate, PluginHook::Migrate]),
            grants: ResourceGrants::default(),
        };
        fs::write(
            package.join(MANIFEST_FILE),
            toml::to_string(&manifest).unwrap(),
        )
        .unwrap();
        fs::write(
            package.join(MODULE_FILE),
            wat::parse_str(format!(
                "(module
                    (func (export \"keith_activate\") (result i32) i32.const 0)
                    (func (export \"keith_migrate\") (result i32) i32.const {migrate_status}))"
            ))
            .unwrap(),
        )
        .unwrap();
        package
    }

    fn valid_plugin_toml() -> String {
        toml::to_string(&PluginManifest {
            manifest_version: 1,
            id: "bounded".into(),
            name: "Bounded".into(),
            version: "1.0.0".into(),
            host_api_min: 1,
            host_api_max: 1,
            kind: PluginKind::WasiComponent,
            hooks: BTreeSet::new(),
            grants: ResourceGrants::default(),
        })
        .unwrap()
    }

    #[test]
    #[allow(clippy::too_many_lines)]
    fn every_supported_upgrade_failure_compatibility_and_rollback_path_runs() {
        let mut matrix = MigrationMatrix::default();

        let legacy = r#"
config_version = 0
data_root = "legacy-data"
max_workers = 4
max_processes = 6
"#;
        let global = parse_or_migrate_toml(legacy).unwrap();
        assert_eq!(global.kind, LayerKind::Global);
        assert_eq!(global.version, CURRENT_SCHEMA_VERSION);
        matrix.record(MigrationCase::LegacyConfiguration);

        let defaults = RuntimeConfig::secure_defaults();
        let mut manager = ConfigManager::new(defaults).unwrap();
        manager.apply(global).unwrap();
        for kind in [
            LayerKind::Profile,
            LayerKind::Workspace,
            LayerKind::Session,
            LayerKind::Action,
        ] {
            manager
                .apply(ConfigLayer::new(kind, ConfigPatch::default()))
                .unwrap();
        }
        assert_eq!(manager.revision().get(), 5);
        matrix.record(MigrationCase::ConfigurationLayering);
        let before = manager.active().clone();
        let notifications = manager.subscribe();
        let invalid = ConfigLayer::new(
            LayerKind::Action,
            ConfigPatch {
                execution: Some(ExecutionPatch {
                    workspace_confinement: Some(false),
                    ..ExecutionPatch::default()
                }),
                ..ConfigPatch::default()
            },
        );
        assert!(manager.apply(invalid).is_err());
        assert_eq!(manager.active(), &before);
        assert!(matches!(
            notifications.recv().unwrap(),
            keith_configuration::ConfigNotification::Rejected { .. }
        ));
        matrix.record(MigrationCase::InvalidConfigurationFallback);

        let state_root = tempdir().unwrap();
        let state_path = state_root.path().join("state.sqlite");
        legacy_database(&state_path);
        let state = EmbeddedStore::open(&state_path, Some(&FileBackupHook)).unwrap();
        assert_eq!(state.schema_version().unwrap(), 1);
        let backups = fs::read_dir(state_root.path())
            .unwrap()
            .map(|entry| entry.unwrap().file_name().to_string_lossy().into_owned())
            .filter(|name| name.contains("pre-v0-to-v1"))
            .count();
        assert_eq!(backups, 1);
        let legacy_value: String = Connection::open(&state_path)
            .unwrap()
            .query_row("SELECT value FROM legacy_data", [], |row| row.get(0))
            .unwrap();
        assert_eq!(legacy_value, "preserve-me");
        matrix.record(MigrationCase::LegacyStateBackup);

        let failed_path = state_root.path().join("failed.sqlite");
        legacy_database(&failed_path);
        assert!(matches!(
            EmbeddedStore::open(&failed_path, Some(&FailingBackup)),
            Err(StoreError::Backup(_))
        ));
        let failed = Connection::open(&failed_path).unwrap();
        let version: u32 = failed
            .pragma_query_value(None, "user_version", |row| row.get(0))
            .unwrap();
        assert_eq!(version, 0);
        let preserved: String = failed
            .query_row("SELECT value FROM legacy_data", [], |row| row.get(0))
            .unwrap();
        assert_eq!(preserved, "preserve-me");
        matrix.record(MigrationCase::FailedStateRollback);

        let hello = ClientHello {
            protocol: CURRENT_PROTOCOL_VERSION,
            client_id: ClientId::new(),
            client_name: "migration-matrix".into(),
            client_version: "0.9.0".into(),
            supported_features: BTreeSet::from([Feature::SessionLifecycle]),
            resume: None,
        };
        let negotiated = negotiate(
            &hello,
            CURRENT_PROTOCOL_VERSION,
            EntityId::new(),
            &hello.supported_features,
        )
        .unwrap();
        let encoded = encode(WireFormat::Json, &WireMessage::ClientHello(hello)).unwrap();
        assert!(matches!(
            decode_negotiated_bounded(
                WireFormat::Json,
                &encoded,
                negotiated.protocol,
                encoded.len() - 1,
            ),
            Err(ProtocolError::MessageTooLarge { .. })
        ));
        assert!(matches!(
            negotiate(
                &ClientHello {
                    protocol: ProtocolVersion::new(CURRENT_PROTOCOL_VERSION.major + 1, 0),
                    client_id: ClientId::new(),
                    client_name: "future".into(),
                    client_version: "2".into(),
                    supported_features: BTreeSet::new(),
                    resume: None,
                },
                CURRENT_PROTOCOL_VERSION,
                EntityId::new(),
                &BTreeSet::new(),
            ),
            Err(ProtocolError::MajorMismatch { .. })
        ));
        matrix.record(MigrationCase::ProtocolNegotiation);

        let entry_id = EntryId::new();
        let session_id = SessionId::new();
        let legacy_manifest = serde_json::to_vec(&serde_json::json!({
            "schema_version": 0,
            "kind": SessionKind::Root,
            "session_id": session_id,
            "root_tree_id": RootTreeId::new(),
            "parent_session_id": null,
            "profile_id": ProfileId::new(),
            "workspace_id": WorkspaceId::new(),
            "created_at": UtcTimestamp::UNIX_EPOCH,
            "active_leaf": entry_id,
            "label": "legacy",
            "branch_labels": BTreeMap::from([("main", entry_id.clone())]),
            "archived": false
        }))
        .unwrap();
        let legacy_history = serde_json::to_vec(&serde_json::json!({
            "id": entry_id,
            "parent_id": null,
            "timestamp": UtcTimestamp::UNIX_EPOCH,
            "payload": {
                "payload": "lifecycle",
                "state": "ready",
                "detail": null
            }
        }))
        .unwrap();
        let before_manifest = legacy_manifest.clone();
        let before_history = legacy_history.clone();
        let migrated = SessionStore::migrate_legacy_export(
            &legacy_manifest,
            &legacy_history,
            LegacySessionExportLimits::default(),
        )
        .unwrap();
        assert_eq!(migrated.version, CURRENT_SCHEMA_VERSION);
        assert_eq!(migrated.entries.len(), 1);
        assert_eq!(legacy_manifest, before_manifest);
        assert_eq!(legacy_history, before_history);
        assert!(matches!(
            SessionStore::migrate_legacy_export(
                &legacy_manifest,
                &legacy_history,
                LegacySessionExportLimits {
                    max_history_bytes: legacy_history.len() - 1,
                    ..LegacySessionExportLimits::default()
                },
            ),
            Err(SessionStoreError::LegacyExportLimit)
        ));
        matrix.record(MigrationCase::LegacySessionExport);

        let workspace_root = tempdir().unwrap();
        let markdown = b"# User memory\nNever rewrite this text.\n";
        fs::write(workspace_root.path().join("MEMORY.md"), markdown).unwrap();
        let workspace = PersonalWorkspace::open(
            workspace_root.path(),
            PersonalWorkspaceLimits::default(),
            UtcTimestamp::UNIX_EPOCH,
        )
        .unwrap();
        assert_eq!(fs::read(workspace.layout().memory).unwrap(), markdown);
        let index: serde_json::Value = serde_json::from_slice(
            &fs::read(workspace_root.path().join(".keith/index.json")).unwrap(),
        )
        .unwrap();
        assert_eq!(index["version"]["major"], CURRENT_SCHEMA_VERSION.major);
        matrix.record(MigrationCase::WorkspaceSchema);
        let retrieval = RetrievalService::open(
            workspace_root.path().join("index"),
            RetrievalLimits::default(),
            RankWeights::default(),
            None,
        )
        .unwrap();
        let profile = ProfileId::new();
        assert_eq!(
            retrieval
                .rebuild_workspace(&profile, workspace_root.path(), UtcTimestamp::UNIX_EPOCH)
                .unwrap(),
            1
        );
        assert!(
            !retrieval
                .search(&profile, "Never rewrite", 5)
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            fs::read(workspace_root.path().join("MEMORY.md")).unwrap(),
            markdown
        );
        matrix.record(MigrationCase::DerivedIndexRebuild);

        let plugin_toml = valid_plugin_toml();
        assert!(PluginManifest::parse_bounded(&plugin_toml, plugin_toml.len()).is_ok());
        assert_eq!(
            PluginManifest::parse_bounded(&plugin_toml, plugin_toml.len() - 1),
            Err(ManifestError::TooLarge)
        );
        let mut incompatible: toml::Value = toml::from_str(&plugin_toml).unwrap();
        incompatible["host_api_min"] = toml::Value::Integer(2);
        assert_eq!(
            PluginManifest::parse(&toml::to_string(&incompatible).unwrap()),
            Err(ManifestError::Incompatible)
        );
        matrix.record(MigrationCase::PluginMismatch);

        let plugin_root = tempdir().unwrap();
        let packages = tempdir().unwrap();
        let first = plugin_package(packages.path(), "1.0.0", 0);
        let second = plugin_package(packages.path(), "2.0.0", 0);
        let failed_plugin = plugin_package(packages.path(), "3.0.0", 7);
        let mut host = PluginHost::open(plugin_root.path(), false).unwrap();
        host.install(first).unwrap();
        host.install(second).unwrap();
        assert!(matches!(
            host.install(failed_plugin),
            Err(PluginHostError::HookStatus(7))
        ));
        assert_eq!(host.record("matrix").unwrap().active_version, "2.0.0");
        host.rollback("matrix", "1.0.0").unwrap();
        assert_eq!(host.record("matrix").unwrap().active_version, "1.0.0");
        matrix.record(MigrationCase::PluginRollback);

        assert!(
            matrix.is_complete(),
            "missing migration cases: {:?}",
            matrix.missing()
        );
    }
}
