#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fs;
use std::path::{Component, Path, PathBuf};
use std::process::Command;

pub use keith_build_info::BuildReport;
use ring::signature::{ED25519, UnparsedPublicKey};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

pub const MANIFEST_FILE: &str = "release-manifest.json";
pub const SIGNATURE_FILE: &str = "release-manifest.sig";
pub const PUBLIC_KEY_FILE: &str = "release-public-key.hex";
pub const MANIFEST_FORMAT: &str = "keith-release-manifest-v1";
pub const PACKAGE_NAME: &str = "keith-agent";

#[derive(Clone, Debug, Eq, PartialEq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ReleaseManifest {
    pub format: String,
    pub package: String,
    pub version: String,
    pub target: String,
    pub build_id: String,
    pub protocol_version: String,
    pub storage_schema: String,
    pub components: BTreeMap<String, BuildReport>,
    pub files: Vec<ReleaseFile>,
}

#[derive(Clone, Debug, Eq, PartialEq, Deserialize, Serialize)]
#[serde(deny_unknown_fields)]
pub struct ReleaseFile {
    pub path: String,
    pub bytes: u64,
    pub sha256: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize)]
pub struct VerifiedRelease {
    pub manifest: ReleaseManifest,
    pub manifest_sha256: String,
    pub public_key_hex: String,
}

