#![forbid(unsafe_code)]

use std::fmt::Write as _;
use std::fmt::{self, Debug};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};
use std::sync::{Mutex, MutexGuard};

use cap_fs_ext::{FollowSymlinks, OpenOptionsFollowExt};
use cap_std::ambient_authority;
use cap_std::fs::{Dir, OpenOptions};
use keith_agent_types::{CURRENT_SCHEMA_VERSION, EntityId, ProfileId, SchemaVersion, UtcTimestamp};
use keith_model_registry::CredentialResolver;
use keith_provider_core::{ProviderCredential, ProviderError, ProviderErrorKind};
use ring::aead::{AES_256_GCM, Aad, LessSafeKey, Nonce, UnboundKey};
use ring::rand::{SecureRandom, SystemRandom};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

const NONCE_BYTES: usize = 12;
const KEY_BYTES: usize = 32;
const MAX_SECRET_BYTES: usize = 64 * 1_024;

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "owner", content = "id")]
pub enum CredentialOwner {
    Provider(String),
    Channel(String),
    Mcp(String),
    Tool(String),
    ProfileService {
        profile_id: ProfileId,
        service: CredentialService,
        resource: String,
    },
}

#[derive(Clone, Copy, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CredentialService {
    Channel,
    Acp,
    Plugin,
    ConnectedApp,
    Computer,
    Teaching,
}

impl CredentialOwner {
    pub const fn kind(&self) -> &'static str {
        match self {
            Self::Provider(_) => "provider",
            Self::Channel(_) => "channel",
            Self::Mcp(_) => "mcp",
            Self::Tool(_) => "tool",
            Self::ProfileService { .. } => "profile_service",
        }
    }

    fn id(&self) -> &str {
        match self {
            Self::Provider(id) | Self::Channel(id) | Self::Mcp(id) | Self::Tool(id) => id,
            Self::ProfileService { resource, .. } => resource,
        }
    }
}

#[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CredentialRef {
    pub name: String,
    pub owner: CredentialOwner,
}

impl CredentialRef {
    /// # Errors
    ///
    /// Returns an error when the stable non-secret name or owner ID is malformed.
    pub fn new(name: impl Into<String>, owner: CredentialOwner) -> Result<Self, CredentialError> {
        let reference = Self {
            name: name.into(),
            owner,
        };
        reference.validate()?;
        Ok(reference)
    }

    fn validate(&self) -> Result<(), CredentialError> {
        if !valid_identifier(&self.name) || !valid_identifier(self.owner.id()) {
            return Err(CredentialError::InvalidReference);
        }
        Ok(())
    }

    fn aad(&self) -> Result<Vec<u8>, CredentialError> {
        serde_json::to_vec(self).map_err(CredentialError::Serialize)
    }

    fn filename(&self) -> Result<String, CredentialError> {
        Ok(format!("{}.cred", hex_digest(&self.aad()?)))
    }
}

pub struct SecretValue {
    bytes: Vec<u8>,
}

impl SecretValue {
    /// # Errors
    ///
    /// Returns an error for empty, oversized, or line-breaking secret values.
    pub fn new(value: impl Into<Vec<u8>>) -> Result<Self, CredentialError> {
        let bytes = value.into();
        if bytes.is_empty() || bytes.len() > MAX_SECRET_BYTES {
            return Err(CredentialError::InvalidSecret);
        }
        Ok(Self { bytes })
    }

    pub fn with_bytes<T>(&self, use_secret: impl FnOnce(&[u8]) -> T) -> T {
        use_secret(&self.bytes)
    }
}

impl Debug for SecretValue {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("SecretValue([REDACTED])")
    }
}

impl Drop for SecretValue {
    fn drop(&mut self) {
        self.bytes.fill(0);
    }
}

pub struct MasterKey {
    bytes: [u8; KEY_BYTES],
}

impl MasterKey {
    /// # Errors
    ///
    /// Returns an error when the operating system random source fails.
    pub fn generate() -> Result<Self, CredentialError> {
        let mut bytes = [0_u8; KEY_BYTES];
        SystemRandom::new()
            .fill(&mut bytes)
            .map_err(|_| CredentialError::Crypto)?;
        Ok(Self { bytes })
    }

    pub const fn from_bytes(bytes: [u8; KEY_BYTES]) -> Self {
        Self { bytes }
    }
}

impl Debug for MasterKey {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("MasterKey([REDACTED])")
    }
}

impl Drop for MasterKey {
    fn drop(&mut self) {
        self.bytes.fill(0);
    }
}

pub struct NativeMasterKeyStore {
    service: String,
    account: String,
}

pub struct RestrictedMasterKeyStore {
    directory: Dir,
    ambient_root: PathBuf,
}

impl Debug for RestrictedMasterKeyStore {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("RestrictedMasterKeyStore")
            .field("root", &"<redacted-path>")
            .finish_non_exhaustive()
    }
}

