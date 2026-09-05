use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, File};
use std::io::Write;
use std::path::{Path, PathBuf};

use keith_agent_types::{EntityId, ProfileId, UtcTimestamp};
use keith_platform_contracts::{
    CancellationId, ComputerSessionId, ControlLease, ControlLeaseId, ControlOwner,
    ExternalPrincipalId, RedactedText,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};
use thiserror::Error;
use url::Url;

use crate::CancellationToken;

const CONTROL_FILE: &str = "computer-control.json";
const CONTROL_VERSION: u16 = 1;
const DEFAULT_LEASE_TTL_MS: i64 = 5 * 60 * 1_000;
const MAX_OBSERVERS: usize = 64;
const MAX_STREAM_TICKETS: usize = 128;

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ScreenConnectionState {
    Negotiating,
    Connected,
    Reconnecting,
    Closed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ScreenQuality {
    Low,
    Balanced,
    High,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ScreenSession {
    pub id: EntityId,
    pub computer_session_id: ComputerSessionId,
    pub profile_id: ProfileId,
    pub connection: ScreenConnectionState,
    pub quality: ScreenQuality,
    pub control: ControlLease,
    pub observers: BTreeSet<ExternalPrincipalId>,
    pub frame_sequence: u64,
    pub active_action: Option<RedactedText>,
    pub intended_action: Option<RedactedText>,
    pub recording: bool,
    pub safe_error: Option<RedactedText>,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ScreenStreamGrant {
    pub screen_session_id: EntityId,
    pub stream_path: String,
    pub stream_ticket: String,
    pub expires_at: UtcTimestamp,
    pub read_only: bool,
}

#[derive(Clone, Debug)]
struct StreamTicket {
    screen_session_id: EntityId,
    profile_id: ProfileId,
    observer_id: ExternalPrincipalId,
    origin: String,
    digest: [u8; 32],
    expires_at: UtcTimestamp,
}

#[derive(Clone, Debug, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct ControlState {
    version: u16,
    screens: BTreeMap<EntityId, ScreenSession>,
    computer_index: BTreeMap<ComputerSessionId, EntityId>,
    pending_keith: BTreeMap<EntityId, ExternalPrincipalId>,
}

impl Default for ControlState {
    fn default() -> Self {
        Self {
            version: CONTROL_VERSION,
            screens: BTreeMap::new(),
            computer_index: BTreeMap::new(),
            pending_keith: BTreeMap::new(),
        }
    }
}

pub struct ComputerControlService {
    root: PathBuf,
    state: ControlState,
    stream_tickets: BTreeMap<String, StreamTicket>,
    in_flight_keith_input: BTreeMap<EntityId, Vec<CancellationToken>>,
}

impl ComputerControlService {
    /// Opens durable exclusive-control state and pauses every expired lease after restart.
    ///
    /// # Errors
    ///
    /// Returns an error for unsafe roots, corrupt state, or persistence failure.
    pub fn open(root: impl AsRef<Path>, now: UtcTimestamp) -> Result<Self, ControlError> {
        let root = root.as_ref();
        if let Ok(metadata) = fs::symlink_metadata(root) {
            if metadata.file_type().is_symlink() || !metadata.is_dir() {
                return Err(ControlError::InvalidRoot);
            }
        } else {
            fs::create_dir_all(root)?;
        }
        discard_temporary_files(root)?;
        let state_path = root.join(CONTROL_FILE);
        let mut state = match fs::read(&state_path) {
            Ok(bytes) => serde_json::from_slice::<ControlState>(&bytes)?,
            Err(error) if error.kind() == std::io::ErrorKind::NotFound => ControlState::default(),
            Err(error) => return Err(error.into()),
        };
        validate_state(&state)?;
        let mut changed = false;
        for screen in state.screens.values_mut() {
            if screen.control.expires_at <= now {
                screen.control = paused_lease(
                    &screen.computer_session_id,
                    &screen.profile_id,
                    screen.control.revision.saturating_add(1),
                    now,
                )?;
                screen.connection = ScreenConnectionState::Reconnecting;
                screen.safe_error =
                    Some(RedactedText::parse("control lease expired during restart")?);
                screen.updated_at = now;
                changed = true;
            }
        }
        if changed {
            persist_state(root, &state)?;
        }
        Ok(Self {
            root: root.to_path_buf(),
            state,
            stream_tickets: BTreeMap::new(),
            in_flight_keith_input: BTreeMap::new(),
        })
    }

    /// Creates one screen/control session for an owned computer.
    ///
    /// # Errors
    ///
    /// Returns an error for duplicate computers or invalid lease time.
    pub fn create_screen(
        &mut self,
        computer_session_id: ComputerSessionId,
        profile_id: ProfileId,
        keith_principal: ExternalPrincipalId,
        now: UtcTimestamp,
    ) -> Result<ScreenSession, ControlError> {
        if self.state.computer_index.contains_key(&computer_session_id) {
            return Err(ControlError::AlreadyExists);
        }
        let expires_at = add_millis(now, DEFAULT_LEASE_TTL_MS)?;
        let lease = ControlLease {
            id: ControlLeaseId::new(),
            computer_session_id: computer_session_id.clone(),
            profile_id: profile_id.clone(),
            owner: ControlOwner::KeithControl,
            holder: Some(keith_principal),
            revision: 0,
            issued_at: now,
            expires_at,
        };
        lease.validate()?;
        let screen = ScreenSession {
            id: EntityId::new(),
            computer_session_id: computer_session_id.clone(),
            profile_id,
            connection: ScreenConnectionState::Negotiating,
            quality: ScreenQuality::Balanced,
            control: lease,
            observers: BTreeSet::new(),
            frame_sequence: 0,
            active_action: None,
            intended_action: None,
            recording: false,
            safe_error: None,
            updated_at: now,
        };
        let previous = self.state.clone();
        self.state
            .computer_index
            .insert(computer_session_id, screen.id.clone());
        self.state.screens.insert(screen.id.clone(), screen.clone());
        if let Err(error) = self.persist() {
            self.state = previous;
            return Err(error);
        }
        Ok(screen)
    }

    /// Returns one profile-owned screen session.
    ///
    /// # Errors
    ///
    /// Returns an error when the screen is absent or belongs to another profile.
    pub fn screen(
        &self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
    ) -> Result<&ScreenSession, ControlError> {
        let screen = self
            .state
            .screens
            .get(screen_id)
            .ok_or(ControlError::NotFound)?;
        if &screen.profile_id != profile_id {
            return Err(ControlError::ProfileDenied);
        }
        Ok(screen)
    }

    /// Issues one in-memory, short-lived, same-origin stream ticket for a read-only observer.
    ///
    /// # Errors
    ///
    /// Returns an error for profile mismatch, capacity, or invalid expiry.
    pub fn negotiate_stream(
        &mut self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
        observer_id: ExternalPrincipalId,
        origin: &str,
        now: UtcTimestamp,
        ttl_ms: u64,
    ) -> Result<ScreenStreamGrant, ControlError> {
        self.expire_tickets(now);
        validate_origin(origin)?;
        if self.stream_tickets.len() >= MAX_STREAM_TICKETS {
            return Err(ControlError::Capacity);
        }
        let ttl = i64::try_from(ttl_ms).map_err(|_| ControlError::InvalidTime)?;
        if !(1_000..=60_000).contains(&ttl) {
            return Err(ControlError::InvalidTime);
        }
        let previous = self.state.clone();
        let screen = self
            .state
            .screens
            .get_mut(screen_id)
            .ok_or(ControlError::NotFound)?;
        if &screen.profile_id != profile_id {
            return Err(ControlError::ProfileDenied);
        }
        if screen.observers.len() >= MAX_OBSERVERS && !screen.observers.contains(&observer_id) {
            return Err(ControlError::Capacity);
        }
        screen.observers.insert(observer_id.clone());
        screen.updated_at = now;
        let raw_ticket = EntityId::new().to_string();
        let ticket_key = ticket_digest(&raw_ticket);
        let ticket_value_digest = Sha256::digest(raw_ticket.as_bytes()).into();
        let expires_at = add_millis(now, ttl)?;
        let grant = ScreenStreamGrant {
            screen_session_id: screen_id.clone(),
            stream_path: format!("/api/computers/{screen_id}/screen"),
            stream_ticket: raw_ticket,
            expires_at,
            read_only: true,
        };
        if let Err(error) = self.persist() {
            self.state = previous;
            return Err(error);
        }
        self.stream_tickets.insert(
            ticket_key,
            StreamTicket {
                screen_session_id: screen_id.clone(),
                profile_id: profile_id.clone(),
                observer_id,
                origin: origin.to_string(),
                digest: ticket_value_digest,
                expires_at,
            },
        );
        Ok(grant)
    }

    /// Authenticates a stream ticket exactly once without exposing a backend display endpoint.
    ///
    /// # Errors
    ///
    /// Returns an error for an invalid origin or absent, expired, consumed, or mismatched ticket.
    pub fn consume_stream_ticket(
        &mut self,
        profile_id: &ProfileId,
        observer_id: &ExternalPrincipalId,
        origin: &str,
        raw_ticket: &str,
        now: UtcTimestamp,
    ) -> Result<EntityId, ControlError> {
        validate_origin(origin)?;
        let key = ticket_digest(raw_ticket);
        let ticket = self
            .stream_tickets
            .remove(&key)
            .ok_or(ControlError::StreamDenied)?;
        let presented: [u8; 32] = Sha256::digest(raw_ticket.as_bytes()).into();
        if ticket.expires_at <= now
            || &ticket.profile_id != profile_id
            || &ticket.observer_id != observer_id
            || ticket.origin != origin
            || !constant_time_eq(&ticket.digest, &presented)
        {
            return Err(ControlError::StreamDenied);
        }
        Ok(ticket.screen_session_id)
    }

    /// Registers a cancellation handle before Keith begins an input sequence.
    ///
    /// # Errors
    ///
    /// Returns an error unless the exact current Keith lease can inject synchronized input.
    pub fn register_keith_input(
        &mut self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
        expected_revision: u64,
        keith_principal: &ExternalPrincipalId,
        token: CancellationToken,
        now: UtcTimestamp,
    ) -> Result<(), ControlError> {
        self.authorize_input(
            profile_id,
            screen_id,
            expected_revision,
            keith_principal,
            true,
            true,
            now,
        )?;
        if self.screen(profile_id, screen_id)?.control.owner != ControlOwner::KeithControl {
            return Err(ControlError::InputDenied);
        }
        self.in_flight_keith_input
            .entry(screen_id.clone())
            .or_default()
            .push(token);
        Ok(())
    }

    /// Releases one completed Keith input sequence from takeover cancellation tracking.
    ///
    /// # Errors
    ///
    /// Returns an error when the screen is absent or belongs to another profile.
    pub fn finish_keith_input(
        &mut self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
        cancellation_id: &CancellationId,
    ) -> Result<(), ControlError> {
        self.screen(profile_id, screen_id)?;
        let empty = if let Some(tokens) = self.in_flight_keith_input.get_mut(screen_id) {
            tokens.retain(|token| token.id() != Some(cancellation_id));
            tokens.is_empty()
        } else {
            false
        };
        if empty {
            self.in_flight_keith_input.remove(screen_id);
        }
        Ok(())
    }

    /// Immediately cancels all Keith input and atomically transfers the exclusive lease to user.
    ///
    /// # Errors
    ///
    /// Returns an error for stale revisions, profile mismatch, or invalid expiry.
    pub fn take_user_control(
        &mut self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
        expected_revision: u64,
        user_principal: ExternalPrincipalId,
        now: UtcTimestamp,
    ) -> Result<ScreenSession, ControlError> {
        self.validate_transfer(profile_id, screen_id, expected_revision)?;
        let screen = self.transfer(
            profile_id,
            screen_id,
            expected_revision,
            ControlOwner::UserControl,
            Some(user_principal),
            now,
        )?;
        if let Some(tokens) = self.in_flight_keith_input.remove(screen_id) {
            for token in tokens {
                token.cancel();
            }
        }
        Ok(screen)
    }

    /// Records Keith's request for control without changing the current owner.
    ///
    /// # Errors
    ///
    /// Returns an error for profile mismatch or persistence failure.
    pub fn request_keith_control(
        &mut self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
        keith_principal: ExternalPrincipalId,
    ) -> Result<(), ControlError> {
        self.screen(profile_id, screen_id)?;
        let previous = self.state.clone();
        self.state
            .pending_keith
            .insert(screen_id.clone(), keith_principal);
        if let Err(error) = self.persist() {
            self.state = previous;
            return Err(error);
        }
        Ok(())
    }

    /// Grants a previously requested Keith lease under optimistic revision control.
    ///
    /// # Errors
    ///
    /// Returns an error when no request exists, the lease is stale, or persistence fails.
    pub fn grant_keith_control(
        &mut self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
        expected_revision: u64,
        now: UtcTimestamp,
    ) -> Result<ScreenSession, ControlError> {
        let principal = self
            .state
            .pending_keith
            .get(screen_id)
            .cloned()
            .ok_or(ControlError::NoPendingRequest)?;
        self.transfer(
            profile_id,
            screen_id,
            expected_revision,
            ControlOwner::KeithControl,
            Some(principal),
            now,
        )
    }

    /// Pauses all input under optimistic revision control and cancels active Keith input.
    ///
    /// # Errors
    ///
    /// Returns an error for profile mismatch, stale leases, invalid time, or persistence failure.
    pub fn pause(
        &mut self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
        expected_revision: u64,
        now: UtcTimestamp,
    ) -> Result<ScreenSession, ControlError> {
        self.validate_transfer(profile_id, screen_id, expected_revision)?;
        let screen = self.transfer(
            profile_id,
            screen_id,
            expected_revision,
            ControlOwner::Paused,
            None,
            now,
        )?;
        if let Some(tokens) = self.in_flight_keith_input.remove(screen_id) {
            for token in tokens {
                token.cancel();
            }
        }
        Ok(screen)
    }

    /// Deletes every screen, observer, ticket, pending request, and cancellation for one profile.
    ///
    /// # Errors
    ///
    /// Returns an error when the durable deletion cannot be committed.
    pub fn delete_profile(&mut self, profile_id: &ProfileId) -> Result<usize, ControlError> {
        let removed = self
            .state
            .screens
            .iter()
            .filter(|(_, screen)| &screen.profile_id == profile_id)
            .map(|(screen_id, _)| screen_id.clone())
            .collect::<Vec<_>>();
        if removed.is_empty() {
            return Ok(0);
        }
        let previous = self.state.clone();
        for screen_id in &removed {
            if let Some(screen) = self.state.screens.remove(screen_id) {
                self.state
                    .computer_index
                    .remove(&screen.computer_session_id);
            }
            self.state.pending_keith.remove(screen_id);
        }
        if let Err(error) = self.persist() {
            self.state = previous;
            return Err(error);
        }
        self.stream_tickets
            .retain(|_, ticket| &ticket.profile_id != profile_id);
        for screen_id in &removed {
            if let Some(tokens) = self.in_flight_keith_input.remove(screen_id) {
                for token in tokens {
                    token.cancel();
                }
            }
        }
        Ok(removed.len())
    }

    /// Enforces exact holder, lease revision, focus, and stream synchronization before input.
    ///
    /// # Errors
    ///
    /// Returns an error when any profile, lease, holder, focus, stream, or expiry check fails.
    #[allow(clippy::too_many_arguments)]
    pub fn authorize_input(
        &self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
        expected_revision: u64,
        principal: &ExternalPrincipalId,
        focus_unambiguous: bool,
        stream_synchronized: bool,
        now: UtcTimestamp,
    ) -> Result<(), ControlError> {
        let screen = self.screen(profile_id, screen_id)?;
        if screen.control.revision != expected_revision {
            return Err(ControlError::StaleLease);
        }
        if !focus_unambiguous {
            return Err(ControlError::FocusAmbiguous);
        }
        if !stream_synchronized || screen.connection != ScreenConnectionState::Connected {
            return Err(ControlError::StreamDesynchronized);
        }
        if !screen.control.can_inject(principal, now) {
            return Err(ControlError::InputDenied);
        }
        Ok(())
    }

    /// Updates the bounded screen projection without permitting frame regression.
    ///
    /// # Errors
    ///
    /// Returns an error for profile mismatch, stale frames, invalid state, or persistence failure.
    #[allow(clippy::too_many_arguments)]
    pub fn update_screen(
        &mut self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
        connection: ScreenConnectionState,
        quality: ScreenQuality,
        frame_sequence: u64,
        active_action: Option<RedactedText>,
        intended_action: Option<RedactedText>,
        recording: bool,
        safe_error: Option<RedactedText>,
        now: UtcTimestamp,
    ) -> Result<ScreenSession, ControlError> {
        let previous = self.state.clone();
        let screen = self
            .state
            .screens
            .get_mut(screen_id)
            .ok_or(ControlError::NotFound)?;
        if &screen.profile_id != profile_id {
            return Err(ControlError::ProfileDenied);
        }
        if frame_sequence < screen.frame_sequence {
            return Err(ControlError::StaleFrame);
        }
        screen.connection = connection;
        screen.quality = quality;
        screen.frame_sequence = frame_sequence;
        screen.active_action = active_action;
        screen.intended_action = intended_action;
        screen.recording = recording;
        screen.safe_error = safe_error;
        screen.updated_at = now;
        let result = screen.clone();
        if let Err(error) = self.persist() {
            self.state = previous;
            return Err(error);
        }
        Ok(result)
    }

    fn transfer(
        &mut self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
        expected_revision: u64,
        owner: ControlOwner,
        holder: Option<ExternalPrincipalId>,
        now: UtcTimestamp,
    ) -> Result<ScreenSession, ControlError> {
        self.validate_transfer(profile_id, screen_id, expected_revision)?;
        let previous = self.state.clone();
        let screen = self
            .state
            .screens
            .get_mut(screen_id)
            .ok_or(ControlError::NotFound)?;
        if &screen.profile_id != profile_id {
            return Err(ControlError::ProfileDenied);
        }
        screen.control = screen.control.transfer(
            expected_revision,
            owner,
            holder,
            now,
            add_millis(now, DEFAULT_LEASE_TTL_MS)?,
        )?;
        screen.updated_at = now;
        let result = screen.clone();
        self.state.pending_keith.remove(screen_id);
        if let Err(error) = self.persist() {
            self.state = previous;
            return Err(error);
        }
        Ok(result)
    }

    fn validate_transfer(
        &self,
        profile_id: &ProfileId,
        screen_id: &EntityId,
        expected_revision: u64,
    ) -> Result<(), ControlError> {
        let screen = self.screen(profile_id, screen_id)?;
        if screen.control.revision != expected_revision {
            return Err(ControlError::StaleLease);
        }
        Ok(())
    }

    fn expire_tickets(&mut self, now: UtcTimestamp) {
        self.stream_tickets
            .retain(|_, ticket| ticket.expires_at > now);
    }

    fn persist(&self) -> Result<(), ControlError> {
        validate_state(&self.state)?;
        persist_state(&self.root, &self.state)
    }
}

fn validate_state(state: &ControlState) -> Result<(), ControlError> {
    if state.version != CONTROL_VERSION || state.screens.len() != state.computer_index.len() {
        return Err(ControlError::InvalidState);
    }
    for (id, screen) in &state.screens {
        screen.control.validate()?;
        if id != &screen.id
            || screen.observers.len() > MAX_OBSERVERS
            || state.computer_index.get(&screen.computer_session_id) != Some(id)
            || screen.control.computer_session_id != screen.computer_session_id
            || screen.control.profile_id != screen.profile_id
        {
            return Err(ControlError::InvalidState);
        }
    }
    if state
        .pending_keith
        .keys()
        .any(|screen_id| !state.screens.contains_key(screen_id))
    {
        return Err(ControlError::InvalidState);
    }
    Ok(())
}

fn paused_lease(
    computer_session_id: &ComputerSessionId,
    profile_id: &ProfileId,
    revision: u64,
    now: UtcTimestamp,
) -> Result<ControlLease, ControlError> {
    let lease = ControlLease {
        id: ControlLeaseId::new(),
        computer_session_id: computer_session_id.clone(),
        profile_id: profile_id.clone(),
        owner: ControlOwner::Paused,
        holder: None,
        revision,
        issued_at: now,
        expires_at: add_millis(now, DEFAULT_LEASE_TTL_MS)?,
    };
    lease.validate()?;
    Ok(lease)
}

fn persist_state(root: &Path, state: &ControlState) -> Result<(), ControlError> {
    let temporary = root.join(format!(".{CONTROL_FILE}.{}.tmp", EntityId::new()));
    let result = (|| {
        let bytes = serde_json::to_vec_pretty(state)?;
        let mut file = File::create(&temporary)?;
        file.write_all(&bytes)?;
        file.sync_all()?;
        fs::rename(&temporary, root.join(CONTROL_FILE))?;
        File::open(root)?.sync_all()?;
        Ok::<(), ControlError>(())
    })();
    if result.is_err() {
        let _ = fs::remove_file(&temporary);
    }
    result
}

fn discard_temporary_files(root: &Path) -> Result<(), ControlError> {
    for entry in fs::read_dir(root)? {
        let entry = entry?;
        let name = entry.file_name();
        let name = name.to_string_lossy();
        if name.starts_with(&format!(".{CONTROL_FILE}.")) && name.ends_with(".tmp") {
            fs::remove_file(entry.path())?;
        }
    }
    Ok(())
}

fn ticket_digest(value: &str) -> String {
    encode_hex(&Sha256::digest(value.as_bytes()))
}

fn validate_origin(value: &str) -> Result<(), ControlError> {
    if value.len() > 2_048 || value.chars().any(char::is_control) {
        return Err(ControlError::OriginDenied);
    }
    let url = Url::parse(value).map_err(|_| ControlError::OriginDenied)?;
    if !matches!(url.scheme(), "http" | "https")
        || !url.username().is_empty()
        || url.password().is_some()
        || url.path() != "/"
        || url.query().is_some()
        || url.fragment().is_some()
        || url.origin().ascii_serialization() != value
    {
        return Err(ControlError::OriginDenied);
    }
    Ok(())
}

fn encode_hex(bytes: &[u8]) -> String {
    const HEX: &[u8; 16] = b"0123456789abcdef";
    let mut encoded = String::with_capacity(bytes.len() * 2);
    for byte in bytes {
        encoded.push(char::from(HEX[usize::from(byte >> 4)]));
        encoded.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }
    encoded
}

fn constant_time_eq(left: &[u8], right: &[u8]) -> bool {
    let mut difference = left.len() ^ right.len();
    let max_len = left.len().max(right.len());
    for index in 0..max_len {
        difference |= usize::from(
            left.get(index).copied().unwrap_or_default()
                ^ right.get(index).copied().unwrap_or_default(),
        );
    }
    difference == 0
}

fn add_millis(now: UtcTimestamp, millis: i64) -> Result<UtcTimestamp, ControlError> {
    now.unix_millis()
        .checked_add(millis)
        .map(UtcTimestamp::from_unix_millis)
        .ok_or(ControlError::InvalidTime)
}

#[derive(Debug, Error)]
pub enum ControlError {
    #[error("computer control storage root is unsafe")]
    InvalidRoot,
    #[error("computer control state is corrupt or incompatible")]
    InvalidState,
    #[error("computer screen session already exists")]
    AlreadyExists,
    #[error("computer screen session was not found")]
    NotFound,
    #[error("computer screen belongs to another profile")]
    ProfileDenied,
    #[error("computer control or stream capacity was reached")]
    Capacity,
    #[error("screen stream ticket is invalid, expired, or already consumed")]
    StreamDenied,
    #[error("screen stream origin is invalid or does not match")]
    OriginDenied,
    #[error("computer control lease is stale")]
    StaleLease,
    #[error("computer input is not owned by this principal")]
    InputDenied,
    #[error("screen stream is desynchronized")]
    StreamDesynchronized,
    #[error("computer focus is ambiguous")]
    FocusAmbiguous,
    #[error("screen frame is stale")]
    StaleFrame,
    #[error("Keith has not requested computer control")]
    NoPendingRequest,
    #[error("computer control time range is invalid")]
    InvalidTime,
    #[error(transparent)]
    Contract(#[from] keith_platform_contracts::ContractError),
    #[error(transparent)]
    Io(#[from] std::io::Error),
    #[error(transparent)]
    Json(#[from] serde_json::Error),
}

#[cfg(test)]
mod tests {
    use super::*;
    use keith_platform_contracts::CancellationId;

    #[test]
    fn screen_stream_is_profile_scoped_single_use_and_hides_backend_endpoints() {
        let root = tempfile::tempdir().unwrap();
        let now = UtcTimestamp::from_unix_millis(100);
        let profile = ProfileId::new();
        let observer = ExternalPrincipalId::new();
        let mut service = ComputerControlService::open(root.path(), now).unwrap();
        let screen = service
            .create_screen(
                ComputerSessionId::new(),
                profile.clone(),
                ExternalPrincipalId::new(),
                now,
            )
            .unwrap();
        let grant = service
            .negotiate_stream(
                &profile,
                &screen.id,
                observer.clone(),
                "https://keith.test",
                now,
                10_000,
            )
            .unwrap();
        assert!(grant.stream_path.starts_with("/api/computers/"));
        assert!(!grant.stream_path.contains("vnc"));
        assert!(!grant.stream_path.contains("debug"));
        assert_eq!(
            service
                .consume_stream_ticket(
                    &profile,
                    &observer,
                    "https://keith.test",
                    &grant.stream_ticket,
                    UtcTimestamp::from_unix_millis(101),
                )
                .unwrap(),
            screen.id
        );
        assert!(matches!(
            service.consume_stream_ticket(
                &profile,
                &observer,
                "https://keith.test",
                &grant.stream_ticket,
                UtcTimestamp::from_unix_millis(102),
            ),
            Err(ControlError::StreamDenied)
        ));
        let mismatched = service
            .negotiate_stream(
                &profile,
                &screen.id,
                observer.clone(),
                "https://keith.test",
                UtcTimestamp::from_unix_millis(103),
                10_000,
            )
            .unwrap();
        assert!(matches!(
            service.consume_stream_ticket(
                &profile,
                &observer,
                "https://other.test",
                &mismatched.stream_ticket,
                UtcTimestamp::from_unix_millis(104),
            ),
            Err(ControlError::StreamDenied)
        ));
        assert!(
            !fs::read_to_string(root.path().join(CONTROL_FILE))
                .unwrap()
                .contains(&grant.stream_ticket)
        );
    }

    #[test]
    fn user_takeover_cancels_keith_input_and_requires_explicit_return() {
        let root = tempfile::tempdir().unwrap();
        let now = UtcTimestamp::from_unix_millis(100);
        let profile = ProfileId::new();
        let keith = ExternalPrincipalId::new();
        let user = ExternalPrincipalId::new();
        let mut service = ComputerControlService::open(root.path(), now).unwrap();
        let screen = service
            .create_screen(
                ComputerSessionId::new(),
                profile.clone(),
                keith.clone(),
                now,
            )
            .unwrap();
        service
            .update_screen(
                &profile,
                &screen.id,
                ScreenConnectionState::Connected,
                ScreenQuality::High,
                1,
                Some(RedactedText::parse("typing a form").unwrap()),
                Some(RedactedText::parse("submit after review").unwrap()),
                false,
                None,
                now,
            )
            .unwrap();
        let token = CancellationToken::new(CancellationId::new());
        service
            .register_keith_input(
                &profile,
                &screen.id,
                0,
                &keith,
                token.clone(),
                UtcTimestamp::from_unix_millis(101),
            )
            .unwrap();
        let user_screen = service
            .take_user_control(
                &profile,
                &screen.id,
                0,
                user.clone(),
                UtcTimestamp::from_unix_millis(102),
            )
            .unwrap();
        assert!(token.is_cancelled());
        assert_eq!(user_screen.control.owner, ControlOwner::UserControl);
        assert!(matches!(
            service.authorize_input(
                &profile,
                &screen.id,
                1,
                &keith,
                true,
                true,
                UtcTimestamp::from_unix_millis(103),
            ),
            Err(ControlError::InputDenied)
        ));
        service
            .request_keith_control(&profile, &screen.id, keith.clone())
            .unwrap();
        assert_eq!(
            service.screen(&profile, &screen.id).unwrap().control.owner,
            ControlOwner::UserControl
        );
        let keith_screen = service
            .grant_keith_control(&profile, &screen.id, 1, UtcTimestamp::from_unix_millis(104))
            .unwrap();
        assert_eq!(keith_screen.control.owner, ControlOwner::KeithControl);
    }

    #[test]
    fn cross_profile_takeover_cannot_cancel_or_change_the_owned_lease() {
        let root = tempfile::tempdir().unwrap();
        let now = UtcTimestamp::from_unix_millis(100);
        let profile = ProfileId::new();
        let attacker_profile = ProfileId::new();
        let keith = ExternalPrincipalId::new();
        let mut service = ComputerControlService::open(root.path(), now).unwrap();
        let screen = service
            .create_screen(
                ComputerSessionId::new(),
                profile.clone(),
                keith.clone(),
                now,
            )
            .unwrap();
        service
            .update_screen(
                &profile,
                &screen.id,
                ScreenConnectionState::Connected,
                ScreenQuality::Balanced,
                1,
                None,
                None,
                false,
                None,
                now,
            )
            .unwrap();
        let token = CancellationToken::new(CancellationId::new());
        service
            .register_keith_input(&profile, &screen.id, 0, &keith, token.clone(), now)
            .unwrap();
        assert!(matches!(
            service.take_user_control(
                &attacker_profile,
                &screen.id,
                0,
                ExternalPrincipalId::new(),
                now,
            ),
            Err(ControlError::ProfileDenied)
        ));
        assert!(!token.is_cancelled());
        assert_eq!(
            service.screen(&profile, &screen.id).unwrap().control.owner,
            ControlOwner::KeithControl
        );
    }

    #[test]
    fn restart_pauses_expired_control_and_refuses_stale_focus_or_stream() {
        let root = tempfile::tempdir().unwrap();
        let profile = ProfileId::new();
        let principal = ExternalPrincipalId::new();
        let mut service =
            ComputerControlService::open(root.path(), UtcTimestamp::from_unix_millis(0)).unwrap();
        let screen = service
            .create_screen(
                ComputerSessionId::new(),
                profile.clone(),
                principal.clone(),
                UtcTimestamp::from_unix_millis(0),
            )
            .unwrap();
        service
            .update_screen(
                &profile,
                &screen.id,
                ScreenConnectionState::Connected,
                ScreenQuality::Balanced,
                4,
                None,
                None,
                false,
                None,
                UtcTimestamp::from_unix_millis(1),
            )
            .unwrap();
        assert!(matches!(
            service.authorize_input(
                &profile,
                &screen.id,
                0,
                &principal,
                false,
                true,
                UtcTimestamp::from_unix_millis(2),
            ),
            Err(ControlError::FocusAmbiguous)
        ));
        assert!(matches!(
            service.authorize_input(
                &profile,
                &screen.id,
                0,
                &principal,
                true,
                false,
                UtcTimestamp::from_unix_millis(2),
            ),
            Err(ControlError::StreamDesynchronized)
        ));
        drop(service);
        let reopened = ComputerControlService::open(
            root.path(),
            UtcTimestamp::from_unix_millis(DEFAULT_LEASE_TTL_MS + 1),
        )
        .unwrap();
        assert_eq!(
            reopened.screen(&profile, &screen.id).unwrap().control.owner,
            ControlOwner::Paused
        );
    }

    #[test]
    fn profile_deletion_removes_control_streams_and_persisted_screen_state() {
        let root = tempfile::tempdir().unwrap();
        let profile = ProfileId::new();
        let now = UtcTimestamp::from_unix_millis(100);
        let mut service = ComputerControlService::open(root.path(), now).unwrap();
        let screen = service
            .create_screen(
                ComputerSessionId::new(),
                profile.clone(),
                ExternalPrincipalId::new(),
                now,
            )
            .unwrap();
        let observer = ExternalPrincipalId::new();
        let grant = service
            .negotiate_stream(
                &profile,
                &screen.id,
                observer.clone(),
                "https://keith.test",
                now,
                10_000,
            )
            .unwrap();
        assert_eq!(service.delete_profile(&profile).unwrap(), 1);
        assert!(matches!(
            service.screen(&profile, &screen.id),
            Err(ControlError::NotFound)
        ));
        assert!(matches!(
            service.consume_stream_ticket(
                &profile,
                &observer,
                "https://keith.test",
                &grant.stream_ticket,
                UtcTimestamp::from_unix_millis(101),
            ),
            Err(ControlError::StreamDenied)
        ));
        let reopened = ComputerControlService::open(root.path(), now).unwrap();
        assert!(matches!(
            reopened.screen(&profile, &screen.id),
            Err(ControlError::NotFound)
        ));
    }
}
