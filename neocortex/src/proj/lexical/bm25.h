#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <limits>
#include <span>
#include <string_view>
#include <vector>

#include "core/error.h"
#include "log/frame.h"
#include "proj/store.h"
#include "proj/vectors/vector_lane.h"

namespace neocortex::proj {

struct LexicalHit final {
  std::uint64_t lsn;
  std::uint64_t score_q32;
};

struct FusedHit final {
  std::uint64_t lsn;
  std::uint64_t rrf_score;
  std::uint32_t vector_rank;
  std::uint32_t lexical_rank;
};

struct LexicalRebuildProgress final {
  std::uint64_t applied_lsn;
  std::size_t applied_frames;
  bool complete;
};

struct FusionOptions final {
  std::size_t limit;
  std::uint32_t rank_constant = 60;
};

class LexicalProjection final {
 public:
  [[nodiscard]] static std::expected<void, Error> ApplyEvent(
      ProjectionStore& store, const log::Frame& frame);
  [[nodiscard]] static std::expected<LexicalRebuildProgress, Error> Rebuild(
      ProjectionStore& store, std::span<const log::Frame> frames,
      bool reset = false,
      std::size_t maximum_frames = std::numeric_limits<std::size_t>::max());
  [[nodiscard]] static std::expected<std::vector<LexicalHit>, Error> Query(
      const ReadSnapshot& snapshot, std::string_view query,
      std::size_t limit);
  [[nodiscard]] static std::vector<FusedHit> Fuse(
      std::span<const VectorHit> vector_hits,
      std::span<const LexicalHit> lexical_hits, FusionOptions options);
};

}  // namespace neocortex::proj
