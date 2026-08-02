package financial

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"math/big"
	"net/url"
	"sort"
	"strings"
	"time"

	"matrix/workforce/internal/contracts"
	"matrix/workforce/internal/lease"
	"matrix/workforce/internal/provider/external"
	"matrix/workforce/internal/skills"
)

const SchemaVersion = contracts.SchemaVersionV1

var (
	ErrDenied          = errors.New("financial adapter: operation denied")
	ErrConflict        = errors.New("financial adapter: durable identity conflict")
	ErrAmbiguous       = errors.New("financial adapter: outcome requires authoritative reconciliation")
	ErrUnavailable     = errors.New("financial adapter: provider unavailable")
	ErrIntegrity       = errors.New("financial adapter: sealed record integrity failure")
	ErrLimit           = errors.New("financial adapter: capital or risk limit exceeded")
	ErrFrozen          = errors.New("financial adapter: conflicting financial scope is frozen")
	ErrStaleValuation  = errors.New("financial adapter: valuation is stale or not current")
	ErrStaleRisk       = errors.New("financial adapter: risk state is stale or not current")
	ErrReserved        = errors.New("financial adapter: founder-reserved action lacks exact authority")
	ErrCircuitOpen     = errors.New("financial adapter: operation circuit is open")
	ErrCapacity        = errors.New("financial adapter: scoped capacity exhausted")
	ErrOutOfBandChange = errors.New("financial adapter: provider resource changed out of band")
)

type Family string

const (
	FamilyPaxeer         Family = "paxeer"
	FamilyLayerX         Family = "layerx"
	FamilyBilling        Family = "billing"
	FamilyInvoicing      Family = "invoicing"
	FamilyPayment        Family = "payment"
	FamilyCollection     Family = "collection"
	FamilyTreasury       Family = "treasury"
	FamilyTrading        Family = "trading"
	FamilySettlement     Family = "settlement"
	FamilyTransfer       Family = "transfer"
	FamilyReconciliation Family = "reconciliation"
)

func (value Family) Valid() bool {
	switch value {
	case FamilyPaxeer, FamilyLayerX, FamilyBilling, FamilyInvoicing,
		FamilyPayment, FamilyCollection, FamilyTreasury, FamilyTrading,
		FamilySettlement, FamilyTransfer, FamilyReconciliation:
		return true
	default:
		return false
	}
}

type Action string

const (
	ActionIssueInvoice    Action = "issue_invoice"
	ActionVoidInvoice     Action = "void_invoice"
	ActionCharge          Action = "charge"
	ActionCollect         Action = "collect"
	ActionRefund          Action = "refund"
	ActionTransfer        Action = "transfer"
	ActionSettle          Action = "settle"
	ActionWithdraw        Action = "withdraw"
	ActionCustodyTransfer Action = "custody_transfer"
	ActionPlaceTrade      Action = "place_trade"
	ActionCancelTrade     Action = "cancel_trade"
	ActionLeverage        Action = "leverage"
	ActionBorrow          Action = "borrow"
	ActionRepayDebt       Action = "repay_debt"
	ActionReconcile       Action = "reconcile"
	ActionObserveBalance  Action = "observe_balance"
)

func (value Action) Valid() bool {
	switch value {
	case ActionIssueInvoice, ActionVoidInvoice, ActionCharge, ActionCollect,
		ActionRefund, ActionTransfer, ActionSettle, ActionWithdraw,
		ActionCustodyTransfer, ActionPlaceTrade, ActionCancelTrade,
		ActionLeverage, ActionBorrow, ActionRepayDebt, ActionReconcile,
		ActionObserveBalance:
		return true
	default:
		return false
	}
}

func (value Action) Mutates() bool {
	return value.Valid() && value != ActionReconcile && value != ActionObserveBalance
}

func (value Action) ConstitutionReserved() bool {
	switch value {
	case ActionWithdraw, ActionCustodyTransfer, ActionLeverage, ActionBorrow:
		return true
	default:
		return false
	}
}

type RiskClass string

const (
	RiskNeutral  RiskClass = "neutral"
	RiskInflow   RiskClass = "inflow"
	RiskOutflow  RiskClass = "outflow"
	RiskExposure RiskClass = "exposure"
)

func (value RiskClass) Valid() bool {
	return value == RiskNeutral || value == RiskInflow || value == RiskOutflow || value == RiskExposure
}

type FinancialState string

const (
	StateAccepted   FinancialState = "accepted"
	StatePending    FinancialState = "pending"
	StateSubmitted  FinancialState = "submitted"
	StateAuthorized FinancialState = "authorized"
	StatePosted     FinancialState = "posted"
	StateSettled    FinancialState = "settled"
	StateCollected  FinancialState = "collected"
	StateReconciled FinancialState = "reconciled"
	StateRejected   FinancialState = "rejected"
	StateReversed   FinancialState = "reversed"
	StateFailed     FinancialState = "failed"
	StateUnknown    FinancialState = "unknown"
)

func (value FinancialState) Valid() bool {
	switch value {
	case StateAccepted, StatePending, StateSubmitted, StateAuthorized,
		StatePosted, StateSettled, StateCollected, StateReconciled,
		StateRejected, StateReversed, StateFailed, StateUnknown:
		return true
	default:
		return false
	}
}

func (value FinancialState) Inconclusive() bool {
	switch value {
	case StateAccepted, StatePending, StateSubmitted, StateAuthorized, StateUnknown:
		return true
	default:
		return false
	}
}

func (value FinancialState) DefinitiveFailure() bool {
	return value == StateRejected || value == StateFailed
}

type ScopeKind string

const (
	ScopeAsset        ScopeKind = "asset"
	ScopeVenue        ScopeKind = "venue"
	ScopeCounterparty ScopeKind = "counterparty"
	ScopeInitiative   ScopeKind = "initiative"
)

func (value ScopeKind) Valid() bool {
	return value == ScopeAsset || value == ScopeVenue || value == ScopeCounterparty || value == ScopeInitiative
}

type CapitalDirection string

const (
	DirectionNeutral CapitalDirection = "neutral"
	DirectionInflow  CapitalDirection = "inflow"
	DirectionOutflow CapitalDirection = "outflow"
)

func (value CapitalDirection) Valid() bool {
	return value == DirectionNeutral || value == DirectionInflow || value == DirectionOutflow
}

type AuthorityBinding struct {
	MissionVersion          uint64                `json:"mission_version"`
	ConstitutionVersion     uint64                `json:"constitution_version"`
	OrganizationVersion     uint64                `json:"organization_version"`
	OperatingScopeID        string                `json:"operating_scope_id"`
	OperatingScopeHash      contracts.ContentHash `json:"operating_scope_hash"`
	PolicyRefs              []contracts.PolicyRef `json:"policy_refs"`
	ValuationIssuerKeyIDs   []string              `json:"valuation_issuer_key_ids"`
	RiskStateIssuerKeyIDs   []string              `json:"risk_state_issuer_key_ids"`
	AccountingMethodologyID string                `json:"accounting_methodology_id"`
}

func (value AuthorityBinding) Validate() error {
	if value.MissionVersion == 0 || value.ConstitutionVersion == 0 ||
		value.OrganizationVersion == 0 || token("operating scope id", value.OperatingScopeID) != nil ||
		value.OperatingScopeHash.Validate() != nil || len(value.PolicyRefs) == 0 ||
		len(value.PolicyRefs) > 64 || !sortedTokens(value.ValuationIssuerKeyIDs, 32) ||
		!sortedTokens(value.RiskStateIssuerKeyIDs, 32) ||
		token("accounting methodology id", value.AccountingMethodologyID) != nil {
		return fmt.Errorf("financial adapter: authority binding is incomplete")
	}
	seen := make(map[contracts.PolicyID]bool, len(value.PolicyRefs))
	for _, reference := range value.PolicyRefs {
		if reference.Validate() != nil || seen[reference.ID] {
			return fmt.Errorf("financial adapter: authority policy binding is invalid")
		}
		seen[reference.ID] = true
	}
	return nil
}

