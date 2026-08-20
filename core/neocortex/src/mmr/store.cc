#include "mmr/store.h"

#include <algorithm>
#include <bit>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <fcntl.h>
#include <limits>
#include <string>
#include <string_view>
#include <sys/stat.h>
#include <unistd.h>
#include <vector>

#include <crc32c/crc32c.h>
#include <sodium/crypto_sign.h>

namespace neocortex::mmr {
namespace {

constexpr std::uint32_t kCheckpointMagic = 0x4E434350U;
constexpr std::uint32_t kCheckpointVersion = 1;
constexpr std::size_t kCheckpointBytes = 164;
constexpr std::size_t kCheckpointChecksumOffset = 160;
constexpr std::size_t kNodeRecordBytes = 60;
constexpr std::size_t kNodeChecksumOffset = 56;
constexpr std::string_view kCheckpointDomain = "neocortex-checkpoint-v1";

class FileDescriptor final {
 public:
  explicit FileDescriptor(int value = -1) : value_(value) {}
  ~FileDescriptor() {
    if (value_ >= 0) {
      static_cast<void>(::close(value_));
    }
  }
  FileDescriptor(FileDescriptor&& other) noexcept : value_(other.Release()) {}
  FileDescriptor& operator=(FileDescriptor&& other) noexcept {
    if (this != &other) {
      if (value_ >= 0) {
        static_cast<void>(::close(value_));
      }
      value_ = other.Release();
    }
    return *this;
  }
  FileDescriptor(const FileDescriptor&) = delete;
  FileDescriptor& operator=(const FileDescriptor&) = delete;

  [[nodiscard]] int get() const { return value_; }
  [[nodiscard]] bool valid() const { return value_ >= 0; }

