#include <algorithm>
#include <array>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <filesystem>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include <flatbuffers/flatbuffers.h>
#include <signal.h>
#include <sys/wait.h>
#include <unistd.h>

#include "compose/composer.h"
#include "proj/beliefs/belief_store.h"
#include "proj/conversation_heads.h"
#include "proj/entity/entity_index.h"
#include "proj/intent/intent_frame.h"
#include "proj/ladder/temporal_ladder.h"
#include "proj/ledger/work_ledger.h"
#include "proj/lexical/bm25.h"
#include "schema/events_generated.h"

namespace {

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

std::span<const std::byte> Bytes(std::string_view value) {
  return {reinterpret_cast<const std::byte*>(value.data()), value.size()};
}

bool Equal(std::span<const std::byte> value, std::string_view expected) {
  return value.size() == expected.size() &&
         std::equal(value.begin(), value.end(), Bytes(expected).begin());
}

std::vector<std::byte> Finish(flatbuffers::FlatBufferBuilder& builder,
                              neocortex::schema::EventPayload type,
                              flatbuffers::Offset<void> payload) {
  const auto envelope =
      neocortex::schema::CreateEventEnvelope(builder, 1, type, payload);
  neocortex::schema::FinishEventEnvelopeBuffer(builder, envelope);
  return {reinterpret_cast<const std::byte*>(builder.GetBufferPointer()),
          reinterpret_cast<const std::byte*>(builder.GetBufferPointer()) +
              builder.GetSize()};
}

std::vector<std::byte> Intent(std::string_view objective) {
  flatbuffers::FlatBufferBuilder builder;
  const auto data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(objective.data()), objective.size());
  return Finish(builder, neocortex::schema::EventPayload::IntentSet,
                neocortex::schema::CreateIntentSet(builder, data).Union());
}

std::vector<std::byte> OpenLoop(std::string_view id,
                                std::string_view objective) {
  flatbuffers::FlatBufferBuilder builder;
  const auto id_data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(id.data()), id.size());
  const auto objective_data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(objective.data()), objective.size());
  return Finish(builder, neocortex::schema::EventPayload::LoopOpened,
                neocortex::schema::CreateLoopOpened(builder, id_data,
                                                     objective_data)
                    .Union());
}

std::vector<std::byte> CloseLoop(
    std::string_view id, neocortex::schema::LoopCloseReason reason,
    std::string_view cause) {
  flatbuffers::FlatBufferBuilder builder;
  const auto id_data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(id.data()), id.size());
  const auto cause_data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(cause.data()), cause.size());
  return Finish(builder, neocortex::schema::EventPayload::LoopClosed,
                neocortex::schema::CreateLoopClosed(builder, id_data, reason,
                                                     cause_data)
                    .Union());
}

struct ToolCallInput final {
  std::string_view id;
  std::string_view tool;
  std::string_view arguments;
};

std::vector<std::byte> ToolCall(ToolCallInput input) {
  flatbuffers::FlatBufferBuilder builder;
  const auto id_data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(input.id.data()), input.id.size());
  const auto tool_data = builder.CreateString(input.tool);
  const auto argument_data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(input.arguments.data()),
      input.arguments.size());
  return Finish(builder, neocortex::schema::EventPayload::ToolCall,
                neocortex::schema::CreateToolCall(builder, id_data, tool_data,
                                                   argument_data)
                    .Union());
}

std::vector<std::byte> ToolResult(std::string_view id, std::uint64_t call_lsn,
                                  neocortex::schema::ResultStatus status,
                                  std::string_view result) {
  flatbuffers::FlatBufferBuilder builder;
  const auto id_data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(id.data()), id.size());
  const auto result_data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(result.data()), result.size());
  return Finish(builder, neocortex::schema::EventPayload::ToolResult,
                neocortex::schema::CreateToolResult(builder, id_data, call_lsn,
                                                     status, result_data)
                    .Union());
}