type VenueScope struct {
	Venue             string `json:"venue"`
	Rail              string `json:"rail"`
	ActivationRef     string `json:"activation_ref"`
	FounderReserved   bool   `json:"founder_reserved"`
	AllowsTrading     bool   `json:"allows_trading"`
	AllowsLeverage    bool   `json:"allows_leverage"`
	AllowsDebt        bool   `json:"allows_debt"`
	AllowsWithdrawal  bool   `json:"allows_withdrawal"`
	AllowsCustodyMove bool   `json:"allows_custody_move"`
}

func (value VenueScope) Validate() error {
	if token("venue", value.Venue) != nil || token("rail", value.Rail) != nil ||
		bounded("venue activation ref", value.ActivationRef) != nil {
		return fmt.Errorf("financial adapter: venue scope is invalid")
	}
	if (value.AllowsLeverage || value.AllowsDebt || value.AllowsWithdrawal || value.AllowsCustodyMove) &&
		!value.FounderReserved {
		return fmt.Errorf("financial adapter: high-risk venue capabilities must remain founder-reserved")
	}
	return nil
}

type GovernancePolicy struct {
	Initiatives              []string                `json:"initiatives"`
	Purposes                 []string                `json:"purposes"`
	Assets                   []string                `json:"assets"`
	Instruments              []string                `json:"instruments"`
	VenueScopes              []VenueScope            `json:"venue_scopes"`
	Counterparties           []string                `json:"counterparties"`
	Jurisdictions            []string                `json:"jurisdictions"`
	DestinationHashes        []contracts.ContentHash `json:"destination_hashes"`
	LegalPolicyRefs          []string                `json:"legal_policy_refs"`
	SecurityPolicyRefs       []string                `json:"security_policy_refs"`
	ReconciliationPolicyRefs []string                `json:"reconciliation_policy_refs"`
}

func (value GovernancePolicy) Validate() error {
	if !sortedTokens(value.Initiatives, 128) || !sortedValues(value.Purposes, 128, 1024) ||
		!sortedTokens(value.Assets, 128) || !sortedTokens(value.Instruments, 128) ||
		len(value.VenueScopes) == 0 || len(value.VenueScopes) > 64 ||
		!sortedValues(value.Counterparties, 128, 1024) ||
		!sortedValues(value.Jurisdictions, 64, 256) ||
		len(value.DestinationHashes) == 0 || len(value.DestinationHashes) > 256 ||
		!sortedValues(value.LegalPolicyRefs, 64, 1024) ||
		!sortedValues(value.SecurityPolicyRefs, 64, 1024) ||
		!sortedValues(value.ReconciliationPolicyRefs, 64, 1024) {
		return fmt.Errorf("financial adapter: governance allowlists are invalid")
	}
	for index, scope := range value.VenueScopes {
		if scope.Validate() != nil || index > 0 &&
			(value.VenueScopes[index-1].Venue > scope.Venue ||
				value.VenueScopes[index-1].Venue == scope.Venue && value.VenueScopes[index-1].Rail >= scope.Rail) {
			return fmt.Errorf("financial adapter: venue scopes must be unique and ordered")
		}
	}
	previous := ""
	for _, hash := range value.DestinationHashes {
		if hash.Validate() != nil || previous >= hash.Digest && previous != "" {
			return fmt.Errorf("financial adapter: destination hashes must be unique and ordered")
		}
		previous = hash.Digest
	}
	return nil
}

func (value GovernancePolicy) Venue(venue, rail string) (VenueScope, bool) {
	for _, scope := range value.VenueScopes {
		if scope.Venue == venue && scope.Rail == rail {
			return scope, true
		}
	}
	return VenueScope{}, false
}

type ScopeLimit struct {
	Kind                  ScopeKind `json:"kind"`
	Key                   string    `json:"key"`
	PerEffectMicrounits   uint64    `json:"per_effect_microunits"`
	RollingMicrounits     uint64    `json:"rolling_microunits"`
	MaxExposureMicrounits uint64    `json:"max_exposure_microunits"`
	MaxVelocityCount      uint32    `json:"max_velocity_count"`
}

func (value ScopeLimit) Validate() error {
	if !value.Kind.Valid() || bounded("scope limit key", value.Key) != nil ||
		!money(value.PerEffectMicrounits) || !money(value.RollingMicrounits) ||
		!money(value.MaxExposureMicrounits) || value.MaxVelocityCount == 0 ||
		value.MaxVelocityCount > 100000 || value.PerEffectMicrounits > value.RollingMicrounits ||
		value.PerEffectMicrounits > value.MaxExposureMicrounits {
		return fmt.Errorf("financial adapter: scoped limit is invalid")
	}
	return nil
}

type CapitalPolicy struct {
	BaseCurrency               string        `json:"base_currency"`
	PerEffectMicrounits        uint64        `json:"per_effect_microunits"`
	DailyMicrounits            uint64        `json:"daily_microunits"`
	RollingMicrounits          uint64        `json:"rolling_microunits"`
	AggregateCapitalMicrounits uint64        `json:"aggregate_capital_microunits"`
	MaxGrossExposureMicrounits uint64        `json:"max_gross_exposure_microunits"`
	MaxDrawdownMicrounits      uint64        `json:"max_drawdown_microunits"`
	MinLiquidityMicrounits     uint64        `json:"min_liquidity_microunits"`
	MinRunwayMicrounits        uint64        `json:"min_runway_microunits"`
	MaxFeeMicrounits           uint64        `json:"max_fee_microunits"`
	MaxToleranceBPS            uint16        `json:"max_tolerance_bps"`
	MaxConcentrationBPS        uint16        `json:"max_concentration_bps"`
	MaxVelocityCount           uint32        `json:"max_velocity_count"`
	RollingWindow              time.Duration `json:"rolling_window"`
	MaxValuationAge            time.Duration `json:"max_valuation_age"`
	MaxRiskStateAge            time.Duration `json:"max_risk_state_age"`
	MaxConcurrent              uint16        `json:"max_concurrent"`
	MaxReconciliationAttempts  uint16        `json:"max_reconciliation_attempts"`
	FailureThreshold           uint16        `json:"failure_threshold"`
	CircuitWindow              time.Duration `json:"circuit_window"`
	CircuitOpenDuration        time.Duration `json:"circuit_open_duration"`
	OutputBytes                uint64        `json:"output_bytes"`
	Scoped                     []ScopeLimit  `json:"scoped"`
}

func (value CapitalPolicy) Validate() error {
	values := []uint64{
		value.PerEffectMicrounits, value.DailyMicrounits, value.RollingMicrounits,
		value.AggregateCapitalMicrounits, value.MaxGrossExposureMicrounits,
		value.MaxDrawdownMicrounits, value.MinLiquidityMicrounits,
		value.MinRunwayMicrounits, value.MaxFeeMicrounits,
	}
	if token("base currency", value.BaseCurrency) != nil {
		return fmt.Errorf("financial adapter: base currency is invalid")
	}
	for _, amount := range values {
		if !money(amount) {
			return fmt.Errorf("financial adapter: capital amount is outside exact integer limits")
		}
	}
	if value.PerEffectMicrounits > value.DailyMicrounits ||
		value.DailyMicrounits > value.RollingMicrounits ||
		value.RollingMicrounits > value.AggregateCapitalMicrounits ||
		value.MaxGrossExposureMicrounits > value.AggregateCapitalMicrounits ||
		value.MaxDrawdownMicrounits > value.AggregateCapitalMicrounits ||
		value.MinLiquidityMicrounits > value.AggregateCapitalMicrounits ||
		value.MinRunwayMicrounits > value.AggregateCapitalMicrounits ||
		value.MaxFeeMicrounits > value.PerEffectMicrounits ||
		value.MaxToleranceBPS > 10000 || value.MaxConcentrationBPS == 0 ||
		value.MaxConcentrationBPS > 10000 || value.MaxVelocityCount == 0 ||
		value.MaxVelocityCount > 100000 || value.RollingWindow <= 0 ||
		value.RollingWindow > 30*24*time.Hour || value.MaxValuationAge <= 0 ||
		value.MaxValuationAge > 7*24*time.Hour || value.MaxRiskStateAge <= 0 ||
		value.MaxRiskStateAge > 7*24*time.Hour || value.MaxConcurrent == 0 ||
		value.MaxConcurrent > 32 || value.MaxReconciliationAttempts == 0 ||
		value.MaxReconciliationAttempts > 32 || value.FailureThreshold == 0 ||
		value.FailureThreshold > 100 || value.CircuitWindow <= 0 ||
		value.CircuitWindow > 24*time.Hour || value.CircuitOpenDuration <= 0 ||
		value.CircuitOpenDuration > 24*time.Hour || value.OutputBytes == 0 ||
		value.OutputBytes > 1<<20 || len(value.Scoped) == 0 || len(value.Scoped) > 512 {
		return fmt.Errorf("financial adapter: capital policy is outside hard limits")
	}
	for index, scoped := range value.Scoped {
		if scoped.Validate() != nil || index > 0 &&
			(string(value.Scoped[index-1].Kind) > string(scoped.Kind) ||
				value.Scoped[index-1].Kind == scoped.Kind && value.Scoped[index-1].Key >= scoped.Key) {
			return fmt.Errorf("financial adapter: scoped limits must be unique and ordered")
		}
	}
	return nil
}

