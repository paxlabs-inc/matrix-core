#include "proj/store.h"

#include <algorithm>
#include <array>
#include <cstring>
#include <limits>
#include <string>

namespace neocortex::proj {
namespace {

constexpr std::array<std::string_view, kProjectionCount> kProjectionNames = {
    "belief_store",     "entity_index", "vector_lane",      "bm25",
    "temporal_ladder",  "intent_frame", "work_ledger",      "conversation_heads",
};

Error DatabaseError(int status) {
  return Error{status == MDB_MAP_FULL ? ErrorCode::kMapFull
                                      : ErrorCode::kBackendUnavailable,
               status};
}

std::array<std::byte, 8> EncodeU64(std::uint64_t value) {
  std::array<std::byte, 8> encoded{};
  for (std::size_t index = 0; index < encoded.size(); ++index) {
    encoded[index] = static_cast<std::byte>((value >> (index * 8U)) & 0xffU);
  }
  return encoded;
}

std::expected<std::uint64_t, Error> DecodeU64(std::span<const std::byte> value) {
  if (value.size() != 8) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  std::uint64_t decoded = 0;
  for (std::size_t index = 0; index < value.size(); ++index) {
    decoded |= std::to_integer<std::uint64_t>(value[index]) << (index * 8U);
  }
  return decoded;
}

MDB_val Value(std::span<const std::byte> bytes) {
  return MDB_val{.mv_size = bytes.size(),
                 .mv_data = const_cast<std::byte*>(bytes.data())};
}

std::span<const std::byte> Bytes(const MDB_val& value) {
  return {static_cast<const std::byte*>(value.mv_data), value.mv_size};
}

std::expected<std::uint64_t, Error> ReadCheckpoint(MDB_txn* transaction,
                                                   MDB_dbi metadata_database,
                                                   ProjectionId projection) {
  const auto name = ProjectionName(projection);
  MDB_val key{.mv_size = name.size(), .mv_data = const_cast<char*>(name.data())};
  MDB_val value{};
  const int status = ::mdb_get(transaction, metadata_database, &key, &value);
  if (status == MDB_NOTFOUND) {
    return 0;
  }
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  return DecodeU64(Bytes(value));
}

std::expected<void, Error> WriteCheckpoint(MDB_txn* transaction,
                                           MDB_dbi metadata_database,
                                           ProjectionId projection,
                                           std::uint64_t checkpoint) {
  const auto name = ProjectionName(projection);
  auto encoded = EncodeU64(checkpoint);
  MDB_val key{.mv_size = name.size(), .mv_data = const_cast<char*>(name.data())};
  auto value = Value(encoded);
  const int status = ::mdb_put(transaction, metadata_database, &key, &value, 0);
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  return {};
}

void AppendU64(std::vector<std::byte>& output, std::uint64_t value) {
  const auto encoded = EncodeU64(value);
  output.insert(output.end(), encoded.begin(), encoded.end());
}

struct CursorDeleter final {
  void operator()(MDB_cursor* cursor) const {
    if (cursor != nullptr) {
      ::mdb_cursor_close(cursor);
    }
  }
};

}  // namespace

std::string_view ProjectionName(ProjectionId id) {
  const auto index = static_cast<std::size_t>(id);
  return index < kProjectionNames.size() ? kProjectionNames[index] : std::string_view{};
}

void EnvironmentDeleter::operator()(MDB_env* environment) const {
  if (environment != nullptr) {
    ::mdb_env_close(environment);
  }
}

void TransactionDeleter::operator()(MDB_txn* transaction) const {
  if (transaction != nullptr) {
    ::mdb_txn_abort(transaction);
  }
}

ReadSnapshot::ReadSnapshot(std::shared_ptr<EnvironmentHandle> environment,
                           TransactionHandle transaction, std::uint64_t epoch,
                           std::array<MDB_dbi, kProjectionCount> databases,
                           MDB_dbi metadata_database)
    : environment_(std::move(environment)),
      transaction_(std::move(transaction)),
      databases_(databases),
      metadata_database_(metadata_database),
      epoch_(epoch) {}

std::expected<std::optional<std::vector<std::byte>>, Error> ReadSnapshot::Get(
    ProjectionId projection, std::span<const std::byte> key_bytes) const {
  const auto index = static_cast<std::size_t>(projection);
  if (index >= databases_.size() || key_bytes.empty()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto key = Value(key_bytes);
  MDB_val value{};
  const int status = ::mdb_get(transaction_.get(), databases_[index], &key, &value);
  if (status == MDB_NOTFOUND) {
    return std::optional<std::vector<std::byte>>{};
  }
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  return std::optional<std::vector<std::byte>>(
      std::in_place, Bytes(value).begin(), Bytes(value).end());
}

std::expected<std::optional<KeyValue>, Error> ReadSnapshot::FirstAtOrAfter(
    ProjectionId projection, std::span<const std::byte> key_bytes) const {
  const auto index = static_cast<std::size_t>(projection);
  if (index >= databases_.size() || key_bytes.empty()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  MDB_cursor* raw_cursor = nullptr;
  const int open_status =
      ::mdb_cursor_open(transaction_.get(), databases_[index], &raw_cursor);
  if (open_status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(open_status));
  }
  const std::unique_ptr<MDB_cursor, CursorDeleter> cursor(raw_cursor);
  auto key = Value(key_bytes);
  MDB_val value{};
  const int status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_SET_RANGE);
  if (status == MDB_NOTFOUND) {
    return std::optional<KeyValue>{};
  }
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  return std::optional<KeyValue>(std::in_place,
                                 std::vector<std::byte>(Bytes(key).begin(),
                                                        Bytes(key).end()),
                                 std::vector<std::byte>(Bytes(value).begin(),
                                                        Bytes(value).end()));
}

std::expected<std::vector<KeyValue>, Error> ReadSnapshot::ScanPrefix(
    ProjectionId projection, std::span<const std::byte> prefix,
    std::size_t limit) const {
  return ScanPrefixFrom(projection, prefix, prefix, limit);
}

std::expected<std::vector<KeyValue>, Error> ReadSnapshot::ScanPrefixFrom(
    ProjectionId projection, std::span<const std::byte> prefix,
    std::span<const std::byte> first_key, std::size_t limit) const {
  const auto index = static_cast<std::size_t>(projection);
  if (index >= databases_.size() || prefix.empty() || first_key.empty() ||
      first_key.size() < prefix.size() ||
      !std::equal(prefix.begin(), prefix.end(), first_key.begin()) ||
      limit == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  MDB_cursor* raw_cursor = nullptr;
  const int open_status =
      ::mdb_cursor_open(transaction_.get(), databases_[index], &raw_cursor);
  if (open_status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(open_status));
  }
  const std::unique_ptr<MDB_cursor, CursorDeleter> cursor(raw_cursor);
  auto key = Value(first_key);
  MDB_val value{};
  int status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_SET_RANGE);
  std::vector<KeyValue> output;
  while (status == MDB_SUCCESS && output.size() < limit) {
    const auto key_bytes = Bytes(key);
    if (key_bytes.size() < prefix.size() ||
        !std::equal(prefix.begin(), prefix.end(), key_bytes.begin())) {
      break;
    }
    output.push_back(KeyValue{
        .key = std::vector<std::byte>(key_bytes.begin(), key_bytes.end()),
        .value = std::vector<std::byte>(Bytes(value).begin(), Bytes(value).end()),
    });
    status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_NEXT);
  }
  if (status != MDB_SUCCESS && status != MDB_NOTFOUND) {
    return std::unexpected(DatabaseError(status));
  }
  return output;
}

