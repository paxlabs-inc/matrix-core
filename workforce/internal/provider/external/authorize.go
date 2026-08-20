package external

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/url"
	"strings"
	"time"

	"centra/workforce/internal/contracts"
	"centra/workforce/internal/effect"
	"centra/workforce/internal/lease"
	"centra/workforce/internal/skills"
)

type authorizedOperation struct {
	loaded      loadedConnection
	policy      OperationPolicy
	envelope    Envelope
	origin      string
	requestHash contracts.ContentHash
}

func (adapter *Adapter) authorize(
	ctx context.Context,
	ctxTime time.Time,
	operation effect.Operation,
) (authorizedOperation, error) {
	var envelope Envelope
	if err := decodeStrict(operation.Input, &envelope); err != nil ||
		envelope.SchemaVersion != SchemaVersion ||
		token("connection id", envelope.ConnectionID) != nil ||
		envelope.ConnectionVersion == 0 || envelope.ConnectionHash.Validate() != nil {
		return authorizedOperation{}, fmt.Errorf("%w: canonical envelope is invalid", ErrDenied)
	}
	canonicalEnvelope, err := contracts.EncodeCanonical(&envelope)
	if err != nil || !bytes.Equal(canonicalEnvelope, operation.Input) {
		return authorizedOperation{}, fmt.Errorf("%w: envelope is not canonical", ErrDenied)
	}
	loaded, err := adapter.store.loadActive(
		ctx, operation.OrganizationID,
		envelope.ConnectionID, envelope.ConnectionVersion,
	)
	if err != nil {
		return authorizedOperation{}, err
	}
	connection := loaded.connection
	if connection.OrganizationID != adapter.organizationID ||
		connection.ID != adapter.connectionID || connection.AdapterName != adapter.name ||
		envelope.ConnectionID != connection.ID ||
		envelope.ConnectionVersion != connection.Version ||
		envelope.ConnectionHash != loaded.hash {
		return authorizedOperation{}, fmt.Errorf("%w: connection binding mismatch", ErrDenied)
	}
	if !validUTC(ctxTime) || connection.EffectiveAt.After(ctxTime) ||
		!connection.ExpiresAt.After(ctxTime) {
		return authorizedOperation{}, fmt.Errorf("%w: connection is outside its validity", ErrDenied)
	}
	grant := envelope.Grant
	if grant.Request.Validate() != nil || grant.Fence.Validate() != nil ||
		grant.State != lease.StateActive || grant.OrganizationID != operation.OrganizationID ||
		grant.SeatID != operation.SeatID || grant.ID != operation.LeaseID ||
		grant.Fence != operation.Fence || !grant.ExpiresAt.After(ctxTime) {
		return authorizedOperation{}, fmt.Errorf("%w: lease and fence binding mismatch", ErrDenied)
	}
	for _, required := range connection.Authority.PolicyRefs {
		matched := false
		for _, bound := range grant.Policies {
			if bound == required {
				matched = true
				break
			}
		}
		if !matched {
			return authorizedOperation{}, fmt.Errorf("%w: lease omits signed external policy", ErrDenied)
		}
	}
	policy, found := connection.Operation(operation.Name)
	if !found {
		return authorizedOperation{}, fmt.Errorf("%w: operation is not allowlisted", ErrDenied)
	}
	request := envelope.Request
	if err := validateBoundRequest(connection, policy, request, operation.IdempotencyKey, ctxTime); err != nil {
		return authorizedOperation{}, err
	}
	if request.ExpiresAt.After(grant.ExpiresAt) || request.ExpiresAt.After(connection.ExpiresAt) {
		return authorizedOperation{}, fmt.Errorf("%w: request exceeds authority expiry", ErrDenied)
	}
	origin, err := targetOrigin(request.TargetURL)
	if err != nil {
		return authorizedOperation{}, err
	}
	requestHash, err := CanonicalHash(&request)
	if err != nil {
		return authorizedOperation{}, err
	}
	return authorizedOperation{
		loaded: loaded, policy: policy, envelope: envelope,
		origin: origin, requestHash: requestHash,
	}, nil
}

