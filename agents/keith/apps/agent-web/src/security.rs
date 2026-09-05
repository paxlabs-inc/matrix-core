use std::collections::{BTreeMap, VecDeque};
use std::time::{Duration, Instant};

use axum::http::{HeaderMap, header};
use ring::digest::{SHA256, digest};
use ring::hmac;
use ring::rand::{SecureRandom, SystemRandom};
use thiserror::Error;

const COOKIE_NAME: &str = "keith_session";
const TOKEN_BYTES: usize = 32;
const COMPARISON_KEY: &[u8] = b"keith-agent-browser-boundary-v1";
const HEX: &[u8; 16] = b"0123456789abcdef";

pub struct BrowserSecurity {
    exact_origin: String,
    login_tag: [u8; 32],
    sessions: std::sync::Mutex<BTreeMap<[u8; 32], SessionRecord>>,
    session_lifetime: Duration,
    mutation_limit_per_second: usize,
    secure_cookie: bool,
}

struct SessionRecord {
    csrf: String,
    csrf_tag: [u8; 32],
    expires_at: Instant,
    recent_mutations: VecDeque<Instant>,
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct IssuedSession {
    pub cookie: String,
    pub csrf: String,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub struct AuthenticatedSession {
    key: [u8; 32],
}

#[derive(Clone, Copy, Debug, Error, Eq, PartialEq)]
pub enum SecurityError {
    #[error("authentication required")]
    Authentication,
    #[error("request origin is not allowed")]
    Origin,
    #[error("CSRF proof is invalid")]
    Csrf,
    #[error("mutation rate limit exceeded")]
    RateLimit,
    #[error("secure random generation failed")]
    Random,
    #[error("authentication state is unavailable")]
    Lock,
}

impl BrowserSecurity {
    pub fn new(
        exact_origin: String,
        login_secret: &[u8],
        session_lifetime: Duration,
        mutation_limit_per_second: usize,
    ) -> Result<Self, SecurityError> {
        if login_secret.is_empty() || session_lifetime.is_zero() || mutation_limit_per_second == 0 {
            return Err(SecurityError::Authentication);
        }
        let login_tag = comparison_tag(login_secret);
        let secure_cookie = exact_origin.starts_with("https://");
        Ok(Self {
            exact_origin,
            login_tag,
            sessions: std::sync::Mutex::new(BTreeMap::new()),
            session_lifetime,
            mutation_limit_per_second,
            secure_cookie,
        })
    }

    pub fn issue(&self, supplied_secret: &[u8]) -> Result<IssuedSession, SecurityError> {
        verify_value(supplied_secret, &self.login_tag)
            .map_err(|_| SecurityError::Authentication)?;
        let token = random_hex()?;
        let csrf = random_hex()?;
        let key = token_key(&token);
        self.sessions
            .lock()
            .map_err(|_| SecurityError::Lock)?
            .insert(
                key,
                SessionRecord {
                    csrf_tag: comparison_tag(csrf.as_bytes()),
                    csrf: csrf.clone(),
                    expires_at: Instant::now() + self.session_lifetime,
                    recent_mutations: VecDeque::with_capacity(self.mutation_limit_per_second),
                },
            );
        let secure = if self.secure_cookie { "; Secure" } else { "" };
        Ok(IssuedSession {
            cookie: format!(
                "{COOKIE_NAME}={token}; HttpOnly; SameSite=Strict; Path=/; Max-Age={}{}",
                self.session_lifetime.as_secs(),
                secure
            ),
            csrf,
        })
    }

    pub fn issue_for_request(
        &self,
        headers: &HeaderMap,
        supplied_secret: &[u8],
    ) -> Result<IssuedSession, SecurityError> {
        self.verify_origin(headers)?;
        self.issue(supplied_secret)
    }

    pub fn authenticate(&self, headers: &HeaderMap) -> Result<AuthenticatedSession, SecurityError> {
        let token = cookie_value(headers, COOKIE_NAME).ok_or(SecurityError::Authentication)?;
        let key = token_key(token);
        let now = Instant::now();
        let mut sessions = self.sessions.lock().map_err(|_| SecurityError::Lock)?;
        sessions.retain(|_, record| record.expires_at > now);
        sessions
            .contains_key(&key)
            .then_some(AuthenticatedSession { key })
            .ok_or(SecurityError::Authentication)
    }

    pub fn authorize_read_socket(
        &self,
        headers: &HeaderMap,
    ) -> Result<AuthenticatedSession, SecurityError> {
        self.verify_origin(headers)?;
        self.authenticate(headers)
    }

    pub fn authorize_mutation(
        &self,
        headers: &HeaderMap,
        csrf: &str,
    ) -> Result<AuthenticatedSession, SecurityError> {
        self.verify_origin(headers)?;
        let session = self.authenticate(headers)?;
        let now = Instant::now();
        let mut sessions = self.sessions.lock().map_err(|_| SecurityError::Lock)?;
        let record = sessions
            .get_mut(&session.key)
            .ok_or(SecurityError::Authentication)?;
        verify_value(csrf.as_bytes(), &record.csrf_tag).map_err(|_| SecurityError::Csrf)?;
        while record
            .recent_mutations
            .front()
            .is_some_and(|seen| now.duration_since(*seen) >= Duration::from_secs(1))
        {
            record.recent_mutations.pop_front();
        }
        if record.recent_mutations.len() >= self.mutation_limit_per_second {
            return Err(SecurityError::RateLimit);
        }
        record.recent_mutations.push_back(now);
        Ok(session)
    }

    pub fn csrf(&self, session: AuthenticatedSession) -> Result<String, SecurityError> {
        self.sessions
            .lock()
            .map_err(|_| SecurityError::Lock)?
            .get(&session.key)
            .map(|record| record.csrf.clone())
            .ok_or(SecurityError::Authentication)
    }

    fn verify_origin(&self, headers: &HeaderMap) -> Result<(), SecurityError> {
        let origin = headers
            .get(header::ORIGIN)
            .and_then(|value| value.to_str().ok())
            .ok_or(SecurityError::Origin)?;
        verify_value(
            origin.as_bytes(),
            &comparison_tag(self.exact_origin.as_bytes()),
        )
        .map_err(|_| SecurityError::Origin)
    }
}

fn cookie_value<'a>(headers: &'a HeaderMap, name: &str) -> Option<&'a str> {
    headers
        .get(header::COOKIE)?
        .to_str()
        .ok()?
        .split(';')
        .filter_map(|part| part.trim().split_once('='))
        .find_map(|(candidate, value)| (candidate == name).then_some(value))
}

