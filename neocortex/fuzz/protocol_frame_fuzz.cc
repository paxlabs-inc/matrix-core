#include <cstddef>
#include <cstdint>
#include <span>

#include "serve/protocol.h"

extern "C" int LLVMFuzzerTestOneInput(const std::uint8_t* data,
                                      std::size_t size) {
  const auto encoded = std::as_bytes(std::span(data, size));
  neocortex::serve::ProtocolFrameParser parser;
  const auto midpoint = size / 2U;
  auto first = parser.Push(encoded.first(midpoint));
  if (first) {
    auto second = parser.Push(encoded.subspan(midpoint));
    if (second) {
      for (const auto& payload : *second) {
        static_cast<void>(neocortex::serve::VerifyProtocolPayload(payload));
      }
    }
  }
  static_cast<void>(neocortex::serve::VerifyProtocolPayload(encoded));
  auto framed = neocortex::serve::EncodeProtocolFrame(encoded);
  if (framed) {
    neocortex::serve::ProtocolFrameParser valid_parser;
    static_cast<void>(valid_parser.Push(*framed));
  }
  return 0;
}
