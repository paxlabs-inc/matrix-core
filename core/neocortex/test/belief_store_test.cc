#include <algorithm>
#include <array>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <filesystem>
#include <limits>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include <flatbuffers/flatbuffers.h>
#include <unistd.h>

#include "event_fixture.h"
#include "proj/beliefs/belief_store.h"
#include "schema/events.h"

namespace {

struct AssertionSpec final {
  std::uint16_t id;
  std::string_view identity;
  std::string_view value;
  std::int64_t valid_from_ns;
  std::int64_t valid_to_ns;
  std::uint64_t provenance_first;
  std::uint64_t provenance_last;
  neocortex::schema::BeliefType type = neocortex::schema::BeliefType::fact;
  std::string_view conflict_domain{};
  neocortex::schema::AssertionClaim claim =
      neocortex::schema::AssertionClaim::affirmative;
};

struct RetractionSpec final {
  std::uint16_t id;
  std::uint64_t provenance_lsn;
};

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

std::array<std::uint8_t, 2> IdBytes(std::uint16_t id) {
  return {static_cast<std::uint8_t>(id & 0xffU),
          static_cast<std::uint8_t>((id >> 8U) & 0xffU)};
}

std::vector<std::byte> Finish(flatbuffers::FlatBufferBuilder& builder,
                              neocortex::schema::EventPayload payload_type,
                              flatbuffers::Offset<void> payload) {
  const auto envelope = neocortex::schema::CreateEventEnvelope(
      builder, 1, payload_type, payload);
  neocortex::schema::FinishEventEnvelopeBuffer(builder, envelope);
  return {reinterpret_cast<const std::byte*>(builder.GetBufferPointer()),
          reinterpret_cast<const std::byte*>(builder.GetBufferPointer()) +
              builder.GetSize()};
}

flatbuffers::Offset<neocortex::schema::Assertion> AddAssertion(
    flatbuffers::FlatBufferBuilder& builder, const AssertionSpec& spec,
    bool provenance_present = true) {
  const auto id_bytes = IdBytes(spec.id);
  const auto id = builder.CreateVector(id_bytes.data(), id_bytes.size());
  const auto identity = builder.CreateString(spec.identity);
  const auto conflict_domain =
      spec.conflict_domain.empty()
          ? flatbuffers::Offset<flatbuffers::String>{}
          : builder.CreateString(spec.conflict_domain);
  const auto value = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(spec.value.data()),
      spec.value.size());
  const neocortex::schema::ProvenanceRange range(spec.provenance_first,
                                                  spec.provenance_last);
  const auto provenance = provenance_present
                              ? builder.CreateVectorOfStructs(&range, 1)
                              : builder.CreateVectorOfStructs(
                                    static_cast<const neocortex::schema::ProvenanceRange*>(
                                        nullptr),
                                    0);
  return neocortex::schema::CreateAssertion(
      builder, id, spec.type, identity, value, spec.valid_from_ns,
      spec.valid_to_ns, provenance, conflict_domain, spec.claim);
}

std::vector<std::byte> BuildAssertion(const AssertionSpec& spec,
                                      bool provenance_present = true) {
  flatbuffers::FlatBufferBuilder builder(256);
  const auto assertion = AddAssertion(builder, spec, provenance_present);
  return Finish(builder, neocortex::schema::EventPayload::Assertion,
                assertion.Union());
}

std::vector<std::byte> BuildConsolidation(const AssertionSpec& first,
                                          const AssertionSpec& second) {
  flatbuffers::FlatBufferBuilder builder(512);
  const auto first_assertion = AddAssertion(builder, first);
  const auto second_assertion = AddAssertion(builder, second);
  const std::array assertions = {first_assertion, second_assertion};
  const auto values = builder.CreateVector(assertions.data(), assertions.size());
  const auto consolidation =
      neocortex::schema::CreateConsolidation(builder, values);
  return Finish(builder, neocortex::schema::EventPayload::Consolidation,
                consolidation.Union());
}

