#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <expected>
#include <filesystem>
#include <memory>
#include <optional>
#include <span>
#include <string_view>
#include <thread>
#include <vector>

#include <lmdb.h>

#include "core/error.h"

namespace neocortex::proj {

class BeliefProjection;

enum class ProjectionId : std::uint8_t {
  kBeliefStore,
  kEntityIndex,
  kVectorLane,
  kBm25,
  kTemporalLadder,
  kIntentFrame,
  kWorkLedger,
  kConversationHeads,
  kCount,
};

inline constexpr std::size_t kProjectionCount =
    static_cast<std::size_t>(ProjectionId::kCount);

[[nodiscard]] std::string_view ProjectionName(ProjectionId id);

enum class MutationKind : std::uint8_t {
  kPut,
  kDelete,
};

struct Mutation final {
  MutationKind kind;
  std::span<const std::byte> key;
  std::span<const std::byte> value;
};

struct KeyValue final {
  std::vector<std::byte> key;
  std::vector<std::byte> value;
};

struct EnvironmentDeleter final {
  void operator()(MDB_env* environment) const;
};

struct TransactionDeleter final {
  void operator()(MDB_txn* transaction) const;
};

using EnvironmentHandle = std::unique_ptr<MDB_env, EnvironmentDeleter>;
using TransactionHandle = std::unique_ptr<MDB_txn, TransactionDeleter>;

class ReadSnapshot final {
 public:
  ReadSnapshot(ReadSnapshot&&) noexcept = default;
  ReadSnapshot& operator=(ReadSnapshot&&) noexcept = default;
  ReadSnapshot(const ReadSnapshot&) = delete;
  ReadSnapshot& operator=(const ReadSnapshot&) = delete;

  [[nodiscard]] std::expected<std::optional<std::vector<std::byte>>, Error> Get(
      ProjectionId projection, std::span<const std::byte> key) const;
  [[nodiscard]] std::expected<std::optional<KeyValue>, Error> FirstAtOrAfter(
      ProjectionId projection, std::span<const std::byte> key) const;
  [[nodiscard]] std::expected<std::vector<KeyValue>, Error> ScanPrefix(
      ProjectionId projection, std::span<const std::byte> prefix,
      std::size_t limit) const;
  [[nodiscard]] std::expected<std::vector<KeyValue>, Error> ScanPrefixFrom(
      ProjectionId projection, std::span<const std::byte> prefix,
      std::span<const std::byte> first_key, std::size_t limit) const;
  [[nodiscard]] std::expected<std::vector<KeyValue>, Error> ScanPrefixReverse(
      ProjectionId projection, std::span<const std::byte> prefix,
      std::size_t limit) const;
  [[nodiscard]] std::expected<std::vector<KeyValue>, Error>
  ScanPrefixReverseBefore(ProjectionId projection,
                          std::span<const std::byte> prefix,
                          std::span<const std::byte> before_key,
                          std::size_t limit) const;
  [[nodiscard]] std::expected<std::uint64_t, Error> Checkpoint(
      ProjectionId projection) const;
  [[nodiscard]] std::expected<std::vector<std::byte>, Error> CanonicalDump(
      ProjectionId projection) const;
  [[nodiscard]] std::uint64_t epoch() const { return epoch_; }

 private:
  friend class ProjectionStore;
  ReadSnapshot(std::shared_ptr<EnvironmentHandle> environment,
               TransactionHandle transaction, std::uint64_t epoch,
               std::array<MDB_dbi, kProjectionCount> databases,
               MDB_dbi metadata_database);

  std::shared_ptr<EnvironmentHandle> environment_;
  TransactionHandle transaction_;
  std::array<MDB_dbi, kProjectionCount> databases_;
  MDB_dbi metadata_database_;
  std::uint64_t epoch_;
};

class ProjectionStore final {
 public:
  static std::expected<ProjectionStore, Error> Open(
      const std::filesystem::path& actor_directory,
      std::size_t map_bytes = static_cast<std::size_t>(256) * 1024U * 1024U);

  ProjectionStore(ProjectionStore&&) noexcept = default;
  ProjectionStore& operator=(ProjectionStore&&) noexcept = default;
  ProjectionStore(const ProjectionStore&) = delete;
  ProjectionStore& operator=(const ProjectionStore&) = delete;

  [[nodiscard]] std::expected<void, Error> Apply(
      ProjectionId projection, std::uint64_t applied_lsn,
      std::span<const Mutation> mutations);
  [[nodiscard]] std::expected<void, Error> Reset(ProjectionId projection);
 [[nodiscard]] std::expected<ReadSnapshot, Error> BeginSnapshot() const;

 private:
  friend class BeliefProjection;
  ProjectionStore(std::shared_ptr<EnvironmentHandle> environment,
                  std::array<MDB_dbi, kProjectionCount> databases,
                  MDB_dbi metadata_database, std::thread::id writer_thread);
  [[nodiscard]] std::expected<void, Error> ApplyInternal(
      ProjectionId projection, std::uint64_t applied_lsn,
      std::span<const Mutation> mutations);

  std::shared_ptr<EnvironmentHandle> environment_;
  std::array<MDB_dbi, kProjectionCount> databases_;
  MDB_dbi metadata_database_;
  std::thread::id writer_thread_;
};

}  // namespace neocortex::proj
