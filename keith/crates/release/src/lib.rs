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
    decode_hex_exact::<32>(encoded.trim()).map_err(|_| ReleaseError::InvalidPublicKey)
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
            .map_err(|_| ReleaseError::InvalidPublicKey)?;
    if &packaged_public_key != expected_public_key {
        return Err(ReleaseError::UntrustedPublicKey);
    }
    let signature = decode_hex_exact::<64>(&fs::read_to_string(root.join(SIGNATURE_FILE))?)
        .map_err(|_| ReleaseError::InvalidSignature)?;
    UnparsedPublicKey::new(&ED25519, packaged_public_key)
        .verify(&manifest_bytes, &signature)
        .map_err(|_| ReleaseError::InvalidSignature)?;

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