std::vector<std::byte> BuildRetraction(RetractionSpec spec) {
  flatbuffers::FlatBufferBuilder builder(192);
  const auto id_bytes = IdBytes(spec.id);
  const auto belief_id = builder.CreateVector(id_bytes.data(), id_bytes.size());
  const neocortex::schema::ProvenanceRange range(spec.provenance_lsn,
                                                  spec.provenance_lsn);
  const auto provenance = builder.CreateVectorOfStructs(&range, 1);
  const auto retraction =
      neocortex::schema::CreateRetract(builder, belief_id, provenance);
  return Finish(builder, neocortex::schema::EventPayload::Retract,
                retraction.Union());
}

std::vector<std::byte> BuildToolResult(
    neocortex::schema::ResultStatus status) {
  flatbuffers::FlatBufferBuilder builder(192);
  const std::uint8_t call_id_value = 0x5a;
  const std::uint8_t result_value = 0x31;
  const auto call_id = builder.CreateVector(&call_id_value, 1);
  const auto result = builder.CreateVector(&result_value, 1);
  const auto tool_result = neocortex::schema::CreateToolResult(
      builder, call_id, 1, status, result);
  return Finish(builder, neocortex::schema::EventPayload::ToolResult,
                tool_result.Union());
}

neocortex::log::Frame Frame(std::uint64_t lsn,
                            neocortex::log::EventKind kind,
                            std::vector<std::byte> payload) {
  return neocortex::log::Frame{
      .header = neocortex::log::FrameHeader{
          .lsn = lsn,
          .kind = kind,
          .wall_timestamp_ns = static_cast<std::int64_t>(lsn) * 1000,
          .actor = 19,
          .conversation = {},
      },
      .sealed_payload = std::move(payload),
  };
}

std::expected<std::optional<neocortex::proj::BeliefRecord>, neocortex::Error>
Read(neocortex::proj::ProjectionStore& store, std::string_view identity,
     std::int64_t valid_time_ns, std::uint64_t transaction_lsn) {
  auto snapshot = store.BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  return neocortex::proj::BeliefProjection::ReadAsOf(
      *snapshot, neocortex::schema::BeliefType::fact, identity,
      neocortex::proj::BeliefAsOf{.valid_time_ns = valid_time_ns,
                                  .transaction_lsn = transaction_lsn});
}

std::expected<std::vector<std::byte>, neocortex::Error> Dump(
    neocortex::proj::ProjectionStore& store) {
  auto snapshot = store.BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  return snapshot->CanonicalDump(neocortex::proj::ProjectionId::kBeliefStore);
}