func validateBoundRequest(
	connection Connection,
	policy OperationPolicy,
	request BoundRequest,
	idempotencyKey string,
	now time.Time,
) error {
	if request.Action != policy.Action || request.AccountID != connection.AccountID ||
		request.IdentityID != connection.IdentityID ||
		!contains(connection.NavigationClasses, request.Navigation) ||
		!contains(connection.DataClassifications, request.DataClassification) ||
		request.OutputBytes == 0 || request.OutputBytes > connection.Limits.OutputBytes ||
		!validUTC(request.IssuedAt) || !validUTC(request.ExpiresAt) ||
		request.IssuedAt.After(now) || !request.ExpiresAt.After(now) ||
		!request.ExpiresAt.After(request.IssuedAt) ||
		request.ExpiresAt.Sub(request.IssuedAt) > connection.Limits.TotalTimeout {
		return fmt.Errorf("%w: request identity, time, or resource scope mismatch", ErrDenied)
	}
	if len(request.TargetURL) == 0 || len(request.TargetURL) > 4096 {
		return fmt.Errorf("%w: target URL is outside bounds", ErrDenied)
	}
	origin, err := targetOrigin(request.TargetURL)
	if err != nil || !contains(connection.TargetOrigins, origin) {
		return fmt.Errorf("%w: target origin is outside the signed scope", ErrDenied)
	}
	target, err := targetPath(request.TargetURL)
	if err != nil || !allowedPath(connection.NavigationPrefixes, target) {
		return fmt.Errorf("%w: target path is outside the signed navigation scope", ErrDenied)
	}
	if len(request.Body) == 0 || len(request.Body) > 192<<10 ||
		!json.Valid(request.Body) {
		return fmt.Errorf("%w: operation body is invalid or unbounded", ErrDenied)
	}
	var body any
	if err := decodeStrict(request.Body, &body); err != nil {
		return fmt.Errorf("%w: operation body is invalid", ErrDenied)
	}
	canonicalBody, err := json.Marshal(body)
	if err != nil || !bytes.Equal(canonicalBody, request.Body) {
		return fmt.Errorf("%w: operation body is not canonical", ErrDenied)
	}
	if err := validateEmbeddedDestinations(body, connection.TargetOrigins, 0); err != nil {
		return err
	}
	if request.Upload != nil {
		if err := request.Upload.Validate(connection.Upload); err != nil {
			return err
		}
	} else if policy.Action == ActionUpload {
		return fmt.Errorf("%w: upload action requires exact inline content", ErrDenied)
	}
	if request.Upload != nil && policy.Action != ActionUpload && policy.Action != ActionPublish &&
		policy.Action != ActionDeploy && policy.Action != ActionSubmit {
		return fmt.Errorf("%w: operation action does not permit an upload", ErrDenied)
	}
	if request.Download {
		if connection.Download.Mode == TransferDeny ||
			request.OutputBytes > connection.Download.MaxBytes {
			return fmt.Errorf("%w: download is outside signed policy", ErrDenied)
		}
	} else if policy.Action == ActionDownload {
		return fmt.Errorf("%w: download action requires an explicit download", ErrDenied)
	}
	if policy.Idempotency == IdempotencyResourceVersion &&
		token("expected resource version", request.ExpectedResourceVersion) != nil {
		return fmt.Errorf("%w: resource-version idempotency requires If-Match authority", ErrDenied)
	}
	if policy.Idempotency != IdempotencyResourceVersion && request.ExpectedResourceVersion != "" {
		return fmt.Errorf("%w: unexpected resource-version authority", ErrDenied)
	}
	if policy.CompensatesOperation != "" {
		if token("compensates key", request.CompensatesKey) != nil ||
			request.CompensatesKey == idempotencyKey {
			return fmt.Errorf("%w: compensation target is invalid", ErrDenied)
		}
	} else if request.CompensatesKey != "" {
		return fmt.Errorf("%w: non-compensation operation cannot resolve another effect", ErrDenied)
	}
	if err := validateGovernance(connection, policy, request); err != nil {
		return err
	}
	return nil
}

func validateGovernance(
	connection Connection,
	policy OperationPolicy,
	request BoundRequest,
) error {
	governance := connection.Governance
	fields := []struct {
		name    string
		value   string
		allowed []string
	}{
		{"channel", request.Channel, governance.Channels},
		{"audience", request.Audience, governance.Audiences},
		{"environment", request.Environment, governance.Environments},
		{"jurisdiction", request.Jurisdiction, governance.Jurisdictions},
		{"consent", request.ConsentRef, governance.ConsentRefs},
		{"recipient", request.Recipient, governance.Recipients},
		{"counterparty", request.Counterparty, governance.Counterparties},
		{"brand policy", request.BrandPolicyRef, governance.BrandPolicyRefs},
		{"legal policy", request.LegalPolicyRef, governance.LegalPolicyRefs},
		{"security policy", request.SecurityPolicyRef, governance.SecurityPolicyRefs},
		{"rollback policy", request.RollbackPolicyRef, governance.RollbackPolicyRefs},
	}
	for _, field := range fields {
		if field.value == "" {
			continue
		}
		if !contains(field.allowed, field.value) {
			return fmt.Errorf("%w: %s is outside signed policy", ErrDenied, field.name)
		}
	}
	for _, claim := range request.Claims {
		if !contains(governance.Claims, claim) {
			return fmt.Errorf("%w: claim is outside signed policy", ErrDenied)
		}
	}
	if len(request.Claims) > 128 || !validDistinctValues(request.Claims, 1024) {
		return fmt.Errorf("%w: claims are invalid", ErrDenied)
	}
	publication := connection.Family == FamilyPublication ||
		connection.Family == FamilyWebsite
	deployment := connection.Family == FamilyDeployment ||
		connection.Family == FamilyInfrastructure
	if policy.Action.Mutates() && request.Jurisdiction == "" {
		return fmt.Errorf("%w: mutation requires exact jurisdiction", ErrDenied)
	}
	if policy.Action.Mutates() && request.SecurityPolicyRef == "" {
		return fmt.Errorf("%w: mutation requires exact security policy", ErrDenied)
	}
	if policy.EffectClass == skills.EffectReversible && request.RollbackPolicyRef == "" {
		return fmt.Errorf("%w: reversible mutation requires exact rollback policy", ErrDenied)
	}
	if publication && policy.Action.Mutates() {
		if request.Channel == "" || request.Audience == "" || len(request.Claims) == 0 ||
			request.BrandPolicyRef == "" || request.LegalPolicyRef == "" ||
			request.SecurityPolicyRef == "" || request.RollbackPolicyRef == "" {
			return fmt.Errorf("%w: publication governance is incomplete", ErrDenied)
		}
	}
	if deployment && policy.Action.Mutates() {
		if request.Environment == "" || request.SecurityPolicyRef == "" ||
			request.RollbackPolicyRef == "" {
			return fmt.Errorf("%w: deployment governance is incomplete", ErrDenied)
		}
	}
	if request.DataClassification == DataCustomerPersonal &&
		(request.ConsentRef == "" || request.Jurisdiction == "") {
		return fmt.Errorf("%w: customer personal data requires consent and jurisdiction", ErrDenied)
	}
	if request.Recipient != "" || request.Counterparty != "" {
		if request.ConsentRef == "" || request.LegalPolicyRef == "" {
			return fmt.Errorf("%w: recipient or counterparty effect requires consent and legal policy", ErrDenied)
		}
	}
	return nil
}

