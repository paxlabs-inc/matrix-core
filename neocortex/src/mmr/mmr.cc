#include "mmr/mmr.h"

#include <algorithm>
#include <cstring>
#include <limits>

#include <blake3.h>

namespace neocortex::mmr {
namespace {

constexpr std::byte kNodeDomain{0x01};
constexpr std::byte kRootDomain{0x02};
constexpr std::byte kFrameDomain{0x03};

void PutU16(std::span<std::byte> output, std::size_t offset, std::uint16_t value) {
  output[offset] = static_cast<std::byte>(value & 0xffU);
  output[offset + 1] = static_cast<std::byte>((value >> 8U) & 0xffU);
}

void PutU64(std::span<std::byte> output, std::size_t offset, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    output[offset + index] = static_cast<std::byte>((value >> (index * 8U)) & 0xffU);
  }
}

Hash Blake3(std::span<const std::byte> first, std::span<const std::byte> second = {},
            std::span<const std::byte> third = {}) {
  blake3_hasher hasher{};
  blake3_hasher_init(&hasher);
  blake3_hasher_update(&hasher, first.data(), first.size());
  blake3_hasher_update(&hasher, second.data(), second.size());
  blake3_hasher_update(&hasher, third.data(), third.size());
  Hash output{};
  blake3_hasher_finalize(&hasher, reinterpret_cast<std::uint8_t*>(output.data()), output.size());
  return output;
}

Hash ParentHash(const Hash& left, const Hash& right) {
  return Blake3(std::span(&kNodeDomain, 1), left, right);
}

Hash BagPeaks(std::uint64_t leaf_count, std::span<const Node> peaks) {
  blake3_hasher hasher{};
  blake3_hasher_init(&hasher);
  blake3_hasher_update(&hasher, &kRootDomain, 1);
  std::array<std::byte, 8> encoded_count{};
  PutU64(encoded_count, 0, leaf_count);
  blake3_hasher_update(&hasher, encoded_count.data(), encoded_count.size());
  for (const Node& peak : peaks) {
    blake3_hasher_update(&hasher, peak.hash.data(), peak.hash.size());
  }
  Hash output{};
  blake3_hasher_finalize(&hasher, reinterpret_cast<std::uint8_t*>(output.data()), output.size());
  return output;
}

std::uint64_t HighestPowerOfTwo(std::uint64_t value) {
  std::uint64_t power = 1;
  while (power <= value / 2U) {
    power *= 2U;
  }
  return power;
}

std::uint8_t HeightFor(std::uint64_t leaf_count) {
  std::uint8_t height = 0;
  while (leaf_count > 1) {
    leaf_count /= 2U;
    ++height;
  }
  return height;
}

bool IsPowerOfTwo(std::uint64_t value) {
  return value != 0 && (value & (value - 1U)) == 0;
}

bool Disjoint(std::uint64_t first_start, std::uint64_t first_count,
              std::uint64_t second_start, std::uint64_t second_count) {
  return first_start + first_count <= second_start ||
         second_start + second_count <= first_start;
}

bool Inside(std::uint64_t node_start, std::uint64_t node_count,
            std::uint64_t range_start, std::uint64_t range_count) {
  return node_start >= range_start &&
         node_start + node_count <= range_start + range_count;
}

}  // namespace

Hash HashBytes(std::span<const std::byte> bytes) {
  return Blake3(bytes);
}

Hash HashFramePlaintext(const log::FrameHeader& header,
                        std::span<const std::byte> plaintext_payload) {
  std::array<std::byte, 44> committed_header{};
  committed_header[0] = kFrameDomain;
  PutU64(committed_header, 1, header.lsn);
  committed_header[9] = static_cast<std::byte>(header.kind);
  PutU64(committed_header, 10, static_cast<std::uint64_t>(header.wall_timestamp_ns));
  PutU16(committed_header, 18, header.actor);
  std::memcpy(committed_header.data() + 20, header.conversation.bytes.data(),
              header.conversation.bytes.size());
  PutU64(committed_header, 36, plaintext_payload.size());
  return Blake3(committed_header, plaintext_payload);
}

Hash RootFromPeaks(std::uint64_t leaf_count, std::span<const Node> peaks) {
  return BagPeaks(leaf_count, peaks);
}

AppendResult Mmr::Append(const Hash& leaf_hash) {
  Node node{.height = 0,
            .start = leaf_count_,
            .leaf_count = 1,
            .hash = leaf_hash};
  if (retain_history_) {
    leaves_.push_back(leaf_hash);
  }
  ++leaf_count_;
  AppendResult result{.root = {}, .created_nodes = {node}};
  if (retain_history_) {
    nodes_.insert_or_assign(NodeKey{node.start, node.leaf_count}, node);
  }
  while (!peaks_.empty() && peaks_.back().height == node.height) {
    const Node left = peaks_.back();
    peaks_.pop_back();
    node = Node{.height = static_cast<std::uint8_t>(node.height + 1U),
                .start = left.start,
                .leaf_count = left.leaf_count + node.leaf_count,
                .hash = ParentHash(left.hash, node.hash)};
    if (retain_history_) {
      nodes_.insert_or_assign(NodeKey{node.start, node.leaf_count}, node);
    }
    result.created_nodes.push_back(node);
  }
  peaks_.push_back(node);
  result.root = Root();
  return result;
}

Hash Mmr::Root() const {
  return BagPeaks(leaf_count(), peaks_);
}

std::expected<Node, Error> Mmr::Lookup(std::uint64_t start,
                                      std::uint64_t leaf_count_value) const {
  const auto iterator = nodes_.find(NodeKey{start, leaf_count_value});
  if (iterator == nodes_.end()) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0, start});
  }
  return iterator->second;
}

