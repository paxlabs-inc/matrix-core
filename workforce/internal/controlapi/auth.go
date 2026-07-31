package controlapi

import (
	"crypto/sha256"
	"fmt"
	"net/http"
	"strings"
)

type Authenticator interface {
	Authenticate(*http.Request) (Principal, error)
}

type StaticAuthenticator struct {
	principals map[[32]byte]Principal
}

func NewStaticAuthenticator(tokens map[string]Principal) (*StaticAuthenticator, error) {
	if len(tokens) == 0 {
		return nil, fmt.Errorf("controlapi: at least one owner token is required")
	}
	result := &StaticAuthenticator{principals: make(map[[32]byte]Principal, len(tokens))}
	for token, principal := range tokens {
		if strings.TrimSpace(token) == "" || principal.TenantID == "" ||
			principal.OrganizationID == "" || principal.OwnerID == "" {
			return nil, fmt.Errorf("controlapi: owner token binding is incomplete")
		}
		result.principals[sha256.Sum256([]byte(token))] = principal
	}
	return result, nil
}

func (auth *StaticAuthenticator) Authenticate(request *http.Request) (Principal, error) {
	header := request.Header.Get("Authorization")
	token, found := strings.CutPrefix(header, "Bearer ")
	if !found || strings.TrimSpace(token) == "" {
		return Principal{}, fmt.Errorf("controlapi: bearer authentication required")
	}
	principal, exists := auth.principals[sha256.Sum256([]byte(token))]
	if !exists {
		return Principal{}, fmt.Errorf("controlapi: bearer authentication failed")
	}
	return principal, nil
}
