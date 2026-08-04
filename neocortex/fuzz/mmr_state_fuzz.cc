#include <cstddef>
#include <cstdint>
#include <span>

#include "mmr/store.h"

extern "C" int LLVMFuzzerTestOneInput(const std::uint8_t* data, std::size_t size) {
  const auto bytes = std::as_bytes(std::span(data, size));
  static_cast<void>(neocortex::mmr::DecodeCheckpoint(bytes));
  static_cast<void>(neocortex::mmr::RestorePeakRecords(bytes));
  return 0;
}
