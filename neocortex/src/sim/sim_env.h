#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <limits>
#include <span>
#include <vector>

#include "core/env.h"

namespace neocortex::sim {

class SimClock final : public core::Clock {
 public:
  explicit SimClock(std::int64_t initial_ns = 1'000'000)
      : next_ns_(initial_ns) {}

  [[nodiscard]] std::expected<std::int64_t, Error> NowNs() override;

 private:
  std::int64_t next_ns_;
  static constexpr std::int64_t kStepNs = 1'000;
};

class SimEntropy final : public core::Entropy {
 public:
  explicit SimEntropy(std::uint64_t seed) : state_(seed) {}
  [[nodiscard]] std::expected<void, Error> Fill(
      std::span<std::byte> output) override;

 private:
  std::uint64_t state_;
};

struct FaultPlan final {
  std::size_t maximum_write_bytes = std::numeric_limits<std::size_t>::max();
  std::size_t maximum_read_bytes = std::numeric_limits<std::size_t>::max();
  std::size_t torn_after_bytes = std::numeric_limits<std::size_t>::max();
  std::uint64_t kill_after_durable_lsn = 0;
  bool reverse_write_completion = false;
  bool fsync_lies = false;
};

class SimulatedStorage final : public core::Storage {
 public:
  [[nodiscard]] std::expected<core::StorageCommit, Error> Append(
      std::span<const log::Frame> frames) override;
  [[nodiscard]] std::expected<std::vector<log::Frame>, Error> Recover() override;
  [[nodiscard]] std::expected<std::vector<log::Frame>, Error> ReadFrom(
      std::uint64_t first_lsn, std::size_t maximum_frames) override;

  void SetFaultPlan(FaultPlan plan) { plan_ = plan; }
  void Crash();
  [[nodiscard]] std::expected<void, Error> CorruptDurableByte(std::size_t offset,
                                                              std::byte mask);
  [[nodiscard]] const std::vector<std::byte>& durable_bytes() const {
    return durable_bytes_;
  }

 private:
  [[nodiscard]] std::expected<std::vector<log::Frame>, Error> ParseDurable(
      bool repair_tail);

  FaultPlan plan_{};
  std::vector<std::byte> durable_bytes_;
  std::vector<std::byte> volatile_bytes_;
  std::uint64_t next_lsn_ = 1;
};

}  // namespace neocortex::sim
