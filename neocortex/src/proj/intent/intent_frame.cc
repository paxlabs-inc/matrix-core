#include "proj/intent/intent_frame.h"

#include <algorithm>
#include <array>
#include <optional>
#include <utility>

#include "schema/events.h"

namespace neocortex::proj {
namespace {

constexpr std::byte kObjectivePrefix{0x49};
constexpr std::byte kOpenPrefix{0x4f};
constexpr std::byte kClosedPrefix{0x43};
constexpr std::byte kLoopIndexPrefix{0x4c};
constexpr std::size_t kMaximumOpenLoops = 4096;

struct OwnedMutation final {
  MutationKind kind;
  std::vector<std::byte> key;
  std::vector<std::byte> value;
};

void AppendU64(std::vector<std::byte>& output, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    output.push_back(
        static_cast<std::byte>((value >> (index * 8U)) & 0xffU));
  }
}

std::expected<std::uint64_t, Error> ReadU64(std::span<const std::byte> bytes,
                                           std::size_t& offset) {
  if (bytes.size() - std::min(bytes.size(), offset) < 8) {
    return std::unexpected(Error{ErrorCode::kIntentFrameCorrupt, 0});
  }
  std::uint64_t value = 0;
  for (std::size_t index = 0; index < 8; ++index) {
    value |= std::to_integer<std::uint64_t>(bytes[offset + index])
             << (index * 8U);
  }
  offset += 8;
  return value;
}

void AppendBytes(std::vector<std::byte>& output,
                 std::span<const std::byte> bytes) {
  AppendU64(output, bytes.size());
  output.insert(output.end(), bytes.begin(), bytes.end());
}

std::expected<std::vector<std::byte>, Error> ReadBytes(
    std::span<const std::byte> bytes, std::size_t& offset) {
  auto length = ReadU64(bytes, offset);
  if (!length || *length > bytes.size() - std::min(bytes.size(), offset)) {
    return std::unexpected(Error{ErrorCode::kIntentFrameCorrupt, 0});
  }
  const auto size = static_cast<std::size_t>(*length);
  std::vector<std::byte> value(bytes.begin() + static_cast<std::ptrdiff_t>(offset),
                               bytes.begin() +
                                   static_cast<std::ptrdiff_t>(offset + size));
  offset += size;
  return value;
}

std::vector<std::byte> Key(std::byte prefix,
                           const log::ConversationId& conversation,
                           std::span<const std::byte> suffix = {}) {
  std::vector<std::byte> key;
  key.reserve(17U + suffix.size());
  key.push_back(prefix);
  key.insert(key.end(), conversation.bytes.begin(), conversation.bytes.end());
  key.insert(key.end(), suffix.begin(), suffix.end());
  return key;
}

std::vector<std::byte> IndexKey(std::span<const std::byte> loop_id) {
  std::vector<std::byte> key;
  key.reserve(loop_id.size() + 1U);
  key.push_back(kLoopIndexPrefix);
  key.insert(key.end(), loop_id.begin(), loop_id.end());
  return key;
}

std::vector<std::byte> EncodeObjective(std::uint64_t lsn,
                                       std::span<const std::byte> content) {
  std::vector<std::byte> value;
  AppendU64(value, lsn);
  AppendBytes(value, content);
  return value;
}

std::expected<IntentObjective, Error> DecodeObjective(
    std::span<const std::byte> value) {
  std::size_t offset = 0;
  auto lsn = ReadU64(value, offset);
  auto content = ReadBytes(value, offset);
  if (!lsn || !content || offset != value.size()) {
    return std::unexpected(Error{ErrorCode::kIntentFrameCorrupt, 0});
  }
  return IntentObjective{.set_lsn = *lsn, .content = std::move(*content)};
}

std::vector<std::byte> EncodeOpen(std::uint64_t lsn,
                                  std::span<const std::byte> objective) {
  return EncodeObjective(lsn, objective);
}

std::expected<OpenLoop, Error> DecodeOpen(const KeyValue& item) {
  if (item.key.size() <= 17 || item.key[0] != kOpenPrefix) {
    return std::unexpected(Error{ErrorCode::kIntentFrameCorrupt, 0});
  }
  auto decoded = DecodeObjective(item.value);
  if (!decoded) {
    return std::unexpected(decoded.error());
  }
  return OpenLoop{
      .opened_lsn = decoded->set_lsn,
      .loop_id = std::vector<std::byte>(item.key.begin() + 17, item.key.end()),
      .objective = std::move(decoded->content),
  };
}

std::vector<std::byte> EncodeClosed(const OpenLoop& open,
                                    std::uint64_t closed_lsn,
                                    LoopClosure reason,
                                    std::span<const std::byte> cause) {
  std::vector<std::byte> value;
  AppendU64(value, open.opened_lsn);
  AppendU64(value, closed_lsn);
  value.push_back(static_cast<std::byte>(reason));
  AppendBytes(value, open.objective);
  AppendBytes(value, cause);
  return value;
}

std::expected<ClosedLoop, Error> DecodeClosed(const KeyValue& item) {
  if (item.key.size() <= 17 || item.key[0] != kClosedPrefix) {
    return std::unexpected(Error{ErrorCode::kIntentFrameCorrupt, 0});
  }
  std::size_t offset = 0;
  auto opened_lsn = ReadU64(item.value, offset);
  auto closed_lsn = ReadU64(item.value, offset);
  if (!opened_lsn || !closed_lsn || offset >= item.value.size()) {
    return std::unexpected(Error{ErrorCode::kIntentFrameCorrupt, 0});
  }
  const auto raw_reason = std::to_integer<std::uint8_t>(item.value[offset++]);
  auto objective = ReadBytes(item.value, offset);
  auto cause = ReadBytes(item.value, offset);
  if (raw_reason > static_cast<std::uint8_t>(LoopClosure::kSuperseded) ||
      !objective || !cause || offset != item.value.size()) {
    return std::unexpected(Error{ErrorCode::kIntentFrameCorrupt, 0});
  }
  return ClosedLoop{
      .opened_lsn = *opened_lsn,
      .closed_lsn = *closed_lsn,
      .reason = static_cast<LoopClosure>(raw_reason),
      .loop_id = std::vector<std::byte>(item.key.begin() + 17, item.key.end()),
      .objective = std::move(*objective),
      .cause = std::move(*cause),
  };
}

std::vector<std::byte> EncodeIndex(const log::ConversationId& conversation,
                                   std::uint64_t opened_lsn, bool closed) {
  std::vector<std::byte> value(conversation.bytes.begin(),
                               conversation.bytes.end());
  AppendU64(value, opened_lsn);
  value.push_back(closed ? std::byte{1} : std::byte{0});
  return value;
}

std::span<const std::byte> FbBytes(
    const flatbuffers::Vector<std::uint8_t>* value) {
  return {reinterpret_cast<const std::byte*>(value->Data()), value->size()};
}

std::vector<Mutation> Borrow(std::span<const OwnedMutation> owned) {
  std::vector<Mutation> mutations;
  mutations.reserve(owned.size());
  for (const auto& mutation : owned) {
    mutations.push_back(Mutation{.kind = mutation.kind,
                                 .key = mutation.key,
                                 .value = mutation.value});
  }
  return mutations;
}

}  // namespace

