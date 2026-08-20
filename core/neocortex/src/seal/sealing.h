#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <expected>
#include <filesystem>
#include <span>
#include <vector>

#include "core/env.h"
#include "log/frame.h"
#include "mmr/store.h"

namespace neocortex::seal {

using KeyEncryptionKey = std::array<std::byte, 32>;
using UserId = std::array<std::byte, 16>;

struct DeletionReceipt final {
  std::uint16_t actor;
  UserId user;
  std::uint64_t deleted_at_lsn;
  mmr::Hash key_fingerprint;
  mmr::Hash checkpoint_root;
  mmr::PublicKey public_key;
  mmr::Signature signature;

  bool operator==(const DeletionReceipt&) const = default;
};

[[nodiscard]] std::expected<DeletionReceipt, Error> LoadDeletionReceipt(
    const std::filesystem::path& actor_directory,
    const mmr::PublicKey& trusted_public_key);

[[nodiscard]] std::expected<void, Error> VerifyDeletionReceipt(
    const DeletionReceipt& receipt,
    const mmr::PublicKey& trusted_public_key);

class KeyHierarchy final {
 public:
  static std::expected<KeyHierarchy, Error> OpenOrCreate(
      const std::filesystem::path& actor_directory, std::uint16_t actor,
      const UserId& user, const KeyEncryptionKey& kek, core::Entropy& entropy,
      const mmr::PublicKey& trusted_deletion_public_key, bool allow_create);

  ~KeyHierarchy();
  KeyHierarchy(KeyHierarchy&& other) noexcept;
  KeyHierarchy& operator=(KeyHierarchy&&) noexcept = delete;
  KeyHierarchy(const KeyHierarchy&) = delete;
  KeyHierarchy& operator=(const KeyHierarchy&) = delete;

  [[nodiscard]] std::expected<std::vector<std::byte>, Error> Seal(
      const log::FrameHeader& header, std::span<const std::byte> plaintext,
      core::Entropy& entropy) const;
  [[nodiscard]] std::expected<std::vector<std::byte>, Error> Unseal(
      const log::FrameHeader& header,
      std::span<const std::byte> sealed_payload) const;
  [[nodiscard]] std::expected<DeletionReceipt, Error> Destroy(
      std::uint64_t deleted_at_lsn, const mmr::Hash& checkpoint_root,
      const mmr::SigningKeyPair& signing_key);

  [[nodiscard]] bool destroyed() const { return destroyed_; }

 private:
  struct KeyMaterial final {
    std::array<std::byte, 32> user_key;
    std::array<std::byte, 32> data_key;
  };

  KeyHierarchy(std::filesystem::path actor_directory,
               std::filesystem::path key_directory, std::uint16_t actor,
               UserId user, KeyMaterial key_material,
               mmr::PublicKey trusted_deletion_public_key);

  std::filesystem::path actor_directory_;
  std::filesystem::path key_directory_;
  std::uint16_t actor_;
  UserId user_;
  std::array<std::byte, 32> user_key_;
  std::array<std::byte, 32> data_key_;
  mmr::PublicKey trusted_deletion_public_key_;
  bool destroyed_ = false;
};

}  // namespace neocortex::seal