 private:
  int Release() {
    const int value = value_;
    value_ = -1;
    return value;
  }
  int value_;
};

struct FilePosition final {
  std::uint64_t offset;
};

struct NodeLocation final {
  int descriptor;
  std::uint64_t record_index;
};

void PutU16(std::span<std::byte> output, std::size_t offset, std::uint16_t value) {
  output[offset] = static_cast<std::byte>(value & 0xffU);
  output[offset + 1] = static_cast<std::byte>((value >> 8U) & 0xffU);
}

void PutU32(std::span<std::byte> output, std::size_t offset, std::uint32_t value) {
  for (std::size_t index = 0; index < 4; ++index) {
    output[offset + index] = static_cast<std::byte>((value >> (index * 8U)) & 0xffU);
  }
}

void PutU64(std::span<std::byte> output, std::size_t offset, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    output[offset + index] = static_cast<std::byte>((value >> (index * 8U)) & 0xffU);
  }
}

std::uint16_t GetU16(std::span<const std::byte> input, std::size_t offset) {
  return static_cast<std::uint16_t>(
      std::to_integer<std::uint16_t>(input[offset]) |
      (std::to_integer<std::uint16_t>(input[offset + 1]) << 8U));
}

std::uint32_t GetU32(std::span<const std::byte> input, std::size_t offset) {
  std::uint32_t value = 0;
  for (std::size_t index = 0; index < 4; ++index) {
    value |= std::to_integer<std::uint32_t>(input[offset + index]) << (index * 8U);
  }
  return value;
}

std::uint64_t GetU64(std::span<const std::byte> input, std::size_t offset) {
  std::uint64_t value = 0;
  for (std::size_t index = 0; index < 8; ++index) {
    value |= std::to_integer<std::uint64_t>(input[offset + index]) << (index * 8U);
  }
  return value;
}

std::uint32_t Checksum(std::span<const std::byte> bytes, std::size_t checksum_offset) {
  std::vector<std::byte> copy(bytes.begin(), bytes.end());
  PutU32(copy, checksum_offset, 0);
  return crc32c::Crc32c(reinterpret_cast<const char*>(copy.data()), copy.size());
}

std::expected<void, Error> WriteAllAt(int descriptor, std::span<const std::byte> bytes,
                                      FilePosition position) {
  std::size_t written = 0;
  while (written < bytes.size()) {
    const ssize_t result = ::pwrite(descriptor, bytes.data() + written, bytes.size() - written,
                                    static_cast<off_t>(position.offset + written));
    if (result < 0) {
      if (errno == EINTR) {
        continue;
      }
      return std::unexpected(
          Error{ErrorCode::kWriteFailed, errno, 0, position.offset + written});
    }
    if (result == 0) {
      return std::unexpected(
          Error{ErrorCode::kWriteFailed, 0, 0, position.offset + written});
    }
    written += static_cast<std::size_t>(result);
  }
  return {};
}

std::expected<std::vector<std::byte>, Error> ReadFile(const std::filesystem::path& path) {
  const FileDescriptor descriptor(::open(path.c_str(), O_RDONLY | O_CLOEXEC));
  if (!descriptor.valid()) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  struct stat info {};
  if (::fstat(descriptor.get(), &info) != 0 || info.st_size < 0) {
    return std::unexpected(Error{ErrorCode::kReadFailed, errno});
  }
  std::vector<std::byte> bytes(static_cast<std::size_t>(info.st_size));
  std::size_t read_bytes = 0;
  while (read_bytes < bytes.size()) {
    const ssize_t result = ::pread(descriptor.get(), bytes.data() + read_bytes,
                                   bytes.size() - read_bytes,
                                   static_cast<off_t>(read_bytes));
    if (result < 0) {
      if (errno == EINTR) {
        continue;
      }
      return std::unexpected(Error{ErrorCode::kReadFailed, errno, 0, read_bytes});
    }
    if (result == 0) {
      return std::unexpected(Error{ErrorCode::kTruncated, 0, 0, read_bytes});
    }
    read_bytes += static_cast<std::size_t>(result);
  }
  return bytes;
}

std::expected<void, Error> SyncDirectory(const std::filesystem::path& directory) {
  const FileDescriptor descriptor(
      ::open(directory.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC));
  if (!descriptor.valid()) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  if (::fsync(descriptor.get()) != 0) {
    return std::unexpected(Error{ErrorCode::kSyncFailed, errno});
  }
  return {};
}

std::vector<std::byte> CheckpointMessage(const Checkpoint& checkpoint) {
  std::vector<std::byte> message(kCheckpointDomain.size() + 2U + 8U + 8U + 32U);
  std::memcpy(message.data(), kCheckpointDomain.data(), kCheckpointDomain.size());
  std::size_t offset = kCheckpointDomain.size();
  PutU16(message, offset, checkpoint.actor);
  offset += 2U;
  PutU64(message, offset, checkpoint.lsn);
  offset += 8U;
  PutU64(message, offset, checkpoint.leaf_count);
  offset += 8U;
  std::memcpy(message.data() + offset, checkpoint.root.data(), checkpoint.root.size());
  return message;
}

std::vector<std::byte> EncodeCheckpoint(const Checkpoint& checkpoint) {
  std::vector<std::byte> bytes(kCheckpointBytes);
  PutU32(bytes, 0, kCheckpointMagic);
  PutU32(bytes, 4, kCheckpointVersion);
  PutU16(bytes, 8, checkpoint.actor);
  PutU64(bytes, 16, checkpoint.lsn);
  PutU64(bytes, 24, checkpoint.leaf_count);
  std::memcpy(bytes.data() + 32, checkpoint.root.data(), checkpoint.root.size());
  std::memcpy(bytes.data() + 64, checkpoint.public_key.data(), checkpoint.public_key.size());
  std::memcpy(bytes.data() + 96, checkpoint.signature.data(), checkpoint.signature.size());
  PutU32(bytes, kCheckpointChecksumOffset, Checksum(bytes, kCheckpointChecksumOffset));
  return bytes;
}

std::vector<std::byte> EncodeNodes(std::span<const Node> nodes) {
  std::vector<std::byte> bytes(nodes.size() * kNodeRecordBytes);
  for (std::size_t index = 0; index < nodes.size(); ++index) {
    const Node& node = nodes[index];
    const std::size_t offset = index * kNodeRecordBytes;
    bytes[offset] = static_cast<std::byte>(node.height);
    PutU64(bytes, offset + 8, node.start);
    PutU64(bytes, offset + 16, node.leaf_count);
    std::memcpy(bytes.data() + offset + 24, node.hash.data(), node.hash.size());
    const auto record = std::span<const std::byte>(bytes).subspan(offset, kNodeRecordBytes);
    PutU32(bytes, offset + kNodeChecksumOffset, Checksum(record, kNodeChecksumOffset));
  }
  return bytes;
}

std::expected<std::vector<Node>, Error> DecodeNodes(std::span<const std::byte> bytes) {
  if (bytes.size() % kNodeRecordBytes != 0) {
    return std::unexpected(Error{ErrorCode::kTruncated, 0, 0, bytes.size()});
  }
  std::vector<Node> nodes;
  nodes.reserve(bytes.size() / kNodeRecordBytes);
  for (std::size_t offset = 0; offset < bytes.size(); offset += kNodeRecordBytes) {
    const auto record = bytes.subspan(offset, kNodeRecordBytes);
    if (GetU32(record, kNodeChecksumOffset) != Checksum(record, kNodeChecksumOffset)) {
      return std::unexpected(Error{ErrorCode::kChecksumMismatch, 0, 0, offset});
    }
    Node node{.height = std::to_integer<std::uint8_t>(record[0]),
              .start = GetU64(record, 8),
              .leaf_count = GetU64(record, 16),
              .hash = {}};
    std::memcpy(node.hash.data(), record.data() + 24, node.hash.size());
    nodes.push_back(node);
  }
  return nodes;
}

std::expected<Node, Error> ReadNodeRecord(NodeLocation location) {
  const auto descriptor = location.descriptor;
  const auto record_index = location.record_index;
  if (record_index > std::numeric_limits<std::uint64_t>::max() /
                         kNodeRecordBytes) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  std::array<std::byte, kNodeRecordBytes> bytes{};
  std::size_t read_bytes = 0;
  const auto base = record_index * kNodeRecordBytes;
  while (read_bytes < bytes.size()) {
    const auto result = ::pread(
        descriptor, bytes.data() + read_bytes, bytes.size() - read_bytes,
        static_cast<off_t>(base + read_bytes));
    if (result < 0 && errno == EINTR) {
      continue;
    }
    if (result <= 0) {
      return std::unexpected(Error{ErrorCode::kTruncated, result < 0 ? errno : 0,
                                   0, base + read_bytes});
    }
    read_bytes += static_cast<std::size_t>(result);
  }
  auto nodes = DecodeNodes(bytes);
  if (!nodes || nodes->size() != 1) {
    return std::unexpected(nodes ? Error{ErrorCode::kCheckpointMismatch, 0,
                                         0, base}
                                 : nodes.error());
  }
  return nodes->front();
}

std::expected<Node, Error> ReadNodeRecord(const std::filesystem::path& path,
                                          std::uint64_t record_index) {
  const FileDescriptor descriptor(::open(path.c_str(), O_RDONLY | O_CLOEXEC));
  if (!descriptor.valid()) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  return ReadNodeRecord(
      NodeLocation{.descriptor = descriptor.get(), .record_index = record_index});
}

std::expected<Mmr, Error> RestoreMmrFile(const std::filesystem::path& path) {
  const FileDescriptor descriptor(::open(path.c_str(), O_RDONLY | O_CLOEXEC));
  if (!descriptor.valid()) {
    if (errno == ENOENT) {
      return Mmr(false);
    }
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  struct stat info {};
  if (::fstat(descriptor.get(), &info) != 0 || info.st_size < 0 ||
      static_cast<std::uint64_t>(info.st_size) % kNodeRecordBytes != 0) {
    return std::unexpected(Error{ErrorCode::kTruncated, errno});
  }
  const auto record_count =
      static_cast<std::uint64_t>(info.st_size) / kNodeRecordBytes;
  Mmr mmr(false);
  std::uint64_t record_index = 0;
  while (record_index < record_count) {
    auto leaf = ReadNodeRecord(
        NodeLocation{.descriptor = descriptor.get(), .record_index = record_index});
    if (!leaf || leaf->height != 0 || leaf->leaf_count != 1 ||
        leaf->start != mmr.leaf_count()) {
      return std::unexpected(leaf ? Error{ErrorCode::kCheckpointMismatch, 0,
                                          mmr.leaf_count(), record_index}
                                  : leaf.error());
    }
    const auto append = mmr.Append(leaf->hash);
    if (append.created_nodes.size() > record_count - record_index) {
      return std::unexpected(
          Error{ErrorCode::kTruncated, 0, mmr.leaf_count(), record_index});
    }
    for (std::size_t created = 0; created < append.created_nodes.size(); ++created) {
      auto stored = ReadNodeRecord(NodeLocation{
          .descriptor = descriptor.get(), .record_index = record_index + created});
      if (!stored || *stored != append.created_nodes[created]) {
        return std::unexpected(stored ? Error{ErrorCode::kCheckpointMismatch, 0,
                                              mmr.leaf_count(),
                                              record_index + created}
                                      : stored.error());
      }
    }
    record_index += append.created_nodes.size();
  }
  return mmr;
}

std::expected<Hash, Error> RootAtFile(const std::filesystem::path& path,
                                      std::uint64_t leaf_count) {
  std::vector<Node> peaks;
  peaks.reserve(64);
  std::uint64_t start = 0;
  std::uint64_t remaining = leaf_count;
  while (remaining > 0) {
    const auto count = std::bit_floor(remaining);
    const auto height = static_cast<std::uint8_t>(std::countr_zero(count));
    const auto last = start + count - 1U;
    const auto record_index =
        last * 2U - static_cast<std::uint64_t>(std::popcount(last)) + height;
    auto node = ReadNodeRecord(path, record_index);
    if (!node || node->start != start || node->leaf_count != count ||
        node->height != height) {
      return std::unexpected(node ? Error{ErrorCode::kCheckpointMismatch, 0,
                                          start, record_index}
                                  : node.error());
    }
    peaks.push_back(*node);
    start += count;
    remaining -= count;
  }
  return RootFromPeaks(leaf_count, peaks);
}

std::expected<Mmr, Error> RestoreMmr(std::span<const Node> records,
                                    bool retain_history) {
  Mmr mmr(retain_history);
  std::size_t index = 0;
  while (index < records.size()) {
    if (records[index].height != 0 || records[index].leaf_count != 1 ||
        records[index].start != mmr.leaf_count()) {
      return std::unexpected(
          Error{ErrorCode::kCheckpointMismatch, 0, mmr.leaf_count(), index});
    }
    const AppendResult append = mmr.Append(records[index].hash);
    if (append.created_nodes.size() > records.size() - index) {
      return std::unexpected(
          Error{ErrorCode::kTruncated, 0, mmr.leaf_count(), index});
    }
    for (std::size_t created = 0; created < append.created_nodes.size(); ++created) {
      if (append.created_nodes[created] != records[index + created]) {
        return std::unexpected(
            Error{ErrorCode::kCheckpointMismatch, 0, mmr.leaf_count(), index + created});
      }
    }
    index += append.created_nodes.size();
  }
  return mmr;
}

std::filesystem::path CheckpointPath(const std::filesystem::path& directory,
                                     std::uint64_t lsn) {
  std::array<char, 26> name{};
  const int written = std::snprintf(name.data(), name.size(), "%020llu.ckpt",
                                    static_cast<unsigned long long>(lsn));
  if (written != 25) {
    return directory / "invalid.ckpt";
  }
  return directory / name.data();
}

std::expected<std::uint64_t, Error> ParseCheckpointLsn(std::string_view name) {
  if (name.size() != 25 || name.substr(20) != ".ckpt") {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  std::uint64_t value = 0;
  for (std::size_t index = 0; index < 20; ++index) {
    if (name[index] < '0' || name[index] > '9') {
      return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
    }
    const auto digit = static_cast<std::uint64_t>(name[index] - '0');
    if (value > (std::numeric_limits<std::uint64_t>::max() - digit) / 10U) {
      return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
    }
    value = value * 10U + digit;
  }
  return value;
}

}  // namespace

std::expected<SigningKeyPair, Error> SigningKeyPairFromSeed(const SigningSeed& seed) {
  SigningKeyPair key_pair{};
  if (crypto_sign_seed_keypair(reinterpret_cast<unsigned char*>(key_pair.public_key.data()),
                               reinterpret_cast<unsigned char*>(key_pair.secret_key.data()),
                               reinterpret_cast<const unsigned char*>(seed.data())) != 0) {
    return std::unexpected(Error{ErrorCode::kSignatureInvalid, 0});
  }
  return key_pair;
}

std::expected<Checkpoint, Error> LoadCheckpoint(const std::filesystem::path& path) {
  auto bytes = ReadFile(path);
  if (!bytes) {
    return std::unexpected(bytes.error());
  }
  return DecodeCheckpoint(*bytes);
}

std::expected<Checkpoint, Error> DecodeCheckpoint(std::span<const std::byte> encoded) {
  if (encoded.size() != kCheckpointBytes) {
    return std::unexpected(Error{ErrorCode::kTruncated, 0, 0, encoded.size()});
  }
  if (GetU32(encoded, 0) != kCheckpointMagic ||
      GetU32(encoded, 4) != kCheckpointVersion ||
      GetU32(encoded, kCheckpointChecksumOffset) !=
          Checksum(encoded, kCheckpointChecksumOffset)) {
    return std::unexpected(Error{ErrorCode::kCheckpointMismatch, 0});
  }
  Checkpoint checkpoint{.actor = GetU16(encoded, 8),
                        .lsn = GetU64(encoded, 16),
                        .leaf_count = GetU64(encoded, 24),
                        .root = {},
                        .public_key = {},
                        .signature = {}};
  std::memcpy(checkpoint.root.data(), encoded.data() + 32, checkpoint.root.size());
  std::memcpy(checkpoint.public_key.data(), encoded.data() + 64,
              checkpoint.public_key.size());
  std::memcpy(checkpoint.signature.data(), encoded.data() + 96,
              checkpoint.signature.size());
  return checkpoint;
}

std::expected<Mmr, Error> RestorePeakRecords(std::span<const std::byte> encoded) {
  auto nodes = DecodeNodes(encoded);
  if (!nodes) {
    return std::unexpected(nodes.error());
  }
  return RestoreMmr(*nodes, true);
}

std::expected<void, Error> VerifyCheckpointSignature(
    const Checkpoint& checkpoint, const PublicKey& expected_public_key) {
  if (checkpoint.public_key != expected_public_key) {
    return std::unexpected(Error{ErrorCode::kSignatureInvalid, 0, checkpoint.lsn});
  }
  const auto message = CheckpointMessage(checkpoint);
  if (crypto_sign_verify_detached(
          reinterpret_cast<const unsigned char*>(checkpoint.signature.data()),
          reinterpret_cast<const unsigned char*>(message.data()), message.size(),
          reinterpret_cast<const unsigned char*>(expected_public_key.data())) != 0) {
    return std::unexpected(Error{ErrorCode::kSignatureInvalid, 0, checkpoint.lsn});
  }
  return {};
}

MmrStore::MmrStore(std::filesystem::path actor_directory, std::uint16_t actor,
                   PublicKey expected_public_key, Mmr mmr, VerificationStatus status)
    : actor_directory_(std::move(actor_directory)),
      mmr_directory_(actor_directory_ / "mmr"),
      checkpoint_directory_(mmr_directory_ / "checkpoints"),
      actor_(actor),
      expected_public_key_(expected_public_key),
      mmr_(std::move(mmr)),
      status_(status) {}

std::expected<Hash, Error> MmrStore::LeafHash(std::uint64_t index) const {
  if (index >= mmr_.leaf_count() ||
      index > std::numeric_limits<std::uint64_t>::max() / 2U) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0, index});
  }
  const auto record_index =
      index * 2U - static_cast<std::uint64_t>(std::popcount(index));
  auto node = ReadNodeRecord(mmr_directory_ / "peaks", record_index);
  if (!node || node->height != 0 || node->start != index ||
      node->leaf_count != 1) {
    return std::unexpected(node ? Error{ErrorCode::kCheckpointMismatch, 0,
                                        index, record_index}
                                : node.error());
  }
  return node->hash;
}