#[derive(Debug, Error)]
pub enum ReleaseError {
    #[error("release I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("release manifest is invalid: {0}")]
    Manifest(#[from] serde_json::Error),
    #[error("release public key is invalid")]
    InvalidPublicKey,
    #[error("release public key does not match the trusted key")]
    UntrustedPublicKey,
    #[error("release signature is invalid")]
    InvalidSignature,
    #[error("release identity is invalid")]
    InvalidIdentity,
    #[error("release component report is missing or inconsistent: {0}")]
    InvalidComponent(String),
    #[error("release manifest path is unsafe or non-canonical: {0}")]
    UnsafePath(String),
    #[error("release manifest paths are not strictly sorted and unique")]
    UnorderedPaths,
    #[error("release payload does not exactly match the signed manifest")]
    PayloadMismatch,
    #[error("release file digest does not match: {0}")]
    DigestMismatch(String),
    #[error("release contains a symlink or unsupported filesystem entry: {0}")]
    UnsupportedEntry(PathBuf),
    #[error("release entry is writable by another user: {0}")]
    UnsafePermissions(PathBuf),
    #[error("release executable could not be inspected: {0}")]
    ExecutableInspection(String),
    #[error("release executable build report does not match the signed manifest: {0}")]
    BuildReportMismatch(String),
}

/// Decodes the trusted public key distributed through an independent channel.
///
/// # Errors
///
/// Returns an error unless the value is exactly 32 bytes encoded as hexadecimal.
pub fn decode_public_key(encoded: &str) -> Result<[u8; 32], ReleaseError> {
    decode_hex_exact::<32>(encoded.trim()).map_err(|()| ReleaseError::InvalidPublicKey)
}

/// Verifies a detached Ed25519 signature with an independently trusted public key.
///
/// # Errors
///
/// Returns [`ReleaseError::InvalidSignature`] when the signature is malformed or invalid.
pub fn verify_detached_signature(
    message: &[u8],
    signature: &[u8],
    expected_public_key: &[u8; 32],
) -> Result<(), ReleaseError> {
    let signature: [u8; 64] = signature
        .try_into()
        .map_err(|_| ReleaseError::InvalidSignature)?;
    UnparsedPublicKey::new(&ED25519, expected_public_key)
        .verify(message, &signature)
        .map_err(|_| ReleaseError::InvalidSignature)
}

/// Verifies release identity, publisher key, signature, component compatibility, and every file.
///
/// The release's own public-key file is never treated as a trust root. The caller must supply the
/// expected key from an independent trusted channel.
///
/// # Errors
///
/// Returns an error for any trust, manifest, path, filesystem, size, or digest mismatch.
pub fn verify_release(
    root: &Path,
    expected_public_key: &[u8; 32],
) -> Result<VerifiedRelease, ReleaseError> {
    reject_unsupported(root)?;
    let manifest_bytes = fs::read(root.join(MANIFEST_FILE))?;
    let packaged_public_key =
        decode_hex_exact::<32>(&fs::read_to_string(root.join(PUBLIC_KEY_FILE))?)
            .map_err(|()| ReleaseError::InvalidPublicKey)?;
    if &packaged_public_key != expected_public_key {
        return Err(ReleaseError::UntrustedPublicKey);
    }
    let signature = decode_hex_exact::<64>(&fs::read_to_string(root.join(SIGNATURE_FILE))?)
        .map_err(|()| ReleaseError::InvalidSignature)?;
    verify_detached_signature(&manifest_bytes, &signature, &packaged_public_key)?;

    let manifest: ReleaseManifest = serde_json::from_slice(&manifest_bytes)?;
    validate_manifest(&manifest)?;
    verify_payload(root, &manifest.files)?;
    Ok(VerifiedRelease {
        manifest,
        manifest_sha256: hex_encode(&Sha256::digest(&manifest_bytes)),
        public_key_hex: hex_encode(expected_public_key),
    })
}

/// Executes the already-authenticated daemon and worker report modes and compares their complete
/// compatibility reports with the signed manifest.
///
/// Call this only after [`verify_release`] has authenticated the exact payload and after confirming
/// that the manifest target matches the current host.
///
/// # Errors
///
/// Returns an error when either executable cannot run, emits invalid JSON, exits unsuccessfully,
/// or disagrees with the signed report.
pub fn verify_packaged_build_reports(
    root: &Path,
    manifest: &ReleaseManifest,
) -> Result<(), ReleaseError> {
    for (component, binary) in [("daemon", "agentd"), ("worker", "agent-worker")] {
        let expected = manifest
            .components
            .get(component)
            .ok_or_else(|| ReleaseError::InvalidComponent(component.into()))?;
        let filename = format!("{binary}{}", std::env::consts::EXE_SUFFIX);
        let output = Command::new(root.join("bin").join(filename))
            .arg("--build-info")
            .output()
            .map_err(|_| ReleaseError::ExecutableInspection(binary.into()))?;
        if !output.status.success() {
            return Err(ReleaseError::ExecutableInspection(binary.into()));
        }
        let actual: BuildReport = serde_json::from_slice(&output.stdout)
            .map_err(|_| ReleaseError::ExecutableInspection(binary.into()))?;
        if &actual != expected {
            return Err(ReleaseError::BuildReportMismatch(component.into()));
        }
    }
    Ok(())
}

fn validate_manifest(manifest: &ReleaseManifest) -> Result<(), ReleaseError> {
    if manifest.format != MANIFEST_FORMAT
        || manifest.package != PACKAGE_NAME
        || manifest.version.is_empty()
        || manifest.target.is_empty()
        || manifest.build_id.is_empty()
        || manifest.protocol_version.is_empty()
        || manifest.storage_schema.is_empty()
    {
        return Err(ReleaseError::InvalidIdentity);
    }
    for component in ["daemon", "worker"] {
        let report = manifest
            .components
            .get(component)
            .ok_or_else(|| ReleaseError::InvalidComponent(component.into()))?;
        if report.component != component
            || report.package_version != manifest.version
            || report.build_id != manifest.build_id
            || report.protocol_version != manifest.protocol_version
            || report.storage_schema != manifest.storage_schema
            || report.enabled_features.is_empty()
        {
            return Err(ReleaseError::InvalidComponent(component.into()));
        }
    }
    Ok(())
}

fn verify_payload(root: &Path, files: &[ReleaseFile]) -> Result<(), ReleaseError> {
    let mut expected = BTreeSet::new();
    let mut previous: Option<&str> = None;
    for file in files {
        if previous.is_some_and(|path| path >= file.path.as_str()) {
            return Err(ReleaseError::UnorderedPaths);
        }
        previous = Some(&file.path);
        let relative = canonical_relative_path(&file.path)?;
        if !expected.insert(file.path.clone()) {
            return Err(ReleaseError::UnorderedPaths);
        }
        let path = root.join(relative);
        reject_unsupported(&path)?;
        let bytes = fs::read(&path)?;
        if u64::try_from(bytes.len()).ok() != Some(file.bytes)
            || hex_encode(&Sha256::digest(&bytes)) != file.sha256
        {
            return Err(ReleaseError::DigestMismatch(file.path.clone()));
        }
    }

    let mut actual_paths = Vec::new();
    collect_payload_files(root, root, &mut actual_paths)?;
    let actual = actual_paths
        .into_iter()
        .filter(|path| !is_control_file(path))
        .map(|path| normalized_relative(&path))
        .collect::<Result<BTreeSet<_>, _>>()?;
    if actual != expected {
        return Err(ReleaseError::PayloadMismatch);
    }
    Ok(())
}

fn collect_payload_files(
    root: &Path,
    directory: &Path,
    output: &mut Vec<PathBuf>,
) -> Result<(), ReleaseError> {
    reject_unsupported(directory)?;
    for entry in fs::read_dir(directory)? {
        let entry = entry?;
        let path = entry.path();
        let file_type = entry.file_type()?;
        if file_type.is_symlink() {
            return Err(ReleaseError::UnsupportedEntry(path));
        }
        if file_type.is_dir() {
            collect_payload_files(root, &path, output)?;
        } else if file_type.is_file() {
            output.push(
                path.strip_prefix(root)
                    .map_err(|_| ReleaseError::UnsafePath(path.display().to_string()))?
                    .to_path_buf(),
            );
        } else {
            return Err(ReleaseError::UnsupportedEntry(path));
        }
    }
    Ok(())
}

fn reject_unsupported(path: &Path) -> Result<(), ReleaseError> {
    let metadata = fs::symlink_metadata(path)?;
    if metadata.file_type().is_symlink() || (!metadata.is_file() && !metadata.is_dir()) {
        Err(ReleaseError::UnsupportedEntry(path.to_path_buf()))
    } else if is_group_or_world_writable(&metadata) {
        Err(ReleaseError::UnsafePermissions(path.to_path_buf()))
    } else {
        Ok(())
    }
}

#[cfg(unix)]
fn is_group_or_world_writable(metadata: &fs::Metadata) -> bool {
    use std::os::unix::fs::PermissionsExt as _;
    metadata.permissions().mode() & 0o022 != 0
}

#[cfg(not(unix))]
const fn is_group_or_world_writable(_metadata: &fs::Metadata) -> bool {
    false
}

fn canonical_relative_path(value: &str) -> Result<PathBuf, ReleaseError> {
    if value.is_empty() || value.contains('\\') {
        return Err(ReleaseError::UnsafePath(value.into()));
    }
    let path = Path::new(value);
    if path.is_absolute()
        || path
            .components()
            .any(|component| !matches!(component, Component::Normal(_)))
    {
        return Err(ReleaseError::UnsafePath(value.into()));
    }
    Ok(path.to_path_buf())
}

fn normalized_relative(path: &Path) -> Result<String, ReleaseError> {
    let mut parts = Vec::new();
    for component in path.components() {
        let Component::Normal(part) = component else {
            return Err(ReleaseError::UnsafePath(path.display().to_string()));
        };
        parts.push(
            part.to_str()
                .ok_or_else(|| ReleaseError::UnsafePath(path.display().to_string()))?,
        );
    }
    Ok(parts.join("/"))
}

fn is_control_file(path: &Path) -> bool {
    matches!(
        normalized_relative(path).as_deref(),
        Ok(MANIFEST_FILE | SIGNATURE_FILE | PUBLIC_KEY_FILE)
    )
}

fn decode_hex_exact<const N: usize>(encoded: &str) -> Result<[u8; N], ()> {
    let encoded = encoded.trim().as_bytes();
    if encoded.len() != N.saturating_mul(2) {
        return Err(());
    }
    let mut decoded = [0_u8; N];
    for (target, pair) in decoded.iter_mut().zip(encoded.chunks_exact(2)) {
        *target = (hex_digit(pair[0])? << 4) | hex_digit(pair[1])?;
    }
    Ok(decoded)
}

fn hex_digit(value: u8) -> Result<u8, ()> {
    match value {
        b'0'..=b'9' => Ok(value - b'0'),
        b'a'..=b'f' => Ok(value - b'a' + 10),
        b'A'..=b'F' => Ok(value - b'A' + 10),
        _ => Err(()),
    }
}

pub fn hex_encode(bytes: &[u8]) -> String {
    let mut encoded = String::with_capacity(bytes.len().saturating_mul(2));
    for byte in bytes {
        use std::fmt::Write as _;
        let _ = write!(encoded, "{byte:02x}");
    }
    encoded
}

#[cfg(test)]
mod tests {
    use super::*;
    use ring::signature::{Ed25519KeyPair, KeyPair};

