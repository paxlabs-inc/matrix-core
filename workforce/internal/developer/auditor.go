package developer

import (
	"context"
	"crypto/ed25519"
	"fmt"

	"centra/workforce/internal/audit"
	"centra/workforce/internal/contracts"
)

// Auditor is the workforced-owned Developer review boundary. It assembles and
// signs the exact current Developer evidence, then invokes the same fresh
// workforce-auditor process used for every other verdict.
type Auditor struct {
	runner     audit.Runner
	keyID      string
	privateKey ed25519.PrivateKey
}

// NewAuditor constructs a Developer Auditor dispatcher bound to one trusted
// kernel evidence-signing authority.
func NewAuditor(
	runner audit.Runner,
	keyID string,
	privateKey ed25519.PrivateKey,
) (*Auditor, error) {
	if len(privateKey) != ed25519.PrivateKeySize || keyID == "" ||
		runner.DeveloperAuthorityKeyID != keyID ||
		!ed25519.PublicKey(privateKey.Public().(ed25519.PublicKey)).Equal(
			runner.DeveloperAuthorityKey,
		) {
		return nil, fmt.Errorf("developer Auditor authority is invalid")
	}
	return &Auditor{
		runner: runner, keyID: keyID,
		privateKey: append(ed25519.PrivateKey(nil), privateKey...),
	}, nil
}

// Run signs the closed Developer extension and executes a fresh isolated
// Auditor process. No Project Brain store, transcript, or prior verdict crosses
// this boundary.
func (dispatcher *Auditor) Run(
	ctx context.Context,
	base contracts.VerdictPacket,
	packet AuditPacket,
) (audit.Decision, error) {
	if dispatcher == nil {
		return audit.Decision{}, fmt.Errorf("developer Auditor is unavailable")
	}
	verdict, err := packet.AttachToVerdict(
		base, dispatcher.keyID, dispatcher.privateKey,
	)
	if err != nil {
		return audit.Decision{}, err
	}
	return dispatcher.runner.Run(ctx, verdict)
}
