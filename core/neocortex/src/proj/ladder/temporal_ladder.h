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

enum class TemporalLevel : std::uint8_t {
  kMinute = 0,
  kHour = 1,
  kDay = 2,
  kWeek = 3,
};

struct TemporalWindow final {
  TemporalLevel level;
  std::int64_t start_ns;
  std::int64_t end_ns;
  std::uint64_t member_count;
};

struct TemporalRebuildProgress final {
  std::uint64_t applied_lsn;
  std::size_t applied_frames;
  bool complete;
};

class TemporalLadder final {
 public:
  [[nodiscard]] static std::expected<void, Error> ApplyEvent(
      ProjectionStore& store, const log::Frame& frame);
  [[nodiscard]] static std::expected<TemporalRebuildProgress, Error> Rebuild(
      ProjectionStore& store, std::span<const log::Frame> frames,
      bool reset = false,
      std::size_t maximum_frames = std::numeric_limits<std::size_t>::max());
  [[nodiscard]] static std::expected<std::vector<TemporalWindow>, Error>
  ListWindows(const ReadSnapshot& snapshot, TemporalLevel level,
              std::int64_t from_ns, std::int64_t to_ns,
              std::size_t limit = 1024);
  [[nodiscard]] static std::expected<std::vector<TemporalWindow>, Error>
  OpenWindow(const ReadSnapshot& snapshot, const TemporalWindow& window,
             std::size_t limit = 1024);
  [[nodiscard]] static std::expected<std::vector<std::uint64_t>, Error>
  ResolveMembers(const ReadSnapshot& snapshot, const TemporalWindow& window,
                 std::size_t limit);
};

}  // namespace neocortex::proj