std::expected<void, Error> IntentFrameProjection::ApplyEvent(
    ProjectionStore& store, const log::Frame& frame) {
  auto verified = events::VerifyEvent(frame.sealed_payload, frame.header.kind,
                                      events::Boundary::kDisk);
  if (!verified) {
    auto error = verified.error();
    error.lsn = frame.header.lsn;
    return std::unexpected(error);
  }
  std::vector<OwnedMutation> owned;
  if (frame.header.kind == log::EventKind::kIntentSet) {
    const auto* value = verified->envelope->payload_as_IntentSet();
    owned.push_back(OwnedMutation{
        .kind = MutationKind::kPut,
        .key = Key(kObjectivePrefix, frame.header.conversation),
        .value = EncodeObjective(frame.header.lsn, FbBytes(value->objective())),
    });
  } else if (frame.header.kind == log::EventKind::kLoopOpened) {
    const auto* value = verified->envelope->payload_as_LoopOpened();
    const auto loop_id = FbBytes(value->loop_id());
    auto snapshot = store.BeginSnapshot();
    if (!snapshot) {
      return std::unexpected(snapshot.error());
    }
    auto existing = snapshot->Get(ProjectionId::kIntentFrame,
                                  IndexKey(loop_id));
    const auto prefix = Key(kOpenPrefix, frame.header.conversation);
    auto open_loops = snapshot->ScanPrefix(ProjectionId::kIntentFrame, prefix,
                                           kMaximumOpenLoops);
    if (!existing || !open_loops) {
      return std::unexpected(existing ? open_loops.error() : existing.error());
    }
    if (existing->has_value()) {
      return std::unexpected(
          Error{ErrorCode::kAlreadyExists, 0, frame.header.lsn});
    }
    if (open_loops->size() >= kMaximumOpenLoops) {
      return std::unexpected(
          Error{ErrorCode::kInvariantViolation, 0, frame.header.lsn});
    }
    owned.push_back(OwnedMutation{
        .kind = MutationKind::kPut,
        .key = Key(kOpenPrefix, frame.header.conversation, loop_id),
        .value = EncodeOpen(frame.header.lsn, FbBytes(value->objective())),
    });
    owned.push_back(OwnedMutation{
        .kind = MutationKind::kPut,
        .key = IndexKey(loop_id),
        .value = EncodeIndex(frame.header.conversation, frame.header.lsn, false),
    });
  } else if (frame.header.kind == log::EventKind::kLoopClosed) {
    const auto* value = verified->envelope->payload_as_LoopClosed();
    const auto loop_id = FbBytes(value->loop_id());
    auto snapshot = store.BeginSnapshot();
    if (!snapshot) {
      return std::unexpected(snapshot.error());
    }
    const auto index_key = IndexKey(loop_id);
    auto index_value = snapshot->Get(ProjectionId::kIntentFrame, index_key);
    if (!index_value) {
      return std::unexpected(index_value.error());
    }
    if (!index_value->has_value() || (*index_value)->size() != 25 ||
        (*index_value)->back() != std::byte{0}) {
      return std::unexpected(
          Error{ErrorCode::kLoopNotFound, 0, frame.header.lsn});
    }
    log::ConversationId conversation{};
    std::copy_n((*index_value)->begin(), conversation.bytes.size(),
                conversation.bytes.begin());
    if (conversation != frame.header.conversation) {
      return std::unexpected(
          Error{ErrorCode::kOrderingViolation, 0, frame.header.lsn});
    }
    const auto open_key = Key(kOpenPrefix, conversation, loop_id);
    auto encoded_open = snapshot->Get(ProjectionId::kIntentFrame, open_key);
    if (!encoded_open) {
      return std::unexpected(encoded_open.error());
    }
    auto maybe_open = std::move(*encoded_open);
    if (!maybe_open.has_value()) {
      return std::unexpected(
          Error{ErrorCode::kIntentFrameCorrupt, 0, frame.header.lsn});
    }
    const KeyValue open_item{.key = open_key, .value = std::move(*maybe_open)};
    auto open = DecodeOpen(open_item);
    if (!open) {
      auto error = open.error();
      error.lsn = frame.header.lsn;
      return std::unexpected(error);
    }
    const auto reason = static_cast<LoopClosure>(value->reason());
    owned.push_back(OwnedMutation{.kind = MutationKind::kDelete,
                                  .key = open_key,
                                  .value = {}});
    owned.push_back(OwnedMutation{
        .kind = MutationKind::kPut,
        .key = Key(kClosedPrefix, conversation, loop_id),
        .value = EncodeClosed(*open, frame.header.lsn, reason,
                              FbBytes(value->cause())),
    });
    owned.push_back(OwnedMutation{
        .kind = MutationKind::kPut,
        .key = index_key,
        .value = EncodeIndex(conversation, open->opened_lsn, true),
    });
  }
  const auto mutations = Borrow(owned);
  return store.Apply(ProjectionId::kIntentFrame, frame.header.lsn, mutations);
}

