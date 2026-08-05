#include "compose/composer.h"

#include <algorithm>
#include <array>
#include <limits>
#include <set>
#include <utility>

#include <flatbuffers/flatbuffers.h>

#include "proj/beliefs/belief_store.h"
#include "proj/conversation_heads.h"
#include "proj/entity/entity_index.h"
#include "proj/intent/intent_frame.h"
#include "proj/ladder/temporal_ladder.h"
#include "proj/ledger/work_ledger.h"
#include "proj/lexical/bm25.h"

namespace neocortex::compose {
namespace {

constexpr std::array<char, 16> kHex = {'0', '1', '2', '3', '4', '5', '6', '7',
                                        '8', '9', 'a', 'b', 'c', 'd', 'e', 'f'};
constexpr std::size_t kMaximumCandidates = 4096;
constexpr std::size_t kMaximumConversationRecords = 16384;
constexpr std::size_t kMaximumQueryBytes =
    static_cast<std::size_t>(1024) * 1024U;

std::string Hex(std::span<const std::byte> bytes) {
  std::string output;
  output.reserve(bytes.size() * 2U);
  for (const auto value : bytes) {
    const auto raw = std::to_integer<std::uint8_t>(value);
    output.push_back(kHex[raw >> 4U]);
    output.push_back(kHex[raw & 0x0fU]);
  }
  return output;
}

void AppendU64(std::vector<std::byte>& output, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    output.push_back(
        static_cast<std::byte>((value >> (index * 8U)) & 0xffU));
  }
}

void AppendBytes(std::vector<std::byte>& output,
                 std::span<const std::byte> value) {
  AppendU64(output, value.size());
  output.insert(output.end(), value.begin(), value.end());
}

std::vector<std::uint64_t> BeliefProvenance(
    const proj::BeliefRecord& belief) {
  std::vector<std::uint64_t> provenance;
  provenance.reserve(belief.provenance.size() * 2U + 1U);
  provenance.push_back(belief.transaction_lsn);
  for (const auto& range : belief.provenance) {
    provenance.push_back(range.first_lsn);
    if (range.last_lsn != range.first_lsn) {
      provenance.push_back(range.last_lsn);
    }
  }
  std::sort(provenance.begin(), provenance.end());
  provenance.erase(std::unique(provenance.begin(), provenance.end()),
                   provenance.end());
  return provenance;
}

std::string BeliefUri(const proj::BeliefRecord& belief) {
  return "belief://" + Hex(belief.belief_id);
}

std::expected<void, Error> AddItem(ActivationBundle& bundle,
                                   const TokenModel& model, Tier tier,
                                   std::string uri,
                                   std::vector<std::uint64_t> provenance,
                                   std::vector<std::byte> content,
                                   bool coarsened = false) {
  if (model.count == nullptr || uri.empty() || provenance.empty()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto tokens = model.count(model.context, tier, uri, provenance, content);
  if (!tokens) {
    return std::unexpected(tokens.error());
  }
  const auto index = static_cast<std::size_t>(tier);
  if (index >= bundle.sections.size() ||
      *tokens > std::numeric_limits<std::size_t>::max() -
                    bundle.sections[index].tokens) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
  }
  bundle.sections[index].tokens += *tokens;
  bundle.sections[index].items.push_back(ActivationItem{
      .tier = tier,
      .uri = std::move(uri),
      .provenance = std::move(provenance),
      .content = std::move(content),
      .tokens = *tokens,
      .coarsened = coarsened,
  });
  return {};
}

std::expected<void, Error> AddBelief(ActivationBundle& bundle,
                                     const TokenModel& model, Tier tier,
                                     const proj::BeliefRecord& belief) {
  return AddItem(bundle, model, tier, BeliefUri(belief),
                 BeliefProvenance(belief), belief.value);
}

std::size_t Total(const ActivationBundle& bundle) {
  std::size_t total = 0;
  for (const auto& section : bundle.sections) {
    total += section.tokens;
  }
  return total;
}

std::expected<void, Error> Trim(ActivationBundle& bundle,
                                const ActivationRequest& request) {
  std::size_t total = Total(bundle);
  for (std::size_t tier_index = 8; tier_index > 4 &&
                                        total > request.budget_tokens;
       --tier_index) {
    auto& section = bundle.sections[tier_index - 1U];
    while (!section.items.empty() && total > request.budget_tokens) {
      auto item = std::move(section.items.back());
      section.items.pop_back();
      section.tokens -= item.tokens;
      total -= item.tokens;
      ++section.trimmed_items;
      bundle.trimmed.push_back(
          TrimmedItem{.tier = item.tier, .uri = std::move(item.uri)});
    }
  }
  auto& conversation = bundle.sections[3];
  for (auto& item : conversation.items) {
    if (total <= request.budget_tokens) {
      break;
    }
    std::vector<std::byte> compact;
    compact.reserve(9);
    AppendU64(compact, item.provenance.front());
    compact.push_back(item.content.empty() ? std::byte{0} : item.content.front());
    auto tokens = request.token_model.count(
        request.token_model.context, Tier::kConversation, item.uri,
        item.provenance, compact);
    if (!tokens) {
      return std::unexpected(tokens.error());
    }
    if (*tokens < item.tokens) {
      total -= item.tokens - *tokens;
      conversation.tokens -= item.tokens - *tokens;
      item.tokens = *tokens;
      item.content = std::move(compact);
      item.coarsened = true;
      ++conversation.coarsened_items;
    }
  }
  // A large resident or work projection must degrade to a smaller valid
  // activation, never turn memory pressure into a supervisor respawn. Optional
  // recall tiers were removed first above; only then shed resident, intent, and
  // work items while preserving explicit trim diagnostics.
  for (const std::size_t tier_index : {0U, 1U, 2U}) {
    auto& section = bundle.sections[tier_index];
    while (!section.items.empty() && total > request.budget_tokens) {
      auto item = std::move(section.items.back());
      section.items.pop_back();
      section.tokens -= item.tokens;
      total -= item.tokens;
      ++section.trimmed_items;
      bundle.trimmed.push_back(
          TrimmedItem{.tier = item.tier, .uri = std::move(item.uri)});
    }
  }
  bundle.spent_tokens = total;
  return {};
}

bool IsResident(schema::BeliefType type) {
  return type == schema::BeliefType::identity ||
         type == schema::BeliefType::constraint ||
         type == schema::BeliefType::goal;
}

bool IsPromptResident(const proj::BeliefRecord& belief) {
  if (!IsResident(belief.type)) {
    return false;
  }
  return belief.canonical_identity != "structural-self" &&
         !belief.canonical_identity.starts_with("failure:");
}

bool IsActivatableEvent(log::EventKind kind) {
  return kind == log::EventKind::kUserMsg ||
         kind == log::EventKind::kDeliveredMsg ||
         kind == log::EventKind::kAssertion;
}

std::vector<std::byte> TypedEventContent(
    const proj::ConversationRecord& record) {
  std::vector<std::byte> content;
  content.reserve(record.payload.size() + 1U);
  content.push_back(static_cast<std::byte>(record.kind));
  content.insert(content.end(), record.payload.begin(), record.payload.end());
  return content;
}

}  // namespace

