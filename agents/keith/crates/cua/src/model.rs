use std::fmt::{self, Display};
use std::str::FromStr;
use std::time::Duration;

use keith_agent_types::{EntityId, EntityIdError, ProfileId, UtcTimestamp};
use keith_platform_contracts::{
    AuditEnvelope, CancellationId, ComputerSessionId, ExternalAction, ExternalPrincipalId,
    RedactedText,
};
use serde::{Deserialize, Serialize};

pub const MAX_DOM_BYTES: usize = 512 * 1_024;
pub const MAX_ACCESSIBILITY_NODES: usize = 8_192;
pub const MAX_RECENT_ACTIONS: usize = 128;
pub const MAX_ALTERNATE_ACTIONS: usize = 3;

macro_rules! cua_ids {
    ($($name:ident),+ $(,)?) => {
        $(
            #[derive(Clone, Debug, Eq, Hash, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
            #[serde(transparent)]
            pub struct $name(EntityId);

            impl $name {
                pub fn new() -> Self {
                    Self(EntityId::new())
                }

                pub fn from_u128(value: u128) -> Self {
                    Self(EntityId::from_u128(value))
                }

                pub fn as_str(&self) -> &str {
                    self.0.as_str()
                }
            }

            impl Default for $name {
                fn default() -> Self {
                    Self::new()
                }
            }

            impl Display for $name {
                fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
                    Display::fmt(&self.0, formatter)
                }
            }

            impl FromStr for $name {
                type Err = EntityIdError;

                fn from_str(value: &str) -> Result<Self, Self::Err> {
                    EntityId::from_str(value).map(Self)
                }
            }
        )+
    };
}

