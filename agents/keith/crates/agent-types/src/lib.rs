#![forbid(unsafe_code)]

mod bindings;
mod world;

pub use bindings::{
    BindingContractError, BindingTargetKind, BindingTargetSlot, BindingTaskScope, ObjectBindingKey,
    ObjectBindingReference,
};
pub use world::{CURRENT_WORLD_VERSION, WorldVersion, WorldVersionError};

use std::fmt::{self, Display};
use std::str::FromStr;
use std::time::{SystemTime, UNIX_EPOCH};

use schemars::JsonSchema;
use serde::{Deserialize, Deserializer, Serialize};
use thiserror::Error;

pub const CURRENT_PROTOCOL_VERSION: ProtocolVersion = ProtocolVersion::new(1, 1);
pub const CURRENT_SCHEMA_VERSION: SchemaVersion = SchemaVersion::new(1, 0);

#[derive(
    Clone, Copy, Debug, Eq, Hash, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize,
)]
#[serde(deny_unknown_fields)]
pub struct ProtocolVersion {
    pub major: u16,
    pub minor: u16,
}

impl ProtocolVersion {
    pub const fn new(major: u16, minor: u16) -> Self {
        Self { major, minor }
    }

    pub const fn is_major_compatible_with(self, other: Self) -> bool {
        self.major == other.major
    }

    pub const fn common_minor(self, other: Self) -> Option<Self> {
        if self.is_major_compatible_with(other) {
            Some(Self::new(
                self.major,
                if self.minor < other.minor {
                    self.minor
                } else {
                    other.minor
                },
            ))
        } else {
            None
        }
    }
}

impl Display for ProtocolVersion {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{}.{}", self.major, self.minor)
    }
}

#[derive(
    Clone, Copy, Debug, Eq, Hash, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize,
)]
#[serde(deny_unknown_fields)]
pub struct SchemaVersion {
    pub major: u16,
    pub minor: u16,
}

impl SchemaVersion {
    pub const fn new(major: u16, minor: u16) -> Self {
        Self { major, minor }
    }
}

impl Display for SchemaVersion {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{}.{}", self.major, self.minor)
    }
}

#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum EntityIdError {
    #[error("entity ID must be a canonical 26-character ULID")]
    Invalid,
}

#[derive(Clone, Debug, Eq, Hash, JsonSchema, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(transparent)]
pub struct EntityId(String);

impl EntityId {
    pub fn new() -> Self {
        Self(ulid::Ulid::new().to_string())
    }

    pub fn from_u128(value: u128) -> Self {
        Self(ulid::Ulid::from(value).to_string())
    }

    /// # Errors
    ///
    /// Returns [`EntityIdError::Invalid`] unless the value is a canonical ULID.
    pub fn parse(value: impl Into<String>) -> Result<Self, EntityIdError> {
        let value = value.into();
        let parsed = ulid::Ulid::from_string(&value).map_err(|_| EntityIdError::Invalid)?;
        if value == parsed.to_string() {
            Ok(Self(value))
        } else {
            Err(EntityIdError::Invalid)
        }
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl Default for EntityId {
    fn default() -> Self {
        Self::new()
    }
}

impl Display for EntityId {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter.write_str(&self.0)
    }
}

impl FromStr for EntityId {
    type Err = EntityIdError;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        Self::parse(value)
    }
}

impl TryFrom<String> for EntityId {
    type Error = EntityIdError;

    fn try_from(value: String) -> Result<Self, Self::Error> {
        Self::parse(value)
    }
}

impl<'de> Deserialize<'de> for EntityId {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        let value = String::deserialize(deserializer)?;
        Self::parse(value).map_err(serde::de::Error::custom)
    }
}

