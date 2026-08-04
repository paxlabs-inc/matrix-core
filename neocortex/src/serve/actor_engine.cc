#include "serve/actor_engine.h"

#include <algorithm>
#include <iterator>
#include <limits>
#include <string_view>

#include "compose/composer.h"
#include "proj/conversation_heads.h"
#include "proj/entity/entity_index.h"
#include "proj/intent/intent_frame.h"
#include "proj/ledger/work_ledger.h"
#include "proj/lexical/bm25.h"
#include "schema/events.h"
#include "serve/protocol.h"

namespace neocortex::serve {
namespace {

constexpr std::size_t kMaximumLogicalConnections = 4096;
constexpr std::size_t kRecoveryBatchSize = 1024;
constexpr std::size_t kMaximumTurnBytes = 4096;
constexpr std::size_t kLatestCheckpointScan = 65536;
constexpr std::string_view kTurnDomain = "neocortex-turn-v1";
using ConnectionIdBytes = std::array<std::byte, 16>;

struct WireTokenModel final {
  const flatbuffers::Vector<std::uint16_t>* weights;
  std::uint32_t overhead;
};

std::expected<std::size_t, Error> CountWireTokens(
    void* context, compose::Tier, std::string_view,
    std::span<const std::uint64_t>, std::span<const std::byte> content) {
  const auto* model = static_cast<const WireTokenModel*>(context);
  std::uint64_t weighted = 0;
  for (const auto value : content) {
    weighted += (*model->weights)[std::to_integer<std::uint8_t>(value)];
  }
  const auto tokens = static_cast<std::size_t>(model->overhead) +
                      static_cast<std::size_t>((weighted + 255U) >> 8U);
  return tokens == 0 ? 1U : tokens;
}

log::ConversationId TurnConversation(std::span<const std::byte> turn_id) {
  std::vector<std::byte> material;
  material.reserve(kTurnDomain.size() + turn_id.size());
  std::transform(kTurnDomain.begin(), kTurnDomain.end(),
                 std::back_inserter(material),
                 [](char value) { return static_cast<std::byte>(value); });
  material.insert(material.end(), turn_id.begin(), turn_id.end());
  const auto hash = mmr::HashBytes(material);
  log::ConversationId conversation{};
  std::copy_n(hash.begin(), conversation.bytes.size(),
              conversation.bytes.begin());
  return conversation;
}

std::vector<std::byte> EncodeCheckpointCursor(
    std::span<const std::byte> turn_id, std::span<const std::byte> blob) {
  std::vector<std::byte> cursor;
  cursor.reserve(2U + turn_id.size() + blob.size());
  cursor.push_back(static_cast<std::byte>(turn_id.size() & 0xffU));
  cursor.push_back(static_cast<std::byte>((turn_id.size() >> 8U) & 0xffU));
  cursor.insert(cursor.end(), turn_id.begin(), turn_id.end());
  cursor.insert(cursor.end(), blob.begin(), blob.end());
  return cursor;
}

std::expected<std::span<const std::byte>, Error> DecodeCheckpointCursor(
    std::span<const std::byte> cursor, std::span<const std::byte> turn_id) {
  if (cursor.size() < 2U) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  const auto length = static_cast<std::size_t>(
      std::to_integer<std::uint8_t>(cursor[0]) |
      (std::to_integer<std::uint8_t>(cursor[1]) << 8U));
  if (length == 0 || length > cursor.size() - 2U) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  const auto encoded = cursor.subspan(2U, length);
  if (encoded.size() != turn_id.size() ||
      !std::equal(encoded.begin(), encoded.end(), turn_id.begin())) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  return cursor.subspan(2U + length);
}

struct EventIdempotency final {
  std::uint64_t client_seq;
  std::uint32_t event_index;
  std::uint32_t event_count;
};

void AppendU64(std::vector<std::byte>& output, std::uint64_t value) {
  for (std::size_t index = 0; index < sizeof(value); ++index) {
    output.push_back(
        static_cast<std::byte>((value >> (index * 8U)) & 0xffU));
  }
}

std::expected<ConnectionIdBytes, Error> ReadConnectionId(
    const flatbuffers::Vector<std::uint8_t>* value) {
  if (value == nullptr || value->size() != 16) {
    return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
  }
  ConnectionIdBytes id{};
  std::transform(value->begin(), value->end(), id.begin(),
                 [](std::uint8_t byte) { return static_cast<std::byte>(byte); });
  return id;
}

std::expected<ConnectionIdBytes, Error> ReadConnectionId(
    std::span<const std::byte> value) {
  if (value.size() != 16) {
    return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
  }
  ConnectionIdBytes id{};
  std::copy(value.begin(), value.end(), id.begin());
  return id;
}

std::expected<void, Error> CheckEnvelopeIdempotency(
    const schema::EventEnvelope& envelope,
    std::span<const std::byte> connection_id, EventIdempotency expected) {
  const auto* encoded_id = envelope.connection_id();
  if (encoded_id == nullptr || encoded_id->size() != connection_id.size() ||
      !std::equal(encoded_id->begin(), encoded_id->end(), connection_id.begin(),
                  [](std::uint8_t left, std::byte right) {
                    return left == std::to_integer<std::uint8_t>(right);
                  }) ||
      envelope.client_seq() != expected.client_seq ||
      envelope.client_event_index() != expected.event_index ||
      envelope.client_event_count() != expected.event_count) {
    return std::unexpected(Error{ErrorCode::kIdempotencyConflict, 0});
  }
  return {};
}

}  // namespace

ActorEngine::ActorEngine(ActorEngineConfig config) : config_(std::move(config)) {}

std::expected<std::unique_ptr<ActorEngine>, Error> ActorEngine::Open(
    ActorEngineConfig config) {
  auto engine = std::unique_ptr<ActorEngine>(new ActorEngine(std::move(config)));
  auto initialized = engine->Initialize();
  if (!initialized) {
    return std::unexpected(initialized.error());
  }
  return engine;
}

std::expected<void, Error> ActorEngine::Initialize() {
  auto storage = log::SegmentStorage::Open(
      config_.actor_directory, config_.actor, config_.signing_key.public_key,
      config_.user, config_.kek, entropy_,
      {.backend = log::WriteBackend::kPwrite});
  if (!storage) {
    return std::unexpected(storage.error());
  }
  storage_ = std::make_unique<log::SegmentStorage>(std::move(*storage));
  auto rolled_back = RollBackTornBatch();
  if (!rolled_back) {
    return std::unexpected(rolled_back.error());
  }
  auto loop = core::ApplyLoop::Boot(
      {.clock = clock_,
       .entropy = entropy_,
       .storage = *storage_,
       .actor = config_.actor});
  if (!loop) {
    return std::unexpected(loop.error());
  }
  apply_loop_ = std::make_unique<core::ApplyLoop>(std::move(*loop));
  auto projections = proj::ProjectionStore::Open(config_.actor_directory,
                                                  config_.projection_map_bytes);
  if (!projections) {
    return std::unexpected(projections.error());
  }
  projection_store_ =
      std::make_unique<proj::ProjectionStore>(std::move(*projections));
  auto vectors = proj::VectorLane::Open(config_.actor_directory);
  if (!vectors) {
    return std::unexpected(vectors.error());
  }
  vector_lane_ = std::make_unique<proj::VectorLane>(std::move(*vectors));
  auto rebuilt = RebuildAll(false);
  if (!rebuilt) {
    return std::unexpected(rebuilt.error());
  }
  auto dedup = RebuildDedup();
  if (!dedup) {
    return std::unexpected(dedup.error());
  }
  ready_ = true;
  return {};
}

std::expected<void, Error> ActorEngine::RollBackTornBatch() {
  const auto next = storage_->next_lsn();
  if (next <= 1U) {
    return {};
  }
  const auto tail = next - 1U;
  auto frames = storage_->ReadFrom(tail, 1);
  if (!frames) {
    return std::unexpected(frames.error());
  }
  if (frames->size() != 1 || frames->front().header.lsn != tail) {
    return std::unexpected(Error{ErrorCode::kReadFailed, 0, tail});
  }
  auto verified = events::VerifyEvent(frames->front().sealed_payload,
                                      frames->front().header.kind,
                                      events::Boundary::kDisk);
  if (!verified) {
    return std::unexpected(verified.error());
  }
  const auto& envelope = *verified->envelope;
  if (envelope.connection_id() == nullptr ||
      envelope.connection_id()->empty() ||
      envelope.client_event_count() == 0) {
    return {};
  }
  const auto index = static_cast<std::uint64_t>(envelope.client_event_index());
  const auto count = static_cast<std::uint64_t>(envelope.client_event_count());
  if (index + 1U == count) {
    return {};
  }
  if (index + 1U > count || index >= tail) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0, tail});
  }
  return storage_->TruncateTo(tail - index - 1U);
}