impl RestrictedMasterKeyStore {
    /// Opens a permission-restricted local key store.
    ///
    /// # Errors
    ///
    /// Returns an error when the directory cannot be created, restricted, or opened.
    pub fn open(root: impl AsRef<Path>) -> Result<Self, CredentialError> {
        std::fs::create_dir_all(root.as_ref())?;
        restrict_directory(root.as_ref())?;
        let ambient_root = std::fs::canonicalize(root.as_ref())?;
        let directory = Dir::open_ambient_dir(&ambient_root, ambient_authority())?;
        Ok(Self {
            directory,
            ambient_root,
        })
    }

    /// Loads the existing master key or creates it atomically with owner-only permissions.
    ///
    /// # Errors
    ///
    /// Returns an error when the key is inaccessible, malformed, or cannot be persisted safely.
    pub fn load_or_create(&self) -> Result<MasterKey, CredentialError> {
        const FILENAME: &str = "master-key";
        match self.load(FILENAME) {
            Ok(key) => return Ok(key),
            Err(CredentialError::Io(error)) if error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error),
        }
        let key = MasterKey::generate()?;
        let mut options = OpenOptions::new();
        options
            .write(true)
            .create_new(true)
            .follow(FollowSymlinks::No);
        configure_file_mode(&mut options);
        match self.directory.open_with(FILENAME, &options) {
            Ok(mut file) => {
                file.write_all(&key.bytes)?;
                file.sync_all()?;
                std::fs::File::open(&self.ambient_root)?.sync_all()?;
                Ok(key)
            }
            Err(error) if error.kind() == std::io::ErrorKind::AlreadyExists => self.load(FILENAME),
            Err(error) => Err(error.into()),
        }
    }

    fn load(&self, filename: &str) -> Result<MasterKey, CredentialError> {
        let metadata = self.directory.symlink_metadata(filename)?;
        if metadata.file_type().is_symlink() || !metadata.is_file() {
            return Err(CredentialError::Corrupt);
        }
        restrict_key_file(&self.ambient_root.join(filename))?;
        let mut file = self.directory.open(filename)?;
        let mut bytes = Vec::new();
        file.read_to_end(&mut bytes)?;
        let key: [u8; KEY_BYTES] = bytes.try_into().map_err(|_| CredentialError::Corrupt)?;
        Ok(MasterKey::from_bytes(key))
    }
}

impl Debug for NativeMasterKeyStore {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("NativeMasterKeyStore")
            .field("backend", &Self::backend_name())
            .finish_non_exhaustive()
    }
}

impl NativeMasterKeyStore {
    /// # Errors
    ///
    /// Returns an error when the native service or account identifier is unsafe.
    pub fn new(
        service: impl Into<String>,
        account: impl Into<String>,
    ) -> Result<Self, CredentialError> {
        let store = Self {
            service: service.into(),
            account: account.into(),
        };
        if !valid_identifier(&store.service) || !valid_identifier(&store.account) {
            return Err(CredentialError::InvalidReference);
        }
        Ok(store)
    }

