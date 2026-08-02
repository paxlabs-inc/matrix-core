package external

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/effect"
)

type connectorRequest struct {
	SchemaVersion     string                `json:"schema_version"`
	OrganizationID    string                `json:"organization_id"`
	ConnectionID      string                `json:"connection_id"`
	ConnectionVersion uint64                `json:"connection_version"`
	ConnectionHash    contracts.ContentHash `json:"connection_hash"`
	Family            Family                `json:"family"`
	Provider          string                `json:"provider"`
	Operation         string                `json:"operation"`
	SeatID            contracts.SeatID      `json:"seat_id"`
	LeaseID           contracts.LeaseID     `json:"lease_id"`
	Fence             contracts.FenceToken  `json:"fence"`
	Probe             bool                  `json:"probe"`
	IdempotencyKey    string                `json:"idempotency_key"`
	Request           BoundRequest          `json:"request"`
}

type networkFailure struct {
	started  bool
	safeCode string
	cause    error
}

func (failure networkFailure) Error() string {
	return "external adapter: " + failure.safeCode
}

func (failure networkFailure) Unwrap() error { return failure.cause }

func (adapter *Adapter) callProvider(
	ctx context.Context,
	authorized authorizedOperation,
	operation effect.Operation,
	probe bool,
) (providerCall, error) {
	connection := authorized.loaded.connection
	now, timeErr := adapter.store.currentTime()
	if timeErr != nil {
		return providerCall{safeCode: "provider_time_unavailable"}, timeErr
	}
	deadline := authorized.envelope.Request.ExpiresAt
	limit := now.Add(connection.Limits.TotalTimeout)
	if deadline.After(limit) {
		deadline = limit
	}
	providerContext, cancel := context.WithDeadline(ctx, deadline)
	defer cancel()
	var response ProviderResponse
	var failure networkFailure
	var err error
	switch connection.Protocol {
	case ProtocolWorkforceJSON:
		response, failure, err = adapter.callJSONConnector(
			providerContext, authorized, operation, probe,
		)
	case ProtocolMatrixMCP:
		response, failure, err = adapter.callMatrixMCP(
			providerContext, authorized, operation, probe,
		)
	default:
		return providerCall{safeCode: "unsupported_provider_protocol"}, ErrDenied
	}
	call := providerCall{
		response: response, started: failure.started,
		safeCode: failure.safeCode,
	}
	if err != nil {
		if call.safeCode == "" {
			call.safeCode = "provider_request_failed"
		}
		return call, err
	}
	if err := validateProviderResponse(
		connection, authorized.policy, authorized.envelope.Request,
		operation.IdempotencyKey, response, probe, now,
	); err != nil {
		if strings.TrimSpace(call.response.ExternalID) == "" ||
			len(call.response.ExternalID) > 512 || containsLineBreak(call.response.ExternalID) {
			call.response.ExternalID = ""
		}
		call.response.Output = nil
		call.started = authorized.policy.Action.Mutates()
		call.safeCode = "invalid_provider_response"
		return call, err
	}
	call.response = response
	if call.response.FinalURL == "" {
		call.response.FinalURL = authorized.envelope.Request.TargetURL
	}
	call.started = true
	call.safeCode = ""
	call.untrusted = true
	if connection.Protocol == ProtocolMatrixMCP || !response.Authoritative {
		call.authority = AuthorityUntrustedExternal
	} else if connection.Family == FamilyDeployment ||
		connection.Family == FamilyInfrastructure {
		call.authority = AuthorityControlPlane
	} else {
		call.authority = AuthorityProvider
	}
	if probe && !authorized.policy.ProbeAuthoritative {
		call.authority = AuthorityUntrustedExternal
	}
	if !probe && !authorized.policy.DispatchAuthoritative {
		call.authority = AuthorityUntrustedExternal
	}
	return call, nil
}