std::expected<std::vector<KeyValue>, Error> ReadSnapshot::ScanPrefixReverse(
    ProjectionId projection, std::span<const std::byte> prefix,
    std::size_t limit) const {
  const auto index = static_cast<std::size_t>(projection);
  if (index >= databases_.size() || prefix.empty() || limit == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  MDB_cursor* raw_cursor = nullptr;
  const int open_status =
      ::mdb_cursor_open(transaction_.get(), databases_[index], &raw_cursor);
  if (open_status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(open_status));
  }
  const std::unique_ptr<MDB_cursor, CursorDeleter> cursor(raw_cursor);
  std::vector<std::byte> upper(prefix.begin(), prefix.end());
  bool has_upper = false;
  for (std::size_t index_from_end = upper.size(); index_from_end > 0;
       --index_from_end) {
    const auto index_to_increment = index_from_end - 1U;
    const auto value = std::to_integer<std::uint8_t>(upper[index_to_increment]);
    if (value != std::numeric_limits<std::uint8_t>::max()) {
      upper[index_to_increment] = static_cast<std::byte>(value + 1U);
      upper.resize(index_to_increment + 1U);
      has_upper = true;
      break;
    }
  }
  MDB_val key{};
  MDB_val value{};
  int status = MDB_NOTFOUND;
  if (has_upper) {
    key = Value(upper);
    status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_SET_RANGE);
    if (status == MDB_SUCCESS) {
      status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_PREV);
    } else if (status == MDB_NOTFOUND) {
      status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_LAST);
    }
  } else {
    status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_LAST);
  }
  std::vector<KeyValue> output;
  while (status == MDB_SUCCESS && output.size() < limit) {
    const auto key_bytes = Bytes(key);
    if (key_bytes.size() < prefix.size() ||
        !std::equal(prefix.begin(), prefix.end(), key_bytes.begin())) {
      break;
    }
    output.push_back(KeyValue{
        .key = std::vector<std::byte>(key_bytes.begin(), key_bytes.end()),
        .value = std::vector<std::byte>(Bytes(value).begin(), Bytes(value).end()),
    });
    status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_PREV);
  }
  if (status != MDB_SUCCESS && status != MDB_NOTFOUND) {
    return std::unexpected(DatabaseError(status));
  }
  return output;
}