    pub const fn backend_name() -> &'static str {
        if cfg!(target_os = "linux") {
            "secret_service"
        } else if cfg!(target_os = "macos") {
            "apple_keychain"
        } else if cfg!(target_os = "windows") {
            "windows_credential_manager"
        } else {
            "unsupported"
        }
    }

    /// # Errors
    ///
    /// Returns an error when the native store is unavailable, corrupt, or cannot persist a key.
    pub fn load_or_create(&self) -> Result<MasterKey, CredentialError> {
        let entry = keyring::v1::Entry::new(&self.service, &self.account)
            .map_err(|error| CredentialError::NativeStore(error.to_string()))?;
        match entry.get_secret() {
            Ok(secret) => {
                let bytes: [u8; KEY_BYTES] =
                    secret.try_into().map_err(|_| CredentialError::Corrupt)?;
                Ok(MasterKey::from_bytes(bytes))
            }
            Err(keyring::v1::Error::NoEntry) => {
                let key = MasterKey::generate()?;
                entry
                    .set_secret(&key.bytes)
                    .map_err(|error| CredentialError::NativeStore(error.to_string()))?;
                Ok(key)
            }
            Err(error) => Err(CredentialError::NativeStore(error.to_string())),
        }
    }

    /// # Errors
    ///
    /// Returns an error when the native credential cannot be removed.
    pub fn delete(&self) -> Result<(), CredentialError> {
        let entry = keyring::v1::Entry::new(&self.service, &self.account)
            .map_err(|error| CredentialError::NativeStore(error.to_string()))?;
        match entry.delete_credential() {
            Ok(()) | Err(keyring::v1::Error::NoEntry) => Ok(()),
            Err(error) => Err(CredentialError::NativeStore(error.to_string())),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CredentialMetadata {
    pub reference: CredentialRef,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CredentialInspection {
    pub owner_kind: String,
    pub reference: String,
    pub configured: bool,
    pub updated_at: UtcTimestamp,
}

impl From<&CredentialMetadata> for CredentialInspection {
    fn from(metadata: &CredentialMetadata) -> Self {
        Self {
            owner_kind: metadata.reference.owner.kind().into(),
            reference: "<redacted-reference>".into(),
            configured: true,
            updated_at: metadata.updated_at,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct EncryptedRecord {
    version: SchemaVersion,
    metadata: CredentialMetadata,
    nonce: [u8; NONCE_BYTES],
    ciphertext: String,
}

#[derive(Debug, Error)]
pub enum CredentialError {
    #[error("credential reference is invalid")]
    InvalidReference,
    #[error("credential value is invalid")]
    InvalidSecret,
    #[error("credential is not configured")]
    NotFound,
    #[error("credential owner is not authorized")]
    ScopeDenied,
    #[error("credential request authentication failed")]
    Authentication,
    #[error("credential browser origin is not allowed")]
    Origin,
    #[error("credential browser CSRF validation failed")]
    Csrf,
    #[error("credential browser payload exceeded its limit")]
    PayloadLimit,
    #[error("credential encryption failed")]
    Crypto,
    #[error("credential persistence failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("native credential store failed: {0}")]
    NativeStore(String),
    #[error("credential record serialization failed: {0}")]
    Serialize(serde_json::Error),
    #[error("credential record is corrupt")]
    Corrupt,
    #[error("credential store lock was poisoned")]
    LockPoisoned,
}

pub struct EncryptedCredentialStore {
    directory: Dir,
    ambient_root: PathBuf,
    key: MasterKey,
    random: SystemRandom,
    lock: Mutex<()>,
}

impl Debug for EncryptedCredentialStore {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("EncryptedCredentialStore")
            .field("root", &"<redacted-path>")
            .finish_non_exhaustive()
    }
}

impl EncryptedCredentialStore {
    /// # Errors
    ///
    /// Returns an error when the restricted store directory cannot be created or opened.
    pub fn open(root: impl AsRef<Path>, key: MasterKey) -> Result<Self, CredentialError> {
        std::fs::create_dir_all(root.as_ref())?;
        restrict_directory(root.as_ref())?;
        let ambient_root = std::fs::canonicalize(root.as_ref())?;
        let directory = Dir::open_ambient_dir(&ambient_root, ambient_authority())?;
        Ok(Self {
            directory,
            ambient_root,
            key,
            random: SystemRandom::new(),
            lock: Mutex::new(()),
        })
    }

    /// # Errors
    ///
    /// Returns an error when the reference, encryption, or atomic persistence fails.
    #[allow(clippy::needless_pass_by_value)]
    pub fn put(
        &self,
        reference: CredentialRef,
        mut secret: SecretValue,
        timestamp: UtcTimestamp,
    ) -> Result<CredentialMetadata, CredentialError> {
        reference.validate()?;
        let _guard = self.lock()?;
        let filename = reference.filename()?;
        let created_at = self
            .read_record_if_present(&filename)?
            .map_or(timestamp, |record| record.metadata.created_at);
        let metadata = CredentialMetadata {
            reference: reference.clone(),
            created_at,
            updated_at: timestamp,
        };
        let mut nonce = [0_u8; NONCE_BYTES];
        self.random
            .fill(&mut nonce)
            .map_err(|_| CredentialError::Crypto)?;
        let key = LessSafeKey::new(
            UnboundKey::new(&AES_256_GCM, &self.key.bytes).map_err(|_| CredentialError::Crypto)?,
        );
        key.seal_in_place_append_tag(
            Nonce::assume_unique_for_key(nonce),
            Aad::from(metadata_aad(&metadata)?),
            &mut secret.bytes,
        )
        .map_err(|_| CredentialError::Crypto)?;
        let record = EncryptedRecord {
            version: CURRENT_SCHEMA_VERSION,
            metadata: metadata.clone(),
            nonce,
            ciphertext: hex_encode(&secret.bytes),
        };
        self.atomic_write(
            &filename,
            &serde_json::to_vec(&record).map_err(CredentialError::Serialize)?,
        )?;
        Ok(metadata)
    }

    /// # Errors
    ///
    /// Returns an error when the reference is missing, scoped to another owner, or cannot decrypt.
    pub fn resolve(
        &self,
        reference: &CredentialRef,
        requester: &CredentialOwner,
    ) -> Result<SecretValue, CredentialError> {
        reference.validate()?;
        if &reference.owner != requester {
            return Err(CredentialError::ScopeDenied);
        }
        let _guard = self.lock()?;
        let record = self
            .read_record_if_present(&reference.filename()?)?
            .ok_or(CredentialError::NotFound)?;
        if &record.metadata.reference != reference {
            return Err(CredentialError::Corrupt);
        }
        self.decrypt(&record)
    }

    /// Permanently removes an encrypted credential owned by the exact requesting boundary.
    ///
    /// # Errors
    ///
    /// Returns an error when the reference is missing, belongs to another owner, is corrupt, or
    /// cannot be durably removed.
    pub fn delete(
        &self,
        reference: &CredentialRef,
        requester: &CredentialOwner,
    ) -> Result<(), CredentialError> {
        reference.validate()?;
        if &reference.owner != requester {
            return Err(CredentialError::ScopeDenied);
        }
        let _guard = self.lock()?;
        let filename = reference.filename()?;
        let metadata = match self.directory.symlink_metadata(&filename) {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                return Err(CredentialError::NotFound);
            }
            Err(error) => return Err(error.into()),
        };
        if metadata.file_type().is_symlink() || !metadata.is_file() {
            return Err(CredentialError::Corrupt);
        }
        let record = self.read_record(Path::new(&filename))?;
        if &record.metadata.reference != reference {
            return Err(CredentialError::Corrupt);
        }
        self.directory.remove_file(&filename)?;
        std::fs::File::open(&self.ambient_root)?.sync_all()?;
        Ok(())
    }

    /// # Errors
    ///
    /// Returns an error when an encrypted record cannot be inspected.
    pub fn inspect(&self) -> Result<Vec<CredentialInspection>, CredentialError> {
        let _guard = self.lock()?;
        let mut inspections = Vec::new();
        for entry in self.directory.entries()? {
            let entry = entry?;
            let filename = PathBuf::from(entry.file_name());
            if entry.file_type()?.is_symlink()
                || filename.extension().and_then(|value| value.to_str()) != Some("cred")
            {
                continue;
            }
            let record = self.read_record(&filename)?;
            if record.metadata.reference.filename()? != filename.to_string_lossy() {
                return Err(CredentialError::Corrupt);
            }
            inspections.push(CredentialInspection::from(&record.metadata));
        }
        inspections.sort_by(|left, right| left.owner_kind.cmp(&right.owner_kind));
        Ok(inspections)
    }

    /// # Errors
    ///
    /// Returns an error when the browser gate fails or encrypted persistence fails.
    #[allow(clippy::too_many_arguments)]
    pub fn configure_from_browser(
        &self,
        policy: &BrowserWritePolicy,
        authenticated: bool,
        origin: &str,
        csrf: &[u8],
        reference: CredentialRef,
        secret: SecretValue,
        content_length: usize,
        timestamp: UtcTimestamp,
    ) -> Result<CredentialInspection, CredentialError> {
        if !authenticated {
            return Err(CredentialError::Authentication);
        }
        if origin != policy.exact_origin {
            return Err(CredentialError::Origin);
        }
        if !constant_time_equal(csrf, &policy.csrf.bytes) {
            return Err(CredentialError::Csrf);
        }
        if content_length > policy.max_payload_bytes
            || content_length < reference.name.len().saturating_add(secret.bytes.len())
        {
            return Err(CredentialError::PayloadLimit);
        }
        let metadata = self.put(reference, secret, timestamp)?;
        Ok(CredentialInspection::from(&metadata))
    }

    fn decrypt(&self, record: &EncryptedRecord) -> Result<SecretValue, CredentialError> {
        if record.version.major != CURRENT_SCHEMA_VERSION.major {
            return Err(CredentialError::Corrupt);
        }
        let mut ciphertext = hex_decode(&record.ciphertext)?;
        let key = LessSafeKey::new(
            UnboundKey::new(&AES_256_GCM, &self.key.bytes).map_err(|_| CredentialError::Crypto)?,
        );
        let plaintext = key
            .open_in_place(
                Nonce::assume_unique_for_key(record.nonce),
                Aad::from(metadata_aad(&record.metadata)?),
                &mut ciphertext,
            )
            .map_err(|_| CredentialError::Corrupt)?;
        let result = SecretValue::new(plaintext.to_vec())?;
        ciphertext.fill(0);
        Ok(result)
    }

    fn atomic_write(&self, filename: &str, bytes: &[u8]) -> Result<(), CredentialError> {
        let temporary = format!(
            ".{}.{}.tmp",
            hex_digest(filename.as_bytes()),
            EntityId::new()
        );
        let mut options = OpenOptions::new();
        options
            .write(true)
            .create_new(true)
            .follow(FollowSymlinks::No);
        configure_file_mode(&mut options);
        let result = (|| {
            let mut file = self.directory.open_with(&temporary, &options)?;
            file.write_all(bytes)?;
            file.sync_all()?;
            keith_platform::replace_file(
                &self.ambient_root.join(&temporary),
                &self.ambient_root.join(filename),
            )?;
            std::fs::File::open(&self.ambient_root)?.sync_all()?;
            Ok(())
        })();
        if result.is_err() {
            let _ = self.directory.remove_file(&temporary);
        }
        result
    }

    fn read_record_if_present(
        &self,
        filename: &str,
    ) -> Result<Option<EncryptedRecord>, CredentialError> {
        match self.directory.symlink_metadata(filename) {
            Ok(metadata) if metadata.file_type().is_symlink() => Err(CredentialError::Corrupt),
            Ok(_) => self.read_record(Path::new(filename)).map(Some),
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
            Err(error) => Err(error.into()),
        }
    }

    fn read_record(&self, filename: &Path) -> Result<EncryptedRecord, CredentialError> {
        let mut options = OpenOptions::new();
        options.read(true).follow(FollowSymlinks::No);
        let mut file = self.directory.open_with(filename, &options)?;
        if file.metadata()?.len() > 4 * MAX_SECRET_BYTES as u64 {
            return Err(CredentialError::Corrupt);
        }
        let mut bytes = Vec::new();
        file.read_to_end(&mut bytes)?;
        serde_json::from_slice(&bytes).map_err(|_| CredentialError::Corrupt)
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, CredentialError> {
        self.lock.lock().map_err(|_| CredentialError::LockPoisoned)
    }
}

pub struct CsrfToken {
    bytes: Vec<u8>,
}

impl CsrfToken {
    /// # Errors
    ///
    /// Returns an error for an empty or oversized CSRF token.
    pub fn new(bytes: impl Into<Vec<u8>>) -> Result<Self, CredentialError> {
        let bytes = bytes.into();
        if bytes.is_empty() || bytes.len() > 1_024 {
            return Err(CredentialError::Csrf);
        }
        Ok(Self { bytes })
    }
}

impl Debug for CsrfToken {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("CsrfToken([REDACTED])")
    }
}

impl Drop for CsrfToken {
    fn drop(&mut self) {
        self.bytes.fill(0);
    }
}

#[derive(Debug)]
pub struct BrowserWritePolicy {
    pub exact_origin: String,
    pub csrf: CsrfToken,
    pub max_payload_bytes: usize,
}

pub struct ProviderCredentialResolver<'a> {
    store: &'a EncryptedCredentialStore,
}

impl<'a> ProviderCredentialResolver<'a> {
    pub const fn new(store: &'a EncryptedCredentialStore) -> Self {
        Self { store }
    }
}

impl CredentialResolver for ProviderCredentialResolver<'_> {
    fn resolve(
        &self,
        provider: &str,
        credential_ref: Option<&str>,
    ) -> Result<ProviderCredential, ProviderError> {
        let name = credential_ref.ok_or_else(|| {
            ProviderError::new(
                ProviderErrorKind::Authentication,
                "provider credential reference is missing",
            )
        })?;
        let owner = CredentialOwner::Provider(provider.to_owned());
        let reference = CredentialRef::new(name, owner.clone()).map_err(provider_auth_error)?;
        let secret = self
            .store
            .resolve(&reference, &owner)
            .map_err(provider_auth_error)?;
        secret.with_bytes(|bytes| ProviderCredential::new(bytes.to_vec()))
    }
}

impl Debug for ProviderCredentialResolver<'_> {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("ProviderCredentialResolver([REDACTED])")
    }
}