std::expected<ActivationBundle, Error> Composer::Activate(
    const proj::ReadSnapshot& snapshot, const ActivationRequest& request,
    const proj::VectorLane* vector_lane) {
  if (request.budget_tokens == 0 || request.token_model.count == nullptr ||
      request.maximum_candidates == 0 ||
      request.maximum_conversation_records == 0 ||
      request.maximum_candidates > kMaximumCandidates ||
      request.maximum_conversation_records > kMaximumConversationRecords ||
      request.query.size() > kMaximumQueryBytes ||
      request.turn_text.size() > kMaximumQueryBytes ||
      (!request.query_embedding.empty() && vector_lane == nullptr)) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto applied_lsn = snapshot.Checkpoint(proj::ProjectionId::kConversationHeads);
  if (!applied_lsn) {
    return std::unexpected(applied_lsn.error());
  }
  ActivationBundle bundle{
      .snapshot_epoch = *applied_lsn,
      .budget_tokens = request.budget_tokens,
      .spent_tokens = 0,
      .sections = {},
      .trimmed = {},
  };
  for (std::size_t index = 0; index < bundle.sections.size(); ++index) {
    bundle.sections[index].tier = static_cast<Tier>(index);
  }

  auto beliefs = proj::BeliefProjection::ReadHeads(snapshot,
                                                    request.maximum_candidates);
  if (!beliefs) {
    return std::unexpected(beliefs.error());
  }
  for (const auto& belief : *beliefs) {
    if (IsPromptResident(belief)) {
      auto added = AddBelief(bundle, request.token_model, Tier::kResident,
                             belief);
      if (!added) {
        return std::unexpected(added.error());
      }
    }
  }

  // Current objectives, open loops, pending effects, and tool state are owned
  // by the authoritative turn checkpoint. They remain available to recovery
  // code but are never duplicated into ordinary cognitive activation.

  auto entity_hits = proj::EntityProjection::Query(
      snapshot, request.query, request.turn_text);
  if (!entity_hits) {
    return std::unexpected(entity_hits.error());
  }
  std::set<std::uint64_t> surfaced_lsns;
  std::set<std::uint64_t> entity_lsns;
  for (const auto& hit : *entity_hits) {
    entity_lsns.insert(hit.lsn);
  }
  std::size_t entity_count = 0;
  for (const auto lsn : entity_lsns) {
    if (entity_count >= request.maximum_candidates) {
      break;
    }
    auto record = proj::ReadConversationRecord(snapshot, lsn);
    if (!record) {
      return std::unexpected(record.error());
    }
    auto maybe_record = std::move(*record);
    if (!maybe_record.has_value()) {
      return std::unexpected(Error{ErrorCode::kInvariantViolation, 0, lsn});
    }
    // The live transcript is the only source for current-conversation turns.
    // Ambient entity activation may add only a semantic assertion owned by
    // this conversation, never a duplicated user/assistant turn.
    if (maybe_record->conversation != request.conversation ||
        maybe_record->kind != log::EventKind::kAssertion) {
      continue;
    }
    auto added = AddItem(bundle, request.token_model, Tier::kEntity,
                         "event://" + std::to_string(lsn), {lsn},
                         TypedEventContent(*maybe_record));
    if (!added) {
      return std::unexpected(added.error());
    }
    surfaced_lsns.insert(lsn);
    ++entity_count;
  }

  auto lexical = proj::LexicalProjection::Query(
      snapshot, request.query, request.maximum_candidates);
  if (!lexical) {
    return std::unexpected(lexical.error());
  }
  std::vector<proj::VectorHit> vectors;
  if (vector_lane != nullptr && !request.query_embedding.empty()) {
    auto found = vector_lane->Search(request.query_embedding,
                                     request.query_binary_prefilter,
                                     request.maximum_candidates);
    if (!found) {
      return std::unexpected(found.error());
    }
    vectors = std::move(*found);
  }
  const auto fused = proj::LexicalProjection::Fuse(
      vectors, *lexical,
      proj::FusionOptions{.limit = request.maximum_candidates});
  for (const auto& hit : fused) {
    auto record = proj::ReadConversationRecord(snapshot, hit.lsn);
    if (!record) {
      return std::unexpected(record.error());
    }
    auto maybe_record = std::move(*record);
    if (!maybe_record.has_value()) {
      continue;
    }
    if (maybe_record->conversation == request.conversation ||
        !IsActivatableEvent(maybe_record->kind)) {
      continue;
    }
    const auto source = std::to_string(
        static_cast<std::uint8_t>(maybe_record->kind));
    const auto reason = hit.vector_rank != 0 && hit.lexical_rank != 0
                            ? "vector_and_lexical"
                        : hit.vector_rank != 0 ? "vector"
                                               : "lexical";
    auto added = AddItem(bundle, request.token_model, Tier::kFused,
                         "recall://" + Hex(maybe_record->conversation.bytes) +
                             "/" + std::to_string(hit.lsn) + "?at_ns=" +
                             std::to_string(maybe_record->wall_timestamp_ns) +
                             "&source=" + source + "&score=" +
                             std::to_string(hit.rrf_score) + "&vector_rank=" +
                             std::to_string(hit.vector_rank) +
                             "&lexical_rank=" +
                             std::to_string(hit.lexical_rank) + "&why=" + reason,
                         {hit.lsn}, TypedEventContent(*maybe_record));
    if (!added) {
      return std::unexpected(added.error());
    }
    surfaced_lsns.insert(hit.lsn);
  }

  for (const auto& belief : *beliefs) {
    const auto provenance = BeliefProvenance(belief);
    const bool surfaced = std::ranges::any_of(provenance, [&](std::uint64_t lsn) {
      return surfaced_lsns.contains(lsn);
    });
    const bool active_conflict = std::ranges::any_of(
        belief.conflict_edges, [](const proj::BeliefConflictEdge& edge) {
          return edge.obligated_surfacing && edge.resolved_lsn == 0;
        });
    if (surfaced && active_conflict) {
      auto added = AddBelief(bundle, request.token_model, Tier::kConflicts,
                             belief);
      if (!added) {
        return std::unexpected(added.error());
      }
      for (const auto& edge : belief.conflict_edges) {
        if (!edge.obligated_surfacing || edge.resolved_lsn != 0) {
          continue;
        }
        auto other = proj::BeliefProjection::ReadAsOf(
            snapshot, edge.other_type, edge.other_canonical_identity,
            proj::BeliefAsOf{.valid_time_ns = request.temporal_to_ns,
                             .transaction_lsn = edge.created_lsn});
        if (!other) {
          return std::unexpected(other.error());
        }
        auto maybe_other = std::move(*other);
        if (maybe_other.has_value()) {
          auto other_added = AddBelief(bundle, request.token_model,
                                       Tier::kConflicts, *maybe_other);
          if (!other_added) {
            return std::unexpected(other_added.error());
          }
        }
      }
    }
  }

  if (request.temporal_from_ns < request.temporal_to_ns) {
    auto windows = proj::TemporalLadder::ListWindows(
        snapshot, proj::TemporalLevel::kWeek, request.temporal_from_ns,
        request.temporal_to_ns, request.maximum_candidates);
    if (!windows) {
      return std::unexpected(windows.error());
    }
    for (const auto& window : *windows) {
      auto members = proj::TemporalLadder::ResolveMembers(
          snapshot, window, request.maximum_candidates);
      if (!members) {
        return std::unexpected(members.error());
      }
      if (members->empty()) {
        continue;
      }
      std::vector<std::byte> content;
      AppendU64(content, static_cast<std::uint64_t>(window.start_ns));
      AppendU64(content, static_cast<std::uint64_t>(window.end_ns));
      AppendU64(content, window.member_count);
      auto added = AddItem(
          bundle, request.token_model, Tier::kTemporal,
          "time://week/" + std::to_string(window.start_ns),
          std::move(*members), std::move(content));
      if (!added) {
        return std::unexpected(added.error());
      }
    }
  }

  auto trimmed = Trim(bundle, request);
  if (!trimmed) {
    return std::unexpected(trimmed.error());
  }
  return bundle;
}

