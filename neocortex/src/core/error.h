#pragma once

#include <cstdint>

namespace neocortex {

enum class ErrorCode : std::uint8_t {
  kInvalidArgument,
  kInvalidLength,
  kInvalidKind,
  kSequenceViolation,
  kOpenFailed,
  kReadFailed,
  kWriteFailed,
  kSyncFailed,
  kCloseFailed,
  kTruncated,
  kChecksumMismatch,
  kInteriorCorruption,
  kManifestCorrupt,
  kBackendUnavailable,
  kSegmentFull,
  kProofInvalid,
  kSignatureInvalid,
  kCheckpointMismatch,
  kAlreadyExists,
  kWriterViolation,
  kProcessKilled,
  kDurabilityFailure,
  kProjectionCheckpoint,
  kMapFull,
  kCryptoAuthentication,
  kKeyDestroyed,
  kLegacyPlaintext,
  kSchemaInvalid,
  kSchemaVersion,
  kForbiddenKind,
  kOrderingViolation,
  kBeliefWriteGate,
  kBeliefIdConflict,
  kBeliefNotFound,
  kBeliefCorrupt,
  kNegativeExistenceUncorroborated,
  kEntityIndexCorrupt,
  kVectorIndexCorrupt,
  kLexicalIndexCorrupt,
  kTemporalLadderCorrupt,
  kIntentFrameCorrupt,
  kLoopNotFound,
  kWorkLedgerCorrupt,
  kWorkItemNotFound,
  kInvariantViolation,
  kProtocolInvalid,
  kProtocolVersion,
  kCapabilityDenied,
  kCapacityExceeded,
  kOperationUnavailable,
  kIdempotencyConflict,
};

struct Error final {
  ErrorCode code;
  int system_error;
  std::uint64_t lsn = 0;
  std::uint64_t offset = 0;
};

}  // namespace neocortex