func (value CapitalPolicy) Limit(kind ScopeKind, key string) (ScopeLimit, bool) {
	for _, limit := range value.Scoped {
		if limit.Kind == kind && limit.Key == key {
			return limit, true
		}
	}
	return ScopeLimit{}, false
}

type OperationPolicy struct {
	Name                       string               `json:"name"`
	ExternalOperation          string               `json:"external_operation"`
	Family                     Family               `json:"family"`
	Action                     Action               `json:"action"`
	ExternalAction             external.ActionClass `json:"external_action"`
	EffectClass                skills.EffectClass   `json:"effect_class"`
	RiskClass                  RiskClass            `json:"risk_class"`
	CapitalDirection           CapitalDirection     `json:"capital_direction"`
	FounderReserved            bool                 `json:"founder_reserved"`
	AuthoritativeSuccessStates []FinancialState     `json:"authoritative_success_states"`
}

func (value OperationPolicy) Validate() error {
	if token("operation", value.Name) != nil || token("external operation", value.ExternalOperation) != nil ||
		!value.Family.Valid() || !value.Action.Valid() || !value.ExternalAction.Valid() ||
		!value.EffectClass.Valid() || !value.RiskClass.Valid() || !value.CapitalDirection.Valid() ||
		value.Action.Mutates() != value.ExternalAction.Mutates() {
		return fmt.Errorf("financial adapter: operation policy is invalid")
	}
	if value.Action.Mutates() {
		if value.EffectClass != skills.EffectIrreversible || len(value.AuthoritativeSuccessStates) == 0 {
			return fmt.Errorf("financial adapter: mutation requires irreversible gateway approval and terminal reconciliation states")
		}
	} else if value.EffectClass != skills.EffectRead || value.RiskClass != RiskNeutral ||
		value.CapitalDirection != DirectionNeutral {
		return fmt.Errorf("financial adapter: observation operation must be read-only and capital-neutral")
	}
	if value.RiskClass == RiskInflow && value.CapitalDirection != DirectionInflow ||
		value.RiskClass == RiskOutflow && value.CapitalDirection != DirectionOutflow ||
		value.RiskClass == RiskNeutral && value.CapitalDirection != DirectionNeutral {
		return fmt.Errorf("financial adapter: risk class and capital direction disagree")
	}
	if value.Action.ConstitutionReserved() && !value.FounderReserved {
		return fmt.Errorf("financial adapter: constitution-reserved action cannot be delegated")
	}
	if len(value.AuthoritativeSuccessStates) > 6 {
		return fmt.Errorf("financial adapter: terminal state allowlist is too broad")
	}
	previous := FinancialState("")
	for _, state := range value.AuthoritativeSuccessStates {
		if !state.Valid() || state.Inconclusive() || state.DefinitiveFailure() ||
			previous != "" && string(previous) >= string(state) {
			return fmt.Errorf("financial adapter: terminal state allowlist is invalid")
		}
		previous = state
	}
	return nil
}

func (value OperationPolicy) Succeeds(state FinancialState) bool {
	for _, allowed := range value.AuthoritativeSuccessStates {
		if state == allowed {
			return true
		}
	}
	return false
}

type ProviderContractKind string

const (
	ContractPaxeerEVM      ProviderContractKind = "paxeer_evm_json_rpc"
	ContractLayerXAccount  ProviderContractKind = "layerx_account_v1"
	ContractBillingLedger  ProviderContractKind = "billing_ledger_v1"
	ContractTreasuryLedger ProviderContractKind = "treasury_ledger_v1"
)

func (value ProviderContractKind) Valid() bool {
	return value == ContractPaxeerEVM || value == ContractLayerXAccount ||
		value == ContractBillingLedger || value == ContractTreasuryLedger
}

type ProviderContract struct {
	Kind                  ProviderContractKind `json:"kind"`
	NetworkID             string               `json:"network_id"`
	ChainID               uint64               `json:"chain_id"`
	SettlementContract    string               `json:"settlement_contract"`
	ContractVersion       string               `json:"contract_version"`
	RequiredConfirmations uint32               `json:"required_confirmations"`
}

func (value ProviderContract) Validate(family Family) error {
	if !value.Kind.Valid() || token("network id", value.NetworkID) != nil ||
		token("contract version", value.ContractVersion) != nil || value.ChainID > math.MaxInt64 {
		return fmt.Errorf("financial adapter: provider contract is invalid")
	}
	switch value.Kind {
	case ContractPaxeerEVM:
		if family != FamilyPaxeer || value.ChainID == 0 || !evmAddress(value.SettlementContract) ||
			value.RequiredConfirmations == 0 || value.RequiredConfirmations > 100000 {
			return fmt.Errorf("financial adapter: Paxeer contract binding is invalid")
		}
	case ContractLayerXAccount:
		if family != FamilyLayerX || value.ChainID == 0 || !evmAddress(value.SettlementContract) ||
			value.RequiredConfirmations == 0 || value.RequiredConfirmations > 100000 {
			return fmt.Errorf("financial adapter: LayerX settlement binding is invalid")
		}
	case ContractBillingLedger:
		if family != FamilyBilling && family != FamilyInvoicing && family != FamilyPayment &&
			family != FamilyCollection || value.ChainID != 0 || value.SettlementContract != "" ||
			value.RequiredConfirmations != 0 {
			return fmt.Errorf("financial adapter: billing ledger contract binding is invalid")
		}
	case ContractTreasuryLedger:
		if family != FamilyTreasury && family != FamilyTrading && family != FamilySettlement &&
			family != FamilyTransfer && family != FamilyReconciliation || value.ChainID != 0 ||
			value.SettlementContract != "" || value.RequiredConfirmations != 0 {
			return fmt.Errorf("financial adapter: treasury ledger contract binding is invalid")
		}
	}
	return nil
}

type Connection struct {
	SchemaVersion             string                   `json:"schema_version"`
	ID                        string                   `json:"id"`
	Version                   uint64                   `json:"version"`
	OrganizationID            contracts.OrganizationID `json:"organization_id"`
	AdapterName               string                   `json:"adapter_name"`
	ExternalAdapterName       string                   `json:"external_adapter_name"`
	ExternalConnectionID      string                   `json:"external_connection_id"`
	ExternalConnectionVersion uint64                   `json:"external_connection_version"`
	ExternalConnectionHash    contracts.ContentHash    `json:"external_connection_hash"`
	ProviderTargetURL         string                   `json:"provider_target_url"`
	ProviderContract          ProviderContract         `json:"provider_contract"`
	Family                    Family                   `json:"family"`
	AccountID                 string                   `json:"account_id"`
	IdentityID                string                   `json:"identity_id"`
	Authority                 AuthorityBinding         `json:"authority"`
	Governance                GovernancePolicy         `json:"governance"`
	Capital                   CapitalPolicy            `json:"capital"`
	Operations                []OperationPolicy        `json:"operations"`
	EffectiveAt               time.Time                `json:"effective_at"`
	ExpiresAt                 time.Time                `json:"expires_at"`
	Signature                 contracts.Signature      `json:"signature"`
}