    fn report(component: &str) -> BuildReport {
        BuildReport {
            component: component.into(),
            package_version: "1.2.3".into(),
            build_id: "release-test-build".into(),
            protocol_version: "1.0".into(),
            storage_schema: "1.0".into(),
            enabled_features: BTreeSet::from(["release-test".into()]),
        }
    }

    fn signed_release(root: &Path, seed: [u8; 32]) -> [u8; 32] {
        fs::create_dir_all(root.join("bin")).unwrap();
        fs::write(root.join("bin/agentd"), b"packaged daemon").unwrap();
        let payload = fs::read(root.join("bin/agentd")).unwrap();
        let manifest = ReleaseManifest {
            format: MANIFEST_FORMAT.into(),
            package: PACKAGE_NAME.into(),
            version: "1.2.3".into(),
            target: "test-target".into(),
            build_id: "release-test-build".into(),
            protocol_version: "1.0".into(),
            storage_schema: "1.0".into(),
            components: BTreeMap::from([
                ("daemon".into(), report("daemon")),
                ("worker".into(), report("worker")),
            ]),
            files: vec![ReleaseFile {
                path: "bin/agentd".into(),
                bytes: u64::try_from(payload.len()).unwrap(),
                sha256: hex_encode(&Sha256::digest(&payload)),
            }],
        };
        let manifest_bytes = serde_json::to_vec_pretty(&manifest).unwrap();
        let key = Ed25519KeyPair::from_seed_unchecked(&seed).unwrap();
        fs::write(root.join(MANIFEST_FILE), &manifest_bytes).unwrap();
        fs::write(
            root.join(SIGNATURE_FILE),
            hex_encode(key.sign(&manifest_bytes).as_ref()),
        )
        .unwrap();
        let public_key: [u8; 32] = key.public_key().as_ref().try_into().unwrap();
        fs::write(root.join(PUBLIC_KEY_FILE), hex_encode(&public_key)).unwrap();
        public_key
    }

