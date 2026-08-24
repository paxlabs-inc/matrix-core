use core::fmt;

use keith_agent_types::{
    ChildId, ComputerId, EntityId, GrantId, ProfileId, Revision, StableKey, TakeoverLeaseId,
    UtcTimestamp,
};
use serde::{Deserialize, Serialize};

pub const COMPUTER_STREAM_MAX_FRAME_BYTES: usize = 4 * 1024 * 1024;
pub const COMPUTER_STREAM_MAX_INPUT_BYTES: usize = 64 * 1024;
pub const COMPUTER_STREAM_MAX_WIDTH: u32 = 7_680;
pub const COMPUTER_STREAM_MAX_HEIGHT: u32 = 4_320;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerStreamSubject {
    pub profile_id: ProfileId,
    pub computer_id: ComputerId,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerStreamOrigin {
    pub server_instance_id: EntityId,
    pub stream_instance_id: EntityId,
    pub authority_key: StableKey,
    pub generation: u64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ComputerFrameEncoding {
    Png,
    Jpeg,
    WebP,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerStreamCursor {
    pub generation: u64,
    pub sequence: u64,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerStreamLimits {
    pub max_frame_bytes: usize,
    pub max_input_bytes: usize,
    pub max_width: u32,
    pub max_height: u32,
}

impl ComputerStreamLimits {
    pub const STRICT: Self = Self {
        max_frame_bytes: COMPUTER_STREAM_MAX_FRAME_BYTES,
        max_input_bytes: COMPUTER_STREAM_MAX_INPUT_BYTES,
        max_width: COMPUTER_STREAM_MAX_WIDTH,
        max_height: COMPUTER_STREAM_MAX_HEIGHT,
    };

    pub fn validate(self) -> Result<Self, ComputerStreamError> {
        if self.max_frame_bytes == 0
            || self.max_frame_bytes > COMPUTER_STREAM_MAX_FRAME_BYTES
            || self.max_input_bytes == 0
            || self.max_input_bytes > COMPUTER_STREAM_MAX_INPUT_BYTES
            || self.max_width == 0
            || self.max_width > COMPUTER_STREAM_MAX_WIDTH
            || self.max_height == 0
            || self.max_height > COMPUTER_STREAM_MAX_HEIGHT
        {
            return Err(ComputerStreamError::InvalidBounds);
        }
        Ok(self)
    }
}

impl Default for ComputerStreamLimits {
    fn default() -> Self {
        Self::STRICT
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComputerStreamAuthorization {
    subject: ComputerStreamSubject,
    computer_revision: Revision,
    origin: ComputerStreamOrigin,
    issued_at: UtcTimestamp,
    expires_at: UtcTimestamp,
    controller: ComputerStreamController,
}

impl ComputerStreamAuthorization {
    pub fn issue(
        subject: ComputerStreamSubject,
        computer_revision: Revision,
        origin: ComputerStreamOrigin,
        issued_at: UtcTimestamp,
        expires_at: UtcTimestamp,
        controller: ComputerStreamController,
    ) -> Result<Self, ComputerStreamError> {
        if expires_at <= issued_at || !controller.is_for_subject(&subject) {
            return Err(ComputerStreamError::UnauthorizedSubject);
        }
        Ok(Self {
            subject,
            computer_revision,
            origin,
            issued_at,
            expires_at,
            controller,
        })
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(tag = "authority", rename_all = "snake_case", deny_unknown_fields)]
pub enum ComputerStreamController {
    Agent {
        profile_id: ProfileId,
        task_key: StableKey,
        fencing_token: u64,
    },
    Routine {
        profile_id: ProfileId,
        routine_id: EntityId,
        task_key: StableKey,
        fencing_token: u64,
    },
    Child {
        profile_id: ProfileId,
        child_id: ChildId,
        task_key: StableKey,
        fencing_token: u64,
    },
    UserTakeover {
        profile_id: ProfileId,
        lease_id: TakeoverLeaseId,
        task_key: StableKey,
        fencing_token: u64,
        lease_revision: Revision,
    },
}

impl ComputerStreamController {
    pub fn is_for_subject(&self, subject: &ComputerStreamSubject) -> bool {
        match self {
            Self::Agent { profile_id, .. }
            | Self::Routine { profile_id, .. }
            | Self::Child { profile_id, .. }
            | Self::UserTakeover { profile_id, .. } => profile_id == &subject.profile_id,
        }
    }

    fn takeover_lease_id(&self) -> Option<&TakeoverLeaseId> {
        match self {
            Self::UserTakeover { lease_id, .. } => Some(lease_id),
            Self::Agent { .. } | Self::Routine { .. } | Self::Child { .. } => None,
        }
    }

    pub fn task_key(&self) -> &StableKey {
        match self {
            Self::Agent { task_key, .. }
            | Self::Routine { task_key, .. }
            | Self::Child { task_key, .. }
            | Self::UserTakeover { task_key, .. } => task_key,
        }
    }

    pub fn fencing_token(&self) -> u64 {
        match self {
            Self::Agent { fencing_token, .. }
            | Self::Routine { fencing_token, .. }
            | Self::Child { fencing_token, .. }
            | Self::UserTakeover { fencing_token, .. } => *fencing_token,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerStreamOpenRequest {
    pub subject: ComputerStreamSubject,
    pub resume: Option<ComputerStreamResume>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerStreamResume {
    pub origin: ComputerStreamOrigin,
    pub cursor: ComputerStreamCursor,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerStreamDescriptor {
    pub session_id: EntityId,
    pub subject: ComputerStreamSubject,
    pub computer_revision: Revision,
    pub origin: ComputerStreamOrigin,
    pub cursor: ComputerStreamCursor,
    pub controller: ComputerStreamController,
    pub takeover_lease_id: Option<TakeoverLeaseId>,
    pub limits: ComputerStreamLimits,
    pub connected_at: UtcTimestamp,
    pub liveness_deadline: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerFrame {
    pub session_id: EntityId,
    pub subject: ComputerStreamSubject,
    pub origin: ComputerStreamOrigin,
    pub sequence: u64,
    pub captured_at: UtcTimestamp,
    pub width: u32,
    pub height: u32,
    pub encoding: ComputerFrameEncoding,
    pub key_frame: bool,
    pub bytes: Vec<u8>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerFrameReceipt {
    pub cursor: ComputerStreamCursor,
    pub captured_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerObservation {
    pub session_id: EntityId,
    pub subject: ComputerStreamSubject,
    pub origin: ComputerStreamOrigin,
    pub url: String,
    pub title: String,
    pub text: String,
    pub observed_at: UtcTimestamp,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ComputerPointerButton {
    Primary,
    Middle,
    Secondary,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ComputerButtonState {
    Pressed,
    Released,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(tag = "kind", rename_all = "snake_case", deny_unknown_fields)]
pub enum ComputerInputPayload {
    PointerMove {
        x: u32,
        y: u32,
    },
    PointerButton {
        x: u32,
        y: u32,
        button: ComputerPointerButton,
        state: ComputerButtonState,
    },
    Scroll {
        delta_x: i32,
        delta_y: i32,
    },
    Key {
        code: String,
        state: ComputerButtonState,
        alt: bool,
        control: bool,
        meta: bool,
        shift: bool,
    },
    Text {
        text: String,
    },
    CredentialReference {
        grant_id: GrantId,
    },
    Focus,
    ReleaseAll,
}

impl ComputerInputPayload {
    fn bounded_len(&self) -> usize {
        match self {
            Self::Key { code, .. } => code.len(),
            Self::Text { text } => text.len(),
            Self::PointerMove { .. }
            | Self::PointerButton { .. }
            | Self::Scroll { .. }
            | Self::CredentialReference { .. }
            | Self::Focus
            | Self::ReleaseAll => 64,
        }
    }

    fn coordinates_within(&self, limits: ComputerStreamLimits) -> bool {
        match self {
            Self::PointerMove { x, y } | Self::PointerButton { x, y, .. } => {
                *x < limits.max_width && *y < limits.max_height
            }
            Self::Key { code, .. } => {
                !code.is_empty() && code.len() <= 128 && !code.chars().any(char::is_control)
            }
            Self::Text { text } => !text.chars().any(|character| character == '\0'),
            Self::Scroll { .. }
            | Self::CredentialReference { .. }
            | Self::Focus
            | Self::ReleaseAll => true,
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerInputCommand {
    pub session_id: EntityId,
    pub subject: ComputerStreamSubject,
    pub origin: ComputerStreamOrigin,
    pub sequence: u64,
    pub expected_computer_revision: Revision,
    pub controller: ComputerStreamController,
    pub payload: ComputerInputPayload,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComputerInputReceipt {
    pub sequence: u64,
    pub computer_revision: Revision,
    pub takeover_lease_id: Option<TakeoverLeaseId>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComputerStreamSession {
    session_id: EntityId,
    subject: ComputerStreamSubject,
    computer_revision: Revision,
    origin: ComputerStreamOrigin,
    cursor: ComputerStreamCursor,
    controller: ComputerStreamController,
    limits: ComputerStreamLimits,
    connected_at: UtcTimestamp,
    last_observed_at: UtcTimestamp,
    liveness_deadline: UtcTimestamp,
    next_input_sequence: u64,
}

impl ComputerStreamSession {
    pub fn open(
        session_id: EntityId,
        request: ComputerStreamOpenRequest,
        authorization: ComputerStreamAuthorization,
        limits: ComputerStreamLimits,
        now: UtcTimestamp,
        liveness_deadline: UtcTimestamp,
    ) -> Result<Self, ComputerStreamError> {
        let limits = limits.validate()?;
        if request.subject != authorization.subject
            || now < authorization.issued_at
            || now >= authorization.expires_at
            || liveness_deadline <= now
        {
            return Err(ComputerStreamError::UnauthorizedSubject);
        }
        let cursor = if let Some(resume) = request.resume {
            if resume.origin != authorization.origin {
                return Err(ComputerStreamError::OriginChanged);
            }
            if resume.cursor.generation != authorization.origin.generation {
                return Err(ComputerStreamError::GenerationChanged);
            }
            resume.cursor
        } else {
            ComputerStreamCursor {
                generation: authorization.origin.generation,
                sequence: 0,
            }
        };
        Ok(Self {
            session_id,
            subject: authorization.subject,
            computer_revision: authorization.computer_revision,
            origin: authorization.origin,
            cursor,
            controller: authorization.controller,
            limits,
            connected_at: now,
            last_observed_at: now,
            liveness_deadline,
            next_input_sequence: 0,
        })
    }

    pub fn descriptor(&self) -> ComputerStreamDescriptor {
        ComputerStreamDescriptor {
            session_id: self.session_id.clone(),
            subject: self.subject.clone(),
            computer_revision: self.computer_revision,
            origin: self.origin.clone(),
            cursor: self.cursor,
            controller: self.controller.clone(),
            takeover_lease_id: self.controller.takeover_lease_id().cloned(),
            limits: self.limits,
            connected_at: self.connected_at,
            liveness_deadline: self.liveness_deadline,
        }
    }

    pub fn cursor(&self) -> ComputerStreamCursor {
        self.cursor
    }

    pub fn is_live(&self, now: UtcTimestamp) -> bool {
        now < self.liveness_deadline
    }

    pub fn accept_frame(
        &mut self,
        frame: &ComputerFrame,
        now: UtcTimestamp,
        next_liveness_deadline: UtcTimestamp,
    ) -> Result<ComputerFrameReceipt, ComputerStreamError> {
        self.ensure_live(now)?;
        if frame.session_id != self.session_id
            || frame.subject != self.subject
            || frame.origin != self.origin
        {
            return Err(ComputerStreamError::ForgedSubject);
        }
        let expected = self
            .cursor
            .sequence
            .checked_add(1)
            .ok_or(ComputerStreamError::SequenceExhausted)?;
        if frame.sequence != expected {
            return Err(ComputerStreamError::SequenceGap {
                expected,
                actual: frame.sequence,
            });
        }
        if frame.width == 0
            || frame.width > self.limits.max_width
            || frame.height == 0
            || frame.height > self.limits.max_height
            || frame.bytes.is_empty()
            || frame.bytes.len() > self.limits.max_frame_bytes
        {
            return Err(ComputerStreamError::FrameOutOfBounds);
        }
        if next_liveness_deadline <= now {
            return Err(ComputerStreamError::Expired);
        }
        self.cursor.sequence = frame.sequence;
        self.last_observed_at = now;
        self.liveness_deadline = next_liveness_deadline;
        Ok(ComputerFrameReceipt {
            cursor: self.cursor,
            captured_at: frame.captured_at,
        })
    }

    pub fn authorize_input(
        &mut self,
        input: ComputerInputCommand,
        now: UtcTimestamp,
        next_liveness_deadline: UtcTimestamp,
    ) -> Result<(ComputerInputReceipt, ComputerInputPayload), ComputerStreamError> {
        self.ensure_live(now)?;
        if input.session_id != self.session_id
            || input.subject != self.subject
            || input.origin != self.origin
        {
            return Err(ComputerStreamError::ForgedSubject);
        }
        if input.expected_computer_revision != self.computer_revision {
            return Err(ComputerStreamError::StaleRevision);
        }
        if input.controller != self.controller || !input.controller.is_for_subject(&self.subject) {
            return Err(ComputerStreamError::UnauthorizedController);
        }
        if input.sequence != self.next_input_sequence {
            return Err(ComputerStreamError::SequenceGap {
                expected: self.next_input_sequence,
                actual: input.sequence,
            });
        }
        if input.payload.bounded_len() > self.limits.max_input_bytes
            || !input.payload.coordinates_within(self.limits)
        {
            return Err(ComputerStreamError::InputOutOfBounds);
        }
        if next_liveness_deadline <= now {
            return Err(ComputerStreamError::Expired);
        }
        self.next_input_sequence = self
            .next_input_sequence
            .checked_add(1)
            .ok_or(ComputerStreamError::SequenceExhausted)?;
        self.last_observed_at = now;
        self.liveness_deadline = next_liveness_deadline;
        let receipt = ComputerInputReceipt {
            sequence: input.sequence,
            computer_revision: self.computer_revision,
            takeover_lease_id: self.controller.takeover_lease_id().cloned(),
        };
        Ok((receipt, input.payload))
    }

    pub fn accept_observation(
        &mut self,
        observation: &ComputerObservation,
        now: UtcTimestamp,
        next_liveness_deadline: UtcTimestamp,
    ) -> Result<(), ComputerStreamError> {
        self.ensure_live(now)?;
        if observation.session_id != self.session_id
            || observation.subject != self.subject
            || observation.origin != self.origin
        {
            return Err(ComputerStreamError::ForgedSubject);
        }
        if next_liveness_deadline <= now {
            return Err(ComputerStreamError::Expired);
        }
        self.last_observed_at = now;
        self.liveness_deadline = next_liveness_deadline;
        Ok(())
    }

    pub fn reconnect(
        &mut self,
        authorization: ComputerStreamAuthorization,
        cursor: ComputerStreamCursor,
        now: UtcTimestamp,
        next_liveness_deadline: UtcTimestamp,
    ) -> Result<ComputerStreamDescriptor, ComputerStreamError> {
        if authorization.subject != self.subject {
            return Err(ComputerStreamError::ForgedSubject);
        }
        if authorization.origin != self.origin {
            return Err(ComputerStreamError::OriginChanged);
        }
        if authorization.computer_revision != self.computer_revision {
            return Err(ComputerStreamError::StaleRevision);
        }
        if authorization.controller != self.controller
            || now < authorization.issued_at
            || now >= authorization.expires_at
        {
            return Err(ComputerStreamError::UnauthorizedController);
        }
        if cursor.generation != self.cursor.generation || cursor.sequence > self.cursor.sequence {
            return Err(ComputerStreamError::InvalidCursor);
        }
        if next_liveness_deadline <= now {
            return Err(ComputerStreamError::Expired);
        }
        self.last_observed_at = now;
        self.liveness_deadline = next_liveness_deadline;
        Ok(self.descriptor())
    }

    pub fn reconcile_origin(
        &mut self,
        authorization: ComputerStreamAuthorization,
        now: UtcTimestamp,
        next_liveness_deadline: UtcTimestamp,
    ) -> Result<ComputerStreamDescriptor, ComputerStreamError> {
        if authorization.subject != self.subject {
            return Err(ComputerStreamError::ForgedSubject);
        }
        if authorization.controller != self.controller
            || now < authorization.issued_at
            || now >= authorization.expires_at
        {
            return Err(ComputerStreamError::UnauthorizedController);
        }
        if authorization.origin.server_instance_id == self.origin.server_instance_id
            && authorization.origin.stream_instance_id == self.origin.stream_instance_id
            && authorization.origin.authority_key != self.origin.authority_key
        {
            return Err(ComputerStreamError::ForgedOrigin);
        }
        if authorization.origin.generation <= self.origin.generation
            || next_liveness_deadline <= now
        {
            return Err(ComputerStreamError::GenerationChanged);
        }
        self.origin = authorization.origin;
        self.computer_revision = authorization.computer_revision;
        self.cursor = ComputerStreamCursor {
            generation: self.origin.generation,
            sequence: 0,
        };
        self.next_input_sequence = 0;
        self.last_observed_at = now;
        self.liveness_deadline = next_liveness_deadline;
        Ok(self.descriptor())
    }

    fn ensure_live(&self, now: UtcTimestamp) -> Result<(), ComputerStreamError> {
        if self.is_live(now) {
            Ok(())
        } else {
            Err(ComputerStreamError::Expired)
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub enum ComputerStreamError {
    InvalidBounds,
    UnauthorizedSubject,
    UnauthorizedController,
    ForgedSubject,
    ForgedOrigin,
    OriginChanged,
    GenerationChanged,
    InvalidCursor,
    StaleRevision,
    Expired,
    FrameOutOfBounds,
    InputOutOfBounds,
    SequenceGap { expected: u64, actual: u64 },
    SequenceExhausted,
}

impl fmt::Display for ComputerStreamError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        match self {
            Self::InvalidBounds => formatter.write_str("computer stream bounds are invalid"),
            Self::UnauthorizedSubject => {
                formatter.write_str("computer stream subject is unauthorized")
            }
            Self::UnauthorizedController => {
                formatter.write_str("computer stream controller is unauthorized")
            }
            Self::ForgedSubject => {
                formatter.write_str("computer stream subject does not match the session")
            }
            Self::ForgedOrigin => formatter.write_str("computer stream origin proof is invalid"),
            Self::OriginChanged => formatter.write_str("computer stream origin changed"),
            Self::GenerationChanged => formatter.write_str("computer stream generation changed"),
            Self::InvalidCursor => formatter.write_str("computer stream cursor is invalid"),
            Self::StaleRevision => formatter.write_str("computer revision is stale"),
            Self::Expired => formatter.write_str("computer stream session expired"),
            Self::FrameOutOfBounds => {
                formatter.write_str("computer frame exceeds the negotiated bounds")
            }
            Self::InputOutOfBounds => {
                formatter.write_str("computer input exceeds the negotiated bounds")
            }
            Self::SequenceGap { expected, actual } => {
                write!(
                    formatter,
                    "computer stream sequence gap: expected {expected}, got {actual}"
                )
            }
            Self::SequenceExhausted => formatter.write_str("computer stream sequence is exhausted"),
        }
    }
}

impl std::error::Error for ComputerStreamError {}
