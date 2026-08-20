#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <span>
#include <vector>

#include "core/error.h"
#include "schema/protocol_generated.h"

namespace neocortex::serve {

inline constexpr std::uint16_t kProtocolVersion = 2;
inline constexpr std::uint32_t kMaximumProtocolPayloadBytes = 17U * 1024U * 1024U;
inline constexpr std::size_t kProtocolHeaderBytes = 8;
inline constexpr std::size_t kMaximumBatchEvents = 256;
inline constexpr std::size_t kMaximumSubscriptions = 64;

[[nodiscard]] std::expected<std::vector<std::byte>, Error> EncodeProtocolFrame(
    std::span<const std::byte> payload,
    std::uint32_t maximum_payload_bytes = kMaximumProtocolPayloadBytes);

class ProtocolFrameParser final {
 public:
  explicit ProtocolFrameParser(
      std::uint32_t maximum_payload_bytes = kMaximumProtocolPayloadBytes);

  [[nodiscard]] std::expected<std::vector<std::vector<std::byte>>, Error> Push(
      std::span<const std::byte> bytes);

 private:
  std::uint32_t maximum_payload_bytes_;
  std::vector<std::byte> pending_;
};

[[nodiscard]] std::expected<const protocol::WireEnvelope*, Error>
VerifyProtocolPayload(std::span<const std::byte> payload);

[[nodiscard]] std::expected<void, Error> ValidateRequest(
    const protocol::Request& request);

}  // namespace neocortex::serve
