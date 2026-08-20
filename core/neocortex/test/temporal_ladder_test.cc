#include <algorithm>
#include <array>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <filesystem>
#include <limits>
#include <span>
#include <string>
#include <vector>

#include <unistd.h>

#include "event_fixture.h"
#include "proj/ladder/temporal_ladder.h"

namespace {

constexpr std::int64_t kMinute = 60'000'000'000LL;
constexpr std::int64_t kHour = 60LL * kMinute;
constexpr std::int64_t kDay = 24LL * kHour;
constexpr std::int64_t kWeek = 7LL * kDay;

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

neocortex::log::Frame Message(std::uint64_t lsn, std::int64_t timestamp_ns) {
  const std::array<std::byte, 1> content = {std::byte{0x41}};
  return neocortex::log::Frame{
      .header = neocortex::log::FrameHeader{
          .lsn = lsn,
          .kind = neocortex::log::EventKind::kUserMsg,
          .wall_timestamp_ns = timestamp_ns,
          .actor = 91,
          .conversation = {},
      },
      .sealed_payload = neocortex::test::BuildEvent(
          neocortex::log::EventKind::kUserMsg, lsn, content),
  };
}

std::expected<std::vector<std::byte>, neocortex::Error> Dump(
    neocortex::proj::ProjectionStore& store) {
  auto snapshot = store.BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  return snapshot->CanonicalDump(
      neocortex::proj::ProjectionId::kTemporalLadder);
}

int TestLadder(const std::filesystem::path& path) {
  auto store = neocortex::proj::ProjectionStore::Open(path);
  if (!store) {
    return Fail("temporal ladder store open failed");
  }
  std::vector<neocortex::log::Frame> frames;
  frames.reserve(256);
  for (std::uint64_t index = 0; index < 256; ++index) {
    const auto signed_index = static_cast<std::int64_t>(index);
    const std::int64_t timestamp =
        -2LL * kDay + signed_index * 37LL * kMinute;
    frames.push_back(Message(index + 1U, timestamp));
  }

  auto partial = neocortex::proj::TemporalLadder::Rebuild(
      *store, frames, false, 73);
  auto resumed = neocortex::proj::TemporalLadder::Rebuild(*store, frames);
  if (!partial || partial->complete || partial->applied_lsn != 73 ||
      !resumed || !resumed->complete || resumed->applied_lsn != 256) {
    return Fail("temporal ladder bounded rebuild and resume failed");
  }

  auto snapshot = store->BeginSnapshot();
  if (!snapshot) {
    return Fail("temporal ladder snapshot failed");
  }
  auto weeks = neocortex::proj::TemporalLadder::ListWindows(
      *snapshot, neocortex::proj::TemporalLevel::kWeek, -kWeek, 2LL * kWeek);
  auto days = neocortex::proj::TemporalLadder::ListWindows(
      *snapshot, neocortex::proj::TemporalLevel::kDay, -3LL * kDay,
      6LL * kDay);
  auto hours = neocortex::proj::TemporalLadder::ListWindows(
      *snapshot, neocortex::proj::TemporalLevel::kHour, -2LL * kDay,
      -kDay);
  auto minutes = neocortex::proj::TemporalLadder::ListWindows(
      *snapshot, neocortex::proj::TemporalLevel::kMinute, -2LL * kDay,
      -2LL * kDay + 4LL * kHour);
  if (!weeks || weeks->empty() || !days || days->empty() || !hours ||
      hours->empty() || !minutes || minutes->empty()) {
    return Fail("temporal ladder omitted a populated resolution");
  }
  if (!std::ranges::is_sorted(*days, {},
                              &neocortex::proj::TemporalWindow::start_ns)) {
    return Fail("temporal ladder windows were not ordered");
  }
  auto child_days = neocortex::proj::TemporalLadder::OpenWindow(
      *snapshot, weeks->front());
  if (!child_days || child_days->empty() ||
      !std::ranges::all_of(*child_days, [&](const auto& child) {
        return child.level == neocortex::proj::TemporalLevel::kDay &&
               child.start_ns >= weeks->front().start_ns &&
               child.end_ns <= weeks->front().end_ns;
      })) {
    return Fail("week to day recursive descent failed");
  }
  auto child_hours = neocortex::proj::TemporalLadder::OpenWindow(
      *snapshot, child_days->front());
  if (!child_hours || child_hours->empty()) {
    return Fail("day to hour recursive descent failed");
  }
  auto child_minutes = neocortex::proj::TemporalLadder::OpenWindow(
      *snapshot, child_hours->front());
  if (!child_minutes || child_minutes->empty()) {
    return Fail("hour to minute recursive descent failed");
  }
  auto terminal = neocortex::proj::TemporalLadder::OpenWindow(
      *snapshot, child_minutes->front());
  auto members = neocortex::proj::TemporalLadder::ResolveMembers(
      *snapshot, child_minutes->front(), 4096U);
  if (!terminal || !terminal->empty() || !members || members->empty() ||
      !std::ranges::is_sorted(*members) ||
      members->size() != child_minutes->front().member_count) {
    return Fail("minute member resolution failed");
  }
  auto limited = neocortex::proj::TemporalLadder::ResolveMembers(
      *snapshot, child_minutes->front(), 1U);
  if (!limited || limited->size() != 1U ||
      limited->front() != members->front()) {
    return Fail("minute member limit failed");
  }
  snapshot = std::unexpected(neocortex::Error{
      neocortex::ErrorCode::kBackendUnavailable, 0});

  auto original = Dump(*store);
  if (!original) {
    return Fail("temporal ladder canonical dump failed");
  }
  auto reset = store->Reset(neocortex::proj::ProjectionId::kTemporalLadder);
  auto healed = neocortex::proj::TemporalLadder::Rebuild(*store, frames);
  auto rebuilt = Dump(*store);
  if (!reset || !healed || !healed->complete || !rebuilt ||
      *original != *rebuilt) {
    return Fail("missing temporal tiers did not self-heal byte-identically");
  }

  const std::array<std::byte, 2> stale_key = {std::byte{0x7f},
                                              std::byte{0x01}};
  const std::array<std::byte, 1> stale_value = {std::byte{0x55}};
  const neocortex::proj::Mutation stale{
      .kind = neocortex::proj::MutationKind::kPut,
      .key = stale_key,
      .value = stale_value,
  };
  auto polluted = store->Apply(neocortex::proj::ProjectionId::kTemporalLadder,
                               257, std::span(&stale, 1));
  auto repaired = neocortex::proj::TemporalLadder::Rebuild(
      *store, frames, true);
  auto repaired_dump = Dump(*store);
  if (!polluted || !repaired || !repaired->complete || !repaired_dump ||
      *original != *repaired_dump) {
    return Fail("stale temporal tier did not self-heal by replay");
  }

  auto final_snapshot = store->BeginSnapshot();
  if (!final_snapshot) {
    return Fail("final temporal ladder snapshot failed");
  }
  auto invalid = neocortex::proj::TemporalLadder::ListWindows(
      *final_snapshot, neocortex::proj::TemporalLevel::kMinute, 7, 7);
  auto absent = neocortex::proj::TemporalLadder::ListWindows(
      *final_snapshot, neocortex::proj::TemporalLevel::kMinute,
      100LL * kWeek, 101LL * kWeek);
  if (invalid || !absent || !absent->empty()) {
    return Fail("temporal ladder range boundary handling failed");
  }
  return 0;
}

}  // namespace

int main() {
  const auto path = std::filesystem::temp_directory_path() /
                    ("neocortex-ladder-" + std::to_string(::getpid()));
  std::error_code cleanup_error;
  std::filesystem::remove_all(path, cleanup_error);
  const int result = TestLadder(path);
  std::filesystem::remove_all(path, cleanup_error);
  return result;
}