pub struct SecretFilter {
    patterns: Vec<Vec<u8>>,
}

impl SecretFilter {
    pub fn new(secrets: &[&SecretValue]) -> Self {
        Self {
            patterns: secrets.iter().map(|secret| secret.bytes.clone()).collect(),
        }
    }

    pub fn contains_secret(&self, bytes: &[u8]) -> bool {
        self.patterns
            .iter()
            .any(|pattern| !pattern.is_empty() && contains_bytes(bytes, pattern))
    }

    pub fn redact_text(&self, text: &str) -> String {
        let mut redacted = text.to_owned();
        for pattern in &self.patterns {
            if let Ok(secret) = std::str::from_utf8(pattern) {
                redacted = redacted.replace(secret, "[REDACTED]");
            }
        }
        redacted
    }

    /// # Errors
    ///
    /// Returns an error naming only the surface category when a seeded secret is present.
    pub fn scan_surfaces<'a>(
        &self,
        surfaces: impl IntoIterator<Item = (&'a str, &'a [u8])>,
    ) -> Result<(), SecretLeak> {
        for (surface, bytes) in surfaces {
            if self.contains_secret(bytes) {
                return Err(SecretLeak {
                    surface: surface.to_owned(),
                });
            }
        }
        Ok(())
    }
}