std::expected<std::vector<KeyValue>, Error>
ReadSnapshot::ScanPrefixReverseBefore(ProjectionId projection,
                                      std::span<const std::byte> prefix,
                                      std::span<const std::byte> before_key,
                                      std::size_t limit) const {
  const auto index = static_cast<std::size_t>(projection);
  if (index >= databases_.size() || prefix.empty() || before_key.empty() ||
      before_key.size() < prefix.size() ||
      !std::equal(prefix.begin(), prefix.end(), before_key.begin()) ||
      limit == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  MDB_cursor* raw_cursor = nullptr;
  const int open_status =
      ::mdb_cursor_open(transaction_.get(), databases_[index], &raw_cursor);
  if (open_status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(open_status));
  }
  const std::unique_ptr<MDB_cursor, CursorDeleter> cursor(raw_cursor);
  auto key = Value(before_key);
  MDB_val value{};
  int status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_SET_RANGE);
  if (status == MDB_SUCCESS) {
    status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_PREV);
  } else if (status == MDB_NOTFOUND) {
    status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_LAST);
  }
  std::vector<KeyValue> output;
  while (status == MDB_SUCCESS && output.size() < limit) {
    const auto key_bytes = Bytes(key);
    if (key_bytes.size() < prefix.size() ||
        !std::equal(prefix.begin(), prefix.end(), key_bytes.begin())) {
      break;
    }
    output.push_back(KeyValue{
        .key = std::vector<std::byte>(key_bytes.begin(), key_bytes.end()),
        .value = std::vector<std::byte>(Bytes(value).begin(), Bytes(value).end()),
    });
    status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_PREV);
  }
  if (status != MDB_SUCCESS && status != MDB_NOTFOUND) {
    return std::unexpected(DatabaseError(status));
  }
  return output;
}

