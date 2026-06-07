package types

// Stable error codes for agent retry logic.
const (
	CodeCompilerForgeFailed = "COMPILER_FORGE_FAILED"
	CodeCompilerSolcFailed  = "COMPILER_SOLC_FAILED"
	CodeTestForgeFailed     = "TEST_FORGE_FAILED"
	CodeTestAssertionFailed = "TEST_ASSERTION_FAILED"
	CodeChainNotFound       = "CHAIN_NOT_FOUND"
	CodeChainRPCFailed      = "CHAIN_RPC_FAILED"
	CodeSimulateFailed      = "SIMULATE_FAILED"
	CodeDeployFailed        = "DEPLOY_FAILED"
	CodeDeployIdempotent    = "DEPLOY_IDEMPOTENT_HIT"
	CodeCallFailed          = "CALL_FAILED"
	CodeArtifactNotFound    = "ARTIFACT_NOT_FOUND"
	CodeRegistryNotFound    = "REGISTRY_NOT_FOUND"
	CodeWalletDenied        = "WALLET_POLICY_DENIED"
	CodeWalletNotConfigured = "WALLET_NOT_CONFIGURED"
	CodeInvalidRequest      = "INVALID_REQUEST"
	CodeInternal            = "INTERNAL_ERROR"
)

// NewError builds a typed API error.
func NewError(code, message string, retry bool, details any) *Error {
	return &Error{Code: code, Message: message, Retry: retry, Details: details}
}

// Error implements the error interface.
func (e *Error) Error() string {
	if e == nil {
		return ""
	}
	return e.Code + ": " + e.Message
}

// OK wraps success data in an envelope.
func OK[T any](data T) Envelope[T] {
	return Envelope[T]{Ok: true, Data: data}
}

// Fail wraps an error in an envelope.
func Fail[T any](err *Error) Envelope[T] {
	return Envelope[T]{Ok: false, Error: err}
}