int TestTypedUpsertAndBitemporality(const std::filesystem::path& path) {
  auto store = neocortex::proj::ProjectionStore::Open(path);
  if (!store) {
    return Fail("belief store open failed");
  }
  auto direct = store->Apply(neocortex::proj::ProjectionId::kBeliefStore, 1,
                             std::span<const neocortex::proj::Mutation>{});
  if (direct || direct.error().code != neocortex::ErrorCode::kBeliefWriteGate) {
    return Fail("direct belief mutation bypassed the event gate");
  }

  const AssertionSpec first{.id = 101,
                            .identity = "person:primary-name",
                            .value = "Ada",
                            .valid_from_ns = 100,
                            .valid_to_ns = 0,
                            .provenance_first = 1,
                            .provenance_last = 1};
  const AssertionSpec second{.id = 102,
                             .identity = "person:primary-name",
                             .value = "Grace",
                             .valid_from_ns = 200,
                             .valid_to_ns = 0,
                             .provenance_first = 3,
                             .provenance_last = 3};
  const AssertionSpec preference{.id = 103,
                                 .identity = "preference:editor",
                                 .value = "helix",
                                 .valid_from_ns = 0,
                                 .valid_to_ns = 0,
                                 .provenance_first = 6,
                                 .provenance_last = 6};
  const AssertionSpec goal{.id = 104,
                           .identity = "goal:ship",
                           .value = "neocortex",
                           .valid_from_ns = 0,
                           .valid_to_ns = 0,
                           .provenance_first = 6,
                           .provenance_last = 6};
  constexpr std::array<std::byte, 1> filler = {std::byte{0x41}};
  std::vector<neocortex::log::Frame> frames;
  frames.push_back(Frame(1, neocortex::log::EventKind::kAssertion,
                         BuildAssertion(first)));
  frames.push_back(Frame(2, neocortex::log::EventKind::kAssertion,
                         BuildAssertion(first)));
  frames.push_back(Frame(3, neocortex::log::EventKind::kAssertion,
                         BuildAssertion(second)));
  frames.push_back(Frame(
      4, neocortex::log::EventKind::kUserMsg,
      neocortex::test::BuildEvent(neocortex::log::EventKind::kUserMsg, 4,
                                  filler)));
  frames.push_back(Frame(5, neocortex::log::EventKind::kRetract,
                         BuildRetraction(RetractionSpec{.id = second.id,
                                                       .provenance_lsn = 5})));
  frames.push_back(Frame(6, neocortex::log::EventKind::kConsolidation,
                         BuildConsolidation(preference, goal)));

  auto rebuilt =
      neocortex::proj::BeliefProjection::Rebuild(*store, frames, false);
  if (!rebuilt || !rebuilt->complete || rebuilt->applied_lsn != frames.size()) {
    return Fail("belief event replay failed");
  }
  auto at_first = Read(*store, first.identity, 150, 1);
  auto after_duplicate = Read(*store, first.identity, 150, 2);
  auto historical = Read(*store, first.identity, 150, 3);
  auto superseded = Read(*store, first.identity, 250, 3);
  auto retracted = Read(*store, first.identity, 250, 5);
  auto consolidated_preference = Read(*store, preference.identity, 1, 6);
  auto consolidated_goal = Read(*store, goal.identity, 1, 6);
  const auto first_record =
      at_first ? at_first->value_or(neocortex::proj::BeliefRecord{})
               : neocortex::proj::BeliefRecord{};
  const auto duplicate_record =
      after_duplicate
          ? after_duplicate->value_or(neocortex::proj::BeliefRecord{})
          : neocortex::proj::BeliefRecord{};
  const auto historical_record =
      historical ? historical->value_or(neocortex::proj::BeliefRecord{})
                 : neocortex::proj::BeliefRecord{};
  const auto superseded_record =
      superseded ? superseded->value_or(neocortex::proj::BeliefRecord{})
                 : neocortex::proj::BeliefRecord{};
  if (!at_first || !at_first->has_value() || first_record.value !=
                                    std::vector<std::byte>{std::byte{'A'},
                                                           std::byte{'d'},
                                                           std::byte{'a'}} ||
      first_record.version != 1 || !after_duplicate ||
      !after_duplicate->has_value() || duplicate_record.version != 1 ||
      !historical || !historical->has_value() || historical_record.version != 1 ||
      !superseded || !superseded->has_value() || superseded_record.version != 2 ||
      superseded_record.supersedes_version != 1 ||
      superseded_record.transaction_lsn != 3 || !retracted ||
      retracted->has_value() || !consolidated_preference ||
      !consolidated_preference->has_value() || !consolidated_goal ||
      !consolidated_goal->has_value()) {
    return Fail("typed upsert, as_of, or tombstone semantics failed");
  }
  auto original = Dump(*store);
  auto replayed =
      neocortex::proj::BeliefProjection::Rebuild(*store, frames, true);
  auto replay_dump = Dump(*store);
  if (!original || !replayed || !replayed->complete || !replay_dump ||
      *original != *replay_dump) {
    return Fail("belief reset and replay was not byte-identical");
  }
  return 0;
}

