#include "proj/ledger/work_ledger.h"

#include <algorithm>
#include <array>
#include <optional>
#include <string_view>
#include <utility>

#include "schema/events.h"

namespace neocortex::proj {
namespace {

constexpr std::byte kWorkPrefix{0x57};
constexpr std::byte kCallLsnIndexPrefix{0x4e};
constexpr std::byte kEffectIndexPrefix{0x45};

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

void AppendU64Big(std::vector<std::byte>& output, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    output.push_back(
        static_cast<std::byte>((value >> ((7U - index) * 8U)) & 0xffU));
  }
}

std::expected<std::uint64_t, Error> ReadU64(std::span<const std::byte> bytes,
                                           std::size_t& offset) {
  if (bytes.size() - std::min(bytes.size(), offset) < 8) {
    return std::unexpected(Error{ErrorCode::kWorkLedgerCorrupt, 0});
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
    return std::unexpected(Error{ErrorCode::kWorkLedgerCorrupt, 0});
  }
  const auto size = static_cast<std::size_t>(*length);
  std::vector<std::byte> value(bytes.begin() + static_cast<std::ptrdiff_t>(offset),
                               bytes.begin() +
                                   static_cast<std::ptrdiff_t>(offset + size));
  offset += size;
  return value;
}

std::vector<std::byte> WorkPrefix(
    const log::ConversationId& conversation) {
  std::vector<std::byte> key;
  key.reserve(17);
  key.push_back(kWorkPrefix);
  key.insert(key.end(), conversation.bytes.begin(), conversation.bytes.end());
  return key;
}

std::vector<std::byte> WorkKey(const log::ConversationId& conversation,
                               std::uint64_t tool_call_lsn, WorkKind kind,
                               std::span<const std::byte> effect_id = {}) {
  auto key = WorkPrefix(conversation);
  key.reserve(key.size() + 9U + effect_id.size());
  AppendU64Big(key, tool_call_lsn);
  key.push_back(static_cast<std::byte>(kind));
  key.insert(key.end(), effect_id.begin(), effect_id.end());
  return key;
}

std::vector<std::byte> LsnIndexKey(std::uint64_t tool_call_lsn) {
  std::vector<std::byte> key;
  key.push_back(kCallLsnIndexPrefix);
  AppendU64Big(key, tool_call_lsn);
  return key;
}

std::vector<std::byte> EffectIndexKey(
    std::span<const std::byte> effect_id) {
  std::vector<std::byte> key;
  key.reserve(effect_id.size() + 1U);
  key.push_back(kEffectIndexPrefix);
  key.insert(key.end(), effect_id.begin(), effect_id.end());
  return key;
}

std::vector<std::byte> EncodeItem(const WorkItem& item) {
  std::vector<std::byte> value;
  value.push_back(static_cast<std::byte>(item.kind));
  value.push_back(static_cast<std::byte>(item.state));
  AppendU64(value, item.tool_call_lsn);
  AppendU64(value, item.state_lsn);
  AppendBytes(value, item.call_id);
  AppendBytes(value, item.tool_name);
  AppendBytes(value, item.arguments);
  AppendBytes(value, item.effect_id);
  AppendBytes(value, item.detail);
  return value;
}

std::expected<WorkItem, Error> DecodeItem(std::span<const std::byte> value) {
  if (value.size() < 2) {
    return std::unexpected(Error{ErrorCode::kWorkLedgerCorrupt, 0});
  }
  const auto raw_kind = std::to_integer<std::uint8_t>(value[0]);
  const auto raw_state = std::to_integer<std::uint8_t>(value[1]);
  if (raw_kind > static_cast<std::uint8_t>(WorkKind::kEffect) ||
      raw_state > static_cast<std::uint8_t>(WorkState::kOutcomeUnknown)) {
    return std::unexpected(Error{ErrorCode::kWorkLedgerCorrupt, 0});
  }
  std::size_t offset = 2;
  auto call_lsn = ReadU64(value, offset);
  auto state_lsn = ReadU64(value, offset);
  auto call_id = ReadBytes(value, offset);
  auto tool_name = ReadBytes(value, offset);
  auto arguments = ReadBytes(value, offset);
  auto effect_id = ReadBytes(value, offset);
  auto detail = ReadBytes(value, offset);
  if (!call_lsn || !state_lsn || !call_id || !tool_name || !arguments ||
      !effect_id || !detail || offset != value.size() || *call_lsn == 0 ||
      *state_lsn == 0) {
    return std::unexpected(Error{ErrorCode::kWorkLedgerCorrupt, 0});
  }
  const auto kind = static_cast<WorkKind>(raw_kind);
  const auto state = static_cast<WorkState>(raw_state);
  if (call_id->empty() || tool_name->empty() ||
      (kind == WorkKind::kToolCall && !effect_id->empty()) ||
      (kind == WorkKind::kEffect && effect_id->empty())) {
    return std::unexpected(Error{ErrorCode::kWorkLedgerCorrupt, 0});
  }
  return WorkItem{
      .kind = kind,
      .state = state,
      .tool_call_lsn = *call_lsn,
      .state_lsn = *state_lsn,
      .call_id = std::move(*call_id),
      .tool_name = std::move(*tool_name),
      .arguments = std::move(*arguments),
      .effect_id = std::move(*effect_id),
      .detail = std::move(*detail),
      .requires_reconciliation = state != WorkState::kReturned,
  };
}

std::span<const std::byte> FbBytes(
    const flatbuffers::Vector<std::uint8_t>* value) {
  return {reinterpret_cast<const std::byte*>(value->Data()), value->size()};
}

std::span<const std::byte> FbBytes(const flatbuffers::String* value) {
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

std::expected<KeyValue, Error> ReadIndexed(
    const ReadSnapshot& snapshot, std::span<const std::byte> index_key) {
  auto primary_key = snapshot.Get(ProjectionId::kWorkLedger, index_key);
  if (!primary_key) {
    return std::unexpected(primary_key.error());
  }
  auto maybe_primary = std::move(*primary_key);
  if (!maybe_primary.has_value()) {
    return std::unexpected(Error{ErrorCode::kWorkItemNotFound, 0});
  }
  auto primary = std::move(*maybe_primary);
  auto value = snapshot.Get(ProjectionId::kWorkLedger, primary);
  if (!value) {
    return std::unexpected(value.error());
  }
  auto maybe_value = std::move(*value);
  if (!maybe_value.has_value()) {
    return std::unexpected(Error{ErrorCode::kWorkLedgerCorrupt, 0});
  }
  return KeyValue{.key = std::move(primary), .value = std::move(*maybe_value)};
}

bool TransitionAllowed(WorkState current, WorkState next) {
  if (current == next || next == WorkState::kReturned) {
    return true;
  }
  if (current == WorkState::kDispatched) {
    return true;
  }
  return current == WorkState::kCommitted &&
         next == WorkState::kOutcomeUnknown;
}

std::expected<log::ConversationId, Error> ConversationFromWorkKey(
    std::span<const std::byte> key) {
  if (key.size() < 26 || key[0] != kWorkPrefix) {
    return std::unexpected(Error{ErrorCode::kWorkLedgerCorrupt, 0});
  }
  log::ConversationId conversation{};
  std::copy_n(key.begin() + 1, conversation.bytes.size(),
              conversation.bytes.begin());
  return conversation;
}

std::expected<void, Error> UpdateItem(std::vector<OwnedMutation>& owned,
                                      KeyValue source, WorkState next,
                                      std::uint64_t state_lsn,
                                      std::span<const std::byte> detail) {
  auto item = DecodeItem(source.value);
  if (!item) {
    return std::unexpected(item.error());
  }
  if (!TransitionAllowed(item->state, next)) {
    return std::unexpected(
        Error{ErrorCode::kOrderingViolation, 0, state_lsn, item->state_lsn});
  }
  item->state = next;
  item->state_lsn = state_lsn;
  item->detail.assign(detail.begin(), detail.end());
  owned.push_back(OwnedMutation{.kind = MutationKind::kPut,
                                .key = std::move(source.key),
                                .value = EncodeItem(*item)});
  return {};
}

WorkState FromEffectState(schema::EffectState state) {
  return static_cast<WorkState>(state);
}

WorkState FromResultStatus(schema::ResultStatus status) {
  return status == schema::ResultStatus::outcome_unknown
             ? WorkState::kOutcomeUnknown
             : WorkState::kReturned;
}

}  // namespace

std::expected<void, Error> WorkLedgerProjection::ApplyEvent(
    ProjectionStore& store, const log::Frame& frame) {
  auto verified = events::VerifyEvent(frame.sealed_payload, frame.header.kind,
                                      events::Boundary::kDisk);
  if (!verified) {
    auto error = verified.error();
    error.lsn = frame.header.lsn;
    return std::unexpected(error);
  }
  std::vector<OwnedMutation> owned;
  if (frame.header.kind == log::EventKind::kToolCall) {
    const auto* value = verified->envelope->payload_as_ToolCall();
    const WorkItem item{
        .kind = WorkKind::kToolCall,
        .state = WorkState::kDispatched,
        .tool_call_lsn = frame.header.lsn,
        .state_lsn = frame.header.lsn,
        .call_id = std::vector<std::byte>(FbBytes(value->call_id()).begin(),
                                         FbBytes(value->call_id()).end()),
        .tool_name = std::vector<std::byte>(FbBytes(value->tool_name()).begin(),
                                           FbBytes(value->tool_name()).end()),
        .arguments = std::vector<std::byte>(FbBytes(value->arguments()).begin(),
                                           FbBytes(value->arguments()).end()),
        .effect_id = {},
        .detail = {},
        .requires_reconciliation = true,
    };
    auto primary_key = WorkKey(frame.header.conversation, frame.header.lsn,
                               WorkKind::kToolCall);
    owned.push_back(OwnedMutation{.kind = MutationKind::kPut,
                                  .key = primary_key,
                                  .value = EncodeItem(item)});
    owned.push_back(OwnedMutation{.kind = MutationKind::kPut,
                                  .key = LsnIndexKey(frame.header.lsn),
                                  .value = std::move(primary_key)});
  } else if (frame.header.kind == log::EventKind::kToolResult) {
    const auto* value = verified->envelope->payload_as_ToolResult();
    auto snapshot = store.BeginSnapshot();
    if (!snapshot) {
      return std::unexpected(snapshot.error());
    }
    auto source = ReadIndexed(*snapshot, LsnIndexKey(value->tool_call_lsn()));
    if (!source) {
      auto error = source.error();
      error.lsn = frame.header.lsn;
      return std::unexpected(error);
    }
    auto item = DecodeItem(source->value);
    auto conversation = ConversationFromWorkKey(source->key);
    if (!item || !conversation || *conversation != frame.header.conversation ||
        item->call_id != std::vector<std::byte>(
                                     FbBytes(value->call_id()).begin(),
                                     FbBytes(value->call_id()).end())) {
      return std::unexpected(Error{item ? ErrorCode::kOrderingViolation
                                        : item.error().code,
                                   0, frame.header.lsn,
                                   value->tool_call_lsn()});
    }
    auto updated = UpdateItem(owned, std::move(*source),
                              FromResultStatus(value->status()),
                              frame.header.lsn, FbBytes(value->result()));
    if (!updated) {
      return std::unexpected(updated.error());
    }
  } else if (frame.header.kind == log::EventKind::kEffect) {
    const auto* value = verified->envelope->payload_as_Effect();
    auto snapshot = store.BeginSnapshot();
    if (!snapshot) {
      return std::unexpected(snapshot.error());
    }
    auto call_source =
        ReadIndexed(*snapshot, LsnIndexKey(value->tool_call_lsn()));
    if (!call_source) {
      auto error = call_source.error();
      error.lsn = frame.header.lsn;
      return std::unexpected(error);
    }
    auto call = DecodeItem(call_source->value);
    auto conversation = ConversationFromWorkKey(call_source->key);
    if (!call || !conversation) {
      return std::unexpected(call ? conversation.error() : call.error());
    }
    if (*conversation != frame.header.conversation) {
      return std::unexpected(
          Error{ErrorCode::kOrderingViolation, 0, frame.header.lsn,
                value->tool_call_lsn()});
    }
    const auto effect_id = FbBytes(value->effect_id());
    const auto effect_index_key = EffectIndexKey(effect_id);
    auto existing_key = snapshot->Get(ProjectionId::kWorkLedger,
                                      effect_index_key);
    if (!existing_key) {
      return std::unexpected(existing_key.error());
    }
    if (existing_key->has_value()) {
      auto existing = ReadIndexed(*snapshot, effect_index_key);
      if (!existing) {
        return std::unexpected(existing.error());
      }
      auto item = DecodeItem(existing->value);
      if (!item || item->tool_call_lsn != value->tool_call_lsn()) {
        return std::unexpected(
            Error{ErrorCode::kOrderingViolation, 0, frame.header.lsn,
                  value->tool_call_lsn()});
      }
      auto updated = UpdateItem(owned, std::move(*existing),
                                FromEffectState(value->state()),
                                frame.header.lsn, {});
      if (!updated) {
        return std::unexpected(updated.error());
      }
    } else {
      const WorkItem effect{
          .kind = WorkKind::kEffect,
          .state = FromEffectState(value->state()),
          .tool_call_lsn = value->tool_call_lsn(),
          .state_lsn = frame.header.lsn,
          .call_id = call->call_id,
          .tool_name = call->tool_name,
          .arguments = call->arguments,
          .effect_id = std::vector<std::byte>(effect_id.begin(), effect_id.end()),
          .detail = {},
          .requires_reconciliation = value->state() != schema::EffectState::returned,
      };
      auto primary_key = WorkKey(*conversation, value->tool_call_lsn(),
                                 WorkKind::kEffect, effect_id);
      owned.push_back(OwnedMutation{.kind = MutationKind::kPut,
                                    .key = primary_key,
                                    .value = EncodeItem(effect)});
      owned.push_back(OwnedMutation{.kind = MutationKind::kPut,
                                    .key = effect_index_key,
                                    .value = std::move(primary_key)});
    }
  } else if (frame.header.kind == log::EventKind::kOutcome) {
    const auto* value = verified->envelope->payload_as_Outcome();
    auto snapshot = store.BeginSnapshot();
    if (!snapshot) {
      return std::unexpected(snapshot.error());
    }
    auto source =
        ReadIndexed(*snapshot, EffectIndexKey(FbBytes(value->effect_id())));
    if (!source) {
      auto error = source.error();
      error.lsn = frame.header.lsn;
      return std::unexpected(error);
    }
    auto conversation = ConversationFromWorkKey(source->key);
    if (!conversation || *conversation != frame.header.conversation) {
      return std::unexpected(
          Error{ErrorCode::kOrderingViolation, 0, frame.header.lsn});
    }
    auto updated = UpdateItem(owned, std::move(*source),
                              FromResultStatus(value->status()),
                              frame.header.lsn, FbBytes(value->detail()));
    if (!updated) {
      return std::unexpected(updated.error());
    }
  }
  const auto mutations = Borrow(owned);
  return store.Apply(ProjectionId::kWorkLedger, frame.header.lsn, mutations);
}

std::expected<LedgerRebuildProgress, Error> WorkLedgerProjection::Rebuild(
    ProjectionStore& store, std::span<const log::Frame> frames, bool reset,
    std::size_t maximum_frames) {
  if (reset) {
    auto result = store.Reset(ProjectionId::kWorkLedger);
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
    auto value = snapshot->Checkpoint(ProjectionId::kWorkLedger);
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
  return LedgerRebuildProgress{.applied_lsn = applied_lsn,
                               .applied_frames = count,
                               .complete = applied_lsn == frames.size()};
}

std::expected<std::vector<WorkItem>, Error>
WorkLedgerProjection::ReadConversation(
    const ReadSnapshot& snapshot, const log::ConversationId& conversation,
    std::size_t limit) {
  if (limit == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto items = snapshot.ScanPrefixReverse(ProjectionId::kWorkLedger,
                                          WorkPrefix(conversation), limit);
  if (!items) {
    return std::unexpected(items.error());
  }
  std::vector<WorkItem> ledger;
  ledger.reserve(items->size());
  for (const auto& item : *items) {
    auto decoded = DecodeItem(item.value);
    if (!decoded) {
      return std::unexpected(decoded.error());
    }
    ledger.push_back(std::move(*decoded));
  }
  std::reverse(ledger.begin(), ledger.end());
  return ledger;
}

}  // namespace neocortex::proj
