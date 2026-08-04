#include "log/segment_log.h"

#include <algorithm>
#include <cerrno>
#include <cstdio>
#include <cstring>
#include <fcntl.h>
#include <limits>
#include <memory>
#include <string>
#include <string_view>
#include <sys/mman.h>
#include <sys/stat.h>
#include <sys/syscall.h>
#include <unistd.h>

#include <crc32c/crc32c.h>
#include <liburing/io_uring.h>

namespace neocortex::log {
namespace {

constexpr std::uint32_t kManifestMagic = 0x4E434D46U;
constexpr std::uint32_t kManifestVersion = 1;
constexpr std::size_t kManifestHeaderBytes = 16;
constexpr std::size_t kManifestEntryBytes = 32;
constexpr std::string_view kManifestName = "MANIFEST";

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

struct MunmapDeleter final {
  std::size_t bytes;
  void operator()(std::byte* value) const {
    if (value != nullptr) {
      static_cast<void>(::munmap(value, bytes));
    }
  }
};

using MappingPointer = std::unique_ptr<std::byte, MunmapDeleter>;

struct Alignment final {
  std::uint32_t bytes;
};

struct IoPosition final {
  std::uint64_t offset;
  std::size_t maximum_bytes;
};

struct ScannedSegment final {
  std::uint64_t id;
  std::uint64_t first_lsn = 0;
  std::uint64_t last_lsn = 0;
  std::uint64_t physical_bytes = 0;
};

struct ScanResult final {
  ScannedSegment segment;
  std::uint64_t next_lsn;
  std::uint64_t recovered_lsn = 0;
  bool truncated = false;
  std::vector<Frame> frames;
};

struct ScanSettings final {
  std::uint64_t id;
  std::uint16_t actor;
  std::uint64_t expected_lsn;
  bool physical_tail;
  bool recover_tail;
  bool collect_frames;
  std::uint64_t stop_after_lsn = 0;
};

struct WriteResult final {
  WriteBackend backend;
};

std::uint64_t AlignUp(std::uint64_t value, Alignment alignment) {
  const auto wide_alignment = static_cast<std::uint64_t>(alignment.bytes);
  return ((value + wide_alignment - 1U) / wide_alignment) * wide_alignment;
}

void PutU32(std::span<std::byte> out, std::size_t offset, std::uint32_t value) {
  for (std::size_t index = 0; index < 4; ++index) {
    out[offset + index] = static_cast<std::byte>((value >> (index * 8U)) & 0xffU);
  }
}

void PutU64(std::span<std::byte> out, std::size_t offset, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    out[offset + index] = static_cast<std::byte>((value >> (index * 8U)) & 0xffU);
  }
}

std::uint32_t GetU32(std::span<const std::byte> in, std::size_t offset) {
  std::uint32_t value = 0;
  for (std::size_t index = 0; index < 4; ++index) {
    value |= std::to_integer<std::uint32_t>(in[offset + index]) << (index * 8U);
  }
  return value;
}

std::expected<void, Error> PwriteAll(int descriptor, std::span<const std::byte> bytes,
                                     IoPosition position) {
  std::size_t written = 0;
  while (written < bytes.size()) {
    const std::size_t remaining = bytes.size() - written;
    const std::size_t request = std::min(remaining, position.maximum_bytes);
    const ssize_t result = ::pwrite(descriptor, bytes.data() + written, request,
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

std::expected<std::size_t, Error> PreadUpTo(int descriptor, std::span<std::byte> bytes,
                                           IoPosition position) {
  std::size_t read_bytes = 0;
  while (read_bytes < bytes.size()) {
    const std::size_t remaining = bytes.size() - read_bytes;
    const std::size_t request = std::min(remaining, position.maximum_bytes);
    const ssize_t result = ::pread(descriptor, bytes.data() + read_bytes, request,
                                   static_cast<off_t>(position.offset + read_bytes));
    if (result < 0) {
      if (errno == EINTR) {
        continue;
      }
      return std::unexpected(
          Error{ErrorCode::kReadFailed, errno, 0, position.offset + read_bytes});
    }
    if (result == 0) {
      break;
    }
    read_bytes += static_cast<std::size_t>(result);
  }
  return read_bytes;
}

std::expected<void, Error> PreadExact(int descriptor, std::span<std::byte> bytes,
                                     IoPosition position) {
  auto read_bytes = PreadUpTo(descriptor, bytes, position);
  if (!read_bytes) {
    return std::unexpected(read_bytes.error());
  }
  if (*read_bytes != bytes.size()) {
    return std::unexpected(
        Error{ErrorCode::kTruncated, 0, 0, position.offset + *read_bytes});
  }
  return {};
}

std::expected<bool, Error> RangeIsZero(int descriptor, std::uint64_t begin,
                                       std::uint64_t end, std::size_t maximum_io_bytes) {
  std::array<std::byte, 4096> buffer{};
  std::uint64_t offset = begin;
  while (offset < end) {
    const auto request = static_cast<std::size_t>(
        std::min<std::uint64_t>(buffer.size(), end - offset));
    auto read = PreadExact(descriptor, std::span(buffer).first(request),
                           IoPosition{offset, maximum_io_bytes});
    if (!read) {
      return std::unexpected(read.error());
    }
    if (std::any_of(buffer.begin(), buffer.begin() + static_cast<std::ptrdiff_t>(request),
                    [](std::byte value) { return value != std::byte{0}; })) {
      return false;
    }
    offset += request;
  }
  return true;
}

std::expected<bool, Error> HasValidFrameAfter(int descriptor, std::uint64_t begin,
                                              std::uint64_t end, std::uint16_t actor,
                                              std::uint64_t expected_lsn,
                                              const SegmentLogOptions& options) {
  if (end <= begin || end - begin < kFrameHeaderSize) {
    return false;
  }
  std::vector<std::byte> remaining(static_cast<std::size_t>(end - begin));
  auto read = PreadExact(descriptor, remaining,
                         IoPosition{begin, options.maximum_io_bytes});
  if (!read) {
    return std::unexpected(read.error());
  }
  for (std::size_t offset = 0; offset + kFrameHeaderSize <= remaining.size(); ++offset) {
    const auto candidate = std::span<const std::byte>(remaining).subspan(offset);
    auto length = ReadEncodedFrameLength(candidate, options.maximum_frame_bytes);
    if (!length || *length > candidate.size()) {
      continue;
    }
    auto frame = DecodeFrame(candidate.first(*length), options.maximum_frame_bytes);
    if (frame && frame->header.actor == actor && frame->header.lsn > expected_lsn) {
      return true;
    }
  }
  return false;
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

std::filesystem::path SegmentPath(const std::filesystem::path& directory, std::uint64_t id) {
  std::array<char, 25> name{};
  const int written = std::snprintf(name.data(), name.size(), "%020llu.seg",
                                    static_cast<unsigned long long>(id));
  if (written != 24) {
    return directory / "invalid.seg";
  }
  return directory / name.data();
}

std::expected<std::uint64_t, Error> ParseSegmentId(std::string_view name) {
  if (name.size() != 24 || name.substr(20) != ".seg") {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  std::uint64_t value = 0;
  for (std::size_t index = 0; index < 20; ++index) {
    const char digit = name[index];
    if (digit < '0' || digit > '9') {
      return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
    }
    const auto numeric = static_cast<std::uint64_t>(digit - '0');
    if (value > (std::numeric_limits<std::uint64_t>::max() - numeric) / 10U) {
      return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
    }
    value = value * 10U + numeric;
  }
  if (value == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  return value;
}

std::expected<std::vector<std::byte>, Error> ReadFile(const std::filesystem::path& path,
                                                      std::size_t maximum_io_bytes) {
  const FileDescriptor descriptor(::open(path.c_str(), O_RDONLY | O_CLOEXEC));
  if (!descriptor.valid()) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  struct stat info {};
  if (::fstat(descriptor.get(), &info) != 0 || info.st_size < 0) {
    return std::unexpected(Error{ErrorCode::kReadFailed, errno});
  }
  std::vector<std::byte> bytes(static_cast<std::size_t>(info.st_size));
  auto read = PreadExact(descriptor.get(), bytes, IoPosition{0, maximum_io_bytes});
  if (!read) {
    return std::unexpected(read.error());
  }
  return bytes;
}

std::uint32_t ManifestChecksum(std::span<const std::byte> bytes) {
  std::vector<std::byte> copy(bytes.begin(), bytes.end());
  if (copy.size() >= kManifestHeaderBytes) {
    PutU32(copy, 12, 0);
  }
  return crc32c::Crc32c(reinterpret_cast<const char*>(copy.data()), copy.size());
}

std::vector<std::byte> EncodeManifest(std::span<const ScannedSegment> segments) {
  std::vector<std::byte> bytes(kManifestHeaderBytes + segments.size() * kManifestEntryBytes);
  PutU32(bytes, 0, kManifestMagic);
  PutU32(bytes, 4, kManifestVersion);
  PutU32(bytes, 8, static_cast<std::uint32_t>(segments.size()));
  PutU32(bytes, 12, 0);
  for (std::size_t index = 0; index < segments.size(); ++index) {
    const std::size_t offset = kManifestHeaderBytes + index * kManifestEntryBytes;
    PutU64(bytes, offset, segments[index].id);
    PutU64(bytes, offset + 8, segments[index].first_lsn);
    PutU64(bytes, offset + 16, segments[index].last_lsn);
    PutU64(bytes, offset + 24, segments[index].physical_bytes);
  }
  PutU32(bytes, 12, ManifestChecksum(bytes));
  return bytes;
}

std::expected<void, Error> RecoverTail(int descriptor, std::uint64_t offset,
                                       std::uint64_t lsn) {
  if (::ftruncate(descriptor, static_cast<off_t>(offset)) != 0) {
    return std::unexpected(Error{ErrorCode::kWriteFailed, errno, lsn, offset});
  }
  if (::fdatasync(descriptor) != 0) {
    return std::unexpected(Error{ErrorCode::kSyncFailed, errno, lsn, offset});
  }
  return {};
}

std::expected<ScanResult, Error> ScanSegment(const std::filesystem::path& path,
                                             const SegmentLogOptions& options,
                                             ScanSettings settings) {
  const FileDescriptor descriptor(
      ::open(path.c_str(), settings.recover_tail ? O_RDWR | O_CLOEXEC
                                                 : O_RDONLY | O_CLOEXEC));
  if (!descriptor.valid()) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno, settings.expected_lsn});
  }
  struct stat info {};
  if (::fstat(descriptor.get(), &info) != 0 || info.st_size < 0) {
    return std::unexpected(Error{ErrorCode::kReadFailed, errno, settings.expected_lsn});
  }
  const std::uint64_t file_bytes = static_cast<std::uint64_t>(info.st_size);
  ScanResult result{.segment = ScannedSegment{.id = settings.id,
                                              .physical_bytes = file_bytes},
                    .next_lsn = settings.expected_lsn,
                    .recovered_lsn = 0,
                    .truncated = false,
                    .frames = {}};
  std::uint64_t offset = 0;

  const auto recover = [&](std::uint64_t torn_offset) -> std::expected<ScanResult, Error> {
    if (!settings.physical_tail || !settings.recover_tail) {
      return std::unexpected(
          Error{ErrorCode::kInteriorCorruption, 0, result.next_lsn, torn_offset});
    }
    auto truncate = RecoverTail(descriptor.get(), torn_offset, result.next_lsn);
    if (!truncate) {
      return std::unexpected(truncate.error());
    }
    result.segment.physical_bytes = torn_offset;
    result.recovered_lsn = result.next_lsn;
    result.truncated = true;
    return result;
  };

  while (offset < file_bytes) {
    std::array<std::byte, 4> length_bytes{};
    auto prefix_read = PreadUpTo(descriptor.get(), length_bytes,
                                 IoPosition{offset, options.maximum_io_bytes});
    if (!prefix_read) {
      return std::unexpected(prefix_read.error());
    }
    if (*prefix_read < length_bytes.size()) {
      auto zero = RangeIsZero(descriptor.get(), offset, file_bytes, options.maximum_io_bytes);
      if (!zero) {
        return std::unexpected(zero.error());
      }
      if (*zero) {
        break;
      }
      return recover(offset);
    }

    const std::uint32_t raw_length = GetU32(length_bytes, 0);
    if (raw_length == 0) {
      const std::uint64_t padding_end = std::min(
          file_bytes, AlignUp(offset + 1U, Alignment{options.direct_alignment}));
      auto zero = RangeIsZero(descriptor.get(), offset, padding_end, options.maximum_io_bytes);
      if (!zero) {
        return std::unexpected(zero.error());
      }
      if (!*zero) {
        return std::unexpected(
            Error{ErrorCode::kInteriorCorruption, 0, result.next_lsn, offset});
      }
      offset = padding_end;
      continue;
    }

    auto length = ReadEncodedFrameLength(length_bytes, options.maximum_frame_bytes);
    if (!length || offset + raw_length > file_bytes) {
      auto later = HasValidFrameAfter(descriptor.get(), offset + 1U, file_bytes,
                                      settings.actor,
                                      result.next_lsn, options);
      if (!later) {
        return std::unexpected(later.error());
      }
      if (*later) {
        return std::unexpected(
            Error{ErrorCode::kInteriorCorruption, 0, result.next_lsn, offset});
      }
      return recover(offset);
    }
    std::vector<std::byte> encoded(*length);
    auto read = PreadExact(descriptor.get(), encoded,
                           IoPosition{offset, options.maximum_io_bytes});
    if (!read) {
      return recover(offset);
    }
    auto frame = DecodeFrame(encoded, options.maximum_frame_bytes);
    if (!frame) {
      const std::uint64_t remainder_begin = offset + *length;
      auto trailing_zero = RangeIsZero(descriptor.get(), remainder_begin, file_bytes,
                                       options.maximum_io_bytes);
      if (!trailing_zero) {
        return std::unexpected(trailing_zero.error());
      }
      if (settings.physical_tail && settings.recover_tail && *trailing_zero) {
        return recover(offset);
      }
      Error error = frame.error();
      error.code = ErrorCode::kInteriorCorruption;
      error.lsn = result.next_lsn;
      error.offset = offset;
      return std::unexpected(error);
    }
    if (frame->header.lsn != result.next_lsn || frame->header.actor != settings.actor) {
      return std::unexpected(
          Error{ErrorCode::kSequenceViolation, 0, result.next_lsn, offset});
    }
    if (result.segment.first_lsn == 0) {
      result.segment.first_lsn = frame->header.lsn;
    }
    result.segment.last_lsn = frame->header.lsn;
    ++result.next_lsn;
    if (settings.collect_frames) {
      result.frames.push_back(std::move(*frame));
    }
    offset += *length;
    if (settings.stop_after_lsn != 0 &&
        frame->header.lsn == settings.stop_after_lsn) {
      result.segment.physical_bytes = offset;
      return result;
    }
  }
  return result;
}

std::expected<MappingPointer, Error> AllocateAligned(std::size_t bytes) {
  void* value = ::mmap(nullptr, bytes, PROT_READ | PROT_WRITE,
                       MAP_PRIVATE | MAP_ANONYMOUS, -1, 0);
  if (value == MAP_FAILED) {
    return std::unexpected(Error{ErrorCode::kWriteFailed, errno});
  }
  return MappingPointer(static_cast<std::byte*>(value), MunmapDeleter{bytes});
}

std::expected<void, Error> IoUringWrite(int descriptor, std::span<const std::byte> bytes,
                                        std::uint64_t offset) {
#if defined(SYS_io_uring_setup) && defined(SYS_io_uring_enter)
  io_uring_params parameters{};
  const FileDescriptor ring_descriptor(
      static_cast<int>(::syscall(SYS_io_uring_setup, 2U, &parameters)));
  if (!ring_descriptor.valid()) {
    return std::unexpected(Error{ErrorCode::kBackendUnavailable, errno, 0, offset});
  }

  const std::size_t sq_bytes = parameters.sq_off.array +
                               parameters.sq_entries * sizeof(std::uint32_t);
  const std::size_t cq_bytes = parameters.cq_off.cqes +
                               parameters.cq_entries * sizeof(io_uring_cqe);
  const bool single_mapping = (parameters.features & IORING_FEAT_SINGLE_MMAP) != 0;
  const std::size_t ring_bytes = single_mapping ? std::max(sq_bytes, cq_bytes) : sq_bytes;

  void* sq_raw = ::mmap(nullptr, ring_bytes, PROT_READ | PROT_WRITE,
                        MAP_SHARED | MAP_POPULATE, ring_descriptor.get(), IORING_OFF_SQ_RING);
  if (sq_raw == MAP_FAILED) {
    return std::unexpected(Error{ErrorCode::kBackendUnavailable, errno, 0, offset});
  }
  const MappingPointer sq_mapping(static_cast<std::byte*>(sq_raw),
                                  MunmapDeleter{ring_bytes});

  std::byte* cq_base = sq_mapping.get();
  MappingPointer cq_mapping(nullptr, MunmapDeleter{0});
  if (!single_mapping) {
    void* cq_raw = ::mmap(nullptr, cq_bytes, PROT_READ | PROT_WRITE,
                          MAP_SHARED | MAP_POPULATE, ring_descriptor.get(), IORING_OFF_CQ_RING);
    if (cq_raw == MAP_FAILED) {
      return std::unexpected(Error{ErrorCode::kBackendUnavailable, errno, 0, offset});
    }
    cq_mapping = MappingPointer(static_cast<std::byte*>(cq_raw), MunmapDeleter{cq_bytes});
    cq_base = cq_mapping.get();
  }

  const std::size_t sqe_bytes = parameters.sq_entries * sizeof(io_uring_sqe);
  void* sqes_raw = ::mmap(nullptr, sqe_bytes, PROT_READ | PROT_WRITE,
                          MAP_SHARED | MAP_POPULATE, ring_descriptor.get(), IORING_OFF_SQES);
  if (sqes_raw == MAP_FAILED) {
    return std::unexpected(Error{ErrorCode::kBackendUnavailable, errno, 0, offset});
  }
  const MappingPointer sqes_mapping(static_cast<std::byte*>(sqes_raw),
                                    MunmapDeleter{sqe_bytes});

  auto* sq_tail = reinterpret_cast<std::uint32_t*>(sq_mapping.get() + parameters.sq_off.tail);
  auto* sq_mask = reinterpret_cast<std::uint32_t*>(sq_mapping.get() + parameters.sq_off.ring_mask);
  auto* sq_array = reinterpret_cast<std::uint32_t*>(sq_mapping.get() + parameters.sq_off.array);
  auto* sqes = reinterpret_cast<io_uring_sqe*>(sqes_mapping.get());
  auto* cq_head = reinterpret_cast<std::uint32_t*>(cq_base + parameters.cq_off.head);
  auto* cq_tail = reinterpret_cast<std::uint32_t*>(cq_base + parameters.cq_off.tail);
  auto* cq_mask = reinterpret_cast<std::uint32_t*>(cq_base + parameters.cq_off.ring_mask);
  auto* cqes = reinterpret_cast<io_uring_cqe*>(cq_base + parameters.cq_off.cqes);

  const std::uint32_t tail = __atomic_load_n(sq_tail, __ATOMIC_RELAXED);
  const std::uint32_t index = tail & *sq_mask;
  io_uring_sqe& entry = sqes[index];
  std::memset(&entry, 0, sizeof(entry));
  entry.opcode = IORING_OP_WRITE;
  entry.fd = descriptor;
  entry.off = offset;
  entry.addr = reinterpret_cast<std::uint64_t>(bytes.data());
  entry.len = static_cast<std::uint32_t>(bytes.size());
  sq_array[index] = index;
  __atomic_store_n(sq_tail, tail + 1U, __ATOMIC_RELEASE);

  const long submitted = ::syscall(SYS_io_uring_enter, ring_descriptor.get(), 1U, 1U,
                                   IORING_ENTER_GETEVENTS, nullptr, 0U);
  if (submitted < 0) {
    return std::unexpected(Error{ErrorCode::kBackendUnavailable, errno, 0, offset});
  }
  const std::uint32_t completion_tail = __atomic_load_n(cq_tail, __ATOMIC_ACQUIRE);
  std::uint32_t completion_head = __atomic_load_n(cq_head, __ATOMIC_RELAXED);
  if (completion_head == completion_tail) {
    return std::unexpected(Error{ErrorCode::kWriteFailed, 0, 0, offset});
  }
  const io_uring_cqe& completion = cqes[completion_head & *cq_mask];
  const int result = completion.res;
  ++completion_head;
  __atomic_store_n(cq_head, completion_head, __ATOMIC_RELEASE);
  if (result < 0) {
    return std::unexpected(Error{ErrorCode::kWriteFailed, -result, 0, offset});
  }
  if (static_cast<std::size_t>(result) != bytes.size()) {
    return std::unexpected(Error{ErrorCode::kWriteFailed, 0, 0,
                                 offset + static_cast<std::uint64_t>(result)});
  }
  return {};
#else
  static_cast<void>(descriptor);
  static_cast<void>(bytes);
  return std::unexpected(Error{ErrorCode::kBackendUnavailable, 0, 0, offset});
#endif
}

std::expected<WriteResult, Error> WriteGroup(const std::filesystem::path& path,
                                             std::span<const std::byte> bytes,
                                             std::uint64_t offset,
                                             const SegmentLogOptions& options) {
  const bool request_direct = options.backend != WriteBackend::kPwrite;
  if (request_direct) {
    const FileDescriptor direct(
        ::open(path.c_str(), O_CREAT | O_RDWR | O_CLOEXEC | O_DIRECT, 0600));
    if (direct.valid()) {
      auto write = IoUringWrite(direct.get(), bytes, offset);
      if (write) {
        if (::fdatasync(direct.get()) != 0) {
          return std::unexpected(Error{ErrorCode::kSyncFailed, errno, 0, offset});
        }
        return WriteResult{.backend = WriteBackend::kIoUringDirect};
      }
      if (options.backend == WriteBackend::kIoUringDirect) {
        return std::unexpected(write.error());
      }
    } else if (options.backend == WriteBackend::kIoUringDirect) {
      return std::unexpected(Error{ErrorCode::kBackendUnavailable, errno, 0, offset});
    }
  }

  const FileDescriptor portable(
      ::open(path.c_str(), O_CREAT | O_RDWR | O_CLOEXEC, 0600));
  if (!portable.valid()) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno, 0, offset});
  }
  auto write = PwriteAll(portable.get(), bytes,
                         IoPosition{offset, options.maximum_io_bytes});
  if (!write) {
    return std::unexpected(write.error());
  }
  if (::fdatasync(portable.get()) != 0) {
    return std::unexpected(Error{ErrorCode::kSyncFailed, errno, 0, offset});
  }
  return WriteResult{.backend = WriteBackend::kPwrite};
}

bool ValidOptions(const SegmentLogOptions& options) {
  return options.maximum_frame_bytes >= kFrameHeaderSize &&
         options.maximum_io_bytes > 0 && options.direct_alignment >= 512 &&
         options.direct_alignment <= 4096 &&
         (options.direct_alignment & (options.direct_alignment - 1U)) == 0 &&
         options.segment_bytes >= options.direct_alignment &&
         options.segment_bytes % options.direct_alignment == 0 &&
         options.maximum_frame_bytes <= options.segment_bytes &&
         options.segment_bytes <= std::numeric_limits<std::uint32_t>::max();
}

}  // namespace

