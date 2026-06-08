// Package registry implements listing lifecycle (docs/05-api.md §5.3).
package registry

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ethereum/go-ethereum/common"

	"github.com/paxlabs-inc/deus/internal/chain"
	"github.com/paxlabs-inc/deus/internal/indexer"
	"github.com/paxlabs-inc/deus/internal/store"
	"github.com/paxlabs-inc/deus/pkg/manifest"
)

// Service orchestrates listing CRUD and publish.
type Service struct {
	store    *store.Store
	registry *chain.Registry
	indexer  *indexer.Indexer
}

// NewService wires registry dependencies.
func NewService(st *store.Store, reg *chain.Registry, ix *indexer.Indexer) *Service {
	return &Service{store: st, registry: reg, indexer: ix}
}

// CreateInput is the POST /v1/services body.
type CreateInput struct {
	Manifest json.RawMessage
	Owner    string
}

// CreateResult is the POST /v1/services response.
type CreateResult struct {
	ID           string
	Slug         string
	Status       string
	ManifestHash string
	Validation   ValidationResult
}

// Create validates and inserts a draft listing.
func (s *Service) Create(ctx context.Context, in CreateInput) (CreateResult, error) {
	m, err := manifest.ValidateBytes(in.Manifest)
	if err != nil {
		return CreateResult{}, fmt.Errorf("registry: validate: %w", err)
	}
	val := ValidateManifest(m)
	if !val.OK {
		return CreateResult{}, fmt.Errorf("registry: manifest invalid: %v", val.Errors)
	}
	owner := strings.ToLower(in.Owner)
	if owner == "" {
		owner = strings.ToLower(m.Owner)
	}
	if !strings.EqualFold(owner, m.Owner) {
		return CreateResult{}, fmt.Errorf("registry: owner mismatch")
	}

	manifestHash, err := manifest.Hash(m)
	if err != nil {
		return CreateResult{}, err
	}
	devID, err := s.store.UpsertDeveloperByWallet(ctx, owner, m.PayoutAddress, m.DisplayName)
	if err != nil {
		return CreateResult{}, err
	}

	raw, _ := json.Marshal(m)
	id, err := s.store.InsertDraftService(ctx, store.ServiceRow{
		DeveloperID:  devID,
		Slug:         m.Slug,
		Kind:         m.Kind,
		Mode:         m.Mode,
		DisplayName:  m.DisplayName,
		Summary:      m.Summary,
		Manifest:     raw,
		ManifestHash: manifestHash,
		Status:       "draft",
		Confidential: m.Confidential,
	})
	if err != nil {
		return CreateResult{}, err
	}
	if err := s.syncChildren(ctx, id, m); err != nil {
		return CreateResult{}, err
	}
	return CreateResult{
		ID:           id,
		Slug:         m.Slug,
		Status:       "draft",
		ManifestHash: manifestHash,
		Validation:   val,
	}, nil
}

// PublishInput carries on-chain registration parameters.
type PublishInput struct {
	ServiceID     string
	Owner         string
	PrivateKeyHex string
}

// PublishResult is returned after on-chain register + mirror.
type PublishResult struct {
	ID           string
	ChainID      uint64
	Status       string
	ManifestHash string
	TxHash       string
}