std::expected<void, Error> ActorEngine::ApplyFrame(const log::Frame& frame) {
  for (std::size_t index = 0; index < proj::kProjectionCount; ++index) {
    auto applied = ApplyProjection(static_cast<proj::ProjectionId>(index), frame);
    if (!applied) {
      return std::unexpected(applied.error());
    }
  }
  return {};
}

std::expected<void, Error> ActorEngine::ApplyProjection(
    proj::ProjectionId projection, const log::Frame& frame) {
  switch (projection) {
    case proj::ProjectionId::kBeliefStore:
      return proj::BeliefProjection::ApplyEvent(*projection_store_, frame);
    case proj::ProjectionId::kEntityIndex:
      return proj::EntityProjection::ApplyEvent(*projection_store_, frame);
    case proj::ProjectionId::kVectorLane:
      return vector_lane_->ApplyEvent(*projection_store_, frame);
    case proj::ProjectionId::kBm25:
      return proj::LexicalProjection::ApplyEvent(*projection_store_, frame);
    case proj::ProjectionId::kTemporalLadder:
      return proj::TemporalLadder::ApplyEvent(*projection_store_, frame);
    case proj::ProjectionId::kIntentFrame:
      return proj::IntentFrameProjection::ApplyEvent(*projection_store_, frame);
    case proj::ProjectionId::kWorkLedger:
      return proj::WorkLedgerProjection::ApplyEvent(*projection_store_, frame);
    case proj::ProjectionId::kConversationHeads:
      return proj::ApplyConversationHead(*projection_store_, frame);
    case proj::ProjectionId::kCount:
      break;
  }
  return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
}

std::expected<void, Error> ActorEngine::ResetProjection(
    proj::ProjectionId projection) {
  const std::span<const log::Frame> empty;
  switch (projection) {
    case proj::ProjectionId::kBeliefStore: {
      auto result = proj::BeliefProjection::Rebuild(*projection_store_, empty, true);
      return result ? std::expected<void, Error>{}
                    : std::unexpected(result.error());
    }
    case proj::ProjectionId::kEntityIndex: {
      auto result = proj::EntityProjection::Rebuild(*projection_store_, empty, true);
      return result ? std::expected<void, Error>{}
                    : std::unexpected(result.error());
    }
    case proj::ProjectionId::kVectorLane: {
      auto result = vector_lane_->Rebuild(*projection_store_, empty, true);
      return result ? std::expected<void, Error>{}
                    : std::unexpected(result.error());
    }
    case proj::ProjectionId::kBm25: {
      auto result = proj::LexicalProjection::Rebuild(*projection_store_, empty, true);
      return result ? std::expected<void, Error>{}
                    : std::unexpected(result.error());
    }
    case proj::ProjectionId::kTemporalLadder: {
      auto result = proj::TemporalLadder::Rebuild(*projection_store_, empty, true);
      return result ? std::expected<void, Error>{}
                    : std::unexpected(result.error());
    }
    case proj::ProjectionId::kIntentFrame: {
      auto result = proj::IntentFrameProjection::Rebuild(*projection_store_, empty, true);
      return result ? std::expected<void, Error>{}
                    : std::unexpected(result.error());
    }
    case proj::ProjectionId::kWorkLedger: {
      auto result = proj::WorkLedgerProjection::Rebuild(*projection_store_, empty, true);
      return result ? std::expected<void, Error>{}
                    : std::unexpected(result.error());
    }
    case proj::ProjectionId::kConversationHeads: {
      auto result = proj::RebuildConversationHeads(*projection_store_, empty, true);
      return result ? std::expected<void, Error>{}
                    : std::unexpected(result.error());
    }
    case proj::ProjectionId::kCount:
      break;
  }
  return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
}

