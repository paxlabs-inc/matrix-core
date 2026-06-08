package server

import (
	"context"
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

// DevDeveloperAuth resolves developer identity for Phase 1.
// Production uses wallet-signed requests (docs/05-api.md §5.1).
func DevDeveloperAuth(devMode bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			wallet := strings.TrimSpace(r.Header.Get("X-Developer-Wallet"))
			if wallet == "" && devMode {
				wallet = strings.TrimSpace(r.Header.Get("X-Developer-Address"))
			}
			if wallet == "" {
				writeAPIError(w, http.StatusUnauthorized, "unauthorized", "developer wallet required", nil)
				return
			}
			ctx := context.WithValue(r.Context(), developerWalletKey, strings.ToLower(wallet))
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}