std::expected<std::uint64_t, Error> ReadSnapshot::Checkpoint(
    ProjectionId projection) const {
  if (static_cast<std::size_t>(projection) >= databases_.size()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  return ReadCheckpoint(transaction_.get(), metadata_database_, projection);
}

std::expected<std::vector<std::byte>, Error> ReadSnapshot::CanonicalDump(
    ProjectionId projection) const {
  const auto index = static_cast<std::size_t>(projection);
  if (index >= databases_.size()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto checkpoint = Checkpoint(projection);
  if (!checkpoint) {
    return std::unexpected(checkpoint.error());
  }
  MDB_cursor* raw_cursor = nullptr;
  const int open_status =
      ::mdb_cursor_open(transaction_.get(), databases_[index], &raw_cursor);
  if (open_status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(open_status));
  }
  const std::unique_ptr<MDB_cursor, CursorDeleter> cursor(raw_cursor);
  std::vector<std::byte> output;
  AppendU64(output, *checkpoint);
  MDB_val key{};
  MDB_val value{};
  int status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_FIRST);
  while (status == MDB_SUCCESS) {
    AppendU64(output, key.mv_size);
    output.insert(output.end(), Bytes(key).begin(), Bytes(key).end());
    AppendU64(output, value.mv_size);
    output.insert(output.end(), Bytes(value).begin(), Bytes(value).end());
    status = ::mdb_cursor_get(cursor.get(), &key, &value, MDB_NEXT);
  }
  if (status != MDB_NOTFOUND) {
    return std::unexpected(DatabaseError(status));
  }
  return output;
}

ProjectionStore::ProjectionStore(
    std::shared_ptr<EnvironmentHandle> environment,
    std::array<MDB_dbi, kProjectionCount> databases, MDB_dbi metadata_database,
    std::thread::id writer_thread)
    : environment_(std::move(environment)),
      databases_(databases),
      metadata_database_(metadata_database),
      writer_thread_(writer_thread) {}

std::expected<ProjectionStore, Error> ProjectionStore::Open(
    const std::filesystem::path& actor_directory, std::size_t map_bytes) {
  if (map_bytes < static_cast<std::size_t>(1024) * 1024U) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  std::error_code directory_error;
  const auto projection_directory = actor_directory / "projections";
  std::filesystem::create_directories(projection_directory, directory_error);
  if (directory_error) {
    return std::unexpected(
        Error{ErrorCode::kOpenFailed, directory_error.value()});
  }

  MDB_env* raw_environment = nullptr;
  int status = ::mdb_env_create(&raw_environment);
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  EnvironmentHandle owned_environment(raw_environment);
  status = ::mdb_env_set_maxdbs(owned_environment.get(),
                                static_cast<MDB_dbi>(kProjectionCount + 1U));
  if (status == MDB_SUCCESS) {
    status = ::mdb_env_set_mapsize(owned_environment.get(), map_bytes);
  }
  if (status == MDB_SUCCESS) {
    status = ::mdb_env_open(owned_environment.get(),
                            projection_directory.c_str(), MDB_NORDAHEAD, 0600);
  }
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  auto environment =
      std::make_shared<EnvironmentHandle>(std::move(owned_environment));

  MDB_txn* raw_transaction = nullptr;
  status = ::mdb_txn_begin(environment->get(), nullptr, 0, &raw_transaction);
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  TransactionHandle transaction(raw_transaction);
  MDB_dbi metadata_database = 0;
  status = ::mdb_dbi_open(transaction.get(), "projection_checkpoints", MDB_CREATE,
                          &metadata_database);
  std::array<MDB_dbi, kProjectionCount> databases{};
  for (std::size_t index = 0; status == MDB_SUCCESS && index < databases.size();
       ++index) {
    const std::string name(kProjectionNames[index]);
    status = ::mdb_dbi_open(transaction.get(), name.c_str(), MDB_CREATE,
                            &databases[index]);
  }
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  raw_transaction = transaction.release();
  status = ::mdb_txn_commit(raw_transaction);
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  return ProjectionStore(std::move(environment), databases, metadata_database,
                         std::this_thread::get_id());
}