std::expected<void, Error> ActorEngine::RebuildProjectionStream(
    std::optional<proj::ProjectionId> projection, bool reset) {
  std::array<std::uint64_t, proj::kProjectionCount> checkpoints{};
  if (reset) {
    for (std::size_t index = 0; index < proj::kProjectionCount; ++index) {
      const auto id = static_cast<proj::ProjectionId>(index);
      if (!projection || *projection == id) {
        auto cleared = ResetProjection(id);
        if (!cleared) {
          return std::unexpected(cleared.error());
        }
      }
    }
  }
  const auto tail = apply_loop_->state().last_lsn();
  std::uint64_t first_lsn = tail == 0 ? 1U : tail;
  {
    auto snapshot = projection_store_->BeginSnapshot();
    if (!snapshot) {
      return std::unexpected(snapshot.error());
    }
    for (std::size_t index = 0; index < proj::kProjectionCount; ++index) {
      auto checkpoint = snapshot->Checkpoint(static_cast<proj::ProjectionId>(index));
      if (!checkpoint || *checkpoint > tail) {
        return std::unexpected(checkpoint ? Error{ErrorCode::kProjectionCheckpoint,
                                                  0, *checkpoint, tail}
                                          : checkpoint.error());
      }
      checkpoints[index] = *checkpoint;
      if ((!projection || *projection == static_cast<proj::ProjectionId>(index)) &&
          *checkpoint < first_lsn) {
        first_lsn = *checkpoint;
      }
    }
  }
  if (first_lsn == tail) {
    return {};
  }
  ++first_lsn;
  while (first_lsn <= tail) {
    auto frames = storage_->ReadFrom(first_lsn, kRecoveryBatchSize);
    if (!frames) {
      return std::unexpected(frames.error());
    }
    if (frames->empty()) {
      return std::unexpected(Error{ErrorCode::kProjectionCheckpoint, 0,
                                   first_lsn, tail});
    }
    for (const auto& frame : *frames) {
      for (std::size_t index = 0; index < proj::kProjectionCount; ++index) {
        const auto id = static_cast<proj::ProjectionId>(index);
        if ((projection && *projection != id) || checkpoints[index] >= frame.header.lsn) {
          continue;
        }
        if (checkpoints[index] + 1U != frame.header.lsn) {
          return std::unexpected(Error{ErrorCode::kProjectionCheckpoint, 0,
                                       frame.header.lsn, checkpoints[index]});
        }
        auto applied = ApplyProjection(id, frame);
        if (!applied) {
          return std::unexpected(applied.error());
        }
        checkpoints[index] = frame.header.lsn;
      }
      if (frame.header.lsn == tail) {
        return {};
      }
    }
    if (frames->back().header.lsn == std::numeric_limits<std::uint64_t>::max()) {
      return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
    }
    first_lsn = frames->back().header.lsn + 1U;
  }
  return {};
}

std::expected<void, Error> ActorEngine::RebuildAll(bool reset) {
  return RebuildProjectionStream(std::nullopt, reset);
}

std::expected<mmr::Hash, Error> ActorEngine::RequestDigest(
    const protocol::Append& request) const {
  std::vector<std::byte> material;
  material.reserve(static_cast<std::size_t>(request.events()->size()) * 49U);
  for (const auto* event : *request.events()) {
    material.push_back(static_cast<std::byte>(event->kind()));
    std::transform(event->conversation()->begin(), event->conversation()->end(),
                   std::back_inserter(material), [](std::uint8_t byte) {
                     return static_cast<std::byte>(byte);
                   });
    const auto payload = std::as_bytes(std::span(event->payload()->data(),
                                                 event->payload()->size()));
    const auto hash = mmr::HashBytes(payload);
    material.insert(material.end(), hash.begin(), hash.end());
  }
  return mmr::HashBytes(material);
}

std::expected<mmr::Hash, Error> ActorEngine::FrameDigest(
    std::span<const log::Frame> frames) const {
  if (frames.empty()) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  std::vector<std::byte> material;
  material.reserve(frames.size() * 49U);
  for (const auto& frame : frames) {
    material.push_back(static_cast<std::byte>(frame.header.kind));
    material.insert(material.end(), frame.header.conversation.bytes.begin(),
                    frame.header.conversation.bytes.end());
    const auto hash = mmr::HashBytes(frame.sealed_payload);
    material.insert(material.end(), hash.begin(), hash.end());
  }
  return mmr::HashBytes(material);
}

std::expected<mmr::Hash, Error> ActorEngine::ApplyDigest(
    std::span<const core::ApplyRequest> requests) const {
  if (requests.empty()) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  std::vector<std::byte> material;
  material.reserve(requests.size() * 49U);
  for (const auto& request : requests) {
    material.push_back(static_cast<std::byte>(request.kind));
    material.insert(material.end(), request.conversation.bytes.begin(),
                    request.conversation.bytes.end());
    const auto hash = mmr::HashBytes(request.plaintext_payload);
    material.insert(material.end(), hash.begin(), hash.end());
  }
  return mmr::HashBytes(material);
}

