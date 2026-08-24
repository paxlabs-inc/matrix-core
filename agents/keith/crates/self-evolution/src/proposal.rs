use std::collections::BTreeSet;
use std::fs;
use std::path::{Path, PathBuf};

use ring::hmac;
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

use crate::{ChangedPath, EvolutionGuard, GuardError, ShadowError, ShadowTree};

#[cfg(test)]
thread_local! { static FAIL_ROLLBACK: std::cell::Cell<bool> = const { std::cell::Cell::new(false) }; }

#[derive(Clone, Eq, PartialEq)]
pub struct DependencyConsent {
    shadow_id: String,
    base_revision: String,
    dependency_digest: [u8; 32],
    proposal_digest: [u8; 32],
    authority_binding: [u8; 32],
    tag: [u8; 32],
}

impl DependencyConsent {
    pub(crate) fn issue(
        shadow_id: &keith_agent_types::EntityId,
        base_revision: &str,
        dependencies: &[String],
        proposal_digest: [u8; 32],
        authority_binding: [u8; 32],
    ) -> Self {
        let dependency_digest = dependency_digest(dependencies);
        let tag = consent_tag(
            shadow_id.as_str(),
            base_revision,
            &dependency_digest,
            &proposal_digest,
            &authority_binding,
        );
        Self {
            shadow_id: shadow_id.as_str().to_owned(),
            base_revision: base_revision.to_owned(),
            dependency_digest,
            proposal_digest,
            authority_binding,
            tag,
        }
    }