std::expected<void, Error> ProjectionStore::Apply(
    ProjectionId projection, std::uint64_t applied_lsn,
    std::span<const Mutation> mutations) {
  if (projection == ProjectionId::kBeliefStore) {
    return std::unexpected(Error{ErrorCode::kBeliefWriteGate, 0, applied_lsn});
  }
  return ApplyInternal(projection, applied_lsn, mutations);
}

std::expected<void, Error> ProjectionStore::ApplyInternal(
    ProjectionId projection, std::uint64_t applied_lsn,
    std::span<const Mutation> mutations) {
  const auto index = static_cast<std::size_t>(projection);
  if (std::this_thread::get_id() != writer_thread_) {
    return std::unexpected(Error{ErrorCode::kWriterViolation, 0});
  }
  if (index >= databases_.size() || applied_lsn == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  MDB_txn* raw_transaction = nullptr;
  int status = ::mdb_txn_begin(environment_->get(), nullptr, 0, &raw_transaction);
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  TransactionHandle transaction(raw_transaction);
  auto checkpoint = ReadCheckpoint(transaction.get(), metadata_database_, projection);
  if (!checkpoint) {
    return std::unexpected(checkpoint.error());
  }
  if (*checkpoint == std::numeric_limits<std::uint64_t>::max() ||
      applied_lsn != *checkpoint + 1U) {
    return std::unexpected(
        Error{ErrorCode::kProjectionCheckpoint, 0, applied_lsn, *checkpoint});
  }
  for (const auto& mutation : mutations) {
    if (mutation.key.empty()) {
      return std::unexpected(Error{ErrorCode::kInvalidArgument, 0, applied_lsn});
    }
    auto key = Value(mutation.key);
    if (mutation.kind == MutationKind::kPut) {
      auto value = Value(mutation.value);
      status = ::mdb_put(transaction.get(), databases_[index], &key, &value, 0);
    } else {
      status = ::mdb_del(transaction.get(), databases_[index], &key, nullptr);
      if (status == MDB_NOTFOUND) {
        status = MDB_SUCCESS;
      }
    }
    if (status != MDB_SUCCESS) {
      return std::unexpected(DatabaseError(status));
    }
  }
  auto written =
      WriteCheckpoint(transaction.get(), metadata_database_, projection, applied_lsn);
  if (!written) {
    return std::unexpected(written.error());
  }
  raw_transaction = transaction.release();
  status = ::mdb_txn_commit(raw_transaction);
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  return {};
}

std::expected<void, Error> ProjectionStore::Reset(ProjectionId projection) {
  const auto index = static_cast<std::size_t>(projection);
  if (std::this_thread::get_id() != writer_thread_) {
    return std::unexpected(Error{ErrorCode::kWriterViolation, 0});
  }
  if (index >= databases_.size()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  MDB_txn* raw_transaction = nullptr;
  int status = ::mdb_txn_begin(environment_->get(), nullptr, 0, &raw_transaction);
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  TransactionHandle transaction(raw_transaction);
  status = ::mdb_drop(transaction.get(), databases_[index], 0);
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  auto written = WriteCheckpoint(transaction.get(), metadata_database_, projection, 0);
  if (!written) {
    return std::unexpected(written.error());
  }
  raw_transaction = transaction.release();
  status = ::mdb_txn_commit(raw_transaction);
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  return {};
}

std::expected<ReadSnapshot, Error> ProjectionStore::BeginSnapshot() const {
  MDB_txn* raw_transaction = nullptr;
  const int status =
      ::mdb_txn_begin(environment_->get(), nullptr, MDB_RDONLY, &raw_transaction);
  if (status != MDB_SUCCESS) {
    return std::unexpected(DatabaseError(status));
  }
  TransactionHandle transaction(raw_transaction);
  const auto epoch = ::mdb_txn_id(transaction.get());
  return ReadSnapshot(environment_, std::move(transaction), epoch, databases_,
                      metadata_database_);
}

}  // namespace neocortex::proj