std::vector<std::byte> Effect(std::string_view id, std::uint64_t call_lsn,
                              neocortex::schema::EffectState state) {
  flatbuffers::FlatBufferBuilder builder;
  const auto id_data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(id.data()), id.size());
  return Finish(builder, neocortex::schema::EventPayload::Effect,
                neocortex::schema::CreateEffect(builder, id_data, call_lsn,
                                                 state)
                    .Union());
}

std::vector<std::byte> Outcome(std::string_view id,
                               neocortex::schema::ResultStatus status,
                               std::string_view detail) {
  flatbuffers::FlatBufferBuilder builder;
  const auto id_data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(id.data()), id.size());
  const auto detail_data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(detail.data()), detail.size());
  return Finish(builder, neocortex::schema::EventPayload::Outcome,
                neocortex::schema::CreateOutcome(builder, id_data, status,
                                                  detail_data)
                    .Union());
}

std::vector<std::byte> UserMessage(std::string_view content) {
  flatbuffers::FlatBufferBuilder builder;
  const auto data = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(content.data()), content.size());
  return Finish(builder, neocortex::schema::EventPayload::UserMsg,
                neocortex::schema::CreateUserMsg(builder, data).Union());
}

neocortex::log::Frame Frame(std::uint64_t lsn,
                            neocortex::log::EventKind kind,
                            neocortex::log::ConversationId conversation,
                            std::vector<std::byte> payload) {
  return neocortex::log::Frame{
      .header = neocortex::log::FrameHeader{
          .lsn = lsn,
          .kind = kind,
          .wall_timestamp_ns = static_cast<std::int64_t>(lsn) * 1000,
          .actor = 17,
          .conversation = conversation,
      },
      .sealed_payload = std::move(payload),
  };
}

std::vector<neocortex::log::Frame> Workload(
    neocortex::log::ConversationId conversation) {
  std::vector<neocortex::log::Frame> frames;
  frames.push_back(Frame(1, neocortex::log::EventKind::kIntentSet, conversation,
                         Intent("objective-one")));
  frames.push_back(Frame(2, neocortex::log::EventKind::kLoopOpened, conversation,
                         OpenLoop("loop-a", "deliver answer")));
  frames.push_back(Frame(3, neocortex::log::EventKind::kUserMsg, conversation,
                         UserMessage("continue")));
  frames.push_back(Frame(4, neocortex::log::EventKind::kToolCall, conversation,
                         ToolCall({"call-a", "lookup", "alpha"})));
  frames.push_back(Frame(5, neocortex::log::EventKind::kEffect, conversation,
                         Effect("effect-a", 4,
                                neocortex::schema::EffectState::dispatched)));
  frames.push_back(Frame(6, neocortex::log::EventKind::kToolResult, conversation,
                         ToolResult("call-a", 4,
                                    neocortex::schema::ResultStatus::ok,
                                    "lookup complete")));
  frames.push_back(Frame(7, neocortex::log::EventKind::kOutcome, conversation,
                         Outcome("effect-a",
                                 neocortex::schema::ResultStatus::ok,
                                 "effect committed")));
  frames.push_back(Frame(8, neocortex::log::EventKind::kLoopOpened, conversation,
                         OpenLoop("loop-b", "reconcile write")));
  frames.push_back(Frame(9, neocortex::log::EventKind::kToolCall, conversation,
                         ToolCall({"call-b", "write", "beta"})));
  frames.push_back(Frame(10, neocortex::log::EventKind::kEffect, conversation,
                         Effect("effect-b", 9,
                                neocortex::schema::EffectState::committed)));
  frames.push_back(Frame(11, neocortex::log::EventKind::kIntentSet, conversation,
                         Intent("objective-two")));
  frames.push_back(Frame(12, neocortex::log::EventKind::kLoopClosed, conversation,
                         CloseLoop(
                             "loop-a",
                             neocortex::schema::LoopCloseReason::abandoned,
                             "owner changed direction")));
  frames.push_back(Frame(13, neocortex::log::EventKind::kOutcome, conversation,
                         Outcome("effect-b",
                                 neocortex::schema::ResultStatus::outcome_unknown,
                                 "crashed after dispatch")));
  return frames;
}

