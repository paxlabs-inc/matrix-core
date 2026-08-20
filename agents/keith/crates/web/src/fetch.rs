use std::collections::BTreeSet;
use std::fmt::{self, Debug};
use std::io::{self, Read};
use std::net::{IpAddr, Ipv4Addr, Ipv6Addr, SocketAddr, ToSocketAddrs};
use std::time::Duration;

use keith_provider_core::CancellationToken;
use thiserror::Error;
use ureq::config::Config;
use ureq::http::Uri;
use ureq::unversioned::resolver::{ResolvedSocketAddrs, Resolver};
use ureq::unversioned::transport::{DefaultConnector, NextTimeout};
use url::{Host, Url};

const READ_CHUNK_BYTES: usize = 16 * 1_024;

#[derive(Clone, Debug)]
pub struct WebPolicy {
    pub max_redirects: usize,
    pub max_response_bytes: usize,
    pub max_response_header_bytes: usize,
    pub timeout: Duration,
    pub allowed_media_types: BTreeSet<String>,
}

impl Default for WebPolicy {
    fn default() -> Self {
        Self {
            max_redirects: 5,
            max_response_bytes: 4 * 1_024 * 1_024,
            max_response_header_bytes: 64 * 1_024,
            timeout: Duration::from_secs(30),
            allowed_media_types: [
                "application/json",
                "application/octet-stream",
                "application/pdf",
                "text/csv",
                "text/html",
                "text/plain",
            ]
            .into_iter()
            .map(str::to_owned)
            .collect(),
        }
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct ValidatedDestination {
    url: Url,
    socket_addresses: Vec<SocketAddr>,
}

impl ValidatedDestination {
    pub fn url(&self) -> &Url {
        &self.url
    }

    pub fn socket_addresses(&self) -> &[SocketAddr] {
        &self.socket_addresses
    }
}

#[derive(Clone, Debug, Eq, PartialEq)]
pub struct FetchResponse {
    pub status: u16,
    pub media_type: String,
    pub body: Vec<u8>,
    pub final_url: Url,
    pub redirect_count: usize,
}

#[derive(Clone, Copy, Debug, Eq, PartialEq)]
pub enum FetchProgress {
    DestinationValidated,
    Connecting,
    RedirectValidated,
    ResponseHeadersValidated,
    BodyChunkRead { total_bytes: usize },
    Complete { total_bytes: usize },
}

pub trait FetchProgressSink: Send + Sync {
    fn record(&self, event: FetchProgress);
}

#[derive(Clone, Copy, Debug, Default)]
pub struct NoFetchProgress;

impl FetchProgressSink for NoFetchProgress {
    fn record(&self, _event: FetchProgress) {}
}

#[derive(Debug, Error)]
pub enum WebError {
    #[error("only HTTP and HTTPS destinations are allowed")]
    SchemeDenied,
    #[error("destination credentials and sensitive query parameters are not allowed")]
    SecretInUrl,
    #[error("destination host is missing or invalid")]
    InvalidHost,
    #[error("destination DNS resolution failed")]
    DnsResolution,
    #[error("destination resolves to a private or special-purpose address")]
    PrivateDestination,
    #[error("redirect limit exceeded")]
    RedirectLimit,
    #[error("redirect response did not contain a valid location")]
    InvalidRedirect,
    #[error("response media type is missing or denied")]
    ContentTypeDenied,
    #[error("response exceeds its configured byte limit")]
    ResponseTooLarge,
    #[error("web operation timed out or failed")]
    Transport,
    #[error("web operation was cancelled")]
    Cancelled,
}

pub trait DestinationResolver: Debug + Send + Sync {
    /// Resolves a destination to every candidate socket address.
    ///
    /// # Errors
    ///
    /// Returns an error when the resolver cannot produce trustworthy addresses.
    fn resolve(&self, host: &str, port: u16) -> Result<Vec<SocketAddr>, WebError>;
}

#[derive(Clone, Copy, Debug, Default)]
pub struct SystemDestinationResolver;

impl DestinationResolver for SystemDestinationResolver {
    fn resolve(&self, host: &str, port: u16) -> Result<Vec<SocketAddr>, WebError> {
        (host, port)
            .to_socket_addrs()
            .map(Iterator::collect)
            .map_err(|_| WebError::DnsResolution)
    }
}

#[derive(Debug)]
pub struct DestinationValidator<R = SystemDestinationResolver> {
    resolver: R,
}

impl Default for DestinationValidator<SystemDestinationResolver> {
    fn default() -> Self {
        Self {
            resolver: SystemDestinationResolver,
        }
    }
}

impl<R: DestinationResolver> DestinationValidator<R> {
    pub const fn new(resolver: R) -> Self {
        Self { resolver }
    }

    /// Resolves and validates every address before returning a connection-pinning result.
    ///
    /// # Errors
    ///
    /// Returns an error for invalid URLs, secrets in URLs, failed DNS, or any private/special IP.
    pub fn validate(&self, raw_url: &str) -> Result<ValidatedDestination, WebError> {
        let mut url = Url::parse(raw_url).map_err(|_| WebError::InvalidHost)?;
        validate_url_shape(&url)?;
        url.set_fragment(None);
        let port = url.port_or_known_default().ok_or(WebError::InvalidHost)?;
        let addresses = match url.host().ok_or(WebError::InvalidHost)? {
            Host::Ipv4(address) => vec![SocketAddr::new(IpAddr::V4(address), port)],
            Host::Ipv6(address) => vec![SocketAddr::new(IpAddr::V6(address), port)],
            Host::Domain(domain) => self.resolver.resolve(domain, port)?,
        };
        if addresses.is_empty() {
            return Err(WebError::DnsResolution);
        }
        if addresses.iter().any(|address| !is_public_ip(address.ip())) {
            return Err(WebError::PrivateDestination);
        }
        Ok(ValidatedDestination {
            url,
            socket_addresses: addresses,
        })
    }
}

pub struct SafeWebClient<R = SystemDestinationResolver> {
    policy: WebPolicy,
    validator: DestinationValidator<R>,
}

impl Default for SafeWebClient<SystemDestinationResolver> {
    fn default() -> Self {
        Self::new(WebPolicy::default(), SystemDestinationResolver)
    }
}

impl<R: DestinationResolver> SafeWebClient<R> {
    pub const fn new(policy: WebPolicy, resolver: R) -> Self {
        Self {
            policy,
            validator: DestinationValidator::new(resolver),
        }
    }

    pub fn policy(&self) -> &WebPolicy {
        &self.policy
    }

    /// Fetches a URL after validation, pins the approved DNS result, and manually revalidates
    /// every redirect.
    ///
    /// # Errors
    ///
    /// Returns an error when policy validation, transport, cancellation, or response bounds fail.
    pub fn fetch(
        &self,
        raw_url: &str,
        cancellation: &CancellationToken,
        progress: &dyn FetchProgressSink,
    ) -> Result<FetchResponse, WebError> {
        check_cancelled(cancellation)?;
        let mut destination = self.validator.validate(raw_url)?;
        progress.record(FetchProgress::DestinationValidated);

        for redirect_count in 0..=self.policy.max_redirects {
            check_cancelled(cancellation)?;
            progress.record(FetchProgress::Connecting);
            let mut response = self.call_pinned(&destination)?;
            let status = response.status().as_u16();
            if (300..400).contains(&status) {
                if redirect_count == self.policy.max_redirects {
                    return Err(WebError::RedirectLimit);
                }
                let location = response
                    .headers()
                    .get("location")
                    .and_then(|value| value.to_str().ok())
                    .ok_or(WebError::InvalidRedirect)?;
                let next_url = destination
                    .url
                    .join(location)
                    .map_err(|_| WebError::InvalidRedirect)?;
                destination = self.validator.validate(next_url.as_str())?;
                progress.record(FetchProgress::RedirectValidated);
                continue;
            }

            let media_type = response_media_type(&response, &self.policy)?;
            enforce_content_length(&response, self.policy.max_response_bytes)?;
            progress.record(FetchProgress::ResponseHeadersValidated);
            let body = read_bounded(
                response.body_mut().as_reader(),
                self.policy.max_response_bytes,
                cancellation,
                progress,
            )?;
            progress.record(FetchProgress::Complete {
                total_bytes: body.len(),
            });
            return Ok(FetchResponse {
                status,
                media_type,
                body,
                final_url: destination.url,
                redirect_count,
            });
        }
        Err(WebError::RedirectLimit)
    }

    fn call_pinned(
        &self,
        destination: &ValidatedDestination,
    ) -> Result<ureq::http::Response<ureq::Body>, WebError> {
        let config = Config::builder()
            .http_status_as_error(false)
            .max_redirects(0)
            .max_redirects_will_error(false)
            .max_response_header_size(self.policy.max_response_header_bytes)
            .proxy(None)
            .timeout_global(Some(self.policy.timeout))
            .build();
        let resolver = PinnedResolver::new(destination.socket_addresses.clone());
        let agent = ureq::Agent::with_parts(config, DefaultConnector::default(), resolver);
        agent
            .get(destination.url.as_str())
            .header("accept", "*/*")
            .call()
            .map_err(|_| WebError::Transport)
    }
}

#[derive(Clone)]
struct PinnedResolver {
    socket_addresses: Vec<SocketAddr>,
}

impl PinnedResolver {
    const fn new(socket_addresses: Vec<SocketAddr>) -> Self {
        Self { socket_addresses }
    }
}

impl Debug for PinnedResolver {
    fn fmt(&self, formatter: &mut fmt::Formatter<'_>) -> fmt::Result {
        formatter
            .debug_struct("PinnedResolver")
            .field("address_count", &self.socket_addresses.len())
            .finish()
    }
}

impl Resolver for PinnedResolver {
    fn resolve(
        &self,
        _uri: &Uri,
        _config: &Config,
        _timeout: NextTimeout,
    ) -> Result<ResolvedSocketAddrs, ureq::Error> {
        let mut resolved = self.empty();
        for address in self.socket_addresses.iter().copied().take(16) {
            resolved.push(address);
        }
        if resolved.is_empty() {
            Err(ureq::Error::HostNotFound)
        } else {
            Ok(resolved)
        }
    }
}

fn validate_url_shape(url: &Url) -> Result<(), WebError> {
    if !matches!(url.scheme(), "http" | "https") {
        return Err(WebError::SchemeDenied);
    }
    if url.username() != "" || url.password().is_some() {
        return Err(WebError::SecretInUrl);
    }
    if url.host().is_none() {
        return Err(WebError::InvalidHost);
    }
    if url
        .query_pairs()
        .any(|(name, _)| sensitive_query_name(&name))
    {
        return Err(WebError::SecretInUrl);
    }
    Ok(())
}

fn sensitive_query_name(name: &str) -> bool {
    let normalized = name.to_ascii_lowercase().replace(['-', '.'], "_");
    [
        "access_token",
        "api_key",
        "apikey",
        "auth",
        "authorization",
        "credential",
        "jwt",
        "password",
        "secret",
        "session",
        "token",
    ]
    .iter()
    .any(|sensitive| normalized == *sensitive || normalized.ends_with(&format!("_{sensitive}")))
}

fn response_media_type(
    response: &ureq::http::Response<ureq::Body>,
    policy: &WebPolicy,
) -> Result<String, WebError> {
    let media_type = response
        .headers()
        .get("content-type")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.split(';').next())
        .map(str::trim)
        .map(str::to_ascii_lowercase)
        .filter(|value| !value.is_empty())
        .ok_or(WebError::ContentTypeDenied)?;
    if policy.allowed_media_types.contains(&media_type) {
        Ok(media_type)
    } else {
        Err(WebError::ContentTypeDenied)
    }
}

fn enforce_content_length(
    response: &ureq::http::Response<ureq::Body>,
    limit: usize,
) -> Result<(), WebError> {
    if response
        .headers()
        .get("content-length")
        .and_then(|value| value.to_str().ok())
        .and_then(|value| value.parse::<usize>().ok())
        .is_some_and(|length| length > limit)
    {
        Err(WebError::ResponseTooLarge)
    } else {
        Ok(())
    }
}

fn read_bounded(
    mut reader: impl Read,
    limit: usize,
    cancellation: &CancellationToken,
    progress: &dyn FetchProgressSink,
) -> Result<Vec<u8>, WebError> {
    let mut body = Vec::new();
    let mut chunk = [0_u8; READ_CHUNK_BYTES];
    loop {
        check_cancelled(cancellation)?;
        let read = reader.read(&mut chunk).map_err(map_read_error)?;
        if read == 0 {
            return Ok(body);
        }
        if body.len().saturating_add(read) > limit {
            return Err(WebError::ResponseTooLarge);
        }
        body.extend_from_slice(&chunk[..read]);
        progress.record(FetchProgress::BodyChunkRead {
            total_bytes: body.len(),
        });
    }
}

fn map_read_error(_error: io::Error) -> WebError {
    WebError::Transport
}

fn check_cancelled(cancellation: &CancellationToken) -> Result<(), WebError> {
    if cancellation.is_cancelled() {
        Err(WebError::Cancelled)
    } else {
        Ok(())
    }
}

pub fn is_public_ip(address: IpAddr) -> bool {
    match address {
        IpAddr::V4(address) => is_public_ipv4(address),
        IpAddr::V6(address) => is_public_ipv6(address),
    }
}

fn is_public_ipv4(address: Ipv4Addr) -> bool {
    let octets = address.octets();
    if address.is_private()
        || address.is_loopback()
        || address.is_link_local()
        || address.is_broadcast()
        || address.is_documentation()
        || address.is_multicast()
        || address.is_unspecified()
    {
        return false;
    }
    !matches!(
        octets,
        [0 | 240..=255, ..] | [100, 64..=127, ..] | [192, 0, 0, ..] | [198, 18..=19, ..]
    )
}

fn is_public_ipv6(address: Ipv6Addr) -> bool {
    if let Some(mapped) = address.to_ipv4_mapped() {
        return is_public_ipv4(mapped);
    }
    let segments = address.segments();
    let globally_routed_unicast = (segments[0] & 0xe000) == 0x2000;
    globally_routed_unicast
        && !(address.is_unspecified()
            || address.is_loopback()
            || address.is_multicast()
            || (segments[0] & 0xfe00) == 0xfc00
            || (segments[0] & 0xffc0) == 0xfe80
            || (segments[0] & 0xffc0) == 0xfec0
            || (segments[0] == 0x2001 && segments[1] == 0x0db8))
}

#[cfg(test)]
mod tests {
    use std::collections::VecDeque;
    use std::io::Cursor;
    use std::sync::Mutex;