func (value Connection) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("connection id", value.ID) != nil ||
		value.Version == 0 || token("organization id", string(value.OrganizationID)) != nil ||
		token("adapter name", value.AdapterName) != nil ||
		token("external adapter name", value.ExternalAdapterName) != nil ||
		token("external connection id", value.ExternalConnectionID) != nil ||
		value.ExternalConnectionVersion == 0 || value.ExternalConnectionHash.Validate() != nil ||
		!validProviderURL(value.ProviderTargetURL) ||
		!value.Family.Valid() || value.ProviderContract.Validate(value.Family) != nil ||
		bounded("account id", value.AccountID) != nil ||
		bounded("identity id", value.IdentityID) != nil || value.Authority.Validate() != nil ||
		value.Governance.Validate() != nil || value.Capital.Validate() != nil ||
		len(value.Operations) == 0 || len(value.Operations) > 64 ||
		!validUTC(value.EffectiveAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.EffectiveAt) ||
		value.ExpiresAt.Sub(value.EffectiveAt) > 366*24*time.Hour ||
		value.Signature.Validate() != nil {
		return fmt.Errorf("financial adapter: connection is invalid")
	}
	seen := make(map[string]bool, len(value.Operations))
	for _, operation := range value.Operations {
		if operation.Validate() != nil || operation.Family != value.Family || seen[operation.Name] {
			return fmt.Errorf("financial adapter: connection operation is invalid or duplicated")
		}
		seen[operation.Name] = true
	}
	for _, initiative := range value.Governance.Initiatives {
		if _, ok := value.Capital.Limit(ScopeInitiative, initiative); !ok {
			return fmt.Errorf("financial adapter: initiative lacks exact scoped limit")
		}
	}
	for _, asset := range value.Governance.Assets {
		if _, ok := value.Capital.Limit(ScopeAsset, asset); !ok {
			return fmt.Errorf("financial adapter: asset lacks exact scoped limit")
		}
	}
	for _, venue := range value.Governance.VenueScopes {
		if _, ok := value.Capital.Limit(ScopeVenue, venue.Venue+"/"+venue.Rail); !ok {
			return fmt.Errorf("financial adapter: venue and rail lack exact scoped limit")
		}
	}
	for _, counterparty := range value.Governance.Counterparties {
		if _, ok := value.Capital.Limit(ScopeCounterparty, counterparty); !ok {
			return fmt.Errorf("financial adapter: counterparty lacks exact scoped limit")
		}
	}
	return nil
}

func (value Connection) Operation(name string) (OperationPolicy, bool) {
	for _, operation := range value.Operations {
		if operation.Name == name {
			return operation, true
		}
	}
	return OperationPolicy{}, false
}

type ConnectionRevocation struct {
	SchemaVersion  string                   `json:"schema_version"`
	ID             string                   `json:"id"`
	OrganizationID contracts.OrganizationID `json:"organization_id"`
	ConnectionID   string                   `json:"connection_id"`
	Version        uint64                   `json:"version"`
	ReasonCode     string                   `json:"reason_code"`
	RevokedAt      time.Time                `json:"revoked_at"`
	Signature      contracts.Signature      `json:"signature"`
}

func (value ConnectionRevocation) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("revocation id", value.ID) != nil ||
		token("organization id", string(value.OrganizationID)) != nil ||
		token("connection id", value.ConnectionID) != nil || value.Version == 0 ||
		token("reason code", value.ReasonCode) != nil || !validUTC(value.RevokedAt) ||
		value.Signature.Validate() != nil {
		return fmt.Errorf("financial adapter: connection revocation is invalid")
	}
	return nil
}

type AssetValuation struct {
	Asset              string `json:"asset"`
	AtomicDecimals     uint8  `json:"atomic_decimals"`
	MicrounitsPerWhole uint64 `json:"microunits_per_whole"`
	ConfidenceBPS      uint16 `json:"confidence_bps"`
}

func (value AssetValuation) Validate() error {
	if token("asset", value.Asset) != nil || value.AtomicDecimals > 30 ||
		!money(value.MicrounitsPerWhole) || value.ConfidenceBPS == 0 || value.ConfidenceBPS > 10000 {
		return fmt.Errorf("financial adapter: asset valuation is invalid")
	}
	return nil
}

type ValuationSnapshot struct {
	SchemaVersion     string                   `json:"schema_version"`
	ID                string                   `json:"id"`
	Version           uint64                   `json:"version"`
	OrganizationID    contracts.OrganizationID `json:"organization_id"`
	ConnectionID      string                   `json:"connection_id"`
	ConnectionVersion uint64                   `json:"connection_version"`
	BaseCurrency      string                   `json:"base_currency"`
	Prices            []AssetValuation         `json:"prices"`
	Methodology       string                   `json:"methodology"`
	SourceRef         string                   `json:"source_ref"`
	SourceHash        contracts.ContentHash    `json:"source_hash"`
	ObservedAt        time.Time                `json:"observed_at"`
	ExpiresAt         time.Time                `json:"expires_at"`
	Signature         contracts.Signature      `json:"signature"`
}

func (value ValuationSnapshot) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("valuation id", value.ID) != nil ||
		value.Version == 0 || token("organization id", string(value.OrganizationID)) != nil ||
		token("connection id", value.ConnectionID) != nil || value.ConnectionVersion == 0 ||
		token("base currency", value.BaseCurrency) != nil || len(value.Prices) == 0 ||
		len(value.Prices) > 128 || bounded("valuation methodology", value.Methodology) != nil ||
		bounded("valuation source ref", value.SourceRef) != nil || value.SourceHash.Validate() != nil ||
		!validUTC(value.ObservedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.ObservedAt) ||
		value.ExpiresAt.Sub(value.ObservedAt) > 7*24*time.Hour ||
		value.Signature.Validate() != nil {
		return fmt.Errorf("financial adapter: valuation snapshot is invalid")
	}
	for index, price := range value.Prices {
		if price.Validate() != nil || index > 0 && value.Prices[index-1].Asset >= price.Asset {
			return fmt.Errorf("financial adapter: valuations must be unique and ordered")
		}
	}
	return nil
}

func (value ValuationSnapshot) Price(asset string) (AssetValuation, bool) {
	for _, price := range value.Prices {
		if price.Asset == asset {
			return price, true
		}
	}
	return AssetValuation{}, false
}

type ScopeExposure struct {
	Kind       ScopeKind `json:"kind"`
	Key        string    `json:"key"`
	Microunits uint64    `json:"microunits"`
}

func (value ScopeExposure) Validate() error {
	if !value.Kind.Valid() || bounded("exposure key", value.Key) != nil || value.Microunits > math.MaxInt64 {
		return fmt.Errorf("financial adapter: scope exposure is invalid")
	}
	return nil
}

type RiskState struct {
	BaseCurrency                 string          `json:"base_currency"`
	TotalCapitalMicrounits       uint64          `json:"total_capital_microunits"`
	AvailableLiquidityMicrounits uint64          `json:"available_liquidity_microunits"`
	GrossExposureMicrounits      uint64          `json:"gross_exposure_microunits"`
	NetExposureMicrounits        int64           `json:"net_exposure_microunits"`
	DrawdownMicrounits           uint64          `json:"drawdown_microunits"`
	RunwayMicrounits             uint64          `json:"runway_microunits"`
	Scopes                       []ScopeExposure `json:"scopes"`
	ResourceVersion              string          `json:"resource_version"`
}

func (value RiskState) Validate() error {
	if token("base currency", value.BaseCurrency) != nil ||
		value.TotalCapitalMicrounits > math.MaxInt64 ||
		value.AvailableLiquidityMicrounits > value.TotalCapitalMicrounits ||
		value.GrossExposureMicrounits > math.MaxInt64 ||
		value.DrawdownMicrounits > math.MaxInt64 || value.RunwayMicrounits > math.MaxInt64 ||
		bounded("resource version", value.ResourceVersion) != nil || len(value.Scopes) > 512 {
		return fmt.Errorf("financial adapter: risk state is invalid")
	}
	for index, exposure := range value.Scopes {
		if exposure.Validate() != nil || index > 0 &&
			(string(value.Scopes[index-1].Kind) > string(exposure.Kind) ||
				value.Scopes[index-1].Kind == exposure.Kind && value.Scopes[index-1].Key >= exposure.Key) {
			return fmt.Errorf("financial adapter: risk scopes must be unique and ordered")
		}
	}
	return nil
}