impl Debug for SecretFilter {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("SecretFilter([REDACTED])")
    }
}

impl Drop for SecretFilter {
    fn drop(&mut self) {
        for pattern in &mut self.patterns {
            pattern.fill(0);
        }
    }
}

#[derive(Clone, Debug, Eq, Error, PartialEq)]
#[error("secret detected in {surface}")]
pub struct SecretLeak {
    pub surface: String,
}

fn provider_auth_error(_error: impl std::fmt::Display) -> ProviderError {
    ProviderError::new(
        ProviderErrorKind::Authentication,
        "provider credential could not be resolved",
    )
}

fn metadata_aad(metadata: &CredentialMetadata) -> Result<Vec<u8>, CredentialError> {
    serde_json::to_vec(metadata).map_err(CredentialError::Serialize)
}

fn constant_time_equal(left: &[u8], right: &[u8]) -> bool {
    let mut difference = left.len() ^ right.len();
    let length = left.len().max(right.len());
    for index in 0..length {
        let left = left.get(index).copied().unwrap_or(0);
        let right = right.get(index).copied().unwrap_or(0);
        difference |= usize::from(left ^ right);
    }
    difference == 0
}

fn valid_identifier(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 128
        && value.chars().all(|character| {
            character.is_ascii_alphanumeric() || matches!(character, '-' | '_' | '.')
        })
}

