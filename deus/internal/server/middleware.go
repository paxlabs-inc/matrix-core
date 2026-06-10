package server

import (
	"context"
	"errors"
	"net/http"
	"strings"
)

type ctxKey string

const developerWalletKey ctxKey = "developer_wallet"

// DeveloperWalletFromContext returns the authenticated developer wallet.
func DeveloperWalletFromContext(ctx context.Context) string {
	v, _ := ctx.Value(developerWalletKey).(string)
	return v
}

// resolveDeveloperWallet authenticates the developer identity on a request.
// A verified X-Developer-Token (minted by the SIWE flow in devauth.go) always
// wins. The bare X-Developer-Wallet / X-Developer-Address headers are pure
// trust-me assertions and are honored ONLY in dev mode (DEUS_DEV=1) — in
// production they previously let anyone act as any developer.
func resolveDeveloperWallet(r *http.Request, devMode bool, verifier *DeveloperAuth) (string, error) {
	if token := strings.TrimSpace(r.Header.Get("X-Developer-Token")); token != "" {
		if verifier == nil {
			return "", errors.New("developer auth not configured")
		}
		wallet, err := verifier.VerifyToken(token)
		if err != nil {
			return "", err
		}
		return strings.ToLower(wallet), nil
	}
	if devMode {
		wallet := strings.TrimSpace(r.Header.Get("X-Developer-Wallet"))
		if wallet == "" {
			wallet = strings.TrimSpace(r.Header.Get("X-Developer-Address"))
		}
		if wallet != "" {
			return strings.ToLower(wallet), nil
		}
	}
	return "", errors.New("developer authentication required")
}

// requireDeveloperAuth guards owner-scoped routes.
func (s *Server) requireDeveloperAuth() func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wallet, err := resolveDeveloperWallet(r, s.deps.DevMode, s.devAuth)
			if err != nil {
				writeAPIError(w, http.StatusUnauthorized, "unauthorized", err.Error(), nil)
				return
			}
			ctx := context.WithValue(r.Context(), developerWalletKey, wallet)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