func (value RiskState) Exposure(kind ScopeKind, key string) uint64 {
	for _, exposure := range value.Scopes {
		if exposure.Kind == kind && exposure.Key == key {
			return exposure.Microunits
		}
	}
	return 0
}

type RiskSnapshot struct {
	SchemaVersion     string                   `json:"schema_version"`
	ID                string                   `json:"id"`
	Version           uint64                   `json:"version"`
	OrganizationID    contracts.OrganizationID `json:"organization_id"`
	ConnectionID      string                   `json:"connection_id"`
	ConnectionVersion uint64                   `json:"connection_version"`
	AccountID         string                   `json:"account_id"`
	IdentityID        string                   `json:"identity_id"`
	State             RiskState                `json:"state"`
	SourceRef         string                   `json:"source_ref"`
	SourceHash        contracts.ContentHash    `json:"source_hash"`
	ObservedAt        time.Time                `json:"observed_at"`
	ExpiresAt         time.Time                `json:"expires_at"`
	Signature         contracts.Signature      `json:"signature"`
}

func (value RiskSnapshot) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("risk snapshot id", value.ID) != nil ||
		value.Version == 0 || token("organization id", string(value.OrganizationID)) != nil ||
		token("connection id", value.ConnectionID) != nil || value.ConnectionVersion == 0 ||
		bounded("account id", value.AccountID) != nil || bounded("identity id", value.IdentityID) != nil ||
		value.State.Validate() != nil || bounded("risk source ref", value.SourceRef) != nil ||
		value.SourceHash.Validate() != nil || !validUTC(value.ObservedAt) ||
		!validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.ObservedAt) ||
		value.ExpiresAt.Sub(value.ObservedAt) > 7*24*time.Hour || value.Signature.Validate() != nil {
		return fmt.Errorf("financial adapter: risk snapshot is invalid")
	}
	return nil
}

type AssetAmount struct {
	Asset       string `json:"asset"`
	AtomicUnits string `json:"atomic_units"`
	Decimals    uint8  `json:"decimals"`
}

func (value AssetAmount) Validate() error {
	if token("asset", value.Asset) != nil || value.Decimals > 30 || !unsignedDecimal(value.AtomicUnits, true) {
		return fmt.Errorf("financial adapter: asset amount is invalid")
	}
	return nil
}

type Request struct {
	ProposalID                      string                `json:"proposal_id"`
	IntentID                        contracts.IntentID    `json:"intent_id"`
	Operation                       string                `json:"operation"`
	Family                          Family                `json:"family"`
	Action                          Action                `json:"action"`
	InitiativeID                    string                `json:"initiative_id"`
	Purpose                         string                `json:"purpose"`
	AccountID                       string                `json:"account_id"`
	IdentityID                      string                `json:"identity_id"`
	Amount                          AssetAmount           `json:"amount"`
	BaseCurrency                    string                `json:"base_currency"`
	Instrument                      string                `json:"instrument"`
	Venue                           string                `json:"venue"`
	Rail                            string                `json:"rail"`
	Counterparty                    string                `json:"counterparty"`
	Jurisdiction                    string                `json:"jurisdiction"`
	Destination                     string                `json:"destination"`
	DestinationHash                 contracts.ContentHash `json:"destination_hash"`
	NotionalMicrounits              uint64                `json:"notional_microunits"`
	ExposureIncreaseMicrounits      uint64                `json:"exposure_increase_microunits"`
	MaximumLossMicrounits           uint64                `json:"maximum_loss_microunits"`
	FeeCeilingMicrounits            uint64                `json:"fee_ceiling_microunits"`
	PriceToleranceBPS               uint16                `json:"price_tolerance_bps"`
	ValuationID                     string                `json:"valuation_id"`
	ValuationVersion                uint64                `json:"valuation_version"`
	ValuationHash                   contracts.ContentHash `json:"valuation_hash"`
	RiskSnapshotID                  string                `json:"risk_snapshot_id"`
	RiskSnapshotVersion             uint64                `json:"risk_snapshot_version"`
	RiskSnapshotHash                contracts.ContentHash `json:"risk_snapshot_hash"`
	ExpectedProviderResourceVersion string                `json:"expected_provider_resource_version"`
	AccountingMethodologyID         string                `json:"accounting_methodology_id"`
	LegalPolicyRef                  string                `json:"legal_policy_ref"`
	SecurityPolicyRef               string                `json:"security_policy_ref"`
	ReconciliationPolicyRef         string                `json:"reconciliation_policy_ref"`
	FinancialPolicy                 contracts.PolicyRef   `json:"financial_policy"`
	ApprovalID                      contracts.ApprovalID  `json:"approval_id"`
	ApprovalCostMicrounits          uint64                `json:"approval_cost_microunits"`
	IdempotencyKey                  string                `json:"idempotency_key"`
	IssuedAt                        time.Time             `json:"issued_at"`
	ExpiresAt                       time.Time             `json:"expires_at"`
}

func (value Request) Validate() error {
	if token("proposal id", value.ProposalID) != nil || token("intent id", string(value.IntentID)) != nil ||
		token("operation", value.Operation) != nil || !value.Family.Valid() || !value.Action.Valid() ||
		token("initiative id", value.InitiativeID) != nil || bounded("purpose", value.Purpose) != nil ||
		bounded("account id", value.AccountID) != nil || bounded("identity id", value.IdentityID) != nil ||
		value.Amount.Validate() != nil || token("base currency", value.BaseCurrency) != nil ||
		token("instrument", value.Instrument) != nil || token("venue", value.Venue) != nil ||
		token("rail", value.Rail) != nil || bounded("counterparty", value.Counterparty) != nil ||
		bounded("jurisdiction", value.Jurisdiction) != nil || bounded("destination", value.Destination) != nil ||
		value.DestinationHash.Validate() != nil || value.NotionalMicrounits > math.MaxInt64 ||
		value.ExposureIncreaseMicrounits > math.MaxInt64 || value.MaximumLossMicrounits > math.MaxInt64 ||
		value.FeeCeilingMicrounits > math.MaxInt64 || value.PriceToleranceBPS > 10000 ||
		token("valuation id", value.ValuationID) != nil || value.ValuationVersion == 0 ||
		value.ValuationHash.Validate() != nil || token("risk snapshot id", value.RiskSnapshotID) != nil ||
		value.RiskSnapshotVersion == 0 || value.RiskSnapshotHash.Validate() != nil ||
		bounded("expected provider resource version", value.ExpectedProviderResourceVersion) != nil ||
		token("accounting methodology id", value.AccountingMethodologyID) != nil ||
		bounded("legal policy ref", value.LegalPolicyRef) != nil ||
		bounded("security policy ref", value.SecurityPolicyRef) != nil ||
		bounded("reconciliation policy ref", value.ReconciliationPolicyRef) != nil ||
		value.FinancialPolicy.Validate() != nil || token("idempotency key", value.IdempotencyKey) != nil ||
		!validUTC(value.IssuedAt) || !validUTC(value.ExpiresAt) ||
		!value.ExpiresAt.After(value.IssuedAt) || value.ExpiresAt.Sub(value.IssuedAt) > 10*time.Minute {
		return fmt.Errorf("financial adapter: request identity, authority, or limits are invalid")
	}
	if DestinationHash(value.Destination) != value.DestinationHash {
		return fmt.Errorf("financial adapter: destination hash mismatch")
	}
	if value.Action.Mutates() {
		if value.NotionalMicrounits == 0 ||
			token("approval id", string(value.ApprovalID)) != nil || value.ApprovalCostMicrounits == 0 {
			return fmt.Errorf("financial adapter: mutation lacks exact amount, loss, or approval binding")
		}
	} else if value.ApprovalID != "" || value.ApprovalCostMicrounits != 0 {
		return fmt.Errorf("financial adapter: read-only reconciliation cannot consume mutation approval")
	}
	return nil
}

