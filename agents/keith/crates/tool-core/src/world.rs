use std::collections::BTreeSet;

use keith_agent_types::{
    ActionId, CapabilityEpoch, CommitmentId, EntityId, GoalId, ProfileId, Revision,
    ToolEffectState, WorkspaceId, WorldVersion,
};
use serde::{Deserialize, Serialize};
use thiserror::Error;

const MAX_FRAME_REFERENCES: usize = 128;
const MAX_PROPERTY_BYTES: usize = 256;

/// Origin does not confer authority. Learned regularities never update native laws.
#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum CausalRuleOrigin {
    RuntimeLaw,
    AdapterGuarantee,
    LearnedRegularity,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ObjectRevision {
    pub entity_id: EntityId,
    pub property: String,
    pub revision: Revision,
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub evidence_id: Option<EntityId>,
}

/// References the existing operation owner; this is not a second operation record.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct PendingEffectReference {
    pub operation_id: EntityId,
    pub effect_state: ToolEffectState,
}

/// Bounded admission context. Current authority must still be checked by the
/// existing admission owner; possession of this snapshot grants no permission.
/// An absent legacy frame must remain absent rather than default to this version.
#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields, try_from = "WorldFrameWire")]
pub struct WorldFrame {
    pub world_version: WorldVersion,
    pub capability_epoch: CapabilityEpoch,
    pub profile_id: ProfileId,
    pub workspace_id: WorkspaceId,
    pub action_id: ActionId,
    /// Goal continuations retain this identity when their queued action changes.
    #[serde(default, skip_serializing_if = "Option::is_none")]
    pub goal_id: Option<GoalId>,
    pub object_revisions: Vec<ObjectRevision>,
    pub active_commitments: Vec<CommitmentId>,
    pub pending_effects: Vec<PendingEffectReference>,
}

#[derive(Deserialize)]
#[serde(deny_unknown_fields)]
struct WorldFrameWire {
    world_version: WorldVersion,
    capability_epoch: CapabilityEpoch,
    profile_id: ProfileId,
    workspace_id: WorkspaceId,
    action_id: ActionId,
    #[serde(default)]
    goal_id: Option<GoalId>,
    object_revisions: Vec<ObjectRevision>,
    active_commitments: Vec<CommitmentId>,
    pending_effects: Vec<PendingEffectReference>,
}

#[derive(Clone, Copy, Debug, Eq, Error, PartialEq)]
pub enum WorldFrameError {
    #[error("world frame exceeds reference limits")]
    TooLarge,
    #[error("world frame contains an invalid object property")]
    InvalidProperty,
    #[error("world frame contains a duplicate reference")]
    DuplicateReference,
    #[error("a committed effect cannot be represented as pending")]
    CommittedEffect,
}

impl WorldFrame {
    /// # Errors
    ///
    /// Rejects unbounded, ambiguous or inconsistent context. This checks record
    /// shape, not freshness, grants, object existence or evidence validity.
    pub fn validate(&self) -> Result<(), WorldFrameError> {
        if self.object_revisions.len() > MAX_FRAME_REFERENCES
            || self.active_commitments.len() > MAX_FRAME_REFERENCES
            || self.pending_effects.len() > MAX_FRAME_REFERENCES
        {
            return Err(WorldFrameError::TooLarge);
        }
        let mut objects = BTreeSet::new();
        for object in &self.object_revisions {
            if object.property.trim().is_empty()
                || object.property.trim() != object.property
                || object.property.len() > MAX_PROPERTY_BYTES
                || object.property.chars().any(char::is_control)
            {
                return Err(WorldFrameError::InvalidProperty);
            }
            if !objects.insert((&object.entity_id, &object.property)) {
                return Err(WorldFrameError::DuplicateReference);
            }
        }
        let commitments = self.active_commitments.iter().collect::<BTreeSet<_>>();
        let operations = self
            .pending_effects
            .iter()
            .map(|effect| &effect.operation_id)
            .collect::<BTreeSet<_>>();
        if commitments.len() != self.active_commitments.len()
            || operations.len() != self.pending_effects.len()
        {
            return Err(WorldFrameError::DuplicateReference);
        }
        if self
            .pending_effects
            .iter()
            .any(|effect| effect.effect_state == ToolEffectState::Committed)
        {
            return Err(WorldFrameError::CommittedEffect);
        }
        Ok(())
    }
}

impl TryFrom<WorldFrameWire> for WorldFrame {
    type Error = WorldFrameError;

    fn try_from(value: WorldFrameWire) -> Result<Self, Self::Error> {
        let frame = Self {
            world_version: value.world_version,
            capability_epoch: value.capability_epoch,
            profile_id: value.profile_id,
            workspace_id: value.workspace_id,
            action_id: value.action_id,
            goal_id: value.goal_id,
            object_revisions: value.object_revisions,
            active_commitments: value.active_commitments,
            pending_effects: value.pending_effects,
        };
        frame.validate()?;
        Ok(frame)
    }
}

