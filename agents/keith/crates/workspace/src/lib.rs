#![forbid(unsafe_code)]

mod model;
mod service;

pub use model::*;
pub use service::*;

use std::collections::BTreeSet;
use std::fs;
use std::path::{Path, PathBuf};

use keith_agent_types::{ProfileId, UtcTimestamp};
use sha2::{Digest, Sha256};

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileWorkspaceProvision {
    pub profile_id: ProfileId,
    pub root: PathBuf,
    pub created: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileWorkspaceDisposition {
    pub profile_id: ProfileId,
    pub root: PathBuf,
    pub retained_shared: BTreeSet<PathBuf>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileWorkspaceEraseReport {
    pub profile_id: ProfileId,
    pub removed: bool,
    pub retained_shared: BTreeSet<PathBuf>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileWorkspaceInventoryEntry {
    pub relative_path: PathBuf,
    pub bytes: u64,
    pub digest_sha256: String,
    pub retained_shared: bool,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileWorkspaceDeletionInventory {
    pub profile_id: ProfileId,
    pub root: PathBuf,
    pub context_revision: keith_agent_types::Revision,
    pub stable_key: String,
    pub entries: Vec<ProfileWorkspaceInventoryEntry>,
    pub retained_shared: BTreeSet<PathBuf>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ProfileWorkspaceLeakScan {
    pub profile_id: ProfileId,
    pub unexpected_paths: Vec<PathBuf>,
    pub retained_paths: Vec<PathBuf>,
}

pub struct ProfileWorkspaceProvisioner {
    profiles_root: PathBuf,
    limits: PersonalWorkspaceLimits,
}

impl ProfileWorkspaceProvisioner {
    /// # Errors
    /// Returns an error unless the profile resource root can be created and canonicalized.
    pub fn new(
        profiles_root: impl AsRef<Path>,
        limits: PersonalWorkspaceLimits,
    ) -> Result<Self, PersonalWorkspaceError> {
        fs::create_dir_all(profiles_root.as_ref())?;
        Ok(Self {
            profiles_root: fs::canonicalize(profiles_root.as_ref())?,
            limits,
        })
    }

    /// Idempotently creates or reconciles the profile's isolated workspace.
    /// # Errors
    /// Rolls back a newly-created root when workspace validation or initialization fails.
    pub fn provision(
        &self,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<(PersonalWorkspace, ProfileWorkspaceProvision), PersonalWorkspaceError> {
        let root = self.root_for(profile_id);
        let created = !root.exists();
        match PersonalWorkspace::open(&root, self.limits, now) {
            Ok(workspace) => Ok((
                workspace,
                ProfileWorkspaceProvision {
                    profile_id: profile_id.clone(),
                    root,
                    created,
                },
            )),
            Err(error) => {
                if created && root.exists() {
                    fs::remove_dir_all(&root)?;
                }
                Err(error)
            }
        }
    }

    /// # Errors
    /// Returns an error when the existing workspace is corrupt or isolated under another root.
    pub fn reconcile(
        &self,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<PersonalWorkspace, PersonalWorkspaceError> {
        self.provision(profile_id, now)
            .map(|(workspace, _)| workspace)
    }

    /// Removes only a root created by the matching, uncommitted provisioning token.
    /// # Errors
    /// Returns an error for a mismatched token or failed filesystem rollback.
    pub fn rollback(
        &self,
        provision: &ProfileWorkspaceProvision,
    ) -> Result<(), PersonalWorkspaceError> {
        if !provision.created || provision.root != self.root_for(&provision.profile_id) {
            return Err(PersonalWorkspaceError::UnsafePath);
        }
        if provision.root.exists() {
            fs::remove_dir_all(&provision.root)?;
        }
        Ok(())
    }

    /// # Errors
    /// Rejects retained paths outside the exact profile workspace.
    pub fn inspect_disposition(
        &self,
        profile_id: &ProfileId,
        retained_shared: BTreeSet<PathBuf>,
    ) -> Result<ProfileWorkspaceDisposition, PersonalWorkspaceError> {
        if retained_shared.iter().any(|path| {
            path.is_absolute()
                || path
                    .components()
                    .any(|part| matches!(part, std::path::Component::ParentDir))
        }) {
            return Err(PersonalWorkspaceError::UnsafePath);
        }
        Ok(ProfileWorkspaceDisposition {
            profile_id: profile_id.clone(),
            root: self.root_for(profile_id),
            retained_shared,
        })
    }

    pub fn root_for(&self, profile_id: &ProfileId) -> PathBuf {
        self.profiles_root.join(profile_id.to_string())
    }

    /// Erases the exact private workspace only when the disposition has no retained shared data.
    /// # Errors
    /// Rejects a stale/mismatched plan or a plan requiring shared-data retention.
    pub fn erase_private(
        &self,
        disposition: &ProfileWorkspaceDisposition,
    ) -> Result<ProfileWorkspaceEraseReport, PersonalWorkspaceError> {
        if disposition.root != self.root_for(&disposition.profile_id)
            || !disposition.retained_shared.is_empty()
        {
            return Err(PersonalWorkspaceError::UnsafePath);
        }
        let removed = disposition.root.exists();
        if removed {
            fs::remove_dir_all(&disposition.root)?;
        }
        Ok(ProfileWorkspaceEraseReport {
            profile_id: disposition.profile_id.clone(),
            removed,
            retained_shared: disposition.retained_shared.clone(),
        })
    }

    /// Enumerates every regular file under the exact profile root and binds it to one revision.
    /// # Errors
    /// Rejects unsafe retained paths, symlinks, and unreadable or corrupt workspaces.
    pub fn enumerate_deletion_inventory(
        &self,
        profile_id: &ProfileId,
        retained_shared: BTreeSet<PathBuf>,
        now: UtcTimestamp,
    ) -> Result<ProfileWorkspaceDeletionInventory, PersonalWorkspaceError> {
        let disposition = self.inspect_disposition(profile_id, retained_shared.clone())?;
        let workspace = PersonalWorkspace::open(&disposition.root, self.limits, now)?;
        let context_revision = workspace.context_revision()?;
        let paths = regular_files(&disposition.root)?;
        let mut entries = Vec::with_capacity(paths.len());
        for relative_path in paths {
            let bytes = fs::read(disposition.root.join(&relative_path))?;
            entries.push(ProfileWorkspaceInventoryEntry {
                retained_shared: retained_shared.iter().any(|retained| {
                    relative_path == *retained || relative_path.starts_with(retained)
                }),
                relative_path,
                bytes: u64::try_from(bytes.len())
                    .map_err(|_| PersonalWorkspaceError::LimitExceeded)?,
                digest_sha256: hex_digest(&bytes),
            });
        }
        let stable_key = workspace_inventory_key(profile_id, context_revision, &entries);
        Ok(ProfileWorkspaceDeletionInventory {
            profile_id: profile_id.clone(),
            root: disposition.root,
            context_revision,
            stable_key,
            entries,
            retained_shared,
        })
    }

    /// Erases all non-retained files only if a fresh inventory exactly matches the plan.
    /// # Errors
    /// Rejects stale inventories, unsafe paths, symlinks, or filesystem failures.
    pub fn erase_inventory(
        &self,
        inventory: &ProfileWorkspaceDeletionInventory,
        now: UtcTimestamp,
    ) -> Result<ProfileWorkspaceEraseReport, PersonalWorkspaceError> {
        if !inventory.root.exists() {
            return Ok(ProfileWorkspaceEraseReport {
                profile_id: inventory.profile_id.clone(),
                removed: false,
                retained_shared: inventory.retained_shared.clone(),
            });
        }
        let terminal = self.leak_scan(inventory)?;
        if terminal.unexpected_paths.is_empty() {
            return Ok(ProfileWorkspaceEraseReport {
                profile_id: inventory.profile_id.clone(),
                removed: false,
                retained_shared: inventory.retained_shared.clone(),
            });
        }
        let current = self.enumerate_deletion_inventory(
            &inventory.profile_id,
            inventory.retained_shared.clone(),
            now,
        )?;
        if current.stable_key != inventory.stable_key
            || current.context_revision != inventory.context_revision
            || current.entries != inventory.entries
        {
            return Err(PersonalWorkspaceError::Corrupt(
                "deletion inventory changed".into(),
            ));
        }
        for entry in &inventory.entries {
            if !entry.retained_shared {
                fs::remove_file(inventory.root.join(&entry.relative_path))?;
            }
        }
        remove_empty_directories(&inventory.root)?;
        Ok(ProfileWorkspaceEraseReport {
            profile_id: inventory.profile_id.clone(),
            removed: !inventory.root.exists(),
            retained_shared: inventory.retained_shared.clone(),
        })
    }

    /// # Errors
    /// Returns an error when remaining paths cannot be safely enumerated.
    pub fn leak_scan(
        &self,
        inventory: &ProfileWorkspaceDeletionInventory,
    ) -> Result<ProfileWorkspaceLeakScan, PersonalWorkspaceError> {
        let paths = if inventory.root.exists() {
            regular_files(&inventory.root)?
        } else {
            Vec::new()
        };
        let (retained_paths, unexpected_paths) = paths.into_iter().partition(|path| {
            inventory
                .retained_shared
                .iter()
                .any(|retained| path == retained || path.starts_with(retained))
        });
        Ok(ProfileWorkspaceLeakScan {
            profile_id: inventory.profile_id.clone(),
            unexpected_paths,
            retained_paths,
        })
    }
}

fn regular_files(root: &Path) -> Result<Vec<PathBuf>, PersonalWorkspaceError> {
    let mut pending = vec![root.to_path_buf()];
    let mut files = Vec::new();
    while let Some(directory) = pending.pop() {
        for entry in fs::read_dir(directory)? {
            let entry = entry?;
            let file_type = entry.file_type()?;
            if file_type.is_symlink() {
                return Err(PersonalWorkspaceError::Symlink);
            }
            if file_type.is_dir() {
                pending.push(entry.path());
            } else if file_type.is_file() {
                files.push(
                    entry
                        .path()
                        .strip_prefix(root)
                        .map_err(|_| PersonalWorkspaceError::UnsafePath)?
                        .to_path_buf(),
                );
            }
        }
    }
    files.sort();
    Ok(files)
}

fn remove_empty_directories(root: &Path) -> Result<(), PersonalWorkspaceError> {
    let mut directories = Vec::new();
    let mut pending = vec![root.to_path_buf()];
    while let Some(directory) = pending.pop() {
        directories.push(directory.clone());
        for entry in fs::read_dir(&directory)? {
            let entry = entry?;
            if entry.file_type()?.is_dir() {
                pending.push(entry.path());
            }
        }
    }
    directories.sort_by_key(|path| std::cmp::Reverse(path.components().count()));
    for directory in directories {
        if fs::read_dir(&directory)?.next().is_none() {
            fs::remove_dir(directory)?;
        }
    }
    Ok(())
}

fn workspace_inventory_key(
    profile_id: &ProfileId,
    revision: keith_agent_types::Revision,
    entries: &[ProfileWorkspaceInventoryEntry],
) -> String {
    let mut digest = Sha256::new();
    digest.update(profile_id.to_string());
    digest.update(revision.get().to_be_bytes());
    for entry in entries {
        digest.update(entry.relative_path.to_string_lossy().as_bytes());
        digest.update(entry.bytes.to_be_bytes());
        digest.update(entry.digest_sha256.as_bytes());
        digest.update([u8::from(entry.retained_shared)]);
    }
    format!("workspace-delete:{}", hex_bytes(&digest.finalize()))
}

fn hex_digest(bytes: &[u8]) -> String {
    hex_bytes(&Sha256::digest(bytes))
}
fn hex_bytes(bytes: &[u8]) -> String {
    bytes
        .iter()
        .fold(String::with_capacity(bytes.len() * 2), |mut value, byte| {
            use std::fmt::Write as _;
            let _ = write!(value, "{byte:02x}");
            value
        })
}

#[cfg(test)]
mod agent_lifecycle_tests {
    use super::*;
    use keith_agent_types::EntityId;

    #[test]
    fn agent_lifecycle_provision_is_isolated_idempotent_and_rollback_scoped() {
        let directory = tempfile::tempdir().unwrap();
        let provisioner =
            ProfileWorkspaceProvisioner::new(directory.path(), PersonalWorkspaceLimits::default())
                .unwrap();
        let first = ProfileId::from(EntityId::from_u128(1));
        let second = ProfileId::from(EntityId::from_u128(2));
        let (_, created) = provisioner.provision(&first, UtcTimestamp(1)).unwrap();
        let (_, replay) = provisioner.provision(&first, UtcTimestamp(2)).unwrap();
        let (_, other) = provisioner.provision(&second, UtcTimestamp(2)).unwrap();
        assert!(created.created);
        assert!(!replay.created);
        assert_ne!(replay.root, other.root);
        provisioner.rollback(&other).unwrap();
        assert!(replay.root.exists());
        assert!(!other.root.exists());
    }

    #[test]
    fn agent_lifecycle_deletion_inventory_retains_shared_and_detects_leaks() {
        let directory = tempfile::tempdir().unwrap();
        let provisioner =
            ProfileWorkspaceProvisioner::new(directory.path(), PersonalWorkspaceLimits::default())
                .unwrap();
        let profile = ProfileId::from(EntityId::from_u128(3));
        let (workspace, _) = provisioner.provision(&profile, UtcTimestamp(1)).unwrap();
        fs::write(workspace.layout().knowledge.join("shared.txt"), b"shared").unwrap();
        fs::write(workspace.layout().state.join("private.txt"), b"private").unwrap();
        let inventory = provisioner
            .enumerate_deletion_inventory(
                &profile,
                BTreeSet::from([PathBuf::from("knowledge/shared.txt")]),
                UtcTimestamp(2),
            )
            .unwrap();
        provisioner
            .erase_inventory(&inventory, UtcTimestamp(3))
            .unwrap();
        assert!(
            !provisioner
                .erase_inventory(&inventory, UtcTimestamp(4))
                .unwrap()
                .removed
        );
        let scan = provisioner.leak_scan(&inventory).unwrap();
        assert!(scan.unexpected_paths.is_empty());
        assert_eq!(
            scan.retained_paths,
            vec![PathBuf::from("knowledge/shared.txt")]
        );
        assert!(!workspace.layout().state.join("private.txt").exists());
    }
}