type FounderReservation struct {
	SchemaVersion          string                   `json:"schema_version"`
	ID                     string                   `json:"id"`
	OrganizationID         contracts.OrganizationID `json:"organization_id"`
	ConnectionID           string                   `json:"connection_id"`
	ConnectionVersion      uint64                   `json:"connection_version"`
	RequestHash            contracts.ContentHash    `json:"request_hash"`
	ProposalID             string                   `json:"proposal_id"`
	IntentID               contracts.IntentID       `json:"intent_id"`
	Operation              string                   `json:"operation"`
	ApprovalID             contracts.ApprovalID     `json:"approval_id"`
	ApprovalCostMicrounits uint64                   `json:"approval_cost_microunits"`
	MaximumNotional        uint64                   `json:"maximum_notional_microunits"`
	DestinationHash        contracts.ContentHash    `json:"destination_hash"`
	IssuedAt               time.Time                `json:"issued_at"`
	ExpiresAt              time.Time                `json:"expires_at"`
	Signature              contracts.Signature      `json:"signature"`
}

func (value FounderReservation) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("reservation id", value.ID) != nil ||
		token("organization id", string(value.OrganizationID)) != nil ||
		token("connection id", value.ConnectionID) != nil || value.ConnectionVersion == 0 ||
		value.RequestHash.Validate() != nil || token("proposal id", value.ProposalID) != nil ||
		token("intent id", string(value.IntentID)) != nil || token("operation", value.Operation) != nil ||
		token("approval id", string(value.ApprovalID)) != nil ||
		!money(value.ApprovalCostMicrounits) || !money(value.MaximumNotional) ||
		value.DestinationHash.Validate() != nil || !validUTC(value.IssuedAt) ||
		!validUTC(value.ExpiresAt) || !value.ExpiresAt.After(value.IssuedAt) ||
		value.ExpiresAt.Sub(value.IssuedAt) > 24*time.Hour || value.Signature.Validate() != nil {
		return fmt.Errorf("financial adapter: founder reservation is invalid")
	}
	return nil
}

type Envelope struct {
	SchemaVersion     string                `json:"schema_version"`
	ConnectionID      string                `json:"connection_id"`
	ConnectionVersion uint64                `json:"connection_version"`
	ConnectionHash    contracts.ContentHash `json:"connection_hash"`
	Grant             lease.Grant           `json:"grant"`
	Request           Request               `json:"request"`
	Founder           *FounderReservation   `json:"founder_reservation"`
	External          external.Envelope     `json:"external"`
}

func (value Envelope) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("connection id", value.ConnectionID) != nil ||
		value.ConnectionVersion == 0 || value.ConnectionHash.Validate() != nil ||
		value.Grant.Request.Validate() != nil || value.Grant.Fence.Validate() != nil ||
		value.Grant.State != lease.StateActive || value.Request.Validate() != nil ||
		value.External.Validate() != nil || value.Request.ExpiresAt.After(value.Grant.ExpiresAt) {
		return fmt.Errorf("financial adapter: envelope authority or request is invalid")
	}
	left, err := json.Marshal(&value.Grant)
	if err != nil {
		return err
	}
	right, err := json.Marshal(&value.External.Grant)
	if err != nil || !bytes.Equal(left, right) {
		return fmt.Errorf("financial adapter: external grant does not match financial grant")
	}
	if value.Founder != nil && value.Founder.Validate() != nil {
		return fmt.Errorf("financial adapter: founder reservation is invalid")
	}
	return nil
}

type ProviderCommand struct {
	SchemaVersion              string                   `json:"schema_version"`
	OrganizationID             contracts.OrganizationID `json:"organization_id"`
	InitiativeID               string                   `json:"initiative_id"`
	Family                     Family                   `json:"family"`
	Action                     Action                   `json:"action"`
	ProviderContract           ProviderContract         `json:"provider_contract"`
	Purpose                    string                   `json:"purpose"`
	AccountID                  string                   `json:"account_id"`
	IdentityID                 string                   `json:"identity_id"`
	Amount                     AssetAmount              `json:"amount"`
	BaseCurrency               string                   `json:"base_currency"`
	Instrument                 string                   `json:"instrument"`
	Venue                      string                   `json:"venue"`
	Rail                       string                   `json:"rail"`
	Counterparty               string                   `json:"counterparty"`
	Jurisdiction               string                   `json:"jurisdiction"`
	Destination                string                   `json:"destination"`
	DestinationHash            contracts.ContentHash    `json:"destination_hash"`
	NotionalMicrounits         uint64                   `json:"notional_microunits"`
	ExposureIncreaseMicrounits uint64                   `json:"exposure_increase_microunits"`
	MaximumLossMicrounits      uint64                   `json:"maximum_loss_microunits"`
	FeeCeilingMicrounits       uint64                   `json:"fee_ceiling_microunits"`
	PriceToleranceBPS          uint16                   `json:"price_tolerance_bps"`
	ValuationID                string                   `json:"valuation_id"`
	ValuationVersion           uint64                   `json:"valuation_version"`
	ValuationHash              contracts.ContentHash    `json:"valuation_hash"`
	ExpectedResourceVersion    string                   `json:"expected_resource_version"`
	LegalPolicyRef             string                   `json:"legal_policy_ref"`
	SecurityPolicyRef          string                   `json:"security_policy_ref"`
	ReconciliationPolicyRef    string                   `json:"reconciliation_policy_ref"`
	IdempotencyKey             string                   `json:"idempotency_key"`
}

func (value ProviderCommand) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("organization id", string(value.OrganizationID)) != nil ||
		token("initiative id", value.InitiativeID) != nil || !value.Family.Valid() || !value.Action.Valid() ||
		value.ProviderContract.Validate(value.Family) != nil ||
		bounded("purpose", value.Purpose) != nil || bounded("account id", value.AccountID) != nil ||
		bounded("identity id", value.IdentityID) != nil || value.Amount.Validate() != nil ||
		token("base currency", value.BaseCurrency) != nil || token("instrument", value.Instrument) != nil ||
		token("venue", value.Venue) != nil || token("rail", value.Rail) != nil ||
		bounded("counterparty", value.Counterparty) != nil || bounded("jurisdiction", value.Jurisdiction) != nil ||
		bounded("destination", value.Destination) != nil || value.DestinationHash.Validate() != nil ||
		DestinationHash(value.Destination) != value.DestinationHash || value.NotionalMicrounits > math.MaxInt64 ||
		value.ExposureIncreaseMicrounits > math.MaxInt64 || value.MaximumLossMicrounits > math.MaxInt64 ||
		value.FeeCeilingMicrounits > math.MaxInt64 || value.PriceToleranceBPS > 10000 ||
		token("valuation id", value.ValuationID) != nil || value.ValuationVersion == 0 ||
		value.ValuationHash.Validate() != nil || bounded("expected resource version", value.ExpectedResourceVersion) != nil ||
		bounded("legal policy ref", value.LegalPolicyRef) != nil ||
		bounded("security policy ref", value.SecurityPolicyRef) != nil ||
		bounded("reconciliation policy ref", value.ReconciliationPolicyRef) != nil ||
		token("idempotency key", value.IdempotencyKey) != nil {
		return fmt.Errorf("financial adapter: provider command is invalid")
	}
	if value.Action.Mutates() && value.NotionalMicrounits == 0 {
		return fmt.Errorf("financial adapter: provider mutation lacks exact amount")
	}
	return nil
}

type AccountingSide string

const (
	AccountingDebit  AccountingSide = "debit"
	AccountingCredit AccountingSide = "credit"
)

func (value AccountingSide) Valid() bool {
	return value == AccountingDebit || value == AccountingCredit
}

type AccountingLine struct {
	AccountID  string         `json:"account_id"`
	Side       AccountingSide `json:"side"`
	Currency   string         `json:"currency"`
	Microunits uint64         `json:"microunits"`
}