std::expected<void, Error> MmrStore::VerifyLeafHashes(
    std::uint64_t first_index, std::span<const Hash> hashes) const {
  if (first_index > mmr_.leaf_count() ||
      hashes.size() > mmr_.leaf_count() - first_index) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0, first_index});
  }
  const FileDescriptor descriptor(
      ::open((mmr_directory_ / "peaks").c_str(), O_RDONLY | O_CLOEXEC));
  if (!descriptor.valid()) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  for (std::size_t offset = 0; offset < hashes.size(); ++offset) {
    const auto index = first_index + static_cast<std::uint64_t>(offset);
    if (index > std::numeric_limits<std::uint64_t>::max() / 2U) {
      return std::unexpected(Error{ErrorCode::kInvalidArgument, 0, index});
    }
    const auto record_index =
        index * 2U - static_cast<std::uint64_t>(std::popcount(index));
    auto node = ReadNodeRecord(
        NodeLocation{.descriptor = descriptor.get(), .record_index = record_index});
    if (!node || node->height != 0 || node->start != index ||
        node->leaf_count != 1 || node->hash != hashes[offset]) {
      return std::unexpected(node ? Error{ErrorCode::kCheckpointMismatch, 0,
                                          index, record_index}
                                  : node.error());
    }
  }
  return {};
}

