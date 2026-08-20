#include "proj/conversation_heads.h"

#include <algorithm>
#include <array>
#include <bit>
#include <optional>

namespace neocortex::proj {
namespace {

constexpr std::byte kEventPrefix{0x45};
constexpr std::byte kLsnIndexPrefix{0x4c};

std::array<std::byte, 17> EncodeHead(const log::Frame& frame) {
  std::array<std::byte, 17> value{};
  for (std::size_t index = 0; index < 8; ++index) {
    value[index] =
        static_cast<std::byte>((frame.header.lsn >> (index * 8U)) & 0xffU);
    value[index + 8] = static_cast<std::byte>(
        (static_cast<std::uint64_t>(frame.header.wall_timestamp_ns) >>
         (index * 8U)) &
        0xffU);
  }
  value[16] = static_cast<std::byte>(frame.header.kind);
  return value;
}

void AppendU64Big(std::vector<std::byte>& output, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    output.push_back(
        static_cast<std::byte>((value >> ((7U - index) * 8U)) & 0xffU));
  }
}

void AppendU64(std::vector<std::byte>& output, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    output.push_back(
        static_cast<std::byte>((value >> (index * 8U)) & 0xffU));
  }
}

std::vector<std::byte> EventPrefix(
    const log::ConversationId& conversation) {
  std::vector<std::byte> key;
  key.reserve(17);
  key.push_back(kEventPrefix);
  key.insert(key.end(), conversation.bytes.begin(), conversation.bytes.end());
  return key;
}

std::vector<std::byte> EventKey(
    const log::ConversationId& conversation, std::uint64_t lsn) {
  auto key = EventPrefix(conversation);
  AppendU64Big(key, lsn);
  return key;
}

std::vector<std::byte> LsnIndexKey(std::uint64_t lsn) {
  std::vector<std::byte> key;
  key.push_back(kLsnIndexPrefix);
  AppendU64Big(key, lsn);
  return key;
}

std::vector<std::byte> EncodeRecord(const log::Frame& frame) {
  std::vector<std::byte> value;
  value.reserve(frame.sealed_payload.size() + 9U);
  value.push_back(static_cast<std::byte>(frame.header.kind));
  AppendU64(value, std::bit_cast<std::uint64_t>(
                       frame.header.wall_timestamp_ns));
  value.insert(value.end(), frame.sealed_payload.begin(),
               frame.sealed_payload.end());
  return value;
}

std::expected<ConversationRecord, Error> DecodeRecord(const KeyValue& item) {
  if (item.key.size() != 25 || item.key[0] != kEventPrefix ||
      item.value.size() < 9) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
  }
  std::uint64_t lsn = 0;
  for (std::size_t index = 0; index < 8; ++index) {
    lsn = (lsn << 8U) |
          std::to_integer<std::uint64_t>(item.key[17U + index]);
  }
  const auto raw_kind = std::to_integer<std::uint8_t>(item.value[0]);
  if (lsn == 0 || !log::IsEventKind(raw_kind)) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
  }
  std::uint64_t timestamp = 0;
  for (std::size_t index = 0; index < 8; ++index) {
    timestamp |= std::to_integer<std::uint64_t>(item.value[index + 1U])
                 << (index * 8U);
  }
  log::ConversationId conversation{};
  std::copy_n(item.key.begin() + 1, conversation.bytes.size(),
              conversation.bytes.begin());
  return ConversationRecord{
      .lsn = lsn,
      .kind = static_cast<log::EventKind>(raw_kind),
      .wall_timestamp_ns = std::bit_cast<std::int64_t>(timestamp),
      .conversation = conversation,
      .payload = std::vector<std::byte>(item.value.begin() + 9,
                                       item.value.end()),
  };
}

}  // namespace

std::expected<void, Error> ApplyConversationHead(ProjectionStore& store,
                                                  const log::Frame& frame) {
  const auto head = EncodeHead(frame);
  const auto event_key = EventKey(frame.header.conversation, frame.header.lsn);
  const auto record = EncodeRecord(frame);
  const auto index_key = LsnIndexKey(frame.header.lsn);
  const std::array<Mutation, 3> mutations = {
      Mutation{.kind = MutationKind::kPut,
               .key = frame.header.conversation.bytes,
               .value = head},
      Mutation{.kind = MutationKind::kPut,
               .key = event_key,
               .value = record},
      Mutation{.kind = MutationKind::kPut,
               .key = index_key,
               .value = event_key},
  };
  return store.Apply(ProjectionId::kConversationHeads, frame.header.lsn,
                     mutations);
}