func (value AccountingLine) Validate() error {
	if token("account id", value.AccountID) != nil || !value.Side.Valid() ||
		token("currency", value.Currency) != nil || !money(value.Microunits) {
		return fmt.Errorf("financial adapter: accounting line is invalid")
	}
	return nil
}

type ProviderReference struct {
	NetworkID         string `json:"network_id"`
	ChainID           uint64 `json:"chain_id"`
	TransactionHash   string `json:"transaction_hash"`
	BlockNumber       uint64 `json:"block_number"`
	Confirmations     uint32 `json:"confirmations"`
	LayerXSequence    uint64 `json:"layerx_sequence"`
	SettlementBatchID string `json:"settlement_batch_id"`
	InvoiceID         string `json:"invoice_id"`
	PaymentID         string `json:"payment_id"`
	LedgerEntryID     string `json:"ledger_entry_id"`
}

func (value ProviderReference) Validate(family Family, action Action, state FinancialState, authoritative bool) error {
	if token("network id", value.NetworkID) != nil {
		return fmt.Errorf("financial adapter: provider reference network is invalid")
	}
	terminal := authoritative && !state.Inconclusive() && !state.DefinitiveFailure()
	if !terminal {
		for _, field := range []string{
			value.TransactionHash, value.SettlementBatchID, value.InvoiceID,
			value.PaymentID, value.LedgerEntryID,
		} {
			if field != "" && bounded("provider reference", field) != nil {
				return fmt.Errorf("financial adapter: provider reference is invalid")
			}
		}
		return nil
	}
	switch family {
	case FamilyPaxeer:
		if value.ChainID == 0 || value.BlockNumber == 0 || value.Confirmations == 0 ||
			action.Mutates() && !evmTransactionHash(value.TransactionHash) {
			return fmt.Errorf("financial adapter: settled Paxeer outcome lacks chain proof")
		}
	case FamilyLayerX:
		if value.ChainID == 0 || value.LayerXSequence == 0 ||
			action.Mutates() && token("settlement batch id", value.SettlementBatchID) != nil {
			return fmt.Errorf("financial adapter: settled LayerX outcome lacks sequence and batch proof")
		}
	case FamilyBilling, FamilyInvoicing, FamilyPayment, FamilyCollection:
		if token("ledger entry id", value.LedgerEntryID) != nil {
			return fmt.Errorf("financial adapter: billing outcome lacks authoritative ledger entry")
		}
		if family == FamilyInvoicing && token("invoice id", value.InvoiceID) != nil {
			return fmt.Errorf("financial adapter: invoice outcome lacks invoice identity")
		}
		if (family == FamilyPayment || family == FamilyCollection) && token("payment id", value.PaymentID) != nil {
			return fmt.Errorf("financial adapter: payment outcome lacks payment identity")
		}
	default:
		if token("ledger entry id", value.LedgerEntryID) != nil {
			return fmt.Errorf("financial adapter: financial outcome lacks authoritative ledger identity")
		}
	}
	return nil
}

type ProviderOutcome struct {
	SchemaVersion           string                   `json:"schema_version"`
	OrganizationID          contracts.OrganizationID `json:"organization_id"`
	InitiativeID            string                   `json:"initiative_id"`
	AccountID               string                   `json:"account_id"`
	IdentityID              string                   `json:"identity_id"`
	Family                  Family                   `json:"family"`
	Action                  Action                   `json:"action"`
	Reference               ProviderReference        `json:"reference"`
	Asset                   string                   `json:"asset"`
	Instrument              string                   `json:"instrument"`
	Venue                   string                   `json:"venue"`
	Rail                    string                   `json:"rail"`
	Counterparty            string                   `json:"counterparty"`
	DestinationHash         contracts.ContentHash    `json:"destination_hash"`
	IdempotencyKey          string                   `json:"idempotency_key"`
	ExternalID              string                   `json:"external_id"`
	State                   FinancialState           `json:"state"`
	Authoritative           bool                     `json:"authoritative"`
	PrincipalMicrounits     uint64                   `json:"principal_microunits"`
	FeeMicrounits           uint64                   `json:"fee_microunits"`
	ValuationID             string                   `json:"valuation_id"`
	ValuationVersion        uint64                   `json:"valuation_version"`
	ValuationHash           contracts.ContentHash    `json:"valuation_hash"`
	PreviousResourceVersion string                   `json:"previous_resource_version"`
	ResourceVersion         string                   `json:"resource_version"`
	SettlementEvidenceHash  contracts.ContentHash    `json:"settlement_evidence_hash"`
	ObservedAt              time.Time                `json:"observed_at"`
	RiskAfter               *RiskState               `json:"risk_after"`
	Accounting              []AccountingLine         `json:"accounting"`
}

func (value ProviderOutcome) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("organization id", string(value.OrganizationID)) != nil ||
		token("initiative id", value.InitiativeID) != nil || bounded("account id", value.AccountID) != nil ||
		bounded("identity id", value.IdentityID) != nil || !value.Family.Valid() || !value.Action.Valid() ||
		value.Reference.Validate(value.Family, value.Action, value.State, value.Authoritative) != nil ||
		token("asset", value.Asset) != nil || token("instrument", value.Instrument) != nil ||
		token("venue", value.Venue) != nil || token("rail", value.Rail) != nil ||
		bounded("counterparty", value.Counterparty) != nil || value.DestinationHash.Validate() != nil ||
		token("idempotency key", value.IdempotencyKey) != nil || bounded("external id", value.ExternalID) != nil ||
		!value.State.Valid() || value.PrincipalMicrounits > math.MaxInt64 ||
		value.FeeMicrounits > math.MaxInt64 || token("valuation id", value.ValuationID) != nil ||
		value.ValuationVersion == 0 || value.ValuationHash.Validate() != nil ||
		bounded("previous resource version", value.PreviousResourceVersion) != nil ||
		bounded("resource version", value.ResourceVersion) != nil || !validUTC(value.ObservedAt) ||
		len(value.Accounting) > 128 {
		return fmt.Errorf("financial adapter: provider outcome is invalid")
	}
	terminalTruth := value.Authoritative && !value.State.Inconclusive() && !value.State.DefinitiveFailure()
	if terminalTruth {
		if value.SettlementEvidenceHash.Validate() != nil || value.RiskAfter == nil ||
			value.RiskAfter.Validate() != nil || len(value.Accounting) == 0 {
			return fmt.Errorf("financial adapter: terminal financial outcome lacks reconciliation evidence")
		}
		if err := validateAccounting(value.Accounting, value.RiskAfter.BaseCurrency); err != nil {
			return err
		}
	} else {
		if value.RiskAfter != nil || len(value.Accounting) != 0 ||
			value.SettlementEvidenceHash.Algorithm != "" || value.SettlementEvidenceHash.Digest != "" {
			return fmt.Errorf("financial adapter: non-terminal outcome cannot assert accounting or risk truth")
		}
	}
	return nil
}

type Observation struct {
	SchemaVersion      string                        `json:"schema_version"`
	OrganizationID     contracts.OrganizationID      `json:"organization_id"`
	ConnectionID       string                        `json:"connection_id"`
	ConnectionVersion  uint64                        `json:"connection_version"`
	Family             Family                        `json:"family"`
	Operation          string                        `json:"operation"`
	Action             Action                        `json:"action"`
	InitiativeID       string                        `json:"initiative_id"`
	Asset              string                        `json:"asset"`
	Venue              string                        `json:"venue"`
	Rail               string                        `json:"rail"`
	Counterparty       string                        `json:"counterparty"`
	DestinationHash    contracts.ContentHash         `json:"destination_hash"`
	ExternalID         string                        `json:"external_id"`
	IdempotencyKey     string                        `json:"idempotency_key"`
	State              FinancialState                `json:"state"`
	Authority          external.ObservationAuthority `json:"authority"`
	Reconciled         bool                          `json:"reconciled"`
	EconomicTruth      bool                          `json:"economic_truth"`
	ValuationTime      time.Time                     `json:"valuation_time"`
	ProviderObservedAt time.Time                     `json:"provider_observed_at"`
	CapturedAt         time.Time                     `json:"captured_at"`
	ExternalHash       contracts.ContentHash         `json:"external_hash"`
	OutcomeHash        contracts.ContentHash         `json:"outcome_hash"`
	Outcome            ProviderOutcome               `json:"outcome"`
}

