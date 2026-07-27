// Copyright © 2026 Paxlabs Inc. All rights reserved. SPDX-License-Identifier: LicenseRef-Paxlabs-Matrix-Protocol
// Contact · license@Paxeer.app · legal@Paxeer.app

package provider

import (
	"encoding/json"
	"errors"

	"matrix/neo/internal/runtime/protocol"
)

var ErrToolProtocol = errors.New("resurrection provider: incompatible tool protocol")

type Adapter interface {
	Name() string
	TranslateRequest(protocol.GenerationRequest) (json.RawMessage, error)
	TranslateResponse(json.RawMessage) (protocol.NormalizedGeneration, error)
	TranslateStreamEvent([]byte) (protocol.StreamChunk, error)
}

type GenerationFinalizer interface {
	FinalizeGeneration(protocol.NormalizedGeneration) (protocol.NormalizedGeneration, error)
}