int TestIdempotencyProperty(const std::filesystem::path& path) {
  auto store = neocortex::proj::ProjectionStore::Open(path);
  if (!store) {
    return Fail("property belief store open failed");
  }
  std::vector<neocortex::log::Frame> frames;
  frames.reserve(128);
  for (std::uint16_t index = 0; index < 64; ++index) {
    const std::uint64_t first_lsn = static_cast<std::uint64_t>(index) * 2U + 1U;
    const std::string identity = "property:" + std::to_string(index);
    const std::string value = "value:" + std::to_string((index * 37U) % 53U);
    const AssertionSpec spec{.id = static_cast<std::uint16_t>(1000U + index),
                             .identity = identity,
                             .value = value,
                             .valid_from_ns = index,
                             .valid_to_ns = 0,
                             .provenance_first = first_lsn,
                             .provenance_last = first_lsn};
    frames.push_back(Frame(first_lsn, neocortex::log::EventKind::kAssertion,
                           BuildAssertion(spec)));
    frames.push_back(Frame(first_lsn + 1U,
                           neocortex::log::EventKind::kAssertion,
                           BuildAssertion(spec)));
  }
  auto rebuilt =
      neocortex::proj::BeliefProjection::Rebuild(*store, frames, false);
  auto original = Dump(*store);
  if (!rebuilt || !rebuilt->complete || !original) {
    return Fail("idempotency property replay failed");
  }
  {
    auto snapshot = store->BeginSnapshot();
    if (!snapshot) {
      return Fail("idempotency property snapshot failed");
    }
    for (std::uint16_t index = 0; index < 64; ++index) {
      const std::string identity = "property:" + std::to_string(index);
      auto record = neocortex::proj::BeliefProjection::ReadAsOf(
          *snapshot, neocortex::schema::BeliefType::fact, identity,
          neocortex::proj::BeliefAsOf{
              .valid_time_ns = std::numeric_limits<std::int64_t>::max(),
              .transaction_lsn = std::numeric_limits<std::uint64_t>::max()});
      const auto stored =
          record ? record->value_or(neocortex::proj::BeliefRecord{})
                 : neocortex::proj::BeliefRecord{};
      if (!record || !record->has_value() || stored.version != 1 ||
          stored.transaction_lsn !=
              static_cast<std::uint64_t>(index) * 2U + 1U) {
        return Fail("duplicate upsert created another belief version");
      }
    }
  }
  auto replayed =
      neocortex::proj::BeliefProjection::Rebuild(*store, frames, true);
  auto replay_dump = Dump(*store);
  if (!replayed || !replayed->complete || !replay_dump ||
      *original != *replay_dump) {
    return Fail("idempotency property was not replay deterministic");
  }
  return 0;
}

int TestProvenanceAndRetractionRejection(const std::filesystem::path& path) {
  const AssertionSpec invalid{.id = 400,
                              .identity = "invalid:provenance",
                              .value = "unsafe",
                              .valid_from_ns = 0,
                              .valid_to_ns = 0,
                              .provenance_first = 1,
                              .provenance_last = 1};
  const auto encoded = BuildAssertion(invalid, false);
  auto verified = neocortex::events::VerifyEvent(
      encoded, neocortex::log::EventKind::kAssertion,
      neocortex::events::Boundary::kImport);
  if (verified || verified.error().code != neocortex::ErrorCode::kSchemaInvalid) {
    return Fail("provenance-free belief became representable");
  }
  auto store = neocortex::proj::ProjectionStore::Open(path);
  if (!store) {
    return Fail("retraction rejection store open failed");
  }
  const auto unknown_bytes =
      BuildRetraction(RetractionSpec{.id = 999, .provenance_lsn = 1});
  const neocortex::proj::AdmissionEvent unknown_admission{
      .kind = neocortex::log::EventKind::kRetract,
      .envelope = neocortex::schema::GetEventEnvelope(unknown_bytes.data()),
      .assigned_lsn = 1};
  auto admitted = neocortex::proj::BeliefProjection::AdmitBatch(
      *store, std::span(&unknown_admission, 1));
  if (admitted ||
      admitted.error().code != neocortex::ErrorCode::kBeliefNotFound) {
    return Fail("unknown belief retraction was admitted at the socket gate");
  }
  const auto unknown =
      Frame(1, neocortex::log::EventKind::kRetract, unknown_bytes);
  auto retracted = neocortex::proj::BeliefProjection::ApplyEvent(*store, unknown);
  if (!retracted) {
    return Fail("unknown belief retraction replay was not a deterministic skip");
  }
  auto snapshot = store->BeginSnapshot();
  auto checkpoint = snapshot ? snapshot->Checkpoint(
                                   neocortex::proj::ProjectionId::kBeliefStore)
                             : std::expected<std::uint64_t, neocortex::Error>(
                                   std::unexpected(neocortex::Error{
                                       neocortex::ErrorCode::kOpenFailed, 0}));
  if (!checkpoint || *checkpoint != 1) {
    return Fail("skipped retraction did not advance the belief checkpoint");
  }
  return 0;
}