    use super::*;

    #[derive(Debug)]
    struct SequenceResolver {
        answers: Mutex<VecDeque<Vec<SocketAddr>>>,
    }

    impl SequenceResolver {
        fn new(answers: impl IntoIterator<Item = Vec<SocketAddr>>) -> Self {
            Self {
                answers: Mutex::new(answers.into_iter().collect()),
            }
        }
    }

    impl DestinationResolver for SequenceResolver {
        fn resolve(&self, _host: &str, _port: u16) -> Result<Vec<SocketAddr>, WebError> {
            self.answers
                .lock()
                .map_err(|_| WebError::DnsResolution)?
                .pop_front()
                .ok_or(WebError::DnsResolution)
        }
    }

    fn socket(address: &str) -> SocketAddr {
        address.parse().expect("valid test address")
    }

    #[test]
    fn rejects_ssrf_destinations_and_non_http_schemes() {
        let validator = DestinationValidator::new(SequenceResolver::new([
            vec![socket("127.0.0.1:80")],
            vec![socket("169.254.169.254:80")],
            vec![socket("10.1.2.3:80")],
        ]));
        assert!(matches!(
            validator.validate("http://first.invalid/"),
            Err(WebError::PrivateDestination)
        ));
        assert!(matches!(
            validator.validate("http://metadata.invalid/latest"),
            Err(WebError::PrivateDestination)
        ));
        assert!(matches!(
            validator.validate("http://internal.invalid/"),
            Err(WebError::PrivateDestination)
        ));
        assert!(matches!(
            validator.validate("file:///etc/passwd"),
            Err(WebError::SchemeDenied)
        ));
    }