std::expected<std::size_t, neocortex::Error> CountTokens(
    void*, neocortex::compose::Tier, std::string_view,
    std::span<const std::uint64_t>, std::span<const std::byte> content) {
  return std::max<std::size_t>(1, content.size());
}

bool BuildAll(neocortex::proj::ProjectionStore& store,
              std::span<const neocortex::log::Frame> frames) {
  return neocortex::proj::RebuildConversationHeads(store, frames, false) &&
         neocortex::proj::BeliefProjection::Rebuild(store, frames, false) &&
         neocortex::proj::EntityProjection::Rebuild(store, frames) &&
         neocortex::proj::LexicalProjection::Rebuild(store, frames) &&
         neocortex::proj::TemporalLadder::Rebuild(store, frames) &&
         neocortex::proj::IntentFrameProjection::Rebuild(store, frames) &&
         neocortex::proj::WorkLedgerProjection::Rebuild(store, frames);
}

neocortex::compose::ActivationRequest Request(
    neocortex::log::ConversationId conversation, std::size_t budget) {
  return neocortex::compose::ActivationRequest{
      .conversation = conversation,
      .query = "continue write",
      .turn_text = "continue",
      .budget_tokens = budget,
      .token_model = neocortex::compose::TokenModel{
          .context = nullptr, .count = CountTokens},
      .query_embedding = {},
      .query_binary_prefilter = {},
      .temporal_from_ns = -1,
      .temporal_to_ns = 1'000'000,
      .maximum_candidates = 64,
      .maximum_conversation_records = 64,
  };
}

int VerifyPrefix(const std::filesystem::path& path,
                 std::span<const neocortex::log::Frame> frames,
                 neocortex::log::ConversationId conversation) {
  auto store = neocortex::proj::ProjectionStore::Open(path);
  if (!store) {
    return Fail("continuity store open failed");
  }
  auto intent = neocortex::proj::IntentFrameProjection::Rebuild(*store, frames);
  auto ledger = neocortex::proj::WorkLedgerProjection::Rebuild(*store, frames);
  if (!intent || !intent->complete || !ledger || !ledger->complete) {
    return Fail("continuity prefix rebuild failed");
  }
  auto snapshot = store->BeginSnapshot();
  if (!snapshot) {
    return Fail("continuity prefix snapshot failed");
  }
  auto intent_view =
      neocortex::proj::IntentFrameProjection::Read(*snapshot, conversation);
  auto work = neocortex::proj::WorkLedgerProjection::ReadConversation(
      *snapshot, conversation);
  if (!intent_view || !work) {
    return Fail("continuity prefix read failed");
  }
  auto maybe_objective = std::move(intent_view->objective);
  if (!maybe_objective.has_value()) {
    return Fail("continuity prefix objective missing");
  }
  const auto tail = frames.size();
  const std::string_view expected_objective =
      tail >= 11 ? "objective-two" : "objective-one";
  const auto objective = std::move(*maybe_objective);
  if (!Equal(objective.content, expected_objective)) {
    return Fail("intent objective did not survive prefix boot");
  }
  const bool loop_a_open = tail >= 2 && tail < 12;
  const bool loop_b_open = tail >= 8;
  if (intent_view->open_loops.size() !=
      static_cast<std::size_t>(loop_a_open) +
          static_cast<std::size_t>(loop_b_open)) {
    return Fail("open loop disappeared or closed silently after prefix boot");
  }
  const auto expected_work =
      (tail >= 4 ? 1U : 0U) + (tail >= 5 ? 1U : 0U) +
      (tail >= 9 ? 1U : 0U) + (tail >= 10 ? 1U : 0U);
  if (work->size() != expected_work) {
    return Fail("work ledger lost a call or effect after prefix boot");
  }
  for (const auto& item : *work) {
    const bool expected_reconcile =
        (item.tool_call_lsn == 4 &&
         ((item.kind == neocortex::proj::WorkKind::kToolCall && tail < 6) ||
          (item.kind == neocortex::proj::WorkKind::kEffect && tail < 7))) ||
        item.tool_call_lsn == 9;
    if (item.requires_reconciliation != expected_reconcile) {
      return Fail("uncertain work did not receive exact reconciliation state");
    }
  }
  return 0;
}