std::expected<void, Error> ActorEngine::RebuildDedup() {
  dedup_.clear();
  std::uint64_t lsn = 1;
  const auto tail = apply_loop_->state().last_lsn();
  while (lsn <= tail) {
    auto batch = storage_->ReadFrom(lsn, kRecoveryBatchSize);
    if (!batch || batch->empty()) {
      return std::unexpected(batch ? Error{ErrorCode::kReadFailed, 0, lsn}
                                   : batch.error());
    }
    std::size_t index = 0;
    while (index < batch->size()) {
      const auto event_lsn = batch->at(index).header.lsn;
      auto verified = events::VerifyEvent(batch->at(index).sealed_payload,
                                          batch->at(index).header.kind,
                                          events::Boundary::kDisk);
      if (!verified) {
        return std::unexpected(verified.error());
      }
      const auto& envelope = *verified->envelope;
      if (envelope.connection_id() == nullptr || envelope.connection_id()->empty()) {
        ++index;
        continue;
      }
      auto connection = ReadConnectionId(envelope.connection_id());
      const auto count = static_cast<std::size_t>(envelope.client_event_count());
      if (!connection || envelope.client_seq() == 0 || count == 0 ||
          count > kMaximumBatchEvents || envelope.client_event_index() != 0 ||
          count > tail - event_lsn + 1U) {
        return std::unexpected(
            Error{ErrorCode::kIdempotencyConflict, 0, event_lsn});
      }
      std::optional<std::vector<log::Frame>> overflow;
      std::span<const log::Frame> frames;
      if (count <= batch->size() - index) {
        frames = std::span<const log::Frame>(*batch).subspan(index, count);
      } else {
        auto loaded = storage_->ReadFrom(event_lsn, count);
        if (!loaded || loaded->size() != count) {
          return std::unexpected(loaded ? Error{ErrorCode::kReadFailed, 0, event_lsn}
                                        : loaded.error());
        }
        overflow.emplace(std::move(*loaded));
        frames = *overflow;
      }
      for (std::size_t offset = 0; offset < count; ++offset) {
        auto member = events::VerifyEvent(frames[offset].sealed_payload,
                                          frames[offset].header.kind,
                                          events::Boundary::kDisk);
        if (!member) {
          return std::unexpected(member.error());
        }
      auto matching = CheckEnvelopeIdempotency(
          *member->envelope, *connection,
          {.client_seq = envelope.client_seq(),
           .event_index = static_cast<std::uint32_t>(offset),
           .event_count = static_cast<std::uint32_t>(count)});
        if (!matching) {
          return std::unexpected(matching.error());
        }
      }
      auto digest = FrameDigest(frames);
      if (!digest) {
        return std::unexpected(digest.error());
      }
      const DedupState state{.client_seq = envelope.client_seq(),
                             .first_lsn = frames.front().header.lsn,
                             .last_lsn = frames.back().header.lsn,
                             .digest = *digest};
      const auto found = dedup_.find(*connection);
      if (found != dedup_.end() &&
          state.client_seq != found->second.client_seq + 1U) {
        return std::unexpected(Error{ErrorCode::kSequenceViolation, 0,
                                     state.first_lsn});
      }
      if (found == dedup_.end() &&
          dedup_.size() >= kMaximumLogicalConnections) {
        return std::unexpected(
            Error{ErrorCode::kCapacityExceeded, 0, state.first_lsn});
      }
      dedup_[*connection] = state;
      index += count;
    }
    lsn += static_cast<std::uint64_t>(index);
  }
  return {};
}

std::expected<void, Error> ActorEngine::EnsureReady() {
  if (ready_) {
    return {};
  }
  auto recovered = RebuildAll(false);
  if (!recovered) {
    return std::unexpected(recovered.error());
  }
  auto dedup = RebuildDedup();
  if (!dedup) {
    return std::unexpected(dedup.error());
  }
  ready_ = true;
  return {};
}

std::expected<std::vector<log::Frame>, Error> ActorEngine::CommitApplyRequests(
    std::span<const core::ApplyRequest> requests) {
  auto acknowledgements = apply_loop_->ApplyBatch(requests);
  if (!acknowledgements) {
    return std::unexpected(acknowledgements.error());
  }
  std::vector<log::Frame> frames;
  frames.reserve(requests.size());
  for (std::size_t index = 0; index < requests.size(); ++index) {
    frames.push_back(log::Frame{
        .header = log::FrameHeader{
            .lsn = acknowledgements->at(index).lsn,
            .kind = requests[index].kind,
            .wall_timestamp_ns = acknowledgements->at(index).wall_timestamp_ns,
            .actor = config_.actor,
            .conversation = requests[index].conversation},
        .sealed_payload = std::vector<std::byte>(
            requests[index].plaintext_payload.begin(),
            requests[index].plaintext_payload.end())});
  }
  for (const auto& frame : frames) {
    auto applied = ApplyFrame(frame);
    if (!applied) {
      ready_ = false;
      return std::unexpected(applied.error());
    }
  }
  return frames;
}

