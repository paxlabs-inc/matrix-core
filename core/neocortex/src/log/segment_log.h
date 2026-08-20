#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <filesystem>
#include <span>
#include <vector>

#include "core/error.h"
#include "log/frame.h"

namespace neocortex::log {

inline constexpr std::uint64_t kDefaultSegmentBytes = 128ULL * 1024ULL * 1024ULL;

enum class WriteBackend : std::uint8_t {
  kAuto,
  kPwrite,
  kIoUringDirect,
};

struct SegmentLogOptions final {
  std::uint64_t segment_bytes = kDefaultSegmentBytes;
  std::uint32_t maximum_frame_bytes = kDefaultMaximumFrameBytes;
  std::uint32_t direct_alignment = 4096;
  std::size_t maximum_io_bytes = static_cast<std::size_t>(-1);
  WriteBackend backend = WriteBackend::kAuto;
};

struct AppendRequest final {
  EventKind kind;
  std::int64_t wall_timestamp_ns;
  ConversationId conversation;
  std::span<const std::byte> sealed_payload;
};

struct CommitResult final {
  std::uint64_t first_lsn;
  std::uint64_t last_lsn;
  WriteBackend backend;
  std::uint32_t segment_count;
};

struct RecoveryReport final {
  std::uint64_t next_lsn = 1;
  std::uint64_t recovered_tail_lsn = 0;
  std::uint32_t segment_count = 0;
  bool truncated_torn_tail = false;
  bool rebuilt_manifest = false;
};

class SegmentLog final {
 public:
  static std::expected<SegmentLog, Error> Open(const std::filesystem::path& actor_directory,
                                               std::uint16_t actor,
                                               SegmentLogOptions options = {});

  SegmentLog(SegmentLog&&) noexcept = default;
  SegmentLog& operator=(SegmentLog&&) noexcept = default;
  SegmentLog(const SegmentLog&) = delete;
  SegmentLog& operator=(const SegmentLog&) = delete;

  [[nodiscard]] std::expected<CommitResult, Error> AppendBatch(
      std::span<const AppendRequest> requests);
  [[nodiscard]] std::expected<void, Error> TruncateTo(std::uint64_t last_lsn);

  [[nodiscard]] std::expected<std::vector<Frame>, Error> ReadAll() const;
  [[nodiscard]] std::expected<std::vector<Frame>, Error> ReadFrom(
      std::uint64_t first_lsn, std::size_t maximum_frames) const;

  [[nodiscard]] const RecoveryReport& recovery_report() const { return recovery_report_; }
  [[nodiscard]] std::uint64_t next_lsn() const { return next_lsn_; }

 private:
  struct SegmentEntry final {
    std::uint64_t id;
    std::uint64_t first_lsn;
    std::uint64_t last_lsn;
    std::uint64_t physical_bytes;
  };

  SegmentLog(std::filesystem::path actor_directory, std::uint16_t actor,
             SegmentLogOptions options, std::vector<SegmentEntry> segments,
             RecoveryReport recovery_report);

  [[nodiscard]] std::expected<void, Error> WriteManifest();

  std::filesystem::path actor_directory_;
  std::filesystem::path log_directory_;
  std::uint16_t actor_;
  SegmentLogOptions options_;
  std::vector<SegmentEntry> segments_;
  RecoveryReport recovery_report_;
  std::uint64_t next_lsn_;
};

}  // namespace neocortex::log
