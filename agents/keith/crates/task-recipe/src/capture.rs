use std::collections::BTreeMap;
use std::fmt;

use keith_agent_types::{ProfileId, SchemaVersion, UtcTimestamp};
use keith_platform_contracts::{ComputerSessionId, ControlOwner, DemonstrationId};
use serde::{Deserialize, Serialize};

use crate::{TaskRecipeError, TaskRecipeStore};

pub const DEMONSTRATION_SCHEMA_VERSION: SchemaVersion = SchemaVersion::new(1, 0);

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum DemonstrationState {
    Recording,
    Paused,
    Completed,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum MediaSanitization {
    NoSensitiveContent,
    SensitiveRegionsRedacted,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct MediaReference {
    pub digest: String,
    pub media_type: String,
    pub byte_len: u64,
    pub sanitization: MediaSanitization,
}

impl MediaReference {
    pub fn validate(&self) -> Result<(), TaskRecipeError> {
        if self.digest.len() != 64
            || !self.digest.bytes().all(|byte| byte.is_ascii_hexdigit())
            || self.media_type.trim().is_empty()
            || self.byte_len == 0
        {
            return Err(TaskRecipeError::InvalidDemonstration(
                "media reference is malformed".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FrameReference {
    pub frame_id: String,
    pub media: MediaReference,
    pub width: u32,
    pub height: u32,
}

impl FrameReference {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        if !valid_name(&self.frame_id) || self.width == 0 || self.height == 0 {
            return Err(TaskRecipeError::InvalidDemonstration(
                "frame reference is malformed".into(),
            ));
        }
        self.media.validate()
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ParameterSource {
    RuntimeInput,
    NamedCredential,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ParameterReference {
    pub name: String,
    pub source: ParameterSource,
}

impl ParameterReference {
    pub fn new(name: impl Into<String>, source: ParameterSource) -> Result<Self, TaskRecipeError> {
        let reference = Self {
            name: name.into(),
            source,
        };
        if !valid_name(&reference.name) {
            return Err(TaskRecipeError::InvalidDemonstration(
                "parameter reference is malformed".into(),
            ));
        }
        Ok(reference)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum SensitiveKind {
    Password,
    AuthenticationToken,
    Payment,
    PersonalData,
    PrivateKey,
    UserMarked,
    Unknown,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "capture", content = "value")]
pub enum CapturedValue {
    Literal(String),
    Parameter(ParameterReference),
    Redacted(SensitiveKind),
}

impl CapturedValue {
    pub fn as_literal(&self) -> Option<&str> {
        match self {
            Self::Literal(value) => Some(value),
            Self::Parameter(_) | Self::Redacted(_) => None,
        }
    }

    fn validate(&self, field: &FieldMetadata) -> Result<(), TaskRecipeError> {
        match self {
            Self::Literal(value)
                if field.sensitive_kind().is_some() || secret_in_value(value).is_some() =>
            {
                Err(TaskRecipeError::InvalidDemonstration(
                    "sensitive literal reached the persistent capture model".into(),
                ))
            }
            Self::Literal(value) if value.is_empty() => Err(TaskRecipeError::InvalidDemonstration(
                "captured literal is empty".into(),
            )),
            Self::Parameter(reference) if !valid_name(&reference.name) => Err(
                TaskRecipeError::InvalidDemonstration("captured parameter is malformed".into()),
            ),
            Self::Literal(_) | Self::Parameter(_) | Self::Redacted(_) => Ok(()),
        }
    }
}

pub struct RawValue(String);

impl RawValue {
    pub fn new(value: impl Into<String>) -> Self {
        Self(value.into())
    }

    fn into_inner(self) -> String {
        self.0
    }
}

impl fmt::Debug for RawValue {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str("RawValue([REDACTED])")
    }
}

#[derive(Clone, Debug, Default, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FieldMetadata {
    pub name: Option<String>,
    pub role: Option<String>,
    pub autocomplete: Option<String>,
    pub user_marked_sensitive: bool,
}

impl FieldMetadata {
    pub fn named(name: impl Into<String>) -> Self {
        Self {
            name: Some(name.into()),
            ..Self::default()
        }
    }

    pub fn sensitive_kind(&self) -> Option<SensitiveKind> {
        if self.user_marked_sensitive {
            return Some(SensitiveKind::UserMarked);
        }
        let joined = [
            self.name.as_deref().unwrap_or_default(),
            self.role.as_deref().unwrap_or_default(),
            self.autocomplete.as_deref().unwrap_or_default(),
        ]
        .join(" ")
        .to_ascii_lowercase();
        if contains_any(&joined, &["password", "passwd", "passcode", "pin"]) {
            Some(SensitiveKind::Password)
        } else if contains_any(
            &joined,
            &[
                "token",
                "secret",
                "api-key",
                "api_key",
                "apikey",
                "authorization",
                "cookie",
            ],
        ) {
            Some(SensitiveKind::AuthenticationToken)
        } else if contains_any(&joined, &["credit-card", "cc-number", "cvv", "cvc"]) {
            Some(SensitiveKind::Payment)
        } else if contains_any(&joined, &["social-security", "ssn", "national-id"]) {
            Some(SensitiveKind::PersonalData)
        } else {
            None
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct RedactionPolicy {
    substitutions: BTreeMap<String, ParameterReference>,
    max_value_bytes: usize,
}

impl RedactionPolicy {
    pub fn new(max_value_bytes: usize) -> Result<Self, TaskRecipeError> {
        if max_value_bytes == 0 {
            return Err(TaskRecipeError::LimitExceeded(
                "captured values must have a positive byte ceiling".into(),
            ));
        }
        Ok(Self {
            substitutions: BTreeMap::new(),
            max_value_bytes,
        })
    }

    pub fn bind(
        &mut self,
        field_name: impl Into<String>,
        parameter: ParameterReference,
    ) -> Result<(), TaskRecipeError> {
        let field_name = normalize_field(&field_name.into());
        if field_name.is_empty() {
            return Err(TaskRecipeError::InvalidDemonstration(
                "substitution field name is malformed".into(),
            ));
        }
        self.substitutions.insert(field_name, parameter);
        Ok(())
    }
}

impl Default for RedactionPolicy {
    fn default() -> Self {
        Self {
            substitutions: BTreeMap::new(),
            max_value_bytes: 16 * 1_024,
        }
    }
}

#[derive(Clone, Debug, Default)]
pub struct CaptureSanitizer {
    policy: RedactionPolicy,
}

impl CaptureSanitizer {
    pub fn new(policy: RedactionPolicy) -> Self {
        Self { policy }
    }

    pub fn bind_parameter(
        &mut self,
        field_name: impl Into<String>,
        parameter: ParameterReference,
    ) -> Result<(), TaskRecipeError> {
        self.policy.bind(field_name, parameter)
    }

    pub fn sanitize(
        &self,
        field: &FieldMetadata,
        raw: RawValue,
    ) -> Result<CapturedValue, TaskRecipeError> {
        let value = raw.into_inner();
        if value.is_empty() || value.len() > self.policy.max_value_bytes {
            return Err(TaskRecipeError::LimitExceeded(
                "captured value is empty or oversized".into(),
            ));
        }
        if let Some(name) = field.name.as_deref().map(normalize_field)
            && let Some(reference) = self.policy.substitutions.get(&name)
        {
            return Ok(CapturedValue::Parameter(reference.clone()));
        }
        if let Some(kind) = field.sensitive_kind().or_else(|| secret_in_value(&value)) {
            return Ok(CapturedValue::Redacted(kind));
        }
        Ok(CapturedValue::Literal(value))
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct SemanticTarget {
    pub role: String,
    pub accessible_name: CapturedValue,
    pub stable_attributes: BTreeMap<String, CapturedValue>,
    pub bounds: Rectangle,
    pub field: FieldMetadata,
}

impl SemanticTarget {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        if self.role.trim().is_empty() || self.bounds.width == 0 || self.bounds.height == 0 {
            return Err(TaskRecipeError::InvalidDemonstration(
                "semantic target is malformed".into(),
            ));
        }
        self.accessible_name.validate(&self.field)?;
        for (name, value) in &self.stable_attributes {
            if !valid_name(name) {
                return Err(TaskRecipeError::InvalidDemonstration(
                    "semantic target attribute name is malformed".into(),
                ));
            }
            value.validate(&FieldMetadata::named(name))?;
        }
        Ok(())
    }
}

#[derive(Debug)]
pub struct RawSemanticTarget {
    pub role: String,
    pub accessible_name: RawValue,
    pub stable_attributes: BTreeMap<String, RawValue>,
    pub bounds: Rectangle,
    pub field: FieldMetadata,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Rectangle {
    pub x: i32,
    pub y: i32,
    pub width: u32,
    pub height: u32,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CaptureContext {
    pub frame: Option<FrameReference>,
    pub semantic_target: Option<SemanticTarget>,
    pub url: Option<CapturedValue>,
    pub window: Option<CapturedValue>,
    pub application: Option<CapturedValue>,
    pub control_owner: ControlOwner,
}

impl CaptureContext {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        if let Some(frame) = &self.frame {
            frame.validate()?;
        }
        if let Some(target) = &self.semantic_target {
            target.validate()?;
        }
        for (field, value) in [
            (FieldMetadata::named("url"), self.url.as_ref()),
            (FieldMetadata::named("window"), self.window.as_ref()),
            (
                FieldMetadata::named("application"),
                self.application.as_ref(),
            ),
        ] {
            if let Some(value) = value {
                value.validate(&field)?;
            }
        }
        Ok(())
    }
}

#[derive(Debug)]
pub struct RawCaptureContext {
    pub frame: Option<FrameReference>,
    pub semantic_target: Option<RawSemanticTarget>,
    pub url: Option<RawValue>,
    pub window: Option<RawValue>,
    pub application: Option<RawValue>,
    pub control_owner: ControlOwner,
}

impl RawCaptureContext {
    fn sanitize(self, sanitizer: &CaptureSanitizer) -> Result<CaptureContext, TaskRecipeError> {
        let semantic_target = self
            .semantic_target
            .map(|target| {
                let accessible_name = sanitizer.sanitize(&target.field, target.accessible_name)?;
                let stable_attributes = target
                    .stable_attributes
                    .into_iter()
                    .map(|(name, value)| {
                        let captured_attribute =
                            sanitizer.sanitize(&FieldMetadata::named(&name), value)?;
                        Ok::<_, TaskRecipeError>((name, captured_attribute))
                    })
                    .collect::<Result<_, _>>()?;
                Ok::<SemanticTarget, TaskRecipeError>(SemanticTarget {
                    role: target.role,
                    accessible_name,
                    stable_attributes,
                    bounds: target.bounds,
                    field: target.field,
                })
            })
            .transpose()?;
        let context = CaptureContext {
            frame: self.frame,
            semantic_target,
            url: sanitize_optional(sanitizer, "url", self.url)?,
            window: sanitize_optional(sanitizer, "window", self.window)?,
            application: sanitize_optional(sanitizer, "application", self.application)?,
            control_owner: self.control_owner,
        };
        context.validate()?;
        Ok(context)
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PointerAction {
    Move,
    ButtonDown,
    ButtonUp,
    Scroll,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum PointerButton {
    Primary,
    Secondary,
    Middle,
    None,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PointerInput {
    pub action: PointerAction,
    pub button: PointerButton,
    pub x: i32,
    pub y: i32,
    pub delta_x: i32,
    pub delta_y: i32,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum KeyPhase {
    Down,
    Up,
    Repeat,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct KeyboardInput {
    pub phase: KeyPhase,
    pub key: CapturedValue,
    pub code: String,
    pub modifiers: Vec<String>,
    pub field: FieldMetadata,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ClipboardOperation {
    Read,
    Write,
    Clear,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum FileOperation {
    Select,
    Upload,
    Download,
    Save,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case", tag = "event", content = "data")]
pub enum DemonstrationEventKind {
    FrameCaptured(FrameReference),
    Pointer(PointerInput),
    Keyboard(KeyboardInput),
    Clipboard {
        operation: ClipboardOperation,
        value: Option<CapturedValue>,
        field: FieldMetadata,
    },
    File {
        operation: FileOperation,
        path: CapturedValue,
        field: FieldMetadata,
        media: Option<MediaReference>,
    },
    Pause {
        reason: CapturedValue,
    },
    Resume,
    Narration(CapturedValue),
    ControlChanged(ControlOwner),
    Navigate {
        url: CapturedValue,
    },
    Wait {
        duration_ms: u64,
    },
}

impl DemonstrationEventKind {
    fn validate(&self) -> Result<(), TaskRecipeError> {
        let generic = FieldMetadata::default();
        match self {
            Self::FrameCaptured(frame) => frame.validate(),
            Self::Keyboard(input) => {
                if input.code.trim().is_empty() {
                    return Err(TaskRecipeError::InvalidDemonstration(
                        "keyboard code is empty".into(),
                    ));
                }
                input.key.validate(&input.field)
            }
            Self::Clipboard { value, field, .. } => {
                value.as_ref().map_or(Ok(()), |value| value.validate(field))
            }
            Self::File {
                path, field, media, ..
            } => {
                path.validate(field)?;
                if let Some(media) = media {
                    media.validate()?;
                }
                Ok(())
            }
            Self::Pause { reason } | Self::Narration(reason) => reason.validate(&generic),
            Self::Navigate { url } => url.validate(&FieldMetadata::named("url")),
            Self::Wait { duration_ms: 0 } => Err(TaskRecipeError::InvalidDemonstration(
                "captured wait duration must be positive".into(),
            )),
            Self::Pointer(_) | Self::Resume | Self::ControlChanged(_) | Self::Wait { .. } => Ok(()),
        }
    }
}

#[derive(Debug)]
pub enum RawDemonstrationEventKind {
    FrameCaptured(FrameReference),
    Pointer(PointerInput),
    Keyboard {
        phase: KeyPhase,
        key: RawValue,
        code: String,
        modifiers: Vec<String>,
        field: FieldMetadata,
    },
    Clipboard {
        operation: ClipboardOperation,
        value: Option<RawValue>,
        field: FieldMetadata,
    },
    File {
        operation: FileOperation,
        path: RawValue,
        field: FieldMetadata,
        media: Option<MediaReference>,
    },
    Pause {
        reason: RawValue,
    },
    Resume,
    Narration(RawValue),
    ControlChanged(ControlOwner),
    Navigate {
        url: RawValue,
    },
    Wait {
        duration_ms: u64,
    },
}

impl RawDemonstrationEventKind {
    fn sanitize(
        self,
        sanitizer: &CaptureSanitizer,
    ) -> Result<DemonstrationEventKind, TaskRecipeError> {
        match self {
            Self::FrameCaptured(frame) => Ok(DemonstrationEventKind::FrameCaptured(frame)),
            Self::Pointer(input) => Ok(DemonstrationEventKind::Pointer(input)),
            Self::Keyboard {
                phase,
                key,
                code,
                modifiers,
                field,
            } => Ok(DemonstrationEventKind::Keyboard(KeyboardInput {
                phase,
                key: sanitizer.sanitize(&field, key)?,
                code,
                modifiers,
                field,
            })),
            Self::Clipboard {
                operation,
                value,
                field,
            } => Ok(DemonstrationEventKind::Clipboard {
                operation,
                value: value
                    .map(|value| sanitizer.sanitize(&field, value))
                    .transpose()?,
                field,
            }),
            Self::File {
                operation,
                path,
                field,
                media,
            } => Ok(DemonstrationEventKind::File {
                operation,
                path: sanitizer.sanitize(&field, path)?,
                field,
                media,
            }),
            Self::Pause { reason } => Ok(DemonstrationEventKind::Pause {
                reason: sanitizer.sanitize(&FieldMetadata::named("pause_reason"), reason)?,
            }),
            Self::Resume => Ok(DemonstrationEventKind::Resume),
            Self::Narration(value) => Ok(DemonstrationEventKind::Narration(
                sanitizer.sanitize(&FieldMetadata::named("narration"), value)?,
            )),
            Self::ControlChanged(owner) => Ok(DemonstrationEventKind::ControlChanged(owner)),
            Self::Navigate { url } => Ok(DemonstrationEventKind::Navigate {
                url: sanitizer.sanitize(&FieldMetadata::named("url"), url)?,
            }),
            Self::Wait { duration_ms } => Ok(DemonstrationEventKind::Wait { duration_ms }),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct DemonstrationEvent {
    pub sequence: u64,
    pub captured_at: UtcTimestamp,
    pub elapsed_ms: u64,
    pub context: CaptureContext,
    pub kind: DemonstrationEventKind,
}

#[derive(Debug)]
pub struct RawDemonstrationEvent {
    pub captured_at: UtcTimestamp,
    pub context: RawCaptureContext,
    pub kind: RawDemonstrationEventKind,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CaptureLimits {
    pub max_events: usize,
    pub max_event_bytes: usize,
    pub max_total_bytes: usize,
    pub max_duration_ms: u64,
}

impl Default for CaptureLimits {
    fn default() -> Self {
        Self {
            max_events: 100_000,
            max_event_bytes: 64 * 1_024,
            max_total_bytes: 64 * 1_024 * 1_024,
            max_duration_ms: 8 * 60 * 60 * 1_000,
        }
    }
}

impl CaptureLimits {
    fn validate(self) -> Result<(), TaskRecipeError> {
        if self.max_events == 0
            || self.max_event_bytes == 0
            || self.max_total_bytes < self.max_event_bytes
            || self.max_duration_ms == 0
        {
            return Err(TaskRecipeError::LimitExceeded(
                "demonstration capture limits are invalid".into(),
            ));
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RetentionPolicy {
    pub retain_until: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct Demonstration {
    pub schema: SchemaVersion,
    pub id: DemonstrationId,
    pub profile_id: ProfileId,
    pub computer_session_id: ComputerSessionId,
    pub title: String,
    pub started_at: UtcTimestamp,
    pub ended_at: Option<UtcTimestamp>,
    pub state: DemonstrationState,
    pub retention: RetentionPolicy,
    pub limits: CaptureLimits,
    events: Vec<DemonstrationEvent>,
}

impl Demonstration {
    pub fn new(
        profile_id: ProfileId,
        computer_session_id: ComputerSessionId,
        title: impl Into<String>,
        started_at: UtcTimestamp,
        retention: RetentionPolicy,
        limits: CaptureLimits,
    ) -> Result<Self, TaskRecipeError> {
        limits.validate()?;
        let title = title.into();
        if title.trim().is_empty() || retention.retain_until <= started_at {
            return Err(TaskRecipeError::InvalidDemonstration(
                "title or retention window is invalid".into(),
            ));
        }
        Ok(Self {
            schema: DEMONSTRATION_SCHEMA_VERSION,
            id: DemonstrationId::new(),
            profile_id,
            computer_session_id,
            title,
            started_at,
            ended_at: None,
            state: DemonstrationState::Recording,
            retention,
            limits,
            events: Vec::new(),
        })
    }

    pub fn events(&self) -> &[DemonstrationEvent] {
        &self.events
    }

    pub fn record(
        &mut self,
        raw: RawDemonstrationEvent,
        sanitizer: &CaptureSanitizer,
    ) -> Result<&DemonstrationEvent, TaskRecipeError> {
        let is_pause = matches!(&raw.kind, RawDemonstrationEventKind::Pause { .. });
        let is_resume = matches!(&raw.kind, RawDemonstrationEventKind::Resume);
        match (self.state, is_pause, is_resume) {
            (DemonstrationState::Recording, false, true)
            | (DemonstrationState::Paused, true, _)
            | (DemonstrationState::Paused, false, false)
            | (DemonstrationState::Completed, _, _) => {
                return Err(TaskRecipeError::InvalidState);
            }
            (DemonstrationState::Recording, _, _) | (DemonstrationState::Paused, false, true) => {}
        }
        if self.events.len() >= self.limits.max_events {
            return Err(TaskRecipeError::LimitExceeded(
                "demonstration event count exhausted".into(),
            ));
        }
        let elapsed = raw
            .captured_at
            .unix_millis()
            .checked_sub(self.started_at.unix_millis())
            .and_then(|value| u64::try_from(value).ok())
            .ok_or_else(|| {
                TaskRecipeError::InvalidDemonstration(
                    "event timestamp precedes demonstration".into(),
                )
            })?;
        if elapsed > self.limits.max_duration_ms
            || self
                .events
                .last()
                .is_some_and(|event| event.captured_at > raw.captured_at)
        {
            return Err(TaskRecipeError::LimitExceeded(
                "demonstration timing is not monotonic or is over budget".into(),
            ));
        }
        let context = raw.context.sanitize(sanitizer)?;
        let kind = raw.kind.sanitize(sanitizer)?;
        if matches!(
            kind,
            DemonstrationEventKind::Pointer(_) | DemonstrationEventKind::Keyboard(_)
        ) && context.frame.is_none()
        {
            return Err(TaskRecipeError::InvalidDemonstration(
                "raw input must identify its source frame".into(),
            ));
        }
        let sequence = u64::try_from(self.events.len()).map_err(|_| {
            TaskRecipeError::LimitExceeded("demonstration sequence exhausted".into())
        })?;
        let event = DemonstrationEvent {
            sequence,
            captured_at: raw.captured_at,
            elapsed_ms: elapsed,
            context,
            kind,
        };
        event.context.validate()?;
        event.kind.validate()?;
        event.validate_synchronization()?;
        let event_bytes = serde_json::to_vec(&event)?.len();
        if event_bytes > self.limits.max_event_bytes {
            return Err(TaskRecipeError::LimitExceeded(
                "demonstration event byte ceiling exhausted".into(),
            ));
        }
        let total_bytes = self.events.iter().try_fold(event_bytes, |total, event| {
            total
                .checked_add(serde_json::to_vec(event)?.len())
                .ok_or_else(|| TaskRecipeError::LimitExceeded("capture byte count overflow".into()))
        })?;
        if total_bytes > self.limits.max_total_bytes {
            return Err(TaskRecipeError::LimitExceeded(
                "demonstration byte ceiling exhausted".into(),
            ));
        }
        if is_pause {
            self.state = DemonstrationState::Paused;
        } else if is_resume {
            self.state = DemonstrationState::Recording;
        }
        self.events.push(event);
        self.events.last().ok_or_else(|| {
            TaskRecipeError::InvalidDemonstration("captured event disappeared".into())
        })
    }

    pub fn complete(&mut self, ended_at: UtcTimestamp) -> Result<(), TaskRecipeError> {
        if self.state == DemonstrationState::Completed || self.events.is_empty() {
            return Err(TaskRecipeError::InvalidState);
        }
        if ended_at
            < self
                .events
                .last()
                .map_or(self.started_at, |event| event.captured_at)
        {
            return Err(TaskRecipeError::InvalidDemonstration(
                "completion timestamp precedes captured events".into(),
            ));
        }
        self.ended_at = Some(ended_at);
        self.state = DemonstrationState::Completed;
        self.validate()
    }

    pub fn validate(&self) -> Result<(), TaskRecipeError> {
        if self.schema != DEMONSTRATION_SCHEMA_VERSION {
            return Err(TaskRecipeError::InvalidDemonstration(
                "unsupported demonstration schema".into(),
            ));
        }
        self.limits.validate()?;
        if self.title.trim().is_empty() || self.retention.retain_until <= self.started_at {
            return Err(TaskRecipeError::InvalidDemonstration(
                "demonstration metadata is malformed".into(),
            ));
        }
        if self.events.len() > self.limits.max_events {
            return Err(TaskRecipeError::LimitExceeded(
                "demonstration event count exhausted".into(),
            ));
        }
        let mut previous = self.started_at;
        let mut lifecycle = DemonstrationState::Recording;
        let mut total_bytes = 0_usize;
        for (index, event) in self.events.iter().enumerate() {
            if event.sequence != u64::try_from(index).unwrap_or(u64::MAX)
                || event.captured_at < previous
                || event.elapsed_ms
                    != u64::try_from(
                        event.captured_at.unix_millis() - self.started_at.unix_millis(),
                    )
                    .unwrap_or(u64::MAX)
            {
                return Err(TaskRecipeError::InvalidDemonstration(
                    "event synchronization is invalid".into(),
                ));
            }
            if event.elapsed_ms > self.limits.max_duration_ms {
                return Err(TaskRecipeError::LimitExceeded(
                    "demonstration duration ceiling exhausted".into(),
                ));
            }
            event.context.validate()?;
            event.kind.validate()?;
            event.validate_synchronization()?;
            let event_bytes = serde_json::to_vec(event)?.len();
            if event_bytes > self.limits.max_event_bytes {
                return Err(TaskRecipeError::LimitExceeded(
                    "demonstration event byte ceiling exhausted".into(),
                ));
            }
            total_bytes = total_bytes.checked_add(event_bytes).ok_or_else(|| {
                TaskRecipeError::LimitExceeded("capture byte count overflow".into())
            })?;
            lifecycle = match (&event.kind, lifecycle) {
                (DemonstrationEventKind::Pause { .. }, DemonstrationState::Recording) => {
                    DemonstrationState::Paused
                }
                (DemonstrationEventKind::Resume, DemonstrationState::Paused) => {
                    DemonstrationState::Recording
                }
                (DemonstrationEventKind::Pause { .. } | DemonstrationEventKind::Resume, _)
                | (_, DemonstrationState::Paused | DemonstrationState::Completed) => {
                    return Err(TaskRecipeError::InvalidDemonstration(
                        "captured event lifecycle is invalid".into(),
                    ));
                }
                (_, DemonstrationState::Recording) => DemonstrationState::Recording,
            };
            previous = event.captured_at;
        }
        if total_bytes > self.limits.max_total_bytes {
            return Err(TaskRecipeError::LimitExceeded(
                "demonstration byte ceiling exhausted".into(),
            ));
        }
        match (self.state, self.ended_at) {
            (DemonstrationState::Completed, Some(ended_at)) if ended_at >= previous => Ok(()),
            (DemonstrationState::Recording, None) if lifecycle == DemonstrationState::Recording => {
                Ok(())
            }
            (DemonstrationState::Paused, None) if lifecycle == DemonstrationState::Paused => Ok(()),
            _ => Err(TaskRecipeError::InvalidDemonstration(
                "demonstration lifecycle is inconsistent".into(),
            )),
        }
    }

    pub fn export(&self, store: &TaskRecipeStore) -> Result<Vec<u8>, TaskRecipeError> {
        store.export_demonstration(&self.id)
    }
}

impl DemonstrationEvent {
    fn validate_synchronization(&self) -> Result<(), TaskRecipeError> {
        match &self.kind {
            DemonstrationEventKind::FrameCaptured(frame)
                if self.context.frame.as_ref() != Some(frame) =>
            {
                Err(TaskRecipeError::InvalidDemonstration(
                    "frame event and capture context disagree".into(),
                ))
            }
            DemonstrationEventKind::Pointer(pointer) => {
                let frame = self.context.frame.as_ref().ok_or_else(|| {
                    TaskRecipeError::InvalidDemonstration(
                        "pointer event has no source frame".into(),
                    )
                })?;
                let x = u32::try_from(pointer.x).ok();
                let y = u32::try_from(pointer.y).ok();
                if x.is_none_or(|x| x >= frame.width) || y.is_none_or(|y| y >= frame.height) {
                    return Err(TaskRecipeError::InvalidDemonstration(
                        "pointer event lies outside its source frame".into(),
                    ));
                }
                Ok(())
            }
            DemonstrationEventKind::Keyboard(_) if self.context.frame.is_none() => Err(
                TaskRecipeError::InvalidDemonstration("keyboard event has no source frame".into()),
            ),
            DemonstrationEventKind::ControlChanged(owner)
                if *owner != self.context.control_owner =>
            {
                Err(TaskRecipeError::InvalidDemonstration(
                    "control transition and capture context disagree".into(),
                ))
            }
            DemonstrationEventKind::FrameCaptured(_)
            | DemonstrationEventKind::Keyboard(_)
            | DemonstrationEventKind::Clipboard { .. }
            | DemonstrationEventKind::File { .. }
            | DemonstrationEventKind::Pause { .. }
            | DemonstrationEventKind::Resume
            | DemonstrationEventKind::Narration(_)
            | DemonstrationEventKind::ControlChanged(_)
            | DemonstrationEventKind::Navigate { .. }
            | DemonstrationEventKind::Wait { .. } => Ok(()),
        }
    }
}

fn sanitize_optional(
    sanitizer: &CaptureSanitizer,
    name: &str,
    value: Option<RawValue>,
) -> Result<Option<CapturedValue>, TaskRecipeError> {
    value
        .map(|value| sanitizer.sanitize(&FieldMetadata::named(name), value))
        .transpose()
}

fn normalize_field(value: &str) -> String {
    value
        .trim()
        .chars()
        .map(|character| {
            if character.is_ascii_alphanumeric() {
                character.to_ascii_lowercase()
            } else {
                '_'
            }
        })
        .collect::<String>()
        .trim_matches('_')
        .to_owned()
}

fn valid_name(value: &str) -> bool {
    let mut characters = value.chars();
    characters
        .next()
        .is_some_and(|first| first.is_ascii_alphanumeric())
        && value.len() <= 128
        && characters.all(|character| {
            character.is_ascii_alphanumeric() || matches!(character, '-' | '_' | '.')
        })
}

fn contains_any(value: &str, needles: &[&str]) -> bool {
    needles.iter().any(|needle| value.contains(needle))
}

fn secret_in_value(value: &str) -> Option<SensitiveKind> {
    let lower = value.to_ascii_lowercase();
    if lower.contains("-----begin ") && lower.contains("private key-----") {
        Some(SensitiveKind::PrivateKey)
    } else if lower.starts_with("bearer ")
        || contains_any(
            &lower,
            &[
                "password=",
                "password:",
                "access_token=",
                "api_key=",
                "apikey=",
                "authorization:",
            ],
        )
        || url_has_user_info(&lower)
    {
        Some(SensitiveKind::AuthenticationToken)
    } else {
        None
    }
}

fn url_has_user_info(value: &str) -> bool {
    value
        .split_once("://")
        .and_then(|(_, rest)| rest.split_once('@').map(|(user_info, _)| user_info))
        .is_some_and(|user_info| user_info.contains(':'))
}