std::expected<RebuildProgress, Error> RebuildConversationHeads(
    ProjectionStore& store, std::span<const log::Frame> frames, bool reset,
    std::size_t maximum_frames) {
  if (reset) {
    auto reset_result = store.Reset(ProjectionId::kConversationHeads);
    if (!reset_result) {
      return std::unexpected(reset_result.error());
    }
  }
  std::uint64_t checkpoint = 0;
  {
    auto snapshot = store.BeginSnapshot();
    if (!snapshot) {
      return std::unexpected(snapshot.error());
    }
    auto read_checkpoint = snapshot->Checkpoint(ProjectionId::kConversationHeads);
    if (!read_checkpoint) {
      return std::unexpected(read_checkpoint.error());
    }
    checkpoint = *read_checkpoint;
  }
  if (checkpoint > frames.size()) {
    return std::unexpected(
        Error{ErrorCode::kProjectionCheckpoint, 0, checkpoint, frames.size()});
  }
  const auto remaining = frames.size() - static_cast<std::size_t>(checkpoint);
  const auto apply_count = std::min(remaining, maximum_frames);
  const auto begin = static_cast<std::size_t>(checkpoint);
  for (std::size_t index = begin; index < begin + apply_count; ++index) {
    if (frames[index].header.lsn != static_cast<std::uint64_t>(index) + 1U) {
      return std::unexpected(Error{ErrorCode::kSequenceViolation, 0,
                                   frames[index].header.lsn, index});
    }
    auto applied = ApplyConversationHead(store, frames[index]);
    if (!applied) {
      return std::unexpected(applied.error());
    }
  }
  const auto applied_lsn = checkpoint + static_cast<std::uint64_t>(apply_count);
  return RebuildProgress{.applied_lsn = applied_lsn,
                         .applied_frames = apply_count,
                         .complete = applied_lsn == frames.size()};
}

std::expected<std::vector<ConversationRecord>, Error> ReadConversationRecords(
    const ReadSnapshot& snapshot, const log::ConversationId& conversation,
    std::size_t limit) {
  if (limit == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto items = snapshot.ScanPrefixReverse(ProjectionId::kConversationHeads,
                                          EventPrefix(conversation), limit);
  if (!items) {
    return std::unexpected(items.error());
  }
  std::vector<ConversationRecord> records;
  records.reserve(items->size());
  for (const auto& item : *items) {
    auto record = DecodeRecord(item);
    if (!record) {
      return std::unexpected(record.error());
    }
    records.push_back(std::move(*record));
  }
  std::reverse(records.begin(), records.end());
  return records;
}

std::expected<LatestRecordScan, Error> ScanLatestConversationRecordOfKind(
    const ReadSnapshot& snapshot, const log::ConversationId& conversation,
    log::EventKind kind, std::uint64_t before_lsn,
    std::size_t maximum_scanned) {
  if (before_lsn == 0 || maximum_scanned == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  constexpr std::size_t kChunk = 64;
  const auto prefix = EventPrefix(conversation);
  LatestRecordScan scan{.record = std::nullopt,
                        .next_before_lsn = before_lsn,
                        .scanned = 0};
  while (scan.scanned < maximum_scanned) {
    const auto chunk_limit =
        std::min<std::size_t>(kChunk, maximum_scanned - scan.scanned);
    const auto before_key = EventKey(conversation, scan.next_before_lsn);
    auto items = snapshot.ScanPrefixReverseBefore(
        ProjectionId::kConversationHeads, prefix, before_key, chunk_limit);
    if (!items) {
      return std::unexpected(items.error());
    }
    if (items->empty()) {
      scan.next_before_lsn = 0;
      return scan;
    }
    for (const auto& item : *items) {
      ++scan.scanned;
      if (item.key.size() != 25 || item.value.empty()) {
        return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
      }
      std::uint64_t lsn = 0;
      for (std::size_t index = 0; index < 8; ++index) {
        lsn = (lsn << 8U) |
              std::to_integer<std::uint64_t>(item.key[17U + index]);
      }
      scan.next_before_lsn = lsn;
      if (std::to_integer<std::uint8_t>(item.value[0]) !=
          static_cast<std::uint8_t>(kind)) {
        continue;
      }
      auto record = DecodeRecord(item);
      if (!record) {
        return std::unexpected(record.error());
      }
      scan.record = std::move(*record);
      return scan;
    }
    if (items->size() < chunk_limit) {
      scan.next_before_lsn = 0;
      return scan;
    }
  }
  return scan;
}

std::expected<std::optional<ConversationRecord>, Error> ReadConversationRecord(
    const ReadSnapshot& snapshot, std::uint64_t lsn) {
  if (lsn == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto key = snapshot.Get(ProjectionId::kConversationHeads, LsnIndexKey(lsn));
  if (!key) {
    return std::unexpected(key.error());
  }
  auto maybe_key = std::move(*key);
  if (!maybe_key.has_value()) {
    return std::optional<ConversationRecord>{};
  }
  auto value = snapshot.Get(ProjectionId::kConversationHeads, *maybe_key);
  if (!value) {
    return std::unexpected(value.error());
  }
  auto maybe_value = std::move(*value);
  if (!maybe_value.has_value()) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0, lsn});
  }
  auto record = DecodeRecord(
      KeyValue{.key = std::move(*maybe_key), .value = std::move(*maybe_value)});
  if (!record || record->lsn != lsn) {
    return std::unexpected(record ? Error{ErrorCode::kInvariantViolation, 0, lsn}
                                  : record.error());
  }
  return std::optional<ConversationRecord>(std::move(*record));
}

}  // namespace neocortex::proj