std::expected<AppendResult, Error> ActorEngine::Append(
    std::span<const std::byte> connection_id,
    const protocol::Append& request) {
  auto ready = EnsureReady();
  if (!ready) {
    return std::unexpected(ready.error());
  }
  auto connection = ReadConnectionId(connection_id);
  if (!connection) {
    return std::unexpected(connection.error());
  }
  auto digest = RequestDigest(request);
  if (!digest) {
    return std::unexpected(digest.error());
  }
  const auto prior = dedup_.find(*connection);
  if (prior == dedup_.end() &&
      dedup_.size() >= kMaximumLogicalConnections) {
    return std::unexpected(Error{ErrorCode::kCapacityExceeded, 0});
  }
  if (prior != dedup_.end() && request.client_seq() == prior->second.client_seq) {
    if (*digest != prior->second.digest) {
      return std::unexpected(Error{ErrorCode::kIdempotencyConflict, 0});
    }
    auto stored = storage_->ReadFrom(prior->second.last_lsn, 1);
    if (!stored || stored->empty()) {
      return std::unexpected(stored ? Error{ErrorCode::kReadFailed, 0,
                                            prior->second.last_lsn}
                                    : stored.error());
    }
    const auto& status = storage_->mmr_store().verification_status();
    return AppendResult{.client_seq = request.client_seq(),
                        .first_lsn = prior->second.first_lsn,
                        .last_lsn = prior->second.last_lsn,
                        .duplicate = true,
                        .leaf_count = status.leaf_count,
                        .last_leaf_hash = mmr::HashFramePlaintext(
                            stored->front().header,
                            stored->front().sealed_payload),
                        .root = status.root,
                        .frames = {}};
  }
  const auto expected_seq =
      prior == dedup_.end() ? 1U : prior->second.client_seq + 1U;
  if (request.client_seq() != expected_seq) {
    return std::unexpected(Error{ErrorCode::kSequenceViolation, 0});
  }
  std::vector<core::ApplyRequest> apply_requests;
  apply_requests.reserve(request.events()->size());
  std::vector<proj::AdmissionEvent> admission;
  admission.reserve(request.events()->size());
  bool needs_admission = false;
  const auto event_count =
      static_cast<std::uint32_t>(request.events()->size());
  const auto tail_lsn = apply_loop_->state().last_lsn();
  for (std::size_t index = 0; index < request.events()->size(); ++index) {
    const auto* event =
        request.events()->Get(static_cast<flatbuffers::uoffset_t>(index));
    const auto kind = static_cast<log::EventKind>(event->kind());
    const auto payload = std::as_bytes(
        std::span(event->payload()->data(), event->payload()->size()));
    auto verified = events::VerifyEvent(payload, kind, events::Boundary::kSocket);
    if (!verified) {
      return std::unexpected(verified.error());
    }
    const auto assigned_lsn = tail_lsn + 1U + static_cast<std::uint64_t>(index);
    if (kind == log::EventKind::kEmbedding) {
      const auto* embedding = verified->envelope->payload_as_Embedding();
      if (embedding->target_lsn() >= assigned_lsn) {
        return std::unexpected(
            Error{ErrorCode::kInvalidArgument, 0, embedding->target_lsn()});
      }
    }
    admission.push_back(proj::AdmissionEvent{
        .kind = kind,
        .envelope = verified->envelope,
        .assigned_lsn = assigned_lsn});
    needs_admission = needs_admission ||
                      kind == log::EventKind::kAssertion ||
                      kind == log::EventKind::kConsolidation ||
                      kind == log::EventKind::kRetract;
    auto matching = CheckEnvelopeIdempotency(
        *verified->envelope, connection_id,
        {.client_seq = request.client_seq(),
         .event_index = static_cast<std::uint32_t>(index),
         .event_count = event_count});
    if (!matching) {
      return std::unexpected(matching.error());
    }
    log::ConversationId conversation{};
    std::transform(event->conversation()->begin(), event->conversation()->end(),
                   conversation.bytes.begin(), [](std::uint8_t byte) {
                     return static_cast<std::byte>(byte);
                   });
    apply_requests.push_back(core::ApplyRequest{
        .kind = kind,
        .conversation = conversation,
        .plaintext_payload = payload,
        .boundary = events::Boundary::kSocket});
  }
  if (needs_admission) {
    auto admitted =
        proj::BeliefProjection::AdmitBatch(*projection_store_, admission);
    if (!admitted) {
      return std::unexpected(admitted.error());
    }
  }
  auto frames = CommitApplyRequests(apply_requests);
  if (!frames) {
    return std::unexpected(frames.error());
  }
  const DedupState state{.client_seq = request.client_seq(),
                         .first_lsn = frames->front().header.lsn,
                         .last_lsn = frames->back().header.lsn,
                         .digest = *digest};
  dedup_[*connection] = state;
  const auto& status = storage_->mmr_store().verification_status();
  return AppendResult{.client_seq = state.client_seq,
                      .first_lsn = state.first_lsn,
                      .last_lsn = state.last_lsn,
                      .duplicate = false,
                      .leaf_count = status.leaf_count,
                      .last_leaf_hash = mmr::HashFramePlaintext(
                          frames->back().header, frames->back().sealed_payload),
                      .root = status.root,
                      .frames = std::move(*frames)};
}

std::expected<std::uint64_t, Error> ActorEngine::NextClientSequence(
    std::span<const std::byte> connection_id) {
  auto ready = EnsureReady();
  if (!ready) {
    return std::unexpected(ready.error());
  }
  auto connection = ReadConnectionId(connection_id);
  if (!connection) {
    return std::unexpected(connection.error());
  }
  const auto prior = dedup_.find(*connection);
  if (prior == dedup_.end() &&
      dedup_.size() >= kMaximumLogicalConnections) {
    return std::unexpected(Error{ErrorCode::kCapacityExceeded, 0});
  }
  return prior == dedup_.end() ? 1U : prior->second.client_seq + 1U;
}

