// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol

package briefsettings

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"matrix/vault"
)

func vaultSessionFor(t *testing.T, user string) *vault.Session {
	t.Helper()
	kek := hex.EncodeToString([]byte("0123456789abcdef0123456789abcdef"))
	sess, err := vault.Boot(context.Background(), vault.Config{
		Required: true, DataDir: t.TempDir(), UserDID: user, KEKHex: kek,
	})
	if err != nil {
		t.Fatalf("vault boot: %v", err)
	}
	if !sess.Encrypting() {
		t.Fatal("expected an encrypting session")
	}
	return sess
}

// TestDefaultOff proves an absent file is the OFF state (req 11.1).
func TestDefaultOff(t *testing.T) {
	s := Open(t.TempDir())
	if s.Enabled() {
		t.Fatal("brief must default OFF")
	}
	if s.Paused() {
		t.Fatal("brief must default un-paused")
	}
}

// TestScheduleRoundTrip persists and reloads the schedule fields.
func TestScheduleRoundTrip(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	sc := Schedule{
		Timezone:       "America/New_York",
		DeliveryTime:   "07:30",
		Days:           []int{1, 2, 3, 4, 5},
		Length:         "standard",
		Sections:       []string{"news", "music"},
		ConversationID: "brief-conv",
	}
	if err := s.SetSchedule(sc); err != nil {
		t.Fatalf("SetSchedule: %v", err)
	}
	if err := s.SetEnabled(true, "alarm-1"); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}

	s2 := Open(dir)
	v := s2.View()
	if !v.Enabled || v.AlarmID != "alarm-1" {
		t.Fatalf("enabled/alarm not reloaded: %+v", v)
	}
	if v.Timezone != "America/New_York" || v.DeliveryTime != "07:30" {
		t.Fatalf("schedule not reloaded: %+v", v)
	}
	if len(v.Days) != 5 || len(v.Sections) != 2 || v.ConversationID != "brief-conv" {
		t.Fatalf("schedule fields not reloaded: %+v", v)
	}
}

// TestDisableClearsAlarmAndPause proves disable resets alarm id + pause.
func TestDisableClearsAlarmAndPause(t *testing.T) {
	s := Open(t.TempDir())
	_ = s.SetEnabled(true, "a-1")
	_ = s.SetPaused(true)
	if err := s.SetEnabled(false, ""); err != nil {
		t.Fatalf("SetEnabled(false): %v", err)
	}
	v := s.View()
	if v.Enabled || v.AlarmID != "" || v.Paused {
		t.Fatalf("disable did not reset: %+v", v)
	}
}

// TestDeliveredGuard proves single-delivery-per-local-date bookkeeping.
func TestDeliveredGuard(t *testing.T) {
	s := Open(t.TempDir())
	if s.AlreadyDelivered("2026-07-15") {
		t.Fatal("nothing delivered yet")
	}
	if err := s.MarkDelivered("2026-07-15"); err != nil {
		t.Fatalf("MarkDelivered: %v", err)
	}
	if !s.AlreadyDelivered("2026-07-15") {
		t.Fatal("delivery not recorded")
	}
	if s.AlreadyDelivered("2026-07-16") {
		t.Fatal("wrong-date guard tripped")
	}
}

// TestVaultRoundTrip proves the settings file is sealed yet reads back verbatim.
func TestVaultRoundTrip(t *testing.T) {
	dir := t.TempDir()
	user := "did:matrix:alice"
	sess := vaultSessionFor(t, user)

	s := Open(dir)
	s.SetVault(sess, user)
	if err := s.SetEnabled(true, "alarm-secret-1"); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	raw, err := os.ReadFile(s.path())
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if !vault.IsVault(raw) {
		t.Fatal("settings file not sealed")
	}
	if strings.Contains(string(raw), "alarm-secret-1") {
		t.Fatal("plaintext leaked into sealed file")
	}
	s2 := Open(dir)
	s2.SetVault(sess, user)
	if !s2.Enabled() || s2.AlarmID() != "alarm-secret-1" {
		t.Fatalf("round trip mismatch: enabled=%v alarm=%q", s2.Enabled(), s2.AlarmID())
	}
}

// TestVaultWrongUser proves another user's key cannot read the file.
func TestVaultWrongUser(t *testing.T) {
	dir := t.TempDir()
	s := Open(dir)
	s.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	if err := s.SetEnabled(true, "alarm-1"); err != nil {
		t.Fatalf("SetEnabled: %v", err)
	}
	s2 := Open(dir)
	s2.SetVault(vaultSessionFor(t, "did:matrix:mallory"), "did:matrix:mallory")
	if s2.Enabled() || s2.AlarmID() != "" {
		t.Fatalf("wrong user read sealed settings: %v %q", s2.Enabled(), s2.AlarmID())
	}
}

// TestVaultLegacyPlaintext proves a pre-vault plaintext file stays readable and
// the next write re-seals it.
func TestVaultLegacyPlaintext(t *testing.T) {
	dir := t.TempDir()
	plain := Open(dir)
	if err := plain.SetEnabled(true, "alarm-legacy"); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if b, _ := os.ReadFile(plain.path()); !json.Valid(b) {
		t.Fatal("seed should be plaintext JSON")
	}
	s := Open(dir)
	s.SetVault(vaultSessionFor(t, "did:matrix:alice"), "did:matrix:alice")
	if !s.Enabled() || s.AlarmID() != "alarm-legacy" {
		t.Fatalf("legacy unreadable: %v %q", s.Enabled(), s.AlarmID())
	}
	if err := s.SetPaused(true); err != nil {
		t.Fatalf("SetPaused: %v", err)
	}
	raw, _ := os.ReadFile(s.path())
	if !vault.IsVault(raw) {
		t.Fatal("not re-sealed after a vault write")
	}
}
