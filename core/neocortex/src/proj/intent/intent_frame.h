#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <limits>
#include <optional>
#include <span>
#include <vector>

#include "core/error.h"
#include "log/frame.h"
#include "proj/store.h"

namespace neocortex::proj {

enum class LoopClosure : std::uint8_t {
  kDone = 0,
  kAbandoned = 1,
  kHandedOff = 2,
  kSuperseded = 3,
};

struct IntentObjective final {
  std::uint64_t set_lsn;
  std::vector<std::byte> content;

  bool operator==(const IntentObjective&) const = default;
};

struct OpenLoop final {
  std::uint64_t opened_lsn;
  std::vector<std::byte> loop_id;
  std::vector<std::byte> objective;

  bool operator==(const OpenLoop&) const = default;
};

struct ClosedLoop final {
  std::uint64_t opened_lsn;
  std::uint64_t closed_lsn;
  LoopClosure reason;
  std::vector<std::byte> loop_id;
  std::vector<std::byte> objective;
  std::vector<std::byte> cause;

  bool operator==(const ClosedLoop&) const = default;
};

struct IntentFrameView final {
  std::optional<IntentObjective> objective;
  std::vector<OpenLoop> open_loops;

  bool operator==(const IntentFrameView&) const = default;
};

struct IntentRebuildProgress final {
  std::uint64_t applied_lsn;
  std::size_t applied_frames;
  bool complete;
};

class IntentFrameProjection final {
 public:
  [[nodiscard]] static std::expected<void, Error> ApplyEvent(
      ProjectionStore& store, const log::Frame& frame);
  [[nodiscard]] static std::expected<IntentRebuildProgress, Error> Rebuild(
      ProjectionStore& store, std::span<const log::Frame> frames,
      bool reset = false,
      std::size_t maximum_frames = std::numeric_limits<std::size_t>::max());
  [[nodiscard]] static std::expected<IntentFrameView, Error> Read(
      const ReadSnapshot& snapshot, const log::ConversationId& conversation);
  [[nodiscard]] static std::expected<std::vector<ClosedLoop>, Error> ReadClosed(
      const ReadSnapshot& snapshot, const log::ConversationId& conversation);
};

}  // namespace neocortex::proj
