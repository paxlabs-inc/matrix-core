package customer

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/effect"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/provider/external"
	"matrix/workforce/internal/skills"
)

type authorizedOperation struct {
	connection      loadedConnection
	customer        loadedCustomer
	consent         *loadedConsent
	policy          OperationPolicy
	envelope        Envelope
	requestHash     contracts.ContentHash
	recipientHash   contracts.ContentHash
	frequencyLimit  uint16
	frequencyWindow time.Duration
}

func (adapter *Adapter) authorize(
	ctx context.Context,
	now time.Time,
	operation effect.Operation,
	reconcile bool,
) (authorizedOperation, error) {
	var envelope Envelope
	if err := decodeStrict(operation.Input, &envelope); err != nil {
		return authorizedOperation{}, fmt.Errorf("%w: canonical envelope is invalid", ErrDenied)
	}
	if err := envelope.Request.Validate(); err != nil {
		return authorizedOperation{}, err
	}
	if err := envelope.Validate(); err != nil {
		return authorizedOperation{}, fmt.Errorf("%w: canonical envelope is invalid", ErrDenied)
	}
	canonical, err := contracts.EncodeCanonical(&envelope)
	if err != nil || !bytes.Equal(canonical, operation.Input) {
		return authorizedOperation{}, fmt.Errorf("%w: envelope is not canonical", ErrDenied)
	}
	connection, err := adapter.store.loadActive(
		ctx, operation.OrganizationID, envelope.ConnectionID,
		envelope.ConnectionVersion, envelope.ConnectionHash,
	)
	if err != nil {
		return authorizedOperation{}, err
	}
	if connection.connection.OrganizationID != adapter.organizationID ||
		connection.connection.ID != adapter.connectionID ||
		connection.connection.AdapterName != adapter.name ||
		connection.connection.ExternalAdapterName != adapter.upstream.Name() {
		return authorizedOperation{}, fmt.Errorf("%w: adapter or connection binding mismatch", ErrDenied)
	}
	grant := envelope.Grant
	if grant.Request.Validate() != nil || grant.Fence.Validate() != nil ||
		grant.State != lease.StateActive || grant.OrganizationID != operation.OrganizationID ||
		grant.SeatID != operation.SeatID || grant.ID != operation.LeaseID ||
		grant.Fence != operation.Fence || !grant.ExpiresAt.After(now) {
		return authorizedOperation{}, fmt.Errorf("%w: lease and fence binding mismatch", ErrDenied)
	}
	for _, required := range connection.connection.Authority.PolicyRefs {
		matched := false
		for _, bound := range grant.Policies {
			if required == bound {
				matched = true
				break
			}
		}
		if !matched {
			return authorizedOperation{}, fmt.Errorf("%w: lease omits customer policy authority", ErrDenied)
		}
	}
	policy, found := connection.connection.Operation(operation.Name)
	if !found || envelope.Request.Operation != operation.Name ||
		envelope.Request.Family != policy.Family || envelope.Request.Action != policy.Action {
		return authorizedOperation{}, fmt.Errorf("%w: operation is not allowlisted", ErrDenied)
	}
	request := envelope.Request
	if !request.ExpiresAt.After(now) || request.IssuedAt.After(now) ||
		request.ExpiresAt.After(grant.ExpiresAt) ||
		request.ExpiresAt.After(connection.connection.ExpiresAt) ||
		request.OutputBytes > connection.connection.Limits.OutputBytes {
		return authorizedOperation{}, fmt.Errorf("%w: request is outside time or output authority", ErrDenied)
	}
	policyHistorical := reconcile || policy.Action == ActionCompensate
	var customer loadedCustomer
	if policyHistorical {
		customer, err = adapter.store.loadCustomerVersion(
			ctx, operation.OrganizationID, request.CustomerID, request.CustomerVersion,
		)
	} else {
		customer, err = adapter.store.loadCustomerCurrent(ctx, operation.OrganizationID, request.CustomerID)
	}
	if err != nil {
		return authorizedOperation{}, err
	}
	if customer.customer.Version != request.CustomerVersion || customer.hash != request.CustomerHash ||
		customer.customer.ConnectionID != connection.connection.ID ||
		customer.customer.ConnectionVersion != connection.connection.Version ||
		customer.customer.RecipientRef != request.RecipientRef ||
		customer.customer.DestinationHash != request.DestinationHash {
		return authorizedOperation{}, fmt.Errorf("%w: exact customer scope binding mismatch", ErrDenied)
	}
	if err := validateRequestGovernance(connection.connection, customer.customer, policy, request); err != nil {
		return authorizedOperation{}, err
	}
	var consent *loadedConsent
	frequencyLimit := connection.connection.Limits.MaxPerRecipientWindow
	frequencyWindow := connection.connection.Limits.FrequencyWindow
	if policy.RequiresConsent {
		if request.ConsentID == "" {
			return authorizedOperation{}, ErrConsent
		}
		var loaded loadedConsent
		if reconcile {
			loaded, err = adapter.store.loadConsentVersion(
				ctx, operation.OrganizationID, request.ConsentID, request.ConsentVersion,
			)
		} else {
			loaded, err = adapter.store.loadConsentCurrent(ctx, operation.OrganizationID, request.ConsentID)
		}
		if err != nil {
			return authorizedOperation{}, err
		}
		if loaded.consent.Version != request.ConsentVersion || loaded.hash != request.ConsentHash ||
			loaded.consent.ConnectionID != connection.connection.ID ||
			loaded.consent.ConnectionVersion != connection.connection.Version ||
			loaded.consent.CustomerID != customer.customer.ID ||
			loaded.consent.CustomerVersion != customer.customer.Version ||
			loaded.consent.RecipientRef != request.RecipientRef ||
			loaded.consent.DestinationHash != request.DestinationHash ||
			loaded.consent.Channel != request.Channel || loaded.consent.Purpose != request.Purpose ||
			loaded.consent.Jurisdiction != request.Jurisdiction ||
			loaded.consent.PrivacyPolicyRef != request.PrivacyPolicyRef ||
			!containsBasis(policy.ConsentBases, loaded.consent.Basis) {
			return authorizedOperation{}, fmt.Errorf("%w: exact consent scope binding mismatch", ErrConsent)
		}
		if !reconcile && loaded.consent.State == ConsentWithdrawn {
			return authorizedOperation{}, ErrUnsubscribed
		}
		validAt := now
		if reconcile {
			validAt = request.IssuedAt
		}
		if loaded.consent.State != ConsentGranted || loaded.consent.EffectiveAt.After(validAt) ||
			!loaded.consent.ExpiresAt.After(validAt) {
			return authorizedOperation{}, ErrConsent
		}
		if !reconcile {
			latestID, latestVersion, latestState, err := adapter.store.latestConsentForScope(
				ctx, operation.OrganizationID, connection.connection.ID,
				customer.customer.ID, request.DestinationHash.Digest,
				request.Channel, request.Purpose, now,
			)
			if err != nil {
				return authorizedOperation{}, err
			}
			if latestState == ConsentWithdrawn {
				return authorizedOperation{}, ErrUnsubscribed
			}
			if latestID != loaded.consent.ID || latestVersion != loaded.consent.Version {
				return authorizedOperation{}, fmt.Errorf("%w: request does not bind latest scope consent", ErrConsent)
			}
		}
		if loaded.consent.FrequencyMaximum < frequencyLimit {
			frequencyLimit = loaded.consent.FrequencyMaximum
		}
		if loaded.consent.FrequencyWindow > frequencyWindow {
			frequencyWindow = loaded.consent.FrequencyWindow
		}
		consent = &loaded
	} else if request.ConsentID != "" {
		return authorizedOperation{}, fmt.Errorf("%w: consent-free operation cannot borrow unrelated consent", ErrDenied)
	}
	if err := validateExternalEnvelope(connection.connection, policy, request, envelope.External); err != nil {
		return authorizedOperation{}, err
	}
	requestHash, err := CanonicalHash(request)
	if err != nil {
		return authorizedOperation{}, err
	}
	return authorizedOperation{
		connection: connection, customer: customer, consent: consent,
		policy: policy, envelope: envelope, requestHash: requestHash,
		recipientHash: request.DestinationHash, frequencyLimit: frequencyLimit,
		frequencyWindow: frequencyWindow,
	}, nil
}

