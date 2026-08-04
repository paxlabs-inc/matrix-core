#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <expected>
#include <span>
#include <thread>
#include <vector>

#include "core/env.h"
#include "log/frame.h"
#include "mmr/mmr.h"
#include "schema/events.h"

namespace neocortex::core {

inline constexpr std::size_t kEventKindCount = 21;

struct ApplyRequest final {
  log::EventKind kind;
  log::ConversationId conversation;
  std::span<const std::byte> plaintext_payload;
  events::Boundary boundary = events::Boundary::kSocket;
};

struct ApplyAck final {
  std::uint64_t lsn;
  std::int64_t wall_timestamp_ns;
};

class AppliedState final {
 public:
  [[nodiscard]] std::expected<void, Error> Apply(const log::Frame& frame);
  [[nodiscard]] std::vector<std::byte> CanonicalBytes() const;

  [[nodiscard]] std::uint64_t last_lsn() const { return last_lsn_; }
  [[nodiscard]] std::uint64_t event_count() const { return event_count_; }
  [[nodiscard]] const std::array<std::uint64_t, kEventKindCount>& kind_counts() const {
    return kind_counts_;
  }
  [[nodiscard]] const mmr::Hash& digest() const { return digest_; }

 private:
  std::uint64_t last_lsn_ = 0;
  std::uint64_t event_count_ = 0;
  std::array<std::uint64_t, kEventKindCount> kind_counts_{};
  mmr::Hash digest_ = mmr::HashBytes({});
};

class ApplyLoop final {
 public:
  static std::expected<ApplyLoop, Error> Boot(Environment environment,
                                              std::size_t maximum_batch_size = 256);

  ApplyLoop(ApplyLoop&&) noexcept = default;
  ApplyLoop& operator=(ApplyLoop&&) = delete;
  ApplyLoop(const ApplyLoop&) = delete;
  ApplyLoop& operator=(const ApplyLoop&) = delete;

  [[nodiscard]] std::expected<std::vector<ApplyAck>, Error> ApplyBatch(
      std::span<const ApplyRequest> requests);

  [[nodiscard]] const AppliedState& state() const { return state_; }

 private:
  ApplyLoop(Environment environment, std::size_t maximum_batch_size,
            std::thread::id writer_thread, AppliedState state,
            events::OrderingState ordering_state);

  Environment environment_;
  std::size_t maximum_batch_size_;
  std::thread::id writer_thread_;
  AppliedState state_;
  events::OrderingState ordering_state_;
};

}  // namespace neocortex::core