func (adapter *Adapter) callJSONConnector(
	ctx context.Context,
	authorized authorizedOperation,
	operation effect.Operation,
	probe bool,
) (ProviderResponse, networkFailure, error) {
	connection := authorized.loaded.connection
	policy := authorized.policy
	endpointPath := policy.DispatchPath
	method := policy.Method
	if probe {
		if !policy.ProbeAuthoritative || policy.ProbePath == "" {
			return ProviderResponse{}, networkFailure{
				safeCode: "authoritative_probe_unavailable",
			}, fmt.Errorf("%w: authoritative probe unavailable", ErrAmbiguous)
		}
		endpointPath = policy.ProbePath
		method = policy.ProbeMethod
	}
	endpoint, err := connectorEndpoint(connection.EndpointURL, endpointPath)
	if err != nil {
		return ProviderResponse{}, networkFailure{safeCode: "invalid_provider_endpoint"}, err
	}
	payload, err := json.Marshal(connectorRequest{
		SchemaVersion:     SchemaVersion,
		OrganizationID:    string(operation.OrganizationID),
		ConnectionID:      connection.ID,
		ConnectionVersion: connection.Version,
		ConnectionHash:    authorized.loaded.hash,
		Family:            connection.Family,
		Provider:          connection.Provider,
		Operation:         operation.Name,
		SeatID:            operation.SeatID,
		LeaseID:           operation.LeaseID,
		Fence:             operation.Fence,
		Probe:             probe,
		IdempotencyKey:    operation.IdempotencyKey,
		Request:           authorized.envelope.Request,
	})
	if err != nil {
		return ProviderResponse{}, networkFailure{safeCode: "request_encode_failed"}, err
	}
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(payload))
	if err != nil {
		return ProviderResponse{}, networkFailure{safeCode: "request_build_failed"}, err
	}
	request.Header.Set("Content-Type", "application/vnd.matrix.workforce-external+json")
	request.Header.Set("Accept", "application/vnd.matrix.workforce-external+json, application/json")
	request.Header.Set("X-Workforce-Connection", connection.ID)
	request.Header.Set("X-Workforce-Connection-Version", strconv.FormatUint(connection.Version, 10))
	request.Header.Set("X-Workforce-Operation", operation.Name)
	request.Header.Set("X-Workforce-Account", connection.AccountID)
	request.Header.Set("X-Workforce-Identity", connection.IdentityID)
	if err := applyCredential(request, authorized.loaded.credential); err != nil {
		return ProviderResponse{}, networkFailure{safeCode: "credential_profile_invalid"}, err
	}
	if policy.Idempotency == IdempotencyProviderKey {
		request.Header.Set(policy.IdempotencyHeader, operation.IdempotencyKey)
	} else if policy.Idempotency == IdempotencyResourceVersion {
		request.Header.Set("If-Match", authorized.envelope.Request.ExpectedResourceVersion)
	}
	client := adapter.httpClient(connection, endpoint)
	response, err := client.Do(request)
	if err != nil {
		return ProviderResponse{}, networkFailure{
			started: policy.Action.Mutates(), safeCode: "provider_transport_failed",
			cause: err,
		}, fmt.Errorf("%w: provider transport failed", ErrUnavailable)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		_, _ = readBoundedIdle(ctx, response.Body, 4096, connection.Limits.StreamIdleTimeout)
		return ProviderResponse{}, networkFailure{
			started:  policy.Action.Mutates(),
			safeCode: "provider_http_" + strconv.Itoa(response.StatusCode),
		}, fmt.Errorf("%w: provider returned non-success status", ErrUnavailable)
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	if !strings.Contains(contentType, "json") {
		return ProviderResponse{}, networkFailure{
			started: policy.Action.Mutates(), safeCode: "provider_content_type_invalid",
		}, fmt.Errorf("%w: provider response is not JSON", ErrUnavailable)
	}
	limit := authorized.envelope.Request.OutputBytes
	data, err := readBoundedIdle(
		ctx, response.Body, limit, connection.Limits.StreamIdleTimeout,
	)
	if err != nil {
		return ProviderResponse{}, networkFailure{
			started: policy.Action.Mutates(), safeCode: "provider_response_bounded_read_failed",
			cause: err,
		}, fmt.Errorf("%w: bounded provider response failed", ErrUnavailable)
	}
	var decoded ProviderResponse
	if err := decodeStrict(data, &decoded); err != nil {
		return ProviderResponse{}, networkFailure{
			started: policy.Action.Mutates(), safeCode: "provider_response_decode_failed",
		}, fmt.Errorf("%w: provider response contract is invalid", ErrUnavailable)
	}
	return decoded, networkFailure{started: true}, nil
}

func (adapter *Adapter) httpClient(
	connection Connection,
	endpoint *url.URL,
) *http.Client {
	transport := adapter.transport.Clone()
	dialer := &net.Dialer{
		Timeout:   connection.Limits.ConnectTimeout,
		KeepAlive: 30 * time.Second,
	}
	transport.DialContext = dialer.DialContext
	if connection.PrivateNetworkHTTP {
		transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
			host, port, err := net.SplitHostPort(address)
			if err != nil {
				return nil, fmt.Errorf("external adapter: private provider address is invalid")
			}
			addresses, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil || len(addresses) == 0 {
				return nil, fmt.Errorf("external adapter: private provider DNS unavailable")
			}
			for _, candidate := range addresses {
				if !candidate.IP.IsPrivate() && !candidate.IP.IsLoopback() {
					return nil, fmt.Errorf("external adapter: private provider resolved outside private network")
				}
			}
			return dialer.DialContext(
				ctx, network, net.JoinHostPort(addresses[0].IP.String(), port),
			)
		}
	}
	transport.ResponseHeaderTimeout = connection.Limits.ResponseHeaderTimeout
	transport.TLSHandshakeTimeout = connection.Limits.ConnectTimeout
	transport.IdleConnTimeout = connection.Limits.StreamIdleTimeout
	transport.TLSClientConfig = &tls.Config{MinVersion: tls.VersionTLS12}
	origin := strings.ToLower(endpoint.Scheme + "://" + endpoint.Host)
	return &http.Client{
		Transport: transport,
		CheckRedirect: func(request *http.Request, via []*http.Request) error {
			if uint16(len(via)) > connection.Limits.MaxRedirects {
				return fmt.Errorf("external adapter: provider redirect limit exceeded")
			}
			redirectOrigin := strings.ToLower(request.URL.Scheme + "://" + request.URL.Host)
			if redirectOrigin != origin || request.URL.Path != via[0].URL.Path ||
				request.URL.RawQuery != via[0].URL.RawQuery || request.Method != via[0].Method {
				return fmt.Errorf("external adapter: provider redirect escaped exact endpoint")
			}
			return nil
		},
	}
}