    #[test]
    fn exact_signed_release_verifies_with_independent_key() {
        let directory = tempfile::tempdir().unwrap();
        let public_key = signed_release(directory.path(), [5_u8; 32]);
        let verified = verify_release(directory.path(), &public_key).unwrap();
        assert_eq!(verified.manifest.version, "1.2.3");
        assert_eq!(verified.manifest.files.len(), 1);
        assert!(!verified.manifest_sha256.is_empty());
    }

    #[test]
    fn wrong_key_unlisted_file_and_tampering_fail_closed() {
        let directory = tempfile::tempdir().unwrap();
        let public_key = signed_release(directory.path(), [6_u8; 32]);
        assert!(matches!(
            verify_release(directory.path(), &[7_u8; 32]),
            Err(ReleaseError::UntrustedPublicKey)
        ));

        fs::write(directory.path().join("unlisted"), b"not signed").unwrap();
        assert!(matches!(
            verify_release(directory.path(), &public_key),
            Err(ReleaseError::PayloadMismatch)
        ));
        fs::remove_file(directory.path().join("unlisted")).unwrap();

        fs::write(directory.path().join("bin/agentd"), b"tampered").unwrap();
        assert!(matches!(
            verify_release(directory.path(), &public_key),
            Err(ReleaseError::DigestMismatch(path)) if path == "bin/agentd"
        ));
    }
}
#[derive(
    Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, serde::Serialize, serde::Deserialize,
)]
#[serde(rename_all = "snake_case")]
pub enum TeammatesPackageComponent {
    Daemon,
    AgentWorker,
    ChannelGateway,
    BrowserRunner,
    WebClient,
    DesktopClient,
    TerminalClient,
    CliClient,
    ConversationSchema,
    CoordinationSchema,
    ComputerSchema,
    StateStoreSchema,
    Migration,
    ServiceDefinition,
    ContainerAsset,
    WebAsset,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesPackageArtifact {
    pub component: TeammatesPackageComponent,
    pub relative_path: String,
    pub sha256: String,
    pub size_bytes: u64,
    pub executable: bool,
    pub required: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PackageDependencyClass {
    LocalSystem,
    UserConfiguredProvider,
    OptionalExternalChannel,
    HostedControlPlane,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PackageRuntimeDependency {
    pub name: String,
    pub class: PackageDependencyClass,
    pub required: bool,
    pub version_requirement: Option<String>,
    pub endpoint_origin: Option<String>,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesPackageCompatibility {
    pub target_triples: BTreeSet<String>,
    pub minimum_schema_version: u32,
    pub maximum_schema_version: u32,
    pub native_protocol_major: u16,
    pub teammates_protocol_major: u16,
    pub package_format_version: u32,
    pub self_hosted: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesPackageInventory {
    pub release_version: String,
    pub build_id: String,
    pub source_revision: String,
    pub compatibility: TeammatesPackageCompatibility,
    pub artifacts: Vec<TeammatesPackageArtifact>,
    pub runtime_dependencies: Vec<PackageRuntimeDependency>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PackageSignatureAlgorithm {
    Ed25519,
}

#[derive(Clone, Debug, Eq, PartialEq, serde::Serialize, serde::Deserialize)]
#[serde(deny_unknown_fields)]
pub struct TeammatesPackageSignature {
    pub algorithm: PackageSignatureAlgorithm,
    pub key_id: String,
    pub public_key: Vec<u8>,
    pub signature: Vec<u8>,
    pub manifest_sha256: String,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SignedTeammatesPackage {
    pub manifest_bytes: Vec<u8>,
    pub signature: TeammatesPackageSignature,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct TeammatesHostCompatibility {
    pub target_triple: String,
    pub schema_version: u32,
    pub native_protocol_major: u16,
    pub teammates_protocol_major: u16,
    pub trusted_signing_key_ids: BTreeSet<String>,
}

pub trait PackageCryptography {
    fn sha256(&self, bytes: &[u8]) -> [u8; 32];

    fn verify_ed25519(
        &self,
        public_key: &[u8],
        message: &[u8],
        signature: &[u8],
    ) -> Result<bool, TeammatesPackageError>;
}

pub trait PackageManifestDecoder {
    fn decode_inventory(
        &self,
        manifest_bytes: &[u8],
    ) -> Result<TeammatesPackageInventory, TeammatesPackageError>;
}

pub trait PackageArtifactReader {
    fn read_artifact(&self, relative_path: &str) -> Result<Vec<u8>, TeammatesPackageError>;
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct VerifiedTeammatesPackage {
    pub inventory: TeammatesPackageInventory,
    pub manifest_sha256: String,
    pub signing_key_id: String,
    pub verified_artifacts: usize,
    pub verified_bytes: u64,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum TeammatesPackageError {
    Missing(&'static str),
    InvalidPath(String),
    InvalidDigest(String),
    DuplicatePath(String),
    MissingComponent(TeammatesPackageComponent),
    UnsupportedTarget(String),
    UnsupportedSchema(u32),
    UnsupportedProtocol,
    HostedControlPlaneDependency(String),
    RequiredProviderDependency(String),
    UntrustedSigningKey(String),
    InvalidSignature,
    ArtifactMissing(String),
    ArtifactSizeMismatch(String),
    ArtifactDigestMismatch(String),
    Decode(String),
    Io(String),
    Crypto(String),
}

impl std::fmt::Display for TeammatesPackageError {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        write!(formatter, "{self:?}")
    }
}

impl std::error::Error for TeammatesPackageError {}

impl TeammatesPackageInventory {
    pub fn validate(&self) -> Result<(), TeammatesPackageError> {
        for (name, value) in [
            ("release_version", self.release_version.as_str()),
            ("build_id", self.build_id.as_str()),
            ("source_revision", self.source_revision.as_str()),
        ] {
            if value.trim().is_empty() {
                return Err(TeammatesPackageError::Missing(name));
            }
        }
        if !self.compatibility.self_hosted {
            return Err(TeammatesPackageError::HostedControlPlaneDependency(
                "package is not self-hosted".into(),
            ));
        }
        if self.compatibility.minimum_schema_version > self.compatibility.maximum_schema_version
            || self.compatibility.target_triples.is_empty()
        {
            return Err(TeammatesPackageError::UnsupportedProtocol);
        }
        let mut paths = BTreeSet::new();
        let mut components = BTreeSet::new();
        for artifact in &self.artifacts {
            validate_relative_path(&artifact.relative_path)?;
            validate_sha256(&artifact.sha256)?;
            if !paths.insert(artifact.relative_path.clone()) {
                return Err(TeammatesPackageError::DuplicatePath(
                    artifact.relative_path.clone(),
                ));
            }
            if artifact.required {
                components.insert(artifact.component);
            }
        }
        for required in [
            TeammatesPackageComponent::Daemon,
            TeammatesPackageComponent::AgentWorker,
            TeammatesPackageComponent::ChannelGateway,
            TeammatesPackageComponent::BrowserRunner,
            TeammatesPackageComponent::WebClient,
            TeammatesPackageComponent::DesktopClient,
            TeammatesPackageComponent::TerminalClient,
            TeammatesPackageComponent::CliClient,
            TeammatesPackageComponent::ConversationSchema,
            TeammatesPackageComponent::CoordinationSchema,
            TeammatesPackageComponent::ComputerSchema,
            TeammatesPackageComponent::StateStoreSchema,
            TeammatesPackageComponent::Migration,
            TeammatesPackageComponent::ServiceDefinition,
            TeammatesPackageComponent::WebAsset,
        ] {
            if !components.contains(&required) {
                return Err(TeammatesPackageError::MissingComponent(required));
            }
        }
        for dependency in &self.runtime_dependencies {
            if dependency.name.trim().is_empty() {
                return Err(TeammatesPackageError::Missing("runtime dependency name"));
            }
            match dependency.class {
                PackageDependencyClass::HostedControlPlane => {
                    return Err(TeammatesPackageError::HostedControlPlaneDependency(
                        dependency.name.clone(),
                    ));
                }
                PackageDependencyClass::UserConfiguredProvider if dependency.required => {
                    return Err(TeammatesPackageError::RequiredProviderDependency(
                        dependency.name.clone(),
                    ));
                }
                PackageDependencyClass::LocalSystem
                    if dependency
                        .endpoint_origin
                        .as_deref()
                        .is_some_and(|origin| !is_local_origin(origin)) =>
                {
                    return Err(TeammatesPackageError::HostedControlPlaneDependency(
                        dependency.name.clone(),
                    ));
                }
                PackageDependencyClass::LocalSystem
                | PackageDependencyClass::UserConfiguredProvider
                | PackageDependencyClass::OptionalExternalChannel => {}
            }
        }
        Ok(())
    }

    pub fn verify_host(
        &self,
        host: &TeammatesHostCompatibility,
    ) -> Result<(), TeammatesPackageError> {
        if !self
            .compatibility
            .target_triples
            .contains(&host.target_triple)
        {
            return Err(TeammatesPackageError::UnsupportedTarget(
                host.target_triple.clone(),
            ));
        }
        if host.schema_version < self.compatibility.minimum_schema_version
            || host.schema_version > self.compatibility.maximum_schema_version
        {
            return Err(TeammatesPackageError::UnsupportedSchema(
                host.schema_version,
            ));
        }
        if host.native_protocol_major != self.compatibility.native_protocol_major
            || host.teammates_protocol_major != self.compatibility.teammates_protocol_major
        {
            return Err(TeammatesPackageError::UnsupportedProtocol);
        }
        Ok(())
    }
}

pub fn verify_signed_teammates_package<C, D, R>(
    package: &SignedTeammatesPackage,
    host: &TeammatesHostCompatibility,
    cryptography: &C,
    decoder: &D,
    artifacts: &R,
) -> Result<VerifiedTeammatesPackage, TeammatesPackageError>
where
    C: PackageCryptography,
    D: PackageManifestDecoder,
    R: PackageArtifactReader,
{
    if package.signature.algorithm != PackageSignatureAlgorithm::Ed25519 {
        return Err(TeammatesPackageError::InvalidSignature);
    }
    if !host
        .trusted_signing_key_ids
        .contains(&package.signature.key_id)
    {
        return Err(TeammatesPackageError::UntrustedSigningKey(
            package.signature.key_id.clone(),
        ));
    }
    validate_sha256(&package.signature.manifest_sha256)?;
    let manifest_digest = hex_digest(cryptography.sha256(&package.manifest_bytes));
    if manifest_digest != package.signature.manifest_sha256 {
        return Err(TeammatesPackageError::InvalidDigest("manifest".into()));
    }
    if !cryptography.verify_ed25519(
        &package.signature.public_key,
        &package.manifest_bytes,
        &package.signature.signature,
    )? {
        return Err(TeammatesPackageError::InvalidSignature);
    }
    let inventory = decoder.decode_inventory(&package.manifest_bytes)?;
    inventory.validate()?;
    inventory.verify_host(host)?;
    let mut verified_bytes = 0_u64;
    for artifact in &inventory.artifacts {
        let bytes = artifacts
            .read_artifact(&artifact.relative_path)
            .map_err(|_| TeammatesPackageError::ArtifactMissing(artifact.relative_path.clone()))?;
        if u64::try_from(bytes.len()).ok() != Some(artifact.size_bytes) {
            return Err(TeammatesPackageError::ArtifactSizeMismatch(
                artifact.relative_path.clone(),
            ));
        }
        if hex_digest(cryptography.sha256(&bytes)) != artifact.sha256 {
            return Err(TeammatesPackageError::ArtifactDigestMismatch(
                artifact.relative_path.clone(),
            ));
        }
        verified_bytes = verified_bytes.checked_add(artifact.size_bytes).ok_or(
            TeammatesPackageError::ArtifactSizeMismatch(artifact.relative_path.clone()),
        )?;
    }
    Ok(VerifiedTeammatesPackage {
        verified_artifacts: inventory.artifacts.len(),
        inventory,
        manifest_sha256: manifest_digest,
        signing_key_id: package.signature.key_id.clone(),
        verified_bytes,
    })
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct InstalledTeammatesResource {
    pub component: TeammatesPackageComponent,
    pub relative_path: String,
    pub present: bool,
    pub expected_sha256: String,
    pub observed_sha256: Option<String>,
    pub expected_size_bytes: u64,
    pub observed_size_bytes: Option<u64>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct InstalledTeammatesResourceReport {
    pub release_version: String,
    pub build_id: String,
    pub manifest_sha256: String,
    pub resources: Vec<InstalledTeammatesResource>,
    pub missing_required: Vec<String>,
    pub digest_mismatches: Vec<String>,
}

pub fn report_installed_teammates_resources<C, R>(
    inventory: &TeammatesPackageInventory,
    manifest_sha256: &str,
    cryptography: &C,
    artifacts: &R,
) -> Result<InstalledTeammatesResourceReport, TeammatesPackageError>
where
    C: PackageCryptography,
    R: PackageArtifactReader,
{
    validate_sha256(manifest_sha256)?;
    let mut resources = Vec::with_capacity(inventory.artifacts.len());
    let mut missing_required = Vec::new();
    let mut digest_mismatches = Vec::new();
    for artifact in &inventory.artifacts {
        match artifacts.read_artifact(&artifact.relative_path) {
            Ok(bytes) => {
                let observed_sha256 = hex_digest(cryptography.sha256(&bytes));
                let observed_size_bytes = u64::try_from(bytes.len()).ok();
                if observed_sha256 != artifact.sha256
                    || observed_size_bytes != Some(artifact.size_bytes)
                {
                    digest_mismatches.push(artifact.relative_path.clone());
                }
                resources.push(InstalledTeammatesResource {
                    component: artifact.component,
                    relative_path: artifact.relative_path.clone(),
                    present: true,
                    expected_sha256: artifact.sha256.clone(),
                    observed_sha256: Some(observed_sha256),
                    expected_size_bytes: artifact.size_bytes,
                    observed_size_bytes,
                });
            }
            Err(_) => {
                if artifact.required {
                    missing_required.push(artifact.relative_path.clone());
                }
                resources.push(InstalledTeammatesResource {
                    component: artifact.component,
                    relative_path: artifact.relative_path.clone(),
                    present: false,
                    expected_sha256: artifact.sha256.clone(),
                    observed_sha256: None,
                    expected_size_bytes: artifact.size_bytes,
                    observed_size_bytes: None,
                });
            }
        }
    }
    Ok(InstalledTeammatesResourceReport {
        release_version: inventory.release_version.clone(),
        build_id: inventory.build_id.clone(),
        manifest_sha256: manifest_sha256.to_owned(),
        resources,
        missing_required,
        digest_mismatches,
    })
}

fn validate_relative_path(path: &str) -> Result<(), TeammatesPackageError> {
    let candidate = std::path::Path::new(path);
    if path.trim().is_empty()
        || candidate.is_absolute()
        || candidate.components().any(|component| {
            matches!(
                component,
                std::path::Component::ParentDir
                    | std::path::Component::RootDir
                    | std::path::Component::Prefix(_)
            )
        })
    {
        return Err(TeammatesPackageError::InvalidPath(path.to_owned()));
    }
    Ok(())
}

fn validate_sha256(value: &str) -> Result<(), TeammatesPackageError> {
    if value.len() != 64 || !value.bytes().all(|byte| byte.is_ascii_hexdigit()) {
        return Err(TeammatesPackageError::InvalidDigest(value.to_owned()));
    }
    Ok(())
}

fn hex_digest(bytes: [u8; 32]) -> String {
    let mut output = String::with_capacity(64);
    for byte in bytes {
        use std::fmt::Write as _;
        write!(&mut output, "{byte:02x}").expect("writing to String cannot fail");
    }
    output
}

fn is_local_origin(origin: &str) -> bool {
    let normalized = origin.to_ascii_lowercase();
    normalized.starts_with("unix:")
        || normalized.starts_with("file:")
        || normalized.starts_with("http://127.0.0.1")
        || normalized.starts_with("http://localhost")
        || normalized.starts_with("http://[::1]")
        || normalized.starts_with("https://127.0.0.1")
        || normalized.starts_with("https://localhost")
        || normalized.starts_with("https://[::1]")
}
