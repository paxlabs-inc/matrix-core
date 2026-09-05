use std::collections::BTreeSet;

use keith_agent_types::UtcTimestamp;
use keith_platform_contracts::{AuthorityBoundary, Capability, ExternalAction};
use serde::{Deserialize, Serialize};
use sha2::{Digest, Sha256};

use crate::BridgeError;

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcpPermissionRequest {
    pub tool_call_id: String,
    pub title: String,
    pub action: ExternalAction,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum AcpPermissionOptionKind {
    AllowOnce,
    AllowForSession,
    RejectOnce,
    RejectForSession,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcpPermissionOption {
    pub id: String,
    pub label: String,
    pub kind: AcpPermissionOptionKind,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct AcpPermissionChallenge {
    pub tool_call_id: String,
    pub request_digest: String,
    pub options: Vec<AcpPermissionOption>,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum AcpPermissionDecision {
    AllowOnce,
    AllowForSession,
    RejectOnce,
    RejectForSession,
    Cancelled,
}

pub struct AcpPermissionBridge {
    profile_ceiling: AuthorityBoundary,
    mandatory_denials: BTreeSet<Capability>,
}

impl AcpPermissionBridge {
    #[must_use]
    pub fn new(
        profile_ceiling: AuthorityBoundary,
        mandatory_denials: BTreeSet<Capability>,
    ) -> Self {
        Self {
            profile_ceiling,
            mandatory_denials,
        }
    }

    /// Validates Keith's mandatory/profile policy before constructing a client-visible request.
    ///
    /// # Errors
    ///
    /// Returns an error without producing allow options when a mandatory denial, profile mismatch,
    /// missing grant, expired grant, malformed call identity, or target substitution is detected.
    pub fn begin(
        &self,
        request: &AcpPermissionRequest,
        now: UtcTimestamp,
    ) -> Result<AcpPermissionChallenge, BridgeError> {
        if request.tool_call_id.is_empty()
            || request.tool_call_id.len() > 256
            || request.title.trim().is_empty()
            || request.title.len() > 1_024
            || request.action.profile_id != self.profile_ceiling.profile_id
            || self
                .mandatory_denials
                .contains(&request.action.requested_capability)
            || self
                .profile_ceiling
                .denied
                .contains(&request.action.requested_capability)
        {
            return Err(BridgeError::PermissionDenied(
                "mandatory Keith policy denied the tool action".to_owned(),
            ));
        }
        let granted = self.profile_ceiling.allowed.iter().any(|grant| {
            grant.capability == request.action.requested_capability
                && grant.resource == request.action.target
                && grant.is_active_at(now)
        });
        if !granted {
            return Err(BridgeError::PermissionDenied(
                "the profile has no active exact-target grant".to_owned(),
            ));
        }
        let mut options = vec![
            AcpPermissionOption {
                id: "allow_once".to_owned(),
                label: "Allow once".to_owned(),
                kind: AcpPermissionOptionKind::AllowOnce,
            },
            AcpPermissionOption {
                id: "reject_once".to_owned(),
                label: "Reject".to_owned(),
                kind: AcpPermissionOptionKind::RejectOnce,
            },
            AcpPermissionOption {
                id: "reject_session".to_owned(),
                label: "Reject for this session".to_owned(),
                kind: AcpPermissionOptionKind::RejectForSession,
            },
        ];
        if request.action.risk <= self.profile_ceiling.max_automatic_risk
            && !request.action.risk.is_consequential()
        {
            options.insert(
                1,
                AcpPermissionOption {
                    id: "allow_session".to_owned(),
                    label: "Allow for this session".to_owned(),
                    kind: AcpPermissionOptionKind::AllowForSession,
                },
            );
        }
        Ok(AcpPermissionChallenge {
            tool_call_id: request.tool_call_id.clone(),
            request_digest: request_digest(request)?,
            options,
        })
    }

    /// Reduces one ACP response to an offered decision without treating unknown ids as consent.
    ///
    /// # Errors
    ///
    /// Returns an error if the request changed, the selected id was not offered, or the client
    /// tries to persist approval for a consequential action.
    pub fn complete(
        &self,
        request: &AcpPermissionRequest,
        challenge: &AcpPermissionChallenge,
        selected_option_id: Option<&str>,
    ) -> Result<AcpPermissionDecision, BridgeError> {
        if challenge.tool_call_id != request.tool_call_id
            || challenge.request_digest != request_digest(request)?
        {
            return Err(BridgeError::PermissionDenied(
                "permission response does not match the current tool target".to_owned(),
            ));
        }
        let Some(selected) = selected_option_id else {
            return Ok(AcpPermissionDecision::Cancelled);
        };
        let option = challenge
            .options
            .iter()
            .find(|option| option.id == selected)
            .ok_or_else(|| {
                BridgeError::PermissionDenied(
                    "ACP client selected an option Keith did not offer".to_owned(),
                )
            })?;
        let decision = match option.kind {
            AcpPermissionOptionKind::AllowOnce => AcpPermissionDecision::AllowOnce,
            AcpPermissionOptionKind::AllowForSession
                if request.action.risk <= self.profile_ceiling.max_automatic_risk
                    && !request.action.risk.is_consequential() =>
            {
                AcpPermissionDecision::AllowForSession
            }
            AcpPermissionOptionKind::AllowForSession => {
                return Err(BridgeError::PermissionDenied(
                    "consequential permission cannot become a session-wide approval".to_owned(),
                ));
            }
            AcpPermissionOptionKind::RejectOnce => AcpPermissionDecision::RejectOnce,
            AcpPermissionOptionKind::RejectForSession => AcpPermissionDecision::RejectForSession,
        };
        Ok(decision)
    }
}

fn request_digest(request: &AcpPermissionRequest) -> Result<String, BridgeError> {
    let bytes = serde_json::to_vec(request)?;
    let mut hasher = Sha256::new();
    hasher.update(b"keith-acp-permission-v1\0");
    hasher.update(bytes.len().to_be_bytes());
    hasher.update(bytes);
    Ok(format!("{:x}", hasher.finalize()))
}

#[cfg(test)]
mod tests {
    use keith_agent_types::{ProfileId, SessionId};
    use keith_platform_contracts::{
        ActionRisk, ApprovalEnvelope, ApprovalState, AuditCorrelationId, CancellationId,
        CapabilityGrant, ExternalEffect, ExternalPrincipalId, RedactedText,
    };

    use super::*;

    fn text(value: &str) -> RedactedText {
        RedactedText::parse(value).unwrap()
    }

    fn request(
        profile_id: &ProfileId,
        capability: Capability,
        risk: ActionRisk,
    ) -> AcpPermissionRequest {
        AcpPermissionRequest {
            tool_call_id: "tool-1".to_owned(),
            title: "Apply requested tool action".to_owned(),
            action: ExternalAction {
                profile_id: profile_id.clone(),
                session_id: SessionId::new(),
                acting_principal: ExternalPrincipalId::new(),
                requested_capability: capability,
                risk,
                approval: ApprovalEnvelope {
                    risk,
                    state: ApprovalState::Required,
                },
                target: text("workspace:file"),
                target_digest: text("sha256:target"),
                cancellation_id: CancellationId::new(),
                reply_route: None,
                audit_correlation: AuditCorrelationId::new(),
                external_effect: ExternalEffect::Repeatable,
            },
        }
    }

    fn bridge(profile_id: &ProfileId, mandatory: BTreeSet<Capability>) -> AcpPermissionBridge {
        AcpPermissionBridge::new(
            AuthorityBoundary {
                profile_id: profile_id.clone(),
                allowed: [CapabilityGrant {
                    capability: Capability::LocalWrite,
                    resource: text("workspace:file"),
                    expires_at: None,
                }]
                .into_iter()
                .collect(),
                denied: BTreeSet::new(),
                max_automatic_risk: ActionRisk::ReversibleLocalWrite,
            },
            mandatory,
        )
    }

    #[test]
    fn mandatory_denial_prevents_any_client_allow_option() {
        let profile = ProfileId::new();
        let permission = bridge(&profile, BTreeSet::from([Capability::LocalWrite]));
        assert!(matches!(
            permission.begin(
                &request(
                    &profile,
                    Capability::LocalWrite,
                    ActionRisk::ReversibleLocalWrite
                ),
                UtcTimestamp::from_unix_millis(1),
            ),
            Err(BridgeError::PermissionDenied(_))
        ));
    }

    #[test]
    fn client_response_cannot_substitute_target_or_unoffered_option() {
        let profile = ProfileId::new();
        let permission = bridge(&profile, BTreeSet::new());
        let original = request(
            &profile,
            Capability::LocalWrite,
            ActionRisk::ReversibleLocalWrite,
        );
        let challenge = permission
            .begin(&original, UtcTimestamp::from_unix_millis(1))
            .unwrap();
        assert_eq!(
            permission
                .complete(&original, &challenge, Some("allow_once"))
                .unwrap(),
            AcpPermissionDecision::AllowOnce
        );
        assert!(
            permission
                .complete(&original, &challenge, Some("invented_allow"))
                .is_err()
        );
        let changed = AcpPermissionRequest {
            action: ExternalAction {
                target_digest: text("sha256:changed"),
                ..original.action.clone()
            },
            ..original
        };
        assert!(
            permission
                .complete(&changed, &challenge, Some("allow_once"))
                .is_err()
        );
    }
}
