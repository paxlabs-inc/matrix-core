//! Shared identities for exact memory bindings and session-owned dependencies.
//! These values identify requirements; they grant no authority and certify no claim.

use crate::{ActionId, EntityId, GoalId, ProfileId, Revision, SessionId, WorkspaceId};
use serde::{Deserialize, Serialize};
use thiserror::Error;

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(try_from = "RawObjectBindingKey")]
pub struct ObjectBindingKey {
    pub entity_id: EntityId,
    pub property: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawObjectBindingKey {
    entity_id: EntityId,
    property: String,
}

impl TryFrom<RawObjectBindingKey> for ObjectBindingKey {
    type Error = BindingContractError;

    fn try_from(raw: RawObjectBindingKey) -> Result<Self, Self::Error> {
        let key = Self {
            entity_id: raw.entity_id,
            property: raw.property,
        };
        key.validate()?;
        Ok(key)
    }
}

impl ObjectBindingKey {
    /// # Errors
    ///
    /// Rejects empty, oversized, or noncanonical property names. Aliases are not keys.
    pub fn validate(&self) -> Result<(), BindingContractError> {
        validate_name(&self.property)
    }
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct BindingTaskScope {
    pub profile_id: ProfileId,
    pub workspace_id: WorkspaceId,
    pub session_id: SessionId,
    pub action_id: ActionId,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub goal_id: Option<GoalId>,
}

impl BindingTaskScope {
    /// Goal continuations retain logical identity across action IDs. An ordinary
    /// action stays scoped to its original session. This does not authorize access
    /// to another session's records.
    pub fn same_task(&self, other: &Self) -> bool {
        self.profile_id == other.profile_id
            && self.workspace_id == other.workspace_id
            && match (&self.goal_id, &other.goal_id) {
                (Some(left), Some(right)) => left == right,
                (None, None) => {
                    self.action_id == other.action_id && self.session_id == other.session_id
                }
                _ => false,
            }
    }

    /// # Errors
    ///
    /// Rejects a scope that does not belong to the supplied runtime envelope.
    pub fn validate_for(
        &self,
        profile_id: &ProfileId,
        workspace_id: &WorkspaceId,
        session_id: &SessionId,
    ) -> Result<(), BindingContractError> {
        if &self.profile_id != profile_id
            || &self.workspace_id != workspace_id
            || &self.session_id != session_id
        {
            return Err(BindingContractError::ScopeMismatch);
        }
        Ok(())
    }
}

/// The evidence reference always names the original quoted source. The binding's
/// canonical owner separately retains the owning memory record and exact span.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(try_from = "RawObjectBindingReference")]
pub struct ObjectBindingReference {
    pub key: ObjectBindingKey,
    pub binding_id: EntityId,
    pub revision: Revision,
    pub evidence_id: EntityId,
    pub evidence_digest: String,
    pub value_digest: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawObjectBindingReference {
    key: ObjectBindingKey,
    binding_id: EntityId,
    revision: Revision,
    evidence_id: EntityId,
    evidence_digest: String,
    value_digest: String,
}

impl TryFrom<RawObjectBindingReference> for ObjectBindingReference {
    type Error = BindingContractError;

    fn try_from(raw: RawObjectBindingReference) -> Result<Self, Self::Error> {
        let reference = Self {
            key: raw.key,
            binding_id: raw.binding_id,
            revision: raw.revision,
            evidence_id: raw.evidence_id,
            evidence_digest: raw.evidence_digest,
            value_digest: raw.value_digest,
        };
        reference.validate()?;
        Ok(reference)
    }
}

impl ObjectBindingReference {
    /// # Errors
    ///
    /// Rejects malformed keys/digests and an uncommitted zero revision. A valid
    /// reference must still be resolved in its current canonical profile store.
    pub fn validate(&self) -> Result<(), BindingContractError> {
        self.key.validate()?;
        if self.revision == Revision::ZERO
            || !valid_digest(&self.evidence_digest)
            || !valid_digest(&self.value_digest)
        {
            return Err(BindingContractError::InvalidReference);
        }
        Ok(())
    }
}

#[derive(Clone, Copy, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum BindingTargetKind {
    WorkspacePath,
    HttpUrl,
    Literal,
}

/// A runtime-approved adapter argument slot. Parsing this shape alone does not
/// establish that the named adapter can attest use of a bound target.
#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(try_from = "RawBindingTargetSlot")]
pub struct BindingTargetSlot {
    pub kind: BindingTargetKind,
    pub tool_name: String,
    pub argument_name: String,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct RawBindingTargetSlot {
    kind: BindingTargetKind,
    tool_name: String,
    argument_name: String,
}

impl TryFrom<RawBindingTargetSlot> for BindingTargetSlot {
    type Error = BindingContractError;