    #[test]
    fn repeated_validation_catches_dns_rebinding() {
        let validator = DestinationValidator::new(SequenceResolver::new([
            vec![socket("93.184.216.34:443")],
            vec![socket("127.0.0.1:443")],
        ]));
        let first = validator
            .validate("https://rebind.invalid/")
            .expect("first public answer is accepted");
        assert_eq!(first.socket_addresses(), &[socket("93.184.216.34:443")]);
        assert!(matches!(
            validator.validate("https://rebind.invalid/next"),
            Err(WebError::PrivateDestination)
        ));
    }

    #[test]
    fn redirect_targets_are_revalidated_before_connection() {
        let client = SafeWebClient::new(
            WebPolicy::default(),
            SequenceResolver::new([
                vec![socket("93.184.216.34:443")],
                vec![socket("169.254.169.254:80")],
            ]),
        );
        let initial = client
            .validator
            .validate("https://public.invalid/start")
            .expect("initial public destination");
        let redirect = initial
            .url
            .join("http://metadata.invalid/latest")
            .expect("redirect URL");
        assert!(matches!(
            client.validator.validate(redirect.as_str()),
            Err(WebError::PrivateDestination)
        ));
    }

    #[test]
    fn denies_secret_bearing_urls() {
        let validator = DestinationValidator::new(SequenceResolver::new([
            vec![socket("93.184.216.34:443")],
            vec![socket("93.184.216.34:443")],
        ]));
        assert!(matches!(
            validator.validate("https://user:password@example.invalid/"),
            Err(WebError::SecretInUrl)
        ));
        assert!(matches!(
            validator.validate("https://example.invalid/?api-key=supersecret"),
            Err(WebError::SecretInUrl)
        ));
    }

