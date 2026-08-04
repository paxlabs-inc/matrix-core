package admin

import "testing"

func TestNeoReconcileEnvironment(t *testing.T) {
	token := "0123456789abcdef0123456789abcdef0123456789abcdef0123456789ABCDEF"
	environment, err := neoReconcileEnvironment(token)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{
		"NEO_MEMORY_SUBSTRATE": "neocortex",
		"NEO_NEOCORTEX_TOKEN":  token,
		"NEO_RUNTIME":          "LEGACY",
	}
	if len(environment) != len(want) {
		t.Fatalf("got %d variables, want %d", len(environment), len(want))
	}
	for name, value := range want {
		if environment[name] != value {
			t.Fatalf("%s=%q, want %q", name, environment[name], value)
		}
	}
}

func TestNeoReconcileEnvironmentRejectsInvalidToken(t *testing.T) {
	for _, token := range []string{
		"",
		"0123456789abcdef",
		"z123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
		"0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef00",
	} {
		if _, err := neoReconcileEnvironment(token); err == nil {
			t.Fatalf("token %q: expected error", token)
		}
	}
}