std::expected<IntentRebuildProgress, Error> IntentFrameProjection::Rebuild(
    ProjectionStore& store, std::span<const log::Frame> frames, bool reset,
    std::size_t maximum_frames) {
  if (reset) {
    auto result = store.Reset(ProjectionId::kIntentFrame);
    if (!result) {
      return std::unexpected(result.error());
    }
  }
  std::uint64_t checkpoint = 0;
  {
    auto snapshot = store.BeginSnapshot();
    if (!snapshot) {
      return std::unexpected(snapshot.error());
    }
    auto value = snapshot->Checkpoint(ProjectionId::kIntentFrame);
    if (!value) {
      return std::unexpected(value.error());
    }
    checkpoint = *value;
  }
  if (checkpoint > frames.size()) {
    return std::unexpected(
        Error{ErrorCode::kProjectionCheckpoint, 0, checkpoint, frames.size()});
  }
  const auto count = std::min(frames.size() - static_cast<std::size_t>(checkpoint),
                              maximum_frames);
  const auto begin = static_cast<std::size_t>(checkpoint);
  for (std::size_t index = begin; index < begin + count; ++index) {
    if (frames[index].header.lsn != static_cast<std::uint64_t>(index) + 1U) {
      return std::unexpected(Error{ErrorCode::kSequenceViolation, 0,
                                   frames[index].header.lsn, index});
    }
    auto applied = ApplyEvent(store, frames[index]);
    if (!applied) {
      return std::unexpected(applied.error());
    }
  }
  const auto applied_lsn = checkpoint + static_cast<std::uint64_t>(count);
  return IntentRebuildProgress{.applied_lsn = applied_lsn,
                               .applied_frames = count,
                               .complete = applied_lsn == frames.size()};
}