    #[test]
    fn rejects_all_known_special_address_classes() {
        for address in [
            "0.0.0.0",
            "10.0.0.1",
            "100.64.0.1",
            "127.0.0.1",
            "169.254.1.1",
            "172.16.0.1",
            "192.0.0.1",
            "192.0.2.1",
            "192.168.0.1",
            "198.18.0.1",
            "198.51.100.1",
            "203.0.113.1",
            "224.0.0.1",
            "255.255.255.255",
            "::",
            "::1",
            "::ffff:127.0.0.1",
            "fc00::1",
            "fe80::1",
            "2001:db8::1",
            "ff02::1",
        ] {
            assert!(
                !is_public_ip(address.parse().expect("valid address")),
                "{address}"
            );
        }
        assert!(is_public_ip("93.184.216.34".parse().unwrap()));
        assert!(is_public_ip("2606:4700:4700::1111".parse().unwrap()));
    }

    #[test]
    fn bounded_reader_stops_output_floods_and_cancellation() {
        let bytes = vec![b'x'; 33_000];
        assert!(matches!(
            read_bounded(
                Cursor::new(bytes),
                32_000,
                &CancellationToken::default(),
                &NoFetchProgress
            ),
            Err(WebError::ResponseTooLarge)
        ));
        let cancellation = CancellationToken::default();
        cancellation.cancel();
        assert!(matches!(
            read_bounded(Cursor::new(b"hello"), 100, &cancellation, &NoFetchProgress),
            Err(WebError::Cancelled)
        ));
    }
}
