#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <limits>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include "core/error.h"
#include "log/frame.h"
#include "proj/store.h"

namespace neocortex::proj {

enum class EntityKind : std::uint8_t {
  kDomain = 1,
  kUrl = 2,
  kPath = 3,
  kHexId = 4,
  kStructuredId = 5,
  kProperName = 6,
};

struct ExtractedEntity final {
  EntityKind kind;
  std::string canonical;

  auto operator<=>(const ExtractedEntity&) const = default;
};

struct EntityHit final {
  std::uint64_t lsn;
  EntityKind kind;
  std::string canonical;

  auto operator<=>(const EntityHit&) const = default;
};

struct EntityRebuildProgress final {
  std::uint64_t applied_lsn;
  std::size_t applied_frames;
  bool complete;
};

[[nodiscard]] std::vector<ExtractedEntity> ExtractEntities(
    std::span<const std::byte> text);
[[nodiscard]] std::vector<ExtractedEntity> ExtractEntities(
    std::string_view text);

class EntityProjection final {
 public:
  [[nodiscard]] static std::expected<void, Error> ApplyEvent(
      ProjectionStore& store, const log::Frame& frame);
  [[nodiscard]] static std::expected<EntityRebuildProgress, Error> Rebuild(
      ProjectionStore& store, std::span<const log::Frame> frames,
      bool reset = false,
      std::size_t maximum_frames = std::numeric_limits<std::size_t>::max());
  [[nodiscard]] static std::expected<std::vector<EntityHit>, Error> Query(
      const ReadSnapshot& snapshot, std::string_view query,
      std::string_view turn_text = {});
};

}  // namespace neocortex::proj
