// Package lxtest assembles a fully-wired, REAL layerxd HTTP handler over a
// real Postgres store for consumers' integration tests (deus/LXP, tools). It
// contains no fakes: the handler is the production server package over the
// production store, ledger, auth, and signer — only the chain settler is
// absent (dev posture, exactly like a chain-unconfigured layerxd).
package lxtest

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"github.com/paxlabs-inc/layerx/internal/auth"
	"github.com/paxlabs-inc/layerx/internal/ledger"
	"github.com/paxlabs-inc/layerx/internal/server"
	"github.com/paxlabs-inc/layerx/internal/sig"
	"github.com/paxlabs-inc/layerx/internal/store"
)

// Harness is a live layerxd surface over a disposable database.
type Harness struct {
	Handler         http.Handler
	SequencerPubHex string

	st *store.Store
}

// Config tunes the harness.
type Config struct {
	// PostgresURI points at a disposable database. Required.
	PostgresURI string
	// MigrationsDir is the layerx migrations directory. Required (the caller
	// knows where the layerx checkout lives relative to its own tests).
	MigrationsDir string
	// MicroThreshold is the material-tier threshold in micro-USDX (default 1 USDX).
	MicroThreshold int64
	// ChallengeTTL bounds nonce life (default 1 minute).
	ChallengeTTL time.Duration
}

// New connects, migrates, and wires the real layerxd handler.
func New(ctx context.Context, cfg Config) (*Harness, error) {
	if cfg.PostgresURI == "" {
		return nil, fmt.Errorf("lxtest: postgres uri required")
	}
	if cfg.MigrationsDir == "" {
		return nil, fmt.Errorf("lxtest: migrations dir required")
	}
	if cfg.MicroThreshold <= 0 {
		cfg.MicroThreshold = 1_000_000
	}
	if cfg.ChallengeTTL <= 0 {
		cfg.ChallengeTTL = time.Minute
	}
	st, err := store.New(ctx, cfg.PostgresURI)
	if err != nil {
		return nil, fmt.Errorf("lxtest: connect: %w", err)
	}
	if err := st.Migrate(ctx, cfg.MigrationsDir); err != nil {
		st.Close()
		return nil, fmt.Errorf("lxtest: migrate: %w", err)
	}
	signer, _, err := sig.New("")
	if err != nil {
		st.Close()
		return nil, fmt.Errorf("lxtest: signer: %w", err)
	}
	srv := server.New(server.Deps{
		Store:           st,
		Ledger:          ledger.New(st, signer, cfg.MicroThreshold),
		Challenges:      auth.NewChallenges(cfg.ChallengeTTL),
		Tokens:          auth.NewTokens("lxtest-agent-secret", time.Hour),
		ChainID:         125,
		SequencerPubHex: signer.PublicHex(),
	})
	return &Harness{
		Handler:         srv.Handler(),
		SequencerPubHex: signer.PublicHex(),
		st:              st,
	}, nil
}

// CreditDeposit funds an account directly (simulating a confirmed on-chain
// deposit), so consumer tests can stage payer balances.
func (h *Harness) CreditDeposit(ctx context.Context, did, evm, depositTx string, amountMicro int64) error {
	return h.st.CreditDeposit(ctx, did, evm, depositTx, amountMicro)
}

// SweepExpiredHolds runs one hold-expiry sweep (what the settle worker ticks).
func (h *Harness) SweepExpiredHolds(ctx context.Context) (int, error) {
	return h.st.SweepExpiredHolds(ctx)
}

// BalanceMicro reads an account's spendable balance in micro-USDX (0 for an
// unknown account).
func (h *Harness) BalanceMicro(ctx context.Context, did string) (int64, error) {
	acct, err := h.st.GetAccount(ctx, did)
	if err != nil {
		if err == store.ErrNotFound {
			return 0, nil
		}
		return 0, err
	}
	return acct.BalanceUSDX, nil
}

// Close releases the store pool.
func (h *Harness) Close() { h.st.Close() }

// IntentMessage re-exports the canonical DID-signed intent preimage so
// consumer tests can sign payer intents byte-identically to layerxd
// (auth.IntentMessage lockstep).
func IntentMessage(op, did, nonce string, fields ...string) string {
	return auth.IntentMessage(op, did, nonce, fields...)
}
