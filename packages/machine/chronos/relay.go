// Copyright © 2026 Sidiora Labs. All rights reserved. SPDX-License-Identifier: LicenseRef-Centra-ai-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package chronos

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"strings"
	"time"
)

type RelayLease struct {
	MachineID string    `json:"machine_id"`
	NextWake  time.Time `json:"next_wake"`
	Nonce     string    `json:"nonce"`
	Revision  uint64    `json:"revision"`
}

func NewRelayLease(machineGene string, nextWake time.Time, revision uint64) (RelayLease, error) {
	if strings.TrimSpace(machineGene) == "" || nextWake.IsZero() || revision == 0 {
		return RelayLease{}, fmt.Errorf("local chronos: relay lease requires machine Gene, next wake, and revision")
	}
	digest := sha256.Sum256([]byte("matrix-chronos-relay-v1\x00" + machineGene))
	var nonce [24]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return RelayLease{}, err
	}
	return RelayLease{MachineID: base64.RawURLEncoding.EncodeToString(digest[:]),
		NextWake: nextWake.UTC(), Nonce: base64.RawURLEncoding.EncodeToString(nonce[:]), Revision: revision}, nil
}

func (lease RelayLease) Validate() error {
	machineID, machineErr := base64.RawURLEncoding.DecodeString(lease.MachineID)
	nonce, nonceErr := base64.RawURLEncoding.DecodeString(lease.Nonce)
	if machineErr != nil || len(machineID) != 32 || nonceErr != nil || len(nonce) != 24 || lease.NextWake.IsZero() || lease.Revision == 0 {
		return fmt.Errorf("local chronos: invalid opaque relay lease")
	}
	zero(machineID)
	zero(nonce)
	return nil
}