func (value Observation) Validate() error {
	if value.SchemaVersion != SchemaVersion || token("organization id", string(value.OrganizationID)) != nil ||
		token("connection id", value.ConnectionID) != nil || value.ConnectionVersion == 0 ||
		!value.Family.Valid() || token("operation", value.Operation) != nil || !value.Action.Valid() ||
		token("initiative id", value.InitiativeID) != nil || token("asset", value.Asset) != nil ||
		token("venue", value.Venue) != nil || token("rail", value.Rail) != nil ||
		bounded("counterparty", value.Counterparty) != nil || value.DestinationHash.Validate() != nil ||
		bounded("external id", value.ExternalID) != nil || token("idempotency key", value.IdempotencyKey) != nil ||
		!value.State.Valid() || !value.Authority.Valid() || !validUTC(value.ValuationTime) ||
		!validUTC(value.ProviderObservedAt) || !validUTC(value.CapturedAt) ||
		value.ProviderObservedAt.After(value.CapturedAt.Add(5*time.Minute)) ||
		value.ExternalHash.Validate() != nil || value.OutcomeHash.Validate() != nil ||
		value.Outcome.Validate() != nil {
		return fmt.Errorf("financial adapter: observation is invalid")
	}
	eligibleTruth := value.Reconciled && value.Outcome.Authoritative && value.Authority != external.AuthorityUntrustedExternal &&
		!value.State.Inconclusive() && !value.State.DefinitiveFailure()
	if value.EconomicTruth && !eligibleTruth {
		return fmt.Errorf("financial adapter: economic truth is inconsistent with authoritative state")
	}
	return nil
}

type ConnectionView struct {
	Connection Connection            `json:"connection"`
	Hash       contracts.ContentHash `json:"hash"`
}

type ValuationView struct {
	Valuation ValuationSnapshot     `json:"valuation"`
	Hash      contracts.ContentHash `json:"hash"`
}

type RiskView struct {
	Risk RiskSnapshot          `json:"risk"`
	Hash contracts.ContentHash `json:"hash"`
}

func DestinationHash(destination string) contracts.ContentHash {
	sum := sha256.Sum256([]byte(destination))
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}
}

func CanonicalHash[T interface{ Validate() error }](value T) (contracts.ContentHash, error) {
	encoded, err := contracts.EncodeCanonical(value)
	if err != nil {
		return contracts.ContentHash{}, err
	}
	sum := sha256.Sum256(encoded)
	return contracts.ContentHash{Algorithm: "sha256", Digest: hex.EncodeToString(sum[:])}, nil
}

func NotionalMicrounits(amount AssetAmount, price AssetValuation) (uint64, error) {
	if amount.Validate() != nil || price.Validate() != nil || amount.Asset != price.Asset || amount.Decimals != price.AtomicDecimals {
		return 0, fmt.Errorf("financial adapter: amount and valuation are incompatible")
	}
	atoms := new(big.Int)
	if _, ok := atoms.SetString(amount.AtomicUnits, 10); !ok || atoms.Sign() < 0 {
		return 0, fmt.Errorf("financial adapter: amount is not an unsigned integer")
	}
	numerator := new(big.Int).Mul(atoms, new(big.Int).SetUint64(price.MicrounitsPerWhole))
	denominator := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(amount.Decimals)), nil)
	quotient, remainder := new(big.Int).QuoRem(numerator, denominator, new(big.Int))
	if remainder.Sign() > 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsUint64() || quotient.Uint64() > math.MaxInt64 {
		return 0, fmt.Errorf("financial adapter: valued amount exceeds exact integer limits")
	}
	return quotient.Uint64(), nil
}

func validateAccounting(lines []AccountingLine, currency string) error {
	var debits, credits uint64
	for index, line := range lines {
		if line.Validate() != nil || line.Currency != currency || index > 0 &&
			(lines[index-1].AccountID > line.AccountID ||
				lines[index-1].AccountID == line.AccountID && string(lines[index-1].Side) >= string(line.Side)) {
			return fmt.Errorf("financial adapter: accounting lines must be valid, ordered, and unique")
		}
		var ok bool
		if line.Side == AccountingDebit {
			debits, ok = add(debits, line.Microunits)
		} else {
			credits, ok = add(credits, line.Microunits)
		}
		if !ok {
			return fmt.Errorf("financial adapter: accounting total overflow")
		}
	}
	if debits == 0 || debits != credits {
		return fmt.Errorf("financial adapter: accounting entry is not balanced")
	}
	return nil
}

func add(left, right uint64) (uint64, bool) {
	if left > math.MaxInt64 || right > math.MaxInt64 || left > math.MaxInt64-right {
		return 0, false
	}
	return left + right, true
}

func concentrationExceeds(value, total uint64, maximumBPS uint16) bool {
	if value == 0 {
		return false
	}
	if total == 0 {
		return true
	}
	left := new(big.Int).Mul(new(big.Int).SetUint64(value), big.NewInt(10000))
	right := new(big.Int).Mul(new(big.Int).SetUint64(total), new(big.Int).SetUint64(uint64(maximumBPS)))
	return left.Cmp(right) > 0
}

func money(value uint64) bool { return value > 0 && value <= math.MaxInt64 }

func unsignedDecimal(value string, allowZero bool) bool {
	if value == "" || len(value) > 96 || len(value) > 1 && value[0] == '0' {
		return false
	}
	for _, character := range value {
		if character < '0' || character > '9' {
			return false
		}
	}
	return allowZero || value != "0"
}

func evmAddress(value string) bool {
	return hexadecimal0x(value, 40)
}

func evmTransactionHash(value string) bool {
	return hexadecimal0x(value, 64)
}

func hexadecimal0x(value string, digits int) bool {
	if len(value) != digits+2 || value[0:2] != "0x" {
		return false
	}
	for _, character := range value[2:] {
		if character >= '0' && character <= '9' || character >= 'a' && character <= 'f' ||
			character >= 'A' && character <= 'F' {
			continue
		}
		return false
	}
	return true
}

func validProviderURL(value string) bool {
	if len(value) == 0 || len(value) > 4096 || strings.TrimSpace(value) != value {
		return false
	}
	parsed, err := url.Parse(value)
	return err == nil && parsed.Scheme == "https" && parsed.Host != "" && parsed.User == nil &&
		parsed.Fragment == ""
}

func token(name, value string) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > 128 {
		return fmt.Errorf("%s must contain 1 to 128 bytes", name)
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' || character == '-' || character == '_' ||
			character == '.' || character == ':' || character == '/' {
			continue
		}
		return fmt.Errorf("%s contains an invalid character", name)
	}
	return nil
}

func bounded(name, value string) error {
	if strings.TrimSpace(value) != value || value == "" || len(value) > 1024 ||
		strings.ContainsAny(value, "\r\n\x00") {
		return fmt.Errorf("%s is invalid", name)
	}
	return nil
}

func sortedTokens(values []string, maximum int) bool {
	if len(values) == 0 || len(values) > maximum || !sort.StringsAreSorted(values) {
		return false
	}
	for index, value := range values {
		if token("value", value) != nil || index > 0 && values[index-1] == value {
			return false
		}
	}
	return true
}

func sortedValues(values []string, maximum, maximumBytes int) bool {
	if len(values) == 0 || len(values) > maximum || !sort.StringsAreSorted(values) {
		return false
	}
	for index, value := range values {
		if strings.TrimSpace(value) != value || value == "" || len(value) > maximumBytes ||
			strings.ContainsAny(value, "\r\n\x00") || index > 0 && values[index-1] == value {
			return false
		}
	}
	return true
}

func contains(values []string, target string) bool {
	index := sort.SearchStrings(values, target)
	return index < len(values) && values[index] == target
}

func containsHash(values []contracts.ContentHash, target contracts.ContentHash) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func containsPolicy(values []contracts.PolicyRef, target contracts.PolicyRef) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func validUTC(value time.Time) bool { return !value.IsZero() && value.Location() == time.UTC }