std::expected<Hash, Error> Mmr::RootAt(std::uint64_t count) const {
  if (count > leaf_count()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0, count});
  }
  std::vector<Node> peaks;
  std::uint64_t start = 0;
  std::uint64_t remaining = count;
  while (remaining > 0) {
    const std::uint64_t size = HighestPowerOfTwo(remaining);
    auto node = Lookup(start, size);
    if (!node) {
      return std::unexpected(node.error());
    }
    peaks.push_back(*node);
    start += size;
    remaining -= size;
  }
  return BagPeaks(count, peaks);
}

std::expected<RangeProof, Error> Mmr::ProveRange(std::uint64_t range_start,
                                                 std::uint64_t range_leaf_count) const {
  if (range_leaf_count == 0 || range_start >= leaf_count() ||
      range_leaf_count > leaf_count() - range_start) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0, range_start});
  }
  RangeProof proof{.total_leaf_count = leaf_count(),
                   .range_start = range_start,
                   .range_leaf_count = range_leaf_count,
                   .expected_root = Root(),
                   .boundary_nodes = {}};
  const auto collect = [&](const auto& self, const Node& node) -> std::expected<void, Error> {
    if (Disjoint(node.start, node.leaf_count, range_start, range_leaf_count)) {
      proof.boundary_nodes.push_back(node);
      return {};
    }
    if (Inside(node.start, node.leaf_count, range_start, range_leaf_count)) {
      return {};
    }
    if (node.leaf_count == 1) {
      return std::unexpected(Error{ErrorCode::kInvariantViolation, 0, node.start});
    }
    const std::uint64_t child_count = node.leaf_count / 2U;
    auto left = Lookup(node.start, child_count);
    auto right = Lookup(node.start + child_count, child_count);
    if (!left) {
      return std::unexpected(left.error());
    }
    if (!right) {
      return std::unexpected(right.error());
    }
    auto left_result = self(self, *left);
    if (!left_result) {
      return left_result;
    }
    return self(self, *right);
  };
  for (const Node& peak : peaks_) {
    auto result = collect(collect, peak);
    if (!result) {
      return std::unexpected(result.error());
    }
  }
  return proof;
}

std::expected<Hash, Error> Mmr::VerifyRange(std::span<const Hash> range_leaf_hashes,
                                            const RangeProof& proof) {
  if (proof.range_leaf_count == 0 || range_leaf_hashes.size() != proof.range_leaf_count ||
      proof.range_start >= proof.total_leaf_count ||
      proof.range_leaf_count > proof.total_leaf_count - proof.range_start) {
    return std::unexpected(Error{ErrorCode::kProofInvalid, 0, proof.range_start});
  }
  std::map<NodeKey, Node> boundary;
  for (const Node& node : proof.boundary_nodes) {
    if (!IsPowerOfTwo(node.leaf_count) || node.height != HeightFor(node.leaf_count) ||
        node.start % node.leaf_count != 0 ||
        !Disjoint(node.start, node.leaf_count, proof.range_start,
                  proof.range_leaf_count) ||
        node.start > proof.total_leaf_count ||
        node.leaf_count > proof.total_leaf_count - node.start ||
        !boundary.emplace(NodeKey{node.start, node.leaf_count}, node).second) {
      return std::unexpected(Error{ErrorCode::kProofInvalid, 0, node.start});
    }
  }
  std::map<NodeKey, bool> used;
  const auto rebuild = [&](const auto& self, std::uint64_t start,
                           std::uint64_t count) -> std::expected<Node, Error> {
    if (Disjoint(start, count, proof.range_start, proof.range_leaf_count)) {
      const auto iterator = boundary.find(NodeKey{start, count});
      if (iterator == boundary.end()) {
        return std::unexpected(Error{ErrorCode::kProofInvalid, 0, start});
      }
      used.insert_or_assign(iterator->first, true);
      return iterator->second;
    }
    if (count == 1) {
      if (start < proof.range_start ||
          start - proof.range_start >= range_leaf_hashes.size()) {
        return std::unexpected(Error{ErrorCode::kProofInvalid, 0, start});
      }
      return Node{.height = 0,
                  .start = start,
                  .leaf_count = 1,
                  .hash = range_leaf_hashes[start - proof.range_start]};
    }
    const std::uint64_t child_count = count / 2U;
    auto left = self(self, start, child_count);
    auto right = self(self, start + child_count, child_count);
    if (!left) {
      return std::unexpected(left.error());
    }
    if (!right) {
      return std::unexpected(right.error());
    }
    return Node{.height = static_cast<std::uint8_t>(left->height + 1U),
                .start = start,
                .leaf_count = count,
                .hash = ParentHash(left->hash, right->hash)};
  };

  std::vector<Node> peaks;
  std::uint64_t start = 0;
  std::uint64_t remaining = proof.total_leaf_count;
  while (remaining > 0) {
    const std::uint64_t count = HighestPowerOfTwo(remaining);
    auto peak = rebuild(rebuild, start, count);
    if (!peak) {
      return std::unexpected(peak.error());
    }
    peaks.push_back(*peak);
    start += count;
    remaining -= count;
  }
  if (used.size() != boundary.size()) {
    return std::unexpected(Error{ErrorCode::kProofInvalid, 0});
  }
  const Hash actual = BagPeaks(proof.total_leaf_count, peaks);
  if (actual != proof.expected_root) {
    return std::unexpected(Error{ErrorCode::kProofInvalid, 0});
  }
  return actual;
}

}  // namespace neocortex::mmr
