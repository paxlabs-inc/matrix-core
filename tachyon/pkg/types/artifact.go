package types

// ArtifactGetRequest fetches a cached artifact by name.
type ArtifactGetRequest struct {
	ProjectID string `json:"project_id,omitempty"`
	Name      string `json:"name"`
}

// ArtifactGetResponse returns one artifact.
type ArtifactGetResponse struct {
	Artifact Artifact `json:"artifact"`
}

// RegistryLookupRequest resolves a prior deployment.
type RegistryLookupRequest struct {
	IdempotencyKey string `json:"idempotency_key"`
	ChainID        string `json:"chain_id"`
}

// RegistryLookupResponse returns deployment record.
type RegistryLookupResponse struct {
	Found      bool   `json:"found"`
	Address    string `json:"address,omitempty"`
	TxHash     string `json:"tx_hash,omitempty"`
	Contract   string `json:"contract,omitempty"`
	Confirmed  bool   `json:"confirmed,omitempty"`
}
