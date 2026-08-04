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
#include <unistd.h>

#include "compose/composer.h"
#include "proj/beliefs/belief_store.h"
#include "proj/conversation_heads.h"
#include "proj/entity/entity_index.h"
#include "proj/intent/intent_frame.h"
#include "proj/ladder/temporal_ladder.h"
#include "proj/ledger/work_ledger.h"
#include "proj/lexical/bm25.h"
#include "schema/events.h"

namespace {

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
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

flatbuffers::Offset<flatbuffers::Vector<std::uint8_t>> Vector(
    flatbuffers::FlatBufferBuilder& builder, std::string_view value) {
  return builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(value.data()), value.size());
}

struct AssertionInput final {
  std::uint64_t lsn;
  neocortex::schema::BeliefType type;
  std::string_view id;
  std::string_view identity;
  std::string_view value;
  std::string_view domain;
};

std::vector<std::byte> Assertion(AssertionInput input) {
  flatbuffers::FlatBufferBuilder builder;
  const auto id_data = Vector(builder, input.id);
  const auto identity_data = builder.CreateString(input.identity);
  const auto value_data = Vector(builder, input.value);
  const neocortex::schema::ProvenanceRange range(input.lsn, input.lsn);
  const auto provenance = builder.CreateVectorOfStructs(&range, 1);
  const auto domain_data =
      input.domain.empty() ? flatbuffers::Offset<flatbuffers::String>{}
                           : builder.CreateString(input.domain);
  return Finish(
      builder, neocortex::schema::EventPayload::Assertion,
      neocortex::schema::CreateAssertion(
          builder, id_data, input.type, identity_data, value_data, 1, 0,
          provenance,
          domain_data, neocortex::schema::AssertionClaim::affirmative)
          .Union());
}

std::vector<std::byte> Intent(std::string_view objective) {
  flatbuffers::FlatBufferBuilder builder;
  return Finish(builder, neocortex::schema::EventPayload::IntentSet,
                neocortex::schema::CreateIntentSet(builder,
                                                    Vector(builder, objective))
                    .Union());
}

struct LoopInput final {
  std::string_view id;
  std::string_view objective;
};

std::vector<std::byte> Loop(LoopInput input) {
  flatbuffers::FlatBufferBuilder builder;
  const auto id_data = Vector(builder, input.id);
  const auto objective_data = Vector(builder, input.objective);
  return Finish(builder, neocortex::schema::EventPayload::LoopOpened,
                neocortex::schema::CreateLoopOpened(builder, id_data,
                                                     objective_data)
                    .Union());
}

std::vector<std::byte> Message(neocortex::schema::EventPayload type,
                               std::string_view content) {
  flatbuffers::FlatBufferBuilder builder;
  const auto data = Vector(builder, content);
  if (type == neocortex::schema::EventPayload::UserMsg) {
    return Finish(builder, type,
                  neocortex::schema::CreateUserMsg(builder, data).Union());
  }
  return Finish(builder, type,
                neocortex::schema::CreateDeliveredMsg(builder, data).Union());
}

std::vector<std::byte> ToolCall() {
  flatbuffers::FlatBufferBuilder builder;
  const auto id = Vector(builder, "call-a");
  const auto name = builder.CreateString("lookup");
  const auto arguments = Vector(builder, "moltbook.com");
  return Finish(builder, neocortex::schema::EventPayload::ToolCall,
                neocortex::schema::CreateToolCall(builder, id, name, arguments)
                    .Union());
}

std::vector<std::byte> Effect() {
  flatbuffers::FlatBufferBuilder builder;
  return Finish(
      builder, neocortex::schema::EventPayload::Effect,
      neocortex::schema::CreateEffect(
          builder, Vector(builder, "effect-a"), 6,
          neocortex::schema::EffectState::dispatched)
          .Union());
}

std::vector<std::byte> ToolResult() {
  flatbuffers::FlatBufferBuilder builder;
  const auto id = Vector(builder, "call-a");
  const auto result = Vector(builder, "found");
  return Finish(builder, neocortex::schema::EventPayload::ToolResult,
                neocortex::schema::CreateToolResult(
                    builder, id, 6, neocortex::schema::ResultStatus::ok, result)
                    .Union());
}