std::expected<FrameScan, Error> ActorEngine::FramesSince(
    std::uint64_t since_lsn,
    const std::optional<log::ConversationId>& conversation,
    FrameScanLimits limits) {
  if (since_lsn == std::numeric_limits<std::uint64_t>::max() ||
      limits.maximum_frames == 0 || limits.maximum_scanned_frames == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  FrameScan scan{.frames = {}, .exhausted = false};
  scan.frames.reserve(
      std::min<std::size_t>(limits.maximum_frames, 1024U));
  std::uint64_t next_lsn = since_lsn + 1U;
  std::size_t scanned = 0;
  std::size_t payload_bytes = 0;
  constexpr std::size_t kReadBatch = 1024;
  while (scan.frames.size() < limits.maximum_frames) {
    const auto read_limit = std::min<std::size_t>(
        kReadBatch, limits.maximum_scanned_frames - scanned);
    if (read_limit == 0) {
      return scan;
    }
    auto frames = storage_->ReadFrom(next_lsn, read_limit);
    if (!frames) {
      return std::unexpected(frames.error());
    }
    if (frames->empty()) {
      scan.exhausted = true;
      return scan;
    }
    for (auto& frame : *frames) {
      if (frame.header.lsn == std::numeric_limits<std::uint64_t>::max()) {
        scan.exhausted = true;
        return scan;
      }
      next_lsn = frame.header.lsn + 1U;
      ++scanned;
      if (!conversation || frame.header.conversation == *conversation) {
        const auto frame_bytes = frame.sealed_payload.size();
        if (!scan.frames.empty() &&
            payload_bytes + frame_bytes > limits.maximum_payload_bytes) {
          return scan;
        }
        payload_bytes += frame_bytes;
        scan.frames.push_back(std::move(frame));
        if (scan.frames.size() == limits.maximum_frames) {
          return scan;
        }
      }
    }
    if (frames->size() < read_limit) {
      scan.exhausted = true;
      return scan;
    }
  }
  return scan;
}

std::expected<std::vector<std::byte>, Error> ActorEngine::Activate(
    const protocol::Activate& request) {
  if (!ready_) {
    return std::unexpected(Error{ErrorCode::kOperationUnavailable, 0});
  }
  const auto* weights = request.token_weights();
  if (weights == nullptr || weights->size() != 256U) {
    return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
  }
  WireTokenModel model{.weights = weights,
                       .overhead = request.token_item_overhead()};
  auto snapshot = projection_store_->BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  log::ConversationId conversation{};
  std::transform(request.conversation()->begin(), request.conversation()->end(),
                 conversation.bytes.begin(), [](std::uint8_t byte) {
                   return static_cast<std::byte>(byte);
                 });
  const compose::ActivationRequest activation{
      .conversation = conversation,
      .query = std::string_view(
          reinterpret_cast<const char*>(request.query()->data()),
          request.query()->size()),
      .turn_text =
          request.turn_text() == nullptr
              ? std::string_view{}
              : std::string_view(
                    reinterpret_cast<const char*>(request.turn_text()->data()),
                    request.turn_text()->size()),
      .budget_tokens = static_cast<std::size_t>(request.budget_tokens()),
      .token_model = {.context = &model, .count = CountWireTokens},
      .query_embedding =
          request.query_embedding() == nullptr
              ? std::span<const std::int8_t>{}
              : std::span<const std::int8_t>(request.query_embedding()->data(),
                                             request.query_embedding()->size()),
      .query_binary_prefilter =
          request.query_binary_prefilter() == nullptr
              ? std::span<const std::byte>{}
              : std::as_bytes(
                    std::span(request.query_binary_prefilter()->data(),
                              request.query_binary_prefilter()->size())),
      .temporal_from_ns = request.temporal_from_ns(),
      .temporal_to_ns = request.temporal_to_ns(),
  };
  auto bundle =
      compose::Composer::Activate(*snapshot, activation, vector_lane_.get());
  if (!bundle) {
    return std::unexpected(bundle.error());
  }
  return compose::Composer::CanonicalBytes(*bundle);
}

std::expected<RecallOutput, Error> ActorEngine::Recall(
    const protocol::Recall& request) {
  if (!ready_) {
    return std::unexpected(Error{ErrorCode::kOperationUnavailable, 0});
  }
  if (request.level() >
      static_cast<std::uint8_t>(proj::TemporalLevel::kWeek)) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  const auto level = static_cast<proj::TemporalLevel>(request.level());
  auto snapshot = projection_store_->BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  RecallOutput output;
  if (request.mode() == protocol::RecallMode::list_windows) {
    auto windows = proj::TemporalLadder::ListWindows(
        *snapshot, level, request.start_ns(), request.end_ns(),
        request.limit());
    if (!windows) {
      return std::unexpected(windows.error());
    }
    output.windows = std::move(*windows);
    return output;
  }
  const proj::TemporalWindow window{.level = level,
                                    .start_ns = request.start_ns(),
                                    .end_ns = request.end_ns(),
                                    .member_count = 0};
  if (request.mode() == protocol::RecallMode::open_window) {
    auto windows =
        proj::TemporalLadder::OpenWindow(*snapshot, window, request.limit());
    if (!windows) {
      return std::unexpected(windows.error());
    }
    output.windows = std::move(*windows);
    return output;
  }
  if (request.mode() != protocol::RecallMode::resolve_members) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto members = proj::TemporalLadder::ResolveMembers(
      *snapshot, window, static_cast<std::size_t>(request.limit()));
  if (!members) {
    return std::unexpected(members.error());
  }
  output.members = std::move(*members);
  return output;
}

std::expected<std::optional<proj::BeliefRecord>, Error> ActorEngine::AsOf(
    const protocol::AsOf& request) {
  if (!ready_) {
    return std::unexpected(Error{ErrorCode::kOperationUnavailable, 0});
  }
  auto transaction_lsn = request.transaction_lsn();
  if (transaction_lsn == 0) {
    transaction_lsn = apply_loop_->state().last_lsn();
  }
  if (transaction_lsn == 0) {
    return std::optional<proj::BeliefRecord>{};
  }
  auto snapshot = projection_store_->BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  return proj::BeliefProjection::ReadAsOf(
      *snapshot, static_cast<schema::BeliefType>(request.belief_type()),
      request.canonical_identity()->string_view(),
      proj::BeliefAsOf{.valid_time_ns = request.valid_time_ns(),
                       .transaction_lsn = transaction_lsn});
}

std::expected<CheckpointWrite, Error> ActorEngine::WriteCheckpoint(
    std::span<const std::byte> connection_id, std::uint64_t client_seq,
    std::span<const std::byte> turn_id, std::span<const std::byte> blob) {
  auto ready = EnsureReady();
  if (!ready) {
    return std::unexpected(ready.error());
  }
  if (turn_id.empty() || turn_id.size() > kMaximumTurnBytes || blob.empty()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto connection = ReadConnectionId(connection_id);
  if (!connection) {
    return std::unexpected(connection.error());
  }
  const auto cursor = EncodeCheckpointCursor(turn_id, blob);
  flatbuffers::FlatBufferBuilder builder(cursor.size() + 256U);
  const auto cursor_offset = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(cursor.data()), cursor.size());
  const auto payload = schema::CreateCheckpoint(builder, cursor_offset);
  const auto connection_offset = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(connection_id.data()),
      connection_id.size());
  const auto envelope = schema::CreateEventEnvelope(
      builder, events::kCurrentSchemaVersion,
      schema::EventPayload::Checkpoint, payload.Union(), connection_offset,
      client_seq, 0, 1);
  schema::FinishEventEnvelopeBuffer(builder, envelope);
  const auto encoded = std::as_bytes(
      std::span(builder.GetBufferPointer(), builder.GetSize()));
  const core::ApplyRequest apply{.kind = log::EventKind::kCheckpoint,
                                 .conversation = TurnConversation(turn_id),
                                 .plaintext_payload = encoded,
                                 .boundary = events::Boundary::kSocket};
  auto digest = ApplyDigest(std::span(&apply, 1));
  if (!digest) {
    return std::unexpected(digest.error());
  }
  const auto prior = dedup_.find(*connection);
  if (prior == dedup_.end() &&
      dedup_.size() >= kMaximumLogicalConnections) {
    return std::unexpected(Error{ErrorCode::kCapacityExceeded, 0});
  }
  if (prior != dedup_.end() && client_seq == prior->second.client_seq) {
    if (*digest != prior->second.digest) {
      return std::unexpected(Error{ErrorCode::kIdempotencyConflict, 0});
    }
    return CheckpointWrite{.lsn = prior->second.last_lsn, .frames = {}};
  }
  const auto expected_seq =
      prior == dedup_.end() ? 1U : prior->second.client_seq + 1U;
  if (client_seq != expected_seq) {
    return std::unexpected(Error{ErrorCode::kSequenceViolation, 0});
  }
  auto frames = CommitApplyRequests(std::span(&apply, 1));
  if (!frames) {
    return std::unexpected(frames.error());
  }
  const DedupState state{.client_seq = client_seq,
                         .first_lsn = frames->front().header.lsn,
                         .last_lsn = frames->back().header.lsn,
                         .digest = *digest};
  dedup_[*connection] = state;
  return CheckpointWrite{.lsn = state.first_lsn,
                         .frames = std::move(*frames)};
}

