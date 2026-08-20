#include <cstddef>
#include <cstdint>
#include <span>

#include "log/frame.h"

extern "C" int LLVMFuzzerTestOneInput(const std::uint8_t* data, std::size_t size) {
  const auto bytes = std::as_bytes(std::span(data, size));
  static_cast<void>(neocortex::log::ReadEncodedFrameLength(bytes));
  static_cast<void>(neocortex::log::DecodeFrame(bytes));
  return 0;
}
