package server

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"
)

type ctxKey string

const developerPrincipalKey ctxKey = "developer_principal"

const (
	DeveloperPrincipalWallet  = "wallet"
	DeveloperPrincipalAccount = "account"
)

var matrixDIDRe = regexp.MustCompile(`^did:matrix:[^:]+:[0-9a-fA-F]{16}$`)

type DeveloperPrincipal struct {
	Kind        string
	Subject     string
	Owner       string
	DisplayName string
}

func DeveloperPrincipalFromContext(ctx context.Context) DeveloperPrincipal {
	v, _ := ctx.Value(developerPrincipalKey).(DeveloperPrincipal)
	return v
}

// DeveloperWalletFromContext remains for legacy SIWE tests and callers.
func DeveloperWalletFromContext(ctx context.Context) string {
	p := DeveloperPrincipalFromContext(ctx)
	if p.Kind == DeveloperPrincipalWallet {
		return p.Subject
	}
	return ""
}

func (s *Server) resolveDeveloperPrincipal(r *http.Request) (DeveloperPrincipal, error) {
	if token := strings.TrimSpace(r.Header.Get("X-Developer-Token")); token != "" {
		if strings.HasPrefix(token, "mkt1.") {
			if s.marketplaceAuth == nil {
				return DeveloperPrincipal{}, errors.New("marketplace developer auth not configured")
			}
			return s.marketplaceAuth.VerifyToken(token)
		}
		if s.devAuth == nil {
			return DeveloperPrincipal{}, errors.New("developer auth not configured")
		}
		wallet, err := s.devAuth.VerifyToken(token)
		if err != nil {
			return DeveloperPrincipal{}, err
		}
		wallet = strings.ToLower(wallet)
		return DeveloperPrincipal{Kind: DeveloperPrincipalWallet, Subject: wallet, Owner: wallet}, nil
	}
	if s.deps.DevMode {
		wallet := strings.TrimSpace(r.Header.Get("X-Developer-Wallet"))
		if wallet == "" {
			wallet = strings.TrimSpace(r.Header.Get("X-Developer-Address"))
		}
		if wallet != "" {
			wallet = strings.ToLower(wallet)
			return DeveloperPrincipal{Kind: DeveloperPrincipalWallet, Subject: wallet, Owner: wallet}, nil
		}
	}
	return DeveloperPrincipal{}, errors.New("developer authentication required")
}

// requireDeveloperAuth guards owner-scoped routes.
func (s *Server) requireDeveloperAuth() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			principal, err := s.resolveDeveloperPrincipal(r)
			if err != nil {
				writeAPIError(w, http.StatusUnauthorized, "unauthorized", err.Error(), nil)
				return
			}
			ctx := context.WithValue(r.Context(), developerPrincipalKey, principal)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
