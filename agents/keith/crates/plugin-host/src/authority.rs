use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::fs;
use std::path::{Path, PathBuf};
use std::sync::Arc;

use ed25519_dalek::{Signature, VerifyingKey};
use keith_plugin_sdk::{
    HOST_API_VERSION, HostRequest, MANIFEST_FILE, MODULE_FILE, PayloadFormat, PluginGrant,
    PluginHook, PluginManifest, PluginOperation, PluginPublisher, PluginStatus,
    PluginToolDescriptor,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{ExecutablePlugin, PluginAbiError, PluginHostContext};

const AUTHORITY_LEDGER_FILE: &str = "plugin-authority.json";
const PACKAGES_DIRECTORY: &str = "packages";
const MAX_MANIFEST_BYTES: u64 = 64 * 1_024;
const MAX_MODULE_BYTES: u64 = 32 * 1_024 * 1_024;
const MAX_RECENT_CALLS: usize = 64;
const CRASH_LOOP_THRESHOLD: u32 = 3;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginLifecycle {
    Discovered,
    Installed,
    Activating,
    Active,
    HealthChecking,
    Disabled,
    Updating,
    Migrating,
    RollingBack,
    Quarantined,
}

impl PluginLifecycle {
    const fn is_interrupted(self) -> bool {
        matches!(
            self,
            Self::Activating
                | Self::HealthChecking
                | Self::Updating
                | Self::Migrating
                | Self::RollingBack
        )
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginHealth {
    Unknown,
    Healthy,
    Failed { safe_error: String },
}

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginSafeModeReason {
    Requested,
    CorruptState,
    CorruptPackage { plugin_id: String },
    IncompatiblePackage { plugin_id: String },
    MigrationFailure { plugin_id: String },
    InterruptedLifecycle { plugin_id: String },
    CrashLoop { plugin_id: String },
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginSafeMode {
    pub active: bool,
    pub reasons: BTreeSet<PluginSafeModeReason>,
}

impl PluginSafeMode {
    fn engage(&mut self, reason: PluginSafeModeReason) {
        self.active = true;
        self.reasons.insert(reason);
    }
}

#[derive(Clone, Debug)]
pub struct PluginPackage {
    pub manifest: PluginManifest,
    pub module: Arc<[u8]>,
    pub source: PathBuf,
}

impl PluginPackage {
    /// # Errors
    ///
    /// Returns an error for unsafe paths, symlinks, malformed manifests, oversized modules, or
    /// digest mismatches.
    pub fn load(directory: impl AsRef<Path>) -> Result<Self, PluginAuthorityError> {
        let directory = directory.as_ref();
        let metadata = fs::symlink_metadata(directory)?;
        if !metadata.is_dir() || metadata.file_type().is_symlink() {
            return Err(PluginAuthorityError::InvalidPackage);
        }
        let manifest_path = directory.join(MANIFEST_FILE);
        let module_path = directory.join(MODULE_FILE);
        let manifest_bytes = read_regular_bounded(&manifest_path, MAX_MANIFEST_BYTES)?;
        let manifest_text = std::str::from_utf8(&manifest_bytes)
            .map_err(|_| PluginAuthorityError::InvalidPackage)?;
        let raw_manifest: toml::Value =
            toml::from_str(manifest_text).map_err(|_| PluginAuthorityError::InvalidPackage)?;
        for identity_field in ["id", "version"] {
            if raw_manifest
                .get(identity_field)
                .and_then(toml::Value::as_str)
                .is_some_and(|value| !safe_segment(value))
            {
                return Err(PluginAuthorityError::Traversal);
            }
        }
        let manifest = PluginManifest::parse_bounded(
            manifest_text,
            usize::try_from(MAX_MANIFEST_BYTES)
                .map_err(|_| PluginAuthorityError::InvalidPackage)?,
        )?;
        if !safe_segment(&manifest.id) || !safe_segment(&manifest.version) {
            return Err(PluginAuthorityError::Traversal);
        }
        let module = read_regular_bounded(&module_path, MAX_MODULE_BYTES)?;
        let digest = module_digest(&module);
        let declared = manifest
            .digest
            .as_ref()
            .ok_or(PluginAuthorityError::DigestMismatch)?;
        if declared.algorithm != "sha256"
            || !constant_time_eq(declared.value.as_bytes(), digest.as_bytes())
        {
            return Err(PluginAuthorityError::DigestMismatch);
        }
        Ok(Self {
            manifest,
            module: module.into(),
            source: fs::canonicalize(directory)?,
        })
    }

    /// # Errors
    ///
    /// Returns an error if the canonical unsigned manifest cannot be encoded.
    pub fn signature_payload(manifest: &PluginManifest) -> Result<Vec<u8>, PluginAuthorityError> {
        let mut unsigned = manifest.clone();
        unsigned.signature = None;
        serde_json::to_vec(&unsigned).map_err(PluginAuthorityError::from)
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TrustedPublisher {
    pub publisher_id: String,
    pub key_id: String,
    verifying_key: VerifyingKey,
}

impl TrustedPublisher {
    /// # Errors
    ///
    /// Returns an error when the publisher identity or Ed25519 public key is invalid.
    pub fn new(
        publisher_id: impl Into<String>,
        key_id: impl Into<String>,
        public_key: [u8; 32],
    ) -> Result<Self, PluginAuthorityError> {
        let publisher_id = publisher_id.into();
        let key_id = key_id.into();
        if !safe_segment(&publisher_id) || !safe_segment(&key_id) {
            return Err(PluginAuthorityError::UntrustedPublisher);
        }
        let verifying_key = VerifyingKey::from_bytes(&public_key)
            .map_err(|_| PluginAuthorityError::UntrustedPublisher)?;
        Ok(Self {
            publisher_id,
            key_id,
            verifying_key,
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct GrantApproval {
    pub plugin_id: String,
    pub from_version: Option<String>,
    pub to_version: String,
    pub grants: BTreeSet<PluginGrant>,
    pub human_confirmed: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginCallOutcome {
    Completed,
    Cancelled,
    Denied,
    Failed,
    HostFailure,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginCall {
    pub sequence: u64,
    pub version: String,
    pub operation: PluginOperation,
    pub target: Option<String>,
    pub outcome: PluginCallOutcome,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginDataAccess {
    pub network_hosts: BTreeSet<String>,
    pub credential_names: BTreeSet<String>,
    pub readable_storage_namespaces: BTreeSet<String>,
    pub writable_storage_namespaces: BTreeSet<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginUpdateDiff {
    pub from_version: Option<String>,
    pub to_version: String,
    pub publisher_changed: bool,
    pub added_grants: BTreeSet<PluginGrant>,
    pub removed_grants: BTreeSet<PluginGrant>,
    pub added_tools: BTreeSet<String>,
    pub removed_tools: BTreeSet<String>,
    pub migration_required: bool,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginInspection {
    pub id: String,
    pub name: String,
    pub version: String,
    pub publisher: PluginPublisher,
    pub lifecycle: PluginLifecycle,
    pub health: PluginHealth,
    pub tools: Vec<PluginToolDescriptor>,
    pub commands: Vec<PluginToolDescriptor>,
    pub grants: BTreeSet<PluginGrant>,
    pub data_access: PluginDataAccess,
    pub recent_calls: Vec<PluginCall>,
    pub update_diff: Option<PluginUpdateDiff>,
    pub installed_versions: BTreeSet<String>,
    pub quarantined_versions: BTreeMap<String, String>,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct AuthorityRecord {
    id: String,
    name: String,
    active_version: String,
    prior_version: Option<String>,
    publisher: PluginPublisher,
    lifecycle: PluginLifecycle,
    health: PluginHealth,
    tools: Vec<PluginToolDescriptor>,
    commands: Vec<PluginToolDescriptor>,
    grants: BTreeSet<PluginGrant>,
    data_access: PluginDataAccess,
    recent_calls: Vec<PluginCall>,
    update_diff: Option<PluginUpdateDiff>,
    installed_versions: BTreeSet<String>,
    quarantined_versions: BTreeMap<String, String>,
    consecutive_failures: u32,
}

impl AuthorityRecord {
    fn inspection(&self) -> PluginInspection {
        PluginInspection {
            id: self.id.clone(),
            name: self.name.clone(),
            version: self.active_version.clone(),
            publisher: self.publisher.clone(),
            lifecycle: self.lifecycle,
            health: self.health.clone(),
            tools: self.tools.clone(),
            commands: self.commands.clone(),
            grants: self.grants.clone(),
            data_access: self.data_access.clone(),
            recent_calls: self.recent_calls.clone(),
            update_diff: self.update_diff.clone(),
            installed_versions: self.installed_versions.clone(),
            quarantined_versions: self.quarantined_versions.clone(),
        }
    }
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct AuthorityLedger {
    records: BTreeMap<String, AuthorityRecord>,
    safe_mode: PluginSafeMode,
    next_call_sequence: u64,
}

#[derive(Debug, Error)]
pub enum PluginAuthorityError {
    #[error("plugin authority I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("plugin authority state failed validation: {0}")]
    Json(#[from] serde_json::Error),
    #[error("plugin manifest failed validation: {0}")]
    Manifest(#[from] keith_plugin_sdk::ManifestError),
    #[error("plugin component failed validation or invocation: {0}")]
    Abi(#[from] PluginAbiError),
    #[error("plugin package is incomplete, oversized, or unsafe")]
    InvalidPackage,
    #[error("plugin package path traversal or symlink was refused")]
    Traversal,
    #[error("plugin package digest does not match its signed manifest")]
    DigestMismatch,
    #[error("plugin publisher or signing key is not trusted")]
    UntrustedPublisher,
    #[error("plugin Ed25519 signature is invalid")]
    InvalidSignature,
    #[error("plugin update changes publisher identity or signing key")]
    PublisherContinuity,
    #[error("new or widened plugin grants require exact human approval")]
    ApprovalRequired,
    #[error("plugin activation or execution is prohibited in safe mode")]
    SafeMode,
    #[error("plugin or requested version was not found")]
    NotFound,
    #[error("plugin version is already installed")]
    AlreadyInstalled,
    #[error("plugin lifecycle state does not permit the operation")]
    InvalidLifecycle,
    #[error("plugin returned a non-success terminal state")]
    InvocationFailed,
}

pub struct PluginAuthorityHost {
    root: PathBuf,
    ledger: AuthorityLedger,
    trusted_publishers: BTreeMap<String, TrustedPublisher>,
    context: Arc<dyn PluginHostContext>,
}

impl PluginAuthorityHost {
    /// # Errors
    ///
    /// Returns an error only when the authority root cannot be created or durable state cannot be
    /// written. Corrupt state and packages enter safe mode instead of preventing startup.
    pub fn open(
        root: impl AsRef<Path>,
        safe_mode: bool,
        trusted_publishers: impl IntoIterator<Item = TrustedPublisher>,
        context: Arc<dyn PluginHostContext>,
    ) -> Result<Self, PluginAuthorityError> {
        fs::create_dir_all(root.as_ref())?;
        let root = fs::canonicalize(root.as_ref())?;
        ensure_real_directory(&root.join(PACKAGES_DIRECTORY))?;
        let mut trusted_by_key = BTreeMap::new();
        for publisher in trusted_publishers {
            if trusted_by_key
                .insert(publisher.key_id.clone(), publisher)
                .is_some()
            {
                return Err(PluginAuthorityError::UntrustedPublisher);
            }
        }
        let ledger_path = root.join(AUTHORITY_LEDGER_FILE);
        let mut corrupt_state = false;
        let ledger = if ledger_path.exists() {
            if let Ok(ledger) = serde_json::from_slice(&fs::read(&ledger_path)?) {
                ledger
            } else {
                corrupt_state = true;
                AuthorityLedger::default()
            }
        } else {
            AuthorityLedger::default()
        };
        let mut host = Self {
            root,
            ledger,
            trusted_publishers: trusted_by_key,
            context,
        };
        if safe_mode {
            host.ledger
                .safe_mode
                .engage(PluginSafeModeReason::Requested);
        }
        if corrupt_state {
            host.ledger
                .safe_mode
                .engage(PluginSafeModeReason::CorruptState);
        }
        host.reconcile()?;
        Ok(host)
    }

    pub const fn safe_mode(&self) -> &PluginSafeMode {
        &self.ledger.safe_mode
    }

    /// Leaves safe mode only after every selected package still matches its signed durable record
    /// and typed component contract. Plugins remain disabled until individually activated.
    ///
    /// # Errors
    ///
    /// Returns an error while any selected package is corrupt, incompatible, or inconsistent with
    /// its durable provenance record.
    pub fn exit_safe_mode(&mut self) -> Result<PluginSafeMode, PluginAuthorityError> {
        let records = self.ledger.records.values().cloned().collect::<Vec<_>>();
        for record in records {
            let package = self.load_stored_package(&record.id, &record.active_version)?;
            if !record_matches_package(&record, &package.manifest) {
                return Err(PluginAuthorityError::InvalidPackage);
            }
            ExecutablePlugin::compile(package.manifest, package.module, Arc::clone(&self.context))?
                .validate_contract()?;
        }
        self.ledger.safe_mode = PluginSafeMode::default();
        self.persist()?;
        Ok(self.ledger.safe_mode.clone())
    }

    /// # Errors
    ///
    /// Returns an error when the package or its publisher proof is invalid.
    pub fn discover(
        &self,
        source: impl AsRef<Path>,
    ) -> Result<PluginPackage, PluginAuthorityError> {
        let package = PluginPackage::load(source)?;
        self.verify_package(&package)?;
        let executable = ExecutablePlugin::compile(
            package.manifest.clone(),
            Arc::clone(&package.module),
            Arc::clone(&self.context),
        )?;
        executable.validate_contract()?;
        Ok(package)
    }

    pub fn inspections(&self) -> impl Iterator<Item = PluginInspection> + '_ {
        self.ledger
            .records
            .values()
            .map(AuthorityRecord::inspection)
    }

    /// # Errors
    ///
    /// Returns an error if the plugin is absent.
    pub fn inspect(&self, id: &str) -> Result<PluginInspection, PluginAuthorityError> {
        self.ledger
            .records
            .get(id)
            .map(AuthorityRecord::inspection)
            .ok_or(PluginAuthorityError::NotFound)
    }

    /// # Errors
    ///
    /// Returns an error if provenance, grant approval, package storage, or activation fails.
    pub fn install(
        &mut self,
        source: impl AsRef<Path>,
        approval: Option<&GrantApproval>,
    ) -> Result<PluginInspection, PluginAuthorityError> {
        let package = self.discover(source)?;
        if self.ledger.records.contains_key(&package.manifest.id) {
            if self.ledger.safe_mode.active {
                return Err(PluginAuthorityError::SafeMode);
            }
            return self.update_package(&package, approval);
        }
        let grants = declared_grants(&package.manifest);
        require_approval(
            &package.manifest.id,
            None,
            &package.manifest.version,
            &grants,
            approval,
        )?;
        self.store_package(&package)?;
        let mut record = record_from_package(&package, PluginLifecycle::Installed)?;
        if self.ledger.safe_mode.active {
            record.lifecycle = PluginLifecycle::Disabled;
        }
        self.ledger.records.insert(record.id.clone(), record);
        self.persist()?;
        if self.ledger.safe_mode.active {
            return self.inspect(&package.manifest.id);
        }
        self.set_lifecycle(&package.manifest.id, PluginLifecycle::Activating)?;
        if package.manifest.hooks.contains(&PluginHook::Activate)
            && let Err(error) = self.run_lifecycle_operation(
                &package.manifest.id,
                &package.manifest.version,
                PluginOperation::Activate,
                b"null".to_vec(),
            )
        {
            self.quarantine_with_error(&package.manifest.id, &error.to_string())?;
            return Err(error);
        }
        let record = self
            .ledger
            .records
            .get_mut(&package.manifest.id)
            .ok_or(PluginAuthorityError::NotFound)?;
        record.lifecycle = PluginLifecycle::Active;
        record.health = PluginHealth::Unknown;
        record.consecutive_failures = 0;
        self.persist()?;
        self.inspect(&package.manifest.id)
    }

    /// # Errors
    ///
    /// Returns an error if safe mode is active, the plugin is absent, or typed activation fails.
    pub fn activate(&mut self, id: &str) -> Result<PluginInspection, PluginAuthorityError> {
        if self.ledger.safe_mode.active {
            return Err(PluginAuthorityError::SafeMode);
        }
        let version = self.active_version(id)?;
        let package = self.load_stored_package(id, &version)?;
        self.set_lifecycle(id, PluginLifecycle::Activating)?;
        if package.manifest.hooks.contains(&PluginHook::Activate)
            && let Err(error) = self.run_lifecycle_operation(
                id,
                &version,
                PluginOperation::Activate,
                b"null".to_vec(),
            )
        {
            self.quarantine_with_error(id, &error.to_string())?;
            return Err(error);
        }
        let record = self
            .ledger
            .records
            .get_mut(id)
            .ok_or(PluginAuthorityError::NotFound)?;
        record.lifecycle = PluginLifecycle::Active;
        record.health = PluginHealth::Unknown;
        record.quarantined_versions.remove(&version);
        record.consecutive_failures = 0;
        self.persist()?;
        self.inspect(id)
    }

    /// # Errors
    ///
    /// Returns an error if the plugin is absent or its health invocation fails.
    pub fn health(&mut self, id: &str) -> Result<PluginHealth, PluginAuthorityError> {
        if self.ledger.safe_mode.active {
            return Err(PluginAuthorityError::SafeMode);
        }
        let version = self.active_version(id)?;
        let package = self.load_stored_package(id, &version)?;
        if !package.manifest.hooks.contains(&PluginHook::Health) {
            return Ok(self
                .ledger
                .records
                .get(id)
                .ok_or(PluginAuthorityError::NotFound)?
                .health
                .clone());
        }
        self.set_lifecycle(id, PluginLifecycle::HealthChecking)?;
        match self.run_lifecycle_operation(id, &version, PluginOperation::Health, b"null".to_vec())
        {
            Ok(()) => {
                let record = self
                    .ledger
                    .records
                    .get_mut(id)
                    .ok_or(PluginAuthorityError::NotFound)?;
                record.lifecycle = PluginLifecycle::Active;
                record.health = PluginHealth::Healthy;
                record.consecutive_failures = 0;
                self.persist()?;
                Ok(PluginHealth::Healthy)
            }
            Err(error) => {
                let safe_error = error.to_string();
                self.quarantine_with_error(id, &safe_error)?;
                Err(error)
            }
        }
    }

    /// # Errors
    ///
    /// Returns an error if the plugin is absent or durable state cannot be written. Deactivation
    /// failure cannot prevent the authority boundary from disabling the plugin.
    pub fn disable(&mut self, id: &str) -> Result<PluginInspection, PluginAuthorityError> {
        let version = self.active_version(id)?;
        if !self.ledger.safe_mode.active
            && let Ok(package) = self.load_stored_package(id, &version)
            && package.manifest.hooks.contains(&PluginHook::Deactivate)
        {
            let _ = self.run_lifecycle_operation(
                id,
                &version,
                PluginOperation::Deactivate,
                b"null".to_vec(),
            );
        }
        let record = self
            .ledger
            .records
            .get_mut(id)
            .ok_or(PluginAuthorityError::NotFound)?;
        record.lifecycle = PluginLifecycle::Disabled;
        self.persist()?;
        self.inspect(id)
    }

    /// # Errors
    ///
    /// Returns an error when the update is incompatible, changes publisher, lacks approval, fails
    /// migration, or fails activation. The previous active version remains selected on failure.
    pub fn update(
        &mut self,
        source: impl AsRef<Path>,
        approval: Option<&GrantApproval>,
    ) -> Result<PluginInspection, PluginAuthorityError> {
        if self.ledger.safe_mode.active {
            return Err(PluginAuthorityError::SafeMode);
        }
        let package = self.discover(source)?;
        self.update_package(&package, approval)
    }

    /// # Errors
    ///
    /// Returns an error if the rollback bytes or provenance are invalid, widened grants lack exact
    /// approval, or activation fails.
    pub fn rollback(
        &mut self,
        id: &str,
        version: &str,
        approval: Option<&GrantApproval>,
    ) -> Result<PluginInspection, PluginAuthorityError> {
        let current_version = self.active_version(id)?;
        let rollback = self.load_stored_package(id, version)?;
        let current_record = self
            .ledger
            .records
            .get(id)
            .cloned()
            .ok_or(PluginAuthorityError::NotFound)?;
        let diff = if let Ok(current_package) = self.load_stored_package(id, &current_version) {
            publisher_continuity(&current_package.manifest, &rollback.manifest)?;
            update_diff(Some(&current_package.manifest), &rollback.manifest)
        } else {
            let rollback_publisher = rollback
                .manifest
                .publisher
                .as_ref()
                .ok_or(PluginAuthorityError::PublisherContinuity)?;
            if current_record.publisher.id != rollback_publisher.id
                || current_record.publisher.key_id != rollback_publisher.key_id
            {
                return Err(PluginAuthorityError::PublisherContinuity);
            }
            rollback_diff(&current_record, &rollback.manifest)
        };
        require_approval(
            id,
            Some(&current_version),
            version,
            &diff.added_grants,
            approval,
        )?;
        self.set_lifecycle(id, PluginLifecycle::RollingBack)?;
        if !self.ledger.safe_mode.active
            && rollback.manifest.hooks.contains(&PluginHook::Activate)
            && let Err(error) = self.run_lifecycle_operation(
                id,
                version,
                PluginOperation::Activate,
                b"null".to_vec(),
            )
        {
            let safe_error = error.to_string();
            self.restore_active_lifecycle(id, &current_version, Some(&safe_error))?;
            return Err(error);
        }
        self.adopt_package(id, &rollback, Some(current_version), Some(diff))?;
        if self.ledger.safe_mode.active {
            self.ledger
                .records
                .get_mut(id)
                .ok_or(PluginAuthorityError::NotFound)?
                .lifecycle = PluginLifecycle::Disabled;
            self.persist()?;
        }
        self.inspect(id)
    }

    /// # Errors
    ///
    /// Returns an error when the plugin is absent or state cannot be persisted.
    pub fn quarantine(
        &mut self,
        id: &str,
        safe_error: impl Into<String>,
    ) -> Result<PluginInspection, PluginAuthorityError> {
        let safe_error = safe_error.into();
        self.quarantine_with_error(id, &safe_error)?;
        self.inspect(id)
    }

    /// # Errors
    ///
    /// Returns an error when the plugin is absent or complete byte and state removal fails.
    pub fn uninstall(&mut self, id: &str) -> Result<PluginInspection, PluginAuthorityError> {
        let inspection = self.inspect(id)?;
        self.disable(id)?;
        let directory = self.plugin_directory(id)?;
        if directory.exists() {
            fs::remove_dir_all(directory)?;
        }
        self.ledger.records.remove(id);
        self.persist()?;
        Ok(inspection)
    }

    /// # Errors
    ///
    /// Returns an error unless the plugin is active or when the typed component invocation fails.
    pub fn invoke(
        &mut self,
        id: &str,
        request: &HostRequest,
    ) -> Result<keith_plugin_sdk::HostResponse, PluginAuthorityError> {
        if self.ledger.safe_mode.active {
            return Err(PluginAuthorityError::SafeMode);
        }
        if !matches!(
            request.operation,
            PluginOperation::Command | PluginOperation::Tool
        ) {
            return Err(PluginAuthorityError::InvalidLifecycle);
        }
        let record = self
            .ledger
            .records
            .get(id)
            .ok_or(PluginAuthorityError::NotFound)?;
        if record.lifecycle != PluginLifecycle::Active {
            return Err(PluginAuthorityError::InvalidLifecycle);
        }
        let version = record.active_version.clone();
        let package = self.load_stored_package(id, &version)?;
        let executable = ExecutablePlugin::compile(
            package.manifest,
            Arc::clone(&package.module),
            Arc::clone(&self.context),
        )?;
        let result = executable.invoke(request);
        match result {
            Ok(response) => {
                let outcome = status_outcome(response.status);
                self.record_call(
                    id,
                    &version,
                    request.operation,
                    request.target.clone(),
                    outcome.clone(),
                    None,
                )?;
                if outcome == PluginCallOutcome::Completed {
                    self.clear_failures(id)?;
                    Ok(response)
                } else {
                    self.record_failure(id)?;
                    Err(PluginAuthorityError::InvocationFailed)
                }
            }
            Err(error) => {
                let safe_error = error.to_string();
                self.record_call(
                    id,
                    &version,
                    request.operation,
                    request.target.clone(),
                    PluginCallOutcome::HostFailure,
                    Some(&safe_error),
                )?;
                self.record_failure(id)?;
                Err(error.into())
            }
        }
    }

    fn update_package(
        &mut self,
        package: &PluginPackage,
        approval: Option<&GrantApproval>,
    ) -> Result<PluginInspection, PluginAuthorityError> {
        let id = package.manifest.id.clone();
        let previous_version = self.active_version(&id)?;
        if previous_version == package.manifest.version
            || self
                .package_directory(&id, &package.manifest.version)?
                .exists()
        {
            return Err(PluginAuthorityError::AlreadyInstalled);
        }
        let previous = self.load_stored_package(&id, &previous_version)?;
        publisher_continuity(&previous.manifest, &package.manifest)?;
        let diff = update_diff(Some(&previous.manifest), &package.manifest);
        require_approval(
            &id,
            Some(&previous_version),
            &package.manifest.version,
            &diff.added_grants,
            approval,
        )?;
        self.store_package(package)?;
        {
            let record = self
                .ledger
                .records
                .get_mut(&id)
                .ok_or(PluginAuthorityError::NotFound)?;
            record
                .installed_versions
                .insert(package.manifest.version.clone());
            record.update_diff = Some(diff.clone());
            record.lifecycle = PluginLifecycle::Updating;
        }
        self.persist()?;
        if package.manifest.hooks.contains(&PluginHook::Migrate) {
            self.set_lifecycle(&id, PluginLifecycle::Migrating)?;
            let payload = serde_json::to_vec(&serde_json::json!({
                "from_version": &previous_version,
                "to_version": &package.manifest.version,
                "state_schema_version": package
                    .manifest
                    .migration
                    .as_ref()
                    .map_or(0, |migration| migration.state_schema_version),
            }))?;
            if let Err(error) = self.run_lifecycle_operation(
                &id,
                &package.manifest.version,
                PluginOperation::Migrate,
                payload,
            ) {
                self.quarantine_candidate(
                    &id,
                    &package.manifest.version,
                    &error.to_string(),
                    &previous_version,
                )?;
                self.engage_safe_mode(PluginSafeModeReason::MigrationFailure {
                    plugin_id: id.clone(),
                });
                self.persist()?;
                return Err(error);
            }
        }
        if package.manifest.hooks.contains(&PluginHook::Activate)
            && let Err(error) = self.run_lifecycle_operation(
                &id,
                &package.manifest.version,
                PluginOperation::Activate,
                b"null".to_vec(),
            )
        {
            self.quarantine_candidate(
                &id,
                &package.manifest.version,
                &error.to_string(),
                &previous_version,
            )?;
            return Err(error);
        }
        self.adopt_package(&id, package, Some(previous_version), Some(diff))?;
        self.inspect(&id)
    }

    fn run_lifecycle_operation(
        &mut self,
        id: &str,
        version: &str,
        operation: PluginOperation,
        payload: Vec<u8>,
    ) -> Result<(), PluginAuthorityError> {
        let package = self.load_stored_package(id, version)?;
        let sequence = self.next_call_sequence();
        let request = HostRequest {
            interface_version: HOST_API_VERSION,
            invocation_id: format!("authority-{sequence}"),
            operation,
            target: None,
            payload_format: PayloadFormat::Json,
            payload,
            cancellation_id: format!("authority-cancel-{sequence}"),
        };
        let executable = ExecutablePlugin::compile(
            package.manifest,
            Arc::clone(&package.module),
            Arc::clone(&self.context),
        )?;
        match executable.invoke(&request) {
            Ok(response) => {
                let outcome = status_outcome(response.status);
                self.record_call(id, version, operation, None, outcome.clone(), None)?;
                if outcome == PluginCallOutcome::Completed {
                    self.clear_failures(id)?;
                    Ok(())
                } else {
                    self.record_failure(id)?;
                    Err(PluginAuthorityError::InvocationFailed)
                }
            }
            Err(error) => {
                let safe_error = error.to_string();
                self.record_call(
                    id,
                    version,
                    operation,
                    None,
                    PluginCallOutcome::HostFailure,
                    Some(&safe_error),
                )?;
                self.record_failure(id)?;
                Err(error.into())
            }
        }
    }

    fn verify_package(&self, package: &PluginPackage) -> Result<(), PluginAuthorityError> {
        let publisher = package
            .manifest
            .publisher
            .as_ref()
            .ok_or(PluginAuthorityError::UntrustedPublisher)?;
        let signature = package
            .manifest
            .signature
            .as_ref()
            .ok_or(PluginAuthorityError::InvalidSignature)?;
        if signature.algorithm != "ed25519" || publisher.key_id != signature.key_id {
            return Err(PluginAuthorityError::InvalidSignature);
        }
        let trusted = self
            .trusted_publishers
            .get(&signature.key_id)
            .filter(|trusted| {
                trusted.publisher_id == publisher.id && trusted.key_id == publisher.key_id
            })
            .ok_or(PluginAuthorityError::UntrustedPublisher)?;
        let signature_bytes = decode_hex::<64>(&signature.value)?;
        let signature = Signature::from_slice(&signature_bytes)
            .map_err(|_| PluginAuthorityError::InvalidSignature)?;
        let payload = PluginPackage::signature_payload(&package.manifest)?;
        trusted
            .verifying_key
            .verify_strict(&payload, &signature)
            .map_err(|_| PluginAuthorityError::InvalidSignature)
    }

    fn reconcile(&mut self) -> Result<(), PluginAuthorityError> {
        let ids = self.ledger.records.keys().cloned().collect::<Vec<_>>();
        for id in ids {
            let record = self
                .ledger
                .records
                .get(&id)
                .cloned()
                .ok_or(PluginAuthorityError::NotFound)?;
            if record.lifecycle.is_interrupted() {
                self.ledger
                    .safe_mode
                    .engage(PluginSafeModeReason::InterruptedLifecycle {
                        plugin_id: id.clone(),
                    });
                self.quarantine_in_memory(&id, "interrupted plugin lifecycle");
                continue;
            }
            if record.consecutive_failures >= CRASH_LOOP_THRESHOLD {
                self.ledger
                    .safe_mode
                    .engage(PluginSafeModeReason::CrashLoop {
                        plugin_id: id.clone(),
                    });
                self.quarantine_in_memory(&id, "plugin crash loop");
                continue;
            }
            match self.load_stored_package(&id, &record.active_version) {
                Ok(package) => {
                    if !record_matches_package(&record, &package.manifest) {
                        self.ledger
                            .safe_mode
                            .engage(PluginSafeModeReason::CorruptState);
                        self.quarantine_in_memory(
                            &id,
                            "plugin authority metadata does not match signed package",
                        );
                        continue;
                    }
                    let contract = ExecutablePlugin::compile(
                        package.manifest,
                        package.module,
                        Arc::clone(&self.context),
                    )
                    .and_then(|executable| executable.validate_contract());
                    if contract.is_err() {
                        self.ledger
                            .safe_mode
                            .engage(PluginSafeModeReason::IncompatiblePackage {
                                plugin_id: id.clone(),
                            });
                        self.quarantine_in_memory(&id, "incompatible plugin component");
                    }
                }
                Err(PluginAuthorityError::Manifest(
                    keith_plugin_sdk::ManifestError::Incompatible,
                )) => {
                    self.ledger
                        .safe_mode
                        .engage(PluginSafeModeReason::IncompatiblePackage {
                            plugin_id: id.clone(),
                        });
                    self.quarantine_in_memory(&id, "incompatible plugin API");
                }
                Err(_) => {
                    self.ledger
                        .safe_mode
                        .engage(PluginSafeModeReason::CorruptPackage {
                            plugin_id: id.clone(),
                        });
                    self.quarantine_in_memory(&id, "corrupt plugin package");
                }
            }
        }
        self.discover_stored_packages()?;
        if self.ledger.safe_mode.active {
            for record in self.ledger.records.values_mut() {
                if record.lifecycle != PluginLifecycle::Quarantined {
                    record.lifecycle = PluginLifecycle::Disabled;
                }
            }
        }
        self.persist()
    }

    fn discover_stored_packages(&mut self) -> Result<(), PluginAuthorityError> {
        let packages = self.root.join(PACKAGES_DIRECTORY);
        let mut plugin_entries = fs::read_dir(packages)?.collect::<Result<Vec<_>, _>>()?;
        plugin_entries.sort_by_key(fs::DirEntry::file_name);
        for plugin_entry in plugin_entries {
            let id = plugin_entry.file_name().to_string_lossy().into_owned();
            if !safe_segment(&id)
                || !plugin_entry.file_type()?.is_dir()
                || plugin_entry.file_type()?.is_symlink()
            {
                self.ledger
                    .safe_mode
                    .engage(PluginSafeModeReason::CorruptPackage {
                        plugin_id: bounded_identifier(&id),
                    });
                continue;
            }
            let mut version_entries =
                fs::read_dir(plugin_entry.path())?.collect::<Result<Vec<_>, _>>()?;
            version_entries.sort_by_key(fs::DirEntry::file_name);
            for version_entry in version_entries {
                let version = version_entry.file_name().to_string_lossy().into_owned();
                if !safe_segment(&version)
                    || !version_entry.file_type()?.is_dir()
                    || version_entry.file_type()?.is_symlink()
                {
                    self.ledger
                        .safe_mode
                        .engage(PluginSafeModeReason::CorruptPackage {
                            plugin_id: id.clone(),
                        });
                    continue;
                }
                match self.discover(version_entry.path()) {
                    Ok(package)
                        if package.manifest.id == id && package.manifest.version == version =>
                    {
                        if let Some(record) = self.ledger.records.get_mut(&id) {
                            record
                                .installed_versions
                                .insert(package.manifest.version.clone());
                        } else {
                            let record = record_from_package(&package, PluginLifecycle::Disabled)?;
                            self.ledger.records.insert(id.clone(), record);
                        }
                    }
                    _ => {
                        self.ledger
                            .safe_mode
                            .engage(PluginSafeModeReason::CorruptPackage {
                                plugin_id: id.clone(),
                            });
                    }
                }
            }
        }
        Ok(())
    }

    fn store_package(&self, package: &PluginPackage) -> Result<(), PluginAuthorityError> {
        let destination =
            self.package_directory(&package.manifest.id, &package.manifest.version)?;
        let parent = destination
            .parent()
            .ok_or(PluginAuthorityError::Traversal)?;
        ensure_real_directory(parent)?;
        match fs::symlink_metadata(&destination) {
            Ok(metadata) if metadata.file_type().is_symlink() => {
                return Err(PluginAuthorityError::Traversal);
            }
            Ok(_) => return Err(PluginAuthorityError::AlreadyInstalled),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
        let staging = self.root.join(format!(
            ".authority-install-{}-{}",
            package.manifest.id, package.manifest.version
        ));
        if staging.exists() {
            fs::remove_dir_all(&staging)?;
        }
        fs::create_dir(&staging)?;
        fs::write(
            staging.join(MANIFEST_FILE),
            toml::to_string(&package.manifest).map_err(|_| PluginAuthorityError::InvalidPackage)?,
        )?;
        fs::write(staging.join(MODULE_FILE), &package.module)?;
        fs::rename(staging, destination)?;
        Ok(())
    }

    fn load_stored_package(
        &self,
        id: &str,
        version: &str,
    ) -> Result<PluginPackage, PluginAuthorityError> {
        let package = PluginPackage::load(self.package_directory(id, version)?)?;
        self.verify_package(&package)?;
        if package.manifest.id != id || package.manifest.version != version {
            return Err(PluginAuthorityError::InvalidPackage);
        }
        Ok(package)
    }

    fn adopt_package(
        &mut self,
        id: &str,
        package: &PluginPackage,
        prior_version: Option<String>,
        diff: Option<PluginUpdateDiff>,
    ) -> Result<(), PluginAuthorityError> {
        let record = self
            .ledger
            .records
            .get_mut(id)
            .ok_or(PluginAuthorityError::NotFound)?;
        record.name.clone_from(&package.manifest.name);
        record.active_version.clone_from(&package.manifest.version);
        record.prior_version = prior_version;
        let publisher = package
            .manifest
            .publisher
            .as_ref()
            .ok_or(PluginAuthorityError::UntrustedPublisher)?;
        record.publisher.clone_from(publisher);
        record.lifecycle = PluginLifecycle::Active;
        record.health = PluginHealth::Unknown;
        record.tools.clone_from(&package.manifest.tools);
        record.commands.clone_from(&package.manifest.commands);
        record.grants = declared_grants(&package.manifest);
        record.data_access = data_access(&package.manifest);
        record.update_diff = diff;
        record
            .installed_versions
            .insert(package.manifest.version.clone());
        record
            .quarantined_versions
            .remove(&package.manifest.version);
        record.consecutive_failures = 0;
        self.persist()
    }

    fn restore_active_lifecycle(
        &mut self,
        id: &str,
        active_version: &str,
        safe_error: Option<&str>,
    ) -> Result<(), PluginAuthorityError> {
        let record = self
            .ledger
            .records
            .get_mut(id)
            .ok_or(PluginAuthorityError::NotFound)?;
        active_version.clone_into(&mut record.active_version);
        record.lifecycle = PluginLifecycle::Active;
        if let Some(diff) = record.update_diff.as_mut() {
            diff.safe_error = safe_error.map(bounded_safe_error);
        }
        self.persist()
    }

    fn quarantine_candidate(
        &mut self,
        id: &str,
        candidate: &str,
        safe_error: &str,
        active_version: &str,
    ) -> Result<(), PluginAuthorityError> {
        let record = self
            .ledger
            .records
            .get_mut(id)
            .ok_or(PluginAuthorityError::NotFound)?;
        record
            .quarantined_versions
            .insert(candidate.to_owned(), bounded_safe_error(safe_error));
        active_version.clone_into(&mut record.active_version);
        record.lifecycle = PluginLifecycle::Active;
        if let Some(diff) = record.update_diff.as_mut() {
            diff.safe_error = Some(bounded_safe_error(safe_error));
        }
        self.persist()
    }

    fn quarantine_with_error(
        &mut self,
        id: &str,
        safe_error: &str,
    ) -> Result<(), PluginAuthorityError> {
        self.quarantine_in_memory(id, safe_error);
        self.persist()
    }

    fn quarantine_in_memory(&mut self, id: &str, safe_error: &str) {
        if let Some(record) = self.ledger.records.get_mut(id) {
            let safe_error = bounded_safe_error(safe_error);
            record
                .quarantined_versions
                .insert(record.active_version.clone(), safe_error.clone());
            record.lifecycle = PluginLifecycle::Quarantined;
            record.health = PluginHealth::Failed { safe_error };
        }
    }

    fn set_lifecycle(
        &mut self,
        id: &str,
        lifecycle: PluginLifecycle,
    ) -> Result<(), PluginAuthorityError> {
        self.ledger
            .records
            .get_mut(id)
            .ok_or(PluginAuthorityError::NotFound)?
            .lifecycle = lifecycle;
        self.persist()
    }

    fn active_version(&self, id: &str) -> Result<String, PluginAuthorityError> {
        self.ledger
            .records
            .get(id)
            .map(|record| record.active_version.clone())
            .ok_or(PluginAuthorityError::NotFound)
    }

    fn plugin_directory(&self, id: &str) -> Result<PathBuf, PluginAuthorityError> {
        if !safe_segment(id) {
            return Err(PluginAuthorityError::Traversal);
        }
        Ok(self.root.join(PACKAGES_DIRECTORY).join(id))
    }

    fn package_directory(&self, id: &str, version: &str) -> Result<PathBuf, PluginAuthorityError> {
        if !safe_segment(version) {
            return Err(PluginAuthorityError::Traversal);
        }
        Ok(self.plugin_directory(id)?.join(version))
    }

    fn next_call_sequence(&mut self) -> u64 {
        let sequence = self.ledger.next_call_sequence;
        self.ledger.next_call_sequence = self.ledger.next_call_sequence.saturating_add(1);
        sequence
    }

    fn record_call(
        &mut self,
        id: &str,
        version: &str,
        operation: PluginOperation,
        target: Option<String>,
        outcome: PluginCallOutcome,
        safe_error: Option<&str>,
    ) -> Result<(), PluginAuthorityError> {
        let sequence = self.next_call_sequence();
        let record = self
            .ledger
            .records
            .get_mut(id)
            .ok_or(PluginAuthorityError::NotFound)?;
        record.recent_calls.push(PluginCall {
            sequence,
            version: version.to_owned(),
            operation,
            target,
            outcome,
            safe_error: safe_error.map(bounded_safe_error),
        });
        if record.recent_calls.len() > MAX_RECENT_CALLS {
            let excess = record.recent_calls.len() - MAX_RECENT_CALLS;
            record.recent_calls.drain(..excess);
        }
        self.persist()
    }

    fn clear_failures(&mut self, id: &str) -> Result<(), PluginAuthorityError> {
        self.ledger
            .records
            .get_mut(id)
            .ok_or(PluginAuthorityError::NotFound)?
            .consecutive_failures = 0;
        self.persist()
    }

    fn record_failure(&mut self, id: &str) -> Result<(), PluginAuthorityError> {
        let failures = {
            let record = self
                .ledger
                .records
                .get_mut(id)
                .ok_or(PluginAuthorityError::NotFound)?;
            record.consecutive_failures = record.consecutive_failures.saturating_add(1);
            record.consecutive_failures
        };
        if failures >= CRASH_LOOP_THRESHOLD {
            self.quarantine_in_memory(id, "plugin crash loop");
            self.engage_safe_mode(PluginSafeModeReason::CrashLoop {
                plugin_id: id.to_owned(),
            });
        }
        self.persist()
    }

    fn engage_safe_mode(&mut self, reason: PluginSafeModeReason) {
        self.ledger.safe_mode.engage(reason);
        for record in self.ledger.records.values_mut() {
            if record.lifecycle != PluginLifecycle::Quarantined {
                record.lifecycle = PluginLifecycle::Disabled;
            }
        }
    }

    fn persist(&self) -> Result<(), PluginAuthorityError> {
        let temporary = self.root.join(format!(".{AUTHORITY_LEDGER_FILE}.tmp"));
        fs::write(&temporary, serde_json::to_vec_pretty(&self.ledger)?)?;
        keith_platform::replace_file(&temporary, &self.root.join(AUTHORITY_LEDGER_FILE))?;
        Ok(())
    }
}

fn record_from_package(
    package: &PluginPackage,
    lifecycle: PluginLifecycle,
) -> Result<AuthorityRecord, PluginAuthorityError> {
    let manifest = &package.manifest;
    Ok(AuthorityRecord {
        id: manifest.id.clone(),
        name: manifest.name.clone(),
        active_version: manifest.version.clone(),
        prior_version: None,
        publisher: manifest
            .publisher
            .clone()
            .ok_or(PluginAuthorityError::UntrustedPublisher)?,
        lifecycle,
        health: PluginHealth::Unknown,
        tools: manifest.tools.clone(),
        commands: manifest.commands.clone(),
        grants: declared_grants(manifest),
        data_access: data_access(manifest),
        recent_calls: Vec::new(),
        update_diff: None,
        installed_versions: BTreeSet::from([manifest.version.clone()]),
        quarantined_versions: BTreeMap::new(),
        consecutive_failures: 0,
    })
}

fn record_matches_package(record: &AuthorityRecord, manifest: &PluginManifest) -> bool {
    manifest.publisher.as_ref().is_some_and(|publisher| {
        record.id == manifest.id
            && record.name == manifest.name
            && record.active_version == manifest.version
            && &record.publisher == publisher
            && record.tools == manifest.tools
            && record.commands == manifest.commands
            && record.grants == declared_grants(manifest)
            && record.data_access == data_access(manifest)
    })
}

fn publisher_continuity(
    previous: &PluginManifest,
    candidate: &PluginManifest,
) -> Result<(), PluginAuthorityError> {
    let previous = previous
        .publisher
        .as_ref()
        .ok_or(PluginAuthorityError::PublisherContinuity)?;
    let candidate = candidate
        .publisher
        .as_ref()
        .ok_or(PluginAuthorityError::PublisherContinuity)?;
    if previous.id == candidate.id && previous.key_id == candidate.key_id {
        Ok(())
    } else {
        Err(PluginAuthorityError::PublisherContinuity)
    }
}

fn require_approval(
    id: &str,
    from_version: Option<&str>,
    to_version: &str,
    widened: &BTreeSet<PluginGrant>,
    approval: Option<&GrantApproval>,
) -> Result<(), PluginAuthorityError> {
    if widened.is_empty() {
        return Ok(());
    }
    let valid = approval.is_some_and(|approval| {
        approval.human_confirmed
            && approval.plugin_id == id
            && approval.from_version.as_deref() == from_version
            && approval.to_version == to_version
            && &approval.grants == widened
    });
    if valid {
        Ok(())
    } else {
        Err(PluginAuthorityError::ApprovalRequired)
    }
}

fn update_diff(previous: Option<&PluginManifest>, candidate: &PluginManifest) -> PluginUpdateDiff {
    let previous_grants = previous.map_or_else(BTreeSet::new, declared_grants);
    let candidate_grants = declared_grants(candidate);
    let previous_tools = previous.map_or_else(BTreeSet::new, callable_names);
    let candidate_tools = callable_names(candidate);
    PluginUpdateDiff {
        from_version: previous.map(|manifest| manifest.version.clone()),
        to_version: candidate.version.clone(),
        publisher_changed: previous.is_some_and(|manifest| {
            manifest
                .publisher
                .as_ref()
                .map(|publisher| (&publisher.id, &publisher.key_id))
                != candidate
                    .publisher
                    .as_ref()
                    .map(|publisher| (&publisher.id, &publisher.key_id))
        }),
        added_grants: candidate_grants
            .difference(&previous_grants)
            .cloned()
            .collect(),
        removed_grants: previous_grants
            .difference(&candidate_grants)
            .cloned()
            .collect(),
        added_tools: candidate_tools
            .difference(&previous_tools)
            .cloned()
            .collect(),
        removed_tools: previous_tools
            .difference(&candidate_tools)
            .cloned()
            .collect(),
        migration_required: candidate.hooks.contains(&PluginHook::Migrate),
        safe_error: None,
    }
}

fn rollback_diff(previous: &AuthorityRecord, candidate: &PluginManifest) -> PluginUpdateDiff {
    let candidate_grants = declared_grants(candidate);
    let previous_tools = previous
        .tools
        .iter()
        .chain(&previous.commands)
        .map(|descriptor| descriptor.name.clone())
        .collect::<BTreeSet<_>>();
    let candidate_tools = callable_names(candidate);
    PluginUpdateDiff {
        from_version: Some(previous.active_version.clone()),
        to_version: candidate.version.clone(),
        publisher_changed: candidate.publisher.as_ref().is_none_or(|publisher| {
            publisher.id != previous.publisher.id || publisher.key_id != previous.publisher.key_id
        }),
        added_grants: candidate_grants
            .difference(&previous.grants)
            .cloned()
            .collect(),
        removed_grants: previous
            .grants
            .difference(&candidate_grants)
            .cloned()
            .collect(),
        added_tools: candidate_tools
            .difference(&previous_tools)
            .cloned()
            .collect(),
        removed_tools: previous_tools
            .difference(&candidate_tools)
            .cloned()
            .collect(),
        migration_required: candidate.hooks.contains(&PluginHook::Migrate),
        safe_error: None,
    }
}

fn declared_grants(manifest: &PluginManifest) -> BTreeSet<PluginGrant> {
    let grants = &manifest.grants;
    let mut declared = BTreeSet::new();
    declared.extend(
        grants
            .network_hosts
            .iter()
            .cloned()
            .map(PluginGrant::HttpHost),
    );
    declared.extend(
        grants
            .credential_names
            .iter()
            .cloned()
            .map(PluginGrant::Credential),
    );
    declared.extend(
        grants
            .readable_storage_namespaces
            .iter()
            .cloned()
            .map(PluginGrant::StorageRead),
    );
    declared.extend(
        grants
            .writable_storage_namespaces
            .iter()
            .cloned()
            .map(PluginGrant::StorageWrite),
    );
    if grants.allow_events {
        declared.insert(PluginGrant::EmitEvent);
    }
    if grants.allow_artifacts {
        declared.insert(PluginGrant::CreateArtifact);
    }
    if grants.allow_clock {
        declared.insert(PluginGrant::Clock);
    }
    if grants.allow_safe_logging {
        declared.insert(PluginGrant::SafeLog);
    }
    declared
}

fn data_access(manifest: &PluginManifest) -> PluginDataAccess {
    PluginDataAccess {
        network_hosts: manifest.grants.network_hosts.iter().cloned().collect(),
        credential_names: manifest.grants.credential_names.clone(),
        readable_storage_namespaces: manifest.grants.readable_storage_namespaces.clone(),
        writable_storage_namespaces: manifest.grants.writable_storage_namespaces.clone(),
    }
}

fn callable_names(manifest: &PluginManifest) -> BTreeSet<String> {
    manifest
        .tools
        .iter()
        .chain(&manifest.commands)
        .map(|descriptor| descriptor.name.clone())
        .collect()
}

fn status_outcome(status: PluginStatus) -> PluginCallOutcome {
    match status {
        PluginStatus::Completed => PluginCallOutcome::Completed,
        PluginStatus::Cancelled => PluginCallOutcome::Cancelled,
        PluginStatus::Denied => PluginCallOutcome::Denied,
        PluginStatus::Failed => PluginCallOutcome::Failed,
    }
}

fn read_regular_bounded(path: &Path, max_bytes: u64) -> Result<Vec<u8>, PluginAuthorityError> {
    let metadata = fs::symlink_metadata(path)?;
    if !metadata.is_file()
        || metadata.file_type().is_symlink()
        || metadata.len() == 0
        || metadata.len() > max_bytes
    {
        return Err(PluginAuthorityError::InvalidPackage);
    }
    fs::read(path).map_err(PluginAuthorityError::from)
}

fn ensure_real_directory(path: &Path) -> Result<(), PluginAuthorityError> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.is_dir() && !metadata.file_type().is_symlink() => Ok(()),
        Ok(_) => Err(PluginAuthorityError::Traversal),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            fs::create_dir(path).map_err(PluginAuthorityError::from)
        }
        Err(error) => Err(error.into()),
    }
}

fn module_digest(module: &[u8]) -> String {
    let digest = Sha256::digest(module);
    let mut encoded = String::with_capacity(digest.len() * 2);
    for byte in digest {
        let _ = write!(encoded, "{byte:02x}");
    }
    encoded
}

fn decode_hex<const N: usize>(value: &str) -> Result<[u8; N], PluginAuthorityError> {
    if value.len() != N * 2 {
        return Err(PluginAuthorityError::InvalidSignature);
    }
    let mut decoded = [0_u8; N];
    for (index, byte) in decoded.iter_mut().enumerate() {
        let offset = index * 2;
        *byte = u8::from_str_radix(&value[offset..offset + 2], 16)
            .map_err(|_| PluginAuthorityError::InvalidSignature)?;
    }
    Ok(decoded)
}

fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    left.len() == right.len()
        && left
            .iter()
            .zip(right)
            .fold(0_u8, |difference, (left, right)| {
                difference | (left ^ right)
            })
            == 0
}

fn safe_segment(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value != "."
        && value != ".."
        && !value.contains("..")
        && !value.contains('/')
        && !value.contains('\\')
        && !value.chars().any(char::is_control)
}

fn bounded_identifier(value: &str) -> String {
    value
        .chars()
        .filter(|character| character.is_ascii_alphanumeric() || *character == '-')
        .take(128)
        .collect()
}

fn bounded_safe_error(error: &str) -> String {
    error
        .chars()
        .filter(|character| !character.is_control())
        .take(1_024)
        .collect()
}
