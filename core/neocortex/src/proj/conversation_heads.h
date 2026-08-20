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

struct RebuildProgress final {
  std::uint64_t applied_lsn;
  std::size_t applied_frames;
  bool complete;
};

struct ConversationRecord final {
  std::uint64_t lsn;
  log::EventKind kind;
  std::int64_t wall_timestamp_ns;
  log::ConversationId conversation;
  std::vector<std::byte> payload;

  bool operator==(const ConversationRecord&) const = default;
};

[[nodiscard]] std::expected<void, Error> ApplyConversationHead(
    ProjectionStore& store, const log::Frame& frame);

[[nodiscard]] std::expected<RebuildProgress, Error> RebuildConversationHeads(
    ProjectionStore& store, std::span<const log::Frame> frames, bool reset,
    std::size_t maximum_frames = static_cast<std::size_t>(-1));

[[nodiscard]] std::expected<std::vector<ConversationRecord>, Error>
ReadConversationRecords(
    const ReadSnapshot& snapshot, const log::ConversationId& conversation,
    std::size_t limit = std::numeric_limits<std::size_t>::max());

struct LatestRecordScan final {
  std::optional<ConversationRecord> record;
  std::uint64_t next_before_lsn;
  std::size_t scanned;
};

[[nodiscard]] std::expected<LatestRecordScan, Error>
ScanLatestConversationRecordOfKind(
    const ReadSnapshot& snapshot, const log::ConversationId& conversation,
    log::EventKind kind, std::uint64_t before_lsn,
    std::size_t maximum_scanned);

[[nodiscard]] std::expected<std::optional<ConversationRecord>, Error>
ReadConversationRecord(const ReadSnapshot& snapshot, std::uint64_t lsn);

}  // namespace neocortex::proj
