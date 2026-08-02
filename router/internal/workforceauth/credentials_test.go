package workforceauth

import (
	"crypto/ed25519"
	"encoding/base64"
	"testing"
)

func TestDeriverProducesStableSeparatedPerUserCredentials(t *testing.T) {
	deriver, err := New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	ownerA, err := deriver.OwnerToken("user-a")
	if err != nil {
		t.Fatal(err)
	}
	wakeA, err := deriver.WakeToken("user-a")
	if err != nil {
		t.Fatal(err)
	}
	ownerB, err := deriver.OwnerToken("user-b")
	if err != nil {
		t.Fatal(err)
	}
	ownerAAgain, err := deriver.OwnerToken("user-a")
	if err != nil {
		t.Fatal(err)
	}
	if ownerA != ownerAAgain || ownerA == wakeA || ownerA == ownerB {
		t.Fatalf("credentials are not stable and domain-separated")
	}

	runtimeEncoded, err := deriver.RuntimePrivateKey("user-a")
	if err != nil {
		t.Fatal(err)
	}
	runtimeKey, err := base64.RawURLEncoding.DecodeString(runtimeEncoded)
	if err != nil || len(runtimeKey) != ed25519.PrivateKeySize {
		t.Fatalf("runtime private key is invalid: bytes=%d err=%v", len(runtimeKey), err)
	}
	companyIssuerEncoded, err := deriver.CompanyIssuerPrivateKey("user-a")
	if err != nil {
		t.Fatal(err)
	}
	companyIssuerKey, err := base64.RawURLEncoding.DecodeString(companyIssuerEncoded)
	if err != nil || len(companyIssuerKey) != ed25519.PrivateKeySize {
		t.Fatalf("company issuer private key is invalid: bytes=%d err=%v", len(companyIssuerKey), err)
	}
	companyIssuerAgain, err := deriver.CompanyIssuerPrivateKey("user-a")
	if err != nil {
		t.Fatal(err)
	}
	companyIssuerB, err := deriver.CompanyIssuerPrivateKey("user-b")
	if err != nil {
		t.Fatal(err)
	}
	if companyIssuerEncoded != companyIssuerAgain ||
		companyIssuerEncoded == companyIssuerB ||
		companyIssuerEncoded == runtimeEncoded {
		t.Fatal("company issuer key is not stable and domain-separated")
	}
	ownerPublicEncoded, err := deriver.BootstrapOwnerPublicKey("user-a")
	if err != nil {
		t.Fatal(err)
	}
	ownerPublic, err := base64.RawURLEncoding.DecodeString(ownerPublicEncoded)
	if err != nil || len(ownerPublic) != ed25519.PublicKeySize {
		t.Fatalf("owner public key is invalid: bytes=%d err=%v", len(ownerPublic), err)
	}
}

func TestDeriverRejectsWeakRootAndMissingUser(t *testing.T) {
	if _, err := New("short"); err == nil {
		t.Fatal("weak root secret accepted")
	}
	deriver, err := New("0123456789abcdef0123456789abcdef")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := deriver.OwnerToken(" "); err == nil {
		t.Fatal("missing user accepted")
	}
}
