#![forbid(unsafe_code)]

use std::collections::BTreeMap;
use std::fs;
use std::path::{Path, PathBuf};

use keith_plugin_sdk::{MANIFEST_FILE, MODULE_FILE, ManifestError, PluginHook, PluginManifest};
use serde::{Deserialize, Serialize};
use thiserror::Error;
use wasmtime::{Config, Engine, Instance, Module, Store, StoreLimits, StoreLimitsBuilder};

const LEDGER_FILE: &str = "plugins.json";
const MAX_MODULE_BYTES: u64 = 32 * 1_024 * 1_024;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PluginState {
    Active,
    Disabled,
    Quarantined,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PluginRecord {
    pub id: String,
    pub active_version: String,
    pub state: PluginState,
    pub safe_error: Option<String>,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct PluginLedger {
    plugins: BTreeMap<String, PluginRecord>,
}

struct StoreState {
    limits: StoreLimits,
}

#[derive(Debug, Error)]
pub enum PluginHostError {
    #[error("plugin manifest failed validation: {0}")]
    Manifest(#[from] ManifestError),
    #[error("plugin I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("plugin state JSON failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("plugin module is invalid or incompatible: {0}")]
    InvalidModule(String),
    #[error("plugin hook failed within its sandbox: {0}")]
    HookFailed(String),
    #[error("plugin hook returned failure status {0}")]
    HookStatus(i32),
    #[error("plugin package is incomplete or exceeds its bound")]
    InvalidPackage,
    #[error("plugin or requested version was not found")]
    NotFound,
    #[error("plugin version is already installed")]
    AlreadyInstalled,
    #[error("third-party activation is prohibited in safe mode")]
    SafeMode,
}

pub struct PluginHost {
    root: PathBuf,
    engine: Engine,
    ledger: PluginLedger,
    safe_mode: bool,
}

impl PluginHost {
    /// Opens the isolated WASM plugin host and discovers valid installed packages.
    ///
    /// # Errors
    ///
    /// Returns an error for engine, filesystem, or durable-state failure.
    pub fn open(root: impl AsRef<Path>, safe_mode: bool) -> Result<Self, PluginHostError> {
        fs::create_dir_all(root.as_ref())?;
        let root = fs::canonicalize(root.as_ref())?;
        let mut config = Config::new();
        config.consume_fuel(true);
        let engine = Engine::new(&config)
            .map_err(|error| PluginHostError::InvalidModule(error.to_string()))?;
        let ledger_path = root.join(LEDGER_FILE);
        let ledger = if ledger_path.exists() {
            serde_json::from_slice(&fs::read(ledger_path)?)?
        } else {
            PluginLedger::default()
        };
        let mut host = Self {
            root,
            engine,
            ledger,
            safe_mode,
        };
        host.reconcile()?;
        Ok(host)
    }

    pub const fn safe_mode(&self) -> bool {
        self.safe_mode
    }

    pub fn records(&self) -> impl Iterator<Item = &PluginRecord> {
        self.ledger.plugins.values()
    }

    pub fn record(&self, id: &str) -> Option<&PluginRecord> {
        self.ledger.plugins.get(id)
    }

    /// Installs or updates from a complete package directory and activates only after migration.
    ///
    /// # Errors
    ///
    /// Returns an error without changing the active version if validation or migration fails.
    pub fn install(&mut self, package: impl AsRef<Path>) -> Result<PluginRecord, PluginHostError> {
        let manifest = read_manifest(package.as_ref())?;
        let module_bytes = read_module(package.as_ref())?;
        self.compile(&module_bytes)?;
        let version_directory = self.version_directory(&manifest.id, &manifest.version);
        if version_directory.exists() {
            return Err(PluginHostError::AlreadyInstalled);
        }
        let staging = self
            .root
            .join(format!(".install-{}-{}", manifest.id, manifest.version));
        match fs::remove_dir_all(&staging) {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
        fs::create_dir_all(&staging)?;
        fs::write(staging.join(MANIFEST_FILE), toml_bytes(package.as_ref())?)?;
        fs::write(staging.join(MODULE_FILE), &module_bytes)?;
        if let Some(parent) = version_directory.parent() {
            fs::create_dir_all(parent)?;
        }
        fs::rename(&staging, &version_directory)?;

        let previous = self.ledger.plugins.get(&manifest.id).cloned();
        let activation = (|| {
            if previous.is_some() && manifest.hooks.contains(&PluginHook::Migrate) {
                self.run_hook(&manifest, &module_bytes, PluginHook::Migrate)?;
            }
            if !self.safe_mode && manifest.hooks.contains(&PluginHook::Activate) {
                self.run_hook(&manifest, &module_bytes, PluginHook::Activate)?;
            }
            Ok::<(), PluginHostError>(())
        })();
        if let Err(error) = activation {
            fs::remove_dir_all(&version_directory)?;
            return Err(error);
        }
        let record = PluginRecord {
            id: manifest.id.clone(),
            active_version: manifest.version,
            state: if self.safe_mode {
                PluginState::Disabled
            } else {
                PluginState::Active
            },
            safe_error: None,
        };
        self.ledger.plugins.insert(manifest.id, record.clone());
        self.persist()?;
        Ok(record)
    }

    /// Activates a validated installed plugin.
    ///
    /// # Errors
    ///
    /// Returns an error in safe mode or when the hook fails.
    pub fn activate(&mut self, id: &str) -> Result<PluginRecord, PluginHostError> {
        if self.safe_mode {
            return Err(PluginHostError::SafeMode);
        }
        let record = self
            .ledger
            .plugins
            .get(id)
            .cloned()
            .ok_or(PluginHostError::NotFound)?;
        let (manifest, module) = self.load_version(id, &record.active_version)?;
        if manifest.hooks.contains(&PluginHook::Activate) {
            self.run_hook(&manifest, &module, PluginHook::Activate)?;
        }
        self.set_state(id, PluginState::Active, None)
    }

    /// Runs health in an isolated fresh store and quarantines failures.
    ///
    /// # Errors
    ///
    /// Returns the isolated hook failure after persisting quarantine state.
    pub fn health(&mut self, id: &str) -> Result<(), PluginHostError> {
        if self.safe_mode {
            return Err(PluginHostError::SafeMode);
        }
        let record = self
            .ledger
            .plugins
            .get(id)
            .cloned()
            .ok_or(PluginHostError::NotFound)?;
        let (manifest, module) = self.load_version(id, &record.active_version)?;
        if !manifest.hooks.contains(&PluginHook::Health) {
            return Ok(());
        }
        if let Err(error) = self.run_hook(&manifest, &module, PluginHook::Health) {
            self.set_state(id, PluginState::Quarantined, Some(error.to_string()))?;
            return Err(error);
        }
        Ok(())
    }

    /// Executes an explicitly declared command or tool hook for one active plugin.
    ///
    /// # Errors
    ///
    /// Returns an error when the plugin is disabled, quarantined, missing the hook, or fails in
    /// its bounded isolated store.
    pub fn invoke(&self, id: &str, hook: PluginHook) -> Result<(), PluginHostError> {
        if !matches!(hook, PluginHook::Command | PluginHook::Tool) {
            return Err(PluginHostError::InvalidPackage);
        }
        let record = self
            .ledger
            .plugins
            .get(id)
            .filter(|record| record.state == PluginState::Active)
            .ok_or(PluginHostError::NotFound)?;
        let (manifest, module) = self.load_version(id, &record.active_version)?;
        if !manifest.hooks.contains(&hook) {
            return Err(PluginHostError::NotFound);
        }
        self.run_hook(&manifest, &module, hook)
    }

    /// Disables a plugin without deleting installed versions.
    ///
    /// # Errors
    ///
    /// Returns an error if the plugin is absent or state cannot persist.
    pub fn disable(&mut self, id: &str) -> Result<PluginRecord, PluginHostError> {
        self.set_state(id, PluginState::Disabled, None)
    }

    /// Rolls active state back to a previously installed, revalidated version.
    ///
    /// # Errors
    ///
    /// Returns an error if that version is missing or activation fails.
    pub fn rollback(&mut self, id: &str, version: &str) -> Result<PluginRecord, PluginHostError> {
        let (manifest, module) = self.load_version(id, version)?;
        if !self.safe_mode && manifest.hooks.contains(&PluginHook::Activate) {
            self.run_hook(&manifest, &module, PluginHook::Activate)?;
        }
        let record = self
            .ledger
            .plugins
            .get_mut(id)
            .ok_or(PluginHostError::NotFound)?;
        version.clone_into(&mut record.active_version);
        record.state = if self.safe_mode {
            PluginState::Disabled
        } else {
            PluginState::Active
        };
        record.safe_error = None;
        let record = record.clone();
        self.persist()?;
        Ok(record)
    }

    /// Uninstalls all third-party bytes and durable state for a plugin.
    ///
    /// # Errors
    ///
    /// Returns an error if the plugin is absent or cleanup fails.
    pub fn uninstall(&mut self, id: &str) -> Result<PluginRecord, PluginHostError> {
        let record = self
            .ledger
            .plugins
            .remove(id)
            .ok_or(PluginHostError::NotFound)?;
        let directory = self.root.join(id);
        if directory.exists() {
            fs::remove_dir_all(directory)?;
        }
        self.persist()?;
        Ok(record)
    }

    fn run_hook(
        &self,
        manifest: &PluginManifest,
        module_bytes: &[u8],
        hook: PluginHook,
    ) -> Result<(), PluginHostError> {
        let module = self.compile(module_bytes)?;
        let limits = StoreLimitsBuilder::new()
            .memory_size(manifest.grants.max_memory_bytes)
            .instances(1)
            .memories(1)
            .build();
        let mut store = Store::new(&self.engine, StoreState { limits });
        store.limiter(|state| &mut state.limits);
        store
            .set_fuel(manifest.grants.max_fuel)
            .map_err(|error| PluginHostError::HookFailed(error.to_string()))?;
        let instance = Instance::new(&mut store, &module, &[])
            .map_err(|error| PluginHostError::HookFailed(error.to_string()))?;
        let function = instance
            .get_typed_func::<(), i32>(&mut store, hook.export_name())
            .map_err(|error| PluginHostError::HookFailed(error.to_string()))?;
        let status = function
            .call(&mut store, ())
            .map_err(|error| PluginHostError::HookFailed(error.to_string()))?;
        if status == 0 {
            Ok(())
        } else {
            Err(PluginHostError::HookStatus(status))
        }
    }

    fn compile(&self, bytes: &[u8]) -> Result<Module, PluginHostError> {
        Module::from_binary(&self.engine, bytes)
            .map_err(|error| PluginHostError::InvalidModule(error.to_string()))
    }

    fn load_version(
        &self,
        id: &str,
        version: &str,
    ) -> Result<(PluginManifest, Vec<u8>), PluginHostError> {
        let directory = self.version_directory(id, version);
        if !directory.is_dir() {
            return Err(PluginHostError::NotFound);
        }
        Ok((read_manifest(&directory)?, read_module(&directory)?))
    }

    fn version_directory(&self, id: &str, version: &str) -> PathBuf {
        self.root.join(id).join(version)
    }

    fn set_state(
        &mut self,
        id: &str,
        state: PluginState,
        safe_error: Option<String>,
    ) -> Result<PluginRecord, PluginHostError> {
        let record = self
            .ledger
            .plugins
            .get_mut(id)
            .ok_or(PluginHostError::NotFound)?;
        record.state = state;
        record.safe_error = safe_error;
        let record = record.clone();
        self.persist()?;
        Ok(record)
    }

    fn reconcile(&mut self) -> Result<(), PluginHostError> {
        self.ledger
            .plugins
            .retain(|id, record| self.root.join(id).join(&record.active_version).is_dir());
        let mut discovered = Vec::new();
        for plugin_entry in fs::read_dir(&self.root)? {
            let plugin_entry = plugin_entry?;
            if !plugin_entry.file_type()?.is_dir() {
                continue;
            }
            for version_entry in fs::read_dir(plugin_entry.path())? {
                let version_entry = version_entry?;
                if !version_entry.file_type()?.is_dir() {
                    continue;
                }
                let directory = version_entry.path();
                let Ok(manifest) = read_manifest(&directory) else {
                    continue;
                };
                let Ok(module) = read_module(&directory) else {
                    continue;
                };
                if self.compile(&module).is_ok() {
                    discovered.push(manifest);
                }
            }
        }
        discovered.sort_by(|left, right| {
            left.id
                .cmp(&right.id)
                .then_with(|| left.version.cmp(&right.version))
        });
        for manifest in discovered {
            self.ledger
                .plugins
                .entry(manifest.id.clone())
                .or_insert(PluginRecord {
                    id: manifest.id,
                    active_version: manifest.version,
                    state: PluginState::Disabled,
                    safe_error: None,
                });
        }
        if self.safe_mode {
            for record in self.ledger.plugins.values_mut() {
                record.state = PluginState::Disabled;
            }
        }
        self.persist()
    }

    fn persist(&self) -> Result<(), PluginHostError> {
        let temporary = self.root.join(format!(".{LEDGER_FILE}.tmp"));
        fs::write(&temporary, serde_json::to_vec_pretty(&self.ledger)?)?;
        keith_platform::replace_file(&temporary, &self.root.join(LEDGER_FILE))?;
        Ok(())
    }
}

fn read_manifest(directory: &Path) -> Result<PluginManifest, PluginHostError> {
    let input = fs::read_to_string(directory.join(MANIFEST_FILE))?;
    PluginManifest::parse(&input).map_err(PluginHostError::from)
}

fn toml_bytes(directory: &Path) -> Result<Vec<u8>, PluginHostError> {
    fs::read(directory.join(MANIFEST_FILE)).map_err(PluginHostError::from)
}

fn read_module(directory: &Path) -> Result<Vec<u8>, PluginHostError> {
    let path = directory.join(MODULE_FILE);
    let metadata = fs::metadata(&path)?;
    if !metadata.is_file() || metadata.len() == 0 || metadata.len() > MAX_MODULE_BYTES {
        return Err(PluginHostError::InvalidPackage);
    }
    fs::read(path).map_err(PluginHostError::from)
}

#[cfg(test)]
mod tests {
    use std::collections::BTreeSet;

    use keith_plugin_sdk::{PluginKind, ResourceGrants};
    use tempfile::TempDir;

    use super::*;

    fn package(
        root: &Path,
        id: &str,
        version: &str,
        hooks: BTreeSet<PluginHook>,
        wasm: &str,
        grants: ResourceGrants,
    ) -> PathBuf {
        let package = root.join(format!("{id}-{version}"));
        fs::create_dir_all(&package).expect("package directory");
        let manifest = PluginManifest {
            manifest_version: 1,
            id: id.to_owned(),
            name: id.to_owned(),
            version: version.to_owned(),
            host_api_min: 1,
            host_api_max: 1,
            kind: PluginKind::WasiComponent,
            hooks,
            grants,
        };
        fs::write(
            package.join(MANIFEST_FILE),
            toml::to_string(&manifest).expect("manifest TOML"),
        )
        .expect("write manifest");
        fs::write(
            package.join(MODULE_FILE),
            wat::parse_str(wasm).expect("WAT module"),
        )
        .expect("write module");
        package
    }

    #[test]
    fn lifecycle_update_migration_rollback_safe_mode_and_cleanup_are_durable() {
        let root = TempDir::new().expect("host root");
        let packages = TempDir::new().expect("packages");
        let hooks = BTreeSet::from([
            PluginHook::Activate,
            PluginHook::Health,
            PluginHook::Migrate,
        ]);
        let good = "(module
            (func (export \"keith_activate\") (result i32) i32.const 0)
            (func (export \"keith_health\") (result i32) i32.const 0)
            (func (export \"keith_migrate\") (result i32) i32.const 0))";
        let first = package(
            packages.path(),
            "lifecycle",
            "1.0.0",
            hooks.clone(),
            good,
            ResourceGrants::default(),
        );
        let second = package(
            packages.path(),
            "lifecycle",
            "2.0.0",
            hooks.clone(),
            good,
            ResourceGrants::default(),
        );
        let failed = package(
            packages.path(),
            "lifecycle",
            "3.0.0",
            hooks,
            "(module
              (func (export \"keith_activate\") (result i32) i32.const 0)
              (func (export \"keith_health\") (result i32) i32.const 0)
              (func (export \"keith_migrate\") (result i32) i32.const 7))",
            ResourceGrants::default(),
        );
        let mut host = PluginHost::open(root.path(), false).expect("open");
        host.install(&first).expect("install");
        host.install(&second).expect("update");
        assert!(matches!(
            host.install(&failed),
            Err(PluginHostError::HookStatus(7))
        ));
        assert_eq!(
            host.record("lifecycle").expect("record").active_version,
            "2.0.0"
        );
        host.rollback("lifecycle", "1.0.0").expect("rollback");
        host.disable("lifecycle").expect("disable");
        drop(host);
        let mut safe = PluginHost::open(root.path(), true).expect("safe-mode restart");
        assert_eq!(
            safe.record("lifecycle").expect("discovered").state,
            PluginState::Disabled
        );
        assert!(matches!(
            safe.activate("lifecycle"),
            Err(PluginHostError::SafeMode)
        ));
        safe.uninstall("lifecycle").expect("uninstall");
        assert!(!root.path().join("lifecycle").exists());
    }

    #[test]
    fn malicious_import_timeout_memory_and_crash_are_isolated_and_quarantined() {
        let root = TempDir::new().expect("host root");
        let packages = TempDir::new().expect("packages");
        let health = BTreeSet::from([PluginHook::Health]);
        let cases = [
            (
                "ambient",
                "(module
                  (import \"wasi_snapshot_preview1\" \"fd_write\"
                    (func $fd_write (param i32 i32 i32 i32) (result i32)))
                  (func (export \"keith_health\") (result i32) i32.const 0))",
            ),
            (
                "timeout",
                "(module (func (export \"keith_health\") (result i32)
                  (loop $again br $again) i32.const 0))",
            ),
            (
                "memory",
                "(module (memory 1 100)
                  (func (export \"keith_health\") (result i32)
                    i32.const 50 memory.grow i32.const -1 i32.eq
                    if unreachable end i32.const 0))",
            ),
            (
                "crash",
                "(module (func (export \"keith_health\") (result i32)
                  unreachable))",
            ),
        ];
        let mut host = PluginHost::open(root.path(), false).expect("open");
        for (id, wasm) in cases {
            let grants = ResourceGrants {
                max_memory_bytes: 64 * 1_024,
                max_fuel: 10_000,
                ..ResourceGrants::default()
            };
            let package = package(packages.path(), id, "1.0.0", health.clone(), wasm, grants);
            host.install(package).expect("module validates");
            assert!(host.health(id).is_err(), "{id} must be contained");
            assert_eq!(
                host.record(id).expect("record").state,
                PluginState::Quarantined
            );
        }
    }

    #[test]
    fn invalid_manifest_never_installs() {
        let root = TempDir::new().expect("host root");
        let package = TempDir::new().expect("package");
        fs::write(package.path().join(MANIFEST_FILE), "not = [valid").expect("invalid manifest");
        fs::write(package.path().join(MODULE_FILE), b"wasm").expect("module");
        let mut host = PluginHost::open(root.path(), false).expect("open");
        assert!(matches!(
            host.install(package.path()),
            Err(PluginHostError::Manifest(_))
        ));
        assert!(host.records().next().is_none());
    }
}