std::expected<Hash, Error> MmrStore::RootAt(std::uint64_t leaf_count) const {
  if (leaf_count > mmr_.leaf_count()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0, leaf_count});
  }
  return RootAtFile(mmr_directory_ / "peaks", leaf_count);
}

std::expected<RangeProof, Error> MmrStore::ProveRange(
    std::uint64_t range_start, std::uint64_t range_leaf_count) const {
  auto encoded = ReadFile(mmr_directory_ / "peaks");
  if (!encoded) {
    return std::unexpected(encoded.error());
  }
  auto full = RestorePeakRecords(*encoded);
  if (!full) {
    return std::unexpected(full.error());
  }
  return full->ProveRange(range_start, range_leaf_count);
}

std::expected<MmrStore, Error> MmrStore::Open(
    const std::filesystem::path& actor_directory, std::uint16_t actor,
    const PublicKey& expected_public_key) {
  if (actor == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  const auto mmr_directory = actor_directory / "mmr";
  const auto checkpoint_directory = mmr_directory / "checkpoints";
  std::error_code directory_error;
  std::filesystem::create_directories(checkpoint_directory, directory_error);
  if (directory_error) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, directory_error.value()});
  }

  auto restored = RestoreMmrFile(mmr_directory / "peaks");
  if (!restored) {
    return std::unexpected(restored.error());
  }
  Mmr mmr = std::move(*restored);

  std::vector<std::pair<std::uint64_t, std::filesystem::path>> checkpoints;
  std::filesystem::directory_iterator iterator(checkpoint_directory, directory_error);
  const std::filesystem::directory_iterator end;
  while (!directory_error && iterator != end) {
    if (iterator->is_regular_file(directory_error)) {
      if (directory_error) {
        break;
      }
      const std::string filename = iterator->path().filename().string();
      auto lsn = ParseCheckpointLsn(filename);
      if (lsn) {
        checkpoints.emplace_back(*lsn, iterator->path());
      } else if (filename.ends_with(".ckpt")) {
        return std::unexpected(lsn.error());
      }
    }
    iterator.increment(directory_error);
  }
  if (directory_error) {
    return std::unexpected(Error{ErrorCode::kReadFailed, directory_error.value()});
  }
  std::sort(checkpoints.begin(), checkpoints.end(), [](const auto& left, const auto& right) {
    return left.first < right.first;
  });

  VerificationStatus status{.root = mmr.Root(),
                            .leaf_count = mmr.leaf_count(),
                            .last_checkpoint_lsn = 0,
                            .verified = false};
  for (const auto& [filename_lsn, path] : checkpoints) {
    auto checkpoint = LoadCheckpoint(path);
    if (!checkpoint) {
      return std::unexpected(checkpoint.error());
    }
    auto signature = VerifyCheckpointSignature(*checkpoint, expected_public_key);
    auto root = RootAtFile(mmr_directory / "peaks", checkpoint->leaf_count);
    if (!signature || !root || checkpoint->actor != actor ||
        checkpoint->lsn != filename_lsn || checkpoint->lsn != checkpoint->leaf_count ||
        checkpoint->root != *root) {
      return std::unexpected(
          Error{ErrorCode::kCheckpointMismatch, 0, checkpoint->lsn});
    }
    status.last_checkpoint_lsn = checkpoint->lsn;
    status.verified = true;
  }
  return MmrStore(actor_directory, actor, expected_public_key, std::move(mmr), status);
}