cua_ids!(ComputerSnapshotId, FrameId);

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ComputerMode {
    Persistent,
    Ephemeral,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ComputerLifecycle {
    Created,
    Starting,
    Running,
    Suspended,
    Interrupted,
    Terminated,
    Deleted,
    Failed,
}

impl ComputerLifecycle {
    pub const fn can_start(self) -> bool {
        matches!(self, Self::Created | Self::Suspended | Self::Interrupted)
    }

    pub const fn is_terminal(self) -> bool {
        matches!(self, Self::Terminated | Self::Deleted | Self::Failed)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum IsolationRequirement {
    Strong,
    ReducedExplicitlyAllowed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum NetworkPolicy {
    Denied,
    LoopbackOnly,
    Allowed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerResourceLimits {
    pub cpu_seconds: u64,
    pub memory_bytes: u64,
    pub disk_bytes: u64,
    pub max_processes: u32,
    pub max_action_duration_ms: u64,
    pub max_lifetime_ms: u64,
    pub idle_timeout_ms: u64,
    pub max_text_bytes: usize,
    pub max_file_bytes: u64,
    pub max_actions_per_minute: u32,
    pub max_retries: u8,
}

impl Default for ComputerResourceLimits {
    fn default() -> Self {
        Self {
            cpu_seconds: 3_600,
            memory_bytes: 2 * 1_024 * 1_024 * 1_024,
            disk_bytes: 8 * 1_024 * 1_024 * 1_024,
            max_processes: 256,
            max_action_duration_ms: 30_000,
            max_lifetime_ms: 4 * 60 * 60 * 1_000,
            idle_timeout_ms: 15 * 60 * 1_000,
            max_text_bytes: 64 * 1_024,
            max_file_bytes: 256 * 1_024 * 1_024,
            max_actions_per_minute: 120,
            max_retries: 2,
        }
    }
}

impl ComputerResourceLimits {
    /// Validates non-zero resource ceilings and the retry bound.
    ///
    /// # Errors
    ///
    /// Returns [`crate::ComputerError::InvalidLimits`] for an unsafe limit set.
    pub fn validate(&self) -> Result<(), crate::ComputerError> {
        if self.cpu_seconds == 0
            || self.memory_bytes < 64 * 1_024 * 1_024
            || self.disk_bytes < 16 * 1_024 * 1_024
            || self.max_processes == 0
            || self.max_action_duration_ms == 0
            || self.max_lifetime_ms == 0
            || self.idle_timeout_ms == 0
            || self.max_text_bytes == 0
            || self.max_file_bytes == 0
            || self.max_actions_per_minute == 0
            || self.max_retries > 8
        {
            return Err(crate::ComputerError::InvalidLimits);
        }
        Ok(())
    }

    pub const fn action_timeout(&self) -> Duration {
        Duration::from_millis(self.max_action_duration_ms)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CreateComputerRequest {
    pub profile_id: ProfileId,
    pub mode: ComputerMode,
    pub isolation: IsolationRequirement,
    pub network: NetworkPolicy,
    pub viewport: Viewport,
    pub limits: ComputerResourceLimits,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerSession {
    pub id: ComputerSessionId,
    pub profile_id: ProfileId,
    pub mode: ComputerMode,
    pub lifecycle: ComputerLifecycle,
    pub isolation: IsolationRequirement,
    pub network: NetworkPolicy,
    pub viewport: Viewport,
    pub limits: ComputerResourceLimits,
    pub created_at: UtcTimestamp,
    pub updated_at: UtcTimestamp,
    pub last_activity_at: UtcTimestamp,
    pub active_snapshot: Option<ComputerSnapshotId>,
    pub generation: u64,
    pub safe_error: Option<RedactedText>,
}

impl ComputerSession {
    pub fn is_owned_by(&self, profile_id: &ProfileId) -> bool {
        &self.profile_id == profile_id
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComputerSessionLayout {
    pub root: std::path::PathBuf,
    pub profile: std::path::PathBuf,
    pub workspace: std::path::PathBuf,
    pub downloads: std::path::PathBuf,
    pub runtime: std::path::PathBuf,
    pub snapshots: std::path::PathBuf,
}

#[derive(Clone, Copy, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Point {
    pub x: i32,
    pub y: i32,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Viewport {
    pub width: u32,
    pub height: u32,
    pub device_scale_milli: u32,
}

impl Default for Viewport {
    fn default() -> Self {
        Self {
            width: 1_280,
            height: 720,
            device_scale_milli: 1_000,
        }
    }
}

impl Viewport {
    /// Validates bounded workstation geometry and scale.
    ///
    /// # Errors
    ///
    /// Returns [`crate::ComputerError::InvalidViewport`] for unsupported geometry.
    pub fn validate(self) -> Result<(), crate::ComputerError> {
        if !(320..=7_680).contains(&self.width)
            || !(240..=4_320).contains(&self.height)
            || !(500..=4_000).contains(&self.device_scale_milli)
        {
            return Err(crate::ComputerError::InvalidViewport);
        }
        Ok(())
    }

    pub fn contains(self, point: Point) -> bool {
        match (u32::try_from(point.x), u32::try_from(point.y)) {
            (Ok(x), Ok(y)) => x < self.width && y < self.height,
            _ => false,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Screenshot {
    pub frame_id: FrameId,
    pub content_digest: String,
    pub media_type: String,
    pub base64_data: String,
    pub width: u32,
    pub height: u32,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DomSnapshot {
    pub frame_id: FrameId,
    pub url: String,
    pub title: String,
    pub html: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AccessibilityNode {
    pub role: String,
    pub name: String,
    pub value: Option<String>,
    pub disabled: bool,
    pub focused: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FocusedWindow {
    pub title: String,
    pub application: String,
    pub window_id: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DialogObservation {
    pub kind: String,
    pub message: RedactedText,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DownloadState {
    InProgress,
    Completed,
    Cancelled,
    Failed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DownloadObservation {
    pub file_name: String,
    pub received_bytes: u64,
    pub total_bytes: Option<u64>,
    pub state: DownloadState,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ApplicationObservation {
    pub name: String,
    pub version: Option<String>,
    pub document_label: Option<RedactedText>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RecentAction {
    pub action_digest: String,
    pub description: String,
    pub occurred_at: UtcTimestamp,
    pub succeeded: bool,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerObservation {
    pub computer_session_id: ComputerSessionId,
    pub profile_id: ProfileId,
    pub captured_at: UtcTimestamp,
    pub screenshot: Screenshot,
    pub dom: Option<DomSnapshot>,
    pub accessibility: Vec<AccessibilityNode>,
    pub focused_window: Option<FocusedWindow>,
    pub url: Option<String>,
    pub viewport: Viewport,
    pub cursor: Point,
    pub dialogs: Vec<DialogObservation>,
    pub downloads: Vec<DownloadObservation>,
    pub applications: Vec<ApplicationObservation>,
    pub recent_actions: Vec<RecentAction>,
}

impl ComputerObservation {
    /// Validates ownership-independent observation consistency and byte/count limits.
    ///
    /// # Errors
    ///
    /// Returns a validation error for inconsistent frames, geometry, or oversized data.
    pub fn validate(&self) -> Result<(), crate::ComputerError> {
        self.viewport.validate()?;
        if self.screenshot.frame_id
            != self.dom.as_ref().map_or_else(
                || self.screenshot.frame_id.clone(),
                |dom| dom.frame_id.clone(),
            )
            || self.screenshot.width != self.viewport.width
            || self.screenshot.height != self.viewport.height
            || self
                .dom
                .as_ref()
                .is_some_and(|dom| dom.html.len() > MAX_DOM_BYTES)
            || self.accessibility.len() > MAX_ACCESSIBILITY_NODES
            || self.recent_actions.len() > MAX_RECENT_ACTIONS
            || !self.viewport.contains(self.cursor)
        {
            return Err(crate::ComputerError::InvalidObservation);
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum SemanticTarget {
    Css { selector: String },
    Text { text: String },
    Accessibility { role: String, name: String },
}

impl SemanticTarget {
    /// Validates bounded semantic selector text.
    ///
    /// # Errors
    ///
    /// Returns [`crate::ComputerError::InvalidAction`] for empty or unsafe target text.
    pub fn validate(&self) -> Result<(), crate::ComputerError> {
        let valid = match self {
            Self::Css { selector } => bounded_non_control(selector, 2_048),
            Self::Text { text } => bounded_non_control(text, 4_096),
            Self::Accessibility { role, name } => {
                bounded_non_control(role, 128) && bounded_non_control(name, 1_024)
            }
        };
        if valid {
            Ok(())
        } else {
            Err(crate::ComputerError::InvalidAction)
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum ActionTarget {
    Semantic { target: SemanticTarget },
    Coordinate { point: Point, source_frame: FrameId },
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MouseButton {
    Left,
    Middle,
    Right,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "type")]
pub enum ComputerAction {
    Move {
        target: ActionTarget,
    },
    Click {
        target: ActionTarget,
        button: MouseButton,
    },
    DoubleClick {
        target: ActionTarget,
        button: MouseButton,
    },
    Drag {
        from: ActionTarget,
        to: ActionTarget,
        duration_ms: u64,
    },
    Scroll {
        delta_x: i32,
        delta_y: i32,
    },
    Key {
        key: String,
    },
    Text {
        text: String,
    },
    Shortcut {
        keys: Vec<String>,
    },
    ClipboardRead,
    ClipboardWrite {
        text: String,
    },
    FileUpload {
        target: SemanticTarget,
        relative_path: String,
    },
    Download {
        target: SemanticTarget,
        expected_file_name: Option<String>,
    },
    NewTab {
        url: Option<String>,
    },
    CloseTab,
    SwitchTab {
        index: usize,
    },
    NewWindow {
        url: Option<String>,
    },
    CloseWindow,
    FocusWindow {
        window_id: String,
    },
    Navigate {
        url: String,
    },
    Wait {
        duration_ms: u64,
    },
    CredentialFill {
        grant: NamedCredentialGrant,
        target: SemanticTarget,
    },
}

impl ComputerAction {
    /// Validates action-specific text, path, geometry, duration, and collection bounds.
    ///
    /// # Errors
    ///
    /// Returns [`crate::ComputerError::InvalidAction`] when any input exceeds its limit.
    pub fn validate(&self, limits: &ComputerResourceLimits) -> Result<(), crate::ComputerError> {
        let validate_target = |target: &ActionTarget| match target {
            ActionTarget::Semantic { target } => target.validate(),
            ActionTarget::Coordinate { .. } => Ok(()),
        };
        match self {
            Self::Move { target }
            | Self::Click { target, .. }
            | Self::DoubleClick { target, .. } => validate_target(target)?,
            Self::Drag {
                from,
                to,
                duration_ms,
            } => {
                validate_target(from)?;
                validate_target(to)?;
                if *duration_ms == 0 || *duration_ms > limits.max_action_duration_ms {
                    return Err(crate::ComputerError::InvalidAction);
                }
            }
            Self::Scroll { delta_x, delta_y } => {
                if delta_x.unsigned_abs() > 100_000 || delta_y.unsigned_abs() > 100_000 {
                    return Err(crate::ComputerError::InvalidAction);
                }
            }
            Self::Key { key } => validate_text(key, 128)?,
            Self::Text { text } | Self::ClipboardWrite { text } => {
                validate_text(text, limits.max_text_bytes)?;
            }
            Self::Shortcut { keys } => {
                if keys.is_empty() || keys.len() > 8 {
                    return Err(crate::ComputerError::InvalidAction);
                }
                for key in keys {
                    validate_text(key, 128)?;
                }
            }
            Self::FileUpload {
                target,
                relative_path,
            } => {
                target.validate()?;
                validate_relative_path(relative_path)?;
            }
            Self::Download {
                target,
                expected_file_name,
            } => {
                target.validate()?;
                if let Some(name) = expected_file_name {
                    validate_file_name(name)?;
                }
            }
            Self::NewTab { url } | Self::NewWindow { url } => {
                if let Some(url) = url {
                    validate_url(url)?;
                }
            }
            Self::SwitchTab { index } if *index > 255 => {
                return Err(crate::ComputerError::InvalidAction);
            }
            Self::FocusWindow { window_id } => validate_text(window_id, 256)?,
            Self::Navigate { url } => validate_url(url)?,
            Self::Wait { duration_ms } => {
                if *duration_ms == 0 || *duration_ms > limits.max_action_duration_ms {
                    return Err(crate::ComputerError::InvalidAction);
                }
            }
            Self::CredentialFill { grant, target } => {
                grant.validate()?;
                target.validate()?;
            }
            Self::ClipboardRead | Self::CloseTab | Self::CloseWindow | Self::SwitchTab { .. } => {}
        }
        Ok(())
    }

    pub const fn requires_consequential_approval(&self) -> bool {
        matches!(
            self,
            Self::Click { .. }
                | Self::DoubleClick { .. }
                | Self::Drag { .. }
                | Self::Key { .. }
                | Self::Text { .. }
                | Self::Shortcut { .. }
                | Self::ClipboardWrite { .. }
                | Self::FileUpload { .. }
                | Self::Download { .. }
                | Self::NewTab { .. }
                | Self::CloseTab
                | Self::NewWindow { .. }
                | Self::CloseWindow
                | Self::Navigate { .. }
                | Self::CredentialFill { .. }
        )
    }

    pub fn coordinate_frames(&self) -> Vec<&FrameId> {
        let mut frames = Vec::with_capacity(2);
        match self {
            Self::Move { target }
            | Self::Click { target, .. }
            | Self::DoubleClick { target, .. } => {
                if let ActionTarget::Coordinate { source_frame, .. } = target {
                    frames.push(source_frame);
                }
            }
            Self::Drag { from, to, .. } => {
                if let ActionTarget::Coordinate { source_frame, .. } = from {
                    frames.push(source_frame);
                }
                if let ActionTarget::Coordinate { source_frame, .. } = to {
                    frames.push(source_frame);
                }
            }
            _ => {}
        }
        frames
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct NamedCredentialGrant {
    pub grant_name: RedactedText,
    pub opaque_handle: RedactedText,
    pub profile_id: ProfileId,
    pub allowed_origin: RedactedText,
    pub expires_at: UtcTimestamp,
}

impl NamedCredentialGrant {
    /// Validates non-secret credential metadata and opaque handle syntax.
    ///
    /// # Errors
    ///
    /// Returns [`crate::ComputerError::InvalidCredentialGrant`] for malformed metadata.
    pub fn validate(&self) -> Result<(), crate::ComputerError> {
        let origin = url::Url::parse(self.allowed_origin.as_str())
            .map_err(|_| crate::ComputerError::InvalidCredentialGrant)?;
        if !bounded_identifier(self.grant_name.as_str(), 128)
            || !bounded_identifier(self.opaque_handle.as_str(), 256)
            || self.allowed_origin.as_str().len() > 2_048
            || !matches!(origin.scheme(), "http" | "https")
            || origin.host_str().is_none()
            || !origin.username().is_empty()
            || origin.password().is_some()
            || origin.query().is_some()
            || origin.fragment().is_some()
            || origin.path() != "/"
        {
            return Err(crate::ComputerError::InvalidCredentialGrant);
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "kind")]
pub enum ProgressExpectation {
    FrameChanged,
    UrlEquals { url: String },
    UrlContains { fragment: String },
    DomContains { text: String },
    DownloadCompleted { file_name: String },
    WindowFocused { window_id: String },
    NoChangeBefore { duration_ms: u64 },
}

impl ProgressExpectation {
    /// Validates bounded progress predicates.
    ///
    /// # Errors
    ///
    /// Returns [`crate::ComputerError::InvalidAction`] for unsafe predicates.
    pub fn validate(&self, limits: &ComputerResourceLimits) -> Result<(), crate::ComputerError> {
        match self {
            Self::UrlEquals { url } => validate_url(url),
            Self::UrlContains { fragment } | Self::DomContains { text: fragment } => {
                validate_text(fragment, 4_096)
            }
            Self::DownloadCompleted { file_name } => validate_file_name(file_name),
            Self::WindowFocused { window_id } => validate_text(window_id, 256),
            Self::NoChangeBefore { duration_ms } => {
                if *duration_ms == 0 || *duration_ms > limits.max_action_duration_ms {
                    Err(crate::ComputerError::InvalidAction)
                } else {
                    Ok(())
                }
            }
            Self::FrameChanged => Ok(()),
        }
    }

    pub fn is_satisfied(&self, before: &ComputerObservation, after: &ComputerObservation) -> bool {
        match self {
            Self::FrameChanged => {
                before.screenshot.content_digest != after.screenshot.content_digest
            }
            Self::UrlEquals { url } => after.url.as_deref() == Some(url.as_str()),
            Self::UrlContains { fragment } => {
                after.url.as_ref().is_some_and(|url| url.contains(fragment))
            }
            Self::DomContains { text } => after
                .dom
                .as_ref()
                .is_some_and(|dom| dom.html.contains(text)),
            Self::DownloadCompleted { file_name } => after.downloads.iter().any(|download| {
                download.file_name == *file_name && download.state == DownloadState::Completed
            }),
            Self::WindowFocused { window_id } => after
                .focused_window
                .as_ref()
                .is_some_and(|window| window.window_id == *window_id),
            Self::NoChangeBefore { .. } => {
                before.screenshot.content_digest == after.screenshot.content_digest
                    && before.url == after.url
            }
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ActionAttempt {
    pub action: ComputerAction,
    pub authority: ExternalAction,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerActionRequest {
    pub computer_session_id: ComputerSessionId,
    pub profile_id: ProfileId,
    pub primary: ActionAttempt,
    pub alternates: Vec<ActionAttempt>,
    pub progress: ProgressExpectation,
}

impl ComputerActionRequest {
    pub fn cancellation_id(&self) -> &CancellationId {
        &self.primary.authority.cancellation_id
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ActionDisposition {
    Completed,
    NoProgress,
    Cancelled,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerActionResult {
    pub disposition: ActionDisposition,
    pub attempts: u8,
    pub observation: ComputerObservation,
    pub audits: Vec<AuditEnvelope>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RuntimeActionResult {
    pub description: String,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerHealth {
    pub computer_session_id: ComputerSessionId,
    pub profile_id: ProfileId,
    pub lifecycle: ComputerLifecycle,
    pub process_running: bool,
    pub restartable: bool,
    pub safe_error: Option<RedactedText>,
    pub updated_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "command")]
pub enum RunnerCommand {
    Create {
        request: CreateComputerRequest,
        now: UtcTimestamp,
    },
    Start {
        session_id: ComputerSessionId,
        profile_id: ProfileId,
        now: UtcTimestamp,
    },
    Suspend {
        session_id: ComputerSessionId,
        profile_id: ProfileId,
        now: UtcTimestamp,
    },
    Resume {
        session_id: ComputerSessionId,
        profile_id: ProfileId,
        now: UtcTimestamp,
    },
    Snapshot {
        session_id: ComputerSessionId,
        profile_id: ProfileId,
        now: UtcTimestamp,
    },
    Restore {
        session_id: ComputerSessionId,
        profile_id: ProfileId,
        snapshot_id: ComputerSnapshotId,
        now: UtcTimestamp,
    },
    Reset {
        session_id: ComputerSessionId,
        profile_id: ProfileId,
        now: UtcTimestamp,
    },
    Terminate {
        session_id: ComputerSessionId,
        profile_id: ProfileId,
        now: UtcTimestamp,
    },
    DeleteProfile {
        profile_id: ProfileId,
        now: UtcTimestamp,
    },
    ReclaimIdle {
        now: UtcTimestamp,
    },
    Observe {
        session_id: ComputerSessionId,
        profile_id: ProfileId,
        now: UtcTimestamp,
    },
    Act {
        request: Box<ComputerActionRequest>,
        boundary: keith_platform_contracts::AuthorityBoundary,
        now: UtcTimestamp,
    },
    ControlledAct {
        request: Box<ComputerActionRequest>,
        boundary: keith_platform_contracts::AuthorityBoundary,
        screen_id: EntityId,
        expected_revision: u64,
        principal: ExternalPrincipalId,
        focus_unambiguous: bool,
        stream_synchronized: bool,
        now: UtcTimestamp,
    },
    Cancel {
        cancellation_id: CancellationId,
    },
    Health {
        session_id: ComputerSessionId,
        profile_id: ProfileId,
    },
    CreateScreen {
        session_id: ComputerSessionId,
        profile_id: ProfileId,
        keith_principal: ExternalPrincipalId,
        now: UtcTimestamp,
    },
    GetScreen {
        screen_id: EntityId,
        profile_id: ProfileId,
    },
    NegotiateScreenStream {
        screen_id: EntityId,
        profile_id: ProfileId,
        observer_id: ExternalPrincipalId,
        origin: String,
        now: UtcTimestamp,
        ttl_ms: u64,
    },
    AuthenticateScreenStream {
        profile_id: ProfileId,
        observer_id: ExternalPrincipalId,
        origin: String,
        stream_ticket: String,
        now: UtcTimestamp,
    },
    TakeUserControl {
        screen_id: EntityId,
        profile_id: ProfileId,
        expected_revision: u64,
        user_principal: ExternalPrincipalId,
        now: UtcTimestamp,
    },
    RequestKeithControl {
        screen_id: EntityId,
        profile_id: ProfileId,
        keith_principal: ExternalPrincipalId,
    },
    GrantKeithControl {
        screen_id: EntityId,
        profile_id: ProfileId,
        expected_revision: u64,
        now: UtcTimestamp,
    },
    PauseControl {
        screen_id: EntityId,
        profile_id: ProfileId,
        expected_revision: u64,
        now: UtcTimestamp,
    },
    UpdateScreen {
        screen_id: EntityId,
        profile_id: ProfileId,
        connection: crate::ScreenConnectionState,
        quality: crate::ScreenQuality,
        frame_sequence: u64,
        active_action: Option<RedactedText>,
        intended_action: Option<RedactedText>,
        recording: bool,
        safe_error: Option<RedactedText>,
        now: UtcTimestamp,
    },
    Shutdown,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "result")]
pub enum RunnerResponse {
    Session {
        session: ComputerSession,
    },
    Snapshot {
        snapshot_id: ComputerSnapshotId,
    },
    Deleted {
        sessions: usize,
    },
    Reclaimed {
        sessions: Vec<ComputerSessionId>,
    },
    Observation {
        observation: Box<ComputerObservation>,
    },
    Action {
        action: Box<ComputerActionResult>,
    },
    Cancelled {
        accepted: bool,
    },
    Health {
        health: ComputerHealth,
    },
    Screen {
        screen: crate::ScreenSession,
    },
    ScreenStream {
        grant: crate::ScreenStreamGrant,
    },
    ScreenStreamAuthenticated {
        screen_id: EntityId,
    },
    KeithControlRequested,
    Shutdown,
    Error {
        code: String,
        safe_message: String,
    },
}

fn validate_text(value: &str, max_bytes: usize) -> Result<(), crate::ComputerError> {
    if bounded_non_control(value, max_bytes) {
        Ok(())
    } else {
        Err(crate::ComputerError::InvalidAction)
    }
}

fn validate_url(value: &str) -> Result<(), crate::ComputerError> {
    if value.len() > 8_192 || value.chars().any(char::is_control) {
        return Err(crate::ComputerError::InvalidAction);
    }
    let Some((scheme, rest)) = value.split_once(':') else {
        return Err(crate::ComputerError::InvalidAction);
    };
    if !matches!(scheme, "http" | "https" | "about" | "file") || rest.is_empty() {
        return Err(crate::ComputerError::InvalidAction);
    }
    Ok(())
}

fn validate_relative_path(value: &str) -> Result<(), crate::ComputerError> {
    let path = std::path::Path::new(value);
    if value.is_empty()
        || value.len() > 4_096
        || path.is_absolute()
        || path.components().any(|component| {
            matches!(
                component,
                std::path::Component::ParentDir
                    | std::path::Component::RootDir
                    | std::path::Component::Prefix(_)
            )
        })
    {
        return Err(crate::ComputerError::InvalidAction);
    }
    Ok(())
}

fn validate_file_name(value: &str) -> Result<(), crate::ComputerError> {
    if bounded_non_control(value, 255)
        && !value.contains(['/', '\\'])
        && value != "."
        && value != ".."
    {
        Ok(())
    } else {
        Err(crate::ComputerError::InvalidAction)
    }
}

fn bounded_non_control(value: &str, max_bytes: usize) -> bool {
    !value.is_empty() && value.len() <= max_bytes && !value.chars().any(char::is_control)
}

fn bounded_identifier(value: &str, max_bytes: usize) -> bool {
    bounded_non_control(value, max_bytes)
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'-' | b'_' | b'.' | b':'))
}