func validateProviderResponse(
	connection Connection,
	policy OperationPolicy,
	request BoundRequest,
	idempotencyKey string,
	response ProviderResponse,
	probe bool,
	now time.Time,
) error {
	expectedRequestHash, err := CanonicalHash(&request)
	if err != nil {
		return err
	}
	if response.SchemaVersion != SchemaVersion ||
		response.IdempotencyKey != idempotencyKey || !response.State.Valid() ||
		response.RequestHash != expectedRequestHash ||
		response.AccountID != connection.AccountID ||
		response.IdentityID != connection.IdentityID ||
		!validUTC(response.ObservedAt) || response.ObservedAt.After(now.Add(5*time.Minute)) ||
		now.Sub(response.ObservedAt) > connection.Limits.MaxObservationAge ||
		strings.TrimSpace(response.ExternalID) == "" || len(response.ExternalID) > 512 ||
		containsLineBreak(response.ExternalID) || len(response.Output) == 0 ||
		uint64(len(response.Output)) > request.OutputBytes || !json.Valid(response.Output) ||
		len(response.ResourceVersion) > 512 || containsLineBreak(response.ResourceVersion) ||
		len(response.FinalURL) > 4096 {
		return fmt.Errorf("%w: provider response fields are invalid", ErrAmbiguous)
	}
	if response.Authoritative {
		if probe && !policy.ProbeAuthoritative || !probe && !policy.DispatchAuthoritative {
			return fmt.Errorf("%w: provider asserted authority outside signed policy", ErrAmbiguous)
		}
	}
	if response.FinalURL == "" {
		response.FinalURL = request.TargetURL
	}
	if response.FinalURL != "" {
		origin, err := targetOrigin(response.FinalURL)
		if err != nil || !contains(connection.TargetOrigins, origin) {
			return fmt.Errorf("%w: provider final origin escaped signed scope", ErrAmbiguous)
		}
		finalPath, err := targetPath(response.FinalURL)
		if err != nil || !allowedPath(connection.NavigationPrefixes, finalPath) {
			return fmt.Errorf("%w: provider final path escaped signed scope", ErrAmbiguous)
		}
	}
	return nil
}

func connectorEndpoint(baseURL, endpointPath string) (*url.URL, error) {
	base, err := url.Parse(baseURL)
	if err != nil {
		return nil, fmt.Errorf("external adapter: invalid provider endpoint")
	}
	joined := *base
	joined.Path = strings.TrimSuffix(base.Path, "/") + endpointPath
	joined.RawPath = ""
	joined.RawQuery = ""
	joined.Fragment = ""
	return &joined, nil
}

func applyCredential(request *http.Request, credential CredentialMaterial) error {
	if err := credential.Validate(); err != nil {
		return err
	}
	switch credential.Kind {
	case CredentialNone:
		return nil
	case CredentialBearer:
		request.Header.Set(credential.HeaderName, credential.Scheme+" "+string(credential.Secret))
	case CredentialAPIKey:
		request.Header.Set(credential.HeaderName, string(credential.Secret))
	case CredentialBasic:
		value := credential.Username + ":" + string(credential.Secret)
		request.Header.Set("Authorization", "Basic "+base64.StdEncoding.EncodeToString([]byte(value)))
	default:
		return fmt.Errorf("external adapter: unsupported credential kind")
	}
	return nil
}

func readBoundedIdle(
	ctx context.Context,
	body io.ReadCloser,
	limit uint64,
	idle time.Duration,
) ([]byte, error) {
	if limit == 0 || limit > 1<<20 || idle <= 0 {
		return nil, fmt.Errorf("external adapter: invalid bounded read policy")
	}
	type readResult struct {
		count int
		err   error
	}
	var output bytes.Buffer
	buffer := make([]byte, 32<<10)
	for {
		result := make(chan readResult, 1)
		go func() {
			count, err := body.Read(buffer)
			result <- readResult{count: count, err: err}
		}()
		timer := time.NewTimer(idle)
		select {
		case <-ctx.Done():
			timer.Stop()
			_ = body.Close()
			return nil, ctx.Err()
		case <-timer.C:
			_ = body.Close()
			return nil, fmt.Errorf("external adapter: provider stream idle timeout")
		case value := <-result:
			timer.Stop()
			if value.count > 0 {
				if uint64(output.Len()+value.count) > limit {
					return nil, fmt.Errorf("external adapter: provider output exceeds limit")
				}
				_, _ = output.Write(buffer[:value.count])
			}
			if value.err == io.EOF {
				return output.Bytes(), nil
			}
			if value.err != nil {
				return nil, value.err
			}
		}
	}
}