fn contains_bytes(haystack: &[u8], needle: &[u8]) -> bool {
    haystack
        .windows(needle.len())
        .any(|candidate| candidate == needle)
}

fn hex_digest(bytes: &[u8]) -> String {
    let digest = Sha256::digest(bytes);
    hex_encode(&digest)
}

fn hex_encode(bytes: &[u8]) -> String {
    let mut encoded = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        write!(&mut encoded, "{byte:02x}").expect("writing to a string cannot fail");
    }
    encoded
}

fn hex_decode(value: &str) -> Result<Vec<u8>, CredentialError> {
    if !value.len().is_multiple_of(2) {
        return Err(CredentialError::Corrupt);
    }
    value
        .as_bytes()
        .chunks_exact(2)
        .map(|pair| {
            let pair = std::str::from_utf8(pair).map_err(|_| CredentialError::Corrupt)?;
            u8::from_str_radix(pair, 16).map_err(|_| CredentialError::Corrupt)
        })
        .collect()
}

#[cfg(unix)]
fn restrict_directory(path: &Path) -> Result<(), CredentialError> {
    use std::os::unix::fs::PermissionsExt;

    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o700))?;
    Ok(())
}

#[cfg(not(unix))]
fn restrict_directory(_path: &Path) -> Result<(), CredentialError> {
    Ok(())
}

#[cfg(unix)]
fn restrict_key_file(path: &Path) -> Result<(), CredentialError> {
    use std::os::unix::fs::PermissionsExt;

    std::fs::set_permissions(path, std::fs::Permissions::from_mode(0o600))?;
    Ok(())
}