std::expected<std::optional<CheckpointRead>, Error>
ActorEngine::LatestCheckpoint(std::span<const std::byte> turn_id) {
  if (!ready_) {
    return std::unexpected(Error{ErrorCode::kOperationUnavailable, 0});
  }
  if (turn_id.empty() || turn_id.size() > kMaximumTurnBytes) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto snapshot = projection_store_->BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  const auto conversation = TurnConversation(turn_id);
  std::uint64_t before_lsn = std::numeric_limits<std::uint64_t>::max();
  std::size_t remaining = kLatestCheckpointScan;
  while (remaining > 0) {
    auto scan = proj::ScanLatestConversationRecordOfKind(
        *snapshot, conversation, log::EventKind::kCheckpoint, before_lsn,
        remaining);
    if (!scan) {
      return std::unexpected(scan.error());
    }
    remaining -= std::min(scan->scanned, remaining);
    if (scan->record.has_value()) {
      const auto record = std::move(scan->record).value_or(
          proj::ConversationRecord{.lsn = 0,
                                   .kind = log::EventKind::kCheckpoint,
                                   .wall_timestamp_ns = 0,
                                   .payload = {}});
      auto verified = events::VerifyEvent(record.payload, record.kind,
                                          events::Boundary::kDisk);
      if (!verified) {
        return std::unexpected(verified.error());
      }
      const auto* cursor =
          verified->envelope->payload_as_Checkpoint()->cursor();
      auto blob = DecodeCheckpointCursor(
          std::as_bytes(std::span(cursor->data(), cursor->size())), turn_id);
      if (blob) {
        return std::optional<CheckpointRead>(CheckpointRead{
            .lsn = record.lsn,
            .blob = std::vector<std::byte>(blob->begin(), blob->end())});
      }
      before_lsn = record.lsn;
      continue;
    }
    if (scan->next_before_lsn == 0) {
      return std::optional<CheckpointRead>{};
    }
    before_lsn = scan->next_before_lsn;
  }
  return std::unexpected(Error{ErrorCode::kCapacityExceeded, 0});
}