func validateEmbeddedDestinations(value any, origins []string, depth int) error {
	if depth > 32 {
		return fmt.Errorf("%w: operation body nesting exceeds limit", ErrDenied)
	}
	switch typed := value.(type) {
	case map[string]any:
		if len(typed) > 1024 {
			return fmt.Errorf("%w: operation body object exceeds limit", ErrDenied)
		}
		for key, child := range typed {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "authorization") || strings.Contains(lower, "credential") ||
				strings.Contains(lower, "password") || strings.Contains(lower, "api_key") ||
				strings.Contains(lower, "system_instruction") || strings.Contains(lower, "mandate") ||
				strings.Contains(lower, "constitution") {
				return fmt.Errorf("%w: operation body contains a protected control or credential field", ErrDenied)
			}
			if err := validateEmbeddedDestinations(child, origins, depth+1); err != nil {
				return err
			}
		}
	case []any:
		if len(typed) > 4096 {
			return fmt.Errorf("%w: operation body array exceeds limit", ErrDenied)
		}
		for _, child := range typed {
			if err := validateEmbeddedDestinations(child, origins, depth+1); err != nil {
				return err
			}
		}
	case string:
		if len(typed) > 128<<10 || containsLineBreak(typed) && len(typed) > 8192 {
			return fmt.Errorf("%w: operation body string exceeds limit", ErrDenied)
		}
		parsed, err := url.Parse(strings.TrimSpace(typed))
		if err == nil && parsed.IsAbs() {
			origin, originErr := targetOrigin(typed)
			if originErr != nil || !contains(origins, origin) {
				return fmt.Errorf("%w: embedded destination is outside signed origins", ErrDenied)
			}
		}
	}
	return nil
}

func allowedPath(prefixes []string, target string) bool {
	for _, prefix := range prefixes {
		if target == prefix || strings.HasPrefix(target, strings.TrimSuffix(prefix, "/")+"/") {
			return true
		}
	}
	return false
}

func decodeStrict(data []byte, target any) error {
	if err := rejectDuplicateJSONKeys(data); err != nil {
		return err
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	decoder.UseNumber()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return fmt.Errorf("external adapter: trailing input")
	}
	return nil
}

func rejectDuplicateJSONKeys(data []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := consumeJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return fmt.Errorf("external adapter: trailing JSON data")
	}
	return nil
}

func consumeJSONValue(decoder *json.Decoder, depth int) error {
	if depth > 64 {
		return fmt.Errorf("external adapter: JSON nesting exceeds limit")
	}
	tokenValue, err := decoder.Token()
	if err != nil {
		return err
	}
	delimiter, compound := tokenValue.(json.Delim)
	if !compound {
		return nil
	}
	switch delimiter {
	case '{':
		seen := make(map[string]bool)
		for decoder.More() {
			keyToken, err := decoder.Token()
			if err != nil {
				return err
			}
			key, ok := keyToken.(string)
			if !ok || seen[key] {
				return fmt.Errorf("external adapter: duplicate or invalid JSON key")
			}
			seen[key] = true
			if err := consumeJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim('}') {
			return fmt.Errorf("external adapter: invalid JSON object")
		}
	case '[':
		for decoder.More() {
			if err := consumeJSONValue(decoder, depth+1); err != nil {
				return err
			}
		}
		end, err := decoder.Token()
		if err != nil || end != json.Delim(']') {
			return fmt.Errorf("external adapter: invalid JSON array")
		}
	default:
		return fmt.Errorf("external adapter: invalid JSON delimiter")
	}
	return nil
}