#[cfg(not(unix))]
fn restrict_key_file(_path: &Path) -> Result<(), CredentialError> {
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
    #[cfg(target_os = "linux")]
    use std::collections::{BTreeMap, BTreeSet};

    #[cfg(target_os = "linux")]
    use keith_tool_runner_core::{
        IsolationRequest, OutputChunk, ProcessLimits, RestrictedProcessRunner, RunRequest,
    };

    use super::*;

    const SEEDED_SECRET: &str = "KEITH-SEEDED-SECRET-7a31f4";

    fn store() -> (tempfile::TempDir, EncryptedCredentialStore) {
        let directory = tempfile::tempdir().unwrap();
        let store = EncryptedCredentialStore::open(
            directory.path(),
            MasterKey::from_bytes([0x5a; KEY_BYTES]),
        )
        .unwrap();
        (directory, store)
    }

    #[test]
    fn native_master_key_store_identifies_the_host_backend_without_exposing_a_key() {
        let store = NativeMasterKeyStore::new("keith-agent", "test-master-key").unwrap();
        assert!(!format!("{store:?}").contains("master-key"));
        assert_ne!(NativeMasterKeyStore::backend_name(), "unsupported");
        assert!(NativeMasterKeyStore::new("bad service", "account").is_err());
    }

    #[test]
    fn encrypted_store_scopes_provider_channel_mcp_and_tool_resolution() {
        let (directory, store) = store();
        let owners = [
            CredentialOwner::Provider("openai".into()),
            CredentialOwner::Channel("slack".into()),
            CredentialOwner::Mcp("documents".into()),
            CredentialOwner::Tool("deploy".into()),
        ];
        for (index, owner) in owners.iter().enumerate() {
            let reference =
                CredentialRef::new(format!("credential-{index}"), owner.clone()).unwrap();
            store
                .put(
                    reference.clone(),
                    SecretValue::new(format!("secret-value-{index}")).unwrap(),
                    UtcTimestamp::from_unix_millis(i64::try_from(index).unwrap()),
                )
                .unwrap();
            let resolved = store.resolve(&reference, owner).unwrap();
            resolved.with_bytes(|bytes| {
                assert_eq!(bytes, format!("secret-value-{index}").as_bytes());
            });
            let wrong = CredentialOwner::Tool("unrelated".into());
            assert!(matches!(
                store.resolve(&reference, &wrong),
                Err(CredentialError::ScopeDenied)
            ));
        }
        let inspections = store.inspect().unwrap();
        assert_eq!(inspections.len(), owners.len());
        assert!(
            inspections
                .iter()
                .all(|inspection| inspection.reference == "<redacted-reference>")
        );
        let raw = std::fs::read_dir(directory.path())
            .unwrap()
            .flat_map(|entry| std::fs::read(entry.unwrap().path()).unwrap())
            .collect::<Vec<_>>();
        for index in 0..owners.len() {
            assert!(!contains_bytes(
                &raw,
                format!("secret-value-{index}").as_bytes()
            ));
        }
    }

    #[test]
    fn profile_service_credentials_are_exactly_scoped_and_durably_deleted() {
        let (directory, store) = store();
        let owner = CredentialOwner::ProfileService {
            profile_id: ProfileId::new(),
            service: CredentialService::ConnectedApp,
            resource: "github-primary".into(),
        };
        let other_profile = CredentialOwner::ProfileService {
            profile_id: ProfileId::new(),
            service: CredentialService::ConnectedApp,
            resource: "github-primary".into(),
        };
        let reference = CredentialRef::new("oauth-access", owner.clone()).unwrap();
        store
            .put(
                reference.clone(),
                SecretValue::new(SEEDED_SECRET).unwrap(),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();

        assert!(matches!(
            store.resolve(&reference, &other_profile),
            Err(CredentialError::ScopeDenied)
        ));
        assert!(matches!(
            store.delete(&reference, &other_profile),
            Err(CredentialError::ScopeDenied)
        ));
        store.delete(&reference, &owner).unwrap();
        assert!(matches!(
            store.resolve(&reference, &owner),
            Err(CredentialError::NotFound)
        ));
        assert_eq!(std::fs::read_dir(directory.path()).unwrap().count(), 0);
    }

    #[test]
    fn provider_resolver_delivers_only_the_named_provider_secret() {
        let (_directory, store) = store();
        let owner = CredentialOwner::Provider("openai".into());
        store
            .put(
                CredentialRef::new("personal", owner).unwrap(),
                SecretValue::new(SEEDED_SECRET).unwrap(),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        store
            .put(
                CredentialRef::new("other", CredentialOwner::Provider("anthropic".into())).unwrap(),
                SecretValue::new("unrelated-secret").unwrap(),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let resolver = ProviderCredentialResolver::new(&store);
        let provider_credential = resolver.resolve("openai", Some("personal")).unwrap();
        assert_eq!(provider_credential.expose_utf8().unwrap(), SEEDED_SECRET);
        let error = resolver.resolve("openai", Some("other")).unwrap_err();
        assert_eq!(error.kind, ProviderErrorKind::Authentication);
        assert!(!format!("{error:?} {error}").contains(SEEDED_SECRET));
        assert!(!format!("{resolver:?}").contains(SEEDED_SECRET));
    }

    #[test]
    fn browser_configuration_is_authenticated_exact_origin_csrf_bounded_and_write_only() {
        let (_directory, store) = store();
        let policy = BrowserWritePolicy {
            exact_origin: "https://127.0.0.1:7443".into(),
            csrf: CsrfToken::new("csrf-value").unwrap(),
            max_payload_bytes: 1_024,
        };
        let reference = || {
            CredentialRef::new(
                "browser-provider",
                CredentialOwner::Provider("openai".into()),
            )
            .unwrap()
        };
        assert!(matches!(
            store.configure_from_browser(
                &policy,
                false,
                &policy.exact_origin,
                b"csrf-value",
                reference(),
                SecretValue::new(SEEDED_SECRET).unwrap(),
                100,
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(CredentialError::Authentication)
        ));
        assert!(matches!(
            store.configure_from_browser(
                &policy,
                true,
                "https://evil.invalid",
                b"csrf-value",
                reference(),
                SecretValue::new(SEEDED_SECRET).unwrap(),
                100,
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(CredentialError::Origin)
        ));
        assert!(matches!(
            store.configure_from_browser(
                &policy,
                true,
                &policy.exact_origin,
                b"wrong",
                reference(),
                SecretValue::new(SEEDED_SECRET).unwrap(),
                100,
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(CredentialError::Csrf)
        ));
        assert!(matches!(
            store.configure_from_browser(
                &policy,
                true,
                &policy.exact_origin,
                b"csrf-value",
                reference(),
                SecretValue::new(SEEDED_SECRET).unwrap(),
                2_000,
                UtcTimestamp::UNIX_EPOCH
            ),
            Err(CredentialError::PayloadLimit)
        ));
        let response = store
            .configure_from_browser(
                &policy,
                true,
                &policy.exact_origin,
                b"csrf-value",
                reference(),
                SecretValue::new(SEEDED_SECRET).unwrap(),
                100,
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let serialized = serde_json::to_string(&response).unwrap();
        assert!(!serialized.contains(SEEDED_SECRET));
        assert!(!serialized.contains("browser-provider"));
        assert_eq!(response.reference, "<redacted-reference>");
    }

    #[test]
    fn authenticated_metadata_detects_record_tampering() {
        let (directory, store) = store();
        let reference =
            CredentialRef::new("tamper-test", CredentialOwner::Tool("database".into())).unwrap();
        store
            .put(
                reference.clone(),
                SecretValue::new(SEEDED_SECRET).unwrap(),
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        let path = std::fs::read_dir(directory.path())
            .unwrap()
            .next()
            .unwrap()
            .unwrap()
            .path();
        let mut record: serde_json::Value =
            serde_json::from_slice(&std::fs::read(&path).unwrap()).unwrap();
        record["metadata"]["updated_at"] = serde_json::json!(99);
        std::fs::write(&path, serde_json::to_vec(&record).unwrap()).unwrap();
        assert!(matches!(
            store.resolve(&reference, &CredentialOwner::Tool("database".into())),
            Err(CredentialError::Corrupt)
        ));
    }

    #[cfg(unix)]
    #[test]
    fn restricted_backend_permissions_exclude_group_and_other_users() {
        use std::os::unix::fs::PermissionsExt;

        let (directory, store) = store();
        store
            .put(
                CredentialRef::new("permissions", CredentialOwner::Tool("test".into())).unwrap(),
                SecretValue::new(SEEDED_SECRET).unwrap(),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        assert_eq!(
            std::fs::metadata(directory.path())
                .unwrap()
                .permissions()
                .mode()
                & 0o777,
            0o700
        );
        let file = std::fs::read_dir(directory.path())
            .unwrap()
            .next()
            .unwrap()
            .unwrap()
            .path();
        assert_eq!(
            std::fs::metadata(file).unwrap().permissions().mode() & 0o777,
            0o600
        );
    }

    #[cfg(target_os = "linux")]
    #[test]
    fn seeded_leak_suite_scans_persistence_browser_process_export_event_log_and_diagnostics() {
        let (directory, store) = store();
        let reference =
            CredentialRef::new("leak-suite", CredentialOwner::Provider("openai".into())).unwrap();
        store
            .put(
                reference.clone(),
                SecretValue::new(SEEDED_SECRET).unwrap(),
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        let resolved = store
            .resolve(&reference, &CredentialOwner::Provider("openai".into()))
            .unwrap();
        let filter = SecretFilter::new(&[&resolved]);
        assert_eq!(
            filter.redact_text(&format!("log value={SEEDED_SECRET}")),
            "log value=[REDACTED]"
        );
        let persistence = std::fs::read_dir(directory.path())
            .unwrap()
            .flat_map(|entry| std::fs::read(entry.unwrap().path()).unwrap())
            .collect::<Vec<_>>();
        let inspection = serde_json::to_vec(&store.inspect().unwrap()).unwrap();
        let diagnostic = format!("{store:?} {:?}", CredentialError::ScopeDenied);
        let browser = serde_json::to_vec(&CredentialInspection {
            owner_kind: "provider".into(),
            reference: "<redacted-reference>".into(),
            configured: true,
            updated_at: UtcTimestamp::UNIX_EPOCH,
        })
        .unwrap();
        let workspace = tempfile::tempdir().unwrap();
        let runner = RestrictedProcessRunner::new(
            workspace.path(),
            vec![PathBuf::from("/usr/bin/env")],
            BTreeSet::new(),
            BTreeMap::new(),
        )
        .unwrap();
        let process = runner
            .run(
                &RunRequest {
                    program: PathBuf::from("/usr/bin/env"),
                    arguments: Vec::new(),
                    working_directory: PathBuf::from("."),
                    environment: BTreeMap::new(),
                    isolation: IsolationRequest::TrustedWorkspace,
                    limits: ProcessLimits::default(),
                },
                &keith_provider_core::CancellationToken::default(),
                &mut |_chunk: &OutputChunk| {},
            )
            .unwrap();
        let log = filter.redact_text(&format!("event={SEEDED_SECRET}"));
        filter
            .scan_surfaces([
                ("persistence", persistence.as_slice()),
                ("browser", browser.as_slice()),
                ("process", process.stdout.as_slice()),
                ("export", inspection.as_slice()),
                ("event_log", log.as_bytes()),
                ("diagnostic", diagnostic.as_bytes()),
            ])
            .unwrap();
        assert!(matches!(
            filter.scan_surfaces([("unfiltered_test", SEEDED_SECRET.as_bytes())]),
            Err(SecretLeak { surface }) if surface == "unfiltered_test"
        ));
    }
}
