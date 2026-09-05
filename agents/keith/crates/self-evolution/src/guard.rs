use std::ffi::OsStr;
use std::fs;
use std::path::{Component, Path, PathBuf};

use thiserror::Error;

/// The recovery and authority boundary is compiled into this crate. It cannot
/// be widened by configuration, reviewed content, or model output.
#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ProtectedSurface;

pub const PROTECTED_SURFACE: ProtectedSurface = ProtectedSurface;

impl ProtectedSurface {
    pub const PATHS: &'static [&'static str] = &[
        "crates/agent-loop",
        "crates/memory",
        "crates/sandbox",
        "crates/self-evolution/src/build.rs",
        "crates/self-evolution/src/lib.rs",
        "crates/self-evolution/src/budget.rs",
        "crates/self-evolution/src/guard.rs",
        "crates/self-evolution/src/ledger.rs",
        "crates/release",
        "crates/tool-runner",
        "apps/xtask",
    ];

    const ALWAYS_UNWRITABLE: &'static [&'static str] = &[
        ".cargo",
        ".git",
        ".keith",
        "Cargo.lock",
        "Cargo.toml",
        "backups",
        "crates/credentials",
        "crates/session-store",
        "resources",
        "rust-toolchain",
        "rust-toolchain.toml",
        "signing-keys",
    ];

    #[must_use]
    pub fn contains(relative: &Path) -> bool {
        Self::PATHS
            .iter()
            .chain(Self::ALWAYS_UNWRITABLE.iter())
            .any(|entry| path_is_within(relative, Path::new(entry)))
            || relative.file_name() == Some(OsStr::new("build.rs"))
    }
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd)]
pub enum ChangeClass {
    A,
    B,
    C,
    D,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum ConsentPolicy {
    Autonomous,
    WatchdogBounded,
    HumanApproval,
    Refused,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ConsentAuthority {
    InstallationOwner { identity: String },
    Client,
    Channel,
    Kernel,
    Plugin,
    Mcp,
    Skill,
    Model,
}

impl ChangeClass {
    #[must_use]
    pub const fn consent_policy(self) -> ConsentPolicy {
        match self {
            Self::A => ConsentPolicy::Autonomous,
            Self::B => ConsentPolicy::WatchdogBounded,
            Self::C => ConsentPolicy::HumanApproval,
            Self::D => ConsentPolicy::Refused,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ChangedPath {
    Write(PathBuf),
    Delete(PathBuf),
    Rename { from: PathBuf, to: PathBuf },
}

#[derive(Clone, Debug, Default, Eq, PartialEq)]
pub struct ArtifactManifest {
    pub source_paths: Vec<PathBuf>,
    pub public_protocol_changed: bool,
    pub persisted_schema_changed: bool,
    pub requires_daemon_replacement: bool,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct Classification {
    pub proposal: ChangeClass,
    pub artifact: ChangeClass,
    pub escalated: bool,
    pub consent: ConsentPolicy,
}

#[derive(Debug, Error)]
pub enum GuardError {
    #[error("workspace root is not a usable directory: {0}")]
    InvalidRoot(PathBuf),
    #[error("absolute paths are forbidden: {0}")]
    AbsolutePath(PathBuf),
    #[error("path traversal is forbidden: {0}")]
    Traversal(PathBuf),
    #[error("device paths are forbidden: {0}")]
    DevicePath(PathBuf),
    #[error("path escapes the workspace after resolution: {0}")]
    EscapesWorkspace(PathBuf),
    #[error("path cannot be resolved: {path}: {source}")]
    Resolution {
        path: PathBuf,
        #[source]
        source: std::io::Error,
    },
    #[error("proposal has no classifiable changed paths")]
    Unclassifiable,
    #[error("protected surface changes are refused")]
    ProtectedSurface,
    #[error("class C requires authenticated installation-owner approval")]
    HumanApprovalRequired,
}

#[derive(Clone, Debug)]
pub struct EvolutionGuard {
    root: PathBuf,
}

impl EvolutionGuard {
    /// Creates a guard rooted at an existing workspace directory.
    ///
    /// # Errors
    ///
    /// Returns [`GuardError::InvalidRoot`] if the root cannot be canonicalized
    /// or is not a directory.
    pub fn new(workspace_root: impl AsRef<Path>) -> Result<Self, GuardError> {
        let requested = workspace_root.as_ref();
        let root = fs::canonicalize(requested)
            .map_err(|_| GuardError::InvalidRoot(requested.to_path_buf()))?;
        if !root.is_dir() {
            return Err(GuardError::InvalidRoot(root));
        }
        Ok(Self { root })
    }

    #[must_use]
    pub fn protected_surface(&self) -> ProtectedSurface {
        PROTECTED_SURFACE
    }

    /// Resolves a proposal path against the canonical workspace root.
    ///
    /// # Errors
    ///
    /// Rejects absolute, traversal, device, unresolvable, and symlink-escaping
    /// paths.
    pub fn resolve(&self, requested: impl AsRef<Path>) -> Result<PathBuf, GuardError> {
        let requested = requested.as_ref();
        validate_lexical(requested)?;

        let candidate = self.root.join(requested);
        let mut existing = candidate.as_path();
        let mut suffix = Vec::new();
        while !existing.exists() {
            let name = existing
                .file_name()
                .ok_or_else(|| GuardError::EscapesWorkspace(requested.to_path_buf()))?;
            suffix.push(name.to_os_string());
            existing = existing
                .parent()
                .ok_or_else(|| GuardError::EscapesWorkspace(requested.to_path_buf()))?;
        }
        let canonical = fs::canonicalize(existing).map_err(|source| GuardError::Resolution {
            path: requested.to_path_buf(),
            source,
        })?;
        if !canonical.starts_with(&self.root) {
            return Err(GuardError::EscapesWorkspace(requested.to_path_buf()));
        }

        let mut resolved = canonical;
        for part in suffix.iter().rev() {
            resolved.push(part);
        }
        if !resolved.starts_with(&self.root) {
            return Err(GuardError::EscapesWorkspace(requested.to_path_buf()));
        }
        resolved
            .strip_prefix(&self.root)
            .map(Path::to_path_buf)
            .map_err(|_| GuardError::EscapesWorkspace(requested.to_path_buf()))
    }

    /// Classifies resolved proposal paths without admitting the proposal.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid paths or an empty/unclassifiable proposal.
    pub fn classify_proposal(&self, changes: &[ChangedPath]) -> Result<ChangeClass, GuardError> {
        if changes.is_empty() {
            return Err(GuardError::Unclassifiable);
        }
        let mut class = ChangeClass::A;
        for change in changes {
            match change {
                ChangedPath::Write(path) | ChangedPath::Delete(path) => {
                    class = class.max(Self::classify_resolved(&self.resolve(path)?)?);
                }
                ChangedPath::Rename { from, to } => {
                    class = class.max(Self::classify_resolved(&self.resolve(from)?)?);
                    class = class.max(Self::classify_resolved(&self.resolve(to)?)?);
                }
            }
        }
        Ok(class)
    }

    /// Admits a proposal only when its resolved paths avoid Class D.
    ///
    /// # Errors
    ///
    /// Returns [`GuardError::ProtectedSurface`] for Class D or any underlying
    /// path/classification error.
    pub fn admit_proposal(&self, changes: &[ChangedPath]) -> Result<ChangeClass, GuardError> {
        refuse_class_d(self.classify_proposal(changes)?)
    }

    /// Re-checks the realized shadow-tree changes immediately before build.
    ///
    /// # Errors
    ///
    /// Applies the same fail-closed rules as [`Self::admit_proposal`].
    pub fn recheck_before_build(
        &self,
        realized_changes: &[ChangedPath],
    ) -> Result<ChangeClass, GuardError> {
        self.admit_proposal(realized_changes)
    }

    /// Classifies a built artifact's actual source manifest.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid paths or an empty/unclassifiable manifest.
    pub fn classify_artifact(
        &self,
        manifest: &ArtifactManifest,
    ) -> Result<ChangeClass, GuardError> {
        if manifest.source_paths.is_empty()
            && !manifest.public_protocol_changed
            && !manifest.persisted_schema_changed
            && !manifest.requires_daemon_replacement
        {
            return Err(GuardError::Unclassifiable);
        }
        let mut class = if manifest.public_protocol_changed
            || manifest.persisted_schema_changed
            || manifest.requires_daemon_replacement
        {
            ChangeClass::C
        } else {
            ChangeClass::A
        };
        for path in &manifest.source_paths {
            class = class.max(Self::classify_resolved(&self.resolve(path)?)?);
        }
        Ok(class)
    }

    /// Re-checks the artifact immediately before promotion or worker roll.
    ///
    /// # Errors
    ///
    /// Returns [`GuardError::ProtectedSurface`] for Class D or any underlying
    /// path/classification error.
    pub fn recheck_before_promotion(
        &self,
        manifest: &ArtifactManifest,
    ) -> Result<ChangeClass, GuardError> {
        refuse_class_d(self.classify_artifact(manifest)?)
    }

    /// Recomputes classification from both proposal and built artifact facts.
    ///
    /// # Errors
    ///
    /// Refuses Class D at either phase and propagates path/classification
    /// failures.
    pub fn recompute(
        &self,
        proposal: &[ChangedPath],
        manifest: &ArtifactManifest,
    ) -> Result<Classification, GuardError> {
        let proposal = self.admit_proposal(proposal)?;
        let artifact = self.recheck_before_promotion(manifest)?;
        let effective = proposal.max(artifact);
        Ok(Classification {
            proposal,
            artifact,
            escalated: artifact > proposal,
            consent: effective.consent_policy(),
        })
    }

    /// Validates that the source of a consent decision has the authority
    /// required by the compiled class policy.
    ///
    /// # Errors
    ///
    /// Refuses Class D and rejects Class C unless the authenticated authority
    /// is the installation owner.
    pub fn validate_consent(
        &self,
        class: ChangeClass,
        authority: &ConsentAuthority,
    ) -> Result<ConsentPolicy, GuardError> {
        match (class, authority) {
            (ChangeClass::D, _) => Err(GuardError::ProtectedSurface),
            (ChangeClass::C, ConsentAuthority::InstallationOwner { identity })
                if !identity.trim().is_empty() =>
            {
                Ok(ConsentPolicy::HumanApproval)
            }
            (ChangeClass::C, _) => Err(GuardError::HumanApprovalRequired),
            (class, _) => Ok(class.consent_policy()),
        }
    }

    fn classify_resolved(relative: &Path) -> Result<ChangeClass, GuardError> {
        if ProtectedSurface::contains(relative) {
            return Ok(ChangeClass::D);
        }
        if is_class_c(relative) {
            return Ok(ChangeClass::C);
        }
        if is_class_a(relative) {
            return Ok(ChangeClass::A);
        }
        if relative
            .extension()
            .is_some_and(|extension| extension == "rs")
        {
            return Ok(ChangeClass::B);
        }
        Err(GuardError::Unclassifiable)
    }
}

fn refuse_class_d(class: ChangeClass) -> Result<ChangeClass, GuardError> {
    if class == ChangeClass::D {
        Err(GuardError::ProtectedSurface)
    } else {
        Ok(class)
    }
}

fn validate_lexical(path: &Path) -> Result<(), GuardError> {
    if path.is_absolute() {
        return Err(GuardError::AbsolutePath(path.to_path_buf()));
    }
    let rendered = path.as_os_str().to_string_lossy();
    if rendered.starts_with(r"\\")
        || rendered
            .as_bytes()
            .get(1)
            .is_some_and(|separator| *separator == b':')
    {
        return Err(GuardError::DevicePath(path.to_path_buf()));
    }
    if path.components().any(|component| {
        matches!(
            component,
            Component::ParentDir | Component::RootDir | Component::Prefix(_)
        )
    }) {
        return Err(GuardError::Traversal(path.to_path_buf()));
    }
    if path.as_os_str().is_empty() || path.components().any(device_component) {
        return Err(GuardError::DevicePath(path.to_path_buf()));
    }
    Ok(())
}

fn device_component(component: Component<'_>) -> bool {
    let Component::Normal(value) = component else {
        return false;
    };
    let name = value.to_string_lossy();
    let stem = name
        .split('.')
        .next()
        .unwrap_or_default()
        .to_ascii_uppercase();
    matches!(stem.as_str(), "CON" | "PRN" | "AUX" | "NUL")
        || (stem.len() == 4
            && (stem.starts_with("COM") || stem.starts_with("LPT"))
            && stem.as_bytes()[3].is_ascii_digit()
            && stem.as_bytes()[3] != b'0')
}

fn path_is_within(path: &Path, prefix: &Path) -> bool {
    path == prefix || path.starts_with(prefix)
}

fn is_class_a(path: &Path) -> bool {
    let extension = path.extension().and_then(OsStr::to_str);
    path.components().any(|component| {
        component.as_os_str() == "tests"
            || component.as_os_str() == "benches"
            || component.as_os_str() == "corpus"
            || component.as_os_str() == "docs"
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

#[cfg(test)]
mod tests {
    use super::*;

    fn guard(root: &Path) -> EvolutionGuard {
        EvolutionGuard::new(root).expect("guard root")
    }

    #[test]
    fn guard_classifies_every_boundary_and_maps_fixed_consent() {
        let root = tempfile::tempdir().expect("workspace");
        let guard = guard(root.path());

        assert_eq!(
            guard
                .classify_proposal(&[ChangedPath::Write("docs/guide.md".into())])
                .expect("class A"),
            ChangeClass::A
        );
        assert_eq!(
            guard
                .classify_proposal(&[ChangedPath::Write("crates/tools/src/lib.rs".into())])
                .expect("class B"),
            ChangeClass::B
        );
        assert_eq!(
            guard
                .classify_proposal(&[ChangedPath::Write("crates/protocol/src/lib.rs".into())])
                .expect("class C"),
            ChangeClass::C
        );
        assert_eq!(
            guard
                .classify_proposal(&[ChangedPath::Write("crates/agent-loop/src/lib.rs".into())])
                .expect("class D"),
            ChangeClass::D
        );
        assert_eq!(ChangeClass::A.consent_policy(), ConsentPolicy::Autonomous);
        assert_eq!(
            ChangeClass::B.consent_policy(),
            ConsentPolicy::WatchdogBounded
        );
        assert_eq!(
            ChangeClass::C.consent_policy(),
            ConsentPolicy::HumanApproval
        );
        assert_eq!(ChangeClass::D.consent_policy(), ConsentPolicy::Refused);
    }

    #[test]
    fn guard_recomputes_artifact_class_and_suspends_on_escalation() {
        let root = tempfile::tempdir().expect("workspace");
        let guard = guard(root.path());
        let result = guard
            .recompute(
                &[ChangedPath::Write("tests/latency.rs".into())],
                &ArtifactManifest {
                    source_paths: vec!["tests/latency.rs".into()],
                    requires_daemon_replacement: true,
                    ..ArtifactManifest::default()
                },
            )
            .expect("classifiable artifact");
        assert!(result.escalated);
        assert_eq!(result.artifact, ChangeClass::C);
        assert_eq!(result.consent, ConsentPolicy::HumanApproval);
    }

    #[test]
    fn guard_refuses_protected_artifacts_and_rename_targets() {
        let root = tempfile::tempdir().expect("workspace");
        let guard = guard(root.path());
        let protected = ArtifactManifest {
            source_paths: vec!["crates/self-evolution/src/guard.rs".into()],
            ..ArtifactManifest::default()
        };
        assert!(matches!(
            guard.recompute(
                &[ChangedPath::Write("crates/tools/src/lib.rs".into())],
                &protected
            ),
            Err(GuardError::ProtectedSurface)
        ));
        assert!(matches!(
            guard.admit_proposal(&[ChangedPath::Rename {
                from: "docs/old.md".into(),
                to: "crates/memory/src/injected.rs".into(),
            }]),
            Err(GuardError::ProtectedSurface)
        ));
        assert!(matches!(
            guard.recheck_before_build(&[ChangedPath::Write(
                "crates/self-evolution/src/lib.rs".into()
            )]),
            Err(GuardError::ProtectedSurface)
        ));
        assert!(matches!(
            guard.admit_proposal(&[ChangedPath::Write(
                "crates/self-evolution/src/budget.rs".into()
            )]),
            Err(GuardError::ProtectedSurface)
        ));
    }

    #[test]
    fn guard_refuses_verification_gate_changes() {
        let root = tempfile::tempdir().expect("workspace");
        let guard = guard(root.path());

        for path in [
            "crates/self-evolution/src/build.rs",
            "apps/xtask/src/main.rs",
            "crates/sandbox/src/lib.rs",
            "crates/tool-runner/src/lib.rs",
        ] {
            let changes = [ChangedPath::Write(path.into())];
            assert_eq!(
                guard.classify_proposal(&changes).expect("class D"),
                ChangeClass::D,
                "verification gate path must be Class D: {path}"
            );
            assert!(
                matches!(
                    guard.admit_proposal(&changes),
                    Err(GuardError::ProtectedSurface)
                ),
                "verification gate path must be refused: {path}"
            );
        }
    }

    #[test]
    fn guard_rejects_absolute_traversal_device_and_symlink_escape() {
        let root = tempfile::tempdir().expect("workspace");
        let outside = tempfile::tempdir().expect("outside");
        let guard = guard(root.path());

        assert!(matches!(
            guard.resolve("/tmp/change.rs"),
            Err(GuardError::AbsolutePath(_))
        ));
        assert!(matches!(
            guard.resolve("src/../Cargo.toml"),
            Err(GuardError::Traversal(_))
        ));
        assert!(matches!(
            guard.resolve("NUL.txt"),
            Err(GuardError::DevicePath(_))
        ));
        assert!(matches!(
            guard.resolve(r"C:\device\change.rs"),
            Err(GuardError::DevicePath(_))
        ));
        assert!(matches!(
            guard.resolve(r"\\server\share\change.rs"),
            Err(GuardError::DevicePath(_))
        ));

        #[cfg(unix)]
        {
            std::os::unix::fs::symlink(outside.path(), root.path().join("escape"))
                .expect("create escape symlink");
            assert!(matches!(
                guard.resolve("escape/generated.rs"),
                Err(GuardError::EscapesWorkspace(_))
            ));
        }
    }

    #[test]
    fn guard_surface_is_compiled_and_cannot_be_widened_by_input() {
        let root = tempfile::tempdir().expect("workspace");
        let guard = guard(root.path());
        let hostile_configuration = "protected_paths=[]; class=autonomous";
        assert!(!hostile_configuration.is_empty());
        assert_eq!(guard.protected_surface(), ProtectedSurface);
        assert_eq!(
            guard
                .classify_proposal(&[ChangedPath::Write("crates/release/src/lib.rs".into())])
                .expect("release path classification"),
            ChangeClass::D
        );
        assert!(matches!(
            guard.classify_proposal(&[]),
            Err(GuardError::Unclassifiable)
        ));
    }

    #[test]
    fn guard_never_accepts_class_c_consent_from_non_human_sources() {
        let root = tempfile::tempdir().expect("workspace");
        let guard = guard(root.path());
        for authority in [
            ConsentAuthority::Client,
            ConsentAuthority::Channel,
            ConsentAuthority::Kernel,
            ConsentAuthority::Plugin,
            ConsentAuthority::Mcp,
            ConsentAuthority::Skill,
            ConsentAuthority::Model,
        ] {
            assert!(matches!(
                guard.validate_consent(ChangeClass::C, &authority),
                Err(GuardError::HumanApprovalRequired)
            ));
        }
        assert!(matches!(
            guard.validate_consent(
                ChangeClass::C,
                &ConsentAuthority::InstallationOwner {
                    identity: String::new()
                }
            ),
            Err(GuardError::HumanApprovalRequired)
        ));
        assert_eq!(
            guard
                .validate_consent(
                    ChangeClass::C,
                    &ConsentAuthority::InstallationOwner {
                        identity: "owner:local".into()
                    }
                )
                .expect("authenticated owner approval"),
            ConsentPolicy::HumanApproval
        );
    }
}
