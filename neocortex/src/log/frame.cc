#include "log/frame.h"

#include <cstring>
#include <limits>

#include <crc32c/crc32c.h>

namespace neocortex::log {
namespace {

constexpr std::size_t kLengthOffset = 0;
constexpr std::size_t kChecksumOffset = 4;
constexpr std::size_t kLsnOffset = 8;
constexpr std::size_t kKindOffset = 16;
constexpr std::size_t kTimestampOffset = 17;
constexpr std::size_t kActorOffset = 25;
constexpr std::size_t kConversationOffset = 27;

void PutU16(std::span<std::byte> out, std::size_t offset, std::uint16_t value) {
  out[offset] = static_cast<std::byte>(value & 0xffU);
  out[offset + 1] = static_cast<std::byte>((value >> 8U) & 0xffU);
}

void PutU32(std::span<std::byte> out, std::size_t offset, std::uint32_t value) {
  for (std::size_t index = 0; index < 4; ++index) {
    out[offset + index] = static_cast<std::byte>((value >> (index * 8U)) & 0xffU);
  }
}

void PutU64(std::span<std::byte> out, std::size_t offset, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    out[offset + index] = static_cast<std::byte>((value >> (index * 8U)) & 0xffU);
  }
}

std::uint16_t GetU16(std::span<const std::byte> in, std::size_t offset) {
  return static_cast<std::uint16_t>(std::to_integer<std::uint16_t>(in[offset]) |
                                    (std::to_integer<std::uint16_t>(in[offset + 1]) << 8U));
}

std::uint32_t GetU32(std::span<const std::byte> in, std::size_t offset) {
  std::uint32_t value = 0;
  for (std::size_t index = 0; index < 4; ++index) {
    value |= std::to_integer<std::uint32_t>(in[offset + index]) << (index * 8U);
  }
  return value;
}

std::uint64_t GetU64(std::span<const std::byte> in, std::size_t offset) {
  std::uint64_t value = 0;
  for (std::size_t index = 0; index < 8; ++index) {
    value |= std::to_integer<std::uint64_t>(in[offset + index]) << (index * 8U);
  }
  return value;
}

std::uint32_t Checksum(std::span<const std::byte> encoded) {
  const auto committed = encoded.subspan(kLsnOffset);
  return crc32c::Crc32c(reinterpret_cast<const char*>(committed.data()), committed.size());
}

}  // namespace

bool IsEventKind(std::uint8_t value) {
  return value >= static_cast<std::uint8_t>(EventKind::kUserMsg) &&
         value <= static_cast<std::uint8_t>(EventKind::kAttestation);
}

std::expected<std::uint32_t, Error> ReadEncodedFrameLength(
    std::span<const std::byte> prefix, std::uint32_t maximum_frame_bytes) {
  if (prefix.size() < sizeof(std::uint32_t)) {
    return std::unexpected(Error{ErrorCode::kTruncated, 0});
  }
  const std::uint32_t length = GetU32(prefix, kLengthOffset);
  if (length < kFrameHeaderSize || length > maximum_frame_bytes) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  return length;
}

std::expected<std::vector<std::byte>, Error> EncodeFrame(
    const Frame& frame, std::uint32_t maximum_frame_bytes) {
  if (!IsEventKind(static_cast<std::uint8_t>(frame.header.kind)) || frame.header.lsn == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidKind, 0, frame.header.lsn});
  }
  if (frame.sealed_payload.size() >
      static_cast<std::size_t>(std::numeric_limits<std::uint32_t>::max()) - kFrameHeaderSize) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0, frame.header.lsn});
  }
  const auto length = static_cast<std::uint32_t>(kFrameHeaderSize + frame.sealed_payload.size());
  if (length > maximum_frame_bytes) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0, frame.header.lsn});
  }

  std::vector<std::byte> encoded(length);
  PutU32(encoded, kLengthOffset, length);
  PutU32(encoded, kChecksumOffset, 0);
  PutU64(encoded, kLsnOffset, frame.header.lsn);
  encoded[kKindOffset] = static_cast<std::byte>(frame.header.kind);
  PutU64(encoded, kTimestampOffset,
         static_cast<std::uint64_t>(frame.header.wall_timestamp_ns));
  PutU16(encoded, kActorOffset, frame.header.actor);
  std::memcpy(encoded.data() + kConversationOffset, frame.header.conversation.bytes.data(),
              frame.header.conversation.bytes.size());
  if (!frame.sealed_payload.empty()) {
    std::memcpy(encoded.data() + kFrameHeaderSize, frame.sealed_payload.data(),
                frame.sealed_payload.size());
  }
  PutU32(encoded, kChecksumOffset, Checksum(encoded));
  return encoded;
}

std::expected<Frame, Error> DecodeFrame(std::span<const std::byte> encoded,
                                        std::uint32_t maximum_frame_bytes) {
  auto length = ReadEncodedFrameLength(encoded, maximum_frame_bytes);
  if (!length) {
    return std::unexpected(length.error());
  }
  if (encoded.size() != *length) {
    return std::unexpected(Error{ErrorCode::kTruncated, 0});
  }
  const std::uint64_t lsn = GetU64(encoded, kLsnOffset);
  const std::uint8_t raw_kind = std::to_integer<std::uint8_t>(encoded[kKindOffset]);
  if (lsn == 0 || !IsEventKind(raw_kind)) {
    return std::unexpected(Error{ErrorCode::kInvalidKind, 0, lsn});
  }
  if (GetU32(encoded, kChecksumOffset) != Checksum(encoded)) {
    return std::unexpected(Error{ErrorCode::kChecksumMismatch, 0, lsn});
  }

  Frame frame{
      .header = FrameHeader{
          .lsn = lsn,
          .kind = static_cast<EventKind>(raw_kind),
          .wall_timestamp_ns = static_cast<std::int64_t>(GetU64(encoded, kTimestampOffset)),
          .actor = GetU16(encoded, kActorOffset),
          .conversation = {},
      },
      .sealed_payload = std::vector<std::byte>(encoded.begin() + kFrameHeaderSize,
                                               encoded.end()),
  };
  std::memcpy(frame.header.conversation.bytes.data(), encoded.data() + kConversationOffset,
              frame.header.conversation.bytes.size());
  return frame;
}

}  // namespace neocortex::log