std::expected<Hash, Error> MmrStore::AppendFrame(
    const log::FrameHeader& header, std::span<const std::byte> plaintext_payload) {
  if (header.actor != actor_ || header.lsn != mmr_.leaf_count() + 1U) {
    return std::unexpected(Error{ErrorCode::kSequenceViolation, 0, header.lsn});
  }
  const Hash frame_hash = HashFramePlaintext(header, plaintext_payload);
  const AppendResult append = mmr_.Append(frame_hash);
  const auto encoded = EncodeNodes(append.created_nodes);
  const auto peaks_path = mmr_directory_ / "peaks";
  const FileDescriptor descriptor(
      ::open(peaks_path.c_str(), O_CREAT | O_RDWR | O_CLOEXEC, 0600));
  if (!descriptor.valid()) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno, header.lsn});
  }
  struct stat info {};
  if (::fstat(descriptor.get(), &info) != 0 || info.st_size < 0) {
    return std::unexpected(Error{ErrorCode::kReadFailed, errno, header.lsn});
  }
  auto write = WriteAllAt(descriptor.get(), encoded,
                          FilePosition{static_cast<std::uint64_t>(info.st_size)});
  if (!write) {
    return std::unexpected(write.error());
  }
  if (::fdatasync(descriptor.get()) != 0) {
    return std::unexpected(Error{ErrorCode::kSyncFailed, errno, header.lsn});
  }
  auto directory_sync = SyncDirectory(mmr_directory_);
  if (!directory_sync) {
    return std::unexpected(directory_sync.error());
  }
  status_.root = append.root;
  status_.leaf_count = mmr_.leaf_count();
  return append.root;
}