    fn validates(
        &self,
        shadow_id: &keith_agent_types::EntityId,
        base_revision: &str,
        dependencies: &[String],
        proposal_digest: [u8; 32],
    ) -> bool {
        let digest = dependency_digest(dependencies);
        self.shadow_id == shadow_id.as_str()
            && self.base_revision == base_revision
            && self.dependency_digest == digest
            && self.proposal_digest == proposal_digest
            && constant_time_equal(
                &self.tag,
                &consent_tag(
                    shadow_id.as_str(),
                    base_revision,
                    &digest,
                    &proposal_digest,
                    &self.authority_binding,
                ),
            )
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ProposalLimits {
    pub max_changed_files: usize,
    pub max_file_bytes: usize,
    pub max_total_bytes: usize,
    pub allow_new_dependencies: bool,
    pub max_new_dependencies: usize,
}

impl Default for ProposalLimits {
    fn default() -> Self {
        Self {
            max_changed_files: 8,
            max_file_bytes: 256 * 1024,
            max_total_bytes: 512 * 1024,
            allow_new_dependencies: false,
            max_new_dependencies: 0,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "operation", deny_unknown_fields)]
pub enum ProposalEdit {
    Write { path: PathBuf, bytes: Vec<u8> },
    Delete { path: PathBuf },
    Rename { from: PathBuf, to: PathBuf },
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProposalPreimage {
    pub path: PathBuf,
    pub prior_bytes: Option<Vec<u8>>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct EvolutionProposal {
    pub summary: String,
    pub changes: Vec<ChangedPath>,
    pub preimages: Vec<ProposalPreimage>,
    pub new_dependencies: Vec<String>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct ReviewerAuthority {
    _sealed: (),
}

impl ReviewerAuthority {
    #[must_use]
    pub const fn read_only() -> Self {
        Self { _sealed: () }
    }

    #[must_use]
    pub const fn selected_source_read(self) -> bool {
        true
    }
    #[must_use]
    pub const fn shell(self) -> bool {
        false
    }
    #[must_use]
    pub const fn write(self) -> bool {
        false
    }
    #[must_use]
    pub const fn network(self) -> bool {
        false
    }
    #[must_use]
    pub const fn credentials(self) -> bool {
        false
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
pub struct ReviewerSource {
    pub path: PathBuf,
    pub bytes: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ReadOnlyReviewerBundle {
    pub authority: ReviewerAuthority,
    pub base_revision: String,
    pub hypothesis: String,
    pub failure_evidence: Vec<String>,
    pub source: Vec<ReviewerSource>,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq)]
pub struct NoToolsReviewer;

impl NoToolsReviewer {
    /// Accepts only a typed proposal response. Shell, network, filesystem, credential, and tool
    /// requests have no protocol representation and are rejected as unknown input.
    ///
    /// # Errors
    /// Returns malformed-patch or limit errors for any response outside the proposal schema.
    pub fn accept_response(
        self,
        bundle: &ReadOnlyReviewerBundle,
        response: &[u8],
        limits: ProposalLimits,
    ) -> Result<Vec<ProposalEdit>, ProposalError> {
        if self != Self
            || bundle.authority != ReviewerAuthority::read_only()
            || response.len() > limits.max_total_bytes
        {
            return Err(ProposalError::LimitExceeded);
        }
        let edits: Vec<ProposalEdit> = serde_json::from_slice(response)
            .map_err(|error| ProposalError::MalformedResponse(error.to_string()))?;
        if edits.is_empty() || edits.len() > limits.max_changed_files {
            return Err(ProposalError::LimitExceeded);
        }
        Ok(edits)
    }
}

#[derive(Debug, Error)]
pub enum ProposalError {
    #[error("proposal limits are invalid or exceeded")]
    LimitExceeded,
    #[error("proposal path occurs more than once")]
    DuplicatePath,
    #[error("proposal summary is empty or unbounded")]
    InvalidSummary,
    #[error("new third-party dependencies are denied: {0:?}")]
    DependencyDenied(Vec<String>),
    #[error("proposal filesystem operation failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("shadow operation failed: {0}")]
    Shadow(#[from] ShadowError),
    #[error("guard rejected proposal: {0}")]
    Guard(#[from] GuardError),
    #[error("Cargo manifest is malformed: {0}")]
    Manifest(String),
    #[error("proposal rollback failed and the shadow was quarantined")]
    RollbackFailed,
    #[error("reviewer response is malformed: {0}")]
    MalformedResponse(String),
}

impl ShadowTree {
    /// Creates the sole reviewer input from bytes already present in this shadow.
    ///
    /// # Errors
    /// Returns an error for unsafe paths, unsupported entries, or exceeded limits.
    pub fn reviewer_bundle(
        &self,
        hypothesis: String,
        failure_evidence: Vec<String>,
        selected: &[PathBuf],
        limits: ProposalLimits,
    ) -> Result<ReadOnlyReviewerBundle, ProposalError> {
        if selected.len() > limits.max_changed_files || hypothesis.len() > limits.max_total_bytes {
            return Err(ProposalError::LimitExceeded);
        }
        let mut total = hypothesis.len();
        for evidence in &failure_evidence {
            total = total.saturating_add(evidence.len());
            if total > limits.max_total_bytes {
                return Err(ProposalError::LimitExceeded);
            }
        }
        let mut seen = BTreeSet::new();
        let mut source = Vec::new();
        for path in selected {
            if !seen.insert(path.clone()) {
                return Err(ProposalError::DuplicatePath);
            }
            let bytes = fs::read(self.resolve(path, false)?)?;
            total = total.saturating_add(bytes.len());
            if bytes.len() > limits.max_file_bytes || total > limits.max_total_bytes {
                return Err(ProposalError::LimitExceeded);
            }
            source.push(ReviewerSource {
                path: path.clone(),
                bytes,
            });
        }
        Ok(ReadOnlyReviewerBundle {
            authority: ReviewerAuthority::read_only(),
            base_revision: self.base_revision().to_owned(),
            hypothesis,
            failure_evidence,
            source,
        })
    }

    /// Applies a bounded typed proposal only to the shadow and records exact preimages.
    ///
    /// # Errors
    /// Returns an error before mutation for invalid proposals and restores exact preimages after
    /// any post-mutation failure.
    #[allow(clippy::too_many_lines)]
    pub fn apply_proposal(
        &self,
        summary: String,
        edits: &[ProposalEdit],
        limits: ProposalLimits,
        dependency_consent: Option<&DependencyConsent>,
    ) -> Result<EvolutionProposal, ProposalError> {
        if summary.trim().is_empty() || summary.len() > 512 {
            return Err(ProposalError::InvalidSummary);
        }
        if edits.is_empty()
            || limits.max_changed_files == 0
            || limits.max_file_bytes == 0
            || limits.max_total_bytes == 0
        {
            return Err(ProposalError::LimitExceeded);
        }
        let before_dependencies = collect_dependencies(self.root())?;
        let before_manifest = self.current_manifest_digest()?;
        let before_directories = collect_directories(self.root())?;
        let mut seen = BTreeSet::new();
        let mut total = 0usize;
        let mut preimages = Vec::new();
        let mut changes = Vec::new();
        for edit in edits {
            match edit {
                ProposalEdit::Write { path, bytes } => {
                    unique(&mut seen, path)?;
                    if bytes.len() > limits.max_file_bytes {
                        return Err(ProposalError::LimitExceeded);
                    }
                    let target = self.resolve(path, true)?;
                    let prior = read_optional(&target)?;
                    if prior
                        .as_ref()
                        .is_some_and(|bytes| bytes.len() > limits.max_file_bytes)
                    {
                        return Err(ProposalError::LimitExceeded);
                    }
                    total = add_total(total, prior.as_ref(), Some(bytes), limits)?;
                    preimages.push(ProposalPreimage {
                        path: path.clone(),
                        prior_bytes: prior,
                    });
                    changes.push(ChangedPath::Write(path.clone()));
                }
                ProposalEdit::Delete { path } => {
                    unique(&mut seen, path)?;
                    let target = self.resolve(path, false)?;
                    let prior = fs::read(&target)?;
                    if prior.len() > limits.max_file_bytes {
                        return Err(ProposalError::LimitExceeded);
                    }
                    total = add_total(total, Some(&prior), None, limits)?;
                    preimages.push(ProposalPreimage {
                        path: path.clone(),
                        prior_bytes: Some(prior),
                    });
                    changes.push(ChangedPath::Delete(path.clone()));
                }
                ProposalEdit::Rename { from, to } => {
                    unique(&mut seen, from)?;
                    unique(&mut seen, to)?;
                    let source = self.resolve(from, false)?;
                    let destination = self.resolve(to, true)?;
                    let prior = fs::read(&source)?;
                    let destination_prior = read_optional(&destination)?;
                    if prior.len() > limits.max_file_bytes
                        || destination_prior
                            .as_ref()
                            .is_some_and(|bytes| bytes.len() > limits.max_file_bytes)
                    {
                        return Err(ProposalError::LimitExceeded);
                    }
                    total = add_total(total, Some(&prior), destination_prior.as_ref(), limits)?;
                    preimages.push(ProposalPreimage {
                        path: from.clone(),
                        prior_bytes: Some(prior),
                    });
                    preimages.push(ProposalPreimage {
                        path: to.clone(),
                        prior_bytes: destination_prior,
                    });
                    changes.push(ChangedPath::Rename {
                        from: from.clone(),
                        to: to.clone(),
                    });
                }
            }
            if seen.len() > limits.max_changed_files {
                return Err(ProposalError::LimitExceeded);
            }
        }
        let guard = EvolutionGuard::new(self.root())?;
        guard.admit_proposal(&changes)?;
        if let Err(error) = apply_edits(self, edits) {
            return Err(rollback_result(
                self,
                &preimages,
                &before_directories,
                &before_manifest,
                error,
            ));
        }
        let after_dependencies = match collect_dependencies(self.root()) {
            Ok(dependencies) => dependencies,
            Err(error) => {
                return Err(rollback_result(
                    self,
                    &preimages,
                    &before_directories,
                    &before_manifest,
                    error,
                ));
            }
        };
        let new_dependencies = after_dependencies
            .difference(&before_dependencies)
            .cloned()
            .collect::<Vec<_>>();
        if (!limits.allow_new_dependencies && !new_dependencies.is_empty())
            || new_dependencies.len() > limits.max_new_dependencies
        {
            let error = ProposalError::DependencyDenied(new_dependencies);
            return Err(rollback_result(
                self,
                &preimages,
                &before_directories,
                &before_manifest,
                error,
            ));
        }
        if !new_dependencies.is_empty() {
            let Some(consent) = dependency_consent else {
                let error = ProposalError::Guard(GuardError::HumanApprovalRequired);
                return Err(rollback_result(
                    self,
                    &preimages,
                    &before_directories,
                    &before_manifest,
                    error,
                ));
            };
            if !consent.validates(
                self.id(),
                self.base_revision(),
                &new_dependencies,
                proposal_digest(&summary, edits),
            ) {
                let error = ProposalError::Guard(GuardError::HumanApprovalRequired);
                return Err(rollback_result(
                    self,
                    &preimages,
                    &before_directories,
                    &before_manifest,
                    error,
                ));
            }
        }
        Ok(EvolutionProposal {
            summary,
            changes,
            preimages,
            new_dependencies,
        })
    }
}

fn apply_edits(shadow: &ShadowTree, edits: &[ProposalEdit]) -> Result<(), ProposalError> {
    for edit in edits {
        match edit {
            ProposalEdit::Write { path, bytes } => {
                let target = shadow.resolve(path, true)?;
                fs::create_dir_all(
                    target
                        .parent()
                        .ok_or_else(|| ShadowError::UnsafePath(path.clone()))?,
                )?;
                fs::write(target, bytes)?;
            }
            ProposalEdit::Delete { path } => fs::remove_file(shadow.resolve(path, false)?)?,
            ProposalEdit::Rename { from, to } => {
                let destination = shadow.resolve(to, true)?;
                fs::create_dir_all(
                    destination
                        .parent()
                        .ok_or_else(|| ShadowError::UnsafePath(to.clone()))?,
                )?;
                fs::rename(shadow.resolve(from, false)?, destination)?;
            }
        }
    }
    Ok(())
}

fn restore_preimages(
    shadow: &ShadowTree,
    preimages: &[ProposalPreimage],
    before_directories: &BTreeSet<PathBuf>,
) -> Result<(), ProposalError> {
    #[cfg(test)]
    if FAIL_ROLLBACK.with(std::cell::Cell::get) {
        return Err(std::io::Error::other("injected rollback failure").into());
    }
    for preimage in preimages.iter().rev() {
        let target = shadow.resolve(&preimage.path, true)?;
        match &preimage.prior_bytes {
            Some(bytes) => {
                if let Some(parent) = target.parent() {
                    fs::create_dir_all(parent)?;
                }
                fs::write(target, bytes)?;
            }
            None => match fs::remove_file(target) {
                Ok(()) => {}
                Err(error) if error.kind() == std::io::ErrorKind::NotFound => {}
                Err(error) => return Err(error.into()),
            },
        }
    }
    remove_new_empty_directories(shadow.root(), before_directories)?;
    Ok(())
}

fn rollback_result(
    shadow: &ShadowTree,
    preimages: &[ProposalPreimage],
    before_directories: &BTreeSet<PathBuf>,
    before_manifest: &str,
    original: ProposalError,
) -> ProposalError {
    let restored = restore_preimages(shadow, preimages, before_directories)
        .and_then(|()| {
            shadow
                .current_manifest_digest()
                .map_err(ProposalError::from)
        })
        .is_ok_and(|digest| digest == before_manifest);
    if restored {
        original
    } else {
        let _ = shadow.quarantine();
        ProposalError::RollbackFailed
    }
}

fn collect_directories(root: &Path) -> Result<BTreeSet<PathBuf>, ProposalError> {
    fn visit(
        root: &Path,
        directory: &Path,
        output: &mut BTreeSet<PathBuf>,
    ) -> Result<(), ProposalError> {
        for entry in fs::read_dir(directory)? {
            let entry = entry?;
            let path = entry.path();
            let kind = entry.file_type()?;
            if kind.is_symlink() {
                return Err(ShadowError::UnsupportedEntry(path).into());
            }
            if kind.is_dir() {
                output.insert(
                    path.strip_prefix(root)
                        .map_err(|error| ProposalError::Manifest(error.to_string()))?
                        .to_path_buf(),
                );
                visit(root, &path, output)?;
            }
        }
        Ok(())
    }
    let mut output = BTreeSet::new();
    visit(root, root, &mut output)?;
    Ok(output)
}

fn remove_new_empty_directories(
    root: &Path,
    before: &BTreeSet<PathBuf>,
) -> Result<(), ProposalError> {
    let mut directories = collect_directories(root)?
        .into_iter()
        .filter(|path| !before.contains(path))
        .collect::<Vec<_>>();
    directories.sort_by_key(|path| std::cmp::Reverse(path.components().count()));
    for relative in directories {
        match fs::remove_dir(root.join(relative)) {
            Ok(()) => {}
            Err(error)
                if error.kind() == std::io::ErrorKind::DirectoryNotEmpty
                    || error.kind() == std::io::ErrorKind::NotFound => {}
            Err(error) => return Err(error.into()),
        }
    }
    Ok(())
}

pub fn proposal_digest(summary: &str, edits: &[ProposalEdit]) -> [u8; 32] {
    let mut digest = Sha256::new();
    digest.update(summary.as_bytes());
    digest.update([0]);
    if let Ok(bytes) = serde_json::to_vec(edits) {
        digest.update(bytes);
    }
    digest.finalize().into()
}

fn unique(seen: &mut BTreeSet<PathBuf>, path: &Path) -> Result<(), ProposalError> {
    if seen.insert(path.to_path_buf()) {
        Ok(())
    } else {
        Err(ProposalError::DuplicatePath)
    }
}

fn read_optional(path: &Path) -> Result<Option<Vec<u8>>, std::io::Error> {
    match fs::read(path) {
        Ok(bytes) => Ok(Some(bytes)),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(None),
        Err(error) => Err(error),
    }
}

fn add_total(
    mut total: usize,
    before: Option<&Vec<u8>>,
    after: Option<&Vec<u8>>,
    limits: ProposalLimits,
) -> Result<usize, ProposalError> {
    total = total
        .saturating_add(before.map_or(0, Vec::len))
        .saturating_add(after.map_or(0, Vec::len));
    if total > limits.max_total_bytes {
        Err(ProposalError::LimitExceeded)
    } else {
        Ok(total)
    }
}

fn collect_dependencies(root: &Path) -> Result<BTreeSet<String>, ProposalError> {
    let mut manifests = Vec::new();
    collect_manifests(root, &mut manifests)?;
    let mut dependencies = BTreeSet::new();
    for manifest in manifests {
        let relative = manifest
            .strip_prefix(root)
            .map_err(|error| ProposalError::Manifest(error.to_string()))?
            .to_string_lossy();
        let text = fs::read_to_string(&manifest)?;
        let value: toml::Value = toml::from_str(&text)
            .map_err(|error| ProposalError::Manifest(format!("{}: {error}", manifest.display())))?;
        collect_dependency_tables(&value, &relative, "", &mut dependencies);
    }
    Ok(dependencies)
}

fn collect_manifests(directory: &Path, output: &mut Vec<PathBuf>) -> Result<(), ProposalError> {
    for entry in fs::read_dir(directory)? {
        let entry = entry?;
        let path = entry.path();
        let kind = entry.file_type()?;
        if kind.is_symlink() {
            return Err(ShadowError::UnsupportedEntry(path).into());
        }
        if kind.is_dir() {
            collect_manifests(&path, output)?;
        } else if kind.is_file() && path.file_name().is_some_and(|name| name == "Cargo.toml") {
            output.push(path);
        }
    }
    Ok(())
}

fn collect_dependency_tables(
    value: &toml::Value,
    manifest: &str,
    prefix: &str,
    output: &mut BTreeSet<String>,
) {
    let Some(table) = value.as_table() else {
        return;
    };
    for (key, child) in table {
        let section = if prefix.is_empty() {
            key.clone()
        } else {
            format!("{prefix}.{key}")
        };
        if matches!(
            key.as_str(),
            "dependencies" | "dev-dependencies" | "build-dependencies"
        ) && let Some(entries) = child.as_table()
        {
            for dependency in entries.keys() {
                let specification = entries
                    .get(dependency)
                    .map(toml::Value::to_string)
                    .unwrap_or_default();
                output.insert(format!("{manifest}:{section}:{dependency}={specification}"));
            }
        }
        collect_dependency_tables(child, manifest, &section, output);
    }
}

fn dependency_digest(dependencies: &[String]) -> [u8; 32] {
    let mut ordered = dependencies.to_vec();
    ordered.sort();
    let mut digest = Sha256::new();
    for dependency in ordered {
        digest.update(dependency.as_bytes());
        digest.update([0]);
    }
    digest.finalize().into()
}

fn consent_tag(
    shadow_id: &str,
    base_revision: &str,
    dependency_digest: &[u8; 32],
    proposal_digest: &[u8; 32],
    binding: &[u8; 32],
) -> [u8; 32] {
    let key = hmac::Key::new(hmac::HMAC_SHA256, binding);
    let mut message = shadow_id.as_bytes().to_vec();
    message.push(0);
    message.extend_from_slice(base_revision.as_bytes());
    message.push(0);
    message.extend_from_slice(dependency_digest);
    message.push(0);
    message.extend_from_slice(proposal_digest);
    hmac::sign(&key, &message)
        .as_ref()
        .try_into()
        .expect("HMAC-SHA256 length")
}

#[cfg(test)]
pub(crate) fn inject_rollback_failure(enabled: bool) {
    FAIL_ROLLBACK.with(|flag| flag.set(enabled));
}

fn constant_time_equal(left: &[u8], right: &[u8]) -> bool {
    left.len() == right.len()
        && left
            .iter()
            .zip(right)
            .fold(0_u8, |difference, (left, right)| {
                difference | (left ^ right)
            })
            == 0
}