std::expected<std::vector<std::byte>, Error> Composer::CanonicalBytes(
    const ActivationBundle& bundle) {
  if (bundle.sections.size() != 8 || bundle.spent_tokens > bundle.budget_tokens) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
  }
  std::vector<std::byte> output = {std::byte{'N'}, std::byte{'C'},
                                  std::byte{'A'}, std::byte{'1'}};
  AppendU64(output, bundle.snapshot_epoch);
  AppendU64(output, bundle.budget_tokens);
  AppendU64(output, bundle.spent_tokens);
  for (std::size_t index = 0; index < bundle.sections.size(); ++index) {
    const auto& section = bundle.sections[index];
    if (section.tier != static_cast<Tier>(index)) {
      return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
    }
    output.push_back(static_cast<std::byte>(section.tier));
    AppendU64(output, section.tokens);
    AppendU64(output, section.trimmed_items);
    AppendU64(output, section.coarsened_items);
    AppendU64(output, section.items.size());
    for (const auto& item : section.items) {
      output.push_back(static_cast<std::byte>(item.tier));
      output.push_back(item.coarsened ? std::byte{1} : std::byte{0});
      AppendU64(output, item.tokens);
      AppendBytes(output, {reinterpret_cast<const std::byte*>(item.uri.data()),
                           item.uri.size()});
      AppendU64(output, item.provenance.size());
      for (const auto lsn : item.provenance) {
        AppendU64(output, lsn);
      }
      AppendBytes(output, item.content);
    }
  }
  AppendU64(output, bundle.trimmed.size());
  for (const auto& trimmed : bundle.trimmed) {
    output.push_back(static_cast<std::byte>(trimmed.tier));
    AppendBytes(output,
                {reinterpret_cast<const std::byte*>(trimmed.uri.data()),
                 trimmed.uri.size()});
  }
  return output;
}

