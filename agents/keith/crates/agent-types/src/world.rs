use std::fmt::{self, Display};

use schemars::JsonSchema;
use serde::{Deserialize, Serialize};
use thiserror::Error;

use crate::SchemaVersion;

/// Identity of the native action schemas, transitions and outcome vocabulary.
/// This is independent of the transport protocol and installed capabilities.
pub const CURRENT_WORLD_VERSION: WorldVersion = WorldVersion(SchemaVersion::new(1, 0));

#[derive(Clone, Copy, Debug, Eq, JsonSchema, PartialEq, Serialize, Deserialize)]
#[serde(try_from = "SchemaVersion", into = "SchemaVersion")]
pub struct WorldVersion(SchemaVersion);

impl WorldVersion {
    /// # Errors
    ///
    /// Rejects a native contract this reader does not implement.
    pub fn new(major: u16, minor: u16) -> Result<Self, WorldVersionError> {
        Self::try_from(SchemaVersion::new(major, minor))
    }

    pub const fn schema_version(self) -> SchemaVersion {
        self.0
    }
}

#[derive(Clone, Copy, Debug, Eq, Error, PartialEq)]
#[error("unsupported native world contract {0}")]
pub struct WorldVersionError(pub SchemaVersion);

impl TryFrom<SchemaVersion> for WorldVersion {
    type Error = WorldVersionError;

    fn try_from(version: SchemaVersion) -> Result<Self, Self::Error> {
        // Reader compatibility is explicit and independent of the version
        // selected for new admissions. Future writers must preserve readers
        // for pending operations rather than redefine their original contract.
        match version {
            SchemaVersion { major: 1, minor: 0 } => Ok(Self(version)),
            _ => Err(WorldVersionError(version)),
        }
    }
}

impl From<WorldVersion> for SchemaVersion {
    fn from(version: WorldVersion) -> Self {
        version.0
    }
}

impl Display for WorldVersion {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        self.0.fmt(formatter)
    }
}

#[cfg(test)]
mod tests {
    use super::*;
    use crate::CapabilityEpoch;

    #[test]
    fn current_world_roundtrips_without_changing_protocol_schema() {
        let encoded = serde_json::to_string(&CURRENT_WORLD_VERSION).expect("encode version");
        assert_eq!(encoded, r#"{"major":1,"minor":0}"#);
        assert_eq!(
            serde_json::from_str::<WorldVersion>(&encoded).expect("decode version"),
            CURRENT_WORLD_VERSION
        );
        assert_eq!(CURRENT_WORLD_VERSION.to_string(), "1.0");
    }

    #[test]
    fn unsupported_or_missing_world_versions_are_not_current_by_default() {
        for value in [
            r#"{"major":0,"minor":0}"#,
            r#"{"major":1,"minor":1}"#,
            r#"{"major":2,"minor":0}"#,
            r#"{"major":1}"#,
            r#"{"major":1,"minor":0,"learned":true}"#,
            "null",
        ] {
            assert!(serde_json::from_str::<WorldVersion>(value).is_err());
        }
        assert!(WorldVersion::new(2, 0).is_err());
    }

    #[test]
    fn capability_epoch_is_monotonic_and_overflow_is_explicit() {
        let epoch = CapabilityEpoch::ZERO.checked_next().expect("first epoch");
        assert_eq!(epoch.get(), 1);
        assert!(CapabilityEpoch::new(u64::MAX).checked_next().is_none());
        assert_eq!(
            serde_json::from_str::<CapabilityEpoch>("17").expect("epoch"),
            CapabilityEpoch::new(17)
        );
    }
}
