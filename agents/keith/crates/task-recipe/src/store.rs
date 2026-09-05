use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, OpenOptions};
use std::io::{Read, Write};
use std::path::{Path, PathBuf};

use keith_agent_types::{EntityId, UtcTimestamp};
use keith_platform_contracts::{DemonstrationId, RecipeId};
use serde::{Deserialize, Serialize, de::DeserializeOwned};
use sha2::{Digest, Sha256};

use crate::{
    Demonstration, DemonstrationEventKind, DemonstrationState, MediaReference, MediaSanitization,
    TaskRecipeError, TaskRecipeHistory, valid_digest,
};

const DEMONSTRATIONS_DIR: &str = "demonstrations";
const RECIPES_DIR: &str = "recipes";
const MEDIA_DIR: &str = "media";

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct StoreLimits {
    pub max_record_bytes: usize,
    pub max_media_bytes: usize,
    pub max_export_bytes: usize,
}

impl Default for StoreLimits {
    fn default() -> Self {
        Self {
            max_record_bytes: 64 * 1_024 * 1_024,
            max_media_bytes: 32 * 1_024 * 1_024,
            max_export_bytes: 256 * 1_024 * 1_024,
        }
    }
}

impl StoreLimits {
    fn validate(self) -> Result<(), TaskRecipeError> {
        if self.max_record_bytes == 0
            || self.max_media_bytes == 0
            || self.max_export_bytes < self.max_record_bytes
        {
            return Err(TaskRecipeError::LimitExceeded(
                "task recipe store limits are invalid".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug)]
pub struct TaskRecipeStore {
    root: PathBuf,
    limits: StoreLimits,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ExportedMedia {
    pub reference: MediaReference,
    pub bytes: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DemonstrationExport {
    pub demonstration: Demonstration,
    pub derived_recipes: Vec<TaskRecipeHistory>,
    pub media: Vec<ExportedMedia>,
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DeletionReport {
    pub demonstration_removed: bool,
    pub removed_recipe_ids: Vec<RecipeId>,
    pub removed_media_digests: Vec<String>,
    pub retained_shared_media_digests: Vec<String>,
}

impl TaskRecipeStore {
    pub fn open(root: impl Into<PathBuf>, limits: StoreLimits) -> Result<Self, TaskRecipeError> {
        limits.validate()?;
        let root = root.into();
        ensure_directory(&root)?;
        for directory in [DEMONSTRATIONS_DIR, RECIPES_DIR, MEDIA_DIR] {
            ensure_directory(&root.join(directory))?;
        }
        Ok(Self { root, limits })
    }

    pub fn root(&self) -> &Path {
        &self.root
    }

    pub fn put_media(
        &self,
        bytes: &[u8],
        media_type: impl Into<String>,
        sanitization: MediaSanitization,
    ) -> Result<MediaReference, TaskRecipeError> {
        if bytes.is_empty() || bytes.len() > self.limits.max_media_bytes {
            return Err(TaskRecipeError::LimitExceeded(
                "media is empty or oversized".into(),
            ));
        }
        let media_type = media_type.into();
        if !valid_media_type(&media_type) {
            return Err(TaskRecipeError::InvalidDemonstration(
                "media type is malformed".into(),
            ));
        }
        let digest = hex_digest(bytes);
        let path = self.media_path(&digest)?;
        if path.exists() {
            let existing = read_bounded(&path, self.limits.max_media_bytes)?;
            if existing != bytes {
                return Err(TaskRecipeError::InvalidDemonstration(
                    "media digest collision or corrupt media".into(),
                ));
            }
        } else {
            atomic_write(&path, bytes)?;
        }
        Ok(MediaReference {
            digest,
            media_type,
            byte_len: u64::try_from(bytes.len())
                .map_err(|_| TaskRecipeError::LimitExceeded("media byte count overflow".into()))?,
            sanitization,
        })
    }

    pub fn save_demonstration(&self, demonstration: &Demonstration) -> Result<(), TaskRecipeError> {
        demonstration.validate()?;
        for reference in demonstration_media(demonstration).values() {
            self.verify_media(reference)?;
        }
        let bytes = serde_json::to_vec_pretty(demonstration)?;
        self.write_record(&self.demonstration_path(&demonstration.id), &bytes)
    }

    pub fn load_demonstration(
        &self,
        id: &DemonstrationId,
    ) -> Result<Demonstration, TaskRecipeError> {
        let demonstration = self.read_record(&self.demonstration_path(id))?;
        Demonstration::validate(&demonstration)?;
        Ok(demonstration)
    }

    pub fn save_recipe_history(&self, history: &TaskRecipeHistory) -> Result<(), TaskRecipeError> {
        history.validate()?;
        let demonstration = self.load_demonstration(&history.active()?.source_demonstration_id)?;
        if demonstration.state != DemonstrationState::Completed {
            return Err(TaskRecipeError::InvalidRecipe(
                "recipe source demonstration is not complete".into(),
            ));
        }
        let source_media = demonstration_media(&demonstration);
        for digest in recipe_media(history) {
            let reference = source_media.get(&digest).ok_or_else(|| {
                TaskRecipeError::InvalidRecipe(
                    "visual fallback does not belong to the source demonstration".into(),
                )
            })?;
            self.verify_media(reference)?;
        }
        let bytes = serde_json::to_vec_pretty(history)?;
        self.write_record(&self.recipe_path(&history.recipe_id), &bytes)
    }

    pub fn load_recipe_history(&self, id: &RecipeId) -> Result<TaskRecipeHistory, TaskRecipeError> {
        let history = self.read_record(&self.recipe_path(id))?;
        TaskRecipeHistory::validate(&history)?;
        Ok(history)
    }

    pub fn export_demonstration(&self, id: &DemonstrationId) -> Result<Vec<u8>, TaskRecipeError> {
        let demonstration = self.load_demonstration(id)?;
        let derived_recipes = self
            .all_recipe_histories()?
            .into_iter()
            .filter(|history| {
                history
                    .active()
                    .is_ok_and(|recipe| &recipe.source_demonstration_id == id)
            })
            .collect::<Vec<_>>();
        let references = demonstration_media(&demonstration);
        for history in &derived_recipes {
            for digest in recipe_media(history) {
                if !references.contains_key(&digest) {
                    return Err(TaskRecipeError::InvalidRecipe(
                        "derived recipe refers to media outside its demonstration".into(),
                    ));
                }
            }
        }
        let mut media = Vec::with_capacity(references.len());
        for reference in references.into_values() {
            let bytes = read_bounded(
                &self.media_path(&reference.digest)?,
                self.limits.max_media_bytes,
            )?;
            media.push(ExportedMedia { reference, bytes });
        }
        let bytes = serde_json::to_vec_pretty(&DemonstrationExport {
            demonstration,
            derived_recipes,
            media,
        })?;
        if bytes.len() > self.limits.max_export_bytes {
            return Err(TaskRecipeError::LimitExceeded(
                "demonstration export byte ceiling exhausted".into(),
            ));
        }
        Ok(bytes)
    }

    pub fn delete_demonstration(
        &self,
        id: &DemonstrationId,
    ) -> Result<DeletionReport, TaskRecipeError> {
        let demonstration = self.load_demonstration(id)?;
        let candidate_media = demonstration_media(&demonstration)
            .into_keys()
            .collect::<BTreeSet<_>>();
        let derived = self
            .all_recipe_histories()?
            .into_iter()
            .filter(|history| {
                history
                    .active()
                    .is_ok_and(|recipe| &recipe.source_demonstration_id == id)
            })
            .collect::<Vec<_>>();
        let mut candidate_media = candidate_media;
        let mut removed_recipe_ids = Vec::with_capacity(derived.len());
        for history in &derived {
            candidate_media.extend(recipe_media(history));
            remove_existing_file(&self.recipe_path(&history.recipe_id))?;
            removed_recipe_ids.push(history.recipe_id.clone());
        }
        remove_existing_file(&self.demonstration_path(id))?;
        let live_media = self.live_media_digests()?;
        let mut removed_media_digests = Vec::new();
        let mut retained_shared_media_digests = Vec::new();
        for digest in candidate_media {
            if live_media.contains(&digest) {
                retained_shared_media_digests.push(digest);
            } else {
                remove_existing_file(&self.media_path(&digest)?)?;
                removed_media_digests.push(digest);
            }
        }
        Ok(DeletionReport {
            demonstration_removed: true,
            removed_recipe_ids,
            removed_media_digests,
            retained_shared_media_digests,
        })
    }

    pub fn delete_recipe(&self, id: &RecipeId) -> Result<Vec<String>, TaskRecipeError> {
        let history = self.load_recipe_history(id)?;
        let candidates = recipe_media(&history);
        remove_existing_file(&self.recipe_path(id))?;
        let live_media = self.live_media_digests()?;
        let mut removed = Vec::new();
        for digest in candidates.difference(&live_media) {
            remove_existing_file(&self.media_path(digest)?)?;
            removed.push(digest.clone());
        }
        Ok(removed)
    }

    pub fn prune_expired(&self, now: UtcTimestamp) -> Result<Vec<DeletionReport>, TaskRecipeError> {
        let expired = self
            .all_demonstrations()?
            .into_iter()
            .filter(|demonstration| demonstration.retention.retain_until <= now)
            .map(|demonstration| demonstration.id)
            .collect::<Vec<_>>();
        expired
            .iter()
            .map(|id| self.delete_demonstration(id))
            .collect()
    }

    fn demonstration_path(&self, id: &DemonstrationId) -> PathBuf {
        self.root
            .join(DEMONSTRATIONS_DIR)
            .join(format!("{id}.json"))
    }

    fn recipe_path(&self, id: &RecipeId) -> PathBuf {
        self.root.join(RECIPES_DIR).join(format!("{id}.json"))
    }

    fn media_path(&self, digest: &str) -> Result<PathBuf, TaskRecipeError> {
        if !valid_digest(digest) {
            return Err(TaskRecipeError::InvalidDemonstration(
                "media digest is malformed".into(),
            ));
        }
        Ok(self.root.join(MEDIA_DIR).join(format!("{digest}.bin")))
    }

    fn verify_media(&self, reference: &MediaReference) -> Result<(), TaskRecipeError> {
        reference.validate()?;
        let bytes = read_bounded(
            &self.media_path(&reference.digest)?,
            self.limits.max_media_bytes,
        )?;
        if u64::try_from(bytes.len()).ok() != Some(reference.byte_len)
            || hex_digest(&bytes) != reference.digest
        {
            return Err(TaskRecipeError::InvalidDemonstration(
                "persisted media does not match its reference".into(),
            ));
        }
        Ok(())
    }

    fn write_record(&self, path: &Path, bytes: &[u8]) -> Result<(), TaskRecipeError> {
        if bytes.len() > self.limits.max_record_bytes {
            return Err(TaskRecipeError::LimitExceeded(
                "record byte ceiling exhausted".into(),
            ));
        }
        atomic_write(path, bytes)
    }

    fn read_record<T: DeserializeOwned>(&self, path: &Path) -> Result<T, TaskRecipeError> {
        if !path.is_file() {
            return Err(TaskRecipeError::NotFound);
        }
        Ok(serde_json::from_slice(&read_bounded(
            path,
            self.limits.max_record_bytes,
        )?)?)
    }

    fn all_demonstrations(&self) -> Result<Vec<Demonstration>, TaskRecipeError> {
        self.read_record_directory(&self.root.join(DEMONSTRATIONS_DIR))
    }

    fn all_recipe_histories(&self) -> Result<Vec<TaskRecipeHistory>, TaskRecipeError> {
        self.read_record_directory(&self.root.join(RECIPES_DIR))
    }

    fn read_record_directory<T: DeserializeOwned>(
        &self,
        directory: &Path,
    ) -> Result<Vec<T>, TaskRecipeError> {
        let mut paths = fs::read_dir(directory)?
            .map(|entry| entry.map(|entry| entry.path()))
            .collect::<Result<Vec<_>, _>>()?;
        paths.sort();
        paths
            .iter()
            .filter(|path| {
                path.extension()
                    .is_some_and(|extension| extension == "json")
            })
            .map(|path| self.read_record(path))
            .collect()
    }

    fn live_media_digests(&self) -> Result<BTreeSet<String>, TaskRecipeError> {
        let mut live = BTreeSet::new();
        for demonstration in self.all_demonstrations()? {
            live.extend(demonstration_media(&demonstration).into_keys());
        }
        for history in self.all_recipe_histories()? {
            live.extend(recipe_media(&history));
        }
        Ok(live)
    }
}

fn demonstration_media(demonstration: &Demonstration) -> BTreeMap<String, MediaReference> {
    let mut references = BTreeMap::new();
    for event in demonstration.events() {
        if let Some(frame) = &event.context.frame {
            references.insert(frame.media.digest.clone(), frame.media.clone());
        }
        match &event.kind {
            DemonstrationEventKind::FrameCaptured(frame) => {
                references.insert(frame.media.digest.clone(), frame.media.clone());
            }
            DemonstrationEventKind::File {
                media: Some(media), ..
            } => {
                references.insert(media.digest.clone(), media.clone());
            }
            DemonstrationEventKind::Pointer(_)
            | DemonstrationEventKind::Keyboard(_)
            | DemonstrationEventKind::Clipboard { .. }
            | DemonstrationEventKind::File { media: None, .. }
            | DemonstrationEventKind::Pause { .. }
            | DemonstrationEventKind::Resume
            | DemonstrationEventKind::Narration(_)
            | DemonstrationEventKind::ControlChanged(_)
            | DemonstrationEventKind::Navigate { .. }
            | DemonstrationEventKind::Wait { .. } => {}
        }
    }
    references
}

fn recipe_media(history: &TaskRecipeHistory) -> BTreeSet<String> {
    history
        .versions()
        .iter()
        .flat_map(|recipe| &recipe.steps)
        .filter_map(|step| match &step.action {
            crate::RecipeAction::Activate { target }
            | crate::RecipeAction::EnterText { target, .. }
            | crate::RecipeAction::Select { target, .. }
            | crate::RecipeAction::Upload { target, .. }
            | crate::RecipeAction::Download { target } => target.visual_fallback.as_ref(),
            crate::RecipeAction::Navigate { .. }
            | crate::RecipeAction::Shortcut { .. }
            | crate::RecipeAction::Wait { .. } => None,
        })
        .map(|fallback| fallback.source_frame_digest.clone())
        .collect()
}

fn ensure_directory(path: &Path) -> Result<(), TaskRecipeError> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.file_type().is_symlink() || !metadata.is_dir() => Err(
            TaskRecipeError::Io(std::io::Error::other("store path is not a safe directory")),
        ),
        Ok(_) => Ok(()),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => {
            fs::create_dir_all(path)?;
            let metadata = fs::symlink_metadata(path)?;
            if metadata.file_type().is_symlink() || !metadata.is_dir() {
                return Err(TaskRecipeError::Io(std::io::Error::other(
                    "store path became unsafe",
                )));
            }
            Ok(())
        }
        Err(error) => Err(error.into()),
    }
}

fn valid_media_type(value: &str) -> bool {
    value.len() <= 128
        && value.split_once('/').is_some_and(|(kind, subtype)| {
            !kind.is_empty()
                && !subtype.is_empty()
                && value.bytes().all(|byte| {
                    byte.is_ascii_alphanumeric() || matches!(byte, b'/' | b'-' | b'+' | b'.')
                })
        })
}

fn hex_digest(bytes: &[u8]) -> String {
    Sha256::digest(bytes)
        .iter()
        .fold(String::with_capacity(64), |mut output, byte| {
            use std::fmt::Write as _;
            write!(output, "{byte:02x}").expect("writing to a String cannot fail");
            output
        })
}

fn read_bounded(path: &Path, max_bytes: usize) -> Result<Vec<u8>, TaskRecipeError> {
    let metadata = fs::symlink_metadata(path)?;
    if metadata.file_type().is_symlink()
        || !metadata.is_file()
        || metadata.len() > u64::try_from(max_bytes).unwrap_or(u64::MAX)
    {
        return Err(TaskRecipeError::LimitExceeded(
            "stored file is unsafe or oversized".into(),
        ));
    }
    let mut bytes = Vec::with_capacity(usize::try_from(metadata.len()).unwrap_or(max_bytes));
    fs::File::open(path)?
        .take(
            u64::try_from(max_bytes)
                .unwrap_or(u64::MAX)
                .saturating_add(1),
        )
        .read_to_end(&mut bytes)?;
    if bytes.len() > max_bytes {
        return Err(TaskRecipeError::LimitExceeded(
            "stored file byte ceiling exhausted".into(),
        ));
    }
    Ok(bytes)
}

fn atomic_write(path: &Path, bytes: &[u8]) -> Result<(), TaskRecipeError> {
    let parent = path
        .parent()
        .ok_or_else(|| TaskRecipeError::Io(std::io::Error::other("record path has no parent")))?;
    ensure_directory(parent)?;
    let temporary = parent.join(format!(".{}.tmp", EntityId::new()));
    let result = (|| {
        let mut file = OpenOptions::new()
            .write(true)
            .create_new(true)
            .open(&temporary)?;
        file.write_all(bytes)?;
        file.sync_all()?;
        fs::rename(&temporary, path)?;
        fs::File::open(parent)?.sync_all()?;
        Ok::<(), std::io::Error>(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result.map_err(TaskRecipeError::from)
}

fn remove_existing_file(path: &Path) -> Result<(), TaskRecipeError> {
    match fs::symlink_metadata(path) {
        Ok(metadata) if metadata.is_file() && !metadata.file_type().is_symlink() => {
            fs::remove_file(path)?;
            if let Some(parent) = path.parent() {
                fs::File::open(parent)?.sync_all()?;
            }
            Ok(())
        }
        Ok(_) => Err(TaskRecipeError::Io(std::io::Error::other(
            "refusing to remove unsafe record path",
        ))),
        Err(error) if error.kind() == std::io::ErrorKind::NotFound => Ok(()),
        Err(error) => Err(error.into()),
    }
}
