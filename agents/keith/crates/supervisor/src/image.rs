use std::collections::{BTreeMap, BTreeSet};
use std::ffi::OsStr;
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Component, Path, PathBuf};

use fs2::FileExt;
use keith_agent_types::UtcTimestamp;
use keith_release::verify_detached_signature;
use serde::{Deserialize, Serialize};
use serde_json::Value;
use sha2::{Digest, Sha256};
use thiserror::Error;

const REGISTRY_VERSION: u32 = 1;
const IMAGE_FORMAT: &str = "keith-worker-image-v1";
const STATE_FILE: &str = "registry.json";
const LOCK_FILE: &str = "registry.lock";
const EXPECTED_GATES: [&str; 6] = [
    "formatting",
    "strict_clippy",
    "workspace_tests",
    "dependency_policy",
    "security",
    "platform",
];
const PROTECTED_PATHS: [&str; 22] = [
    ".cargo",
    ".git",
    ".keith",
    "Cargo.lock",
    "Cargo.toml",
    "apps/xtask",
    "backups",
    "crates/agent-loop",
    "crates/credentials",
    "crates/memory",
    "crates/release",
    "crates/sandbox",
    "crates/self-evolution/src/build.rs",
    "crates/self-evolution/src/guard.rs",
    "crates/self-evolution/src/ledger.rs",
    "crates/self-evolution/src/lib.rs",
    "crates/session-store",
    "crates/tool-runner",
    "resources",
    "rust-toolchain",
    "rust-toolchain.toml",
    "signing-keys",
];

#[derive(Clone, Debug, Eq, PartialEq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct InstalledImage {
    pub image_id: String,
    pub build_id: String,
    pub manifest_sha256: String,
    pub source_manifest_sha256: String,
    pub executable_sha256: String,
    pub change_class: String,
    pub executable: PathBuf,
    pub sequence: u64,
    pub verified: bool,
}

pub struct ImageInstallRequest<'a> {
    pub manifest: &'a [u8],
    pub signature: &'a [u8],
    pub executable: &'a [u8],
    pub trusted_public_key: &'a [u8; 32],
}

