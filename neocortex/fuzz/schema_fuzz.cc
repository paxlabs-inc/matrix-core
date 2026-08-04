#include <cstddef>
#include <cstdint>
#include <span>

#include "schema/events.h"

extern "C" int LLVMFuzzerTestOneInput(const std::uint8_t* data, std::size_t size) {
  const auto encoded = std::as_bytes(std::span(data, size));
  for (std::uint8_t raw_kind = 1; raw_kind <= 21; ++raw_kind) {
    const auto kind = static_cast<neocortex::log::EventKind>(raw_kind);
    for (const auto boundary : {neocortex::events::Boundary::kDisk,
                                neocortex::events::Boundary::kSocket,
                                neocortex::events::Boundary::kImport}) {
      auto verified = neocortex::events::VerifyEvent(encoded, kind, boundary);
      if (verified) {
        volatile const auto accepted = verified->kind;
        static_cast<void>(accepted);
      }
    }
  }
  return 0;
}