SegmentLog::SegmentLog(std::filesystem::path actor_directory, std::uint16_t actor,
                       SegmentLogOptions options, std::vector<SegmentEntry> segments,
                       RecoveryReport recovery_report)
    : actor_directory_(std::move(actor_directory)),
      log_directory_(actor_directory_ / "log"),
      actor_(actor),
      options_(options),
      segments_(std::move(segments)),
      recovery_report_(recovery_report),
      next_lsn_(recovery_report.next_lsn) {}

std::expected<SegmentLog, Error> SegmentLog::Open(
    const std::filesystem::path& actor_directory, std::uint16_t actor,
    SegmentLogOptions options) {
  if (actor == 0 || !ValidOptions(options)) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  const auto log_directory = actor_directory / "log";
  std::error_code directory_error;
  std::filesystem::create_directories(log_directory, directory_error);
  if (directory_error) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, directory_error.value()});
  }

  std::vector<std::pair<std::uint64_t, std::filesystem::path>> paths;
  std::filesystem::directory_iterator iterator(log_directory, directory_error);
  const std::filesystem::directory_iterator end;
  while (!directory_error && iterator != end) {
    if (iterator->is_regular_file(directory_error)) {
      if (directory_error) {
        break;
      }
      const std::string filename = iterator->path().filename().string();
      auto id = ParseSegmentId(filename);
      if (id) {
        paths.emplace_back(*id, iterator->path());
      } else if (filename.ends_with(".seg")) {
        return std::unexpected(id.error());
      }
    }
    iterator.increment(directory_error);
  }
  if (directory_error) {
    return std::unexpected(Error{ErrorCode::kReadFailed, directory_error.value()});
  }
  std::sort(paths.begin(), paths.end(), [](const auto& left, const auto& right) {
    return left.first < right.first;
  });
  for (std::size_t index = 1; index < paths.size(); ++index) {
    if (paths[index].first != paths[index - 1].first + 1U) {
      return std::unexpected(Error{ErrorCode::kSequenceViolation, 0});
    }
  }

  std::vector<SegmentEntry> segments;
  std::vector<ScannedSegment> canonical;
  RecoveryReport report{};
  std::uint64_t next_lsn = 1;
  for (std::size_t index = 0; index < paths.size(); ++index) {
    auto scan = ScanSegment(paths[index].second, options,
                            ScanSettings{.id = paths[index].first,
                                         .actor = actor,
                                         .expected_lsn = next_lsn,
                                         .physical_tail = index + 1U == paths.size(),
                                         .recover_tail = true,
                                         .collect_frames = false});
    if (!scan) {
      return std::unexpected(scan.error());
    }
    next_lsn = scan->next_lsn;
    if (scan->truncated) {
      report.truncated_torn_tail = true;
      report.recovered_tail_lsn = scan->recovered_lsn;
    }
    canonical.push_back(scan->segment);
    segments.push_back(SegmentEntry{.id = scan->segment.id,
                                    .first_lsn = scan->segment.first_lsn,
                                    .last_lsn = scan->segment.last_lsn,
                                    .physical_bytes = scan->segment.physical_bytes});
  }
  report.next_lsn = next_lsn;
  report.segment_count = static_cast<std::uint32_t>(segments.size());

  const auto manifest_path = log_directory / kManifestName;
  const auto canonical_manifest = EncodeManifest(canonical);
  bool manifest_matches = false;
  auto existing_manifest = ReadFile(manifest_path, options.maximum_io_bytes);
  if (existing_manifest) {
    manifest_matches = *existing_manifest == canonical_manifest;
  } else if (existing_manifest.error().system_error != ENOENT) {
    return std::unexpected(existing_manifest.error());
  }
  report.rebuilt_manifest = !manifest_matches;

  SegmentLog log(actor_directory, actor, options, std::move(segments), report);
  if (!manifest_matches) {
    auto write = log.WriteManifest();
    if (!write) {
      return std::unexpected(write.error());
    }
  }
  auto directory_sync = SyncDirectory(log_directory);
  if (!directory_sync) {
    return std::unexpected(directory_sync.error());
  }
  return log;
}

