#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <span>
#include <string_view>
#include <vector>

#include "core/error.h"
#include "log/frame.h"
#include "schema/events_generated.h"

namespace neocortex::events {

inline constexpr std::uint16_t kCurrentSchemaVersion = 1;
inline constexpr std::uint32_t kMaximumEmbeddingDimension = 16U * 1024U;
inline constexpr std::uint32_t kMaximumIdentifierBytes = 256;

enum class Boundary : std::uint8_t {
  kDisk,
  kSocket,
  kImport,
};

struct VerifiedEvent final {
  const schema::EventEnvelope* envelope;
  log::EventKind kind;
  Boundary boundary;
};

[[nodiscard]] std::expected<VerifiedEvent, Error> VerifyEvent(
    std::span<const std::byte> encoded, log::EventKind expected_kind,
    Boundary boundary);

[[nodiscard]] std::expected<log::EventKind, Error> ParseEventKindName(
    std::string_view name);

[[nodiscard]] std::string_view EventKindName(log::EventKind kind);

class OrderingState final {
 public:
  struct KindLookup final {
    void* context;
    std::expected<log::EventKind, Error> (*read)(void*, std::uint64_t);
  };

  [[nodiscard]] std::expected<void, Error> ValidateBatch(
      std::span<const log::Frame> frames, Boundary boundary,
      KindLookup lookup) const;
  [[nodiscard]] std::expected<void, Error> ValidateBatch(
      std::span<const log::Frame> frames, Boundary boundary) const {
    return ValidateBatch(frames, boundary, KindLookup{nullptr, nullptr});
  }
  [[nodiscard]] std::expected<void, Error> RecordBatch(
      std::span<const log::Frame> frames);

  [[nodiscard]] std::uint64_t applied_lsn() const {
    return applied_lsn_;
  }

 private:
  [[nodiscard]] std::expected<log::EventKind, Error> KindAt(
      std::uint64_t lsn, std::span<const log::Frame> batch,
      std::size_t current_index, KindLookup lookup) const;

  std::uint64_t applied_lsn_ = 0;
};

}  // namespace neocortex::events
