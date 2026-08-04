#include <cstddef>
#include <cstdint>
#include <span>

#include "serve/protocol.h"

extern "C" int LLVMFuzzerTestOneInput(const std::uint8_t* data,
                                      std::size_t size) {
  const auto encoded = std::as_bytes(std::span(data, size));
  auto envelope = neocortex::serve::VerifyProtocolPayload(encoded);
  if (envelope &&
      (*envelope)->payload_type() == neocortex::protocol::WirePayload::Request) {
    static_cast<void>(neocortex::serve::ValidateRequest(
        *(*envelope)->payload_as_Request()));
  }
  return 0;
}
