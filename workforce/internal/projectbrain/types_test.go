package projectbrain

import (
	"crypto/ed25519"
	"crypto/rand"
	"testing"
)

func TestEngineeringRecord_RejectsSessionMemoryAndSelfVerification(t *testing.T) {
	record, authorPrivate, _, _, _ := signedRecord(t, "validation")
	if err := record.Validate(); err != nil {
		t.Fatalf("valid record rejected: %v", err)
	}

	invalidOrigin := record
	invalidOrigin.Proposal.Origin = Origin("session_transcript")
	if err := SignProposal(&invalidOrigin.Proposal, "author-key", authorPrivate); err == nil {
		t.Fatal("proposal signer accepted session transcript origin")
	}

	selfVerified := record
	selfVerified.Verification.VerifierSeatID = selfVerified.Proposal.AuthorSeatID
	selfVerified.Verification.ProposalHash, _ = proposalHash(selfVerified.Proposal)
	if err := SignVerification(&selfVerified.Verification, "author-key", authorPrivate); err != nil {
		t.Fatal(err)
	}
	if err := selfVerified.Validate(); err == nil {
		t.Fatal("engineering record accepted author self-verification")
	}
}

func TestEngineeringRecord_RejectsSourceClaimOutsideSnapshot(t *testing.T) {
	record, authorPrivate, verifierPrivate, _, _ := signedRecord(t, "stale-claim")
	record.Proposal.Content.Claims[0].Files[0].Path = "other.go"
	if err := SignProposal(&record.Proposal, "author-key", authorPrivate); err != nil {
		t.Fatal(err)
	}
	record.Verification.ProposalHash, _ = proposalHash(record.Proposal)
	if err := SignVerification(&record.Verification, "verifier-key", verifierPrivate); err != nil {
		t.Fatal(err)
	}
	if err := record.Validate(); err == nil {
		t.Fatal("engineering record accepted source claim absent from snapshot")
	}
}

func TestRecordSignatures_RejectTamperAndWrongAuthority(t *testing.T) {
	record, _, _, authorPublic, verifierPublic := signedRecord(t, "signature")
	if err := verifyRecordSignatures(record, authorPublic, verifierPublic); err != nil {
		t.Fatalf("real signatures rejected: %v", err)
	}
	record.Proposal.Content.Summary = "tampered"
	if err := verifyRecordSignatures(record, authorPublic, verifierPublic); err == nil {
		t.Fatal("proposal tamper passed signature verification")
	}
	otherPublic, _, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	record, _, _, _, _ = signedRecord(t, "wrong-authority")
	if err := verifyRecordSignatures(record, otherPublic, verifierPublic); err == nil {
		t.Fatal("wrong author authority passed signature verification")
	}
}