int TestContinuity(const std::filesystem::path& root) {
  neocortex::log::ConversationId conversation{};
  conversation.bytes[0] = std::byte{0x71};
  const auto frames = Workload(conversation);
  std::error_code cleanup_error;
  for (std::size_t prefix = 1; prefix <= frames.size(); ++prefix) {
    const auto path = root / ("prefix-" + std::to_string(prefix));
    std::filesystem::remove_all(path, cleanup_error);
    if (VerifyPrefix(path, std::span(frames).first(prefix), conversation) != 0) {
      return 1;
    }
  }

  const auto replay_path = root / "replay";
  auto store = neocortex::proj::ProjectionStore::Open(replay_path);
  if (!store) {
    return Fail("continuity replay store open failed");
  }
  auto intent = neocortex::proj::IntentFrameProjection::Rebuild(*store, frames);
  auto ledger = neocortex::proj::WorkLedgerProjection::Rebuild(*store, frames);
  if (!intent || !ledger) {
    return Fail("continuity full rebuild failed");
  }
  std::vector<std::byte> original_intent;
  std::vector<std::byte> original_ledger;
  {
    auto snapshot = store->BeginSnapshot();
    if (!snapshot) {
      return Fail("continuity dump snapshot failed");
    }
    auto first = snapshot->CanonicalDump(
        neocortex::proj::ProjectionId::kIntentFrame);
    auto second = snapshot->CanonicalDump(
        neocortex::proj::ProjectionId::kWorkLedger);
    if (!first || !second) {
      return Fail("continuity canonical dump failed");
    }
    original_intent = std::move(*first);
    original_ledger = std::move(*second);
  }
  auto rebuilt_intent =
      neocortex::proj::IntentFrameProjection::Rebuild(*store, frames, true);
  auto rebuilt_ledger =
      neocortex::proj::WorkLedgerProjection::Rebuild(*store, frames, true);
  auto snapshot = store->BeginSnapshot();
  if (!rebuilt_intent || !rebuilt_ledger || !snapshot) {
    return Fail("continuity reset replay failed");
  }
  auto intent_dump = snapshot->CanonicalDump(
      neocortex::proj::ProjectionId::kIntentFrame);
  auto ledger_dump = snapshot->CanonicalDump(
      neocortex::proj::ProjectionId::kWorkLedger);
  auto closed =
      neocortex::proj::IntentFrameProjection::ReadClosed(*snapshot, conversation);
  auto tail = neocortex::proj::WorkLedgerProjection::ReadConversation(
      *snapshot, conversation, 2);
  if (!intent_dump || !ledger_dump || *intent_dump != original_intent ||
      *ledger_dump != original_ledger || !closed || closed->size() != 1 ||
      closed->front().reason != neocortex::proj::LoopClosure::kAbandoned ||
      !Equal(closed->front().cause, "owner changed direction") || !tail ||
      tail->size() != 2 || tail->front().tool_call_lsn != 9 ||
      tail->back().tool_call_lsn != 9) {
    return Fail("continuity replay or typed loop closure was incorrect");
  }

  const auto killed_path = root / "killed";
  std::array<int, 2> ready{};
  if (::pipe(ready.data()) != 0) {
    return Fail("continuity crash pipe failed");
  }
  const pid_t child = ::fork();
  if (child < 0) {
    return Fail("continuity crash fork failed");
  }
  if (child == 0) {
    ::close(ready[0]);
    auto child_store = neocortex::proj::ProjectionStore::Open(killed_path);
    if (!child_store) {
      _exit(2);
    }
    auto child_intent = neocortex::proj::IntentFrameProjection::Rebuild(
        *child_store, frames, false, 10);
    auto child_ledger = neocortex::proj::WorkLedgerProjection::Rebuild(
        *child_store, frames, false, 10);
    if (!child_intent || !child_ledger || child_intent->applied_lsn != 10 ||
        child_ledger->applied_lsn != 10) {
      _exit(3);
    }
    const std::byte marker{0x41};
    if (::write(ready[1], &marker, 1) != 1) {
      _exit(4);
    }
    for (;;) {
      ::pause();
    }
  }
  ::close(ready[1]);
  std::byte marker{};
  if (::read(ready[0], &marker, 1) != 1 || ::kill(child, SIGKILL) != 0) {
    return Fail("continuity crash synchronization failed");
  }
  int status = 0;
  if (::waitpid(child, &status, 0) != child || !WIFSIGNALED(status) ||
      WTERMSIG(status) != SIGKILL) {
    return Fail("continuity child did not die by SIGKILL");
  }
  auto killed_store = neocortex::proj::ProjectionStore::Open(killed_path);
  if (!killed_store) {
    return Fail("continuity post-kill reopen failed");
  }
  auto resumed_intent =
      neocortex::proj::IntentFrameProjection::Rebuild(*killed_store, frames);
  auto resumed_ledger =
      neocortex::proj::WorkLedgerProjection::Rebuild(*killed_store, frames);
  auto killed_snapshot = killed_store->BeginSnapshot();
  if (!resumed_intent || !resumed_intent->complete || !resumed_ledger ||
      !resumed_ledger->complete || !killed_snapshot) {
    return Fail("continuity projections did not resume after SIGKILL");
  }
  auto killed_intent = killed_snapshot->CanonicalDump(
      neocortex::proj::ProjectionId::kIntentFrame);
  auto killed_ledger = killed_snapshot->CanonicalDump(
      neocortex::proj::ProjectionId::kWorkLedger);
  if (!killed_intent || !killed_ledger || *killed_intent != original_intent ||
      *killed_ledger != original_ledger) {
    return Fail("SIGKILL resume did not converge byte-identically");
  }

  for (std::size_t prefix = 1; prefix <= frames.size(); ++prefix) {
    const auto inv_path = root / ("inv1-" + std::to_string(prefix));
    std::array<int, 2> inv_ready{};
    if (::pipe(inv_ready.data()) != 0) {
      return Fail("INV-1 pipe failed");
    }
    const pid_t inv_child = ::fork();
    if (inv_child < 0) {
      return Fail("INV-1 fork failed");
    }
    if (inv_child == 0) {
      ::close(inv_ready[0]);
      auto child_store = neocortex::proj::ProjectionStore::Open(inv_path);
      if (!child_store ||
          !BuildAll(*child_store, std::span(frames).first(prefix))) {
        _exit(11);
      }
      const std::byte inv_marker{0x51};
      if (::write(inv_ready[1], &inv_marker, 1) != 1) {
        _exit(12);
      }
      for (;;) {
        ::pause();
      }
    }
    ::close(inv_ready[1]);
    std::byte inv_marker{};
    if (::read(inv_ready[0], &inv_marker, 1) != 1 ||
        ::kill(inv_child, SIGKILL) != 0) {
      return Fail("INV-1 kill synchronization failed");
    }
    int inv_status = 0;
    if (::waitpid(inv_child, &inv_status, 0) != inv_child ||
        !WIFSIGNALED(inv_status) || WTERMSIG(inv_status) != SIGKILL) {
      return Fail("INV-1 child did not die by SIGKILL");
    }

    auto successor = neocortex::proj::ProjectionStore::Open(inv_path);
    if (!successor) {
      return Fail("INV-1 successor store open failed");
    }
    auto successor_snapshot = successor->BeginSnapshot();
    if (!successor_snapshot) {
      return Fail("INV-1 successor snapshot failed");
    }
    auto activation = neocortex::compose::Composer::Activate(
        *successor_snapshot, Request(conversation, 1'000'000));
    if (!activation || activation->sections[3].items.size() != prefix) {
      return Fail("INV-1 successor lost conversation evidence");
    }
    const bool loop_a_open = prefix >= 2 && prefix < 12;
    const bool loop_b_open = prefix >= 8;
    const auto expected_intent_items =
        1U + static_cast<std::size_t>(loop_a_open) +
        static_cast<std::size_t>(loop_b_open);
    const auto expected_work =
        (prefix >= 4 ? 1U : 0U) + (prefix >= 5 ? 1U : 0U) +
        (prefix >= 9 ? 1U : 0U) + (prefix >= 10 ? 1U : 0U);
    if (activation->sections[1].items.size() != expected_intent_items ||
        activation->sections[2].items.size() != expected_work) {
      return Fail("INV-1 successor lost intent or ledger evidence");
    }
    auto successor_work = neocortex::proj::WorkLedgerProjection::ReadConversation(
        *successor_snapshot, conversation);
    if (!successor_work) {
      return Fail("INV-1 successor ledger read failed");
    }
    for (const auto& item : *successor_work) {
      const bool expected_reconcile =
          (item.tool_call_lsn == 4 &&
           ((item.kind == neocortex::proj::WorkKind::kToolCall && prefix < 6) ||
            (item.kind == neocortex::proj::WorkKind::kEffect && prefix < 7))) ||
          item.tool_call_lsn == 9;
      if (item.requires_reconciliation != expected_reconcile) {
        return Fail("INV-1 successor misclassified uncertain work");
      }
    }
    const auto mandatory = activation->sections[0].tokens +
                           activation->sections[1].tokens +
                           activation->sections[2].tokens;
    auto overflow = neocortex::compose::Composer::Activate(
        *successor_snapshot, Request(conversation, mandatory + 9U * prefix));
    if (!overflow || overflow->sections[3].items.size() != prefix ||
        overflow->sections[3].coarsened_items == 0 ||
        overflow->spent_tokens > overflow->budget_tokens) {
      return Fail("INV-1 overflow coarsening dropped continuity evidence");
    }

    if (prefix == frames.size()) {
      const auto expected_sections = activation->sections;
      successor_snapshot = std::unexpected(
          neocortex::Error{neocortex::ErrorCode::kBackendUnavailable, 0});
      auto reset = successor->Reset(
          neocortex::proj::ProjectionId::kConversationHeads);
      auto rebuilt = reset ? neocortex::proj::RebuildConversationHeads(
                                 *successor, frames, false)
                           : std::expected<neocortex::proj::RebuildProgress,
                                           neocortex::Error>(
                                 std::unexpected(reset.error()));
      auto compacted_snapshot = successor->BeginSnapshot();
      auto compacted = compacted_snapshot
                           ? neocortex::compose::Composer::Activate(
                                 *compacted_snapshot,
                                 Request(conversation, 1'000'000))
                           : std::expected<
                                 neocortex::compose::ActivationBundle,
                                 neocortex::Error>(std::unexpected(
                                 compacted_snapshot.error()));
      if (!rebuilt || !compacted ||
          compacted->sections != expected_sections) {
        return Fail("INV-1 projection compaction changed briefing evidence");
      }
    }
  }
  return 0;
}

}  // namespace

int main() {
  const auto root = std::filesystem::temp_directory_path() /
                    ("neocortex-continuity-" + std::to_string(::getpid()));
  std::error_code cleanup_error;
  std::filesystem::remove_all(root, cleanup_error);
  const int result = TestContinuity(root);
  std::filesystem::remove_all(root, cleanup_error);
  return result;
}