std::expected<std::vector<log::Frame>, Error> Composer::BuildAttestations(
    const ActivationBundle& bundle, const AttestationRequest& request) {
  if (request.first_lsn == 0 ||
      static_cast<std::uint8_t>(request.disposition) >
          static_cast<std::uint8_t>(schema::AttestationDisposition::ignored)) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  std::set<std::uint64_t> targets;
  for (const auto& section : bundle.sections) {
    for (const auto& item : section.items) {
      targets.insert(item.provenance.begin(), item.provenance.end());
    }
  }
  if (targets.size() >
      std::numeric_limits<std::uint64_t>::max() - request.first_lsn) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
  }
  std::vector<log::Frame> frames;
  frames.reserve(targets.size());
  std::uint64_t lsn = request.first_lsn;
  for (const auto target : targets) {
    flatbuffers::FlatBufferBuilder builder;
    const auto payload =
        schema::CreateAttestation(builder, target, request.disposition);
    const auto envelope = schema::CreateEventEnvelope(
        builder, 1, schema::EventPayload::Attestation, payload.Union());
    schema::FinishEventEnvelopeBuffer(builder, envelope);
    frames.push_back(log::Frame{
        .header = log::FrameHeader{.lsn = lsn,
                                   .kind = log::EventKind::kAttestation,
                                   .wall_timestamp_ns = request.wall_timestamp_ns,
                                   .actor = request.actor,
                                   .conversation = request.conversation},
        .sealed_payload = {
            reinterpret_cast<const std::byte*>(builder.GetBufferPointer()),
            reinterpret_cast<const std::byte*>(builder.GetBufferPointer()) +
                builder.GetSize()},
    });
    ++lsn;
  }
  return frames;
}

}  // namespace neocortex::compose
