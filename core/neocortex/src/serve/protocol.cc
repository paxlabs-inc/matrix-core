#include "serve/protocol.h"

#include <algorithm>
#include <limits>

#include <crc32c/crc32c.h>
#include <flatbuffers/flatbuffers.h>

#include "log/frame.h"

namespace neocortex::serve {
namespace {

constexpr std::size_t kReadSlopBytes = static_cast<std::size_t>(64) * 1024U;
constexpr std::size_t kMaximumQueryBytes =
    static_cast<std::size_t>(1024) * 1024U;
constexpr std::size_t kMaximumTurnIdBytes = 4096;

void PutU32(std::span<std::byte> output, std::size_t offset,
            std::uint32_t value) {
  for (std::size_t index = 0; index < sizeof(value); ++index) {
    output[offset + index] =
        static_cast<std::byte>((value >> (index * 8U)) & 0xffU);
  }
}

std::uint32_t GetU32(std::span<const std::byte> input, std::size_t offset) {
  std::uint32_t value = 0;
  for (std::size_t index = 0; index < sizeof(value); ++index) {
    value |= std::to_integer<std::uint32_t>(input[offset + index])
             << (index * 8U);
  }
  return value;
}

std::uint32_t Checksum(std::span<const std::byte> payload) {
  return crc32c::Crc32c(reinterpret_cast<const char*>(payload.data()),
                        payload.size());
}

template <typename Vector>
bool SizeBetween(const Vector* value, std::size_t minimum,
                 std::size_t maximum) {
  return value != nullptr && value->size() >= minimum &&
         value->size() <= maximum;
}

bool ValidConversation(const flatbuffers::Vector<std::uint8_t>* value) {
  return SizeBetween(value, 16, 16);
}

std::expected<void, Error> ValidateAppend(const protocol::Append& append) {
  if (append.client_seq() == 0 ||
      !SizeBetween(append.events(), 1, kMaximumBatchEvents)) {
    return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
  }
  for (const auto* event : *append.events()) {
    if (event == nullptr || !log::IsEventKind(event->kind()) ||
        !ValidConversation(event->conversation()) ||
        !SizeBetween(event->payload(), 1, log::kDefaultMaximumFrameBytes)) {
      return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    }
  }
  return {};
}

std::expected<void, Error> ValidateActivate(const protocol::Activate& request) {
  if (!ValidConversation(request.conversation()) ||
      !SizeBetween(request.query(), 0, kMaximumQueryBytes) ||
      (request.turn_text() != nullptr &&
       request.turn_text()->size() > kMaximumQueryBytes) ||
      request.budget_tokens() == 0 ||
      request.budget_tokens() > std::numeric_limits<std::uint32_t>::max() ||
      request.temporal_to_ns() < request.temporal_from_ns() ||
      !SizeBetween(request.token_weights(), 256, 256)) {
    return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
  }
  const auto* embedding = request.query_embedding();
  const auto* prefilter = request.query_binary_prefilter();
  if ((embedding == nullptr) != (prefilter == nullptr) ||
      (embedding != nullptr &&
       (embedding->empty() || embedding->size() > 65536U ||
        prefilter->size() != (embedding->size() + 7U) / 8U))) {
    return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
  }
  return {};
}

}  // namespace

std::expected<std::vector<std::byte>, Error> EncodeProtocolFrame(
    std::span<const std::byte> payload, std::uint32_t maximum_payload_bytes) {
  if (payload.empty() || payload.size() > maximum_payload_bytes ||
      payload.size() > std::numeric_limits<std::uint32_t>::max()) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  std::vector<std::byte> encoded(kProtocolHeaderBytes + payload.size());
  PutU32(encoded, 0, static_cast<std::uint32_t>(payload.size()));
  PutU32(encoded, 4, Checksum(payload));
  std::copy(payload.begin(), payload.end(), encoded.begin() + kProtocolHeaderBytes);
  return encoded;
}

ProtocolFrameParser::ProtocolFrameParser(std::uint32_t maximum_payload_bytes)
    : maximum_payload_bytes_(maximum_payload_bytes) {
  pending_.reserve(std::min<std::size_t>(
      static_cast<std::size_t>(maximum_payload_bytes_) + kProtocolHeaderBytes,
      kReadSlopBytes));
}

std::expected<std::vector<std::vector<std::byte>>, Error>
ProtocolFrameParser::Push(std::span<const std::byte> bytes) {
  const auto maximum_pending = static_cast<std::size_t>(maximum_payload_bytes_) +
                               kProtocolHeaderBytes + kReadSlopBytes;
  if (bytes.size() > maximum_pending || pending_.size() > maximum_pending - bytes.size()) {
    return std::unexpected(Error{ErrorCode::kCapacityExceeded, 0});
  }
  pending_.insert(pending_.end(), bytes.begin(), bytes.end());
  std::vector<std::vector<std::byte>> frames;
  std::size_t consumed = 0;
  while (pending_.size() - consumed >= kProtocolHeaderBytes) {
    const auto view = std::span<const std::byte>(pending_).subspan(consumed);
    const auto length = GetU32(view, 0);
    if (length == 0 || length > maximum_payload_bytes_) {
      return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
    }
    const auto framed_length = kProtocolHeaderBytes + static_cast<std::size_t>(length);
    if (view.size() < framed_length) {
      break;
    }
    const auto payload = view.subspan(kProtocolHeaderBytes, length);
    if (GetU32(view, 4) != Checksum(payload)) {
      return std::unexpected(Error{ErrorCode::kChecksumMismatch, 0});
    }
    frames.emplace_back(payload.begin(), payload.end());
    consumed += framed_length;
  }
  if (consumed != 0) {
    pending_.erase(pending_.begin(), pending_.begin() +
                                         static_cast<std::ptrdiff_t>(consumed));
  }
  return frames;
}

std::expected<const protocol::WireEnvelope*, Error> VerifyProtocolPayload(
    std::span<const std::byte> payload) {
  if (payload.empty() || payload.size() > kMaximumProtocolPayloadBytes) {
    return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
  }
  flatbuffers::Verifier verifier(
      reinterpret_cast<const std::uint8_t*>(payload.data()), payload.size(), 64,
      8192);
  if (!protocol::VerifyWireEnvelopeBuffer(verifier)) {
    return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
  }
  const auto* envelope = protocol::GetWireEnvelope(payload.data());
  if (envelope->proto_version() != kProtocolVersion ||
      envelope->payload_type() == protocol::WirePayload::NONE ||
      envelope->payload() == nullptr) {
    return std::unexpected(Error{ErrorCode::kProtocolVersion, 0});
  }
  return envelope;
}

std::expected<void, Error> ValidateRequest(const protocol::Request& request) {
  if (request.request_id() == 0 || request.payload() == nullptr) {
    return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
  }
  switch (request.payload_type()) {
    case protocol::RequestPayload::Append:
      return ValidateAppend(*request.payload_as_Append());
    case protocol::RequestPayload::Activate:
      return ValidateActivate(*request.payload_as_Activate());
    case protocol::RequestPayload::Transcript: {
      const auto* value = request.payload_as_Transcript();
      return ValidConversation(value->conversation()) && value->limit() > 0 &&
                     value->limit() <= 16384U
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    }
    case protocol::RequestPayload::Recall: {
      const auto* value = request.payload_as_Recall();
      return SizeBetween(value->query(), 0, kMaximumQueryBytes) &&
                     value->limit() > 0 && value->limit() <= 4096U &&
                     value->mode() <= protocol::RecallMode::resolve_members &&
                     value->level() <= 3U &&
                     value->end_ns() >= value->start_ns()
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    }
    case protocol::RequestPayload::AsOf: {
      const auto* value = request.payload_as_AsOf();
      return value->belief_type() <= 4U &&
                     value->canonical_identity() != nullptr &&
                     !value->canonical_identity()->empty() &&
                     value->canonical_identity()->size() <= kMaximumQueryBytes
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    }
    case protocol::RequestPayload::Checkpoint: {
      const auto* value = request.payload_as_Checkpoint();
      return SizeBetween(value->turn_id(), 1, kMaximumTurnIdBytes) &&
                     SizeBetween(value->blob(), 1, log::kDefaultMaximumFrameBytes)
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    }
    case protocol::RequestPayload::LatestCheckpoint:
      return SizeBetween(request.payload_as_LatestCheckpoint()->turn_id(), 1,
                         kMaximumTurnIdBytes)
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    case protocol::RequestPayload::Attest: {
      const auto* value = request.payload_as_Attest();
      const auto used = value->used() == nullptr ? 0U : value->used()->size();
      const auto ignored =
          value->ignored() == nullptr ? 0U : value->ignored()->size();
      return used + ignored > 0 && used + ignored <= kMaximumBatchEvents
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    }
    case protocol::RequestPayload::Subscribe: {
      const auto* conversation = request.payload_as_Subscribe()->conversation();
      return (conversation == nullptr || conversation->empty() ||
              ValidConversation(conversation))
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    }
    case protocol::RequestPayload::RebuildProjection: {
      const auto* value = request.payload_as_RebuildProjection();
      const auto* name = value->name();
      return value->actor() != 0 && name != nullptr && !name->empty() &&
                     name->size() <= 64U
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    }
    case protocol::RequestPayload::CryptoDelete:
      return request.payload_as_CryptoDelete()->actor() != 0
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    case protocol::RequestPayload::Health:
    case protocol::RequestPayload::LatencyHistograms:
      return {};
    case protocol::RequestPayload::Stats:
      return request.payload_as_Stats()->actor() != 0
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    case protocol::RequestPayload::VerifyStatus:
      return request.payload_as_VerifyStatus()->actor() != 0
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
    case protocol::RequestPayload::NONE:
      break;
  }
  return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
}

}  // namespace neocortex::serve
