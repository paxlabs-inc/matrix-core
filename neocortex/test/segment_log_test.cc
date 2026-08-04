#include <algorithm>
#include <array>
#include <cerrno>
#include <cstddef>
#include <cstdio>
#include <filesystem>
#include <fcntl.h>
#include <span>
#include <string_view>
#include <sys/stat.h>
#include <sys/wait.h>
#include <unistd.h>
#include <vector>

#include "log/frame.h"
#include "log/segment_log.h"

namespace {

class TempDirectory final {
 public:
  static std::expected<TempDirectory, int> Create() {
    std::array<char, 40> pattern{};
    constexpr std::string_view value = "/tmp/neocortex-segment-XXXXXX";
    std::copy(value.begin(), value.end(), pattern.begin());
    char* created = ::mkdtemp(pattern.data());
    if (created == nullptr) {
      return std::unexpected(errno);
    }
    return TempDirectory(std::filesystem::path(created));
  }

  ~TempDirectory() {
    std::error_code ignored;
    std::filesystem::remove_all(path_, ignored);
  }
  TempDirectory(TempDirectory&&) noexcept = default;
  TempDirectory& operator=(TempDirectory&&) noexcept = default;
  TempDirectory(const TempDirectory&) = delete;
  TempDirectory& operator=(const TempDirectory&) = delete;

  [[nodiscard]] const std::filesystem::path& path() const { return path_; }

