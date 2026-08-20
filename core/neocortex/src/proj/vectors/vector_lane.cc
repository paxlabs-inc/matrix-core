#include "proj/vectors/vector_lane.h"

#include <algorithm>
#include <array>
#include <bit>
#include <cerrno>
#include <cstring>
#include <utility>

#include <fcntl.h>
#include <hwy/highway.h>
#include <sys/mman.h>
#include <sys/stat.h>
#include <unistd.h>

#include "schema/events.h"

namespace neocortex::proj {
namespace {

constexpr std::array<std::byte, 8> kFileMagic = {
    std::byte{'N'}, std::byte{'C'}, std::byte{'V'}, std::byte{'E'},
    std::byte{'C'}, std::byte{'0'}, std::byte{'0'}, std::byte{'1'}};
constexpr std::array<std::byte, 4> kRecordMagic = {
    std::byte{'V'}, std::byte{'R'}, std::byte{'E'}, std::byte{'C'}};
constexpr std::size_t kFileHeaderBytes = 16;
constexpr std::size_t kRecordHeaderBytes = 32;
constexpr std::uint32_t kMaximumDimension = events::kMaximumEmbeddingDimension;

void AppendU32(std::vector<std::byte>& output, std::uint32_t value) {
  for (std::uint32_t shift = 0; shift < 32; shift += 8) {
    output.push_back(static_cast<std::byte>((value >> shift) & 0xffU));
  }
}

void AppendU64(std::vector<std::byte>& output, std::uint64_t value) {
  for (std::uint32_t shift = 0; shift < 64; shift += 8) {
    output.push_back(static_cast<std::byte>((value >> shift) & 0xffU));
  }
}

std::uint32_t ReadU32(std::span<const std::byte> bytes, std::size_t offset) {
  std::uint32_t value = 0;
  for (std::size_t index = 0; index < 4; ++index) {
    value |= std::to_integer<std::uint32_t>(bytes[offset + index])
             << (index * 8U);
  }
  return value;
}

std::uint64_t ReadU64(std::span<const std::byte> bytes, std::size_t offset) {
  std::uint64_t value = 0;
  for (std::size_t index = 0; index < 8; ++index) {
    value |= std::to_integer<std::uint64_t>(bytes[offset + index])
             << (index * 8U);
  }
  return value;
}

std::expected<void, Error> WriteAll(int descriptor,
                                    std::span<const std::byte> bytes,
                                    std::uint64_t offset) {
  std::size_t written = 0;
  while (written < bytes.size()) {
    const auto result = ::pwrite(
        descriptor, bytes.data() + written, bytes.size() - written,
        static_cast<off_t>(offset + static_cast<std::uint64_t>(written)));
    if (result < 0 && errno == EINTR) {
      continue;
    }
    if (result <= 0) {
      return std::unexpected(Error{ErrorCode::kWriteFailed, errno});
    }
    written += static_cast<std::size_t>(result);
  }
  return {};
}

std::vector<std::byte> EmptyFile() {
  std::vector<std::byte> header(kFileMagic.begin(), kFileMagic.end());
  AppendU32(header, 1);
  AppendU32(header, 0);
  return header;
}

struct RecordView final {
  std::uint64_t embedding_lsn;
  std::uint64_t target_lsn;
  std::span<const std::int8_t> quantized;
  std::span<const std::byte> binary;
  std::size_t end_offset;
};

std::expected<std::vector<RecordView>, Error> ParseRecords(
    std::span<const std::byte> bytes) {
  if (bytes.size() < kFileHeaderBytes ||
      !std::equal(kFileMagic.begin(), kFileMagic.end(), bytes.begin()) ||
      ReadU32(bytes, 8) != 1) {
    return std::unexpected(Error{ErrorCode::kVectorIndexCorrupt, 0});
  }
  std::vector<RecordView> records;
  std::size_t offset = kFileHeaderBytes;
  while (offset < bytes.size()) {
    if (bytes.size() - offset < kRecordHeaderBytes ||
        !std::equal(kRecordMagic.begin(), kRecordMagic.end(),
                    bytes.begin() + static_cast<std::ptrdiff_t>(offset))) {
      return std::unexpected(Error{ErrorCode::kVectorIndexCorrupt, 0, 0,
                                   offset});
    }
    const auto record_bytes = ReadU32(bytes, offset + 4U);
    const auto dimension = ReadU32(bytes, offset + 24U);
    const auto binary_bytes = ReadU32(bytes, offset + 28U);
    if (record_bytes < kRecordHeaderBytes || dimension == 0 ||
        dimension > kMaximumDimension ||
        binary_bytes != (dimension + 7U) / 8U ||
        record_bytes != kRecordHeaderBytes + dimension + binary_bytes ||
        record_bytes > bytes.size() - offset) {
      return std::unexpected(Error{ErrorCode::kVectorIndexCorrupt, 0, 0,
                                   offset});
    }
    const auto* quantized = reinterpret_cast<const std::int8_t*>(
        bytes.data() + offset + kRecordHeaderBytes);
    const auto* binary = bytes.data() + offset + kRecordHeaderBytes + dimension;
    records.push_back(RecordView{
        .embedding_lsn = ReadU64(bytes, offset + 8U),
        .target_lsn = ReadU64(bytes, offset + 16U),
        .quantized = {quantized, dimension},
        .binary = {binary, binary_bytes},
        .end_offset = offset + record_bytes,
    });
    offset += record_bytes;
  }
  return records;
}

std::vector<std::byte> MetadataKey(std::uint64_t target_lsn,
                                   std::uint64_t embedding_lsn) {
  std::vector<std::byte> key = {std::byte{'V'}};
  for (const auto value : {target_lsn, embedding_lsn}) {
    for (std::int32_t shift = 56; shift >= 0; shift -= 8) {
      key.push_back(static_cast<std::byte>((value >> shift) & 0xffU));
    }
  }
  return key;
}

std::uint64_t Hamming(std::span<const std::byte> first,
                      std::span<const std::byte> second) {
  std::uint64_t distance = 0;
  for (std::size_t index = 0; index < first.size(); ++index) {
    distance += static_cast<std::uint64_t>(std::popcount(
        std::to_integer<std::uint8_t>(first[index] ^ second[index])));
  }
  return distance;
}

}  // namespace

VectorLane::VectorLane(int descriptor, std::filesystem::path path)
    : descriptor_(descriptor), path_(std::move(path)) {}

VectorLane::VectorLane(VectorLane&& other) noexcept
    : descriptor_(std::exchange(other.descriptor_, -1)),
      path_(std::move(other.path_)),
      mapping_(std::exchange(other.mapping_, {})) {}

VectorLane& VectorLane::operator=(VectorLane&& other) noexcept {
  if (this != &other) {
    Close();
    descriptor_ = std::exchange(other.descriptor_, -1);
    path_ = std::move(other.path_);
    mapping_ = std::exchange(other.mapping_, {});
  }
  return *this;
}

VectorLane::~VectorLane() { Close(); }

void VectorLane::Close() {
  if (!mapping_.empty()) {
    ::munmap(const_cast<std::byte*>(mapping_.data()), mapping_.size());
    mapping_ = {};
  }
  if (descriptor_ >= 0) {
    ::close(descriptor_);
    descriptor_ = -1;
  }
}

std::expected<VectorLane, Error> VectorLane::Open(
    const std::filesystem::path& actor_directory) {
  std::error_code directory_error;
  std::filesystem::create_directories(actor_directory, directory_error);
  if (directory_error) {
    return std::unexpected(Error{ErrorCode::kOpenFailed,
                                 directory_error.value()});
  }
  const auto path = actor_directory / "vectors.flat";
  const int descriptor = ::open(path.c_str(), O_RDWR | O_CREAT | O_CLOEXEC, 0600);
  if (descriptor < 0) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  VectorLane lane(descriptor, path);
  struct stat status {};
  if (::fstat(descriptor, &status) != 0) {
    return std::unexpected(Error{ErrorCode::kReadFailed, errno});
  }
  if (status.st_size == 0) {
    const auto header = EmptyFile();
    auto written = WriteAll(descriptor, header, 0);
    if (!written || ::fdatasync(descriptor) != 0) {
      return std::unexpected(written ? Error{ErrorCode::kSyncFailed, errno}
                                     : written.error());
    }
  }
  auto mapped = lane.RefreshMapping();
  if (!mapped) {
    return std::unexpected(mapped.error());
  }
  auto parsed = ParseRecords(lane.mapping_);
  if (!parsed) {
    return std::unexpected(parsed.error());
  }
  return lane;
}

std::expected<void, Error> VectorLane::RefreshMapping() {
  if (!mapping_.empty()) {
    ::munmap(const_cast<std::byte*>(mapping_.data()), mapping_.size());
    mapping_ = {};
  }
  struct stat status {};
  if (::fstat(descriptor_, &status) != 0 || status.st_size < 0) {
    return std::unexpected(Error{ErrorCode::kReadFailed, errno});
  }
  const auto size = static_cast<std::size_t>(status.st_size);
  void* address = ::mmap(nullptr, size, PROT_READ, MAP_SHARED, descriptor_, 0);
  if (address == MAP_FAILED) {
    return std::unexpected(Error{ErrorCode::kReadFailed, errno});
  }
  mapping_ = {static_cast<const std::byte*>(address), size};
  return {};
}

std::expected<void, Error> VectorLane::ResetFile() {
  if (!mapping_.empty()) {
    ::munmap(const_cast<std::byte*>(mapping_.data()), mapping_.size());
    mapping_ = {};
  }
  if (::ftruncate(descriptor_, 0) != 0) {
    return std::unexpected(Error{ErrorCode::kWriteFailed, errno});
  }
  const auto header = EmptyFile();
  auto written = WriteAll(descriptor_, header, 0);
  if (!written || ::fdatasync(descriptor_) != 0) {
    return std::unexpected(written ? Error{ErrorCode::kSyncFailed, errno}
                                   : written.error());
  }
  return RefreshMapping();
}

std::expected<void, Error> VectorLane::ApplyEvent(ProjectionStore& store,
                                                  const log::Frame& frame) {
  auto verified = events::VerifyEvent(frame.sealed_payload, frame.header.kind,
                                      events::Boundary::kDisk);
  if (!verified) {
    auto error = verified.error();
    error.lsn = frame.header.lsn;
    return std::unexpected(error);
  }
  std::vector<std::byte> record;
  std::vector<std::byte> metadata_key;
  std::array<std::byte, 8> metadata_value{};
  const std::uint64_t original_size = mapping_.size();
  if (frame.header.kind == log::EventKind::kEmbedding) {
    const auto* embedding = verified->envelope->payload_as_Embedding();
    const auto dimension = embedding->dimension();
    const auto binary_bytes = embedding->binary_prefilter()->size();
    if (dimension == 0 || dimension > kMaximumDimension ||
        embedding->quantized()->size() != dimension ||
        binary_bytes != (dimension + 7U) / 8U ||
        embedding->target_lsn() == 0 ||
        embedding->target_lsn() >= frame.header.lsn) {
      auto skipped = store.Apply(ProjectionId::kVectorLane, frame.header.lsn,
                                 std::span<const Mutation>{});
      if (!skipped) {
        return std::unexpected(skipped.error());
      }
      return {};
    }
    const auto record_bytes = kRecordHeaderBytes + dimension + binary_bytes;
    record.insert(record.end(), kRecordMagic.begin(), kRecordMagic.end());
    AppendU32(record, static_cast<std::uint32_t>(record_bytes));
    AppendU64(record, frame.header.lsn);
    AppendU64(record, embedding->target_lsn());
    AppendU32(record, dimension);
    AppendU32(record, binary_bytes);
    const auto* quantized = reinterpret_cast<const std::byte*>(
        embedding->quantized()->Data());
    record.insert(record.end(), quantized, quantized + dimension);
    const auto* binary = reinterpret_cast<const std::byte*>(
        embedding->binary_prefilter()->Data());
    record.insert(record.end(), binary, binary + binary_bytes);
    auto written = WriteAll(descriptor_, record, original_size);
    if (!written || ::fdatasync(descriptor_) != 0) {
      return std::unexpected(written ? Error{ErrorCode::kSyncFailed, errno,
                                             frame.header.lsn}
                                     : written.error());
    }
    metadata_key = MetadataKey(embedding->target_lsn(), frame.header.lsn);
    for (std::size_t index = 0; index < metadata_value.size(); ++index) {
      metadata_value[index] = static_cast<std::byte>(
          (original_size >> (index * 8U)) & 0xffU);
    }
  }
  std::optional<Mutation> mutation;
  if (!metadata_key.empty()) {
    mutation = Mutation{.kind = MutationKind::kPut,
                        .key = metadata_key,
                        .value = metadata_value};
  }
  const auto mutations = mutation.has_value()
                             ? std::span<const Mutation>(&*mutation, 1)
                             : std::span<const Mutation>{};
  auto applied =
      store.Apply(ProjectionId::kVectorLane, frame.header.lsn, mutations);
  if (!applied) {
    if (!record.empty()) {
      if (::ftruncate(descriptor_, static_cast<off_t>(original_size)) != 0) {
        return std::unexpected(Error{ErrorCode::kWriteFailed, errno,
                                     frame.header.lsn});
      }
      if (::fdatasync(descriptor_) != 0) {
        return std::unexpected(Error{ErrorCode::kSyncFailed, errno,
                                     frame.header.lsn});
      }
      auto refreshed = RefreshMapping();
      if (!refreshed) {
        return std::unexpected(refreshed.error());
      }
    }
    return std::unexpected(applied.error());
  }
  if (!record.empty()) {
    auto mapped = RefreshMapping();
    if (!mapped) {
      return std::unexpected(mapped.error());
    }
  }
  return {};
}

std::expected<VectorRebuildProgress, Error> VectorLane::Rebuild(
    ProjectionStore& store, std::span<const log::Frame> frames, bool reset,
    std::size_t maximum_frames) {
  if (reset) {
    auto reset_projection = store.Reset(ProjectionId::kVectorLane);
    if (!reset_projection) {
      return std::unexpected(reset_projection.error());
    }
    auto reset_file = ResetFile();
    if (!reset_file) {
      return std::unexpected(reset_file.error());
    }
  }
  std::uint64_t checkpoint = 0;
  {
    auto snapshot = store.BeginSnapshot();
    if (!snapshot) {
      return std::unexpected(snapshot.error());
    }
    auto read_checkpoint = snapshot->Checkpoint(ProjectionId::kVectorLane);
    if (!read_checkpoint) {
      return std::unexpected(read_checkpoint.error());
    }
    checkpoint = *read_checkpoint;
  }
  if (checkpoint > frames.size()) {
    return std::unexpected(Error{ErrorCode::kProjectionCheckpoint, 0,
                                 checkpoint, frames.size()});
  }
  auto records = ParseRecords(mapping_);
  if (!records) {
    return std::unexpected(records.error());
  }
  std::size_t durable_bytes = kFileHeaderBytes;
  for (const auto& record : *records) {
    if (record.embedding_lsn > checkpoint) {
      break;
    }
    durable_bytes = record.end_offset;
  }
  if (durable_bytes != mapping_.size()) {
    if (!mapping_.empty()) {
      ::munmap(const_cast<std::byte*>(mapping_.data()), mapping_.size());
      mapping_ = {};
    }
    if (::ftruncate(descriptor_, static_cast<off_t>(durable_bytes)) != 0 ||
        ::fdatasync(descriptor_) != 0) {
      return std::unexpected(Error{ErrorCode::kWriteFailed, errno});
    }
    auto refreshed = RefreshMapping();
    if (!refreshed) {
      return std::unexpected(refreshed.error());
    }
  }
  const auto apply_count = std::min(
      frames.size() - static_cast<std::size_t>(checkpoint), maximum_frames);
  const auto begin = static_cast<std::size_t>(checkpoint);
  for (std::size_t index = begin; index < begin + apply_count; ++index) {
    if (frames[index].header.lsn != static_cast<std::uint64_t>(index) + 1U) {
      return std::unexpected(Error{ErrorCode::kSequenceViolation, 0,
                                   frames[index].header.lsn, index});
    }
    auto applied = ApplyEvent(store, frames[index]);
    if (!applied) {
      return std::unexpected(applied.error());
    }
  }
  const auto applied_lsn = checkpoint + static_cast<std::uint64_t>(apply_count);
  return VectorRebuildProgress{.applied_lsn = applied_lsn,
                               .applied_frames = apply_count,
                               .complete = applied_lsn == frames.size()};
}

std::int64_t VectorLane::ScalarDot(std::span<const std::int8_t> first,
                                   std::span<const std::int8_t> second) {
  if (first.size() != second.size()) {
    return std::numeric_limits<std::int64_t>::min();
  }
  std::int64_t sum = 0;
  for (std::size_t index = 0; index < first.size(); ++index) {
    sum += static_cast<std::int64_t>(first[index]) * second[index];
  }
  return sum;
}

std::int64_t VectorLane::SimdDot(std::span<const std::int8_t> first,
                                 std::span<const std::int8_t> second) {
  if (first.size() != second.size()) {
    return std::numeric_limits<std::int64_t>::min();
  }
  namespace hn = hwy::HWY_NAMESPACE;
  const hn::ScalableTag<std::int8_t> d8;
  const hn::Repartition<std::int16_t, decltype(d8)> d16;
  const hn::Repartition<std::int32_t, decltype(d16)> d32;
  const std::size_t lanes = hn::Lanes(d8);
  std::int64_t sum = 0;
  std::size_t offset = 0;
  while (offset < first.size()) {
    const auto count = std::min(lanes, first.size() - offset);
    const auto left = hn::LoadN(d8, first.data() + offset, count);
    const auto right = hn::LoadN(d8, second.data() + offset, count);
    const auto even = hn::MulEven(left, right);
    const auto odd = hn::MulOdd(left, right);
    sum += hn::ReduceSum(d32, hn::PromoteLowerTo(d32, even));
    sum += hn::ReduceSum(d32, hn::PromoteUpperTo(d32, even));
    sum += hn::ReduceSum(d32, hn::PromoteLowerTo(d32, odd));
    sum += hn::ReduceSum(d32, hn::PromoteUpperTo(d32, odd));
    offset += count;
  }
  return sum;
}

std::expected<std::vector<VectorHit>, Error> VectorLane::Search(
    std::span<const std::int8_t> quantized,
    std::span<const std::byte> binary_prefilter, std::size_t limit) const {
  if (quantized.empty() || quantized.size() > kMaximumDimension ||
      binary_prefilter.size() != (quantized.size() + 7U) / 8U || limit == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto records = ParseRecords(mapping_);
  if (!records) {
    return std::unexpected(records.error());
  }
  std::stable_sort(records->begin(), records->end(),
                   [&](const RecordView& first, const RecordView& second) {
                     const auto first_distance =
                         first.binary.size() == binary_prefilter.size()
                             ? Hamming(first.binary, binary_prefilter)
                             : std::numeric_limits<std::uint64_t>::max();
                     const auto second_distance =
                         second.binary.size() == binary_prefilter.size()
                             ? Hamming(second.binary, binary_prefilter)
                             : std::numeric_limits<std::uint64_t>::max();
                     return first_distance != second_distance
                                ? first_distance < second_distance
                                : first.embedding_lsn < second.embedding_lsn;
                   });
  std::vector<VectorHit> hits;
  hits.reserve(records->size());
  for (const auto& record : *records) {
    if (record.quantized.size() != quantized.size() ||
        record.binary.size() != binary_prefilter.size()) {
      continue;
    }
    hits.push_back(VectorHit{
        .target_lsn = record.target_lsn,
        .embedding_lsn = record.embedding_lsn,
        .score = SimdDot(quantized, record.quantized),
        .hamming_distance = Hamming(binary_prefilter, record.binary),
    });
  }
  std::sort(hits.begin(), hits.end(), [](const VectorHit& first,
                                         const VectorHit& second) {
    if (first.score != second.score) {
      return first.score > second.score;
    }
    if (first.target_lsn != second.target_lsn) {
      return first.target_lsn < second.target_lsn;
    }
    return first.embedding_lsn < second.embedding_lsn;
  });
  if (hits.size() > limit) {
    hits.resize(limit);
  }
  return hits;
}

std::expected<std::vector<std::byte>, Error> VectorLane::CanonicalBytes() const {
  auto parsed = ParseRecords(mapping_);
  if (!parsed) {
    return std::unexpected(parsed.error());
  }
  return std::vector<std::byte>(mapping_.begin(), mapping_.end());
}

}  // namespace neocortex::proj