std::vector<std::byte> Outcome() {
  flatbuffers::FlatBufferBuilder builder;
  const auto id = Vector(builder, "effect-a");
  const auto detail = Vector(builder, "committed");
  return Finish(builder, neocortex::schema::EventPayload::Outcome,
                neocortex::schema::CreateOutcome(
                    builder, id, neocortex::schema::ResultStatus::ok, detail)
                    .Union());
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
          .actor = 19,
          .conversation = conversation,
      },
      .sealed_payload = std::move(payload),
  };
}

std::vector<neocortex::log::Frame> Workload(
    neocortex::log::ConversationId conversation) {
  std::vector<neocortex::log::Frame> frames;
  frames.push_back(Frame(1, neocortex::log::EventKind::kAssertion, conversation,
                         Assertion({1, neocortex::schema::BeliefType::identity,
                                    "identity-1", "neo.identity", "Neo", ""})));
  frames.push_back(Frame(2, neocortex::log::EventKind::kIntentSet, conversation,
                         Intent("answer accurately")));
  frames.push_back(Frame(3, neocortex::log::EventKind::kLoopOpened, conversation,
                         Loop({"loop-1", "finish response"})));
  frames.push_back(Frame(4, neocortex::log::EventKind::kUserMsg, conversation,
                         Message(neocortex::schema::EventPayload::UserMsg,
                                 "inspect moltbook.com alpha")));
  frames.push_back(Frame(5, neocortex::log::EventKind::kDeliveredMsg,
                         conversation,
                         Message(neocortex::schema::EventPayload::DeliveredMsg,
                                 "working")));
  frames.push_back(Frame(6, neocortex::log::EventKind::kToolCall, conversation,
                         ToolCall()));
  frames.push_back(Frame(7, neocortex::log::EventKind::kEffect, conversation,
                         Effect()));
  frames.push_back(Frame(8, neocortex::log::EventKind::kToolResult, conversation,
                         ToolResult()));
  frames.push_back(Frame(9, neocortex::log::EventKind::kOutcome, conversation,
                         Outcome()));
  frames.push_back(Frame(10, neocortex::log::EventKind::kAssertion, conversation,
                         Assertion({10, neocortex::schema::BeliefType::fact,
                                    "fact-a", "alpha", "alpha moltbook.com",
                                    "subject"})));
  frames.push_back(Frame(11, neocortex::log::EventKind::kAssertion, conversation,
                         Assertion({11, neocortex::schema::BeliefType::fact,
                                    "fact-b", "beta", "beta replacement",
                                    "subject"})));
  return frames;
}

std::expected<std::size_t, neocortex::Error> CountTokens(
    void*, neocortex::compose::Tier, std::string_view,
    std::span<const std::uint64_t>, std::span<const std::byte> content) {
  return std::max<std::size_t>(1, content.size());
}

std::uint64_t Fnv1a(std::span<const std::byte> bytes) {
  std::uint64_t hash = 1469598103934665603ULL;
  for (const auto value : bytes) {
    hash ^= std::to_integer<std::uint8_t>(value);
    hash *= 1099511628211ULL;
  }
  return hash;
}

int BuildProjections(neocortex::proj::ProjectionStore& store,
                     std::span<const neocortex::log::Frame> frames) {
  auto conversation =
      neocortex::proj::RebuildConversationHeads(store, frames, false);
  auto beliefs = neocortex::proj::BeliefProjection::Rebuild(store, frames, false);
  auto entities = neocortex::proj::EntityProjection::Rebuild(store, frames);
  auto lexical = neocortex::proj::LexicalProjection::Rebuild(store, frames);
  auto ladder = neocortex::proj::TemporalLadder::Rebuild(store, frames);
  auto intent = neocortex::proj::IntentFrameProjection::Rebuild(store, frames);
  auto ledger = neocortex::proj::WorkLedgerProjection::Rebuild(store, frames);
  if (!conversation || !beliefs || !entities || !lexical || !ladder ||
      !intent || !ledger) {
    return Fail("composer projection rebuild failed");
  }
  return 0;
}

