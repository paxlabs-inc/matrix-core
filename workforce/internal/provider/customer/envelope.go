package customer

import (
	"fmt"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/provider/external"
)

func BuildEnvelope(
	connectionView ConnectionView,
	customerView CustomerView,
	consentView *ConsentView,
	grant lease.Grant,
	request Request,
) (Envelope, []byte, error) {
	connection := connectionView.Connection
	customer := customerView.Customer
	if err := connection.Validate(); err != nil || connectionView.Hash.Validate() != nil {
		return Envelope{}, nil, fmt.Errorf("customer adapter: connection view is invalid")
	}
	expectedConnectionHash, err := CanonicalHash(connection)
	if err != nil || expectedConnectionHash != connectionView.Hash {
		return Envelope{}, nil, fmt.Errorf("customer adapter: connection view hash mismatch")
	}
	if err := customer.Validate(); err != nil || customerView.Hash.Validate() != nil {
		return Envelope{}, nil, fmt.Errorf("customer adapter: customer view is invalid")
	}
	expectedCustomerHash, err := CanonicalHash(customer)
	if err != nil || expectedCustomerHash != customerView.Hash {
		return Envelope{}, nil, fmt.Errorf("customer adapter: customer view hash mismatch")
	}
	policy, found := connection.Operation(request.Operation)
	if !found {
		return Envelope{}, nil, fmt.Errorf("%w: operation is not allowlisted", ErrDenied)
	}
	request.Family = connection.Family
	request.Action = policy.Action
	request.AccountID = connection.AccountID
	request.IdentityID = connection.IdentityID
	request.CustomerID = customer.ID
	request.CustomerVersion = customer.Version
	request.CustomerHash = customerView.Hash
	request.RecipientRef = customer.RecipientRef
	request.DestinationHash = DestinationHash(request.Destination)
	if consentView == nil {
		request.ConsentID = ""
		request.ConsentVersion = 0
		request.ConsentHash = contracts.ContentHash{}
	} else {
		consent := consentView.Consent
		if err := consent.Validate(); err != nil || consentView.Hash.Validate() != nil {
			return Envelope{}, nil, fmt.Errorf("customer adapter: consent view is invalid")
		}
		expectedConsentHash, err := CanonicalHash(consent)
		if err != nil || expectedConsentHash != consentView.Hash {
			return Envelope{}, nil, fmt.Errorf("customer adapter: consent view hash mismatch")
		}
		request.ConsentID = consent.ID
		request.ConsentVersion = consent.Version
		request.ConsentHash = consentView.Hash
	}
	if err := request.Validate(); err != nil {
		return Envelope{}, nil, err
	}
	command := ProviderCommand{
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
	body, err := external.EncodeBody(command)
	if err != nil {
		return Envelope{}, nil, err
	}
	externalEnvelope := external.Envelope{
		SchemaVersion:     SchemaVersion,
		Grant:             grant,
		ConnectionID:      connection.ExternalConnectionID,
		ConnectionVersion: connection.ExternalConnectionVersion,
		ConnectionHash:    connection.ExternalConnectionHash,
		Request: external.BoundRequest{
			Action: policy.ExternalAction, Navigation: external.NavigationDirect,
			TargetURL: request.TargetURL, AccountID: request.AccountID,
			IdentityID: request.IdentityID, DataClassification: request.DataClassification,
			Channel: request.Channel, Audience: request.Audience,
			Claims: append([]string(nil), request.ClaimRefs...), Jurisdiction: request.Jurisdiction,
			ConsentRef: request.ConsentID, Recipient: request.Destination,
			Counterparty: request.CustomerID, BrandPolicyRef: request.BrandPolicyRef,
			LegalPolicyRef: request.LegalPolicyRef, SecurityPolicyRef: request.SecurityPolicyRef,
			RollbackPolicyRef:       request.CompensationPolicyRef,
			ExpectedResourceVersion: request.ExpectedResourceVersion,
			CompensatesKey:          request.CompensatesKey, OutputBytes: request.OutputBytes,
			Body: body, Upload: request.Upload, Download: request.Download,
			IssuedAt: request.IssuedAt, ExpiresAt: request.ExpiresAt,
		},
	}
	value := Envelope{
		SchemaVersion: SchemaVersion, ConnectionID: connection.ID,
		ConnectionVersion: connection.Version, ConnectionHash: connectionView.Hash,
		Grant: grant, Request: request, External: externalEnvelope,
	}
	if err := value.Validate(); err != nil {
		return Envelope{}, nil, err
	}
	encoded, err := contracts.EncodeCanonical(&value)
	if err != nil {
		return Envelope{}, nil, err
	}
	return value, encoded, nil
}