#[cfg(test)]
mod tests {
    use keith_agent_types::CURRENT_WORLD_VERSION;

    use super::*;

    fn frame() -> WorldFrame {
        WorldFrame {
            world_version: CURRENT_WORLD_VERSION,
            capability_epoch: CapabilityEpoch::new(3),
            profile_id: ProfileId::new(),
            workspace_id: WorkspaceId::new(),
            action_id: ActionId::new(),
            goal_id: Some(GoalId::new()),
            object_revisions: vec![ObjectRevision {
                entity_id: EntityId::new(),
                property: "endpoint".into(),
                revision: Revision::new(2),
                evidence_id: None,
            }],
            active_commitments: vec![CommitmentId::new()],
            pending_effects: vec![PendingEffectReference {
                operation_id: EntityId::new(),
                effect_state: ToolEffectState::Unknown,
            }],
        }
    }

    fn decode(frame: &WorldFrame) -> Result<WorldFrame, serde_json::Error> {
        serde_json::from_slice(&serde_json::to_vec(frame).expect("serialize frame"))
    }

    #[test]
    fn world_frame_roundtrip_preserves_scope_revisions_and_unknown_effect() {
        let frame = frame();
        assert_eq!(decode(&frame).expect("valid frame"), frame);
        let value = serde_json::to_value(&frame).expect("JSON");
        assert!(value["object_revisions"][0].get("evidence_id").is_none());
    }

    #[test]
    fn goal_lineage_survives_a_new_action_without_inventing_absent_goals() {
        let original = frame();
        let mut continuation = original.clone();
        continuation.action_id = ActionId::new();
        let recovered = decode(&continuation).expect("continuation");
        assert_ne!(recovered.action_id, original.action_id);
        assert_eq!(recovered.goal_id, original.goal_id);
        continuation.goal_id = None;
        let value = serde_json::to_value(&continuation).expect("JSON");
        assert!(value.get("goal_id").is_none());
        assert!(
            decode(&continuation)
                .expect("ordinary action")
                .goal_id
                .is_none()
        );
    }

    #[test]
    fn unknown_or_missing_world_contract_is_not_silently_admitted() {
        let mut value = serde_json::to_value(frame()).expect("JSON");
        value["world_version"]["major"] = 9.into();
        assert!(serde_json::from_value::<WorldFrame>(value.clone()).is_err());
        value
            .as_object_mut()
            .expect("object")
            .remove("world_version");
        assert!(serde_json::from_value::<WorldFrame>(value).is_err());
    }

    #[test]
    fn conflicting_object_revisions_and_duplicate_operations_are_rejected() {
        let mut frame = frame();
        let mut other = frame.object_revisions[0].clone();
        other.revision = Revision::new(99);
        frame.object_revisions.push(other);
        assert_eq!(frame.validate(), Err(WorldFrameError::DuplicateReference));
        assert!(decode(&frame).is_err());
        frame.object_revisions.pop();
        frame.pending_effects.push(frame.pending_effects[0].clone());
        assert!(decode(&frame).is_err());
    }

    #[test]
    fn duplicate_commitments_and_committed_pending_effects_are_rejected() {
        let mut frame = frame();
        frame
            .active_commitments
            .push(frame.active_commitments[0].clone());
        assert!(decode(&frame).is_err());
        frame.active_commitments.pop();
        frame.pending_effects[0].effect_state = ToolEffectState::Committed;
        assert_eq!(frame.validate(), Err(WorldFrameError::CommittedEffect));
        assert!(decode(&frame).is_err());
    }

    #[test]
    fn property_and_reference_limits_are_enforced_on_decode() {
        let mut frame = frame();
        for property in [
            String::new(),
            "\nendpoint".into(),
            " endpoint".into(),
            "endpoint ".into(),
            "x".repeat(257),
        ] {
            frame.object_revisions[0].property = property;
            assert!(decode(&frame).is_err());
        }
        frame.object_revisions = vec![frame.object_revisions[0].clone(); 129];
        assert_eq!(frame.validate(), Err(WorldFrameError::TooLarge));
        assert!(decode(&frame).is_err());
    }

    #[test]
    fn learned_regularities_and_native_laws_have_distinct_wire_values() {
        for origin in [
            CausalRuleOrigin::RuntimeLaw,
            CausalRuleOrigin::AdapterGuarantee,
            CausalRuleOrigin::LearnedRegularity,
        ] {
            let encoded = serde_json::to_string(&origin).expect("origin");
            assert_eq!(
                serde_json::from_str::<CausalRuleOrigin>(&encoded).expect("origin"),
                origin
            );
        }
        assert_ne!(
            serde_json::to_value(CausalRuleOrigin::RuntimeLaw).expect("law"),
            serde_json::to_value(CausalRuleOrigin::LearnedRegularity).expect("learned")
        );
        assert!(serde_json::from_str::<CausalRuleOrigin>("\"model_authority\"").is_err());
    }
}
