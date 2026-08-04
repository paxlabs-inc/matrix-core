#include <array>
#include <algorithm>
#include <atomic>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <filesystem>
#include <span>
#include <string>
#include <thread>
#include <vector>

#include <signal.h>
#include <sys/wait.h>
#include <unistd.h>

#include "log/frame.h"
#include "proj/conversation_heads.h"
#include "proj/store.h"

namespace {

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

std::vector<neocortex::log::Frame> Frames(std::size_t count) {
  std::vector<neocortex::log::Frame> frames;
  frames.reserve(count);
  for (std::size_t index = 0; index < count; ++index) {
    neocortex::log::ConversationId conversation{};
    conversation.bytes[0] = static_cast<std::byte>(index % 13U);
    conversation.bytes[1] = std::byte{0x4d};
    std::vector<std::byte> payload((index % 29U) + 1U);
    for (std::size_t byte = 0; byte < payload.size(); ++byte) {
      payload[byte] = static_cast<std::byte>((index * 17U + byte) & 0xffU);
    }
    frames.push_back(neocortex::log::Frame{
        .header = neocortex::log::FrameHeader{
            .lsn = static_cast<std::uint64_t>(index) + 1U,
            .kind = static_cast<neocortex::log::EventKind>((index % 21U) + 1U),
            .wall_timestamp_ns = 5'000'000 + static_cast<std::int64_t>(index),
            .actor = 41,
            .conversation = conversation,
        },
        .sealed_payload = std::move(payload),
    });
  }
  return frames;
}

std::expected<std::vector<std::byte>, neocortex::Error> Dump(
    neocortex::proj::ProjectionStore& store,
    neocortex::proj::ProjectionId projection) {
  auto snapshot = store.BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  return snapshot->CanonicalDump(projection);
}

int TestSnapshotsAndRebuild(const std::filesystem::path& path,
                            std::span<const neocortex::log::Frame> frames,
                            std::vector<std::byte>& expected_dump) {
  auto store = neocortex::proj::ProjectionStore::Open(path);
  if (!store) {
    return Fail("projection store open failed");
  }
  for (std::size_t index = 0; index < neocortex::proj::kProjectionCount; ++index) {
    auto snapshot = store->BeginSnapshot();
    if (!snapshot) {
      return Fail("initial snapshot failed");
    }
    auto checkpoint = snapshot->Checkpoint(
        static_cast<neocortex::proj::ProjectionId>(index));
    if (!checkpoint || *checkpoint != 0) {
      return Fail("projection was not initialized at checkpoint zero");
    }
  }

  auto first = neocortex::proj::RebuildConversationHeads(
      *store, frames, true, 32);
  if (!first || first->complete || first->applied_lsn != 32) {
    return Fail("bounded rebuild prefix failed");
  }

  std::atomic<bool> reader_ready = false;
  std::atomic<bool> writer_finished = false;
  std::atomic<bool> reader_consistent = false;
  std::thread reader([&]() {
    auto snapshot = store->BeginSnapshot();
    if (!snapshot) {
      reader_ready.store(true, std::memory_order_release);
      return;
    }
    auto before = snapshot->CanonicalDump(
        neocortex::proj::ProjectionId::kConversationHeads);
    reader_ready.store(true, std::memory_order_release);
    while (!writer_finished.load(std::memory_order_acquire)) {
      std::this_thread::yield();
    }
    auto after = snapshot->CanonicalDump(
        neocortex::proj::ProjectionId::kConversationHeads);
    reader_consistent.store(before && after && *before == *after,
                            std::memory_order_release);
  });
  while (!reader_ready.load(std::memory_order_acquire)) {
    std::this_thread::yield();
  }
  if (!neocortex::proj::ApplyConversationHead(*store, frames[32])) {
    writer_finished.store(true, std::memory_order_release);
    reader.join();
    return Fail("writer did not advance beside a pinned reader");
  }
  writer_finished.store(true, std::memory_order_release);
  reader.join();
  if (!reader_consistent.load(std::memory_order_acquire)) {
    return Fail("epoch-pinned reader changed beneath the snapshot");
  }

  auto resumed = neocortex::proj::RebuildConversationHeads(*store, frames, false);
  if (!resumed || !resumed->complete || resumed->applied_lsn != frames.size()) {
    return Fail("projection resume failed");
  }
  auto original = Dump(*store, neocortex::proj::ProjectionId::kConversationHeads);
  if (!original) {
    return Fail("original projection dump failed");
  }
  expected_dump = *original;

  const std::array<std::byte, 2> entity_key = {std::byte{0x61}, std::byte{0x62}};
  const std::array<std::byte, 3> entity_value = {
      std::byte{0x01}, std::byte{0x02}, std::byte{0x03}};
  const neocortex::proj::Mutation entity_mutation{
      .kind = neocortex::proj::MutationKind::kPut,
      .key = entity_key,
      .value = entity_value,
  };
  if (!store->Apply(neocortex::proj::ProjectionId::kEntityIndex, 1,
                    std::span(&entity_mutation, 1))) {
    return Fail("independent projection write failed");
  }
  auto entity_before = Dump(*store, neocortex::proj::ProjectionId::kEntityIndex);
  if (!entity_before) {
    return Fail("independent projection dump failed");
  }
  auto rebuilt = neocortex::proj::RebuildConversationHeads(*store, frames, true);
  auto entity_after = Dump(*store, neocortex::proj::ProjectionId::kEntityIndex);
  auto rebuilt_dump = Dump(*store,
                           neocortex::proj::ProjectionId::kConversationHeads);
  if (!rebuilt || !rebuilt->complete || !entity_after || !rebuilt_dump ||
      *entity_before != *entity_after || *rebuilt_dump != expected_dump) {
    return Fail("selective byte-identical rebuild failed");
  }

  auto skipped = store->Apply(neocortex::proj::ProjectionId::kEntityIndex, 3,
                              std::span<const neocortex::proj::Mutation>{});
  if (skipped ||
      skipped.error().code != neocortex::ErrorCode::kProjectionCheckpoint) {
    return Fail("checkpoint discontinuity was accepted");
  }
  return 0;
}

int TestProcessDeathResume(const std::filesystem::path& path,
                           std::span<const neocortex::log::Frame> frames,
                           std::span<const std::byte> expected_dump) {
  std::array<int, 2> ready_pipe{};
  if (::pipe(ready_pipe.data()) != 0) {
    return Fail("pipe failed");
  }
  const pid_t child = ::fork();
  if (child < 0) {
    return Fail("fork failed");
  }
  if (child == 0) {
    ::close(ready_pipe[0]);
    auto store = neocortex::proj::ProjectionStore::Open(path);
    auto progress = store ? neocortex::proj::RebuildConversationHeads(
                                *store, frames, true, 37)
                          : std::expected<neocortex::proj::RebuildProgress,
                                          neocortex::Error>(
                                std::unexpected(neocortex::Error{
                                    neocortex::ErrorCode::kOpenFailed, 0}));
    if (!progress || progress->applied_lsn != 37) {
      ::_exit(71);
    }
    const std::byte ready{0x01};
    if (::write(ready_pipe[1], &ready, 1) != 1) {
      ::_exit(72);
    }
    for (;;) {
      ::pause();
    }
  }
  ::close(ready_pipe[1]);
  std::byte ready{};
  if (::read(ready_pipe[0], &ready, 1) != 1) {
    return Fail("rebuild child did not reach the durable checkpoint");
  }
  ::close(ready_pipe[0]);
  if (::kill(child, SIGKILL) != 0) {
    return Fail("SIGKILL failed");
  }
  int child_status = 0;
  if (::waitpid(child, &child_status, 0) != child || !WIFSIGNALED(child_status) ||
      WTERMSIG(child_status) != SIGKILL) {
    return Fail("rebuild child did not die at the requested boundary");
  }

  auto store = neocortex::proj::ProjectionStore::Open(path);
  if (!store) {
    return Fail("post-kill store reopen failed");
  }
  {
    auto snapshot = store->BeginSnapshot();
    if (!snapshot) {
      return Fail("post-kill snapshot failed");
    }
    auto checkpoint = snapshot->Checkpoint(
        neocortex::proj::ProjectionId::kConversationHeads);
    if (!checkpoint || *checkpoint != 37) {
      return Fail("post-kill checkpoint was not durable");
    }
  }
  auto resumed = neocortex::proj::RebuildConversationHeads(*store, frames, false);
  auto dump = Dump(*store, neocortex::proj::ProjectionId::kConversationHeads);
  if (!resumed || !resumed->complete || !dump ||
      !std::ranges::equal(*dump, expected_dump)) {
    return Fail("crash-mid-rebuild did not resume byte-identically");
  }
  return 0;
}

}  // namespace

int main() {
  std::array<char, 64> template_path{};
  const std::string seed_path = "/tmp/neocortex-projection-XXXXXX";
  std::copy(seed_path.begin(), seed_path.end(), template_path.begin());
  char* directory = ::mkdtemp(template_path.data());
  if (directory == nullptr) {
    return Fail("mkdtemp failed");
  }
  const std::filesystem::path path(directory);
  const auto frames = Frames(128);
  std::vector<std::byte> expected_dump;
  if (TestSnapshotsAndRebuild(path, frames, expected_dump) != 0 ||
      TestProcessDeathResume(path, frames, expected_dump) != 0) {
    return 1;
  }
  std::error_code cleanup_error;
  std::filesystem::remove_all(path, cleanup_error);
  if (cleanup_error) {
    return Fail("temporary directory cleanup failed");
  }
  return 0;
}
