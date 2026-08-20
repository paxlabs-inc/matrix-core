#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <span>
#include <vector>

#include "core/error.h"
#include "log/frame.h"

namespace neocortex::core {

class Clock {
 public:
  virtual ~Clock() = default;
  [[nodiscard]] virtual std::expected<std::int64_t, Error> NowNs() = 0;
};

class Entropy {
 public:
  virtual ~Entropy() = default;
  [[nodiscard]] virtual std::expected<void, Error> Fill(std::span<std::byte> output) = 0;
};

struct StorageCommit final {
  std::uint64_t first_lsn;
  std::uint64_t last_lsn;
};

class Storage {
 public:
  virtual ~Storage() = default;
  [[nodiscard]] virtual std::expected<StorageCommit, Error> Append(
      std::span<const log::Frame> frames) = 0;
  [[nodiscard]] virtual std::expected<std::vector<log::Frame>, Error> Recover() = 0;
  [[nodiscard]] virtual std::expected<std::vector<log::Frame>, Error> ReadFrom(
      std::uint64_t first_lsn, std::size_t maximum_frames) = 0;
};

struct Environment final {
  Clock& clock;
  Entropy& entropy;
  Storage& storage;
  std::uint16_t actor;
};

class SystemClock final : public Clock {
 public:
  [[nodiscard]] std::expected<std::int64_t, Error> NowNs() override;
};

class SystemEntropy final : public Entropy {
 public:
  [[nodiscard]] std::expected<void, Error> Fill(std::span<std::byte> output) override;
};

}  // namespace neocortex::core