int TestComposer(const std::filesystem::path& path) {
  neocortex::log::ConversationId conversation{};
  conversation.bytes[0] = std::byte{0x33};
  const auto frames = Workload(conversation);
  auto store = neocortex::proj::ProjectionStore::Open(path);
  if (!store || BuildProjections(*store, frames) != 0) {
    return 1;
  }
  auto snapshot = store->BeginSnapshot();
  if (!snapshot) {
    return Fail("composer snapshot failed");
  }
  const neocortex::compose::TokenModel token_model{
      .context = nullptr,
      .count = CountTokens,
  };
  neocortex::compose::ActivationRequest request{
      .conversation = conversation,
      .query = "alpha moltbook.com",
      .turn_text = "inspect alpha",
      .budget_tokens = 1'000'000,
      .token_model = token_model,
      .query_embedding = {},
      .query_binary_prefilter = {},
      .temporal_from_ns = 0,
      .temporal_to_ns = 100'000,
      .maximum_candidates = 64,
      .maximum_conversation_records = 64,
  };
  auto first = neocortex::compose::Composer::Activate(*snapshot, request);
  auto second = neocortex::compose::Composer::Activate(*snapshot, request);
  auto canonical = first
                       ? neocortex::compose::Composer::CanonicalBytes(*first)
                       : std::expected<std::vector<std::byte>, neocortex::Error>(
                             std::unexpected(first.error()));
  if (!first || !second || *first != *second || first->spent_tokens == 0 ||
      first->sections[0].items.size() != 1 ||
      first->sections[1].items.size() != 2 ||
      first->sections[2].items.size() != 2 ||
      first->sections[3].items.size() != frames.size() ||
      first->sections[4].items.empty() || first->sections[5].items.size() != 2 ||
      first->sections[6].items.empty() || first->sections[7].items.empty() ||
      !canonical) {
    return Fail("composer tier order, sources, or determinism failed");
  }
  if (Fnv1a(*canonical) != 17579792627312296872ULL) {
    return Fail("composer canonical golden changed");
  }
  for (std::size_t index = 0; index < first->sections.size(); ++index) {
    if (first->sections[index].tier !=
        static_cast<neocortex::compose::Tier>(index)) {
      return Fail("composer section tier order changed");
    }
  }
  const auto mandatory = first->sections[0].tokens + first->sections[1].tokens +
                         first->sections[2].tokens;
  request.budget_tokens = mandatory + 9U * first->sections[3].items.size();
  auto trimmed = neocortex::compose::Composer::Activate(*snapshot, request);
  if (!trimmed || trimmed->spent_tokens > request.budget_tokens ||
      trimmed->sections[0].items != first->sections[0].items ||
      trimmed->sections[1].items != first->sections[1].items ||
      trimmed->sections[2].items != first->sections[2].items ||
      trimmed->sections[3].items.size() != frames.size() ||
      trimmed->sections[3].coarsened_items == 0 ||
      !trimmed->sections[4].items.empty() ||
      !trimmed->sections[5].items.empty() ||
      !trimmed->sections[6].items.empty() ||
      !trimmed->sections[7].items.empty()) {
    return Fail("composer trim law violated a protected tier");
  }
  request.budget_tokens = mandatory - 1U;
  auto impossible = neocortex::compose::Composer::Activate(*snapshot, request);
  if (impossible ||
      impossible.error().code != neocortex::ErrorCode::kInvariantViolation) {
    return Fail("oversized mandatory context did not fail as an invariant");
  }

  auto attestations = neocortex::compose::Composer::BuildAttestations(
      *first,
      neocortex::compose::AttestationRequest{
          .first_lsn = 100,
          .actor = 19,
          .conversation = conversation,
          .wall_timestamp_ns = 999,
          .disposition = neocortex::schema::AttestationDisposition::used});
  if (!attestations || attestations->empty()) {
    return Fail("surfaced-memory attestations were not produced");
  }
  for (std::size_t index = 0; index < attestations->size(); ++index) {
    auto verified = neocortex::events::VerifyEvent(
        (*attestations)[index].sealed_payload,
        neocortex::log::EventKind::kAttestation,
        neocortex::events::Boundary::kSocket);
    if (!verified || (*attestations)[index].header.lsn != 100U + index) {
      return Fail("attestation event failed schema verification");
    }
  }
  return 0;
}

}  // namespace

int main() {
  const auto path = std::filesystem::temp_directory_path() /
                    ("neocortex-composer-" + std::to_string(::getpid()));
  std::error_code cleanup_error;
  std::filesystem::remove_all(path, cleanup_error);
  const int result = TestComposer(path);
  std::filesystem::remove_all(path, cleanup_error);
  return result;
}