int TestWriteGateIncidents(const std::filesystem::path& root) {
  const AssertionSpec uncorroborated{
      .id = 500,
      .identity = "service:moltbook:status:absent",
      .value = "nonexistent",
      .valid_from_ns = 0,
      .valid_to_ns = 0,
      .provenance_first = 1,
      .provenance_last = 1,
      .type = neocortex::schema::BeliefType::fact,
      .conflict_domain = "service:moltbook:status",
      .claim = neocortex::schema::AssertionClaim::negative_existence,
  };
  auto rejected_store =
      neocortex::proj::ProjectionStore::Open(root / "moltbook-rejected");
  if (!rejected_store) {
    return Fail("Moltbook rejection store open failed");
  }
  const auto uncorroborated_bytes = BuildAssertion(uncorroborated);
  const neocortex::proj::AdmissionEvent uncorroborated_admission{
      .kind = neocortex::log::EventKind::kAssertion,
      .envelope =
          neocortex::schema::GetEventEnvelope(uncorroborated_bytes.data()),
      .assigned_lsn = 1};
  auto rejected = neocortex::proj::BeliefProjection::AdmitBatch(
      *rejected_store, std::span(&uncorroborated_admission, 1));
  if (rejected || rejected.error().code !=
                      neocortex::ErrorCode::kNegativeExistenceUncorroborated) {
    return Fail("uncorroborated negative-existence poisoning was accepted");
  }
  auto skipped = neocortex::proj::BeliefProjection::ApplyEvent(
      *rejected_store,
      Frame(1, neocortex::log::EventKind::kAssertion, uncorroborated_bytes));
  if (!skipped) {
    return Fail("uncorroborated poisoning replay was not a deterministic skip");
  }
  auto skipped_snapshot = rejected_store->BeginSnapshot();
  auto skipped_head =
      skipped_snapshot
          ? neocortex::proj::BeliefProjection::ReadAsOf(
                *skipped_snapshot, uncorroborated.type,
                uncorroborated.identity,
                {.valid_time_ns = 1, .transaction_lsn = 1})
          : std::expected<std::optional<neocortex::proj::BeliefRecord>,
                          neocortex::Error>(
                std::unexpected(neocortex::Error{
                    neocortex::ErrorCode::kOpenFailed, 0}));
  if (!skipped_head || skipped_head->has_value()) {
    return Fail("skipped poisoning still minted a belief head");
  }

  const AssertionSpec consolidation_valid{
      .id = 501,
      .identity = "consolidation:valid-sibling",
      .value = "present",
      .valid_from_ns = 0,
      .valid_to_ns = 0,
      .provenance_first = 1,
      .provenance_last = 1,
  };
  auto consolidation_store = neocortex::proj::ProjectionStore::Open(
      root / "mixed-consolidation");
  if (!consolidation_store) {
    return Fail("mixed consolidation store open failed");
  }
  const auto consolidation_bytes =
      BuildConsolidation(consolidation_valid, uncorroborated);
  const neocortex::proj::AdmissionEvent consolidation_admission{
      .kind = neocortex::log::EventKind::kConsolidation,
      .envelope =
          neocortex::schema::GetEventEnvelope(consolidation_bytes.data()),
      .assigned_lsn = 1};
  auto consolidation_rejected = neocortex::proj::BeliefProjection::AdmitBatch(
      *consolidation_store, std::span(&consolidation_admission, 1));
  if (consolidation_rejected ||
      consolidation_rejected.error().code !=
          neocortex::ErrorCode::kNegativeExistenceUncorroborated) {
    return Fail("mixed consolidation was admitted");
  }
  auto consolidation_skipped = neocortex::proj::BeliefProjection::ApplyEvent(
      *consolidation_store,
      Frame(1, neocortex::log::EventKind::kConsolidation,
            consolidation_bytes));
  auto valid_sibling =
      Read(*consolidation_store, consolidation_valid.identity, 1, 1);
  auto rejected_sibling =
      Read(*consolidation_store, uncorroborated.identity, 1, 1);
  if (!consolidation_skipped || !valid_sibling || valid_sibling->has_value() ||
      !rejected_sibling || rejected_sibling->has_value()) {
    return Fail("mixed consolidation replay was not an atomic skip");
  }

  constexpr std::array<std::byte, 1> content = {std::byte{0x41}};
  auto failed_tool_store =
      neocortex::proj::ProjectionStore::Open(root / "moltbook-failed-tool");
  if (!failed_tool_store ||
      !neocortex::proj::BeliefProjection::ApplyEvent(
          *failed_tool_store,
          Frame(1, neocortex::log::EventKind::kToolCall,
                neocortex::test::BuildEvent(
                    neocortex::log::EventKind::kToolCall, 1, content))) ||
      !neocortex::proj::BeliefProjection::ApplyEvent(
          *failed_tool_store,
          Frame(2, neocortex::log::EventKind::kToolResult,
                BuildToolResult(neocortex::schema::ResultStatus::error)))) {
    return Fail("failed-tool incident prefix did not apply");
  }
  AssertionSpec failed_tool_claim = uncorroborated;
  failed_tool_claim.provenance_first = 2;
  failed_tool_claim.provenance_last = 2;
  const auto failed_tool_bytes = BuildAssertion(failed_tool_claim);
  const neocortex::proj::AdmissionEvent failed_tool_admission{
      .kind = neocortex::log::EventKind::kAssertion,
      .envelope = neocortex::schema::GetEventEnvelope(failed_tool_bytes.data()),
      .assigned_lsn = 3};
  auto failed_tool_rejection = neocortex::proj::BeliefProjection::AdmitBatch(
      *failed_tool_store, std::span(&failed_tool_admission, 1));
  if (failed_tool_rejection ||
      failed_tool_rejection.error().code !=
          neocortex::ErrorCode::kNegativeExistenceUncorroborated) {
    return Fail("failed tool result corroborated a negative-existence claim");
  }

  auto accepted_store =
      neocortex::proj::ProjectionStore::Open(root / "moltbook-accepted");
  if (!accepted_store ||
      !neocortex::proj::BeliefProjection::ApplyEvent(
          *accepted_store,
          Frame(1, neocortex::log::EventKind::kToolCall,
                neocortex::test::BuildEvent(
                    neocortex::log::EventKind::kToolCall, 1, content))) ||
      !neocortex::proj::BeliefProjection::ApplyEvent(
          *accepted_store,
          Frame(2, neocortex::log::EventKind::kToolResult,
                BuildToolResult(neocortex::schema::ResultStatus::ok)))) {
    return Fail("corroborated incident prefix did not apply");
  }
  AssertionSpec corroborated = uncorroborated;
  corroborated.provenance_first = 2;
  corroborated.provenance_last = 2;
  if (!neocortex::proj::BeliefProjection::ApplyEvent(
          *accepted_store,
          Frame(3, neocortex::log::EventKind::kAssertion,
                BuildAssertion(corroborated)))) {
    return Fail("successful tool evidence did not corroborate negative existence");
  }
  auto accepted = Read(*accepted_store, corroborated.identity, 1, 3);
  const auto accepted_record =
      accepted ? accepted->value_or(neocortex::proj::BeliefRecord{})
               : neocortex::proj::BeliefRecord{};
  if (!accepted || !accepted->has_value() ||
      accepted_record.claim !=
          neocortex::schema::AssertionClaim::negative_existence) {
    return Fail("corroborated negative-existence belief was not typed");
  }

  auto conflict_store =
      neocortex::proj::ProjectionStore::Open(root / "conflict");
  if (!conflict_store) {
    return Fail("conflict incident store open failed");
  }
  const AssertionSpec europe{
      .id = 600,
      .identity = "deployment:region:europe",
      .value = "europe",
      .valid_from_ns = 0,
      .valid_to_ns = 0,
      .provenance_first = 1,
      .provenance_last = 1,
      .type = neocortex::schema::BeliefType::fact,
      .conflict_domain = "deployment:region",
  };
  const AssertionSpec america{
      .id = 601,
      .identity = "deployment:region:america",
      .value = "america",
      .valid_from_ns = 0,
      .valid_to_ns = 0,
      .provenance_first = 2,
      .provenance_last = 2,
      .type = neocortex::schema::BeliefType::fact,
      .conflict_domain = "deployment:region",
  };
  AssertionSpec europe_update = europe;
  europe_update.id = 602;
  europe_update.value = "eu-central";
  europe_update.provenance_first = 3;
  europe_update.provenance_last = 3;
  const std::vector conflict_frames = {
      Frame(1, neocortex::log::EventKind::kAssertion, BuildAssertion(europe)),
      Frame(2, neocortex::log::EventKind::kAssertion, BuildAssertion(america)),
      Frame(3, neocortex::log::EventKind::kAssertion,
            BuildAssertion(europe_update)),
      Frame(4, neocortex::log::EventKind::kRetract,
            BuildRetraction(
                RetractionSpec{.id = america.id, .provenance_lsn = 4})),
  };
  auto conflict_replay = neocortex::proj::BeliefProjection::Rebuild(
      *conflict_store, conflict_frames, false);
  auto europe_during = Read(*conflict_store, europe.identity, 1, 3);
  auto america_during = Read(*conflict_store, america.identity, 1, 3);
  auto europe_after = Read(*conflict_store, europe.identity, 1, 4);
  auto america_after = Read(*conflict_store, america.identity, 1, 4);
  const auto europe_during_record =
      europe_during
          ? europe_during->value_or(neocortex::proj::BeliefRecord{})
          : neocortex::proj::BeliefRecord{};
  const auto america_during_record =
      america_during
          ? america_during->value_or(neocortex::proj::BeliefRecord{})
          : neocortex::proj::BeliefRecord{};
  const auto europe_after_record =
      europe_after ? europe_after->value_or(neocortex::proj::BeliefRecord{})
                   : neocortex::proj::BeliefRecord{};
  if (!conflict_replay || !conflict_replay->complete || !europe_during ||
      !europe_during->has_value() || !america_during ||
      !america_during->has_value() ||
      europe_during_record.conflict_edges.size() != 1 ||
      america_during_record.conflict_edges.size() != 1 ||
      !europe_during_record.conflict_edges.front().obligated_surfacing ||
      !america_during_record.conflict_edges.front().obligated_surfacing ||
      europe_during_record.conflict_edges.front().other_canonical_identity !=
          america.identity ||
      america_during_record.conflict_edges.front().other_canonical_identity !=
          europe.identity ||
      europe_during_record.version != 2 || !europe_after ||
      !europe_after->has_value() ||
      !europe_after_record.conflict_edges.empty() || !america_after ||
      america_after->has_value()) {
    return Fail("typed conflict edge or obligated-surfacing interval failed");
  }
  auto original = Dump(*conflict_store);
  auto rebuilt = neocortex::proj::BeliefProjection::Rebuild(
      *conflict_store, conflict_frames, true);
  auto replayed = Dump(*conflict_store);
  if (!original || !rebuilt || !rebuilt->complete || !replayed ||
      *original != *replayed) {
    return Fail("incident gate replay was not byte-identical");
  }
  return 0;
}

std::filesystem::path MakeDirectory(std::string_view suffix) {
  const auto path = std::filesystem::path("/tmp") /
                    ("neocortex-beliefs-" + std::to_string(::getpid()) + "-" +
                     std::string(suffix));
  std::error_code cleanup_error;
  std::filesystem::remove_all(path, cleanup_error);
  return path;
}

}  // namespace

int main() {
  const auto typed_path = MakeDirectory("typed");
  const auto property_path = MakeDirectory("property");
  const auto rejection_path = MakeDirectory("rejection");
  const auto incidents_path = MakeDirectory("incidents");
  if (TestTypedUpsertAndBitemporality(typed_path) != 0 ||
      TestIdempotencyProperty(property_path) != 0 ||
      TestProvenanceAndRetractionRejection(rejection_path) != 0 ||
      TestWriteGateIncidents(incidents_path) != 0) {
    return 1;
  }
  for (const auto& path :
       {typed_path, property_path, rejection_path, incidents_path}) {
    std::error_code cleanup_error;
    std::filesystem::remove_all(path, cleanup_error);
    if (cleanup_error) {
      return Fail("belief test cleanup failed");
    }
  }
  return 0;
}