macro_rules! typed_ids {
    ($($name:ident),+ $(,)?) => {
        $(
            #[derive(Clone, Debug, Eq, Hash, JsonSchema, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
            #[serde(transparent)]
            pub struct $name(pub EntityId);

            impl $name {
                pub fn new() -> Self {
                    Self(EntityId::new())
                }

                pub fn as_entity_id(&self) -> &EntityId {
                    &self.0
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

            impl From<EntityId> for $name {
                fn from(value: EntityId) -> Self {
                    Self(value)
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

typed_ids!(
    ActionId,
    ArtifactId,
    ChildId,
    ClientId,
    CommandId,
    CommitmentId,
    DeliveryId,
    EntryId,
    GoalId,
    JobId,
    KernelId,
    MessageId,
    ProcessInstanceId,
    ProfileId,
    RootTreeId,
    SessionId,
    ToolCallId,
    TurnId,
    WorkerId,
    WorkspaceId,
);

macro_rules! monotonic_counter {
    ($name:ident) => {
        #[derive(
            Clone,
            Copy,
            Debug,
            Default,
            Eq,
            Hash,
            JsonSchema,
            Ord,
            PartialEq,
            PartialOrd,
            Serialize,
            Deserialize,
        )]
        #[serde(transparent)]
        pub struct $name(pub u64);

        impl $name {
            pub const ZERO: Self = Self(0);

            pub const fn new(value: u64) -> Self {
                Self(value)
            }

            pub const fn get(self) -> u64 {
                self.0
            }

            pub const fn checked_next(self) -> Option<Self> {
                match self.0.checked_add(1) {
                    Some(value) => Some(Self(value)),
                    None => None,
                }
            }
        }
    };
}

monotonic_counter!(Generation);
monotonic_counter!(Revision);
monotonic_counter!(Sequence);
monotonic_counter!(CapabilityEpoch);

#[derive(Clone, Copy, Debug, Eq, Error, PartialEq)]
pub enum TimestampError {
    #[error("timestamp is outside the supported signed millisecond range")]
    OutOfRange,
}

#[derive(
    Clone,
    Copy,
    Debug,
    Default,
    Eq,
    Hash,
    JsonSchema,
    Ord,
    PartialEq,
    PartialOrd,
    Serialize,
    Deserialize,
)]
#[serde(transparent)]
pub struct UtcTimestamp(pub i64);

impl UtcTimestamp {
    pub const UNIX_EPOCH: Self = Self(0);

    pub const fn from_unix_millis(value: i64) -> Self {
        Self(value)
    }

    pub const fn unix_millis(self) -> i64 {
        self.0
    }

    /// # Errors
    ///
    /// Returns [`TimestampError::OutOfRange`] if the host clock cannot fit in signed milliseconds.
    pub fn now() -> Result<Self, TimestampError> {
        Self::from_system_time(SystemTime::now())
    }

    /// # Errors
    ///
    /// Returns [`TimestampError::OutOfRange`] if the instant cannot fit in signed milliseconds.
    pub fn from_system_time(time: SystemTime) -> Result<Self, TimestampError> {
        match time.duration_since(UNIX_EPOCH) {
            Ok(duration) => i64::try_from(duration.as_millis())
                .map(Self)
                .map_err(|_| TimestampError::OutOfRange),
            Err(error) => i64::try_from(error.duration().as_millis())
                .ok()
                .and_then(i64::checked_neg)
                .map(Self)
                .ok_or(TimestampError::OutOfRange),
        }
    }
}

#[derive(Clone, Debug, Eq, Error, PartialEq)]
pub enum TimeZoneNameError {
    #[error("time-zone name must be UTC or a valid slash-separated IANA-style name")]
    Invalid,
}

#[derive(Clone, Debug, Eq, Hash, JsonSchema, Ord, PartialEq, PartialOrd, Serialize)]
#[serde(transparent)]
pub struct TimeZoneName(String);

impl TimeZoneName {
    /// # Errors
    ///
    /// Returns [`TimeZoneNameError::Invalid`] for non-IANA-style names.
    pub fn parse(value: impl Into<String>) -> Result<Self, TimeZoneNameError> {
        let value = value.into();
        let valid_character = |character: char| {
            character.is_ascii_alphanumeric() || matches!(character, '_' | '-' | '+')
        };
        let valid = value == "UTC"
            || (value.contains('/')
                && value
                    .split('/')
                    .all(|part| !part.is_empty() && part.chars().all(valid_character)));
        if valid {
            Ok(Self(value))
        } else {
            Err(TimeZoneNameError::Invalid)
        }
    }

    pub fn as_str(&self) -> &str {
        &self.0
    }
}

impl FromStr for TimeZoneName {
    type Err = TimeZoneNameError;

    fn from_str(value: &str) -> Result<Self, Self::Err> {
        Self::parse(value)
    }
}

impl<'de> Deserialize<'de> for TimeZoneName {
    fn deserialize<D>(deserializer: D) -> Result<Self, D::Error>
    where
        D: Deserializer<'de>,
    {
        let value = String::deserialize(deserializer)?;
        Self::parse(value).map_err(serde::de::Error::custom)
    }
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ZonedTimestamp {
    pub instant: UtcTimestamp,
    pub time_zone: TimeZoneName,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ErrorCode {
    InvalidInput,
    NotFound,
    Conflict,
    Unauthorized,
    Forbidden,
    UnsupportedVersion,
    Unavailable,
    ResourceExhausted,
    DeadlineExceeded,
    Cancelled,
    CorruptState,
    Internal,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CommonError {
    pub version: SchemaVersion,
    pub code: ErrorCode,
    pub message: String,
    pub retryable: bool,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ToolErrorCategory {
    InvalidArguments,
    PolicyDenied,
    ConfirmationDeclined,
    NotReady,
    Cancelled,
    Timeout,
    OutputLimit,
    #[serde(rename = "provider_4xx")]
    Provider4xx,
    #[serde(rename = "provider_5xx")]
    Provider5xx,
    Unavailable,
    Execution,
    WorkerDisconnected,
    Internal,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ToolEffectState {
    NotStarted,
    NotCommitted,
    Committed,
    Unknown,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ToolFailureStatus {
    Error,
    Denied,
    Cancelled,
    TimedOut,
    OutputLimitExceeded,
    NotStarted,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ToolErrorDetail {
    pub category: ToolErrorCategory,
    pub code: String,
    pub reason: String,
    pub detail: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ToolRetryDirective {
    pub automatic: bool,
    pub reason: String,
}

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ToolRecoveryActionKind {
    InspectState,
    Retry,
    UseAlternative,
    InformUser,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ToolRecoveryAction {
    pub action: ToolRecoveryActionKind,
    pub description: String,
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ToolFailure {
    pub status: ToolFailureStatus,
    #[serde(deserialize_with = "deserialize_tool_failure_success")]
    pub success: bool,
    pub error: ToolErrorDetail,
    pub retry: ToolRetryDirective,
    pub effect_state: ToolEffectState,
    pub recovery: Vec<ToolRecoveryAction>,
}

fn deserialize_tool_failure_success<'de, D>(deserializer: D) -> Result<bool, D::Error>
where
    D: serde::Deserializer<'de>,
{
    let success = bool::deserialize(deserializer)?;
    if success {
        return Err(serde::de::Error::custom(
            "a tool failure must deserialize with success=false",
        ));
    }
    Ok(false)
}

impl ToolFailure {
    pub fn execution(detail: impl Into<String>, automatic_retry: bool) -> Self {
        let detail = detail.into();
        Self {
            status: ToolFailureStatus::Error,
            success: false,
            error: ToolErrorDetail {
                category: ToolErrorCategory::Execution,
                code: "TOOL_EXECUTION_FAILED".into(),
                reason: "tool_execution_failed".into(),
                detail,
            },
            retry: ToolRetryDirective {
                automatic: automatic_retry,
                reason: if automatic_retry {
                    "The tool declared this failure safe for automatic retry".into()
                } else {
                    "The tool did not declare this failure safe for automatic retry".into()
                },
            },
            effect_state: ToolEffectState::Unknown,
            recovery: vec![
                ToolRecoveryAction {
                    action: ToolRecoveryActionKind::InspectState,
                    description: "Inspect external state before retrying the operation".into(),
                },
                ToolRecoveryAction {
                    action: ToolRecoveryActionKind::InformUser,
                    description: "Explain the failure and any incomplete work".into(),
                },
            ],
        }
    }

    pub fn not_committed(
        category: ToolErrorCategory,
        code: impl Into<String>,
        reason: impl Into<String>,
        detail: impl Into<String>,
    ) -> Self {
        Self {
            status: ToolFailureStatus::Error,
            success: false,
            error: ToolErrorDetail {
                category,
                code: code.into(),
                reason: reason.into(),
                detail: detail.into(),
            },
            retry: ToolRetryDirective {
                automatic: false,
                reason: "Automatic retry was not authorized for this failure".into(),
            },
            effect_state: ToolEffectState::NotCommitted,
            recovery: vec![ToolRecoveryAction {
                action: ToolRecoveryActionKind::InformUser,
                description: "Explain the failure and any incomplete work".into(),
            }],
        }
    }
}

impl CommonError {
    pub fn new(code: ErrorCode, message: impl Into<String>, retryable: bool) -> Self {
        Self {
            version: CURRENT_SCHEMA_VERSION,
            code,
            message: message.into(),
            retryable,
        }
    }
}

impl Display for CommonError {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        write!(formatter, "{:?}: {}", self.code, self.message)
    }
}

impl std::error::Error for CommonError {}

/// # Errors
///
/// Returns an error if `value` cannot be represented as JSON.
pub fn canonical_json_bytes<T: Serialize>(value: &T) -> Result<Vec<u8>, serde_json::Error> {
    let mut value = serde_json::to_value(value)?;
    sort_json_value(&mut value);
    serde_json::to_vec(&value)
}

fn sort_json_value(value: &mut serde_json::Value) {
    match value {
        serde_json::Value::Array(values) => values.iter_mut().for_each(sort_json_value),
        serde_json::Value::Object(values) => {
            let old_values = std::mem::take(values);
            let mut entries: Vec<_> = old_values.into_iter().collect();
            entries.sort_unstable_by(|left, right| left.0.cmp(&right.0));
            for (key, mut value) in entries {
                sort_json_value(&mut value);
                values.insert(key, value);
            }
        }
        _ => {}
    }
}

#[derive(Clone, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct CommonCompatibilityFixture {
    pub schema: SchemaVersion,
    pub protocol: ProtocolVersion,
    pub session_id: SessionId,
    pub created_at: ZonedTimestamp,
    pub error: CommonError,
}

#[derive(JsonSchema)]
pub struct CommonTypesSchema {
    pub fixture: CommonCompatibilityFixture,
    pub action_id: ActionId,
    pub artifact_id: ArtifactId,
    pub child_id: ChildId,
    pub client_id: ClientId,
    pub command_id: CommandId,
    pub commitment_id: CommitmentId,
    pub delivery_id: DeliveryId,
    pub entry_id: EntryId,
    pub goal_id: GoalId,
    pub job_id: JobId,
    pub message_id: MessageId,
    pub process_instance_id: ProcessInstanceId,
    pub profile_id: ProfileId,
    pub root_tree_id: RootTreeId,
    pub tool_call_id: ToolCallId,
    pub turn_id: TurnId,
    pub worker_id: WorkerId,
    pub workspace_id: WorkspaceId,
    pub generation: Generation,
    pub revision: Revision,
    pub sequence: Sequence,
    pub tool_failure: ToolFailure,
}

/// # Errors
///
/// Returns an error if the generated schema cannot be represented as JSON.
pub fn schema_markdown() -> Result<String, serde_json::Error> {
    let schema = schemars::schema_for!(CommonTypesSchema);
    let json = serde_json::to_string_pretty(&schema)?;
    Ok(format!(
        "# Keith common type schema\n\nGenerated from `keith-agent-types` {}. Do not edit by hand.\n\n```json\n{json}\n```\n",
        env!("CARGO_PKG_VERSION")
    ))
}

#[cfg(test)]
mod tests {
    use super::*;
    use proptest::prelude::*;
    use serde_json::json;

    proptest! {
        #[test]
        fn entity_ids_round_trip(raw in any::<u128>()) {
            let text = ulid::Ulid::from(raw).to_string();
            let id = EntityId::parse(text.clone()).expect("ULID text is canonical");
            prop_assert_eq!(id.as_str(), text);
            let json = serde_json::to_string(&id).expect("serialize entity ID");
            let decoded: EntityId = serde_json::from_str(&json).expect("deserialize entity ID");
            prop_assert_eq!(decoded, id);
        }

        #[test]
        fn counters_round_trip(value in any::<u64>()) {
            let sequence = Sequence::new(value);
            let json = serde_json::to_string(&sequence).expect("serialize sequence");
            let decoded: Sequence = serde_json::from_str(&json).expect("deserialize sequence");
            prop_assert_eq!(decoded, sequence);
            prop_assert_eq!(sequence.checked_next().map(Sequence::get), value.checked_add(1));
        }

        #[test]
        fn utc_instant_and_zone_remain_separate(
            millis in any::<i64>(),
            zone in prop_oneof![Just("UTC"), Just("Europe/Berlin"), Just("America/New_York")],
        ) {
            let value = ZonedTimestamp {
                instant: UtcTimestamp::from_unix_millis(millis),
                time_zone: TimeZoneName::parse(zone).expect("known valid time-zone name"),
            };
            let json = serde_json::to_value(&value).expect("serialize zoned timestamp");
            prop_assert_eq!(&json["instant"], &json!(millis));
            prop_assert_eq!(&json["time_zone"], &json!(zone));
            let decoded: ZonedTimestamp = serde_json::from_value(json).expect("deserialize zoned timestamp");
            prop_assert_eq!(decoded, value);
        }
    }

    #[test]
    fn compatibility_fixture_remains_stable() {
        let fixture = include_str!("../tests/fixtures/common-v1.json");
        let decoded: CommonCompatibilityFixture =
            serde_json::from_str(fixture).expect("version-one fixture must remain readable");
        assert_eq!(decoded.schema, CURRENT_SCHEMA_VERSION);
        assert_eq!(decoded.protocol, ProtocolVersion::new(1, 0));
        let canonical = canonical_json_bytes(&decoded).expect("canonical fixture");
        let original: serde_json::Value = serde_json::from_str(fixture).expect("fixture JSON");
        let expected = canonical_json_bytes(&original).expect("canonical fixture JSON");
        assert_eq!(canonical, expected);
    }

    #[test]
    fn canonical_json_sorts_nested_object_keys() {
        let left = json!({"z": 1, "a": {"d": 4, "b": 2}});
        let right = json!({"a": {"b": 2, "d": 4}, "z": 1});
        assert_eq!(
            canonical_json_bytes(&left).expect("canonical left"),
            canonical_json_bytes(&right).expect("canonical right")
        );
    }

    #[test]
    fn closed_types_reject_unknown_fields_and_variants() {
        let version = r#"{"major":1,"minor":0,"patch":1}"#;
        assert!(serde_json::from_str::<ProtocolVersion>(version).is_err());

        let error = r#"{"version":{"major":1,"minor":0},"code":"future_error","message":"no","retryable":false}"#;
        assert!(serde_json::from_str::<CommonError>(error).is_err());
    }

    #[test]
    fn tool_failure_round_trips_status_success_error_retry_effect_and_recovery() {
        let encoded = r#"{
            "status":"error",
            "success":false,
            "error":{
                "category":"provider_5xx",
                "code":"HTTP_502",
                "reason":"server_timed_out",
                "detail":"Upstream server timed out before returning a response"
            },
            "retry":{
                "automatic":false,
                "reason":"The tool declared this failure unsafe for an identical retry"
            },
            "effect_state":"not_committed",
            "recovery":[{
                "action":"use_alternative",
                "description":"Use browser or another authoritative source"
            }]
        }"#;
        let failure: ToolFailure = serde_json::from_str(encoded).unwrap();
        assert_eq!(failure.status, ToolFailureStatus::Error);
        assert!(!failure.success);
        assert_eq!(failure.error.category, ToolErrorCategory::Provider5xx);
        assert_eq!(failure.error.code, "HTTP_502");
        assert!(!failure.retry.automatic);
        assert_eq!(failure.effect_state, ToolEffectState::NotCommitted);
        assert_eq!(
            failure.recovery[0].action,
            ToolRecoveryActionKind::UseAlternative
        );
        let value = serde_json::to_value(&failure).unwrap();
        assert_eq!(value["status"], "error");
        assert_eq!(value["success"], false);

        let mut invalid = value;
        invalid["success"] = json!(true);
        assert!(serde_json::from_value::<ToolFailure>(invalid).is_err());
    }

    #[test]
    fn identifiers_reject_noncanonical_or_unsafe_text() {
        assert!(EntityId::parse("../../secret").is_err());
        assert!(EntityId::parse("01arz3ndektsv4rrffq69g5fav").is_err());
        assert!(EntityId::parse("01ARZ3NDEKTSV4RRFFQ69G5FAV").is_ok());
    }
}