std::expected<void, Error> SegmentLog::WriteManifest() {
  std::vector<ScannedSegment> canonical;
  canonical.reserve(segments_.size());
  for (const SegmentEntry& segment : segments_) {
    canonical.push_back(ScannedSegment{.id = segment.id,
                                       .first_lsn = segment.first_lsn,
                                       .last_lsn = segment.last_lsn,
                                       .physical_bytes = segment.physical_bytes});
  }
  const auto bytes = EncodeManifest(canonical);
  const auto path = log_directory_ / kManifestName;
  const FileDescriptor descriptor(
      ::open(path.c_str(), O_CREAT | O_RDWR | O_CLOEXEC, 0600));
  if (!descriptor.valid()) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  auto write = PwriteAll(descriptor.get(), bytes,
                         IoPosition{0, options_.maximum_io_bytes});
  if (!write) {
    return std::unexpected(write.error());
  }
  if (::ftruncate(descriptor.get(), static_cast<off_t>(bytes.size())) != 0) {
    return std::unexpected(Error{ErrorCode::kWriteFailed, errno});
  }
  if (::fdatasync(descriptor.get()) != 0) {
    return std::unexpected(Error{ErrorCode::kSyncFailed, errno});
  }
  return {};
}

std::expected<CommitResult, Error> SegmentLog::AppendBatch(
    std::span<const AppendRequest> requests) {
  if (requests.empty()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  std::vector<std::vector<std::byte>> encoded;
  encoded.reserve(requests.size());
  std::uint64_t lsn = next_lsn_;
  for (const AppendRequest& request : requests) {
    const Frame frame{.header = FrameHeader{.lsn = lsn,
                                      .kind = request.kind,
                                      .wall_timestamp_ns = request.wall_timestamp_ns,
                                      .actor = actor_,
                                      .conversation = request.conversation},
                .sealed_payload = std::vector<std::byte>(request.sealed_payload.begin(),
                                                         request.sealed_payload.end())};
    auto bytes = EncodeFrame(frame, options_.maximum_frame_bytes);
    if (!bytes) {
      return std::unexpected(bytes.error());
    }
    encoded.push_back(std::move(*bytes));
    ++lsn;
  }

  const std::uint64_t first_lsn = next_lsn_;
  std::size_t request_index = 0;
  std::uint32_t touched_segments = 0;
  WriteBackend actual_backend = WriteBackend::kIoUringDirect;
  while (request_index < encoded.size()) {
    if (segments_.empty()) {
      segments_.push_back(SegmentEntry{.id = 1, .first_lsn = 0, .last_lsn = 0,
                                       .physical_bytes = 0});
    }
    SegmentEntry* segment = &segments_.back();
    std::uint64_t start =
        AlignUp(segment->physical_bytes, Alignment{options_.direct_alignment});
    if (start >= options_.segment_bytes) {
      segments_.push_back(SegmentEntry{.id = segment->id + 1U, .first_lsn = 0,
                                       .last_lsn = 0, .physical_bytes = 0});
      segment = &segments_.back();
      start = 0;
    }

    std::size_t group_end = request_index;
    std::uint64_t logical_bytes = 0;
    while (group_end < encoded.size()) {
      const std::uint64_t candidate = logical_bytes + encoded[group_end].size();
      const std::uint64_t physical =
          AlignUp(candidate, Alignment{options_.direct_alignment});
      if (start + physical > options_.segment_bytes) {
        break;
      }
      logical_bytes = candidate;
      ++group_end;
    }
    if (group_end == request_index) {
      if (segment->first_lsn == 0) {
        return std::unexpected(Error{ErrorCode::kSegmentFull, 0, next_lsn_ + request_index});
      }
      segments_.push_back(SegmentEntry{.id = segment->id + 1U, .first_lsn = 0,
                                       .last_lsn = 0, .physical_bytes = 0});
      continue;
    }

    const auto physical_bytes = static_cast<std::size_t>(
        AlignUp(logical_bytes, Alignment{options_.direct_alignment}));
    auto storage = AllocateAligned(physical_bytes);
    if (!storage) {
      return std::unexpected(storage.error());
    }
    std::memset(storage->get(), 0, physical_bytes);
    std::size_t cursor = 0;
    for (std::size_t index = request_index; index < group_end; ++index) {
      std::memcpy(storage->get() + cursor, encoded[index].data(), encoded[index].size());
      cursor += encoded[index].size();
    }
    const auto path = SegmentPath(log_directory_, segment->id);
    auto write = WriteGroup(path, std::span(storage->get(), physical_bytes), start, options_);
    if (!write) {
      return std::unexpected(write.error());
    }
    if (write->backend == WriteBackend::kPwrite) {
      actual_backend = WriteBackend::kPwrite;
    }
    if (segment->first_lsn == 0) {
      segment->first_lsn = next_lsn_ + request_index;
    }
    segment->last_lsn = next_lsn_ + group_end - 1U;
    segment->physical_bytes = start + physical_bytes;
    ++touched_segments;
    request_index = group_end;
  }

  next_lsn_ = lsn;
  recovery_report_.next_lsn = next_lsn_;
  recovery_report_.segment_count = static_cast<std::uint32_t>(segments_.size());
  auto manifest = WriteManifest();
  if (!manifest) {
    return std::unexpected(manifest.error());
  }
  auto directory_sync = SyncDirectory(log_directory_);
  if (!directory_sync) {
    return std::unexpected(directory_sync.error());
  }
  return CommitResult{.first_lsn = first_lsn,
                      .last_lsn = next_lsn_ - 1U,
                      .backend = actual_backend,
                      .segment_count = touched_segments};
}

std::expected<void, Error> SegmentLog::TruncateTo(std::uint64_t last_lsn) {
  if (next_lsn_ == 1U || last_lsn >= next_lsn_ - 1U) {
    return {};
  }
  while (!segments_.empty() && (segments_.back().first_lsn == 0 ||
                                segments_.back().first_lsn > last_lsn)) {
    std::error_code remove_error;
    std::filesystem::remove(SegmentPath(log_directory_, segments_.back().id),
                            remove_error);
    if (remove_error) {
      return std::unexpected(
          Error{ErrorCode::kWriteFailed, remove_error.value(), last_lsn});
    }
    segments_.pop_back();
  }
  if (last_lsn == 0) {
    if (!segments_.empty()) {
      return std::unexpected(Error{ErrorCode::kInvariantViolation, 0, last_lsn});
    }
    next_lsn_ = 1;
  } else {
    if (segments_.empty() || segments_.back().first_lsn > last_lsn ||
        segments_.back().last_lsn < last_lsn) {
      return std::unexpected(Error{ErrorCode::kInvariantViolation, 0, last_lsn});
    }
    if (segments_.back().last_lsn > last_lsn) {
      const auto path = SegmentPath(log_directory_, segments_.back().id);
      auto scan = ScanSegment(path, options_,
                              ScanSettings{.id = segments_.back().id,
                                           .actor = actor_,
                                           .expected_lsn = segments_.back().first_lsn,
                                           .physical_tail = false,
                                           .recover_tail = false,
                                           .collect_frames = false,
                                           .stop_after_lsn = last_lsn});
      if (!scan) {
        return std::unexpected(scan.error());
      }
      if (scan->segment.last_lsn != last_lsn) {
        return std::unexpected(
            Error{ErrorCode::kInvariantViolation, 0, last_lsn});
      }
      const FileDescriptor descriptor(
          ::open(path.c_str(), O_RDWR | O_CLOEXEC));
      if (!descriptor.valid()) {
        return std::unexpected(Error{ErrorCode::kOpenFailed, errno, last_lsn});
      }
      auto truncate = RecoverTail(descriptor.get(),
                                  scan->segment.physical_bytes, last_lsn);
      if (!truncate) {
        return std::unexpected(truncate.error());
      }
      segments_.back().last_lsn = last_lsn;
      segments_.back().physical_bytes = scan->segment.physical_bytes;
    }
    next_lsn_ = last_lsn + 1U;
  }
  recovery_report_.next_lsn = next_lsn_;
  recovery_report_.segment_count = static_cast<std::uint32_t>(segments_.size());
  auto manifest = WriteManifest();
  if (!manifest) {
    return std::unexpected(manifest.error());
  }
  return SyncDirectory(log_directory_);
}

std::expected<std::vector<Frame>, Error> SegmentLog::ReadAll() const {
  std::vector<Frame> frames;
  std::uint64_t expected_lsn = 1;
  for (std::size_t index = 0; index < segments_.size(); ++index) {
    const SegmentEntry& segment = segments_[index];
    auto scan = ScanSegment(
        SegmentPath(log_directory_, segment.id), options_,
        ScanSettings{.id = segment.id,
                     .actor = actor_,
                     .expected_lsn = expected_lsn,
                     .physical_tail = index + 1U == segments_.size(),
                     .recover_tail = false,
                     .collect_frames = true});
    if (!scan) {
      return std::unexpected(scan.error());
    }
    expected_lsn = scan->next_lsn;
    frames.insert(frames.end(), std::make_move_iterator(scan->frames.begin()),
                  std::make_move_iterator(scan->frames.end()));
  }
  return frames;
}

std::expected<std::vector<Frame>, Error> SegmentLog::ReadFrom(
    std::uint64_t first_lsn, std::size_t maximum_frames) const {
  if (first_lsn == 0 || maximum_frames == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  std::vector<Frame> frames;
  frames.reserve(std::min<std::size_t>(maximum_frames, 1024U));
  for (std::size_t index = 0;
       index < segments_.size() && frames.size() < maximum_frames; ++index) {
    const SegmentEntry& segment = segments_[index];
    if (segment.last_lsn < first_lsn) {
      continue;
    }
    auto scan = ScanSegment(
        SegmentPath(log_directory_, segment.id), options_,
        ScanSettings{.id = segment.id,
                     .actor = actor_,
                     .expected_lsn = segment.first_lsn,
                     .physical_tail = index + 1U == segments_.size(),
                     .recover_tail = false,
                     .collect_frames = true});
    if (!scan) {
      return std::unexpected(scan.error());
    }
    for (auto& frame : scan->frames) {
      if (frame.header.lsn >= first_lsn) {
        frames.push_back(std::move(frame));
        if (frames.size() == maximum_frames) {
          break;
        }
      }
    }
  }
  return frames;
}

}  // namespace neocortex::log
