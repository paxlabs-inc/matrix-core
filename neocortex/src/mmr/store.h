#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <expected>
#include <filesystem>
#include <span>

#include "core/error.h"
#include "log/frame.h"
#include "mmr/mmr.h"

namespace neocortex::mmr {

using SigningSeed = std::array<std::byte, 32>;
using PublicKey = std::array<std::byte, 32>;
using SecretKey = std::array<std::byte, 64>;
using Signature = std::array<std::byte, 64>;

struct SigningKeyPair final {
  PublicKey public_key;
  SecretKey secret_key;
};

struct Checkpoint final {
  std::uint16_t actor;
  std::uint64_t lsn;
  std::uint64_t leaf_count;
  Hash root;
  PublicKey public_key;
  Signature signature;

  bool operator==(const Checkpoint&) const = default;
};

struct VerificationStatus final {
  Hash root{};
  std::uint64_t leaf_count = 0;
  std::uint64_t last_checkpoint_lsn = 0;
  bool verified = false;
};

[[nodiscard]] std::expected<SigningKeyPair, Error> SigningKeyPairFromSeed(
    const SigningSeed& seed);

[[nodiscard]] std::expected<Checkpoint, Error> LoadCheckpoint(
    const std::filesystem::path& path);

[[nodiscard]] std::expected<Checkpoint, Error> DecodeCheckpoint(
    std::span<const std::byte> encoded);

[[nodiscard]] std::expected<Mmr, Error> RestorePeakRecords(
    std::span<const std::byte> encoded);

[[nodiscard]] std::expected<void, Error> VerifyCheckpointSignature(
    const Checkpoint& checkpoint, const PublicKey& expected_public_key);

class MmrStore final {
 public:
  static std::expected<MmrStore, Error> Open(const std::filesystem::path& actor_directory,
                                             std::uint16_t actor,
                                             const PublicKey& expected_public_key);

  MmrStore(MmrStore&&) noexcept = default;
  MmrStore& operator=(MmrStore&&) noexcept = default;
  MmrStore(const MmrStore&) = delete;
  MmrStore& operator=(const MmrStore&) = delete;

  [[nodiscard]] std::expected<Hash, Error> AppendFrame(
      const log::FrameHeader& header, std::span<const std::byte> plaintext_payload);

  [[nodiscard]] std::expected<Checkpoint, Error> CreateCheckpoint(
      const SigningKeyPair& key_pair);
  [[nodiscard]] std::expected<Hash, Error> LeafHash(std::uint64_t index) const;
  [[nodiscard]] std::expected<void, Error> VerifyLeafHashes(
      std::uint64_t first_index, std::span<const Hash> hashes) const;
  [[nodiscard]] std::expected<Hash, Error> RootAt(std::uint64_t leaf_count) const;
  [[nodiscard]] std::expected<void, Error> RewindTo(std::uint64_t leaf_count);

  [[nodiscard]] std::expected<RangeProof, Error> ProveRange(
      std::uint64_t range_start, std::uint64_t range_leaf_count) const;

  [[nodiscard]] const Mmr& mmr() const { return mmr_; }
  [[nodiscard]] const VerificationStatus& verification_status() const { return status_; }

 private:
  MmrStore(std::filesystem::path actor_directory, std::uint16_t actor,
           PublicKey expected_public_key, Mmr mmr, VerificationStatus status);

  std::filesystem::path actor_directory_;
  std::filesystem::path mmr_directory_;
  std::filesystem::path checkpoint_directory_;
  std::uint16_t actor_;
  PublicKey expected_public_key_;
  Mmr mmr_;
  VerificationStatus status_;
};

}  // namespace neocortex::mmr