func validateRequestGovernance(
	connection Connection,
	customer CustomerScope,
	policy OperationPolicy,
	request Request,
) error {
	governance := connection.Governance
	if request.AccountID != connection.AccountID || request.IdentityID != connection.IdentityID ||
		!contains(governance.Channels, request.Channel) ||
		!contains(governance.Purposes, request.Purpose) ||
		!contains(governance.Jurisdictions, request.Jurisdiction) ||
		!contains(governance.PrivacyPolicyRefs, request.PrivacyPolicyRef) ||
		!contains(governance.LegalPolicyRefs, request.LegalPolicyRef) ||
		!contains(governance.SecurityPolicyRefs, request.SecurityPolicyRef) ||
		!containsClassification(governance.DataClassifications, request.DataClassification) ||
		!contains(customer.Channels, request.Channel) ||
		!contains(customer.Purposes, request.Purpose) ||
		!contains(customer.Jurisdictions, request.Jurisdiction) ||
		!containsClassification(customer.DataClassifications, request.DataClassification) {
		return fmt.Errorf("%w: request exceeds account, purpose, privacy, or jurisdiction scope", ErrDenied)
	}
	if request.Audience != "" &&
		(!contains(governance.Audiences, request.Audience) || !contains(customer.Audiences, request.Audience)) {
		return fmt.Errorf("%w: audience is outside customer scope", ErrDenied)
	}
	if (policy.Family == FamilyEmail || policy.Family == FamilyConsentedOutbound ||
		policy.Family == FamilySocialDistribution) &&
		(request.Audience == "" || len(request.ClaimRefs) == 0) {
		return fmt.Errorf("%w: outbound communication lacks exact audience or claim authority", ErrDenied)
	}
	for _, claim := range request.ClaimRefs {
		if !contains(governance.ClaimRefs, claim) {
			return fmt.Errorf("%w: claim is outside approved claim set", ErrDenied)
		}
	}
	if policy.Family.customerFacing() &&
		(request.BrandPolicyRef == "" || !contains(governance.BrandPolicyRefs, request.BrandPolicyRef)) {
		return fmt.Errorf("%w: customer communication lacks exact brand authority", ErrDenied)
	}
	if policy.EffectClass == skills.EffectReversible &&
		(request.CompensationPolicyRef == "" ||
			!contains(governance.CompensationPolicyRefs, request.CompensationPolicyRef)) {
		return fmt.Errorf("%w: reversible customer effect lacks compensation authority", ErrDenied)
	}
	if policy.EffectClass != skills.EffectReversible && request.CompensationPolicyRef != "" {
		return fmt.Errorf("%w: operation cannot borrow compensation authority", ErrDenied)
	}
	if policy.Family == FamilyContractTransmission {
		if request.ContractRef == "" || request.ContractHash.Validate() != nil ||
			!contains(governance.ContractTemplateRefs, request.ContractRef) ||
			!contains(customer.ContractRefs, request.ContractRef) {
			return fmt.Errorf("%w: contract transmission lacks exact contract authority", ErrDenied)
		}
	} else if request.ContractRef != "" {
		return fmt.Errorf("%w: non-contract operation cannot transmit a contract", ErrDenied)
	}
	if policy.Family == FamilySupport {
		if request.SupportQueue == "" || !contains(governance.SupportQueues, request.SupportQueue) ||
			!contains(customer.SupportQueues, request.SupportQueue) {
			return fmt.Errorf("%w: support request lacks exact queue authority", ErrDenied)
		}
	} else if request.SupportQueue != "" {
		return fmt.Errorf("%w: non-support operation cannot use a support queue", ErrDenied)
	}
	if policy.CompensatesOperation != "" {
		if token("compensates key", request.CompensatesKey) != nil {
			return fmt.Errorf("%w: compensation target is invalid", ErrDenied)
		}
	} else if request.CompensatesKey != "" {
		return fmt.Errorf("%w: operation cannot compensate another effect", ErrDenied)
	}
	return nil
}