#[derive(Debug, Error)]
pub enum ImageRegistryError {
    #[error("worker image registry I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("worker image registry state is invalid: {0}")]
    State(#[from] serde_json::Error),
    #[error("worker image signature is invalid")]
    InvalidSignature,
    #[error("worker image manifest is invalid: {0}")]
    InvalidManifest(String),
    #[error("worker image payload digest does not match its manifest")]
    ArtifactMismatch,
    #[error("worker image {0} is not installed")]
    NotInstalled(String),
    #[error("worker image registry is locked by another daemon")]
    Locked,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct SignedManifest {
    format: String,
    build_id: String,
    base_revision: String,
    source_manifest_sha256: String,
    executable_sha256: String,
    executable_bytes: u64,
    toolchain: Value,
    worker_report: WorkerReport,
    gates: Vec<SignedGate>,
    artifact_source_paths: Vec<PathBuf>,
    change_class: String,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct WorkerReport {
    component: String,
    package_version: String,
    build_id: String,
    protocol_version: String,
    storage_schema: String,
    enabled_features: BTreeSet<String>,
}

#[derive(Debug, Deserialize)]
#[serde(deny_unknown_fields)]
struct SignedGate {
    gate: String,
    exit_code: i32,
    #[serde(rename = "elapsed_millis")]
    _elapsed_millis: u64,
    #[serde(rename = "output")]
    _output: String,
    output_sha256: String,
    sandbox: Value,
}

#[derive(Debug, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
struct RegistryState {
    version: u32,
    current: String,
    previous_known_good: Option<String>,
    #[serde(default)]
    known_good: Option<String>,
    #[serde(default)]
    retained_until: BTreeMap<String, UtcTimestamp>,
    next_sequence: u64,
    images: BTreeMap<String, InstalledImage>,
}

pub struct WorkerImageRegistry {
    root: PathBuf,
    _lock: File,
    state: RegistryState,
}

/// Exact files owned by the durable worker-image registry.
#[derive(Clone, Debug, Eq, PartialEq)]
pub struct WorkerImageDataInventory {
    pub registry_root: PathBuf,
    pub image_ids: Vec<String>,
    pub relative_files: Vec<PathBuf>,
}

/// Reads the authoritative registry without taking daemon ownership or changing its contents.
///
/// A missing registry is an empty installation scope. Registry lock files are coordination state
/// and are intentionally excluded from the returned data inventory.
///
/// # Errors
/// Returns an error when the registry, paths, or registered filesystem entries are invalid.
pub fn worker_image_data_inventory(
    root: impl AsRef<Path>,
) -> Result<WorkerImageDataInventory, ImageRegistryError> {
    let requested = root.as_ref();
    if !requested.exists() {
        return Ok(WorkerImageDataInventory {
            registry_root: requested.to_path_buf(),
            image_ids: Vec::new(),
            relative_files: Vec::new(),
        });
    }
    let root = fs::canonicalize(requested)?;
    let registry_path = root.join(STATE_FILE);
    if !registry_path.exists() {
        let images = root.join("images");
        let images_empty = !images.exists() || fs::read_dir(&images)?.next().is_none();
        let only_coordination = fs::read_dir(&root)?.all(|entry| {
            entry
                .is_ok_and(|entry| matches!(entry.file_name().to_str(), Some(LOCK_FILE | "images")))
        });
        if images_empty && only_coordination {
            return Ok(WorkerImageDataInventory {
                registry_root: root,
                image_ids: Vec::new(),
                relative_files: Vec::new(),
            });
        }
        return Err(ImageRegistryError::InvalidManifest(
            "worker image data exists without registry ownership".into(),
        ));
    }
    let metadata = fs::symlink_metadata(&registry_path)?;
    if !metadata.is_file() || metadata.file_type().is_symlink() {
        return Err(ImageRegistryError::ArtifactMismatch);
    }
    let state: RegistryState = serde_json::from_slice(&fs::read(&registry_path)?)?;
    validate_state(&root, &state)?;
    let mut relative_files = vec![PathBuf::from(STATE_FILE)];
    let mut image_ids = Vec::with_capacity(state.images.len());
    for (id, image) in &state.images {
        let directory = PathBuf::from("images").join(id);
        let directory_path = root.join(&directory);
        let directory_metadata = fs::symlink_metadata(&directory_path)?;
        if !directory_metadata.is_dir() || directory_metadata.file_type().is_symlink() {
            return Err(ImageRegistryError::ArtifactMismatch);
        }
        let mut names = vec!["agent-worker"];
        if image.verified {
            names.extend(["manifest.json", "manifest.sig"]);
        }
        for name in names {
            let relative = directory.join(name);
            let path = root.join(&relative);
            let metadata = fs::symlink_metadata(&path)?;
            if !metadata.is_file() || metadata.file_type().is_symlink() {
                return Err(ImageRegistryError::ArtifactMismatch);
            }
            relative_files.push(relative);
        }
        image_ids.push(id.clone());
    }
    relative_files.sort();
    image_ids.sort();
    Ok(WorkerImageDataInventory {
        registry_root: root,
        image_ids,
        relative_files,
    })
}

impl WorkerImageRegistry {
    /// Opens the durable image registry and creates an immutable bootstrap entry on first use.
    ///
    /// # Errors
    /// Returns an error when another daemon owns the registry, state is corrupt, or the bootstrap
    /// executable is not a regular file.
    pub fn open(root: impl Into<PathBuf>, bootstrap: &Path) -> Result<Self, ImageRegistryError> {
        let root = root.into();
        fs::create_dir_all(root.join("images"))?;
        let lock = OpenOptions::new()
            .create(true)
            .read(true)
            .write(true)
            .truncate(false)
            .open(root.join(LOCK_FILE))?;
        lock.try_lock_exclusive()
            .map_err(|_| ImageRegistryError::Locked)?;
        let state_path = root.join(STATE_FILE);
        let state = if state_path.exists() {
            let state: RegistryState = serde_json::from_slice(&fs::read(&state_path)?)?;
            validate_state(&root, &state)?;
            reconcile_orphans(&root, &state)?;
            state
        } else {
            bootstrap_state(&root, bootstrap)?
        };
        let registry = Self {
            root,
            _lock: lock,
            state,
        };
        if !state_path.exists() {
            registry.persist()?;
        }
        Ok(registry)
    }

    /// # Panics
    ///
    /// Panics if the registry's validated current pointer is missing, which cannot
    /// happen for a registry that opened successfully.
    #[must_use]
    pub fn current(&self) -> &InstalledImage {
        self.state
            .images
            .get(&self.state.current)
            .expect("validated registry current pointer")
    }

    #[must_use]
    pub fn previous_known_good(&self) -> Option<&InstalledImage> {
        self.state
            .previous_known_good
            .as_ref()
            .and_then(|id| self.state.images.get(id))
    }

    /// # Panics
    ///
    /// Panics if the registry's validated known-good pointer is missing, which cannot
    /// happen for a registry that opened successfully.
    #[must_use]
    pub fn known_good(&self) -> &InstalledImage {
        let id = self
            .state
            .known_good
            .as_ref()
            .unwrap_or(&self.state.current);
        self.state
            .images
            .get(id)
            .expect("validated registry known-good pointer")
    }

    #[must_use]
    pub fn installed(&self, image_id: &str) -> Option<&InstalledImage> {
        self.state.images.get(image_id)
    }

    /// Authenticates and installs immutable candidate bytes without changing the current pointer.
    ///
    /// # Errors
    /// Fails before publication for signature, manifest, protected-surface, class, or digest errors.
    pub fn install_verified(
        &mut self,
        request: &ImageInstallRequest<'_>,
    ) -> Result<InstalledImage, ImageRegistryError> {
        let manifest = verify_candidate(request)?;
        let image_id = sha256(request.manifest);
        if let Some(existing) = self.state.images.get(&image_id) {
            self.verify_installed(existing, Some(request.trusted_public_key))?;
            return Ok(existing.clone());
        }
        let sequence = self.state.next_sequence;
        let staging = self
            .root
            .join("images")
            .join(format!(".{image_id}.{}.tmp", std::process::id()));
        if staging.exists() {
            fs::remove_dir_all(&staging)?;
        }
        fs::create_dir(&staging)?;
        write_synced(&staging.join("manifest.json"), request.manifest)?;
        write_synced(&staging.join("manifest.sig"), request.signature)?;
        write_synced(&staging.join("agent-worker"), request.executable)?;
        set_executable(&staging.join("agent-worker"))?;
        sync_directory(&staging)?;
        let destination = self.root.join("images").join(&image_id);
        fs::rename(&staging, &destination)?;
        sync_directory(&self.root.join("images"))?;
        let installed = InstalledImage {
            image_id: image_id.clone(),
            build_id: manifest.build_id,
            manifest_sha256: image_id.clone(),
            source_manifest_sha256: manifest.source_manifest_sha256,
            executable_sha256: manifest.executable_sha256,
            change_class: manifest.change_class,
            executable: destination.join("agent-worker"),
            sequence,
            verified: true,
        };
        self.state.next_sequence = sequence.saturating_add(1);
        self.state.images.insert(image_id, installed.clone());
        if let Err(error) = self.persist() {
            self.state.images.remove(&installed.image_id);
            self.state.next_sequence = sequence;
            return Err(error);
        }
        Ok(installed)
    }

    /// Re-verifies an installed image immediately before atomically making it current.
    ///
    /// # Errors
    /// Leaves both pointers unchanged when verification or persistence fails.
    pub fn promote_verified(
        &mut self,
        image_id: &str,
        trusted_public_key: &[u8; 32],
    ) -> Result<InstalledImage, ImageRegistryError> {
        let image = self
            .state
            .images
            .get(image_id)
            .cloned()
            .ok_or_else(|| ImageRegistryError::NotInstalled(image_id.into()))?;
        if !image.verified {
            return Err(ImageRegistryError::InvalidManifest(
                "bootstrap images cannot be promoted as candidates".into(),
            ));
        }
        self.verify_installed(&image, Some(trusted_public_key))?;
        if self.state.current == image_id {
            return Ok(image);
        }
        let prior_current = self.state.current.clone();
        let prior_previous = self.state.previous_known_good.clone();
        let prior_known_good = self.state.known_good.clone();
        if self.state.known_good.is_none() {
            self.state.known_good = Some(prior_current.clone());
        }
        self.state.previous_known_good = Some(prior_current.clone());
        self.state.current = image_id.into();
        if let Err(error) = self.persist() {
            self.state.current = prior_current;
            self.state.previous_known_good = prior_previous;
            self.state.known_good = prior_known_good;
            return Err(error);
        }
        Ok(image)
    }

    /// Marks the current candidate as known-good after its observation window and durably retains
    /// the displaced known-good image until the declared deadline.
    ///
    /// # Errors
    /// Leaves registry state unchanged if identity, authentication, or persistence fails.
    pub fn advance_known_good(
        &mut self,
        image_id: &str,
        trusted_public_key: &[u8; 32],
        retain_previous_until: UtcTimestamp,
    ) -> Result<InstalledImage, ImageRegistryError> {
        if self.state.current != image_id {
            return Err(ImageRegistryError::InvalidManifest(
                "only the current observed candidate can become known-good".into(),
            ));
        }
        let image = self.resolve(image_id)?;
        self.verify_installed(&image, Some(trusted_public_key))?;
        if self.state.known_good.as_deref() == Some(image_id) {
            return Ok(image);
        }
        let prior_known_good = self.state.known_good.clone().unwrap_or_else(|| {
            self.state
                .previous_known_good
                .clone()
                .unwrap_or_else(|| self.state.current.clone())
        });
        let prior_state = (
            self.state.known_good.clone(),
            self.state.previous_known_good.clone(),
            self.state.retained_until.clone(),
        );
        self.state.known_good = Some(image_id.into());
        if prior_known_good != image_id {
            self.state.previous_known_good = Some(prior_known_good.clone());
            self.state
                .retained_until
                .insert(prior_known_good, retain_previous_until);
        }
        if let Err(error) = self.persist() {
            self.state.known_good = prior_state.0;
            self.state.previous_known_good = prior_state.1;
            self.state.retained_until = prior_state.2;
            return Err(error);
        }
        Ok(image)
    }

    /// Restores an exact installed image as both the current and pinned known-good image.
    ///
    /// Unlike promotion, restoration may select the installation bootstrap and never rotates the
    /// failed candidate into the known-good pointer.
    ///
    /// # Errors
    /// Leaves both pointers unchanged when the image is absent, altered, or persistence fails.
    pub fn restore_current(
        &mut self,
        image_id: &str,
    ) -> Result<InstalledImage, ImageRegistryError> {
        let image = self
            .state
            .images
            .get(image_id)
            .cloned()
            .ok_or_else(|| ImageRegistryError::NotInstalled(image_id.into()))?;
        self.verify_installed(&image, None)?;
        let prior_current = self.state.current.clone();
        let prior_previous = self.state.previous_known_good.clone();
        let prior_known_good = self.state.known_good.clone();
        self.state.current = image_id.into();
        self.state.previous_known_good = Some(image_id.into());
        self.state.known_good = Some(image_id.into());
        if let Err(error) = self.persist() {
            self.state.current = prior_current;
            self.state.previous_known_good = prior_previous;
            self.state.known_good = prior_known_good;
            return Err(error);
        }
        Ok(image)
    }

    /// Independently authenticates an installed signed image before restoring both durable
    /// pointers. Installation bootstrap images remain anchored by their immutable copied digest.
    ///
    /// # Errors
    /// Leaves both pointers unchanged when trust, identity, bytes, or persistence are invalid.
    pub fn restore_verified(
        &mut self,
        image_id: &str,
        trusted_public_key: &[u8; 32],
    ) -> Result<InstalledImage, ImageRegistryError> {
        let image = self
            .state
            .images
            .get(image_id)
            .cloned()
            .ok_or_else(|| ImageRegistryError::NotInstalled(image_id.into()))?;
        self.verify_installed(
            &image,
            if image.verified {
                Some(trusted_public_key)
            } else {
                None
            },
        )?;
        let prior_current = self.state.current.clone();
        let prior_previous = self.state.previous_known_good.clone();
        let prior_known_good = self.state.known_good.clone();
        self.state.current = image_id.into();
        self.state.previous_known_good = Some(image_id.into());
        self.state.known_good = Some(image_id.into());
        if let Err(error) = self.persist() {
            self.state.current = prior_current;
            self.state.previous_known_good = prior_previous;
            self.state.known_good = prior_known_good;
            return Err(error);
        }
        Ok(image)
    }

    /// Resolves and authenticates the exact executable bytes bound to a newly claimed generation.
    ///
    /// # Errors
    /// Fails closed on registry tampering, symlinks, or executable replacement.
    pub fn resolve_current(&self) -> Result<InstalledImage, ImageRegistryError> {
        let image = self.current().clone();
        self.verify_installed(&image, None)?;
        Ok(image)
    }

    /// Resolves an exact installed image for adoption or rollback without consulting pointers.
    ///
    /// # Errors
    /// Fails closed when the identity is absent or its immutable executable has changed.
    pub fn resolve(&self, image_id: &str) -> Result<InstalledImage, ImageRegistryError> {
        let image = self
            .state
            .images
            .get(image_id)
            .cloned()
            .ok_or_else(|| ImageRegistryError::NotInstalled(image_id.into()))?;
        self.verify_installed(&image, None)?;
        Ok(image)
    }

    /// Removes only superseded images not protected by pointers or live generation references.
    ///
    /// # Errors
    /// Returns an error when durable state or filesystem reclamation fails.
    pub fn reclaim(
        &mut self,
        retained_history: usize,
        live_image_ids: &BTreeSet<String>,
    ) -> Result<Vec<String>, ImageRegistryError> {
        let mut protected = live_image_ids.clone();
        protected.insert(self.state.current.clone());
        if let Some(previous) = &self.state.previous_known_good {
            protected.insert(previous.clone());
        }
        let mut by_age = self
            .state
            .images
            .values()
            .filter(|image| !protected.contains(&image.image_id))
            .map(|image| (image.sequence, image.image_id.clone()))
            .collect::<Vec<_>>();
        by_age.sort_by(|left, right| right.cmp(left));
        let remove = by_age
            .into_iter()
            .skip(retained_history)
            .map(|(_, id)| id)
            .collect::<Vec<_>>();
        if remove.is_empty() {
            return Ok(Vec::new());
        }
        let prior = remove
            .iter()
            .filter_map(|id| {
                self.state
                    .images
                    .remove(id)
                    .map(|image| (id.clone(), image))
            })
            .collect::<Vec<_>>();
        if let Err(error) = self.persist() {
            self.state.images.extend(prior);
            return Err(error);
        }
        for id in &remove {
            let directory = self.root.join("images").join(id);
            if directory.exists() {
                fs::remove_dir_all(directory)?;
            }
        }
        sync_directory(&self.root.join("images"))?;
        Ok(remove)
    }

    fn verify_installed(
        &self,
        image: &InstalledImage,
        trusted_public_key: Option<&[u8; 32]>,
    ) -> Result<(), ImageRegistryError> {
        let metadata = fs::symlink_metadata(&image.executable)?;
        if !metadata.is_file() || metadata.file_type().is_symlink() {
            return Err(ImageRegistryError::ArtifactMismatch);
        }
        let canonical = fs::canonicalize(&image.executable)?;
        let image_root = fs::canonicalize(self.root.join("images"))?;
        if !canonical.starts_with(image_root)
            || sha256(&fs::read(&canonical)?) != image.executable_sha256
        {
            return Err(ImageRegistryError::ArtifactMismatch);
        }
        if image.verified {
            let directory = canonical
                .parent()
                .ok_or(ImageRegistryError::ArtifactMismatch)?;
            let manifest = fs::read(directory.join("manifest.json"))?;
            if sha256(&manifest) != image.manifest_sha256 {
                return Err(ImageRegistryError::ArtifactMismatch);
            }
            if let Some(key) = trusted_public_key {
                let signature = fs::read(directory.join("manifest.sig"))?;
                let executable = fs::read(&canonical)?;
                let parsed = verify_candidate(&ImageInstallRequest {
                    manifest: &manifest,
                    signature: &signature,
                    executable: &executable,
                    trusted_public_key: key,
                })?;
                if parsed.build_id != image.build_id
                    || parsed.source_manifest_sha256 != image.source_manifest_sha256
                    || parsed.change_class != image.change_class
                {
                    return Err(ImageRegistryError::ArtifactMismatch);
                }
            }
        }
        Ok(())
    }

    fn persist(&self) -> Result<(), ImageRegistryError> {
        let temporary = self.root.join(format!(".{STATE_FILE}.tmp"));
        match fs::remove_file(&temporary) {
            Ok(()) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
        write_synced(&temporary, &serde_json::to_vec(&self.state)?)?;
        keith_platform::replace_file(&temporary, &self.root.join(STATE_FILE))?;
        sync_directory(&self.root)?;
        Ok(())
    }
}

fn bootstrap_state(root: &Path, executable: &Path) -> Result<RegistryState, ImageRegistryError> {
    let metadata = fs::symlink_metadata(executable)?;
    if !metadata.is_file() || metadata.file_type().is_symlink() {
        return Err(ImageRegistryError::ArtifactMismatch);
    }
    let bytes = fs::read(executable)?;
    let executable_sha256 = sha256(&bytes);
    let image_id = format!("bootstrap-{executable_sha256}");
    let destination = root.join("images").join(&image_id);
    fs::create_dir(&destination)?;
    let installed_path = destination.join("agent-worker");
    write_synced(&installed_path, &bytes)?;
    set_executable(&installed_path)?;
    sync_directory(&destination)?;
    sync_directory(&root.join("images"))?;
    let image = InstalledImage {
        image_id: image_id.clone(),
        build_id: "installation-bootstrap".into(),
        manifest_sha256: executable_sha256.clone(),
        source_manifest_sha256: executable_sha256.clone(),
        executable_sha256,
        change_class: "installation".into(),
        executable: installed_path,
        sequence: 1,
        verified: false,
    };
    Ok(RegistryState {
        version: REGISTRY_VERSION,
        current: image_id.clone(),
        previous_known_good: None,
        known_good: Some(image_id.clone()),
        retained_until: BTreeMap::new(),
        next_sequence: 2,
        images: BTreeMap::from([(image_id, image)]),
    })
}

fn validate_state(root: &Path, state: &RegistryState) -> Result<(), ImageRegistryError> {
    if state.version != REGISTRY_VERSION
        || !state.images.contains_key(&state.current)
        || state
            .known_good
            .as_ref()
            .is_some_and(|id| !state.images.contains_key(id))
        || state
            .previous_known_good
            .as_ref()
            .is_some_and(|id| !state.images.contains_key(id))
    {
        return Err(ImageRegistryError::InvalidManifest(
            "registry pointers or version are invalid".into(),
        ));
    }
    let image_root = root.join("images");
    for (id, image) in &state.images {
        if id != &image.image_id
            || !is_digest_or_bootstrap(id)
            || image.executable != image_root.join(id).join("agent-worker")
        {
            return Err(ImageRegistryError::InvalidManifest(
                "registry image identity or path is invalid".into(),
            ));
        }
    }
    Ok(())
}

fn reconcile_orphans(root: &Path, state: &RegistryState) -> Result<(), ImageRegistryError> {
    let images = root.join("images");
    for entry in fs::read_dir(&images)? {
        let entry = entry?;
        let name = entry.file_name().to_string_lossy().into_owned();
        if state.images.contains_key(&name) {
            continue;
        }
        let metadata = fs::symlink_metadata(entry.path())?;
        if metadata.is_dir() && !metadata.file_type().is_symlink() {
            fs::remove_dir_all(entry.path())?;
        } else {
            fs::remove_file(entry.path())?;
        }
    }
    sync_directory(&images)?;
    Ok(())
}

fn verify_candidate(
    request: &ImageInstallRequest<'_>,
) -> Result<SignedManifest, ImageRegistryError> {
    verify_detached_signature(
        request.manifest,
        request.signature,
        request.trusted_public_key,
    )
    .map_err(|_| ImageRegistryError::InvalidSignature)?;
    let manifest: SignedManifest = serde_json::from_slice(request.manifest)?;
    if manifest.format != IMAGE_FORMAT
        || manifest.build_id.trim().is_empty()
        || !is_sha256(&manifest.source_manifest_sha256)
        || !is_sha256(&manifest.executable_sha256)
        || manifest.base_revision.len() != 40
        || u64::try_from(request.executable.len()).ok() != Some(manifest.executable_bytes)
        || sha256(request.executable) != manifest.executable_sha256
    {
        return Err(ImageRegistryError::InvalidManifest(
            "identity or payload metadata is inconsistent".into(),
        ));
    }
    validate_worker_report(&manifest)?;
    validate_gates(&manifest.gates)?;
    let recomputed = classify_paths(&manifest.artifact_source_paths)?;
    if manifest.change_class != recomputed || manifest.change_class == "d" {
        return Err(ImageRegistryError::InvalidManifest(
            "artifact change class is inconsistent or protected".into(),
        ));
    }
    if !manifest.toolchain.is_object() {
        return Err(ImageRegistryError::InvalidManifest(
            "toolchain identity is missing".into(),
        ));
    }
    Ok(manifest)
}

fn validate_worker_report(manifest: &SignedManifest) -> Result<(), ImageRegistryError> {
    let report = &manifest.worker_report;
    if !matches!(report.component.as_str(), "worker" | "daemon")
        || report.build_id != manifest.build_id
        || report.package_version.trim().is_empty()
        || report.protocol_version.trim().is_empty()
        || report.storage_schema.trim().is_empty()
        || report.enabled_features.is_empty()
    {
        return Err(ImageRegistryError::InvalidManifest(
            "worker build identity is invalid".into(),
        ));
    }
    Ok(())
}

fn validate_gates(gates: &[SignedGate]) -> Result<(), ImageRegistryError> {
    if gates.len() != EXPECTED_GATES.len()
        || !gates.iter().zip(EXPECTED_GATES).all(|(actual, expected)| {
            let output_digest_valid = is_sha256(&actual.output_sha256);
            actual.gate == expected
                && actual.exit_code == 0
                && output_digest_valid
                && actual.sandbox.is_object()
        })
    {
        return Err(ImageRegistryError::InvalidManifest(
            "verification gate results are incomplete or invalid".into(),
        ));
    }
    Ok(())
}

fn classify_paths(paths: &[PathBuf]) -> Result<String, ImageRegistryError> {
    if paths.is_empty() {
        return Err(ImageRegistryError::InvalidManifest(
            "artifact source manifest is empty".into(),
        ));
    }
    let mut class = 'a';
    for path in paths {
        validate_relative(path)?;
        if is_protected(path) {
            return Ok("d".into());
        }
        let next = if is_class_c(path) {
            'c'
        } else if is_class_a(path) {
            'a'
        } else if path.extension() == Some(OsStr::new("rs")) {
            'b'
        } else {
            return Err(ImageRegistryError::InvalidManifest(format!(
                "unclassifiable artifact path {}",
                path.display()
            )));
        };
        class = class.max(next);
    }
    Ok(class.to_string())
}

fn validate_relative(path: &Path) -> Result<(), ImageRegistryError> {
    if path.is_absolute()
        || path.as_os_str().is_empty()
        || path.components().any(|component| {
            matches!(
                component,
                Component::ParentDir | Component::RootDir | Component::Prefix(_)
            )
        })
    {
        return Err(ImageRegistryError::InvalidManifest(format!(
            "unsafe artifact path {}",
            path.display()
        )));
    }
    Ok(())
}

fn is_protected(path: &Path) -> bool {
    PROTECTED_PATHS
        .iter()
        .any(|entry| path == Path::new(entry) || path.starts_with(entry))
        || path.file_name() == Some(OsStr::new("build.rs"))
}

fn is_class_a(path: &Path) -> bool {
    let extension = path.extension().and_then(OsStr::to_str);
    path.components().any(|component| {
        matches!(
            component.as_os_str().to_str(),
            Some("tests" | "benches" | "corpus" | "docs")
        )
    }) || matches!(extension, Some("md" | "txt"))
}

fn is_class_c(path: &Path) -> bool {
    path.file_name() == Some(OsStr::new("Cargo.toml"))
        || path.components().any(|component| {
            matches!(
                component.as_os_str().to_str(),
                Some("protocol" | "migrations" | "schema" | "daemon-core" | "agentd")
            )
        })
}

fn is_sha256(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

fn is_digest_or_bootstrap(value: &str) -> bool {
    is_sha256(value) || value.strip_prefix("bootstrap-").is_some_and(is_sha256)
}

fn sha256(bytes: &[u8]) -> String {
    format!("{:x}", Sha256::digest(bytes))
}

fn write_synced(path: &Path, bytes: &[u8]) -> Result<(), std::io::Error> {
    let mut file = OpenOptions::new().create_new(true).write(true).open(path)?;
    file.write_all(bytes)?;
    file.sync_all()
}

#[cfg(unix)]
fn set_executable(path: &Path) -> Result<(), std::io::Error> {
    use std::os::unix::fs::PermissionsExt;
    fs::set_permissions(path, fs::Permissions::from_mode(0o500))
}

#[cfg(not(unix))]
fn set_executable(_path: &Path) -> Result<(), std::io::Error> {
    Ok(())
}

fn sync_directory(path: &Path) -> Result<(), std::io::Error> {
    File::open(path)?.sync_all()
}

#[cfg(test)]
mod tests {
    use ring::rand::SystemRandom;
    use ring::signature::{Ed25519KeyPair, KeyPair};
    use serde_json::json;

    use super::*;

    fn signed_candidate(executable: &[u8], source_path: &str) -> (Vec<u8>, Vec<u8>, [u8; 32]) {
        let output = "real exit status: success";
        let gates = EXPECTED_GATES
            .iter()
            .map(|gate| {
                json!({
                    "gate": gate,
                    "exit_code": 0,
                    "elapsed_millis": 1,
                    "output": output,
                    "output_sha256": sha256(output.as_bytes()),
                    "sandbox": {"backend":"bubblewrap"}
                })
            })
            .collect::<Vec<_>>();
        let manifest = serde_json::to_vec(&json!({
            "format": IMAGE_FORMAT,
            "build_id": "candidate-1",
            "base_revision": "a".repeat(40),
            "source_manifest_sha256": "b".repeat(64),
            "executable_sha256": sha256(executable),
            "executable_bytes": executable.len(),
            "toolchain": {"rustc":"rustc 1.93", "cargo":"cargo 1.93", "target":"test"},
            "worker_report": {
                "component":"worker", "package_version":"0.1.0", "build_id":"candidate-1",
                "protocol_version":"1.0", "storage_schema":"1.0", "enabled_features":["runtime"]
            },
            "gates": gates,
            "artifact_source_paths": [source_path],
            "change_class": if std::path::Path::new(source_path)
                .extension()
                .is_some_and(|extension| extension.eq_ignore_ascii_case("rs"))
            {"b"} else {"a"}
        }))
        .unwrap();
        let key_bytes = Ed25519KeyPair::generate_pkcs8(&SystemRandom::new()).unwrap();
        let key = Ed25519KeyPair::from_pkcs8(key_bytes.as_ref()).unwrap();
        let public_key = key.public_key().as_ref().try_into().unwrap();
        let signature = key.sign(&manifest).as_ref().to_vec();
        (manifest, signature, public_key)
    }

    #[test]
    fn verified_install_promotion_and_retention_are_durable() {
        let directory = tempfile::tempdir().unwrap();
        let bootstrap = directory.path().join("bootstrap-worker");
        fs::write(&bootstrap, b"bootstrap").unwrap();
        let root = directory.path().join("registry");
        let mut registry = WorkerImageRegistry::open(&root, &bootstrap).unwrap();
        let bootstrap_id = registry.current().image_id.clone();
        let (manifest, signature, key) = signed_candidate(b"candidate", "crates/tools/src/lib.rs");
        let installed = registry
            .install_verified(&ImageInstallRequest {
                manifest: &manifest,
                signature: &signature,
                executable: b"candidate",
                trusted_public_key: &key,
            })
            .unwrap();
        assert_eq!(registry.current().image_id, bootstrap_id);
        registry
            .promote_verified(&installed.image_id, &key)
            .unwrap();
        assert_eq!(
            registry.resolve_current().unwrap().image_id,
            installed.image_id
        );
        assert_eq!(
            registry.previous_known_good().unwrap().image_id,
            bootstrap_id
        );
        drop(registry);
        let registry = WorkerImageRegistry::open(&root, &bootstrap).unwrap();
        assert_eq!(registry.current().image_id, installed.image_id);
    }

    #[test]
    fn retention_preserves_pointers_and_live_generation_references() {
        let directory = tempfile::tempdir().unwrap();
        let bootstrap = directory.path().join("bootstrap-worker");
        fs::write(&bootstrap, b"bootstrap").unwrap();
        let mut registry =
            WorkerImageRegistry::open(directory.path().join("registry"), &bootstrap).unwrap();
        let mut installed = Vec::new();
        for executable in [b"candidate-one".as_slice(), b"candidate-two".as_slice()] {
            let (manifest, signature, key) =
                signed_candidate(executable, "crates/tools/src/lib.rs");
            installed.push(
                registry
                    .install_verified(&ImageInstallRequest {
                        manifest: &manifest,
                        signature: &signature,
                        executable,
                        trusted_public_key: &key,
                    })
                    .unwrap(),
            );
        }
        let live = BTreeSet::from([installed[0].image_id.clone()]);
        let removed = registry.reclaim(0, &live).unwrap();
        assert_eq!(removed, [installed[1].image_id.clone()]);
        assert!(registry.installed(&installed[0].image_id).is_some());
        assert!(registry.installed(&registry.current().image_id).is_some());
    }

    #[test]
    fn tampering_and_protected_surface_are_rejected_without_pointer_change() {
        let directory = tempfile::tempdir().unwrap();
        let bootstrap = directory.path().join("bootstrap-worker");
        fs::write(&bootstrap, b"bootstrap").unwrap();
        let mut registry =
            WorkerImageRegistry::open(directory.path().join("registry"), &bootstrap).unwrap();
        let current = registry.current().image_id.clone();
        let (manifest, signature, key) = signed_candidate(b"candidate", "crates/tools/src/lib.rs");
        assert!(
            registry
                .install_verified(&ImageInstallRequest {
                    manifest: &manifest,
                    signature: &signature,
                    executable: b"tampered",
                    trusted_public_key: &key,
                })
                .is_err()
        );
        let installed = registry
            .install_verified(&ImageInstallRequest {
                manifest: &manifest,
                signature: &signature,
                executable: b"candidate",
                trusted_public_key: &key,
            })
            .unwrap();
        fs::write(&installed.executable, b"replaced after verification").unwrap();
        assert!(
            registry
                .promote_verified(&installed.image_id, &key)
                .is_err()
        );
        let (protected, signature, key) =
            signed_candidate(b"candidate", "crates/agent-loop/src/lib.rs");
        assert!(
            registry
                .install_verified(&ImageInstallRequest {
                    manifest: &protected,
                    signature: &signature,
                    executable: b"candidate",
                    trusted_public_key: &key,
                })
                .is_err()
        );
        assert_eq!(registry.current().image_id, current);
    }
}
