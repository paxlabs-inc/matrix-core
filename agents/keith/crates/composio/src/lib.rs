#![forbid(unsafe_code)]

use std::collections::{BTreeMap, BTreeSet};
use std::fs::{self, OpenOptions};
use std::io::Write as _;
use std::path::{Path, PathBuf};
use std::sync::Arc;
use std::time::Duration;

use chrono::DateTime;
use keith_agent_types::{ProfileId, SessionId, UtcTimestamp};
use keith_credentials::{
    CredentialError, CredentialOwner, CredentialRef, EncryptedCredentialStore, SecretValue,
};
use keith_mcp::{
    McpAuthentication, McpCredential, McpError, McpManager, McpServerConfig, McpToolResult,
    McpToolSchema, McpTransport,
};
use keith_platform_contracts::{
    ActionRisk, AuditEnvelope, AuditOutcome, AuthorityBoundary, Capability, ConnectedAccountId,
    ContractError, ExternalAction, HealthProjection, LifecycleState, RedactedText,
};
use keith_provider_core::CancellationToken;
use serde::{Deserialize, Serialize};
use serde_json::{Value, json};
use sha2::{Digest, Sha256};
use thiserror::Error;
use url::Url;

const STATE_FILE: &str = "composio-state.json";
const AUDIT_FILE: &str = "composio-audit.jsonl";
const CONTROL_PLANE_OWNER: &str = "composio-control-plane";
const MCP_CREDENTIAL_NAME: &str = "x-api-key";
const MCP_CREDENTIAL_ENV: &str = "KEITH_COMPOSIO_MCP_API_KEY";

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComposioLimits {
    pub timeout_ms: u64,
    pub session_ttl_ms: u64,
    pub max_profiles: usize,
    pub max_accounts_per_profile: usize,
    pub max_toolkits_per_profile: usize,
    pub max_tools_per_profile: usize,
    pub max_schema_bytes: usize,
    pub max_argument_bytes: usize,
    pub max_result_bytes: usize,
}

