#![forbid(unsafe_code)]

use std::fmt::Write as _;
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{Arc, Mutex, MutexGuard};

use cap_fs_ext::{FollowSymlinks, OpenOptionsFollowExt};
use cap_std::ambient_authority;
use cap_std::fs::{Dir, OpenOptions};
use keith_agent_types::{
    ArtifactId, CURRENT_SCHEMA_VERSION, ChildId, ProfileId, RootTreeId, SchemaVersion, SessionId,
    UtcTimestamp,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

const TREES_DIRECTORY: &str = "trees";
const ARTIFACTS_DIRECTORY: &str = "artifacts";
const METADATA_FILE: &str = "metadata.json";
const CONTENT_FILE: &str = "content";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ArtifactLimits {
    pub max_artifact_bytes: usize,
    pub max_preview_bytes: usize,
    pub max_artifacts_per_tree: usize,
}

impl Default for ArtifactLimits {
    fn default() -> Self {
        Self {
            max_artifact_bytes: 256 * 1_024 * 1_024,
            max_preview_bytes: 4 * 1_024,
            max_artifacts_per_tree: 100_000,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ArtifactScope {
    pub root_tree_id: RootTreeId,
    pub session_id: SessionId,
    pub profile_id: ProfileId,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "source", content = "id")]
pub enum ArtifactSource {
    Tool,
    Kernel,
    Child(ChildId),
    User,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ArtifactState {
    Active,
    Archived,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "retention", content = "at")]
pub enum RetentionPolicy {
    Retain,
    DeleteAt(UtcTimestamp),
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DisplayMetadata {
    pub name: Option<String>,
    pub description: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ArtifactMetadata {
    pub version: SchemaVersion,
    pub id: ArtifactId,
    pub root_tree_id: RootTreeId,
    pub session_id: SessionId,
    pub profile_id: ProfileId,
    pub source: ArtifactSource,
    pub media_type: String,
    pub byte_length: u64,
    pub sha256: String,
    pub relative_path: PathBuf,
    pub created_at: UtcTimestamp,
    pub display: Option<DisplayMetadata>,
    pub state: ArtifactState,
    pub retention: RetentionPolicy,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ArtifactReference {
    pub id: ArtifactId,
    pub root_tree_id: RootTreeId,
    pub profile_id: ProfileId,
}

impl From<&ArtifactMetadata> for ArtifactReference {
    fn from(metadata: &ArtifactMetadata) -> Self {
        Self {
            id: metadata.id.clone(),
            root_tree_id: metadata.root_tree_id.clone(),
            profile_id: metadata.profile_id.clone(),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct NewArtifact<'a> {
    pub scope: ArtifactScope,
    pub source: ArtifactSource,
    pub media_type: &'a str,
    pub bytes: &'a [u8],
    pub created_at: UtcTimestamp,
    pub display: Option<DisplayMetadata>,
    pub retention: RetentionPolicy,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ArtifactExport {
    pub metadata: ArtifactMetadata,
    pub content: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ChildDeliverable {
    pub child_id: ChildId,
    pub artifacts: Vec<ArtifactReference>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SpilledOutput {
    pub artifact_id: ArtifactId,
    pub path: PathBuf,
    pub bytes: usize,
    pub preview: String,
    pub media_type: String,
}

pub trait OutputSpill: Send + Sync {
    /// # Errors
    ///
    /// Returns an artifact error when output cannot be stored durably.
    fn spill(&self, bytes: &[u8]) -> Result<SpilledOutput, ArtifactError>;
}

#[derive(Debug, Error)]
pub enum ArtifactError {
    #[error("artifact I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("artifact metadata encoding failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("artifact is missing")]
    NotFound,
    #[error("artifact access crosses an owning tree or profile")]
    AccessDenied,
    #[error("artifact content exceeds its configured limit")]
    Oversized,
    #[error("artifact metadata, media type, or relative path is invalid")]
    Invalid,
    #[error("artifact content digest or length is corrupt")]
    Corrupt,
    #[error("artifact store lock was poisoned")]
    LockPoisoned,
    #[error("artifact tree reached its configured count limit")]
    CountLimit,
}

pub struct ArtifactService {
    root: Dir,
    ambient_root: PathBuf,
    limits: ArtifactLimits,
    lock: Mutex<()>,
}

impl ArtifactService {
    /// # Errors
    ///
    /// Returns an error when the content root cannot be created, restricted, or opened.
    pub fn open(root: impl AsRef<Path>, limits: ArtifactLimits) -> Result<Self, ArtifactError> {
        std::fs::create_dir_all(root.as_ref())?;
        restrict_directory(root.as_ref())?;
        let ambient_root = std::fs::canonicalize(root.as_ref())?;
        let root = Dir::open_ambient_dir(&ambient_root, ambient_authority())?;
        root.create_dir_all(TREES_DIRECTORY)?;
        Ok(Self {
            root,
            ambient_root,
            limits,
            lock: Mutex::new(()),
        })
    }

    /// # Errors
    ///
    /// Returns an error when validation, count bounds, or atomic creation fails.
    pub fn create(&self, new: NewArtifact<'_>) -> Result<ArtifactMetadata, ArtifactError> {
        validate_media_type(new.media_type)?;
        validate_display(new.display.as_ref())?;
        if new.bytes.len() > self.limits.max_artifact_bytes {
            return Err(ArtifactError::Oversized);
        }
        let _guard = self.lock()?;
        let artifacts_directory = tree_artifacts_directory(&new.scope.root_tree_id);
        self.root.create_dir_all(&artifacts_directory)?;
        if self.root.read_dir(&artifacts_directory)?.count() >= self.limits.max_artifacts_per_tree {
            return Err(ArtifactError::CountLimit);
        }
        let id = ArtifactId::new();
        let directory = artifacts_directory.join(id.to_string());
        self.root.create_dir(&directory)?;
        let content_path = directory.join(CONTENT_FILE);
        let metadata = ArtifactMetadata {
            version: CURRENT_SCHEMA_VERSION,
            id: id.clone(),
            root_tree_id: new.scope.root_tree_id,
            session_id: new.scope.session_id,
            profile_id: new.scope.profile_id,
            source: new.source,
            media_type: new.media_type.to_owned(),
            byte_length: u64::try_from(new.bytes.len()).map_err(|_| ArtifactError::Oversized)?,
            sha256: hex_digest(new.bytes),
            relative_path: content_path.clone(),
            created_at: new.created_at,
            display: new.display,
            state: ArtifactState::Active,
            retention: new.retention,
        };
        let result: Result<(), ArtifactError> = (|| {
            self.write_new(&content_path, new.bytes)?;
            self.atomic_metadata_write(&directory, &metadata)?;
            self.sync_directory(&directory)?;
            self.sync_directory(&artifacts_directory)?;
            Ok(())
        })();
        if result.is_err() {
            let _ = self.root.remove_file(&content_path);
            let _ = self.root.remove_file(directory.join(METADATA_FILE));
            let _ = self.root.remove_dir(&directory);
        }
        result?;
        Ok(metadata)
    }

    /// # Errors
    ///
    /// Returns an error for missing, corrupt, or cross-scope artifact metadata.
    pub fn inspect(
        &self,
        access: &ArtifactScope,
        reference: &ArtifactReference,
    ) -> Result<ArtifactMetadata, ArtifactError> {
        validate_reference_access(access, reference)?;
        let metadata = self.read_metadata(reference)?;
        validate_metadata(&metadata, reference)?;
        validate_metadata_access(access, &metadata)?;
        if !self.root.try_exists(&metadata.relative_path)? {
            return Err(ArtifactError::NotFound);
        }
        Ok(metadata)
    }

    /// # Errors
    ///
    /// Returns an error for cross-scope access, missing data, oversize data, or digest corruption.
    pub fn download(
        &self,
        access: &ArtifactScope,
        reference: &ArtifactReference,
    ) -> Result<Vec<u8>, ArtifactError> {
        let metadata = self.inspect(access, reference)?;
        let content = self.read_bounded(&metadata.relative_path)?;
        if u64::try_from(content.len()).ok() != Some(metadata.byte_length)
            || hex_digest(&content) != metadata.sha256
        {
            return Err(ArtifactError::Corrupt);
        }
        Ok(content)
    }

    /// # Errors
    ///
    /// Returns an error when tree metadata cannot be read or an entry is cross-profile/corrupt.
    pub fn list(&self, access: &ArtifactScope) -> Result<Vec<ArtifactMetadata>, ArtifactError> {
        let directory = tree_artifacts_directory(&access.root_tree_id);
        let entries = match self.root.read_dir(&directory) {
            Ok(entries) => entries,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => return Ok(Vec::new()),
            Err(error) => return Err(error.into()),
        };
        let mut artifacts = Vec::new();
        for entry in entries {
            let entry = entry?;
            if entry.file_type()?.is_symlink() || !entry.file_type()?.is_dir() {
                return Err(ArtifactError::Corrupt);
            }
            let id = entry
                .file_name()
                .to_string_lossy()
                .parse::<ArtifactId>()
                .map_err(|_| ArtifactError::Corrupt)?;
            let reference = ArtifactReference {
                id,
                root_tree_id: access.root_tree_id.clone(),
                profile_id: access.profile_id.clone(),
            };
            artifacts.push(self.inspect(access, &reference)?);
        }
        artifacts.sort_by(|left, right| left.created_at.cmp(&right.created_at));
        Ok(artifacts)
    }

    /// # Errors
    ///
    /// Returns an error when access, content validation, or export assembly fails.
    pub fn export(
        &self,
        access: &ArtifactScope,
        reference: &ArtifactReference,
    ) -> Result<ArtifactExport, ArtifactError> {
        Ok(ArtifactExport {
            metadata: self.inspect(access, reference)?,
            content: self.download(access, reference)?,
        })
    }

    /// # Errors
    ///
    /// Returns an error when access fails or archived metadata cannot be persisted atomically.
    pub fn archive(
        &self,
        access: &ArtifactScope,
        reference: &ArtifactReference,
    ) -> Result<ArtifactMetadata, ArtifactError> {
        let _guard = self.lock()?;
        let mut metadata = self.inspect(access, reference)?;
        metadata.state = ArtifactState::Archived;
        let directory = artifact_directory(&reference.root_tree_id, &reference.id);
        self.atomic_metadata_write(&directory, &metadata)?;
        Ok(metadata)
    }

    /// # Errors
    ///
    /// Returns an error when access fails or known artifact files cannot be removed explicitly.
    pub fn delete(
        &self,
        access: &ArtifactScope,
        reference: &ArtifactReference,
    ) -> Result<(), ArtifactError> {
        let _guard = self.lock()?;
        let metadata = self.inspect(access, reference)?;
        let directory = artifact_directory(&reference.root_tree_id, &reference.id);
        self.root.remove_file(&metadata.relative_path)?;
        self.root.remove_file(directory.join(METADATA_FILE))?;
        self.root.remove_dir(&directory)?;
        self.sync_directory(&tree_artifacts_directory(&reference.root_tree_id))?;
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when retention metadata cannot be scanned or expired content cannot delete.
    pub fn cleanup_expired(&self, now: UtcTimestamp) -> Result<Vec<ArtifactId>, ArtifactError> {
        let mut expired = Vec::new();
        for tree in self.root.read_dir(TREES_DIRECTORY)? {
            let tree = tree?;
            if tree.file_type()?.is_symlink() || !tree.file_type()?.is_dir() {
                return Err(ArtifactError::Corrupt);
            }
            let root_tree_id = tree
                .file_name()
                .to_string_lossy()
                .parse::<RootTreeId>()
                .map_err(|_| ArtifactError::Corrupt)?;
            let directory = tree_artifacts_directory(&root_tree_id);
            let artifacts = match self.root.read_dir(&directory) {
                Ok(artifacts) => artifacts,
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
                Err(error) => return Err(error.into()),
            };
            for entry in artifacts {
                let entry = entry?;
                let id = entry
                    .file_name()
                    .to_string_lossy()
                    .parse::<ArtifactId>()
                    .map_err(|_| ArtifactError::Corrupt)?;
                let metadata = self.read_metadata(&ArtifactReference {
                    id: id.clone(),
                    root_tree_id: root_tree_id.clone(),
                    profile_id: ProfileId::new(),
                })?;
                if matches!(metadata.retention, RetentionPolicy::DeleteAt(at) if at <= now) {
                    let access = ArtifactScope {
                        root_tree_id: metadata.root_tree_id.clone(),
                        session_id: metadata.session_id.clone(),
                        profile_id: metadata.profile_id.clone(),
                    };
                    self.delete(&access, &ArtifactReference::from(&metadata))?;
                    expired.push(id);
                }
            }
        }
        Ok(expired)
    }

    /// # Errors
    ///
    /// Returns an error when any referenced artifact is cross-scope, missing, or not child-owned.
    pub fn child_deliverable(
        &self,
        access: &ArtifactScope,
        child_id: ChildId,
        references: Vec<ArtifactReference>,
    ) -> Result<ChildDeliverable, ArtifactError> {
        for reference in &references {
            let metadata = self.inspect(access, reference)?;
            if metadata.source != ArtifactSource::Child(child_id.clone()) {
                return Err(ArtifactError::AccessDenied);
            }
        }
        Ok(ChildDeliverable {
            child_id,
            artifacts: references,
        })
    }

    pub fn scoped_spill(
        self: &Arc<Self>,
        scope: ArtifactScope,
        source: ArtifactSource,
        media_type: impl Into<String>,
        retention: RetentionPolicy,
    ) -> ArtifactSpill {
        ArtifactSpill {
            service: Arc::clone(self),
            scope,
            source,
            media_type: media_type.into(),
            retention,
        }
    }

    fn read_metadata(
        &self,
        reference: &ArtifactReference,
    ) -> Result<ArtifactMetadata, ArtifactError> {
        let path = artifact_directory(&reference.root_tree_id, &reference.id).join(METADATA_FILE);
        let bytes = match self.read_bounded_with_limit(&path, 1024 * 1024) {
            Ok(bytes) => bytes,
            Err(ArtifactError::Io(error)) if error.kind() == std::io::ErrorKind::NotFound => {
                return Err(ArtifactError::NotFound);
            }
            Err(error) => return Err(error),
        };
        serde_json::from_slice(&bytes).map_err(|_| ArtifactError::Corrupt)
    }

    fn read_bounded(&self, path: &Path) -> Result<Vec<u8>, ArtifactError> {
        self.read_bounded_with_limit(path, self.limits.max_artifact_bytes)
    }

    fn read_bounded_with_limit(&self, path: &Path, limit: usize) -> Result<Vec<u8>, ArtifactError> {
        if self.root.symlink_metadata(path)?.file_type().is_symlink() {
            return Err(ArtifactError::Corrupt);
        }
        let mut options = OpenOptions::new();
        options.read(true).follow(FollowSymlinks::No);
        let file = self.root.open_with(path, &options)?;
        if usize::try_from(file.metadata()?.len()).map_or(true, |length| length > limit) {
            return Err(ArtifactError::Oversized);
        }
        let mut bytes = Vec::new();
        file.take(u64::try_from(limit).unwrap_or(u64::MAX).saturating_add(1))
            .read_to_end(&mut bytes)?;
        if bytes.len() > limit {
            return Err(ArtifactError::Oversized);
        }
        Ok(bytes)
    }

    fn write_new(&self, path: &Path, bytes: &[u8]) -> Result<(), ArtifactError> {
        let mut options = OpenOptions::new();
        options
            .write(true)
            .create_new(true)
            .follow(FollowSymlinks::No);
        configure_file_mode(&mut options);
        let mut file = self.root.open_with(path, &options)?;
        file.write_all(bytes)?;
        file.sync_all()?;
        Ok(())
    }

    fn atomic_metadata_write(
        &self,
        directory: &Path,
        metadata: &ArtifactMetadata,
    ) -> Result<(), ArtifactError> {
        let temporary = directory.join(format!(".{METADATA_FILE}.{}.tmp", ArtifactId::new()));
        let target = directory.join(METADATA_FILE);
        let bytes = serde_json::to_vec(metadata)?;
        let result = (|| {
            self.write_new(&temporary, &bytes)?;
            self.root.rename(&temporary, &self.root, &target)?;
            self.sync_directory(directory)?;
            Ok(())
        })();
        if result.is_err() {
            let _ = self.root.remove_file(&temporary);
        }
        result
    }

    fn sync_directory(&self, relative: &Path) -> Result<(), ArtifactError> {
        std::fs::File::open(self.ambient_root.join(relative))?.sync_all()?;
        Ok(())
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, ArtifactError> {
        self.lock.lock().map_err(|_| ArtifactError::LockPoisoned)
    }
}

pub struct ArtifactSpill {
    service: Arc<ArtifactService>,
    scope: ArtifactScope,
    source: ArtifactSource,
    media_type: String,
    retention: RetentionPolicy,
}

impl OutputSpill for ArtifactSpill {
    fn spill(&self, bytes: &[u8]) -> Result<SpilledOutput, ArtifactError> {
        let media_type = if self.media_type == "auto" {
            detect_media_type(bytes)
        } else {
            self.media_type.clone()
        };
        let preview = preview(bytes, &media_type, self.service.limits.max_preview_bytes);
        let metadata = self.service.create(NewArtifact {
            scope: self.scope.clone(),
            source: self.source.clone(),
            media_type: &media_type,
            bytes,
            created_at: UtcTimestamp::now().map_err(|_| ArtifactError::Invalid)?,
            display: None,
            retention: self.retention,
        })?;
        Ok(SpilledOutput {
            artifact_id: metadata.id,
            path: self.service.ambient_root.join(&metadata.relative_path),
            bytes: bytes.len(),
            preview,
            media_type,
        })
    }
}

fn artifact_directory(root: &RootTreeId, id: &ArtifactId) -> PathBuf {
    tree_artifacts_directory(root).join(id.to_string())
}

fn tree_artifacts_directory(root: &RootTreeId) -> PathBuf {
    PathBuf::from(TREES_DIRECTORY)
        .join(root.to_string())
        .join(ARTIFACTS_DIRECTORY)
}

fn validate_reference_access(
    access: &ArtifactScope,
    reference: &ArtifactReference,
) -> Result<(), ArtifactError> {
    if access.root_tree_id != reference.root_tree_id || access.profile_id != reference.profile_id {
        return Err(ArtifactError::AccessDenied);
    }
    Ok(())
}

fn validate_metadata_access(
    access: &ArtifactScope,
    metadata: &ArtifactMetadata,
) -> Result<(), ArtifactError> {
    if access.root_tree_id != metadata.root_tree_id || access.profile_id != metadata.profile_id {
        return Err(ArtifactError::AccessDenied);
    }
    Ok(())
}

fn validate_metadata(
    metadata: &ArtifactMetadata,
    reference: &ArtifactReference,
) -> Result<(), ArtifactError> {
    if metadata.version.major != CURRENT_SCHEMA_VERSION.major
        || metadata.id != reference.id
        || metadata.root_tree_id != reference.root_tree_id
        || metadata.profile_id != reference.profile_id
        || metadata.relative_path
            != artifact_directory(&metadata.root_tree_id, &metadata.id).join(CONTENT_FILE)
    {
        return Err(ArtifactError::Corrupt);
    }
    validate_media_type(&metadata.media_type)?;
    validate_display(metadata.display.as_ref())
}

fn validate_media_type(media_type: &str) -> Result<(), ArtifactError> {
    let valid = media_type.len() <= 255
        && media_type.split_once('/').is_some_and(|(kind, subtype)| {
            !kind.is_empty()
                && !subtype.is_empty()
                && kind.chars().chain(subtype.chars()).all(|character| {
                    character.is_ascii_alphanumeric() || matches!(character, '-' | '+' | '.')
                })
        });
    if valid {
        Ok(())
    } else {
        Err(ArtifactError::Invalid)
    }
}

fn validate_display(display: Option<&DisplayMetadata>) -> Result<(), ArtifactError> {
    if display.is_some_and(|display| {
        display.name.as_ref().is_some_and(|value| value.len() > 512)
            || display
                .description
                .as_ref()
                .is_some_and(|value| value.len() > 8 * 1_024)
    }) {
        return Err(ArtifactError::Invalid);
    }
    Ok(())
}

fn detect_media_type(bytes: &[u8]) -> String {
    if std::str::from_utf8(bytes).is_ok() {
        "text/plain".into()
    } else {
        "application/octet-stream".into()
    }
}

fn preview(bytes: &[u8], media_type: &str, limit: usize) -> String {
    if media_type.starts_with("text/")
        && let Ok(text) = std::str::from_utf8(bytes)
    {
        let mut preview = String::new();
        for character in text.chars() {
            if preview.len().saturating_add(character.len_utf8()) > limit {
                break;
            }
            preview.push(character);
        }
        return preview;
    }
    let byte_limit = limit / 2;
    hex_encode(&bytes[..bytes.len().min(byte_limit)])
}

fn hex_digest(bytes: &[u8]) -> String {
    hex_encode(&Sha256::digest(bytes))
}

fn hex_encode(bytes: &[u8]) -> String {
    let mut encoded = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        write!(&mut encoded, "{byte:02x}").expect("writing to a string cannot fail");
    }
    encoded
}

#[cfg(unix)]
fn restrict_directory(path: &Path) -> Result<(), ArtifactError> {
    use std::os::unix::fs::PermissionsExt;

    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))?;
    Ok(())
}

#[cfg(not(unix))]
fn restrict_directory(_path: &Path) -> Result<(), ArtifactError> {
    Ok(())
}

#[cfg(unix)]
fn configure_file_mode(options: &mut OpenOptions) {
    use cap_std::fs::OpenOptionsExt;

    options.mode(0o600);
}

#[cfg(not(unix))]
fn configure_file_mode(_options: &mut OpenOptions) {}

#[cfg(test)]
mod tests {
    use std::sync::Arc;
    use std::thread;

    use super::*;

    fn scope() -> ArtifactScope {
        ArtifactScope {
            root_tree_id: RootTreeId::new(),
            session_id: SessionId::new(),
            profile_id: ProfileId::new(),
        }
    }

    fn create(
        service: &ArtifactService,
        scope: &ArtifactScope,
        bytes: &[u8],
        source: ArtifactSource,
        retention: RetentionPolicy,
    ) -> ArtifactMetadata {
        service
            .create(NewArtifact {
                scope: scope.clone(),
                source,
                media_type: "text/plain",
                bytes,
                created_at: UtcTimestamp::from_unix_millis(10),
                display: Some(DisplayMetadata {
                    name: Some("result.txt".into()),
                    description: Some("checked output".into()),
                }),
                retention,
            })
            .unwrap()
    }

    #[test]
    fn digest_validates_across_restart_and_corruption_is_detected() {
        let directory = tempfile::tempdir().unwrap();
        let access = scope();
        let metadata = {
            let service =
                ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
            create(
                &service,
                &access,
                b"durable content",
                ArtifactSource::User,
                RetentionPolicy::Retain,
            )
        };
        let service = ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
        let reference = ArtifactReference::from(&metadata);
        assert_eq!(
            service.download(&access, &reference).unwrap(),
            b"durable content"
        );
        assert_eq!(service.inspect(&access, &reference).unwrap(), metadata);
        std::fs::write(directory.path().join(&metadata.relative_path), b"corrupt").unwrap();
        assert!(matches!(
            service.download(&access, &reference),
            Err(ArtifactError::Corrupt)
        ));
    }

    #[test]
    fn cross_tree_and_cross_profile_access_are_denied_before_content_lookup() {
        let directory = tempfile::tempdir().unwrap();
        let service = ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
        let access = scope();
        let metadata = create(
            &service,
            &access,
            b"private",
            ArtifactSource::User,
            RetentionPolicy::Retain,
        );
        let reference = ArtifactReference::from(&metadata);
        let other_profile = ArtifactScope {
            profile_id: ProfileId::new(),
            ..access.clone()
        };
        assert!(matches!(
            service.inspect(&other_profile, &reference),
            Err(ArtifactError::AccessDenied)
        ));
        let other_tree = ArtifactScope {
            root_tree_id: RootTreeId::new(),
            ..access
        };
        assert!(matches!(
            service.download(&other_tree, &reference),
            Err(ArtifactError::AccessDenied)
        ));
    }

    #[test]
    fn concurrent_creation_produces_unique_complete_artifacts() {
        let directory = tempfile::tempdir().unwrap();
        let service =
            Arc::new(ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap());
        let access = scope();
        let handles = (0..16)
            .map(|index| {
                let service = Arc::clone(&service);
                let access = access.clone();
                thread::spawn(move || {
                    create(
                        &service,
                        &access,
                        format!("artifact-{index}").as_bytes(),
                        ArtifactSource::Tool,
                        RetentionPolicy::Retain,
                    )
                })
            })
            .collect::<Vec<_>>();
        let metadata = handles
            .into_iter()
            .map(|handle| handle.join().unwrap())
            .collect::<Vec<_>>();
        let ids = metadata
            .iter()
            .map(|artifact| artifact.id.clone())
            .collect::<std::collections::BTreeSet<_>>();
        assert_eq!(ids.len(), 16);
        assert_eq!(service.list(&access).unwrap().len(), 16);
        for artifact in metadata {
            service
                .download(&access, &ArtifactReference::from(&artifact))
                .unwrap();
        }
    }

    #[test]
    fn spill_has_bounded_preview_media_detection_and_oversize_rejection() {
        let directory = tempfile::tempdir().unwrap();
        let service = Arc::new(
            ArtifactService::open(
                directory.path(),
                ArtifactLimits {
                    max_artifact_bytes: 32,
                    max_preview_bytes: 8,
                    max_artifacts_per_tree: 10,
                },
            )
            .unwrap(),
        );
        let access = scope();
        let spill = service.scoped_spill(
            access.clone(),
            ArtifactSource::Kernel,
            "auto",
            RetentionPolicy::Retain,
        );
        let text = spill.spill(b"abcdefghijk").unwrap();
        assert_eq!(text.preview, "abcdefgh");
        assert_eq!(text.media_type, "text/plain");
        let binary = spill.spill(&[0xff, 0x00, 0x01, 0x02, 0x03]).unwrap();
        assert_eq!(binary.preview.len(), 8);
        assert_eq!(binary.media_type, "application/octet-stream");
        assert!(matches!(
            spill.spill(&[b'x'; 33]),
            Err(ArtifactError::Oversized)
        ));
        assert_eq!(service.list(&access).unwrap().len(), 2);
    }

    #[test]
    fn archive_export_child_refs_delete_retention_and_stale_refs_are_consistent() {
        let directory = tempfile::tempdir().unwrap();
        let service = ArtifactService::open(directory.path(), ArtifactLimits::default()).unwrap();
        let access = scope();
        let child_id = ChildId::new();
        let child = create(
            &service,
            &access,
            b"child result",
            ArtifactSource::Child(child_id.clone()),
            RetentionPolicy::Retain,
        );
        let child_ref = ArtifactReference::from(&child);
        let deliverable = service
            .child_deliverable(&access, child_id.clone(), vec![child_ref.clone()])
            .unwrap();
        assert_eq!(deliverable.child_id, child_id);
        assert_eq!(deliverable.artifacts, vec![child_ref.clone()]);
        let archived = service.archive(&access, &child_ref).unwrap();
        assert_eq!(archived.state, ArtifactState::Archived);
        let exported = service.export(&access, &child_ref).unwrap();
        assert_eq!(exported.content, b"child result");
        service.delete(&access, &child_ref).unwrap();
        assert!(matches!(
            service.inspect(&access, &child_ref),
            Err(ArtifactError::NotFound)
        ));

        let expired = create(
            &service,
            &access,
            b"expired",
            ArtifactSource::Tool,
            RetentionPolicy::DeleteAt(UtcTimestamp::from_unix_millis(50)),
        );
        assert!(
            service
                .cleanup_expired(UtcTimestamp::from_unix_millis(49))
                .unwrap()
                .is_empty()
        );
        assert_eq!(
            service
                .cleanup_expired(UtcTimestamp::from_unix_millis(50))
                .unwrap(),
            vec![expired.id]
        );
    }

    #[test]
    fn metadata_path_tampering_and_tree_count_limits_are_rejected() {
        let directory = tempfile::tempdir().unwrap();
        let service = ArtifactService::open(
            directory.path(),
            ArtifactLimits {
                max_artifacts_per_tree: 1,
                ..ArtifactLimits::default()
            },
        )
        .unwrap();
        let access = scope();
        let metadata = create(
            &service,
            &access,
            b"first",
            ArtifactSource::User,
            RetentionPolicy::Retain,
        );
        assert!(matches!(
            service.create(NewArtifact {
                scope: access.clone(),
                source: ArtifactSource::User,
                media_type: "text/plain",
                bytes: b"second",
                created_at: UtcTimestamp::from_unix_millis(20),
                display: None,
                retention: RetentionPolicy::Retain,
            }),
            Err(ArtifactError::CountLimit)
        ));
        let metadata_path = directory
            .path()
            .join(artifact_directory(&metadata.root_tree_id, &metadata.id))
            .join(METADATA_FILE);
        let mut value: serde_json::Value =
            serde_json::from_slice(&std::fs::read(&metadata_path).unwrap()).unwrap();
        value["relative_path"] = serde_json::json!("../../outside");
        std::fs::write(metadata_path, serde_json::to_vec(&value).unwrap()).unwrap();
        assert!(matches!(
            service.inspect(&access, &ArtifactReference::from(&metadata)),
            Err(ArtifactError::Corrupt)
        ));
    }
}
