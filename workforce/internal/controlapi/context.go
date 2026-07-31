package controlapi

import (
	"context"
	"net/http"
)

type principalKey struct{}

func withPrincipal(ctx context.Context, principal Principal) context.Context {
	return context.WithValue(ctx, principalKey{}, principal)
}

func principalFrom(request *http.Request) Principal {
	principal, _ := request.Context().Value(principalKey{}).(Principal)
	return principal
}
