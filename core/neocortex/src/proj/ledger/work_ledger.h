#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <limits>
#include <span>
#include <vector>

#include "core/error.h"
#include "log/frame.h"
#include "proj/store.h"

namespace neocortex::proj {

enum class WorkKind : std::uint8_t {
  kToolCall = 0,
  kEffect = 1,
};

enum class WorkState : std::uint8_t {
  kDispatched = 0,
  kCommitted = 1,
  kReturned = 2,
  kOutcomeUnknown = 3,
};

struct WorkItem final {
  WorkKind kind;
  WorkState state;
  std::uint64_t tool_call_lsn;
  std::uint64_t state_lsn;
  std::vector<std::byte> call_id;
  std::vector<std::byte> tool_name;
  std::vector<std::byte> arguments;
  std::vector<std::byte> effect_id;
  std::vector<std::byte> detail;
  bool requires_reconciliation;

  bool operator==(const WorkItem&) const = default;
};

struct LedgerRebuildProgress final {
  std::uint64_t applied_lsn;
  std::size_t applied_frames;
  bool complete;
};

class WorkLedgerProjection final {
 public:
  [[nodiscard]] static std::expected<void, Error> ApplyEvent(
      ProjectionStore& store, const log::Frame& frame);
  [[nodiscard]] static std::expected<LedgerRebuildProgress, Error> Rebuild(
      ProjectionStore& store, std::span<const log::Frame> frames,
      bool reset = false,
      std::size_t maximum_frames = std::numeric_limits<std::size_t>::max());
  [[nodiscard]] static std::expected<std::vector<WorkItem>, Error>
  ReadConversation(const ReadSnapshot& snapshot,
                   const log::ConversationId& conversation,
                   std::size_t limit = 4096);
};

}  // namespace neocortex::proj
