#include <array>
#include <atomic>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <filesystem>
#include <span>
#include <string>
#include <string_view>
#include <thread>
#include <vector>

#include <unistd.h>

#include "core/apply.h"
#include "log/storage_adapter.h"
#include "mmr/store.h"
#include "sim/sim_env.h"
#include "event_fixture.h"

namespace {

using neocortex::core::ApplyLoop;
using neocortex::core::ApplyRequest;

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

std::uint64_t Next(std::uint64_t& state) {
  state ^= state << 13U;
  state ^= state >> 7U;
  state ^= state << 17U;
  return state;
}

std::vector<std::vector<std::byte>> Workload(std::size_t count) {
  std::vector<std::vector<std::byte>> payloads;
  payloads.reserve(count);
  std::uint64_t random = 0x45dcf13a998ef21bULL;
  for (std::size_t index = 0; index < count; ++index) {
    const auto length = static_cast<std::size_t>((Next(random) % 193U) + 1U);
    std::vector<std::byte> payload(length);
    for (auto& value : payload) {
      value = static_cast<std::byte>(Next(random) & 0xffU);
    }
    const auto kind =
        static_cast<neocortex::log::EventKind>((index % 21U) + 1U);
    payloads.push_back(neocortex::test::BuildEvent(
        kind, static_cast<std::uint64_t>(index) + 1U, payload));
  }
  return payloads;
}

neocortex::log::ConversationId Conversation(std::size_t index) {
  neocortex::log::ConversationId conversation{};
  for (std::size_t byte = 0; byte < conversation.bytes.size(); ++byte) {
    conversation.bytes[byte] =
        static_cast<std::byte>((index * 37U + byte * 11U) & 0xffU);
  }
  return conversation;
}

std::vector<ApplyRequest> Requests(
    const std::vector<std::vector<std::byte>>& payloads, std::size_t start,
    std::size_t count) {
  std::vector<ApplyRequest> requests;
  requests.reserve(count);
  for (std::size_t index = start; index < start + count; ++index) {
    requests.push_back(ApplyRequest{
        .kind = static_cast<neocortex::log::EventKind>((index % 21U) + 1U),
        .conversation = Conversation(index % 29U),
        .plaintext_payload = payloads[index],
    });
  }
  return requests;
}

struct Baseline final {
  std::vector<std::byte> state;
  std::vector<std::byte> log;
};

std::expected<Baseline, neocortex::Error> BuildBaseline(
    const std::vector<std::vector<std::byte>>& payloads) {
  neocortex::sim::SimulatedStorage storage;
  neocortex::sim::SimClock clock;
  neocortex::sim::SimEntropy entropy(91);
  auto loop = ApplyLoop::Boot(
      {.clock = clock, .entropy = entropy, .storage = storage, .actor = 17},
      payloads.size());
  if (!loop) {
    return std::unexpected(loop.error());
  }
  const auto requests = Requests(payloads, 0, payloads.size());
  auto applied = loop->ApplyBatch(requests);
  if (!applied) {
    return std::unexpected(applied.error());
  }
  return Baseline{.state = loop->state().CanonicalBytes(),
                  .log = storage.durable_bytes()};
}

int TestRandomizedConvergence() {
  constexpr std::size_t kWorkloadSize = 32;
  constexpr std::size_t kSchedules = 2048;
  const auto payloads = Workload(kWorkloadSize);
  auto baseline = BuildBaseline(payloads);
  if (!baseline) {
    return Fail("baseline failed");
  }

  for (std::size_t schedule = 0; schedule < kSchedules; ++schedule) {
    neocortex::sim::SimulatedStorage storage;
    std::uint64_t random = 0x6a09e667f3bcc909ULL ^ schedule;
    std::size_t iterations = 0;
    while (true) {
      auto recovered = storage.Recover();
      if (!recovered) {
        return Fail("scheduled recovery failed");
      }
      const auto durable_count = recovered->size();
      if (durable_count == payloads.size()) {
        break;
      }
      if (++iterations > kWorkloadSize * 12U) {
        return Fail("fault schedule made no progress");
      }

      neocortex::sim::SimClock clock(
          1'000'000 + static_cast<std::int64_t>(durable_count) * 1'000);
      neocortex::sim::SimEntropy entropy(91 + durable_count);
      auto loop = ApplyLoop::Boot(
          {.clock = clock, .entropy = entropy, .storage = storage, .actor = 17}, 8);
      if (!loop) {
        return Fail("scheduled boot failed");
      }
      const auto batch_size = std::min<std::size_t>(
          (Next(random) % 8U) + 1U, payloads.size() - durable_count);
      const auto requests = Requests(payloads, durable_count, batch_size);

      neocortex::sim::FaultPlan fault;
      fault.maximum_write_bytes = static_cast<std::size_t>((Next(random) % 31U) + 1U);
      fault.maximum_read_bytes = static_cast<std::size_t>((Next(random) % 17U) + 1U);
      fault.reverse_write_completion = (Next(random) & 1U) != 0;
      const auto mode = iterations % 7U;
      if (mode == 1U) {
        fault.torn_after_bytes = static_cast<std::size_t>((Next(random) % 41U) + 1U);
      } else if (mode == 2U) {
        fault.fsync_lies = true;
      } else if (mode == 3U) {
        fault.kill_after_durable_lsn =
            static_cast<std::uint64_t>(durable_count + 1U +
                                       (Next(random) % batch_size));
      }
      storage.SetFaultPlan(fault);
      const auto result = loop->ApplyBatch(requests);
      if (!result && result.error().code != neocortex::ErrorCode::kProcessKilled) {
        return Fail("fault schedule produced an unexpected apply error");
      }
      storage.Crash();
    }

    neocortex::sim::SimClock final_clock;
    neocortex::sim::SimEntropy final_entropy(91);
    auto final_loop = ApplyLoop::Boot(
        {.clock = final_clock,
         .entropy = final_entropy,
         .storage = storage,
         .actor = 17},
        8);
    if (!final_loop || final_loop->state().CanonicalBytes() != baseline->state ||
        storage.durable_bytes() != baseline->log) {
      return Fail("randomized schedule did not converge byte-for-byte");
    }
  }
  return 0;
}

int TestKillAtEveryLsn() {
  constexpr std::size_t kCount = 64;
  const auto payloads = Workload(kCount);
  auto baseline = BuildBaseline(payloads);
  if (!baseline) {
    return Fail("kill baseline failed");
  }
  for (std::size_t kill = 1; kill <= kCount; ++kill) {
    neocortex::sim::SimulatedStorage storage;
    for (std::size_t index = 0; index < kCount; ++index) {
      auto recovered = storage.Recover();
      if (!recovered) {
        return Fail("kill recovery failed");
      }
      neocortex::sim::SimClock clock(
          1'000'000 + static_cast<std::int64_t>(recovered->size()) * 1'000);
      neocortex::sim::SimEntropy entropy(11);
      auto loop = ApplyLoop::Boot(
          {.clock = clock, .entropy = entropy, .storage = storage, .actor = 17}, 1);
      if (!loop) {
        return Fail("kill boot failed");
      }
      neocortex::sim::FaultPlan fault;
      if (index + 1U == kill) {
        fault.kill_after_durable_lsn = static_cast<std::uint64_t>(kill);
      }
      storage.SetFaultPlan(fault);
      const auto request = Requests(payloads, index, 1);
      const auto result = loop->ApplyBatch(request);
      if (!result && result.error().code != neocortex::ErrorCode::kProcessKilled) {
        return Fail("kill schedule produced an unexpected apply error");
      }
      storage.Crash();
    }
    neocortex::sim::SimClock clock;
    neocortex::sim::SimEntropy entropy(11);
    auto loop = ApplyLoop::Boot(
        {.clock = clock, .entropy = entropy, .storage = storage, .actor = 17}, 1);
    if (!loop || loop->state().CanonicalBytes() != baseline->state ||
        storage.durable_bytes() != baseline->log) {
      return Fail("kill-at-LSN recovery diverged");
    }
  }
  return 0;
}

int TestCorruptionAndWriterOwnership() {
  const auto payloads = Workload(3);
  neocortex::sim::SimulatedStorage storage;
  neocortex::sim::SimClock clock;
  neocortex::sim::SimEntropy entropy(7);
  auto loop = ApplyLoop::Boot(
      {.clock = clock, .entropy = entropy, .storage = storage, .actor = 17}, 3);
  if (!loop) {
    return Fail("ownership boot failed");
  }
  const auto requests = Requests(payloads, 0, 3);
  std::atomic<bool> rejected = false;
  std::thread intruder([&]() {
    auto result = loop->ApplyBatch(std::span<const ApplyRequest>(requests).first(1));
    rejected.store(!result &&
                       result.error().code == neocortex::ErrorCode::kWriterViolation,
                   std::memory_order_relaxed);
  });
  intruder.join();
  if (!rejected.load(std::memory_order_relaxed)) {
    return Fail("second writer was accepted");
  }
  if (!loop->ApplyBatch(requests)) {
    return Fail("owner writer was rejected");
  }
  if (!storage.CorruptDurableByte(neocortex::log::kFrameHeaderSize,
                                  std::byte{0x80})) {
    return Fail("corruption injection failed");
  }
  auto corrupted = storage.Recover();
  if (corrupted ||
      corrupted.error().code != neocortex::ErrorCode::kInteriorCorruption) {
    return Fail("interior corruption was not rejected");
  }
  return 0;
}

int TestRealStorage() {
  std::array<char, 64> template_path{};
  const std::string seed_path = "/tmp/neocortex-core-sim-XXXXXX";
  std::copy(seed_path.begin(), seed_path.end(), template_path.begin());
  char* directory = ::mkdtemp(template_path.data());
  if (directory == nullptr) {
    return Fail("mkdtemp failed");
  }
  const std::filesystem::path path(directory);
  neocortex::mmr::SigningSeed seed{};
  seed[0] = std::byte{0x71};
  auto keys = neocortex::mmr::SigningKeyPairFromSeed(seed);
  if (!keys) {
    return Fail("key derivation failed");
  }
  neocortex::seal::UserId user{};
  user[0] = std::byte{0x52};
  neocortex::seal::KeyEncryptionKey kek{};
  kek[0] = std::byte{0xa7};
  neocortex::sim::SimEntropy entropy(7);
  auto storage = neocortex::log::SegmentStorage::Open(
      path, 17, keys->public_key, user, kek, entropy,
      {.maximum_io_bytes = 5,
       .backend = neocortex::log::WriteBackend::kPwrite});
  if (!storage) {
    return Fail("real storage open failed");
  }
  const auto payloads = Workload(37);
  neocortex::sim::SimClock clock;
  auto loop = ApplyLoop::Boot(
      {.clock = clock, .entropy = entropy, .storage = *storage, .actor = 17}, 37);
  if (!loop || !loop->ApplyBatch(Requests(payloads, 0, payloads.size()))) {
    return Fail("real storage apply failed");
  }
  const auto expected = loop->state().CanonicalBytes();

  neocortex::sim::SimEntropy restarted_entropy(7);
  auto reopened = neocortex::log::SegmentStorage::Open(
      path, 17, keys->public_key, user, kek, restarted_entropy,
      {.maximum_io_bytes = 3,
       .backend = neocortex::log::WriteBackend::kPwrite});
  neocortex::sim::SimClock restarted_clock;
  if (!reopened) {
    return Fail("real storage reopen failed");
  }
  auto restarted = ApplyLoop::Boot(
      {.clock = restarted_clock,
       .entropy = restarted_entropy,
       .storage = *reopened,
       .actor = 17},
      37);
  if (!restarted || restarted->state().CanonicalBytes() != expected ||
      reopened->mmr_store().mmr().leaf_count() != payloads.size() ||
      !reopened->mmr_store().mmr().leaves().empty()) {
    return Fail("real storage restart diverged");
  }
  std::error_code cleanup_error;
  std::filesystem::remove_all(path, cleanup_error);
  if (cleanup_error) {
    return Fail("temporary directory cleanup failed");
  }
  return 0;
}

}  // namespace

int main() {
  if (TestRandomizedConvergence() != 0 || TestKillAtEveryLsn() != 0 ||
      TestCorruptionAndWriterOwnership() != 0 || TestRealStorage() != 0) {
    return 1;
  }
  return 0;
}
