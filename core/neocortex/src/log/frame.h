#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <expected>
#include <span>
#include <vector>

#include "core/error.h"

namespace neocortex::log {

inline constexpr std::size_t kFrameHeaderSize = 43;
inline constexpr std::uint32_t kDefaultMaximumFrameBytes = 16U * 1024U * 1024U;

enum class EventKind : std::uint8_t {
  kUserMsg = 1,
  kDeliveredMsg = 2,
  kToolCall = 3,
  kToolResult = 4,
  kReasoning = 5,
  kProviderFrame = 6,
  kMediaRef = 7,
  kEffect = 8,
  kApproval = 9,
  kOutcome = 10,
  kCheckpoint = 11,
  kSupervisor = 12,
  kRecovery = 13,
  kIntentSet = 14,
  kLoopOpened = 15,
  kLoopClosed = 16,
  kAssertion = 17,
  kConsolidation = 18,
  kEmbedding = 19,
  kRetract = 20,
  kAttestation = 21,
};

struct ConversationId final {
  std::array<std::byte, 16> bytes{};

  bool operator==(const ConversationId&) const = default;
};

struct FrameHeader final {
  std::uint64_t lsn;
  EventKind kind;
  std::int64_t wall_timestamp_ns;
  std::uint16_t actor;
  ConversationId conversation;

  bool operator==(const FrameHeader&) const = default;
};

struct Frame final {
  FrameHeader header;
  std::vector<std::byte> sealed_payload;

  bool operator==(const Frame&) const = default;
};

[[nodiscard]] bool IsEventKind(std::uint8_t value);

[[nodiscard]] std::expected<std::vector<std::byte>, Error> EncodeFrame(
    const Frame& frame,
    std::uint32_t maximum_frame_bytes = kDefaultMaximumFrameBytes);

[[nodiscard]] std::expected<Frame, Error> DecodeFrame(
    std::span<const std::byte> encoded,
    std::uint32_t maximum_frame_bytes = kDefaultMaximumFrameBytes);

[[nodiscard]] std::expected<std::uint32_t, Error> ReadEncodedFrameLength(
    std::span<const std::byte> prefix,
    std::uint32_t maximum_frame_bytes = kDefaultMaximumFrameBytes);

}  // namespace neocortex::log