// Publish validates, registers on-chain, and activates the mirror.
func (s *Service) Publish(ctx context.Context, in PublishInput) (PublishResult, error) {
	if s.registry == nil {
		return PublishResult{}, fmt.Errorf("registry: chain registry not configured")
	}
	row, err := s.store.GetServiceByID(ctx, in.ServiceID)
	if err != nil {
		return PublishResult{}, err
	}
	wallet, err := s.store.DeveloperWalletByID(ctx, row.DeveloperID)
	if err != nil {
		return PublishResult{}, err
	}
	if in.Owner != "" && !strings.EqualFold(in.Owner, wallet) {
		return PublishResult{}, fmt.Errorf("registry: forbidden")
	}

	m, err := manifest.Parse(row.Manifest)
	if err != nil {
		return PublishResult{}, err
	}
	val := ValidateManifest(m)
	if !val.OK {
		return PublishResult{}, fmt.Errorf("registry: manifest invalid: %v", val.Errors)
	}
	if m.Mode == "hosted" {
		if _, err := s.store.ActiveDeploymentForService(ctx, row.ID); err != nil {
			return PublishResult{}, fmt.Errorf("registry: hosted service requires active deployment before publish")
		}
	}
	manifestHash, err := manifest.Hash(m)
	if err != nil {
		return PublishResult{}, err
	}
	pricingHash, err := manifest.PricingCommitmentHash(m)
	if err != nil {
		return PublishResult{}, err
	}
	mh, err := hexToBytes32(manifestHash)
	if err != nil {
		return PublishResult{}, err
	}
	ph, err := hexToBytes32(pricingHash)
	if err != nil {
		return PublishResult{}, err
	}

	payout := common.HexToAddress(m.PayoutAddress)
	res, err := s.registry.Register(ctx, chain.RegisterRequest{
		Payout:        payout,
		ManifestHash:  mh,
		PricingHash:   ph,
		Hosted:        m.Mode == "hosted",
		Confidential:  m.Confidential,
		PrivateKeyHex: in.PrivateKeyHex,
	})
	if err != nil {
		return PublishResult{}, err
	}

	if err := s.store.ActivateFromChain(ctx, row.ID, int64(res.ChainServiceID), manifestHash, pricingHash, m.Mode == "hosted", m.Confidential); err != nil {
		return PublishResult{}, err
	}
	if s.indexer != nil {
		_ = s.indexer.Sync(ctx, int64(res.BlockNumber))
	}

	return PublishResult{
		ID:           row.ID,
		ChainID:      res.ChainServiceID,
		Status:       "active",
		ManifestHash: manifestHash,
		TxHash:       res.TxHash,
	}, nil
}

// Get returns a listing by id.
func (s *Service) Get(ctx context.Context, id string) (store.ServiceRow, error) {
	return s.store.GetServiceByID(ctx, id)
}

func (s *Service) syncChildren(ctx context.Context, serviceID string, m *manifest.Manifest) error {
	eps := make([]store.EndpointRow, 0, len(m.Operations))
	for _, op := range m.Operations {
		inSchema, _ := json.Marshal(op.InputSchema)
		outSchema, _ := json.Marshal(op.OutputSchema)
		var proxyURL *string
		if m.Endpoint != nil && m.Endpoint.ProxyURL != "" {
			u := m.Endpoint.ProxyURL
			proxyURL = &u
		}
		eps = append(eps, store.EndpointRow{
			Operation:    op.Name,
			Method:       op.Method,
			InputSchema:  inSchema,
			OutputSchema: outSchema,
			ProxyURL:     proxyURL,
		})
	}
	if err := s.store.InsertEndpoints(ctx, serviceID, eps); err != nil {
		return err
	}
	plans := make([]store.PricingRow, 0, len(m.Pricing))
	for _, p := range m.Pricing {
		plans = append(plans, store.PricingRow{
			Model:        p.Model,
			Unit:         p.Unit,
			PriceWei:     p.PriceWei,
			MinChargeWei: p.MinChargeWei,
			Version:      1,
		})
	}
	return s.store.InsertPricingPlans(ctx, serviceID, plans)
}

func hexToBytes32(hexStr string) ([32]byte, error) {
	var out [32]byte
	h := strings.TrimPrefix(strings.ToLower(hexStr), "0x")
	if len(h) != 64 {
		return out, fmt.Errorf("registry: invalid bytes32 hex len %d", len(h))
	}
	b, err := hex.DecodeString(h)
	if err != nil {
		return out, err
	}
	copy(out[:], b)
	return out, nil
}
