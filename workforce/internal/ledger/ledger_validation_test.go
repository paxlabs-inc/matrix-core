package ledger

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"matrix/workforce/internal/contracts"
)

func TestLedgerValidationAndConstruction(t *testing.T) {
	store, userVault, _ := integrationStore(t, "validation-construction")
	if _, err := New(nil, userVault, store.tenantID, integrationNow); err == nil {
		t.Fatal("New accepted a nil pool")
	}
	if _, err := New(integrationPool, nil, store.tenantID, integrationNow); err == nil {
		t.Fatal("New accepted a nil Vault")
	}
	if _, err := New(integrationPool, userVault, " ", integrationNow); err == nil {
		t.Fatal("New accepted an empty tenant")
	}
	if _, err := New(integrationPool, userVault, "tenant-other", integrationNow); err == nil {
		t.Fatal("New accepted a Vault for another tenant")
	}
	if _, err := New(integrationPool, userVault, store.tenantID, nil); err == nil {
		t.Fatal("New accepted a nil time source")
	}
	if _, err := New(integrationPool, userVault, " "+store.tenantID+" ", integrationNow); err != nil {
		t.Fatalf("New rejected a trimmed matching tenant: %v", err)
	}

	for _, action := range []AccessAction{
		AccessDelivery, AccessOpen, AccessCitation, AccessDerivation,
	} {
		if !action.Valid() {
			t.Fatalf("valid access action %q rejected", action)
		}
	}
	if AccessAction("unknown").Valid() {
		t.Fatal("unknown access action accepted")
	}
	for _, state := range []ReconciliationState{
		ReconciliationApplied, ReconciliationRejected, ReconciliationEscalated,
	} {
		if !state.Valid() {
			t.Fatalf("valid reconciliation state %q rejected", state)
		}
	}
	if ReconciliationState("unknown").Valid() {
		t.Fatal("unknown reconciliation state accepted")
	}

	for _, test := range []struct {
		name  string
		value string
		ok    bool
	}{
		{name: "valid", value: "a-Z_0.9:x", ok: true},
		{name: "blank", value: " "},
		{name: "long", value: strings.Repeat("a", 129)},
		{name: "invalid", value: "not/valid"},
	} {
		err := validateToken("token", test.value)
		if (err == nil) != test.ok {
			t.Fatalf("validateToken(%q) error = %v, want ok=%t", test.value, err, test.ok)
		}
	}

	for _, test := range []struct {
		name string
		now  func() time.Time
		ok   bool
	}{
		{name: "UTC", now: integrationNow, ok: true},
		{name: "zero", now: func() time.Time { return time.Time{} }},
		{name: "non UTC", now: func() time.Time {
			return integrationNow().In(time.FixedZone("test", 3600))
		}},
	} {
		store.now = test.now
		_, err := store.currentTime()
		if (err == nil) != test.ok {
			t.Fatalf("currentTime %s error = %v, want ok=%t", test.name, err, test.ok)
		}
	}

	for _, code := range []string{"40001", "40P01"} {
		if !retryableTransaction(&pgconn.PgError{Code: code}) {
			t.Fatalf("database error %s was not retryable", code)
		}
	}
	if retryableTransaction(&pgconn.PgError{Code: "23505"}) ||
		retryableTransaction(errors.New("plain error")) {
		t.Fatal("non-transaction error was retryable")
	}
}

