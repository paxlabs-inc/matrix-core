#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <expected>
#include <map>
#include <span>
#include <utility>
#include <vector>

#include "core/error.h"
#include "log/frame.h"

namespace neocortex::mmr {

using Hash = std::array<std::byte, 32>;

struct Node final {
  std::uint8_t height;
  std::uint64_t start;
  std::uint64_t leaf_count;
  Hash hash;

  bool operator==(const Node&) const = default;
};

struct AppendResult final {
  Hash root;
  std::vector<Node> created_nodes;
};

struct RangeProof final {
  std::uint64_t total_leaf_count;
  std::uint64_t range_start;
  std::uint64_t range_leaf_count;
  Hash expected_root;
  std::vector<Node> boundary_nodes;
};

[[nodiscard]] Hash HashBytes(std::span<const std::byte> bytes);
[[nodiscard]] Hash HashFramePlaintext(const log::FrameHeader& header,
                                      std::span<const std::byte> plaintext_payload);
[[nodiscard]] Hash RootFromPeaks(std::uint64_t leaf_count,
                                 std::span<const Node> peaks);

class Mmr final {
 public:
  explicit Mmr(bool retain_history = true) : retain_history_(retain_history) {}

  [[nodiscard]] AppendResult Append(const Hash& leaf_hash);
  [[nodiscard]] Hash Root() const;
  [[nodiscard]] std::expected<Hash, Error> RootAt(std::uint64_t leaf_count) const;
  [[nodiscard]] std::expected<RangeProof, Error> ProveRange(
      std::uint64_t range_start, std::uint64_t range_leaf_count) const;

  [[nodiscard]] static std::expected<Hash, Error> VerifyRange(
      std::span<const Hash> range_leaf_hashes, const RangeProof& proof);

  [[nodiscard]] std::uint64_t leaf_count() const {
    return leaf_count_;
  }
  [[nodiscard]] const std::vector<Hash>& leaves() const { return leaves_; }

 private:
  using NodeKey = std::pair<std::uint64_t, std::uint64_t>;

  [[nodiscard]] std::expected<Node, Error> Lookup(std::uint64_t start,
                                                  std::uint64_t leaf_count) const;

  std::vector<Hash> leaves_;
  std::vector<Node> peaks_;
  std::map<NodeKey, Node> nodes_;
  std::uint64_t leaf_count_ = 0;
  bool retain_history_;
};

}  // namespace neocortex::mmr