std::expected<void, Error> MmrStore::RewindTo(std::uint64_t leaf_count) {
  if (mmr_.leaf_count() <= leaf_count) {
    return {};
  }
  if (status_.last_checkpoint_lsn > leaf_count) {
    return std::unexpected(Error{ErrorCode::kCheckpointMismatch, 0,
                                 status_.last_checkpoint_lsn, leaf_count});
  }
  const auto record_count =
      leaf_count * 2U -
      static_cast<std::uint64_t>(std::popcount(leaf_count));
  const auto peaks_path = mmr_directory_ / "peaks";
  const FileDescriptor descriptor(
      ::open(peaks_path.c_str(), O_RDWR | O_CLOEXEC));
  if (!descriptor.valid()) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno, leaf_count});
  }
  if (::ftruncate(descriptor.get(),
                  static_cast<off_t>(record_count * kNodeRecordBytes)) != 0) {
    return std::unexpected(Error{ErrorCode::kWriteFailed, errno, leaf_count});
  }
  if (::fdatasync(descriptor.get()) != 0) {
    return std::unexpected(Error{ErrorCode::kSyncFailed, errno, leaf_count});
  }
  auto directory_sync = SyncDirectory(mmr_directory_);
  if (!directory_sync) {
    return std::unexpected(directory_sync.error());
  }
  auto restored = RestoreMmrFile(peaks_path);
  if (!restored) {
    return std::unexpected(restored.error());
  }
  if (restored->leaf_count() != leaf_count) {
    return std::unexpected(Error{ErrorCode::kCheckpointMismatch, 0,
                                 restored->leaf_count(), leaf_count});
  }
  mmr_ = std::move(*restored);
  status_.root = mmr_.Root();
  status_.leaf_count = mmr_.leaf_count();
  return {};
}