func TestLedgerPointerAndAuthorizationHelpers(t *testing.T) {
	department := contracts.DepartmentID("department-developer")
	seat := contracts.SeatID("seat-reader")
	project := contracts.ProjectID("project-one")
	if optionalDepartmentID(nil) != nil || optionalSeatID(nil) != nil ||
		optionalProjectID(nil) != nil || optionalRecordID(nil) != nil {
		t.Fatal("nil optional IDs did not remain nil")
	}
	if got := optionalDepartmentID(&department); got == nil || *got != string(department) {
		t.Fatalf("optional department = %#v", got)
	}
	if got := optionalSeatID(&seat); got == nil || *got != string(seat) {
		t.Fatalf("optional seat = %#v", got)
	}
	if got := optionalProjectID(&project); got == nil || *got != string(project) {
		t.Fatalf("optional project = %#v", got)
	}
	recordID := contracts.RecordID("record-one")
	if got := optionalRecordID(&recordID); got == nil || *got != string(recordID) {
		t.Fatalf("optional record = %#v", got)
	}

	if departmentPointer(pgtype.Text{}) != nil || seatPointer(pgtype.Text{}) != nil ||
		projectPointer(pgtype.Text{}) != nil {
		t.Fatal("invalid PostgreSQL text became an ID")
	}
	if got := departmentPointer(pgtype.Text{String: string(department), Valid: true}); got == nil || *got != department {
		t.Fatalf("department pointer = %#v", got)
	}
	if got := seatPointer(pgtype.Text{String: string(seat), Valid: true}); got == nil || *got != seat {
		t.Fatalf("seat pointer = %#v", got)
	}
	if got := projectPointer(pgtype.Text{String: string(project), Valid: true}); got == nil || *got != project {
		t.Fatalf("project pointer = %#v", got)
	}
	if !optionalTextEqual(pgtype.Text{}, nil) ||
		optionalTextEqual(pgtype.Text{String: "x", Valid: true}, nil) ||
		!optionalTextEqual(pgtype.Text{String: "x", Valid: true}, stringPointer("x")) ||
		optionalTextEqual(pgtype.Text{}, stringPointer("x")) {
		t.Fatal("optional PostgreSQL text equality is incorrect")
	}

	now := integrationNow()
	base := recordMetadata{
		organizationID: "org-one",
		departmentID:   &department,
		accessSeatID:   &seat,
		projectID:      &project,
		purpose:        "purpose",
		classification: contracts.ClassificationOrganization,
	}
	grant := AccessGrant{
		OrganizationID:  base.organizationID,
		SeatID:          seat,
		DepartmentID:    &department,
		ProjectID:       &project,
		Purpose:         base.purpose,
		Classifications: []contracts.Classification{contracts.ClassificationOrganization},
		Restricted:      true,
		ExpiresAt:       now.Add(time.Hour),
	}
	if err := authorize(base, grant, now); err != nil {
		t.Fatalf("organization authorization: %v", err)
	}
	for _, mutate := range []func(*AccessGrant){
		func(value *AccessGrant) { value.OrganizationID = "org-other" },
		func(value *AccessGrant) { value.SeatID = "" },
		func(value *AccessGrant) { value.Purpose = "other" },
		func(value *AccessGrant) { value.ExpiresAt = value.ExpiresAt.In(time.FixedZone("test", 3600)) },
		func(value *AccessGrant) { value.ExpiresAt = now },
		func(value *AccessGrant) { value.Classifications = nil },
	} {
		changed := grant
		mutate(&changed)
		if err := authorize(base, changed, now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("invalid common grant error = %v, want ErrNotFound", err)
		}
	}
	for _, classification := range []contracts.Classification{
		contracts.ClassificationDepartment,
		contracts.ClassificationSeat,
		contracts.ClassificationProject,
		contracts.ClassificationRestricted,
	} {
		metadata := base
		metadata.classification = classification
		scoped := grant
		scoped.Classifications = []contracts.Classification{classification}
		if err := authorize(metadata, scoped, now); err != nil {
			t.Fatalf("%s authorization: %v", classification, err)
		}
		switch classification {
		case contracts.ClassificationDepartment:
			scoped.DepartmentID = nil
		case contracts.ClassificationSeat:
			scoped.SeatID = "seat-other"
		case contracts.ClassificationProject:
			scoped.ProjectID = nil
		case contracts.ClassificationRestricted:
			scoped.Restricted = false
		}
		if err := authorize(metadata, scoped, now); !errors.Is(err, ErrNotFound) {
			t.Fatalf("%s mismatch error = %v, want ErrNotFound", classification, err)
		}
	}
	if !allowsClassification(
		[]contracts.Classification{contracts.ClassificationSeat},
		contracts.ClassificationSeat,
	) || allowsClassification(nil, contracts.ClassificationSeat) {
		t.Fatal("classification membership check is incorrect")
	}
}

func TestLedgerCryptoRejectsEveryIntegrityBoundary(t *testing.T) {
	store, _, scope := integrationStore(t, "crypto-boundaries")
	record := integrationRecord(scope, "record", contracts.RecordFinding, nil)
	prepared, err := store.prepareRecord(record)
	if err != nil {
		t.Fatalf("prepare record: %v", err)
	}
	if _, err := store.openRecord(
		store.recordAD(record),
		prepared.sealed,
		strings.Repeat("0", sha256.Size*2),
	); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("wrong canonical hash error = %v, want ErrIntegrity", err)
	}
	arbitrary := []byte(`{"not":"a canonical workforce record"}`)
	sum := sha256.Sum256(arbitrary)
	sealed, err := store.vault.SealRecord(store.recordAD(record), arbitrary)
	if err != nil {
		t.Fatalf("seal arbitrary bytes: %v", err)
	}
	if _, err := store.openRecord(
		store.recordAD(record),
		sealed,
		hex.EncodeToString(sum[:]),
	); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("invalid canonical bytes error = %v, want ErrIntegrity", err)
	}
	tampered := append([]byte(nil), prepared.sealed...)
	tampered[len(tampered)-1] ^= 1
	if _, err := store.openRecord(
		store.recordAD(record),
		tampered,
		prepared.canonicalHash.Digest,
	); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("tampered Vault bytes error = %v, want ErrIntegrity", err)
	}

	invalid := record
	invalid.ID = ""
	if _, err := store.prepareRecord(invalid); err == nil {
		t.Fatal("prepareRecord accepted an invalid contract")
	}
	metadata := recordMetadata{
		organizationID: record.OrganizationID,
		recordID:       record.ID,
		kind:           record.Kind,
		authorSeatID:   "seat-other",
		projectID:      record.ProjectID,
		schemaVersion:  record.SchemaVersion,
		canonicalHash:  prepared.canonicalHash.Digest,
		sealed:         prepared.sealed,
	}
	if _, err := store.decryptMetadata(metadata); !errors.Is(err, ErrIntegrity) {
		t.Fatalf("index mismatch error = %v, want ErrIntegrity", err)
	}
	if got := store.recordADFor(
		record.OrganizationID,
		record.ID,
		record.Kind,
		nil,
		record.SchemaVersion,
	); !strings.Contains(got.Stream, "/-/") {
		t.Fatalf("nil-project AAD stream = %q", got.Stream)
	}
}

func stringPointer(value string) *string {
	return &value
}
