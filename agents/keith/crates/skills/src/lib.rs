#![forbid(unsafe_code)]

mod synthesis;

pub use synthesis::*;

use std::collections::{BTreeMap, BTreeSet};
use std::fmt::Write as _;
use std::fs::{self, File, OpenOptions};
use std::io::Write;
use std::path::{Component, Path, PathBuf};
use std::sync::{Mutex, MutexGuard};

use keith_agent_types::{CURRENT_SCHEMA_VERSION, EntityId, Revision, SchemaVersion, UtcTimestamp};
use keith_workspace::{EditOutcome, FileToken, PersonalWorkspace, WorkspaceActor, WorkspaceEvent};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;

const LEDGER_PATH: &str = ".keith/skill-ledger.json";
const HISTORY_ROOT: &str = ".keith/skill-history";

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SkillManifest {
    pub id: String,
    pub version: String,
    pub description: String,
    pub triggers: Vec<String>,
    pub inputs: Vec<String>,
    pub steps: Vec<String>,
    pub required_tools: Vec<String>,
    pub validation: Vec<String>,
    pub known_failures: Vec<String>,
    pub stop_conditions: Vec<String>,
    #[serde(default)]
    pub platforms: Vec<String>,
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SkillScope {
    BuiltIn,
    Global,
    Project,
    Profile,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SkillProvenance {
    pub scope: SkillScope,
    pub origin: String,
    pub source_path: PathBuf,
    pub digest: String,
    pub installed_at: Option<UtcTimestamp>,
    pub revision: Option<Revision>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SkillPackage {
    pub manifest: SkillManifest,
    pub body: String,
    pub source: String,
    pub provenance: SkillProvenance,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SkillHistoryAction {
    Install,
    Update,
    Rollback,
    Disable,
    Enable,
    Delete,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SkillHistoryEntry {
    pub revision: Revision,
    pub action: SkillHistoryAction,
    pub digest: Option<String>,
    pub version: Option<String>,
    pub backup_path: Option<PathBuf>,
    pub at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SkillInspection {
    pub id: String,
    pub enabled: bool,
    pub effective: Option<SkillPackage>,
    pub candidates: Vec<SkillPackage>,
    pub history: Vec<SkillHistoryEntry>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SkillSelectionRequest {
    pub task: String,
    pub platform: String,
    pub ready_tools: BTreeSet<String>,
    pub max_prompt_bytes: usize,
    pub max_skills: usize,
}

#[derive(Clone, Debug, PartialEq)]
pub struct SelectedSkill {
    pub id: String,
    pub score: f32,
    pub prompt: String,
    pub provenance: SkillProvenance,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ExcludedSkill {
    pub id: String,
    pub reason: String,
}

#[derive(Clone, Debug, PartialEq)]
pub struct SkillSelection {
    pub selected: Vec<SelectedSkill>,
    pub excluded: Vec<ExcludedSkill>,
    pub used_prompt_bytes: usize,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct SkillRoots {
    pub built_in: PathBuf,
    pub global: PathBuf,
    pub project: PathBuf,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct SkillLimits {
    pub max_skill_bytes: usize,
    pub max_skills_per_scope: usize,
    pub max_history_per_skill: usize,
}

impl Default for SkillLimits {
    fn default() -> Self {
        Self {
            max_skill_bytes: 256 * 1_024,
            max_skills_per_scope: 2_048,
            max_history_per_skill: 64,
        }
    }
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct SkillLedger {
    version: SchemaVersion,
    next_revision: Revision,
    enabled: BTreeMap<String, bool>,
    installed_at: BTreeMap<String, UtcTimestamp>,
    #[serde(default)]
    origins: BTreeMap<String, String>,
    history: BTreeMap<String, Vec<SkillHistoryEntry>>,
}

impl Default for SkillLedger {
    fn default() -> Self {
        Self {
            version: CURRENT_SCHEMA_VERSION,
            next_revision: Revision::ZERO,
            enabled: BTreeMap::new(),
            installed_at: BTreeMap::new(),
            origins: BTreeMap::new(),
            history: BTreeMap::new(),
        }
    }
}

#[derive(Debug, Error)]
pub enum SkillError {
    #[error("skill I/O failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("skill JSON failed: {0}")]
    Json(#[from] serde_json::Error),
    #[error("skill manifest failed: {0}")]
    Manifest(#[from] toml::de::Error),
    #[error("skill workspace failed: {0}")]
    Workspace(#[from] keith_workspace::PersonalWorkspaceError),
    #[error("skill package is malformed, unsafe, or oversized")]
    InvalidPackage,
    #[error("skill was not found")]
    NotFound,
    #[error("skill already exists")]
    AlreadyExists,
    #[error("skill update conflicts with a newer workspace version")]
    Conflict,
    #[error("built-in skills are immutable")]
    ImmutableBuiltIn,
    #[error("skill history revision was not found")]
    MissingRevision,
    #[error("skill state lock was poisoned")]
    LockPoisoned,
    #[error("skill schema is unsupported")]
    UnsupportedSchema,
}

pub struct SkillRegistry {
    workspace: PersonalWorkspace,
    roots: SkillRoots,
    limits: SkillLimits,
    ledger: Mutex<SkillLedger>,
    serial: Mutex<()>,
}

impl SkillRegistry {
    /// Opens restart-safe lifecycle history over immutable and mutable skill roots.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid limits, corrupt history, or unsupported schema.
    pub fn open(
        workspace: PersonalWorkspace,
        roots: SkillRoots,
        limits: SkillLimits,
    ) -> Result<Self, SkillError> {
        if limits.max_skill_bytes == 0
            || limits.max_skills_per_scope == 0
            || limits.max_history_per_skill == 0
        {
            return Err(SkillError::InvalidPackage);
        }
        for root in [&roots.built_in, &roots.global, &roots.project] {
            fs::create_dir_all(root)?;
        }
        let ledger_path = workspace.layout().root.join(LEDGER_PATH);
        let ledger = if ledger_path.exists() {
            serde_json::from_slice::<SkillLedger>(&fs::read(ledger_path)?)?
        } else {
            SkillLedger::default()
        };
        if ledger.version.major != CURRENT_SCHEMA_VERSION.major
            || ledger.version.minor > CURRENT_SCHEMA_VERSION.minor
        {
            return Err(SkillError::UnsupportedSchema);
        }
        Ok(Self {
            workspace,
            roots,
            limits,
            ledger: Mutex::new(ledger),
            serial: Mutex::new(()),
        })
    }

    /// Discovers declarative packages and applies built-in/global/project/profile precedence.
    ///
    /// # Errors
    ///
    /// Returns an error for malformed packages, symlinks, limits, or workspace scan failure.
    pub fn discover(&self, now: UtcTimestamp) -> Result<Vec<SkillPackage>, SkillError> {
        let _guard = self.lock()?;
        self.scan_workspace(now)?;
        let ledger = self.ledger()?;
        let candidates = self.discover_candidates(&ledger)?;
        Ok(effective_packages(candidates, &ledger.enabled))
    }

    /// Selects relevant declarative skills under platform, ready-tool, count, and byte budgets.
    ///
    /// # Errors
    ///
    /// Returns an error for an empty task/budget or failed discovery.
    pub fn select(
        &self,
        request: &SkillSelectionRequest,
        now: UtcTimestamp,
    ) -> Result<SkillSelection, SkillError> {
        if request.task.trim().is_empty()
            || request.max_prompt_bytes == 0
            || request.max_skills == 0
        {
            return Err(SkillError::InvalidPackage);
        }
        let packages = self.discover(now)?;
        let task = normalize(&request.task);
        let mut ranked = Vec::new();
        let mut excluded = Vec::new();
        for package in packages {
            if !package.manifest.platforms.is_empty()
                && !package
                    .manifest
                    .platforms
                    .iter()
                    .any(|platform| platform.eq_ignore_ascii_case(&request.platform))
            {
                excluded.push(ExcludedSkill {
                    id: package.manifest.id,
                    reason: "platform does not match".into(),
                });
                continue;
            }
            let missing = package
                .manifest
                .required_tools
                .iter()
                .filter(|tool| !request.ready_tools.contains(*tool))
                .cloned()
                .collect::<Vec<_>>();
            if !missing.is_empty() {
                excluded.push(ExcludedSkill {
                    id: package.manifest.id,
                    reason: format!("required tools unavailable: {}", missing.join(", ")),
                });
                continue;
            }
            let score = relevance(&task, &package.manifest);
            if score <= f32::EPSILON {
                excluded.push(ExcludedSkill {
                    id: package.manifest.id,
                    reason: "not relevant to the task".into(),
                });
            } else {
                ranked.push((score, package));
            }
        }
        ranked.sort_by(|(left_score, left), (right_score, right)| {
            right_score
                .total_cmp(left_score)
                .then_with(|| left.manifest.id.cmp(&right.manifest.id))
        });
        let mut selected = Vec::new();
        let mut used_prompt_bytes = 0_usize;
        for (score, package) in ranked {
            let prompt = render_prompt(&package);
            if selected.len() >= request.max_skills
                || used_prompt_bytes.saturating_add(prompt.len()) > request.max_prompt_bytes
            {
                excluded.push(ExcludedSkill {
                    id: package.manifest.id,
                    reason: "context budget exhausted".into(),
                });
                continue;
            }
            used_prompt_bytes += prompt.len();
            selected.push(SelectedSkill {
                id: package.manifest.id,
                score,
                prompt,
                provenance: package.provenance,
            });
        }
        excluded.sort_by(|left, right| left.id.cmp(&right.id));
        Ok(SkillSelection {
            selected,
            excluded,
            used_prompt_bytes,
        })
    }

    /// Installs a validated package into the profile workspace and records immutable provenance.
    ///
    /// # Errors
    ///
    /// Returns an error for existing packages, conflicts, malformed content, or persistence failure.
    pub fn install(
        &self,
        source: impl Into<String>,
        origin: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<SkillPackage, SkillError> {
        let _guard = self.lock()?;
        let source = source.into();
        let origin = origin.into();
        let parsed = parse_skill(&source, self.limits.max_skill_bytes)?;
        self.scan_workspace(now)?;
        let path = profile_skill_path(&parsed.manifest.id)?;
        let expected = self.workspace.token(&path)?;
        if expected.digest.is_some() {
            return Err(SkillError::AlreadyExists);
        }
        ensure_skill_directory(&self.workspace.layout().root, &parsed.manifest.id)?;
        let token = write_workspace_skill(&self.workspace, &path, &expected, &source, now)?;
        let mut ledger = self.ledger()?;
        let entry = record_content_history(
            &self.workspace.layout().root,
            &mut ledger,
            &parsed.manifest,
            &source,
            SkillHistoryAction::Install,
            now,
            self.limits.max_history_per_skill,
        )?;
        ledger.enabled.insert(parsed.manifest.id.clone(), true);
        ledger.installed_at.insert(parsed.manifest.id.clone(), now);
        ledger
            .origins
            .insert(parsed.manifest.id.clone(), origin.clone());
        persist_ledger(&self.workspace.layout().root, &ledger)?;
        package_from_parsed(
            parsed,
            source,
            SkillProvenance {
                scope: SkillScope::Profile,
                origin,
                source_path: path,
                digest: token.digest.ok_or(SkillError::InvalidPackage)?,
                installed_at: Some(now),
                revision: Some(entry.revision),
            },
        )
    }

    /// Updates the profile package only when the expected content digest remains current.
    ///
    /// # Errors
    ///
    /// Returns an error for built-in-only IDs, stale content, malformed updates, or persistence failure.
    pub fn update(
        &self,
        id: &str,
        expected_digest: &str,
        source: impl Into<String>,
        now: UtcTimestamp,
    ) -> Result<SkillPackage, SkillError> {
        let _guard = self.lock()?;
        validate_skill_id(id)?;
        let path = profile_skill_path(id)?;
        self.scan_workspace(now)?;
        let expected = self.workspace.token(&path)?;
        if expected.digest.as_deref() != Some(expected_digest) {
            return if expected.digest.is_none() && self.has_builtin(id)? {
                Err(SkillError::ImmutableBuiltIn)
            } else {
                Err(SkillError::Conflict)
            };
        }
        let source = source.into();
        let parsed = parse_skill(&source, self.limits.max_skill_bytes)?;
        if parsed.manifest.id != id {
            return Err(SkillError::InvalidPackage);
        }
        let token = write_workspace_skill(&self.workspace, &path, &expected, &source, now)?;
        let mut ledger = self.ledger()?;
        let entry = record_content_history(
            &self.workspace.layout().root,
            &mut ledger,
            &parsed.manifest,
            &source,
            SkillHistoryAction::Update,
            now,
            self.limits.max_history_per_skill,
        )?;
        ledger.origins.insert(id.into(), "profile update".into());
        persist_ledger(&self.workspace.layout().root, &ledger)?;
        package_from_parsed(
            parsed,
            source,
            SkillProvenance {
                scope: SkillScope::Profile,
                origin: "profile update".into(),
                source_path: path,
                digest: token.digest.ok_or(SkillError::InvalidPackage)?,
                installed_at: ledger.installed_at.get(id).copied(),
                revision: Some(entry.revision),
            },
        )
    }

    /// Disables an effective skill without deleting its readable package or history.
    ///
    /// # Errors
    ///
    /// Returns an error for unknown skills or persistence failure.
    pub fn disable(&self, id: &str, now: UtcTimestamp) -> Result<(), SkillError> {
        self.set_enabled(id, false, SkillHistoryAction::Disable, now)
    }

    /// Re-enables a discovered skill after validating that a package still exists.
    ///
    /// # Errors
    ///
    /// Returns an error for unknown skills or persistence failure.
    pub fn enable(&self, id: &str, now: UtcTimestamp) -> Result<(), SkillError> {
        self.set_enabled(id, true, SkillHistoryAction::Enable, now)
    }

    /// Restores a historical profile package through the versioned workspace.
    ///
    /// # Errors
    ///
    /// Returns an error for missing revisions, conflicts, invalid backups, or persistence failure.
    pub fn rollback(
        &self,
        id: &str,
        revision: Revision,
        now: UtcTimestamp,
    ) -> Result<SkillPackage, SkillError> {
        let _guard = self.lock()?;
        validate_skill_id(id)?;
        let mut ledger = self.ledger()?;
        let history = ledger.history.get(id).ok_or(SkillError::MissingRevision)?;
        let selected = history
            .iter()
            .find(|entry| entry.revision == revision)
            .and_then(|entry| entry.backup_path.clone())
            .ok_or(SkillError::MissingRevision)?;
        let source = fs::read_to_string(self.workspace.layout().root.join(selected))?;
        let parsed = parse_skill(&source, self.limits.max_skill_bytes)?;
        if parsed.manifest.id != id {
            return Err(SkillError::InvalidPackage);
        }
        self.scan_workspace(now)?;
        let path = profile_skill_path(id)?;
        ensure_skill_directory(&self.workspace.layout().root, id)?;
        let expected = self.workspace.token(&path)?;
        let token = write_workspace_skill(&self.workspace, &path, &expected, &source, now)?;
        let entry = record_content_history(
            &self.workspace.layout().root,
            &mut ledger,
            &parsed.manifest,
            &source,
            SkillHistoryAction::Rollback,
            now,
            self.limits.max_history_per_skill,
        )?;
        ledger.enabled.insert(id.into(), true);
        let rollback_origin = format!("rollback from revision {}", revision.get());
        ledger.origins.insert(id.into(), rollback_origin.clone());
        persist_ledger(&self.workspace.layout().root, &ledger)?;
        package_from_parsed(
            parsed,
            source,
            SkillProvenance {
                scope: SkillScope::Profile,
                origin: rollback_origin,
                source_path: path,
                digest: token.digest.ok_or(SkillError::InvalidPackage)?,
                installed_at: ledger.installed_at.get(id).copied(),
                revision: Some(entry.revision),
            },
        )
    }

    /// Deletes only a mutable profile package while preserving immutable lifecycle history.
    ///
    /// # Errors
    ///
    /// Returns an error for built-in-only IDs, conflicts, missing packages, or persistence failure.
    pub fn delete(&self, id: &str, now: UtcTimestamp) -> Result<(), SkillError> {
        let _guard = self.lock()?;
        validate_skill_id(id)?;
        self.scan_workspace(now)?;
        let path = profile_skill_path(id)?;
        let expected = self.workspace.token(&path)?;
        if expected.digest.is_none() {
            return if self.has_builtin(id)? {
                Err(SkillError::ImmutableBuiltIn)
            } else {
                Err(SkillError::NotFound)
            };
        }
        match self
            .workspace
            .delete(WorkspaceActor::SkillTool, &path, &expected, now)?
        {
            EditOutcome::Written(_) => {}
            EditOutcome::Conflict(_) => return Err(SkillError::Conflict),
        }
        let mut ledger = self.ledger()?;
        record_state_history(
            &mut ledger,
            id,
            SkillHistoryAction::Delete,
            now,
            self.limits.max_history_per_skill,
        )?;
        ledger.enabled.remove(id);
        persist_ledger(&self.workspace.layout().root, &ledger)
    }

    /// Returns effective/candidate provenance and persistent lifecycle history.
    ///
    /// # Errors
    ///
    /// Returns an error for malformed discovery roots or unknown skill IDs.
    pub fn inspect(&self, id: &str, now: UtcTimestamp) -> Result<SkillInspection, SkillError> {
        let _guard = self.lock()?;
        validate_skill_id(id)?;
        self.scan_workspace(now)?;
        let ledger = self.ledger()?;
        let mut candidates = self
            .discover_candidates(&ledger)?
            .into_iter()
            .filter(|package| package.manifest.id == id)
            .collect::<Vec<_>>();
        candidates.sort_by(|left, right| right.provenance.scope.cmp(&left.provenance.scope));
        if candidates.is_empty() && !ledger.history.contains_key(id) {
            return Err(SkillError::NotFound);
        }
        let enabled = ledger.enabled.get(id).copied().unwrap_or(true);
        let effective = enabled.then(|| candidates.first().cloned()).flatten();
        Ok(SkillInspection {
            id: id.into(),
            enabled,
            effective,
            candidates,
            history: ledger.history.get(id).cloned().unwrap_or_default(),
        })
    }

    fn set_enabled(
        &self,
        id: &str,
        enabled: bool,
        action: SkillHistoryAction,
        now: UtcTimestamp,
    ) -> Result<(), SkillError> {
        let _guard = self.lock()?;
        validate_skill_id(id)?;
        let mut ledger = self.ledger()?;
        if !self
            .discover_candidates(&ledger)?
            .iter()
            .any(|package| package.manifest.id == id)
        {
            return Err(SkillError::NotFound);
        }
        ledger.enabled.insert(id.into(), enabled);
        record_state_history(
            &mut ledger,
            id,
            action,
            now,
            self.limits.max_history_per_skill,
        )?;
        persist_ledger(&self.workspace.layout().root, &ledger)
    }

    fn discover_candidates(&self, ledger: &SkillLedger) -> Result<Vec<SkillPackage>, SkillError> {
        let mut candidates = Vec::new();
        for (scope, root, origin) in [
            (
                SkillScope::BuiltIn,
                &self.roots.built_in,
                "distribution built-in",
            ),
            (
                SkillScope::Global,
                &self.roots.global,
                "global installation",
            ),
            (
                SkillScope::Project,
                &self.roots.project,
                "project workspace",
            ),
            (
                SkillScope::Profile,
                &self.workspace.layout().skills,
                "profile workspace",
            ),
        ] {
            candidates.extend(discover_root(
                root,
                scope,
                origin,
                self.limits,
                ledger,
                &self.workspace.layout().root,
            )?);
        }
        Ok(candidates)
    }

    fn has_builtin(&self, id: &str) -> Result<bool, SkillError> {
        let ledger = self.ledger()?;
        Ok(discover_root(
            &self.roots.built_in,
            SkillScope::BuiltIn,
            "distribution built-in",
            self.limits,
            &ledger,
            &self.workspace.layout().root,
        )?
        .iter()
        .any(|package| package.manifest.id == id))
    }

    fn scan_workspace(&self, now: UtcTimestamp) -> Result<(), SkillError> {
        let events = self.workspace.scan_external_changes(now)?;
        if events
            .iter()
            .any(|event| matches!(event, WorkspaceEvent::Rejected { .. }))
        {
            Err(SkillError::InvalidPackage)
        } else {
            Ok(())
        }
    }

    fn ledger(&self) -> Result<MutexGuard<'_, SkillLedger>, SkillError> {
        self.ledger.lock().map_err(|_| SkillError::LockPoisoned)
    }

    fn lock(&self) -> Result<MutexGuard<'_, ()>, SkillError> {
        self.serial.lock().map_err(|_| SkillError::LockPoisoned)
    }
}

struct ParsedSkill {
    manifest: SkillManifest,
    body: String,
}

fn parse_skill(source: &str, max_bytes: usize) -> Result<ParsedSkill, SkillError> {
    if source.len() > max_bytes || !source.starts_with("+++\n") {
        return Err(SkillError::InvalidPackage);
    }
    let rest = &source[4..];
    let end = rest.find("\n+++\n").ok_or(SkillError::InvalidPackage)?;
    let manifest = toml::from_str::<SkillManifest>(&rest[..end])?;
    validate_manifest(&manifest)?;
    let body = rest[end + 5..].trim().to_owned();
    if body.is_empty() {
        return Err(SkillError::InvalidPackage);
    }
    Ok(ParsedSkill { manifest, body })
}

fn validate_manifest(manifest: &SkillManifest) -> Result<(), SkillError> {
    validate_skill_id(&manifest.id)?;
    let required = !manifest.version.trim().is_empty()
        && !manifest.description.trim().is_empty()
        && !manifest.triggers.is_empty()
        && !manifest.inputs.is_empty()
        && !manifest.steps.is_empty()
        && !manifest.validation.is_empty()
        && !manifest.known_failures.is_empty()
        && !manifest.stop_conditions.is_empty();
    if required && manifest.required_tools.iter().all(|tool| valid_name(tool)) {
        Ok(())
    } else {
        Err(SkillError::InvalidPackage)
    }
}

fn validate_skill_id(id: &str) -> Result<(), SkillError> {
    if valid_name(id) {
        Ok(())
    } else {
        Err(SkillError::InvalidPackage)
    }
}

fn valid_name(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 96
        && value
            .chars()
            .all(|character| character.is_ascii_alphanumeric() || matches!(character, '-' | '_'))
}

fn discover_root(
    root: &Path,
    scope: SkillScope,
    origin: &str,
    limits: SkillLimits,
    ledger: &SkillLedger,
    workspace_root: &Path,
) -> Result<Vec<SkillPackage>, SkillError> {
    let mut packages = Vec::new();
    for entry in fs::read_dir(root)? {
        let entry = entry?;
        if entry.file_type()?.is_symlink() || !entry.file_type()?.is_dir() {
            return Err(SkillError::InvalidPackage);
        }
        let path = entry.path().join("SKILL.md");
        let metadata = match fs::symlink_metadata(&path) {
            Ok(metadata) => metadata,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => continue,
            Err(error) => return Err(error.into()),
        };
        if metadata.file_type().is_symlink()
            || metadata.len() > u64::try_from(limits.max_skill_bytes).unwrap_or(u64::MAX)
        {
            return Err(SkillError::InvalidPackage);
        }
        let source = fs::read_to_string(&path)?;
        let parsed = parse_skill(&source, limits.max_skill_bytes)?;
        if entry.file_name().to_string_lossy() != parsed.manifest.id {
            return Err(SkillError::InvalidPackage);
        }
        let relative_path = if scope == SkillScope::Profile {
            path.strip_prefix(workspace_root)
                .map_err(|_| SkillError::InvalidPackage)?
                .to_path_buf()
        } else {
            path.clone()
        };
        let latest = ledger
            .history
            .get(&parsed.manifest.id)
            .and_then(|history| history.last());
        let effective_origin = if scope == SkillScope::Profile {
            ledger
                .origins
                .get(&parsed.manifest.id)
                .cloned()
                .unwrap_or_else(|| origin.into())
        } else {
            origin.into()
        };
        packages.push(package_from_parsed(
            parsed,
            source.clone(),
            SkillProvenance {
                scope,
                origin: effective_origin,
                source_path: relative_path,
                digest: digest(&source),
                installed_at: (scope == SkillScope::Profile)
                    .then(|| {
                        ledger
                            .installed_at
                            .get(&entry.file_name().to_string_lossy().into_owned())
                            .copied()
                    })
                    .flatten(),
                revision: (scope == SkillScope::Profile)
                    .then(|| latest.map(|entry| entry.revision))
                    .flatten(),
            },
        )?);
        if packages.len() > limits.max_skills_per_scope {
            return Err(SkillError::InvalidPackage);
        }
    }
    Ok(packages)
}

fn effective_packages(
    candidates: Vec<SkillPackage>,
    enabled: &BTreeMap<String, bool>,
) -> Vec<SkillPackage> {
    let mut effective = BTreeMap::<String, SkillPackage>::new();
    for package in candidates {
        let id = package.manifest.id.clone();
        if enabled.get(&id).copied() == Some(false) {
            continue;
        }
        let replace = effective.get(&id).is_none_or(|current| {
            package.provenance.scope > current.provenance.scope
                || (package.provenance.scope == current.provenance.scope
                    && package.provenance.source_path < current.provenance.source_path)
        });
        if replace {
            effective.insert(id, package);
        }
    }
    effective.into_values().collect()
}

fn package_from_parsed(
    parsed: ParsedSkill,
    source: String,
    provenance: SkillProvenance,
) -> Result<SkillPackage, SkillError> {
    if provenance.digest != digest(&source) {
        return Err(SkillError::InvalidPackage);
    }
    Ok(SkillPackage {
        manifest: parsed.manifest,
        body: parsed.body,
        source,
        provenance,
    })
}

fn profile_skill_path(id: &str) -> Result<PathBuf, SkillError> {
    validate_skill_id(id)?;
    Ok(PathBuf::from("skills").join(id).join("SKILL.md"))
}

fn ensure_skill_directory(root: &Path, id: &str) -> Result<(), SkillError> {
    validate_skill_id(id)?;
    let skills = root.join("skills");
    let target = skills.join(id);
    match fs::symlink_metadata(&target) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_dir() => {
            Err(SkillError::InvalidPackage)
        }
        Ok(_) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            fs::create_dir(&target)?;
            Ok(())
        }
        Err(error) => Err(error.into()),
    }
}

fn write_workspace_skill(
    workspace: &PersonalWorkspace,
    path: &Path,
    expected: &FileToken,
    source: &str,
    now: UtcTimestamp,
) -> Result<FileToken, SkillError> {
    match workspace.edit(
        WorkspaceActor::SkillTool,
        path,
        expected,
        source.as_bytes(),
        now,
    )? {
        EditOutcome::Written(version) => Ok(FileToken {
            revision: Some(version.revision),
            digest: version.digest,
        }),
        EditOutcome::Conflict(_) => Err(SkillError::Conflict),
    }
}

fn record_content_history(
    root: &Path,
    ledger: &mut SkillLedger,
    manifest: &SkillManifest,
    source: &str,
    action: SkillHistoryAction,
    now: UtcTimestamp,
    max_history: usize,
) -> Result<SkillHistoryEntry, SkillError> {
    let revision = advance_revision(ledger)?;
    let digest = digest(source);
    let relative = PathBuf::from(HISTORY_ROOT)
        .join(&manifest.id)
        .join(format!("{}-{digest}.md", revision.get()));
    write_immutable(&root.join(&relative), source.as_bytes())?;
    let entry = SkillHistoryEntry {
        revision,
        action,
        digest: Some(digest),
        version: Some(manifest.version.clone()),
        backup_path: Some(relative),
        at: now,
    };
    push_history(ledger, &manifest.id, entry.clone(), max_history);
    Ok(entry)
}

fn record_state_history(
    ledger: &mut SkillLedger,
    id: &str,
    action: SkillHistoryAction,
    now: UtcTimestamp,
    max_history: usize,
) -> Result<(), SkillError> {
    let revision = advance_revision(ledger)?;
    push_history(
        ledger,
        id,
        SkillHistoryEntry {
            revision,
            action,
            digest: None,
            version: None,
            backup_path: None,
            at: now,
        },
        max_history,
    );
    Ok(())
}

fn advance_revision(ledger: &mut SkillLedger) -> Result<Revision, SkillError> {
    ledger.next_revision = ledger
        .next_revision
        .checked_next()
        .ok_or(SkillError::UnsupportedSchema)?;
    Ok(ledger.next_revision)
}

fn push_history(ledger: &mut SkillLedger, id: &str, entry: SkillHistoryEntry, max_history: usize) {
    let history = ledger.history.entry(id.into()).or_default();
    history.push(entry);
    if history.len() > max_history {
        history.drain(..history.len() - max_history);
    }
}

fn persist_ledger(root: &Path, ledger: &SkillLedger) -> Result<(), SkillError> {
    let path = root.join(LEDGER_PATH);
    let temporary = path.with_extension(format!("{}.tmp", EntityId::new()));
    let mut file = OpenOptions::new()
        .create_new(true)
        .write(true)
        .open(&temporary)?;
    file.write_all(&keith_agent_types::canonical_json_bytes(ledger)?)?;
    file.sync_all()?;
    keith_platform::replace_file(&temporary, &path)?;
    File::open(path.parent().ok_or(SkillError::InvalidPackage)?)?.sync_all()?;
    Ok(())
}

fn write_immutable(path: &Path, bytes: &[u8]) -> Result<(), SkillError> {
    if let Some(parent) = path.parent() {
        create_safe_directories(parent)?;
    }
    let mut file = OpenOptions::new().create_new(true).write(true).open(path)?;
    file.write_all(bytes)?;
    file.sync_all()?;
    File::open(path.parent().ok_or(SkillError::InvalidPackage)?)?.sync_all()?;
    Ok(())
}

fn create_safe_directories(path: &Path) -> Result<(), SkillError> {
    let mut current = PathBuf::new();
    for component in path.components() {
        match component {
            Component::RootDir => current.push(Path::new("/")),
            Component::Normal(value) => current.push(value),
            _ => return Err(SkillError::InvalidPackage),
        }
        match fs::symlink_metadata(&current) {
            Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_dir() => {
                return Err(SkillError::InvalidPackage);
            }
            Ok(_) => {}
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
                fs::create_dir(&current)?;
            }
            Err(error) => return Err(error.into()),
        }
    }
    Ok(())
}

fn relevance(task: &str, manifest: &SkillManifest) -> f32 {
    let task_tokens = tokens(task);
    let mut score = 0.0_f32;
    let id = normalize(&manifest.id);
    if task.contains(&id) {
        score += 1.0;
    }
    for trigger in &manifest.triggers {
        let trigger = normalize(trigger);
        if task.contains(&trigger) {
            score += 1.0;
        } else {
            score += overlap(&task_tokens, &tokens(&trigger)) * 0.5;
        }
    }
    score + overlap(&task_tokens, &tokens(&normalize(&manifest.description))) * 0.25
}

fn render_prompt(package: &SkillPackage) -> String {
    format!(
        "<skill id=\"{}\" version=\"{}\">\n{}\n</skill>\n",
        package.manifest.id, package.manifest.version, package.source
    )
}

fn normalize(value: &str) -> String {
    value
        .chars()
        .flat_map(char::to_lowercase)
        .map(|character| {
            if character.is_alphanumeric() || matches!(character, '-' | '_') {
                character
            } else {
                ' '
            }
        })
        .collect::<String>()
        .split_whitespace()
        .collect::<Vec<_>>()
        .join(" ")
}

fn tokens(value: &str) -> BTreeSet<&str> {
    value.split_whitespace().collect()
}

fn overlap(left: &BTreeSet<&str>, right: &BTreeSet<&str>) -> f32 {
    if left.is_empty() || right.is_empty() {
        return 0.0;
    }
    let intersection = u16::try_from(left.intersection(right).count().min(usize::from(u16::MAX)))
        .unwrap_or(u16::MAX);
    let denominator = u16::try_from(right.len().min(usize::from(u16::MAX))).unwrap_or(u16::MAX);
    f32::from(intersection) / f32::from(denominator)
}

fn digest(source: &str) -> String {
    Sha256::digest(source.as_bytes())
        .iter()
        .fold(String::with_capacity(64), |mut output, byte| {
            write!(output, "{byte:02x}").expect("writing to a String cannot fail");
            output
        })
}

#[cfg(test)]
mod tests {
    use tempfile::tempdir;

    use super::*;

    fn skill(id: &str, version: &str, description: &str, trigger: &str, tool: &str) -> String {
        format!(
            "+++\nid = \"{id}\"\nversion = \"{version}\"\ndescription = \"{description}\"\ntriggers = [\"{trigger}\"]\ninputs = [\"request\"]\nsteps = [\"inspect\", \"execute\"]\nrequired_tools = [\"{tool}\"]\nvalidation = [\"verify result\"]\nknown_failures = [\"dependency unavailable\"]\nstop_conditions = [\"authority missing\"]\nplatforms = [\"linux\"]\n+++\n# {id}\nDeclarative procedure only.\n"
        )
    }

    fn write_package(root: &Path, id: &str, source: &str) {
        fs::create_dir_all(root.join(id)).unwrap();
        fs::write(root.join(id).join("SKILL.md"), source).unwrap();
    }

    fn registry(root: &Path) -> SkillRegistry {
        let workspace = PersonalWorkspace::open(
            root.join("workspace"),
            keith_workspace::PersonalWorkspaceLimits::default(),
            UtcTimestamp::UNIX_EPOCH,
        )
        .unwrap();
        let roots = SkillRoots {
            built_in: root.join("builtins"),
            global: root.join("global"),
            project: root.join("project"),
        };
        SkillRegistry::open(workspace, roots, SkillLimits::default()).unwrap()
    }

    #[test]
    fn precedence_selection_budget_readiness_and_irrelevance_are_deterministic() {
        let directory = tempdir().unwrap();
        let builtins = directory.path().join("builtins");
        let global = directory.path().join("global");
        let project = directory.path().join("project");
        write_package(
            &builtins,
            "release",
            &skill("release", "1", "builtin release", "deploy release", "shell"),
        );
        write_package(
            &global,
            "release",
            &skill("release", "2", "global release", "deploy release", "shell"),
        );
        write_package(
            &project,
            "release",
            &skill("release", "3", "project release", "deploy release", "shell"),
        );
        write_package(
            &project,
            "database",
            &skill(
                "database",
                "1",
                "database migration",
                "migrate schema",
                "sql",
            ),
        );
        let registry = registry(directory.path());
        let installed = registry
            .install(
                skill("release", "4", "profile release", "deploy release", "shell"),
                "user install",
                UtcTimestamp::UNIX_EPOCH,
            )
            .unwrap();
        assert_eq!(installed.provenance.scope, SkillScope::Profile);
        let discovered = registry
            .discover(UtcTimestamp::from_unix_millis(1))
            .unwrap();
        assert_eq!(
            discovered
                .iter()
                .find(|package| package.manifest.id == "release")
                .unwrap()
                .manifest
                .version,
            "4"
        );
        let selected = registry
            .select(
                &SkillSelectionRequest {
                    task: "deploy the release".into(),
                    platform: "linux".into(),
                    ready_tools: BTreeSet::from(["shell".into()]),
                    max_prompt_bytes: 4_096,
                    max_skills: 1,
                },
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        assert_eq!(selected.selected[0].id, "release");
        assert!(
            selected.selected[0]
                .prompt
                .contains("Declarative procedure")
        );
        assert!(
            selected
                .excluded
                .iter()
                .any(|skill| skill.id == "database" && skill.reason.contains("unavailable"))
        );
    }

    #[test]
    fn lifecycle_history_disable_rollback_delete_and_restart_are_inspectable() {
        let directory = tempdir().unwrap();
        let builtins = directory.path().join("builtins");
        write_package(
            &builtins,
            "core",
            &skill("core", "1", "immutable core", "core task", "read"),
        );
        let initial_registry = registry(directory.path());
        assert!(matches!(
            initial_registry.delete("core", UtcTimestamp::UNIX_EPOCH),
            Err(SkillError::ImmutableBuiltIn)
        ));
        let first = initial_registry
            .install(
                skill("deploy", "1", "deploy service", "deploy", "shell"),
                "local package",
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        let first_revision = first.provenance.revision.unwrap();
        let second = initial_registry
            .update(
                "deploy",
                &first.provenance.digest,
                skill("deploy", "2", "deploy safely", "deploy", "shell"),
                UtcTimestamp::from_unix_millis(2),
            )
            .unwrap();
        initial_registry
            .disable("deploy", UtcTimestamp::from_unix_millis(3))
            .unwrap();
        assert!(
            initial_registry
                .discover(UtcTimestamp::from_unix_millis(4))
                .unwrap()
                .iter()
                .all(|package| package.manifest.id != "deploy")
        );
        drop(initial_registry);
        let reopened = registry(directory.path());
        assert!(
            !reopened
                .inspect("deploy", UtcTimestamp::from_unix_millis(5))
                .unwrap()
                .enabled
        );
        reopened
            .enable("deploy", UtcTimestamp::from_unix_millis(6))
            .unwrap();
        let rolled = reopened
            .rollback("deploy", first_revision, UtcTimestamp::from_unix_millis(7))
            .unwrap();
        assert_eq!(rolled.manifest.version, "1");
        assert_ne!(rolled.provenance.digest, second.provenance.digest);
        reopened
            .delete("deploy", UtcTimestamp::from_unix_millis(8))
            .unwrap();
        let inspection = reopened
            .inspect("deploy", UtcTimestamp::from_unix_millis(9))
            .unwrap();
        assert!(inspection.effective.is_none());
        assert!(
            inspection
                .history
                .iter()
                .any(|entry| entry.action == SkillHistoryAction::Rollback)
        );
        assert!(
            inspection
                .history
                .iter()
                .any(|entry| entry.action == SkillHistoryAction::Delete)
        );
        assert!(
            fs::read_to_string(builtins.join("core/SKILL.md"))
                .unwrap()
                .contains("immutable core")
        );
    }
}