std::expected<IntentFrameView, Error> IntentFrameProjection::Read(
    const ReadSnapshot& snapshot, const log::ConversationId& conversation) {
  IntentFrameView frame;
  auto objective = snapshot.Get(ProjectionId::kIntentFrame,
                                Key(kObjectivePrefix, conversation));
  if (!objective) {
    return std::unexpected(objective.error());
  }
  auto maybe_objective = std::move(*objective);
  if (maybe_objective.has_value()) {
    auto decoded = DecodeObjective(*maybe_objective);
    if (!decoded) {
      return std::unexpected(decoded.error());
    }
    frame.objective = std::move(*decoded);
  } else if (conversation != log::ConversationId{}) {
    auto actor_objective = snapshot.Get(
        ProjectionId::kIntentFrame,
        Key(kObjectivePrefix, log::ConversationId{}));
    if (!actor_objective) {
      return std::unexpected(actor_objective.error());
    }
    auto maybe_actor_objective = std::move(*actor_objective);
    if (maybe_actor_objective.has_value()) {
      auto decoded = DecodeObjective(*maybe_actor_objective);
      if (!decoded) {
        return std::unexpected(decoded.error());
      }
      frame.objective = std::move(*decoded);
    }
  }
  const auto prefix = Key(kOpenPrefix, conversation);
  auto items = snapshot.ScanPrefix(ProjectionId::kIntentFrame, prefix,
                                   kMaximumOpenLoops);
  if (!items) {
    return std::unexpected(items.error());
  }
  frame.open_loops.reserve(items->size());
  for (const auto& item : *items) {
    auto loop = DecodeOpen(item);
    if (!loop) {
      return std::unexpected(loop.error());
    }
    frame.open_loops.push_back(std::move(*loop));
  }
  std::sort(frame.open_loops.begin(), frame.open_loops.end(),
            [](const OpenLoop& left, const OpenLoop& right) {
              return left.opened_lsn < right.opened_lsn;
            });
  return frame;
}

std::expected<std::vector<ClosedLoop>, Error> IntentFrameProjection::ReadClosed(
    const ReadSnapshot& snapshot, const log::ConversationId& conversation) {
  const auto prefix = Key(kClosedPrefix, conversation);
  auto items = snapshot.ScanPrefix(ProjectionId::kIntentFrame, prefix,
                                   kMaximumOpenLoops);
  if (!items) {
    return std::unexpected(items.error());
  }
  std::vector<ClosedLoop> loops;
  loops.reserve(items->size());
  for (const auto& item : *items) {
    auto loop = DecodeClosed(item);
    if (!loop) {
      return std::unexpected(loop.error());
    }
    loops.push_back(std::move(*loop));
  }
  std::sort(loops.begin(), loops.end(),
            [](const ClosedLoop& left, const ClosedLoop& right) {
              return left.closed_lsn < right.closed_lsn;
            });
  return loops;
}

}  // namespace neocortex::proj