func validateExternalEnvelope(
	connection Connection,
	policy OperationPolicy,
	request Request,
	envelope external.Envelope,
) error {
	bound := envelope.Request
	if envelope.ConnectionID != connection.ExternalConnectionID ||
		envelope.ConnectionVersion != connection.ExternalConnectionVersion ||
		envelope.ConnectionHash != connection.ExternalConnectionHash ||
		bound.Action != policy.ExternalAction || bound.Navigation != external.NavigationDirect ||
		bound.Environment != "" || bound.TargetURL != request.TargetURL ||
		bound.AccountID != request.AccountID || bound.IdentityID != request.IdentityID ||
		bound.DataClassification != request.DataClassification || bound.Channel != request.Channel ||
		bound.Audience != request.Audience || bound.Jurisdiction != request.Jurisdiction ||
		bound.ConsentRef != request.ConsentID || bound.Recipient != request.Destination ||
		bound.Counterparty != request.CustomerID || bound.BrandPolicyRef != request.BrandPolicyRef ||
		bound.LegalPolicyRef != request.LegalPolicyRef ||
		bound.SecurityPolicyRef != request.SecurityPolicyRef ||
		bound.RollbackPolicyRef != request.CompensationPolicyRef ||
		bound.ExpectedResourceVersion != request.ExpectedResourceVersion ||
		bound.CompensatesKey != request.CompensatesKey || bound.OutputBytes != request.OutputBytes ||
		bound.Download != request.Download || bound.IssuedAt != request.IssuedAt ||
		bound.ExpiresAt != request.ExpiresAt || !sameStrings(bound.Claims, request.ClaimRefs) {
		return fmt.Errorf("%w: external envelope diverges from customer authority", ErrDenied)
	}
	if request.Upload == nil != (bound.Upload == nil) {
		return fmt.Errorf("%w: external transfer binding mismatch", ErrDenied)
	}
	if request.Upload != nil {
		left, err := json.Marshal(request.Upload)
		if err != nil {
			return err
		}
		right, err := json.Marshal(bound.Upload)
		if err != nil || !bytes.Equal(left, right) {
			return fmt.Errorf("%w: external transfer binding mismatch", ErrDenied)
		}
	}
	expected := ProviderCommand{
		SchemaVersion: SchemaVersion, Family: request.Family, Action: request.Action,
		AccountID: request.AccountID, IdentityID: request.IdentityID,
		CustomerID: request.CustomerID, RecipientRef: request.RecipientRef,
		Destination: request.Destination, DestinationHash: request.DestinationHash,
		Channel: request.Channel, Audience: request.Audience, Purpose: request.Purpose,
		Jurisdiction: request.Jurisdiction, DataClassification: request.DataClassification,
		ConsentID: request.ConsentID, ClaimRefs: append([]string(nil), request.ClaimRefs...),
		BrandPolicyRef: request.BrandPolicyRef, PrivacyPolicyRef: request.PrivacyPolicyRef,
		LegalPolicyRef: request.LegalPolicyRef, ContractRef: request.ContractRef,
		ContractHash: request.ContractHash, SupportQueue: request.SupportQueue,
		Payload: append([]byte(nil), request.Payload...),
	}
	expectedBody, err := external.EncodeBody(expected)
	if err != nil || !bytes.Equal(expectedBody, bound.Body) {
		return fmt.Errorf("%w: provider command diverges from customer request", ErrDenied)
	}
	return nil
}

func sameStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}