std::expected<AttestWrite, Error> ActorEngine::Attest(
    std::span<const std::byte> connection_id, std::uint64_t client_seq,
    std::span<const std::uint64_t> used,
    std::span<const std::uint64_t> ignored) {
  auto ready = EnsureReady();
  if (!ready) {
    return std::unexpected(ready.error());
  }
  const auto total = used.size() + ignored.size();
  if (total == 0 || total > kMaximumBatchEvents) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto connection = ReadConnectionId(connection_id);
  if (!connection) {
    return std::unexpected(connection.error());
  }
  const auto tail = apply_loop_->state().last_lsn();
  std::vector<flatbuffers::FlatBufferBuilder> builders;
  std::vector<core::ApplyRequest> apply_requests;
  builders.reserve(total);
  apply_requests.reserve(total);
  const auto build = [&](std::span<const std::uint64_t> targets,
                         schema::AttestationDisposition disposition)
      -> std::expected<void, Error> {
    for (const auto target : targets) {
      if (target == 0 || target > tail) {
        return std::unexpected(Error{ErrorCode::kInvalidArgument, 0, target});
      }
      auto& builder = builders.emplace_back(256);
      const auto payload =
          schema::CreateAttestation(builder, target, disposition);
      const auto connection_offset = builder.CreateVector(
          reinterpret_cast<const std::uint8_t*>(connection_id.data()),
          connection_id.size());
      const auto envelope = schema::CreateEventEnvelope(
          builder, events::kCurrentSchemaVersion,
          schema::EventPayload::Attestation, payload.Union(),
          connection_offset, client_seq,
          static_cast<std::uint32_t>(apply_requests.size()),
          static_cast<std::uint32_t>(total));
      schema::FinishEventEnvelopeBuffer(builder, envelope);
      apply_requests.push_back(core::ApplyRequest{
          .kind = log::EventKind::kAttestation,
          .conversation = log::ConversationId{},
          .plaintext_payload = std::as_bytes(
              std::span(builder.GetBufferPointer(), builder.GetSize())),
          .boundary = events::Boundary::kSocket});
    }
    return {};
  };
  auto used_built = build(used, schema::AttestationDisposition::used);
  if (!used_built) {
    return std::unexpected(used_built.error());
  }
  auto ignored_built = build(ignored, schema::AttestationDisposition::ignored);
  if (!ignored_built) {
    return std::unexpected(ignored_built.error());
  }
  auto digest = ApplyDigest(apply_requests);
  if (!digest) {
    return std::unexpected(digest.error());
  }
  const auto prior = dedup_.find(*connection);
  if (prior == dedup_.end() &&
      dedup_.size() >= kMaximumLogicalConnections) {
    return std::unexpected(Error{ErrorCode::kCapacityExceeded, 0});
  }
  if (prior != dedup_.end() && client_seq == prior->second.client_seq) {
    if (*digest != prior->second.digest) {
      return std::unexpected(Error{ErrorCode::kIdempotencyConflict, 0});
    }
    return AttestWrite{
        .first_lsn = prior->second.first_lsn,
        .last_lsn = prior->second.last_lsn,
        .count = static_cast<std::uint32_t>(prior->second.last_lsn -
                                            prior->second.first_lsn + 1U),
        .frames = {}};
  }
  const auto expected_seq =
      prior == dedup_.end() ? 1U : prior->second.client_seq + 1U;
  if (client_seq != expected_seq) {
    return std::unexpected(Error{ErrorCode::kSequenceViolation, 0});
  }
  auto frames = CommitApplyRequests(apply_requests);
  if (!frames) {
    return std::unexpected(frames.error());
  }
  const DedupState state{.client_seq = client_seq,
                         .first_lsn = frames->front().header.lsn,
                         .last_lsn = frames->back().header.lsn,
                         .digest = *digest};
  dedup_[*connection] = state;
  return AttestWrite{.first_lsn = state.first_lsn,
                     .last_lsn = state.last_lsn,
                     .count = static_cast<std::uint32_t>(frames->size()),
                     .frames = std::move(*frames)};
}

std::expected<ActorStats, Error> ActorEngine::Stats() {
  auto snapshot = projection_store_->BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  ActorStats stats{.actor = config_.actor,
                   .log_events = apply_loop_->state().event_count(),
                   .log_bytes = 0,
                   .projections = {}};
  stats.projections.reserve(proj::kProjectionCount);
  for (std::size_t index = 0; index < proj::kProjectionCount; ++index) {
    const auto id = static_cast<proj::ProjectionId>(index);
    auto checkpoint = snapshot->Checkpoint(id);
    if (!checkpoint) {
      return std::unexpected(checkpoint.error());
    }
    stats.projections.push_back(
        ProjectionStat{.name = std::string(proj::ProjectionName(id)),
                       .applied_lsn = *checkpoint});
  }
  std::error_code error;
  const auto log_directory = config_.actor_directory / "log";
  if (std::filesystem::exists(log_directory, error)) {
    for (std::filesystem::directory_iterator iterator(log_directory, error), end;
         !error && iterator != end; iterator.increment(error)) {
      if (iterator->is_regular_file(error)) {
        stats.log_bytes += iterator->file_size(error);
      }
    }
  }
  if (error) {
    return std::unexpected(Error{ErrorCode::kReadFailed, error.value()});
  }
  return stats;
}

mmr::VerificationStatus ActorEngine::VerificationStatus() const {
  return storage_->mmr_store().verification_status();
}

std::expected<std::uint64_t, Error> ActorEngine::RebuildProjection(
    std::string_view name) {
  std::optional<proj::ProjectionId> projection;
  if (name == "all") {
    projection = std::nullopt;
  } else if (name == "belief_store") {
    projection = proj::ProjectionId::kBeliefStore;
  } else if (name == "entity_index") {
    projection = proj::ProjectionId::kEntityIndex;
  } else if (name == "vector_lane") {
    projection = proj::ProjectionId::kVectorLane;
  } else if (name == "bm25") {
    projection = proj::ProjectionId::kBm25;
  } else if (name == "temporal_ladder") {
    projection = proj::ProjectionId::kTemporalLadder;
  } else if (name == "intent_frame") {
    projection = proj::ProjectionId::kIntentFrame;
  } else if (name == "work_ledger") {
    projection = proj::ProjectionId::kWorkLedger;
  } else if (name == "conversation_heads") {
    projection = proj::ProjectionId::kConversationHeads;
  } else {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto rebuilt = RebuildProjectionStream(projection, true);
  if (!rebuilt) {
    return std::unexpected(rebuilt.error());
  }
  ready_ = true;
  return apply_loop_->state().last_lsn();
}

std::expected<std::vector<std::byte>, Error> ActorEngine::CryptoDelete() {
  auto receipt = storage_->DestroyKey(config_.signing_key);
  if (!receipt) {
    return std::unexpected(receipt.error());
  }
  std::vector<std::byte> encoded;
  encoded.reserve(2U + 16U + 8U + 32U + 32U + 32U + 64U);
  encoded.push_back(static_cast<std::byte>(receipt->actor & 0xffU));
  encoded.push_back(static_cast<std::byte>((receipt->actor >> 8U) & 0xffU));
  encoded.insert(encoded.end(), receipt->user.begin(), receipt->user.end());
  AppendU64(encoded, receipt->deleted_at_lsn);
  encoded.insert(encoded.end(), receipt->key_fingerprint.begin(),
                 receipt->key_fingerprint.end());
  encoded.insert(encoded.end(), receipt->checkpoint_root.begin(),
                 receipt->checkpoint_root.end());
  encoded.insert(encoded.end(), receipt->public_key.begin(),
                 receipt->public_key.end());
  encoded.insert(encoded.end(), receipt->signature.begin(),
                 receipt->signature.end());
  ready_ = false;
  return encoded;
}

}  // namespace neocortex::serve