    fn try_from(raw: RawBindingTargetSlot) -> Result<Self, Self::Error> {
        let slot = Self {
            kind: raw.kind,
            tool_name: raw.tool_name,
            argument_name: raw.argument_name,
        };
        slot.validate()?;
        Ok(slot)
    }
}

impl BindingTargetSlot {
    /// # Errors
    ///
    /// Rejects noncanonical or unbounded names. Runtime admission separately
    /// validates the exact kind/tool/argument combination it actually supports.
    pub fn validate(&self) -> Result<(), BindingContractError> {
        validate_name(&self.tool_name)?;
        validate_name(&self.argument_name)
    }
}

fn validate_name(value: &str) -> Result<(), BindingContractError> {
    if value.is_empty()
        || value.len() > 128
        || !value.as_bytes()[0].is_ascii_lowercase()
        || !value.bytes().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || b"_.-".contains(&byte)
        })
    {
        return Err(BindingContractError::InvalidName);
    }
    Ok(())
}

fn valid_digest(value: &str) -> bool {
    value.len() == 64
        && value
            .bytes()
            .all(|byte| byte.is_ascii_digit() || (b'a'..=b'f').contains(&byte))
}

#[derive(Clone, Copy, Debug, Eq, Error, PartialEq)]
pub enum BindingContractError {
    #[error("binding property or adapter slot has an invalid name")]
    InvalidName,
    #[error("binding reference has an invalid digest or revision")]
    InvalidReference,
    #[error("binding task does not belong to the runtime scope")]
    ScopeMismatch,
}

#[cfg(test)]
mod tests {
    use super::*;

    fn scope() -> BindingTaskScope {
        BindingTaskScope {
            profile_id: ProfileId::new(),
            workspace_id: WorkspaceId::new(),
            session_id: SessionId::new(),
            action_id: ActionId::new(),
            goal_id: None,
        }
    }

    #[test]
    fn goal_identity_survives_continuation_without_cross_scope_authority() {
        let mut original = scope();
        let mut next = original.clone();
        next.action_id = ActionId::new();
        assert!(!original.same_task(&next));
        original.goal_id = Some(GoalId::new());
        next.goal_id = original.goal_id.clone();
        assert!(original.same_task(&next));
        next.session_id = SessionId::new();
        assert!(original.same_task(&next));
        assert!(
            next.validate_for(
                &original.profile_id,
                &original.workspace_id,
                &original.session_id
            )
            .is_err()
        );
        next.workspace_id = WorkspaceId::new();
        assert!(!original.same_task(&next));
        let mut other_profile = original.clone();
        other_profile.profile_id = ProfileId::new();
        assert!(!original.same_task(&other_profile));
    }

    #[test]
    fn serialized_keys_and_references_refuse_unchecked_identity_fields() {
        let reference = ObjectBindingReference {
            key: ObjectBindingKey {
                entity_id: EntityId::new(),
                property: "api.endpoint".into(),
            },
            binding_id: EntityId::new(),
            revision: Revision::new(1),
            evidence_id: EntityId::new(),
            evidence_digest: "a".repeat(64),
            value_digest: "b".repeat(64),
        };
        let encoded = serde_json::to_value(&reference).unwrap();
        assert_eq!(
            serde_json::from_value::<ObjectBindingReference>(encoded.clone()).unwrap(),
            reference
        );
        for name in ["", "Api", "api endpoint", "../endpoint", "e\u{301}ndpoint"] {
            let mut invalid = encoded.clone();
            invalid["key"]["property"] = name.into();
            assert!(serde_json::from_value::<ObjectBindingReference>(invalid).is_err());
        }
        for field in ["evidence_digest", "value_digest"] {
            let mut invalid = encoded.clone();
            invalid[field] = "A".repeat(64).into();
            assert!(serde_json::from_value::<ObjectBindingReference>(invalid).is_err());
        }
        let mut invalid = encoded.clone();
        invalid["revision"] = serde_json::to_value(Revision::ZERO).unwrap();
        assert!(serde_json::from_value::<ObjectBindingReference>(invalid).is_err());
        let mut unknown = encoded;
        unknown["approved"] = true.into();
        assert!(serde_json::from_value::<ObjectBindingReference>(unknown).is_err());
    }

    #[test]
    fn adapter_slots_are_bounded_shapes_and_absent_goal_stays_absent() {
        let slot = BindingTargetSlot {
            kind: BindingTargetKind::HttpUrl,
            tool_name: "web_fetch".into(),
            argument_name: "url".into(),
        };
        let mut value = serde_json::to_value(&slot).unwrap();
        assert_eq!(
            serde_json::from_value::<BindingTargetSlot>(value.clone()).unwrap(),
            slot
        );
        value["argument_name"] = "x".repeat(129).into();
        assert!(serde_json::from_value::<BindingTargetSlot>(value).is_err());
        let value = serde_json::to_value(scope()).unwrap();
        assert!(value.get("goal_id").is_none());
        assert!(serde_json::from_value::<BindingTaskScope>(value).is_ok());
    }
}