impl ComposioLimits {
    fn validate(&self) -> Result<(), ComposioError> {
        if self.timeout_ms == 0
            || self.session_ttl_ms == 0
            || self.max_profiles == 0
            || self.max_accounts_per_profile == 0
            || self.max_toolkits_per_profile == 0
            || self.max_tools_per_profile == 0
            || self.max_schema_bytes == 0
            || self.max_argument_bytes == 0
            || self.max_result_bytes == 0
        {
            Err(ComposioError::InvalidConfiguration)
        } else {
            Ok(())
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ComposioConfig {
    pub api_base: String,
    pub api_credential: CredentialRef,
    pub limits: ComposioLimits,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ProfileAppPolicy {
    pub tools: BTreeMap<String, BTreeMap<String, ActionRisk>>,
    pub max_context_schema_bytes: usize,
}

impl ProfileAppPolicy {
    fn validate(&self, limits: &ComposioLimits) -> Result<(), ComposioError> {
        let tool_count = self.tools.values().map(BTreeMap::len).sum::<usize>();
        let mut names = BTreeSet::new();
        if self.tools.is_empty()
            || self.tools.len() > limits.max_toolkits_per_profile
            || tool_count == 0
            || tool_count > limits.max_tools_per_profile
            || self.max_context_schema_bytes == 0
            || self.max_context_schema_bytes > limits.max_schema_bytes
            || self.tools.iter().any(|(toolkit, tools)| {
                !valid_toolkit(toolkit)
                    || tools.is_empty()
                    || tools
                        .keys()
                        .any(|tool| !valid_tool(tool) || !names.insert(tool.clone()))
            })
        {
            return Err(ComposioError::InvalidPolicy);
        }
        Ok(())
    }

    fn risk(&self, toolkit: &str, tool: &str) -> Option<ActionRisk> {
        self.tools
            .get(toolkit)
            .and_then(|tools| tools.get(tool))
            .copied()
    }

    fn contains_toolkit(&self, toolkit: &str) -> bool {
        self.tools.contains_key(toolkit)
    }

    fn tool_names(&self) -> BTreeSet<&str> {
        self.tools
            .values()
            .flat_map(|tools| tools.keys().map(String::as_str))
            .collect()
    }
}

#[derive(Clone, Copy, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(rename_all = "snake_case")]
pub enum ConnectedAccountState {
    Connecting,
    Active,
    Disabled,
    Expired,
    Revoked,
    Failed,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConnectedAppAccount {
    pub id: ConnectedAccountId,
    pub profile_id: ProfileId,
    pub provider_account_id: String,
    pub toolkit: String,
    pub account_identity: RedactedText,
    pub auth_config_id: String,
    pub granted_scopes: BTreeSet<String>,
    pub state: ConnectedAccountState,
    pub selection_precedence: u32,
    pub link_expires_at: Option<UtcTimestamp>,
    pub last_health_at: UtcTimestamp,
    pub safe_error: Option<RedactedText>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ComposioSession {
    pub profile_id: ProfileId,
    pub provider_user_id: String,
    pub provider_session_id: String,
    pub mcp_endpoint: String,
    pub mcp_server_id: String,
    pub policy: ProfileAppPolicy,
    pub state: LifecycleState,
    pub generation: u64,
    pub expires_at: UtcTimestamp,
    pub last_transition_at: UtcTimestamp,
    pub safe_error: Option<RedactedText>,
}

impl ComposioSession {
    #[must_use]
    pub fn health(&self) -> HealthProjection {
        HealthProjection {
            state: self.state,
            last_transition_at: self.last_transition_at,
            restartable: matches!(
                self.state,
                LifecycleState::Active | LifecycleState::Interrupted | LifecycleState::Failed
            ),
            cancellable: self.state == LifecycleState::Active,
            safe_error: self.safe_error.clone(),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct AuthLink {
    pub account_id: ConnectedAccountId,
    pub redirect_url: String,
    pub expires_at: UtcTimestamp,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConnectedAppToolProjection {
    pub toolkit: String,
    pub tool: String,
    pub risk: ActionRisk,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConnectedAppAccountProjection {
    pub id: ConnectedAccountId,
    pub toolkit: String,
    pub account_identity: RedactedText,
    pub auth_config_id: String,
    pub granted_scopes: BTreeSet<String>,
    pub state: ConnectedAccountState,
    pub selection_precedence: u32,
    pub link_expires_at: Option<UtcTimestamp>,
    pub last_health_at: UtcTimestamp,
    pub safe_error: Option<RedactedText>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConnectedAppSessionProjection {
    pub state: LifecycleState,
    pub generation: u64,
    pub expires_at: UtcTimestamp,
    pub last_transition_at: UtcTimestamp,
    pub safe_error: Option<RedactedText>,
}

#[derive(Clone, Debug, Eq, PartialEq, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
pub struct ConnectedAppsProjection {
    pub profile_id: ProfileId,
    pub session: Option<ConnectedAppSessionProjection>,
    pub accounts: Vec<ConnectedAppAccountProjection>,
    pub allowed_tools: Vec<ConnectedAppToolProjection>,
}

#[derive(Clone, Debug, PartialEq)]
pub struct ComposioToolCall {
    pub profile_id: ProfileId,
    pub account_id: ConnectedAccountId,
    pub toolkit: String,
    pub tool: String,
    pub arguments: Value,
    pub action: ExternalAction,
}

#[derive(Clone, Debug, Default, Serialize, Deserialize)]
#[serde(deny_unknown_fields)]
struct DurableState {
    policies: BTreeMap<ProfileId, ProfileAppPolicy>,
    sessions: BTreeMap<ProfileId, ComposioSession>,
    accounts: BTreeMap<ConnectedAccountId, ConnectedAppAccount>,
}

#[derive(Debug, Error)]
pub enum ComposioError {
    #[error("Composio connector configuration is invalid")]
    InvalidConfiguration,
    #[error("Composio profile policy is invalid or exceeds its bounds")]
    InvalidPolicy,
    #[error("Composio profile, session, or account was not found")]
    NotFound,
    #[error("Composio object belongs to another profile")]
    ProfileDenied,
    #[error("Composio profile or account capacity was reached")]
    Capacity,
    #[error("Composio lifecycle transition is invalid")]
    InvalidState,
    #[error("Composio session or authentication link expired")]
    Expired,
    #[error("an exact connected account must be selected")]
    AccountSelectionRequired,
    #[error("Composio returned a different profile, session, account, or toolkit")]
    ProviderSubstitution,
    #[error("Composio request or result exceeded a configured bound")]
    SizeLimit,
    #[error("Composio action was cancelled")]
    Cancelled,
    #[error("Composio authority or exact approval was denied: {0}")]
    Authority(#[from] ContractError),
    #[error("Composio credential operation failed")]
    Credential(#[from] CredentialError),
    #[error("Composio MCP operation failed: {0}")]
    Mcp(#[from] McpError),
    #[error("Composio control-plane authentication failed")]
    Authentication,
    #[error("Composio control plane rejected the request")]
    ProviderRejected,
    #[error("Composio control-plane request timed out")]
    Timeout,
    #[error("Composio control-plane transport failed")]
    Transport,
    #[error("Composio response was malformed")]
    Protocol,
    #[error("the HTTPS MCP bridge executable is required")]
    BridgeRequired,
    #[error("Composio persistence failed: {0}")]
    Io(#[from] std::io::Error),
    #[error("Composio state JSON failed: {0}")]
    Json(#[from] serde_json::Error),
}

pub struct ComposioConnector {
    root: PathBuf,
    config: ComposioConfig,
    credentials: Arc<EncryptedCredentialStore>,
    http: ureq::Agent,
    state: DurableState,
}

impl ComposioConnector {
    /// Opens durable Composio state without resolving or exposing its named API credential.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid bounds, endpoints, credential scope, or corrupt state.
    pub fn open(
        root: impl AsRef<Path>,
        config: ComposioConfig,
        credentials: Arc<EncryptedCredentialStore>,
    ) -> Result<Self, ComposioError> {
        config.limits.validate()?;
        validate_api_base(&config.api_base)?;
        if config.api_credential.owner != CredentialOwner::Tool(CONTROL_PLANE_OWNER.to_owned()) {
            return Err(ComposioError::InvalidConfiguration);
        }
        fs::create_dir_all(root.as_ref())?;
        let root_metadata = fs::symlink_metadata(root.as_ref())?;
        if root_metadata.file_type().is_symlink() || !root_metadata.is_dir() {
            return Err(ComposioError::InvalidConfiguration);
        }
        let root = fs::canonicalize(root.as_ref())?;
        let state_path = root.join(STATE_FILE);
        let state = if state_path.exists() {
            ensure_regular_file(&state_path)?;
            serde_json::from_slice(&fs::read(state_path)?)?
        } else {
            DurableState::default()
        };
        validate_durable_state(&state, &config.limits)?;
        let http: ureq::Agent = ureq::Agent::config_builder()
            .timeout_global(Some(Duration::from_millis(config.limits.timeout_ms)))
            .http_status_as_error(false)
            .max_redirects(0)
            .build()
            .into();
        Ok(Self {
            root,
            config,
            credentials,
            http,
            state,
        })
    }

    /// Adds or replaces one profile's exact toolkit/tool/risk ceiling.
    ///
    /// # Errors
    ///
    /// Returns an error when the policy is invalid, capacity is exhausted, or state cannot persist.
    pub fn set_profile_policy(
        &mut self,
        profile_id: ProfileId,
        policy: ProfileAppPolicy,
    ) -> Result<(), ComposioError> {
        policy.validate(&self.config.limits)?;
        if !self.state.policies.contains_key(&profile_id)
            && self.state.policies.len() >= self.config.limits.max_profiles
        {
            return Err(ComposioError::Capacity);
        }
        if self
            .state
            .sessions
            .get(&profile_id)
            .is_some_and(|session| session.policy != policy)
        {
            return Err(ComposioError::InvalidState);
        }
        self.state.policies.insert(profile_id, policy);
        self.persist()
    }

    pub fn policy(&self, profile_id: &ProfileId) -> Option<&ProfileAppPolicy> {
        self.state.policies.get(profile_id)
    }

    pub fn session(&self, profile_id: &ProfileId) -> Option<&ComposioSession> {
        self.state.sessions.get(profile_id)
    }

    /// Resolves one account only when it belongs to the requested profile.
    ///
    /// # Errors
    ///
    /// Returns an error when the account is missing or owned by another profile.
    pub fn account(
        &self,
        profile_id: &ProfileId,
        account_id: &ConnectedAccountId,
    ) -> Result<&ConnectedAppAccount, ComposioError> {
        let account = self
            .state
            .accounts
            .get(account_id)
            .ok_or(ComposioError::NotFound)?;
        if &account.profile_id != profile_id {
            return Err(ComposioError::ProfileDenied);
        }
        Ok(account)
    }

    pub fn accounts(&self, profile_id: &ProfileId) -> Vec<&ConnectedAppAccount> {
        let mut accounts = self
            .state
            .accounts
            .values()
            .filter(|account| &account.profile_id == profile_id)
            .collect::<Vec<_>>();
        accounts.sort_by(|left, right| {
            left.toolkit
                .cmp(&right.toolkit)
                .then(left.selection_precedence.cmp(&right.selection_precedence))
                .then(left.id.cmp(&right.id))
        });
        accounts
    }

    /// Returns the complete browser-safe connected-app view without provider user identities,
    /// hosted MCP endpoints, server identifiers, bearer credentials, or provider account keys.
    #[must_use]
    pub fn connected_apps_projection(&self, profile_id: &ProfileId) -> ConnectedAppsProjection {
        let session = self.state.sessions.get(profile_id);
        let mut allowed_tools = self
            .state
            .policies
            .get(profile_id)
            .into_iter()
            .flat_map(|policy| &policy.tools)
            .flat_map(|(toolkit, tools)| {
                tools.iter().map(|(tool, risk)| ConnectedAppToolProjection {
                    toolkit: toolkit.clone(),
                    tool: tool.clone(),
                    risk: *risk,
                })
            })
            .collect::<Vec<_>>();
        allowed_tools.sort_by(|left, right| {
            left.toolkit
                .cmp(&right.toolkit)
                .then(left.tool.cmp(&right.tool))
        });
        ConnectedAppsProjection {
            profile_id: profile_id.clone(),
            session: session.map(|session| ConnectedAppSessionProjection {
                state: session.state,
                generation: session.generation,
                expires_at: session.expires_at,
                last_transition_at: session.last_transition_at,
                safe_error: session.safe_error.clone(),
            }),
            accounts: self
                .accounts(profile_id)
                .into_iter()
                .map(|account| ConnectedAppAccountProjection {
                    id: account.id.clone(),
                    toolkit: account.toolkit.clone(),
                    account_identity: account.account_identity.clone(),
                    auth_config_id: account.auth_config_id.clone(),
                    granted_scopes: account.granted_scopes.clone(),
                    state: account.state,
                    selection_precedence: account.selection_precedence,
                    link_expires_at: account.link_expires_at,
                    last_health_at: account.last_health_at,
                    safe_error: account.safe_error.clone(),
                })
                .collect(),
            allowed_tools,
        }
    }

    /// Starts a hosted authentication link after exact account-change authorization.
    ///
    /// # Errors
    ///
    /// Returns an error for profile isolation, authority, provider, capacity, or persistence failure.
    #[allow(clippy::too_many_arguments)]
    pub fn begin_connect(
        &mut self,
        profile_id: &ProfileId,
        toolkit: &str,
        auth_config_id: &str,
        account_label: &str,
        selection_precedence: u32,
        action: &ExternalAction,
        authority: &AuthorityBoundary,
        now: UtcTimestamp,
    ) -> Result<AuthLink, ComposioError> {
        let policy = self
            .state
            .policies
            .get(profile_id)
            .ok_or(ComposioError::NotFound)?;
        if !policy.contains_toolkit(toolkit)
            || !valid_identifier(auth_config_id)
            || !valid_identifier(account_label)
        {
            return Err(ComposioError::InvalidPolicy);
        }
        if self.accounts(profile_id).len() >= self.config.limits.max_accounts_per_profile {
            return Err(ComposioError::Capacity);
        }
        let target = connect_target(profile_id, toolkit)?;
        self.admit_action(
            action,
            authority,
            profile_id,
            Capability::AccountChange,
            ActionRisk::AccountChange,
            &target,
            &Value::Null,
            now,
        )?;
        let body = json!({
            "auth_config_id": auth_config_id,
            "user_id": provider_user_id(profile_id),
            "alias": account_label,
        });
        let response = match self.post_json("api/v3.1/connected_accounts/link", &body) {
            Ok(response) => response,
            Err(error) => {
                self.audit(action, AuditOutcome::Failed, now)?;
                return Err(error);
            }
        };
        let parsed = (|| {
            Ok::<_, ComposioError>((
                required_identifier(&response, "connected_account_id")?,
                required_url(&response, "redirect_url")?,
                required_timestamp(&response, "expires_at")?,
            ))
        })();
        let (provider_account_id, redirect_url, expires_at) = match parsed {
            Ok(parsed) => parsed,
            Err(error) => {
                self.audit(action, AuditOutcome::Failed, now)?;
                return Err(error);
            }
        };
        if expires_at <= now {
            self.audit(action, AuditOutcome::Failed, now)?;
            return Err(ComposioError::Expired);
        }
        let account = ConnectedAppAccount {
            id: ConnectedAccountId::new(),
            profile_id: profile_id.clone(),
            provider_account_id,
            toolkit: toolkit.to_owned(),
            account_identity: RedactedText::parse(account_label)?,
            auth_config_id: auth_config_id.to_owned(),
            granted_scopes: BTreeSet::new(),
            state: ConnectedAccountState::Connecting,
            selection_precedence,
            link_expires_at: Some(expires_at),
            last_health_at: now,
            safe_error: None,
        };
        let link = AuthLink {
            account_id: account.id.clone(),
            redirect_url,
            expires_at,
        };
        self.state.accounts.insert(account.id.clone(), account);
        self.persist()?;
        self.audit(action, AuditOutcome::Completed, now)?;
        Ok(link)
    }

    /// Completes a deferred OAuth callback only after Composio verifies the stable Keith profile
    /// identity that started the connection.
    ///
    /// # Errors
    ///
    /// Returns an error for an invalid or replayed callback URI, profile or account substitution,
    /// authority denial, provider rejection, or a non-active refreshed account.
    #[allow(clippy::too_many_arguments)]
    pub fn complete_connect_callback(
        &mut self,
        profile_id: &ProfileId,
        account_id: &ConnectedAccountId,
        session_uri: &str,
        action: &ExternalAction,
        authority: &AuthorityBoundary,
        now: UtcTimestamp,
    ) -> Result<&ConnectedAppAccount, ComposioError> {
        if session_uri.is_empty()
            || session_uri.len() > 2_048
            || session_uri.chars().any(char::is_control)
        {
            return Err(ComposioError::InvalidPolicy);
        }
        let account = self.account(profile_id, account_id)?.clone();
        if account.state != ConnectedAccountState::Connecting {
            return Err(ComposioError::InvalidState);
        }
        if account
            .link_expires_at
            .is_some_and(|expires_at| expires_at <= now)
        {
            return Err(ComposioError::Expired);
        }
        let target = account_target(&account)?;
        let payload = json!({"session_uri": session_uri});
        self.admit_action(
            action,
            authority,
            profile_id,
            Capability::AccountChange,
            ActionRisk::AccountChange,
            &target,
            &payload,
            now,
        )?;
        let response = match self.post_json(
            "api/v3.1/connected_accounts/complete_auth",
            &json!({
                "session_uri": session_uri,
                "user_id": provider_user_id(profile_id),
            }),
        ) {
            Ok(response) => response,
            Err(error) => {
                self.audit(action, AuditOutcome::Failed, now)?;
                return Err(error);
            }
        };
        let returned_account = required_identifier(&response, "connected_account_id")?;
        let returned_toolkit = required_string(&response, "toolkit_slug")?;
        if returned_account != account.provider_account_id || returned_toolkit != account.toolkit {
            self.audit(action, AuditOutcome::Denied, now)?;
            return Err(ComposioError::ProviderSubstitution);
        }
        match self.refresh_account(profile_id, account_id, now) {
            Ok(refreshed) if refreshed.state == ConnectedAccountState::Active => {
                self.audit(action, AuditOutcome::Completed, now)?;
                self.account(profile_id, account_id)
            }
            Ok(_) => {
                self.audit(action, AuditOutcome::Failed, now)?;
                Err(ComposioError::InvalidState)
            }
            Err(error) => {
                self.audit(action, AuditOutcome::Failed, now)?;
                Err(error)
            }
        }
    }

    /// Refreshes safe connected-account identity, scopes, status, and health from Composio.
    ///
    /// # Errors
    ///
    /// Returns an error for profile substitution, malformed provider state, or persistence failure.
    pub fn refresh_account(
        &mut self,
        profile_id: &ProfileId,
        account_id: &ConnectedAccountId,
        now: UtcTimestamp,
    ) -> Result<&ConnectedAppAccount, ComposioError> {
        let current = self.account(profile_id, account_id)?.clone();
        let path = format!(
            "api/v3.1/connected_accounts/{}",
            current.provider_account_id
        );
        let response = self.get_json(&path)?;
        let provider_id = required_identifier(&response, "id")?;
        if provider_id != current.provider_account_id {
            return Err(ComposioError::ProviderSubstitution);
        }
        if let Some(toolkit) = response_toolkit(&response)
            && toolkit != current.toolkit
        {
            return Err(ComposioError::ProviderSubstitution);
        }
        let status = required_string(&response, "status")?;
        let state = match status.as_str() {
            "ACTIVE" => ConnectedAccountState::Active,
            "INITIALIZING" | "INITIATED" => {
                if current.link_expires_at.is_some_and(|expiry| expiry <= now) {
                    ConnectedAccountState::Expired
                } else {
                    ConnectedAccountState::Connecting
                }
            }
            "INACTIVE" | "DISABLED" => ConnectedAccountState::Disabled,
            "EXPIRED" => ConnectedAccountState::Expired,
            "REVOKED" => ConnectedAccountState::Revoked,
            "FAILED" => ConnectedAccountState::Failed,
            _ => return Err(ComposioError::Protocol),
        };
        let scopes = response
            .get("scopes")
            .and_then(Value::as_array)
            .map(|scopes| {
                scopes
                    .iter()
                    .map(|scope| {
                        scope
                            .as_str()
                            .filter(|scope| valid_scope(scope))
                            .map(str::to_owned)
                            .ok_or(ComposioError::Protocol)
                    })
                    .collect::<Result<BTreeSet<_>, _>>()
            })
            .transpose()?
            .unwrap_or_default();
        let account = self
            .state
            .accounts
            .get_mut(account_id)
            .ok_or(ComposioError::NotFound)?;
        account.state = state;
        account.granted_scopes = scopes;
        account.link_expires_at = (state == ConnectedAccountState::Connecting)
            .then_some(account.link_expires_at)
            .flatten();
        account.last_health_at = now;
        account.safe_error = match state {
            ConnectedAccountState::Expired => Some(RedactedText::parse("authentication expired")?),
            ConnectedAccountState::Failed => Some(RedactedText::parse("connection failed")?),
            _ => None,
        };
        self.persist()?;
        self.state
            .accounts
            .get(account_id)
            .ok_or(ComposioError::NotFound)
    }

    /// Creates a bounded direct-tools MCP session for one stable profile identity.
    ///
    /// # Errors
    ///
    /// Returns an error for provider substitution, invalid policy, expiry, or persistence failure.
    pub fn create_session(
        &mut self,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<&ComposioSession, ComposioError> {
        let policy = self
            .state
            .policies
            .get(profile_id)
            .cloned()
            .ok_or(ComposioError::NotFound)?;
        let previous_generation = self
            .state
            .sessions
            .get(profile_id)
            .map_or(0, |session| session.generation);
        if let Some(current) = self.state.sessions.get(profile_id).cloned() {
            if current.state == LifecycleState::Active && current.expires_at > now {
                return Err(ComposioError::InvalidState);
            }
            self.delete_json(&format!(
                "api/v3.1/tool_router/session/{}",
                current.provider_session_id
            ))?;
            self.state.sessions.remove(profile_id);
            self.persist()?;
        }
        let provider_user = provider_user_id(profile_id);
        let body = self.session_body(profile_id, &provider_user, &policy)?;
        let response = self.post_json("api/v3.1/tool_router/session", &body)?;
        let session = parse_session_response(
            profile_id,
            &provider_user,
            &policy,
            &response,
            previous_generation.saturating_add(1),
            expiration(now, self.config.limits.session_ttl_ms)?,
            now,
        )?;
        self.state.sessions.insert(profile_id.clone(), session);
        self.persist()?;
        self.state
            .sessions
            .get(profile_id)
            .ok_or(ComposioError::NotFound)
    }

    /// Reattaches a durable, unexpired session and refuses provider user/session substitution.
    ///
    /// # Errors
    ///
    /// Returns an error when the session expired, changed owner, disappeared, or cannot persist.
    pub fn resume_session(
        &mut self,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<&ComposioSession, ComposioError> {
        let current = self
            .state
            .sessions
            .get(profile_id)
            .cloned()
            .ok_or(ComposioError::NotFound)?;
        if current.expires_at <= now {
            let session = self
                .state
                .sessions
                .get_mut(profile_id)
                .ok_or(ComposioError::NotFound)?;
            session.state = LifecycleState::Interrupted;
            session.last_transition_at = now;
            session.safe_error = Some(RedactedText::parse("provider session expired")?);
            self.persist()?;
            return Err(ComposioError::Expired);
        }
        let response = self.get_json(&format!(
            "api/v3.1/tool_router/session/{}",
            current.provider_session_id
        ))?;
        let resumed = parse_session_response(
            profile_id,
            &current.provider_user_id,
            &current.policy,
            &response,
            current.generation.saturating_add(1),
            current.expires_at,
            now,
        )?;
        if resumed.provider_session_id != current.provider_session_id {
            return Err(ComposioError::ProviderSubstitution);
        }
        self.state.sessions.insert(profile_id.clone(), resumed);
        self.persist()?;
        self.state
            .sessions
            .get(profile_id)
            .ok_or(ComposioError::NotFound)
    }

    /// Configures the existing MCP manager with a profile-only credential and bounded schema cache.
    ///
    /// HTTPS endpoints use the isolated stdio bridge; loopback HTTP is accepted for local testing.
    ///
    /// # Errors
    ///
    /// Returns an error for missing bridge, credential, MCP, schema, or policy bounds.
    pub fn bind_mcp(
        &self,
        profile_id: &ProfileId,
        manager: &mut McpManager,
        bridge_executable: Option<&Path>,
        now: UtcTimestamp,
    ) -> Result<(), ComposioError> {
        let session = self
            .state
            .sessions
            .get(profile_id)
            .ok_or(ComposioError::NotFound)?;
        if session.state != LifecycleState::Active || session.expires_at <= now {
            return Err(ComposioError::Expired);
        }
        self.install_mcp_credential(session, now)?;
        let config = self.mcp_config(session, bridge_executable)?;
        manager.configure(config)?;
        let cache = manager.refresh_schema(&session.mcp_server_id, now)?;
        let encoded = serde_json::to_vec(cache)?;
        if encoded.len() > self.config.limits.max_schema_bytes {
            return Err(ComposioError::SizeLimit);
        }
        let allowed = session.policy.tool_names();
        if cache
            .tools
            .iter()
            .any(|tool| !allowed.contains(tool.name.as_str()))
        {
            return Err(ComposioError::ProviderSubstitution);
        }
        Ok(())
    }

    /// Returns only policy-allowed relevant schemas under both caller and profile context budgets.
    ///
    /// # Errors
    ///
    /// Returns an error when the profile has no active Composio session.
    pub fn discover_tools(
        &self,
        manager: &McpManager,
        profile_id: &ProfileId,
        query: &str,
        query_embedding: &[i32],
        max_bytes: usize,
    ) -> Result<Vec<McpToolSchema>, ComposioError> {
        let session = self
            .state
            .sessions
            .get(profile_id)
            .ok_or(ComposioError::NotFound)?;
        let allowed = session.policy.tool_names();
        Ok(manager
            .relevant_tools(
                profile_id,
                query,
                query_embedding,
                max_bytes.min(session.policy.max_context_schema_bytes),
            )
            .into_iter()
            .filter(|tool| allowed.contains(tool.name.as_str()))
            .collect())
    }

    /// Opens the normalized MCP session only for the owning profile.
    ///
    /// # Errors
    ///
    /// Returns an error for missing, expired, isolated, or saturated sessions.
    pub fn open_mcp_session(
        &self,
        manager: &mut McpManager,
        profile_id: &ProfileId,
        session_id: &SessionId,
        now: UtcTimestamp,
    ) -> Result<(), ComposioError> {
        let session = self
            .state
            .sessions
            .get(profile_id)
            .ok_or(ComposioError::NotFound)?;
        if session.state != LifecycleState::Active || session.expires_at <= now {
            return Err(ComposioError::Expired);
        }
        manager.open_session(session_id, profile_id.clone(), &session.mcp_server_id)?;
        Ok(())
    }

    /// Executes one exact-account MCP call through Keith authority, audit, cancellation, bounds,
    /// and recursive result redaction.
    ///
    /// # Errors
    ///
    /// Returns an error before the call for any profile, account, tool, risk, approval, or size
    /// mismatch, and records a truthful terminal audit outcome after admission.
    #[allow(clippy::too_many_arguments)]
    pub fn call_tool(
        &self,
        manager: &McpManager,
        session_id: &SessionId,
        call: &ComposioToolCall,
        authority: &AuthorityBoundary,
        cancellation: &CancellationToken,
        now: UtcTimestamp,
    ) -> Result<McpToolResult, ComposioError> {
        let session = self
            .state
            .sessions
            .get(&call.profile_id)
            .ok_or(ComposioError::NotFound)?;
        if session.state != LifecycleState::Active || session.expires_at <= now {
            return Err(ComposioError::Expired);
        }
        if &call.action.session_id != session_id {
            return Err(ComposioError::ProfileDenied);
        }
        let account = self.account(&call.profile_id, &call.account_id)?;
        if account.state != ConnectedAccountState::Active
            || account.toolkit != call.toolkit
            || !valid_tool(&call.tool)
        {
            return Err(ComposioError::AccountSelectionRequired);
        }
        let expected_risk = session
            .policy
            .risk(&call.toolkit, &call.tool)
            .ok_or(ComposioError::InvalidPolicy)?;
        let target = tool_target(account, &call.tool)?;
        let argument_bytes = serde_json::to_vec(&call.arguments)?;
        if argument_bytes.len() > self.config.limits.max_argument_bytes {
            return Err(ComposioError::SizeLimit);
        }
        let arguments = with_exact_account(&call.arguments, &account.provider_account_id)?;
        self.admit_action(
            &call.action,
            authority,
            &call.profile_id,
            Capability::ConnectedAppInvoke,
            expected_risk,
            &target,
            &call.arguments,
            now,
        )?;
        if cancellation.is_cancelled() {
            self.audit(&call.action, AuditOutcome::Cancelled, now)?;
            return Err(ComposioError::Cancelled);
        }
        let result =
            match manager.call_tool(session_id, &session.mcp_server_id, &call.tool, &arguments) {
                Ok(result) => result,
                Err(error) => {
                    let outcome = if cancellation.is_cancelled() {
                        AuditOutcome::Cancelled
                    } else {
                        AuditOutcome::Failed
                    };
                    self.audit(&call.action, outcome, now)?;
                    return if cancellation.is_cancelled() {
                        Err(ComposioError::Cancelled)
                    } else {
                        Err(error.into())
                    };
                }
            };
        if cancellation.is_cancelled() {
            self.audit(&call.action, AuditOutcome::Cancelled, now)?;
            return Err(ComposioError::Cancelled);
        }
        let redacted = redact_tool_result(result);
        if serde_json::to_vec(&redacted)?.len() > self.config.limits.max_result_bytes {
            self.audit(&call.action, AuditOutcome::Failed, now)?;
            return Err(ComposioError::SizeLimit);
        }
        self.audit(
            &call.action,
            if redacted.is_error {
                AuditOutcome::Failed
            } else {
                AuditOutcome::Completed
            },
            now,
        )?;
        Ok(redacted)
    }

    /// Revokes upstream credentials before local deletion is permitted.
    ///
    /// # Errors
    ///
    /// Returns an error for profile isolation, authority, provider failure, or persistence failure.
    pub fn revoke_account(
        &mut self,
        profile_id: &ProfileId,
        account_id: &ConnectedAccountId,
        action: &ExternalAction,
        authority: &AuthorityBoundary,
        now: UtcTimestamp,
    ) -> Result<(), ComposioError> {
        let account = self.account(profile_id, account_id)?.clone();
        if account.state != ConnectedAccountState::Active {
            return Err(ComposioError::InvalidState);
        }
        let target = account_target(&account)?;
        self.admit_action(
            action,
            authority,
            profile_id,
            Capability::AccountChange,
            ActionRisk::CredentialChange,
            &target,
            &Value::Null,
            now,
        )?;
        let path = format!(
            "api/v3.1/connected_accounts/{}/revoke",
            account.provider_account_id
        );
        if let Err(error) = self.post_json(&path, &json!({})) {
            self.audit(action, AuditOutcome::Failed, now)?;
            return Err(error);
        }
        let account = self
            .state
            .accounts
            .get_mut(account_id)
            .ok_or(ComposioError::NotFound)?;
        account.state = ConnectedAccountState::Revoked;
        account.last_health_at = now;
        account.safe_error = None;
        self.persist()?;
        self.audit(action, AuditOutcome::Completed, now)
    }

    /// Deletes an already-revoked remote account and removes its local identity and scopes.
    ///
    /// # Errors
    ///
    /// Returns an error for profile isolation, missing revocation, authority, provider, or storage.
    pub fn delete_account(
        &mut self,
        profile_id: &ProfileId,
        account_id: &ConnectedAccountId,
        action: &ExternalAction,
        authority: &AuthorityBoundary,
        now: UtcTimestamp,
    ) -> Result<(), ComposioError> {
        let account = self.account(profile_id, account_id)?.clone();
        if account.state != ConnectedAccountState::Revoked {
            return Err(ComposioError::InvalidState);
        }
        let target = account_target(&account)?;
        self.admit_action(
            action,
            authority,
            profile_id,
            Capability::AccountChange,
            ActionRisk::Delete,
            &target,
            &Value::Null,
            now,
        )?;
        let path = format!(
            "api/v3.1/connected_accounts/{}?revoke_on_delete=true",
            account.provider_account_id
        );
        if let Err(error) = self.delete_json(&path) {
            self.audit(action, AuditOutcome::Failed, now)?;
            return Err(error);
        }
        self.state.accounts.remove(account_id);
        self.persist()?;
        self.audit(action, AuditOutcome::Completed, now)
    }

    /// Deletes the provider session, disables its copied MCP credential, and drops durable state.
    ///
    /// # Errors
    ///
    /// Returns an error for isolation, provider rejection, credential, or persistence failure.
    pub fn delete_session(
        &mut self,
        profile_id: &ProfileId,
        now: UtcTimestamp,
    ) -> Result<(), ComposioError> {
        let session = self
            .state
            .sessions
            .get(profile_id)
            .cloned()
            .ok_or(ComposioError::NotFound)?;
        self.delete_json(&format!(
            "api/v3.1/tool_router/session/{}",
            session.provider_session_id
        ))?;
        let reference = mcp_credential_ref(&session.mcp_server_id)?;
        self.credentials
            .put(reference, SecretValue::new(b"revoked".to_vec())?, now)?;
        self.state.sessions.remove(profile_id);
        self.persist()
    }

    /// Reads the connector's append-only, content-free action audit.
    ///
    /// # Errors
    ///
    /// Returns an error when the audit file cannot be read or decoded.
    pub fn audit_records(&self) -> Result<Vec<AuditEnvelope>, ComposioError> {
        let path = self.root.join(AUDIT_FILE);
        if !path.exists() {
            return Ok(Vec::new());
        }
        fs::read_to_string(path)?
            .lines()
            .map(|line| serde_json::from_str(line).map_err(ComposioError::from))
            .collect()
    }

    fn session_body(
        &self,
        profile_id: &ProfileId,
        provider_user: &str,
        policy: &ProfileAppPolicy,
    ) -> Result<Value, ComposioError> {
        let connected_accounts = self
            .accounts(profile_id)
            .into_iter()
            .filter(|account| account.state == ConnectedAccountState::Active)
            .fold(
                BTreeMap::<String, Vec<String>>::new(),
                |mut grouped, account| {
                    grouped
                        .entry(account.toolkit.clone())
                        .or_default()
                        .push(account.provider_account_id.clone());
                    grouped
                },
            );
        let max_accounts = u64::try_from(self.config.limits.max_accounts_per_profile)
            .map_err(|_| ComposioError::InvalidConfiguration)?;
        Ok(json!({
            "user_id": provider_user,
            "toolkits": {"enabled": policy.tools.keys().collect::<Vec<_>>()},
            "tools": policy.tools.iter().map(|(toolkit, tools)| {
                (toolkit, json!({"enabled": tools.keys().collect::<Vec<_>>()}))
            }).collect::<BTreeMap<_, _>>(),
            "connected_accounts": connected_accounts,
            "multi_account": {
                "enable": true,
                "max_accounts_per_toolkit": max_accounts,
                "require_explicit_selection": true,
            },
            "manage_connections": {"enabled": false},
            "execute": {"enable_multi_execute": false},
            "search": {"enable": false},
            "sandbox": {"enable": false},
            "session_preset": "direct_tools",
            "mcp": true,
        }))
    }

    fn install_mcp_credential(
        &self,
        session: &ComposioSession,
        now: UtcTimestamp,
    ) -> Result<(), ComposioError> {
        let source = self.credentials.resolve(
            &self.config.api_credential,
            &CredentialOwner::Tool(CONTROL_PLANE_OWNER.to_owned()),
        )?;
        let copied = source.with_bytes(|bytes| SecretValue::new(bytes.to_vec()))?;
        self.credentials
            .put(mcp_credential_ref(&session.mcp_server_id)?, copied, now)?;
        Ok(())
    }

    fn mcp_config(
        &self,
        session: &ComposioSession,
        bridge_executable: Option<&Path>,
    ) -> Result<McpServerConfig, ComposioError> {
        let endpoint =
            Url::parse(&session.mcp_endpoint).map_err(|_| ComposioError::InvalidConfiguration)?;
        let host = endpoint
            .host_str()
            .ok_or(ComposioError::InvalidConfiguration)?;
        let (transport, placement, allowed_network_hosts) =
            if endpoint.scheme() == "http" && is_loopback_host(host) {
                (
                    McpTransport::Http {
                        endpoint: session.mcp_endpoint.clone(),
                        headers: BTreeMap::new(),
                    },
                    McpAuthentication::Header("x-api-key".to_owned()),
                    BTreeSet::from([host.to_owned()]),
                )
            } else if endpoint.scheme() == "https" {
                let executable = bridge_executable.ok_or(ComposioError::BridgeRequired)?;
                (
                    McpTransport::Stdio {
                        executable: executable.to_path_buf(),
                        args: vec![
                            session.mcp_endpoint.clone(),
                            self.config.limits.timeout_ms.to_string(),
                            self.config
                                .limits
                                .max_result_bytes
                                .max(self.config.limits.max_schema_bytes)
                                .to_string(),
                        ],
                        working_directory: None,
                        environment: BTreeMap::new(),
                    },
                    McpAuthentication::Environment(MCP_CREDENTIAL_ENV.to_owned()),
                    BTreeSet::new(),
                )
            } else {
                return Err(ComposioError::InvalidConfiguration);
            };
        Ok(McpServerConfig {
            id: session.mcp_server_id.clone(),
            transport,
            enabled_profiles: BTreeSet::from([session.profile_id.clone()]),
            credential: Some(McpCredential {
                reference: mcp_credential_ref(&session.mcp_server_id)?,
                placement,
            }),
            allowed_filesystem_roots: Vec::new(),
            allowed_network_hosts,
            timeout_ms: self.config.limits.timeout_ms,
            max_request_bytes: self.config.limits.max_argument_bytes,
            max_response_bytes: self
                .config
                .limits
                .max_result_bytes
                .max(self.config.limits.max_schema_bytes),
            max_tools: self.config.limits.max_tools_per_profile,
        })
    }

    fn get_json(&self, path: &str) -> Result<Value, ComposioError> {
        let url = self.control_url(path)?;
        let mut response =
            self.with_api_key(|key| self.http.get(url.as_str()).header("x-api-key", key).call())?;
        self.decode_response(&mut response)
    }

    fn post_json(&self, path: &str, body: &Value) -> Result<Value, ComposioError> {
        let url = self.control_url(path)?;
        let encoded = serde_json::to_vec(body)?;
        if encoded.len() > self.config.limits.max_argument_bytes {
            return Err(ComposioError::SizeLimit);
        }
        let mut response = self.with_api_key(|key| {
            self.http
                .post(url.as_str())
                .header("x-api-key", key)
                .header("Content-Type", "application/json")
                .send(&encoded)
        })?;
        self.decode_response(&mut response)
    }

    fn delete_json(&self, path: &str) -> Result<Value, ComposioError> {
        let url = self.control_url(path)?;
        let mut response = self.with_api_key(|key| {
            self.http
                .delete(url.as_str())
                .header("x-api-key", key)
                .call()
        })?;
        self.decode_response(&mut response)
    }

    fn with_api_key<T>(
        &self,
        use_key: impl FnOnce(&str) -> Result<T, ureq::Error>,
    ) -> Result<T, ComposioError> {
        let secret = self.credentials.resolve(
            &self.config.api_credential,
            &CredentialOwner::Tool(CONTROL_PLANE_OWNER.to_owned()),
        )?;
        secret.with_bytes(|bytes| {
            let key = std::str::from_utf8(bytes).map_err(|_| ComposioError::Authentication)?;
            use_key(key).map_err(|error| map_http_error(&error))
        })
    }

    fn decode_response(
        &self,
        response: &mut ureq::http::Response<ureq::Body>,
    ) -> Result<Value, ComposioError> {
        match response.status().as_u16() {
            200..=299 => {}
            401 | 403 => return Err(ComposioError::Authentication),
            408 | 504 => return Err(ComposioError::Timeout),
            _ => return Err(ComposioError::ProviderRejected),
        }
        let limit = u64::try_from(self.config.limits.max_result_bytes)
            .unwrap_or(u64::MAX)
            .saturating_add(1);
        let bytes = response
            .body_mut()
            .with_config()
            .limit(limit)
            .read_to_vec()
            .map_err(|error| map_http_error(&error))?;
        if bytes.len() > self.config.limits.max_result_bytes {
            return Err(ComposioError::SizeLimit);
        }
        if bytes.is_empty() {
            Ok(Value::Null)
        } else {
            serde_json::from_slice(&bytes).map_err(|_| ComposioError::Protocol)
        }
    }

    fn control_url(&self, path: &str) -> Result<Url, ComposioError> {
        let base =
            Url::parse(&self.config.api_base).map_err(|_| ComposioError::InvalidConfiguration)?;
        base.join(path)
            .map_err(|_| ComposioError::InvalidConfiguration)
    }

    fn persist(&self) -> Result<(), ComposioError> {
        let temporary = self.root.join(format!(".{STATE_FILE}.tmp"));
        if temporary.exists() {
            ensure_regular_file(&temporary)?;
            fs::remove_file(&temporary)?;
        }
        let encoded = serde_json::to_vec_pretty(&self.state)?;
        let mut file = OpenOptions::new()
            .create_new(true)
            .write(true)
            .open(&temporary)?;
        file.write_all(&encoded)?;
        file.sync_all()?;
        drop(file);
        keith_platform::replace_file(&temporary, &self.root.join(STATE_FILE))?;
        Ok(())
    }

    fn audit(
        &self,
        action: &ExternalAction,
        outcome: AuditOutcome,
        now: UtcTimestamp,
    ) -> Result<(), ComposioError> {
        let envelope = AuditEnvelope {
            correlation_id: action.audit_correlation.clone(),
            profile_id: action.profile_id.clone(),
            session_id: action.session_id.clone(),
            acting_principal: action.acting_principal.clone(),
            capability: action.requested_capability,
            risk: action.risk,
            target_digest: action.target_digest.clone(),
            occurred_at: now,
            outcome,
        };
        let mut line = serde_json::to_vec(&envelope)?;
        line.push(b'\n');
        let audit_path = self.root.join(AUDIT_FILE);
        if audit_path.exists() {
            ensure_regular_file(&audit_path)?;
        }
        let mut file = OpenOptions::new()
            .create(true)
            .append(true)
            .open(audit_path)?;
        file.write_all(&line)?;
        file.sync_data()?;
        Ok(())
    }

    #[allow(clippy::too_many_arguments)]
    fn admit_action(
        &self,
        action: &ExternalAction,
        authority: &AuthorityBoundary,
        profile_id: &ProfileId,
        capability: Capability,
        risk: ActionRisk,
        target: &RedactedText,
        payload: &Value,
        now: UtcTimestamp,
    ) -> Result<(), ComposioError> {
        validate_exact_action(action, profile_id, capability, risk, target, payload)?;
        self.audit(action, AuditOutcome::Requested, now)?;
        if let Err(error) = authority.authorizes(action, now) {
            self.audit(action, AuditOutcome::Denied, now)?;
            return Err(error.into());
        }
        Ok(())
    }
}

/// Proxies one JSON-RPC request to a Composio hosted MCP endpoint with no ambient credentials.
///
/// # Errors
///
/// Returns an error for invalid endpoints, transport, status, or response bounds.
pub fn proxy_mcp_once(
    endpoint: &str,
    api_key: &str,
    request: &[u8],
    timeout_ms: u64,
    max_response_bytes: usize,
) -> Result<Vec<u8>, ComposioError> {
    let endpoint = Url::parse(endpoint).map_err(|_| ComposioError::InvalidConfiguration)?;
    if !is_allowed_composio_endpoint(&endpoint) {
        return Err(ComposioError::InvalidConfiguration);
    }
    if api_key.is_empty() || request.is_empty() || timeout_ms == 0 || max_response_bytes == 0 {
        return Err(ComposioError::InvalidConfiguration);
    }
    let http: ureq::Agent = ureq::Agent::config_builder()
        .timeout_global(Some(Duration::from_millis(timeout_ms)))
        .http_status_as_error(false)
        .max_redirects(0)
        .build()
        .into();
    let mut response = http
        .post(endpoint.as_str())
        .header("x-api-key", api_key)
        .header("Content-Type", "application/json")
        .send(request)
        .map_err(|error| map_http_error(&error))?;
    match response.status().as_u16() {
        200..=299 => {}
        401 | 403 => return Err(ComposioError::Authentication),
        408 | 504 => return Err(ComposioError::Timeout),
        _ => return Err(ComposioError::ProviderRejected),
    }
    let limit = u64::try_from(max_response_bytes)
        .unwrap_or(u64::MAX)
        .saturating_add(1);
    let bytes = response
        .body_mut()
        .with_config()
        .limit(limit)
        .read_to_vec()
        .map_err(|error| map_http_error(&error))?;
    if bytes.len() > max_response_bytes {
        Err(ComposioError::SizeLimit)
    } else {
        Ok(bytes)
    }
}

/// Builds the exact non-secret authority resource for starting an account connection.
///
/// # Errors
///
/// Returns an error when the identifiers cannot form bounded safe text.
pub fn connect_target(
    profile_id: &ProfileId,
    toolkit: &str,
) -> Result<RedactedText, ComposioError> {
    RedactedText::parse(format!("composio:connect:{profile_id}:{toolkit}")).map_err(Into::into)
}

/// Builds the exact non-secret authority resource for account lifecycle changes.
///
/// # Errors
///
/// Returns an error when the identifiers cannot form bounded safe text.
pub fn account_target(account: &ConnectedAppAccount) -> Result<RedactedText, ComposioError> {
    RedactedText::parse(format!(
        "composio:account:{}:{}",
        account.toolkit, account.id
    ))
    .map_err(Into::into)
}

/// Builds the exact non-secret authority resource for one account and tool.
///
/// # Errors
///
/// Returns an error when the identifiers cannot form bounded safe text.
pub fn tool_target(
    account: &ConnectedAppAccount,
    tool: &str,
) -> Result<RedactedText, ComposioError> {
    RedactedText::parse(format!(
        "composio:tool:{}:{tool}:{}",
        account.toolkit, account.id
    ))
    .map_err(Into::into)
}

/// Digests the exact target and payload without retaining either in audit state.
///
/// # Panics
///
/// Panics only if a lowercase hexadecimal SHA-256 digest violates the safe-text contract.
pub fn action_digest(target: &RedactedText, payload: &Value) -> RedactedText {
    let mut digest = Sha256::new();
    digest.update(target.as_str().as_bytes());
    digest.update([0]);
    digest.update(serde_json::to_vec(payload).unwrap_or_default());
    RedactedText::parse(format!("{:x}", digest.finalize())).expect("hex digest is safe text")
}

fn validate_exact_action(
    action: &ExternalAction,
    profile_id: &ProfileId,
    capability: Capability,
    risk: ActionRisk,
    target: &RedactedText,
    payload: &Value,
) -> Result<(), ComposioError> {
    if &action.profile_id != profile_id
        || action.requested_capability != capability
        || action.risk != risk
        || &action.target != target
        || action.target_digest != action_digest(target, payload)
    {
        return Err(ComposioError::Authority(ContractError::ApprovalMismatch));
    }
    Ok(())
}

fn parse_session_response(
    profile_id: &ProfileId,
    provider_user_id: &str,
    policy: &ProfileAppPolicy,
    response: &Value,
    generation: u64,
    expires_at: UtcTimestamp,
    now: UtcTimestamp,
) -> Result<ComposioSession, ComposioError> {
    let provider_session_id = required_identifier(response, "session_id")?;
    if let Some(returned_user) = response
        .get("config")
        .and_then(|config| config.get("user_id"))
        .and_then(Value::as_str)
        && returned_user != provider_user_id
    {
        return Err(ComposioError::ProviderSubstitution);
    }
    let mcp = response.get("mcp").ok_or(ComposioError::Protocol)?;
    let mcp_endpoint = required_url(mcp, "url")?;
    validate_mcp_endpoint(&mcp_endpoint)?;
    Ok(ComposioSession {
        profile_id: profile_id.clone(),
        provider_user_id: provider_user_id.to_owned(),
        mcp_server_id: mcp_server_id(profile_id),
        provider_session_id,
        mcp_endpoint,
        policy: policy.clone(),
        state: LifecycleState::Active,
        generation,
        expires_at,
        last_transition_at: now,
        safe_error: None,
    })
}

fn validate_durable_state(
    state: &DurableState,
    limits: &ComposioLimits,
) -> Result<(), ComposioError> {
    if state.policies.len() > limits.max_profiles
        || state.sessions.len() > limits.max_profiles
        || state
            .policies
            .values()
            .any(|policy| policy.validate(limits).is_err())
        || state.sessions.iter().any(|(profile, session)| {
            profile != &session.profile_id
                || state.policies.get(profile) != Some(&session.policy)
                || session.provider_user_id != provider_user_id(profile)
                || session.mcp_server_id != mcp_server_id(profile)
                || !valid_identifier(&session.provider_session_id)
                || validate_mcp_endpoint(&session.mcp_endpoint).is_err()
        })
        || state.accounts.iter().any(|(id, account)| {
            id != &account.id
                || !state.policies.contains_key(&account.profile_id)
                || !valid_identifier(&account.provider_account_id)
                || !valid_toolkit(&account.toolkit)
                || !valid_identifier(&account.auth_config_id)
        })
        || state.policies.keys().any(|profile| {
            state
                .accounts
                .values()
                .filter(|account| &account.profile_id == profile)
                .count()
                > limits.max_accounts_per_profile
        })
    {
        Err(ComposioError::InvalidConfiguration)
    } else {
        Ok(())
    }
}

fn provider_user_id(profile_id: &ProfileId) -> String {
    let mut digest = Sha256::new();
    digest.update(profile_id.to_string());
    format!("keith_{:.32}", format!("{:x}", digest.finalize()))
}

fn mcp_server_id(profile_id: &ProfileId) -> String {
    let mut digest = Sha256::new();
    digest.update(profile_id.to_string());
    format!("composio-{:.24}", format!("{:x}", digest.finalize()))
}

fn mcp_credential_ref(server_id: &str) -> Result<CredentialRef, ComposioError> {
    CredentialRef::new(
        MCP_CREDENTIAL_NAME,
        CredentialOwner::Mcp(server_id.to_owned()),
    )
    .map_err(Into::into)
}

fn with_exact_account(
    arguments: &Value,
    provider_account_id: &str,
) -> Result<Value, ComposioError> {
    let mut arguments = arguments
        .as_object()
        .cloned()
        .ok_or(ComposioError::InvalidPolicy)?;
    if arguments
        .get("account")
        .is_some_and(|account| account.as_str() != Some(provider_account_id))
    {
        return Err(ComposioError::ProviderSubstitution);
    }
    arguments.insert(
        "account".to_owned(),
        Value::String(provider_account_id.to_owned()),
    );
    Ok(Value::Object(arguments))
}

fn redact_tool_result(mut result: McpToolResult) -> McpToolResult {
    for value in &mut result.content {
        redact_value(value, None);
    }
    result
}

fn redact_value(value: &mut Value, key: Option<&str>) {
    if key.is_some_and(secret_key) {
        *value = Value::String("[REDACTED]".to_owned());
        return;
    }
    match value {
        Value::Object(object) => {
            for (key, value) in object {
                redact_value(value, Some(key));
            }
        }
        Value::Array(values) => {
            for value in values {
                redact_value(value, None);
            }
        }
        Value::String(text) if resembles_secret(text) => {
            "[REDACTED]".clone_into(text);
        }
        Value::Null | Value::Bool(_) | Value::Number(_) | Value::String(_) => {}
    }
}

fn secret_key(key: &str) -> bool {
    let key = key.to_ascii_lowercase();
    [
        "authorization",
        "token",
        "secret",
        "password",
        "api_key",
        "apikey",
        "credential",
    ]
    .iter()
    .any(|marker| key.contains(marker))
}

fn resembles_secret(value: &str) -> bool {
    let normalized = value.to_ascii_lowercase();
    normalized.contains("bearer ")
        || normalized.contains("access_token")
        || normalized.contains("refresh_token")
        || normalized.starts_with("sk-")
}

fn required_string(value: &Value, field: &str) -> Result<String, ComposioError> {
    value
        .get(field)
        .and_then(Value::as_str)
        .filter(|value| !value.is_empty())
        .map(str::to_owned)
        .ok_or(ComposioError::Protocol)
}

fn required_identifier(value: &Value, field: &str) -> Result<String, ComposioError> {
    required_string(value, field).and_then(|identifier| {
        if valid_identifier(&identifier) {
            Ok(identifier)
        } else {
            Err(ComposioError::Protocol)
        }
    })
}

fn required_url(value: &Value, field: &str) -> Result<String, ComposioError> {
    let raw = required_string(value, field)?;
    let parsed = Url::parse(&raw).map_err(|_| ComposioError::Protocol)?;
    if !parsed.username().is_empty()
        || parsed.password().is_some()
        || !matches!(parsed.scheme(), "http" | "https")
        || (parsed.scheme() == "http" && !parsed.host_str().is_some_and(is_loopback_host))
    {
        return Err(ComposioError::Protocol);
    }
    Ok(raw)
}

fn required_timestamp(value: &Value, field: &str) -> Result<UtcTimestamp, ComposioError> {
    let value = value.get(field).ok_or(ComposioError::Protocol)?;
    if let Some(millis) = value.as_i64() {
        return Ok(UtcTimestamp::from_unix_millis(millis));
    }
    let timestamp = value.as_str().ok_or(ComposioError::Protocol)?;
    let millis = DateTime::parse_from_rfc3339(timestamp)
        .map_err(|_| ComposioError::Protocol)?
        .timestamp_millis();
    Ok(UtcTimestamp::from_unix_millis(millis))
}

fn response_toolkit(value: &Value) -> Option<&str> {
    value
        .get("toolkit")
        .and_then(|toolkit| toolkit.get("slug").or(Some(toolkit)))
        .and_then(Value::as_str)
        .or_else(|| value.get("toolkit_slug").and_then(Value::as_str))
}

fn expiration(now: UtcTimestamp, ttl_ms: u64) -> Result<UtcTimestamp, ComposioError> {
    let ttl = i64::try_from(ttl_ms).map_err(|_| ComposioError::InvalidConfiguration)?;
    now.unix_millis()
        .checked_add(ttl)
        .map(UtcTimestamp::from_unix_millis)
        .ok_or(ComposioError::InvalidConfiguration)
}

fn validate_api_base(value: &str) -> Result<(), ComposioError> {
    let url = Url::parse(value).map_err(|_| ComposioError::InvalidConfiguration)?;
    if !is_allowed_composio_endpoint(&url) {
        return Err(ComposioError::InvalidConfiguration);
    }
    Ok(())
}

fn validate_mcp_endpoint(value: &str) -> Result<(), ComposioError> {
    let url = Url::parse(value).map_err(|_| ComposioError::InvalidConfiguration)?;
    if is_allowed_composio_endpoint(&url) {
        Ok(())
    } else {
        Err(ComposioError::InvalidConfiguration)
    }
}

fn is_allowed_composio_endpoint(url: &Url) -> bool {
    if !url.username().is_empty() || url.password().is_some() {
        return false;
    }
    let Some(host) = url.host_str() else {
        return false;
    };
    (url.scheme() == "http" && is_loopback_host(host))
        || (url.scheme() == "https" && is_composio_host(host))
}

fn is_composio_host(host: &str) -> bool {
    let host = host.trim_end_matches('.').to_ascii_lowercase();
    host == "composio.dev" || host.ends_with(".composio.dev")
}

fn ensure_regular_file(path: &Path) -> Result<(), ComposioError> {
    let metadata = fs::symlink_metadata(path)?;
    if metadata.file_type().is_symlink() || !metadata.is_file() {
        Err(ComposioError::InvalidConfiguration)
    } else {
        Ok(())
    }
}

fn valid_identifier(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 160
        && value
            .bytes()
            .all(|byte| byte.is_ascii_alphanumeric() || matches!(byte, b'_' | b'-' | b'.' | b':'))
}

fn valid_toolkit(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 80
        && value.bytes().all(|byte| {
            byte.is_ascii_lowercase() || byte.is_ascii_digit() || matches!(byte, b'_' | b'-')
        })
}

fn valid_tool(value: &str) -> bool {
    !value.is_empty()
        && value.len() <= 160
        && value
            .bytes()
            .all(|byte| byte.is_ascii_uppercase() || byte.is_ascii_digit() || byte == b'_')
}

fn valid_scope(value: &str) -> bool {
    !value.is_empty() && value.len() <= 256 && !value.chars().any(char::is_control)
}

fn is_loopback_host(host: &str) -> bool {
    matches!(host, "127.0.0.1" | "localhost" | "[::1]" | "::1")
}

fn map_http_error(error: &ureq::Error) -> ComposioError {
    if matches!(error, ureq::Error::Timeout(_)) {
        ComposioError::Timeout
    } else {
        ComposioError::Transport
    }
}

#[cfg(test)]
mod tests;
