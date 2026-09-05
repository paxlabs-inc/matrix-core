//! Durable task requirements and frozen binding use. Resolution remains with the
//! memory owner; these records attest the set and call that runtime admission used.

use std::collections::BTreeSet;

use keith_agent_types::{
    BindingTargetSlot, BindingTaskScope, CURRENT_SCHEMA_VERSION, ObjectBindingKey,
    ObjectBindingReference, SchemaVersion, ToolCallId, TurnId, UtcTimestamp,
    canonical_json_bytes,
};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::{HISTORY_FILE, SessionEntry, SessionEntryPayload, SessionStoreError, SessionWriter,
    parse_complete_history};

pub const MAX_REQUIRED_OBJECT_BINDINGS: usize = 128;

#[derive(Clone, Debug, Eq, Ord, PartialEq, PartialOrd, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RequiredObjectBinding {
    pub key: ObjectBindingKey,
    pub target: BindingTargetSlot,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct RequiredObjectBindings {
    pub version: SchemaVersion,
    pub scope: BindingTaskScope,
    pub required: Vec<RequiredObjectBinding>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FrozenObjectBindingUse {
    pub reference: ObjectBindingReference,
    pub target: BindingTargetSlot,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct FrozenBindingAdmission {
    pub version: SchemaVersion,
    pub scope: BindingTaskScope,
    pub turn_id: TurnId,
    pub call_id: ToolCallId,
    pub tool_name: String,
    pub arguments_digest: String,
    pub required: Vec<RequiredObjectBinding>,
    pub bindings: Vec<FrozenObjectBindingUse>,
    pub admitted_at: UtcTimestamp,
}

/// # Errors
///
/// Returns an error if arguments cannot be represented as canonical JSON.
pub fn binding_arguments_digest(arguments: &serde_json::Value) -> Result<String, SessionStoreError> {
    Ok(format!("{:x}", Sha256::digest(canonical_json_bytes(arguments)?)))
}

fn invalid(reason: &str) -> SessionStoreError {
    SessionStoreError::InvalidBindingAdmission(reason.into())
}

fn checked_requirements(
    required: &[RequiredObjectBinding],
) -> Result<BTreeSet<RequiredObjectBinding>, SessionStoreError> {
    if required.len() > MAX_REQUIRED_OBJECT_BINDINGS {
        return Err(invalid("required binding set exceeds its bound"));
    }
    let mut result = BTreeSet::new();
    for item in required {
        item.key.validate().map_err(|_| invalid("invalid required key"))?;
        item.target.validate().map_err(|_| invalid("invalid target slot"))?;
        if !result.insert(item.clone()) {
            return Err(invalid("required binding set contains duplicates"));
        }
    }
    Ok(result)
}

impl SessionWriter {
    fn validate_binding_scope(&self, scope: &BindingTaskScope) -> Result<(), SessionStoreError> {
        scope.validate_for(&self.manifest.profile_id, &self.manifest.workspace_id,
            &self.manifest.session_id).map_err(|_| invalid("binding scope does not match session"))
    }

    /// Reads the union from complete committed history, including compacted branches.
    /// This uses the session owner's existing full-history read, not a bounded index.
    ///
    /// # Errors
    ///
    /// Rejects scope mismatch, corrupt history, or malformed requirement records.
    pub fn required_object_bindings(
        &self,
        scope: &BindingTaskScope,
    ) -> Result<Vec<RequiredObjectBinding>, SessionStoreError> {
        self.validate_binding_scope(scope)?;
        let index = parse_complete_history(&self.directory.join(HISTORY_FILE))?;
        let mut required = BTreeSet::new();
        for entry in index.entries.values() {
            if let SessionEntryPayload::RequiredObjectBindings { record } = &entry.payload {
                self.validate_binding_scope(&record.scope)?;
                if record.version != CURRENT_SCHEMA_VERSION {
                    return Err(invalid("unsupported required binding version"));
                }
                let keys = checked_requirements(&record.required)?;
                if record.scope.same_task(scope) {
                    required.extend(keys);
                }
            }
        }
        if required.len() > MAX_REQUIRED_OBJECT_BINDINGS {
            return Err(invalid("durable required binding union exceeds its bound"));
        }
        Ok(required.into_iter().collect())
    }

    /// Adds requirements. Empty or omitted entries cannot remove an existing dependency.
    ///
    /// # Errors
    ///
    /// Rejects invalid scope/keys, an oversized union, or failed durable persistence.
    pub fn require_object_bindings(
        &mut self,
        scope: BindingTaskScope,
        required: Vec<RequiredObjectBinding>,
        at: UtcTimestamp,
    ) -> Result<SessionEntry, SessionStoreError> {
        self.ensure_writable()?;
        let mut union = self.required_object_bindings(&scope)?.into_iter().collect::<BTreeSet<_>>();
        union.extend(checked_requirements(&required)?);
        if union.len() > MAX_REQUIRED_OBJECT_BINDINGS {
            return Err(invalid("durable required binding union exceeds its bound"));
        }
        let record = RequiredObjectBindings {
            version: CURRENT_SCHEMA_VERSION,
            scope,
            required: union.into_iter().collect(),
        };
        self.append(self.manifest.active_leaf.clone(), at,
            SessionEntryPayload::RequiredObjectBindings { record })
    }

    /// Freezes selected binding references against the actual durable tool intent.
    /// Runtime admission owns resolution and target-value interpretation; this method
    /// validates scope, complete requirements, selected slots and exact call identity.
    ///
    /// # Errors
    ///
    /// Rejects omission, malformed references, conflicting admission, mismatched tool
    /// intent, or failed persistence. A successful return precedes tool dispatch.
    #[allow(clippy::too_many_lines)]
    pub fn append_binding_admission(
        &mut self,
        admission: FrozenBindingAdmission,
    ) -> Result<SessionEntry, SessionStoreError> {
        self.ensure_writable()?;
        self.validate_binding_scope(&admission.scope)?;
        if admission.version != CURRENT_SCHEMA_VERSION {
            return Err(invalid("unsupported binding admission version"));
        }
        let required = checked_requirements(&admission.required)?;
        let durable = self.required_object_bindings(&admission.scope)?.into_iter().collect::<BTreeSet<_>>();
        if required != durable {
            return Err(invalid("admission omitted or invented a durable requirement"));
        }
        if admission.bindings.len() > MAX_REQUIRED_OBJECT_BINDINGS {
            return Err(invalid("frozen binding use exceeds its bound"));
        }
        let mut slots = BTreeSet::new();
        for used in &admission.bindings {
            used.reference.validate().map_err(|_| invalid("invalid binding reference"))?;
            used.target.validate().map_err(|_| invalid("invalid frozen target slot"))?;
            if used.target.tool_name != admission.tool_name
                || !required.contains(&RequiredObjectBinding {
                    key: used.reference.key.clone(), target: used.target.clone(),
                })
                || !slots.insert(used.target.clone())
            {
                return Err(invalid("frozen use does not select a unique required adapter slot"));
            }
        }
        if admission.bindings.is_empty()
            && required.iter().any(|item| item.target.tool_name == admission.tool_name)
        {
            return Err(invalid("dependent tool has no selected binding"));
        }
        let index = parse_complete_history(&self.directory.join(HISTORY_FILE))?;
        let call = index.entries.values().find(|entry| matches!(&entry.payload,
            SessionEntryPayload::ToolCall { call_id, .. } if call_id == &admission.call_id))
            .ok_or_else(|| invalid("admission does not reference a durable tool call"))?;
        let SessionEntryPayload::ToolCall { name, arguments, .. } = &call.payload else { unreachable!() };
        if name != &admission.tool_name || binding_arguments_digest(arguments)? != admission.arguments_digest {
            return Err(invalid("admission arguments differ from the durable tool call"));
        }
        if !index.entries.values().any(|entry| matches!(&entry.payload,
            SessionEntryPayload::TurnObligation { action_id, turn_id, .. }
                if action_id == &admission.scope.action_id && turn_id == &admission.turn_id))
        {
            return Err(invalid("admission does not match the accepted action and turn"));
        }
        let ancestry = index.ancestry(&call.id)?;
        let call_turn = ancestry.iter().rev().find_map(|entry| match &entry.payload {
            SessionEntryPayload::AssistantActivity { turn_id, .. } => Some(turn_id),
            _ => None,
        });
        if call_turn != Some(&admission.turn_id) {
            return Err(invalid("tool intent belongs to a different assistant turn"));
        }
        if let Some(existing) = index.entries.values().find(|entry| matches!(&entry.payload,
            SessionEntryPayload::BindingAdmission { admission: prior }
                if prior.call_id == admission.call_id))
        {
            let SessionEntryPayload::BindingAdmission { admission: prior } = &existing.payload else { unreachable!() };
            let mut retry = admission.clone();
            retry.admitted_at = prior.admitted_at;
            if prior != &retry { return Err(invalid("tool call has a conflicting frozen admission")); }
            return Ok(existing.clone());
        }
        self.append(self.manifest.active_leaf.clone(), admission.admitted_at,
            SessionEntryPayload::BindingAdmission { admission })
    }
}