 private:
  explicit TempDirectory(std::filesystem::path path) : path_(std::move(path)) {}
  std::filesystem::path path_;
};

std::span<const std::byte> Bytes(std::string_view value) {
  return std::as_bytes(std::span(value));
}

neocortex::log::ConversationId Conversation(std::byte first) {
  neocortex::log::ConversationId value{};
  value.bytes[0] = first;
  return value;
}

neocortex::log::SegmentLogOptions TestOptions() {
  return neocortex::log::SegmentLogOptions{
      .segment_bytes = 4096,
      .maximum_frame_bytes = 1024,
      .direct_alignment = 512,
      .maximum_io_bytes = 3,
      .backend = neocortex::log::WriteBackend::kPwrite,
  };
}

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

std::filesystem::path FirstSegment(const std::filesystem::path& actor) {
  return actor / "log" / "00000000000000000001.seg";
}

int RoundTripAndManifest() {
  auto temporary = TempDirectory::Create();
  if (!temporary) {
    return Fail("temp directory failed");
  }
  const auto actor = temporary->path() / "actor-7";
  auto log = neocortex::log::SegmentLog::Open(actor, 7, TestOptions());
  if (!log) {
    return Fail("open failed");
  }
  const std::array requests{
      neocortex::log::AppendRequest{neocortex::log::EventKind::kUserMsg, 10,
                                    Conversation(std::byte{1}), Bytes("one")},
      neocortex::log::AppendRequest{neocortex::log::EventKind::kToolCall, 11,
                                    Conversation(std::byte{1}), Bytes("two")},
      neocortex::log::AppendRequest{neocortex::log::EventKind::kToolResult, 12,
                                    Conversation(std::byte{1}), Bytes("three")},
  };
  auto commit = log->AppendBatch(requests);
  if (!commit || commit->first_lsn != 1 || commit->last_lsn != 3 ||
      commit->backend != neocortex::log::WriteBackend::kPwrite) {
    return Fail("group commit failed");
  }
  auto frames = log->ReadAll();
  if (!frames || frames->size() != requests.size()) {
    return Fail("read all failed");
  }
  for (std::size_t index = 0; index < frames->size(); ++index) {
    if ((*frames)[index].header.lsn != index + 1U ||
        (*frames)[index].header.actor != 7 ||
        (*frames)[index].header.kind != requests[index].kind ||
        !std::equal((*frames)[index].sealed_payload.begin(),
                    (*frames)[index].sealed_payload.end(),
                    requests[index].sealed_payload.begin(), requests[index].sealed_payload.end())) {
      return Fail("frame mismatch");
    }
  }
  auto range = log->ReadFrom(2, 1);
  auto tail = log->ReadFrom(4, 1);
  if (!range || range->size() != 1 || range->front().header.lsn != 2 ||
      !tail || !tail->empty() || log->ReadFrom(0, 1) ||
      log->ReadFrom(1, 0)) {
    return Fail("bounded range read failed");
  }
  if (!std::filesystem::is_regular_file(actor / "log" / "MANIFEST")) {
    return Fail("manifest missing");
  }
  auto reopened = neocortex::log::SegmentLog::Open(actor, 7, TestOptions());
  if (!reopened || reopened->next_lsn() != 4 || reopened->recovery_report().rebuilt_manifest) {
    return Fail("clean reopen failed");
  }
  const auto manifest_path = actor / "log" / "MANIFEST";
  const int manifest = ::open(manifest_path.c_str(), O_RDWR | O_CLOEXEC);
  std::byte corrupt_magic{0xff};
  if (manifest < 0 || ::pwrite(manifest, &corrupt_magic, 1, 0) != 1 ||
      ::fdatasync(manifest) != 0 || ::close(manifest) != 0) {
    if (manifest >= 0) {
      static_cast<void>(::close(manifest));
    }
    return Fail("manifest corruption failed");
  }
  auto rebuilt = neocortex::log::SegmentLog::Open(actor, 7, TestOptions());
  if (!rebuilt || !rebuilt->recovery_report().rebuilt_manifest || rebuilt->next_lsn() != 4) {
    return Fail("manifest was not rebuilt from the log");
  }
  return 0;
}

int TornTailRecovery() {
  auto temporary = TempDirectory::Create();
  if (!temporary) {
    return Fail("temp directory failed");
  }
  const auto actor = temporary->path() / "actor-7";
  auto log = neocortex::log::SegmentLog::Open(actor, 7, TestOptions());
  if (!log) {
    return Fail("open failed");
  }
  const std::array requests{
      neocortex::log::AppendRequest{neocortex::log::EventKind::kUserMsg, 10,
                                    Conversation(std::byte{2}), Bytes("durable")},
      neocortex::log::AppendRequest{neocortex::log::EventKind::kDeliveredMsg, 11,
                                    Conversation(std::byte{2}), Bytes("torn-tail")},
  };
  auto commit = log->AppendBatch(requests);
  if (!commit) {
    return Fail("append failed");
  }
  const neocortex::log::Frame first{.header = neocortex::log::FrameHeader{
                                  .lsn = 1,
                                  .kind = requests[0].kind,
                                  .wall_timestamp_ns = requests[0].wall_timestamp_ns,
                                  .actor = 7,
                                  .conversation = requests[0].conversation},
                              .sealed_payload = std::vector<std::byte>(
                                  requests[0].sealed_payload.begin(),
                                  requests[0].sealed_payload.end())};
  auto first_encoded = neocortex::log::EncodeFrame(first, TestOptions().maximum_frame_bytes);
  if (!first_encoded) {
    return Fail("encode failed");
  }
  const auto segment = FirstSegment(actor);
  const int descriptor = ::open(segment.c_str(), O_WRONLY | O_CLOEXEC);
  if (descriptor < 0 ||
      ::ftruncate(descriptor, static_cast<off_t>(first_encoded->size() + 7U)) != 0 ||
      ::fdatasync(descriptor) != 0 || ::close(descriptor) != 0) {
    if (descriptor >= 0) {
      static_cast<void>(::close(descriptor));
    }
    return Fail("tail tear failed");
  }
  auto recovered = neocortex::log::SegmentLog::Open(actor, 7, TestOptions());
  if (!recovered || !recovered->recovery_report().truncated_torn_tail ||
      recovered->recovery_report().recovered_tail_lsn != 2 || recovered->next_lsn() != 2) {
    return Fail("tail recovery failed");
  }
  auto frames = recovered->ReadAll();
  if (!frames || frames->size() != 1 || frames->front().header.lsn != 1) {
    return Fail("tail recovery retained wrong frames");
  }
  return 0;
}

int InteriorCorruptionRefusal() {
  auto temporary = TempDirectory::Create();
  if (!temporary) {
    return Fail("temp directory failed");
  }
  const auto actor = temporary->path() / "actor-7";
  auto log = neocortex::log::SegmentLog::Open(actor, 7, TestOptions());
  if (!log) {
    return Fail("open failed");
  }
  const std::array requests{
      neocortex::log::AppendRequest{neocortex::log::EventKind::kUserMsg, 20,
                                    Conversation(std::byte{3}), Bytes("first")},
      neocortex::log::AppendRequest{neocortex::log::EventKind::kDeliveredMsg, 21,
                                    Conversation(std::byte{3}), Bytes("second")},
  };
  if (!log->AppendBatch(requests)) {
    return Fail("append failed");
  }
  const auto segment = FirstSegment(actor);
  const int descriptor = ::open(segment.c_str(), O_RDWR | O_CLOEXEC);
  std::byte changed{};
  const auto payload_offset = static_cast<off_t>(neocortex::log::kFrameHeaderSize);
  if (descriptor < 0 || ::pread(descriptor, &changed, 1, payload_offset) != 1) {
    if (descriptor >= 0) {
      static_cast<void>(::close(descriptor));
    }
    return Fail("corruption read failed");
  }
  changed ^= std::byte{0x40};
  if (::pwrite(descriptor, &changed, 1, payload_offset) != 1 ||
      ::fdatasync(descriptor) != 0 || ::close(descriptor) != 0) {
    return Fail("corruption write failed");
  }
  auto refused = neocortex::log::SegmentLog::Open(actor, 7, TestOptions());
  if (refused || refused.error().code != neocortex::ErrorCode::kInteriorCorruption ||
      refused.error().lsn != 1) {
    return Fail("interior corruption was not refused at lsn 1");
  }
  return 0;
}

int RealKillAfterCommit() {
  auto temporary = TempDirectory::Create();
  if (!temporary) {
    return Fail("temp directory failed");
  }
  const auto actor = temporary->path() / "actor-7";
  const pid_t child = ::fork();
  if (child < 0) {
    return Fail("fork failed");
  }
  if (child == 0) {
    auto log = neocortex::log::SegmentLog::Open(actor, 7, TestOptions());
    const std::array requests{neocortex::log::AppendRequest{
        neocortex::log::EventKind::kToolCall, 30, Conversation(std::byte{4}), Bytes("dispatch")}};
    if (!log || !log->AppendBatch(requests)) {
      _exit(91);
    }
    static_cast<void>(::kill(::getpid(), SIGKILL));
    _exit(92);
  }
  int status = 0;
  if (::waitpid(child, &status, 0) != child || !WIFSIGNALED(status) ||
      WTERMSIG(status) != SIGKILL) {
    return Fail("child was not killed");
  }
  auto recovered = neocortex::log::SegmentLog::Open(actor, 7, TestOptions());
  if (!recovered) {
    return Fail("reopen after kill failed");
  }
  auto frames = recovered->ReadAll();
  if (!frames || frames->size() != 1 ||
      frames->front().header.kind != neocortex::log::EventKind::kToolCall) {
    return Fail("durable frame lost after kill");
  }
  return 0;
}

std::uint64_t NextRandom(std::uint64_t& state) {
  state ^= state << 13U;
  state ^= state >> 7U;
  state ^= state << 17U;
  return state;
}

int AppendReadProperty() {
  auto temporary = TempDirectory::Create();
  if (!temporary) {
    return Fail("temp directory failed");
  }
  const auto actor = temporary->path() / "actor-7";
  auto options = TestOptions();
  options.segment_bytes = 8192;
  options.maximum_frame_bytes = 512;
  options.maximum_io_bytes = 7;
  auto log = neocortex::log::SegmentLog::Open(actor, 7, options);
  if (!log) {
    return Fail("property open failed");
  }

  std::uint64_t random = 0x9e3779b97f4a7c15ULL;
  std::vector<neocortex::log::Frame> expected;
  for (std::size_t batch_index = 0; batch_index < 96; ++batch_index) {
    const std::size_t batch_size = 1U + static_cast<std::size_t>(NextRandom(random) % 7U);
    std::vector<std::vector<std::byte>> payloads;
    payloads.reserve(batch_size);
    for (std::size_t index = 0; index < batch_size; ++index) {
      const std::size_t payload_size = static_cast<std::size_t>(NextRandom(random) % 321U);
      std::vector<std::byte> payload(payload_size);
      for (std::byte& value : payload) {
        value = static_cast<std::byte>(NextRandom(random) & 0xffU);
      }
      payloads.push_back(std::move(payload));
    }

    std::vector<neocortex::log::AppendRequest> requests;
    requests.reserve(batch_size);
    for (std::size_t index = 0; index < batch_size; ++index) {
      const auto kind = static_cast<neocortex::log::EventKind>(1U + NextRandom(random) % 21U);
      const auto timestamp = static_cast<std::int64_t>(NextRandom(random) & 0x7fffffffffffffffULL);
      const auto conversation = Conversation(static_cast<std::byte>(NextRandom(random) & 0xffU));
      requests.push_back(neocortex::log::AppendRequest{kind, timestamp, conversation,
                                                       payloads[index]});
    }
    auto commit = log->AppendBatch(requests);
    if (!commit || commit->first_lsn != expected.size() + 1U ||
        commit->last_lsn != expected.size() + batch_size) {
      return Fail("property append failed");
    }
    for (std::size_t index = 0; index < requests.size(); ++index) {
      expected.push_back(neocortex::log::Frame{
          .header = neocortex::log::FrameHeader{
              .lsn = commit->first_lsn + index,
              .kind = requests[index].kind,
              .wall_timestamp_ns = requests[index].wall_timestamp_ns,
              .actor = 7,
              .conversation = requests[index].conversation,
          },
          .sealed_payload = payloads[index],
      });
    }
  }

  auto reopened = neocortex::log::SegmentLog::Open(actor, 7, options);
  if (!reopened || reopened->recovery_report().segment_count < 2 ||
      reopened->next_lsn() != expected.size() + 1U) {
    return Fail("property reopen failed");
  }
  auto actual = reopened->ReadAll();
  if (!actual || *actual != expected) {
    return Fail("append/read property failed");
  }
  return 0;
}

int AutomaticBackend() {
  auto temporary = TempDirectory::Create();
  if (!temporary) {
    return Fail("temp directory failed");
  }
  const auto actor = temporary->path() / "actor-7";
  auto options = TestOptions();
  options.segment_bytes = 8192;
  options.direct_alignment = 4096;
  options.maximum_io_bytes = static_cast<std::size_t>(-1);
  options.backend = neocortex::log::WriteBackend::kAuto;
  auto log = neocortex::log::SegmentLog::Open(actor, 7, options);
  const std::array requests{neocortex::log::AppendRequest{
      neocortex::log::EventKind::kAssertion, 40, Conversation(std::byte{5}), Bytes("auto")}};
  auto commit = log ? log->AppendBatch(requests)
                    : std::expected<neocortex::log::CommitResult, neocortex::Error>(
                          std::unexpected(log.error()));
  if (!commit || (commit->backend != neocortex::log::WriteBackend::kPwrite &&
                  commit->backend != neocortex::log::WriteBackend::kIoUringDirect)) {
    return Fail("automatic backend failed");
  }
  auto frames = log->ReadAll();
  if (!frames || frames->size() != 1) {
    return Fail("automatic backend read failed");
  }
  return 0;
}

}  // namespace

int main() {
  if (const int result = RoundTripAndManifest(); result != 0) {
    return result;
  }
  if (const int result = TornTailRecovery(); result != 0) {
    return result;
  }
  if (const int result = InteriorCorruptionRefusal(); result != 0) {
    return result;
  }
  if (const int result = RealKillAfterCommit(); result != 0) {
    return result;
  }
  if (const int result = AppendReadProperty(); result != 0) {
    return result;
  }
  return AutomaticBackend();
}
