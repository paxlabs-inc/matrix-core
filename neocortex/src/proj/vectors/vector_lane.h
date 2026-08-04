#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <filesystem>
#include <limits>
#include <span>
#include <vector>

#include "core/error.h"
#include "log/frame.h"
#include "proj/store.h"

namespace neocortex::proj {

struct VectorHit final {
  std::uint64_t target_lsn;
  std::uint64_t embedding_lsn;
  std::int64_t score;
  std::uint64_t hamming_distance;
};

struct VectorRebuildProgress final {
  std::uint64_t applied_lsn;
  std::size_t applied_frames;
  bool complete;
};

class VectorLane final {
 public:
  static std::expected<VectorLane, Error> Open(
      const std::filesystem::path& actor_directory);

  VectorLane(VectorLane&& other) noexcept;
  VectorLane& operator=(VectorLane&& other) noexcept;
  VectorLane(const VectorLane&) = delete;
  VectorLane& operator=(const VectorLane&) = delete;
  ~VectorLane();

  [[nodiscard]] std::expected<void, Error> ApplyEvent(
      ProjectionStore& store, const log::Frame& frame);
  [[nodiscard]] std::expected<VectorRebuildProgress, Error> Rebuild(
      ProjectionStore& store, std::span<const log::Frame> frames,
      bool reset = false,
      std::size_t maximum_frames = std::numeric_limits<std::size_t>::max());
  [[nodiscard]] std::expected<std::vector<VectorHit>, Error> Search(
      std::span<const std::int8_t> quantized,
      std::span<const std::byte> binary_prefilter,
      std::size_t limit) const;
  [[nodiscard]] std::expected<std::vector<std::byte>, Error> CanonicalBytes()
      const;

  [[nodiscard]] static std::int64_t ScalarDot(
      std::span<const std::int8_t> first,
      std::span<const std::int8_t> second);
  [[nodiscard]] static std::int64_t SimdDot(
      std::span<const std::int8_t> first,
      std::span<const std::int8_t> second);

 private:
  explicit VectorLane(int descriptor, std::filesystem::path path);
  [[nodiscard]] std::expected<void, Error> RefreshMapping();
  [[nodiscard]] std::expected<void, Error> ResetFile();
  void Close();

  int descriptor_ = -1;
  std::filesystem::path path_;
  std::span<const std::byte> mapping_;
};

}  // namespace neocortex::proj