std::expected<Checkpoint, Error> MmrStore::CreateCheckpoint(
    const SigningKeyPair& key_pair) {
  if (key_pair.public_key != expected_public_key_ || mmr_.leaf_count() == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0, mmr_.leaf_count()});
  }
  Checkpoint checkpoint{.actor = actor_,
                        .lsn = mmr_.leaf_count(),
                        .leaf_count = mmr_.leaf_count(),
                        .root = mmr_.Root(),
                        .public_key = key_pair.public_key,
                        .signature = {}};
  const auto message = CheckpointMessage(checkpoint);
  unsigned long long signature_bytes = 0;
  if (crypto_sign_detached(
          reinterpret_cast<unsigned char*>(checkpoint.signature.data()), &signature_bytes,
          reinterpret_cast<const unsigned char*>(message.data()), message.size(),
          reinterpret_cast<const unsigned char*>(key_pair.secret_key.data())) != 0 ||
      signature_bytes != checkpoint.signature.size()) {
    return std::unexpected(Error{ErrorCode::kSignatureInvalid, 0, checkpoint.lsn});
  }
  const auto encoded = EncodeCheckpoint(checkpoint);
  const auto path = CheckpointPath(checkpoint_directory_, checkpoint.lsn);
  const FileDescriptor descriptor(
      ::open(path.c_str(), O_CREAT | O_EXCL | O_WRONLY | O_CLOEXEC, 0600));
  if (!descriptor.valid()) {
    if (errno == EEXIST) {
      auto existing = LoadCheckpoint(path);
      if (existing && *existing == checkpoint) {
        return *existing;
      }
      return std::unexpected(Error{ErrorCode::kAlreadyExists, errno, checkpoint.lsn});
    }
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno, checkpoint.lsn});
  }
  auto write = WriteAllAt(descriptor.get(), encoded, FilePosition{0});
  if (!write) {
    return std::unexpected(write.error());
  }
  if (::fdatasync(descriptor.get()) != 0) {
    return std::unexpected(Error{ErrorCode::kSyncFailed, errno, checkpoint.lsn});
  }
  auto directory_sync = SyncDirectory(checkpoint_directory_);
  if (!directory_sync) {
    return std::unexpected(directory_sync.error());
  }
  status_.last_checkpoint_lsn = checkpoint.lsn;
  status_.verified = true;
  return checkpoint;
}

}  // namespace neocortex::mmr
