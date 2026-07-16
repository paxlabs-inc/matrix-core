package server

import (
	"context"
	"fmt"

	"github.com/paxlabs-inc/deus/internal/store"
)

func (s *Server) developerForPrincipal(ctx context.Context, principal DeveloperPrincipal) (store.DeveloperRow, error) {
	if s.deps.Store == nil {
		return store.DeveloperRow{}, fmt.Errorf("store not configured")
	}
	switch principal.Kind {
	case DeveloperPrincipalAccount:
		return s.deps.Store.DeveloperByAccountID(ctx, principal.Subject)
	case DeveloperPrincipalWallet:
		return s.deps.Store.DeveloperByWallet(ctx, principal.Subject)
	default:
		return store.DeveloperRow{}, fmt.Errorf("developer principal missing")
	}
}

func (s *Server) ensureAccountDeveloper(ctx context.Context, principal DeveloperPrincipal) (string, error) {
	if principal.Kind != DeveloperPrincipalAccount {
		return "", nil
	}
	return s.deps.Store.UpsertDeveloperByAccount(ctx, principal.Subject, principal.Owner, principal.DisplayName)
}