fn token_key(token: &str) -> [u8; 32] {
    digest(&SHA256, token.as_bytes())
        .as_ref()
        .try_into()
        .expect("SHA-256 output length is fixed")
}

fn comparison_tag(value: &[u8]) -> [u8; 32] {
    let key = hmac::Key::new(hmac::HMAC_SHA256, COMPARISON_KEY);
    hmac::sign(&key, value)
        .as_ref()
        .try_into()
        .expect("HMAC-SHA256 output length is fixed")
}

fn verify_value(value: &[u8], expected: &[u8; 32]) -> Result<(), ring::error::Unspecified> {
    let key = hmac::Key::new(hmac::HMAC_SHA256, COMPARISON_KEY);
    hmac::verify(&key, value, expected)
}

fn random_hex() -> Result<String, SecurityError> {
    let mut bytes = [0_u8; TOKEN_BYTES];
    SystemRandom::new()
        .fill(&mut bytes)
        .map_err(|_| SecurityError::Random)?;
    let mut encoded = String::with_capacity(TOKEN_BYTES * 2);
    for byte in bytes {
        encoded.push(char::from(HEX[usize::from(byte >> 4)]));
        encoded.push(char::from(HEX[usize::from(byte & 0x0f)]));
    }
    Ok(encoded)
}

impl std::fmt::Debug for BrowserSecurity {
    fn fmt(&self, formatter: &mut std::fmt::Formatter<'_>) -> std::fmt::Result {
        formatter
            .debug_struct("BrowserSecurity")
            .field("exact_origin", &self.exact_origin)
            .field("login_tag", &"[REDACTED]")
            .field("sessions", &"[REDACTED]")
            .field("session_lifetime", &self.session_lifetime)
            .field("mutation_limit_per_second", &self.mutation_limit_per_second)
            .field("secure_cookie", &self.secure_cookie)
            .finish()
    }
}

#[cfg(test)]
mod tests {
    use axum::http::HeaderValue;

    use super::*;

    fn request_headers(cookie: &str, origin: &str) -> HeaderMap {
        let mut headers = HeaderMap::new();
        headers.insert(header::COOKIE, HeaderValue::from_str(cookie).unwrap());
        headers.insert(header::ORIGIN, HeaderValue::from_str(origin).unwrap());
        headers
    }

    #[test]
    fn authentication_origin_csrf_and_rate_are_independent_gates() {
        let security = BrowserSecurity::new(
            "http://127.0.0.1:7341".into(),
            b"login-secret",
            Duration::from_secs(60),
            2,
        )
        .unwrap();
        assert!(matches!(
            security.issue(b"wrong"),
            Err(SecurityError::Authentication)
        ));
        let issued = security.issue(b"login-secret").unwrap();
        let cookie = issued.cookie.split(';').next().unwrap();
        let valid = request_headers(cookie, "http://127.0.0.1:7341");
        let hostile = request_headers(cookie, "http://evil.invalid");
        assert!(matches!(
            security.authorize_mutation(&hostile, &issued.csrf),
            Err(SecurityError::Origin)
        ));
        assert!(matches!(
            security.authorize_mutation(&valid, "wrong"),
            Err(SecurityError::Csrf)
        ));
        security.authorize_mutation(&valid, &issued.csrf).unwrap();
        security.authorize_mutation(&valid, &issued.csrf).unwrap();
        assert!(matches!(
            security.authorize_mutation(&valid, &issued.csrf),
            Err(SecurityError::RateLimit)
        ));
        assert!(!issued.cookie.contains("login-secret"));
        assert!(!format!("{security:?}").contains("login-secret"));
    }
}
