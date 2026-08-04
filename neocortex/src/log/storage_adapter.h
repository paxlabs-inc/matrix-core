#pragma once

#include <cstdint>
#include <expected>
#include <filesystem>
#include <span>
#include <vector>

#include "core/env.h"
#include "seal/sealing.h"
#include "log/segment_log.h"
#include "mmr/store.h"

namespace neocortex::log {

class SegmentStorage final : public core::Storage {
 public:
  static std::expected<SegmentStorage, Error> Open(
      const std::filesystem::path& actor_directory, std::uint16_t actor,
      const mmr::PublicKey& trusted_public_key,
      const seal::UserId& user, const seal::KeyEncryptionKey& kek,
      core::Entropy& entropy, SegmentLogOptions options = {});

  SegmentStorage(SegmentStorage&&) noexcept = default;
  SegmentStorage& operator=(SegmentStorage&&) noexcept = delete;
  SegmentStorage(const SegmentStorage&) = delete;
  SegmentStorage& operator=(const SegmentStorage&) = delete;

  [[nodiscard]] std::expected<core::StorageCommit, Error> Append(
      std::span<const Frame> frames) override;
  [[nodiscard]] std::expected<std::vector<Frame>, Error> Recover() override;
  [[nodiscard]] std::expected<std::vector<Frame>, Error> ReadFrom(
      std::uint64_t first_lsn, std::size_t maximum_frames) override;
  [[nodiscard]] std::expected<void, Error> TruncateTo(std::uint64_t last_lsn);
  [[nodiscard]] std::expected<seal::DeletionReceipt, Error> DestroyKey(
      const mmr::SigningKeyPair& signing_key);

  [[nodiscard]] const SegmentLog& log() const { return log_; }
  [[nodiscard]] const mmr::MmrStore& mmr_store() const { return mmr_store_; }
  [[nodiscard]] std::uint64_t next_lsn() const { return log_.next_lsn(); }

 private:
  SegmentStorage(SegmentLog log, mmr::MmrStore mmr_store,
                 seal::KeyHierarchy key_hierarchy, core::Entropy& entropy);
  [[nodiscard]] std::expected<std::vector<Frame>, Error> VerifyAndRepair();
  [[nodiscard]] std::expected<void, Error> VerifyAndRepairBounded();

  SegmentLog log_;
  mmr::MmrStore mmr_store_;
  seal::KeyHierarchy key_hierarchy_;
  core::Entropy& entropy_;
};

}  // namespace neocortex::log
