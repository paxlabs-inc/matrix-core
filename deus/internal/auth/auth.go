// Package auth resolves caller identity from agent bearer tokens.
package auth

import (
	"context"
	"fmt"
	"net/http"
	"strings"
)

// Caller is an authenticated agent identity.
type Caller struct {
	DID    string
	Wallet string
	Bearer string
}

type ctxKey string

const callerKey ctxKey = "caller"

// FromContext returns the authenticated caller.
func FromContext(ctx context.Context) (Caller, bool) {
	v, ok := ctx.Value(callerKey).(Caller)
	return v, ok
}

// Middleware authenticates agent callers (docs/05-api.md §5.1).
func Middleware(devMode bool) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			c, err := ResolveRequest(r, devMode)
			if err != nil {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusUnauthorized)
				_, _ = w.Write([]byte(`{"error":"unauthorized","message":"agent bearer required"}`))
				return
			}
			ctx := context.WithValue(r.Context(), callerKey, c)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ResolveRequest extracts caller identity from headers.
func ResolveRequest(r *http.Request, devMode bool) (Caller, error) {
	authz := strings.TrimSpace(r.Header.Get("Authorization"))
	if !strings.HasPrefix(strings.ToLower(authz), "bearer ") {
		if devMode {
			did := strings.TrimSpace(r.Header.Get("X-Caller-DID"))
			wallet := strings.TrimSpace(r.Header.Get("X-Caller-Wallet"))
			if did != "" {
				return Caller{DID: did, Wallet: wallet, Bearer: "dev"}, nil
			}
		}
		return Caller{}, fmt.Errorf("missing bearer")
	}
	token := strings.TrimSpace(authz[7:])
	if token == "" {
		return Caller{}, fmt.Errorf("empty bearer")
	}
	did := strings.TrimSpace(r.Header.Get("X-Caller-DID"))
	wallet := strings.TrimSpace(r.Header.Get("X-Caller-Wallet"))
	if devMode && did == "" && strings.HasPrefix(token, "did:") {
		did = token
	}
	if did == "" && devMode {
		did = "did:matrix:dev:caller"
	}
	if did == "" {
		return Caller{}, fmt.Errorf("caller did unresolved")
	}
	return Caller{DID: did, Wallet: wallet, Bearer: token}, nil
}
