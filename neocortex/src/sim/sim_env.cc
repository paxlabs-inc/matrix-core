#include "sim/sim_env.h"

#include <algorithm>
#include <limits>

#include "log/frame.h"

namespace neocortex::sim {

std::expected<std::int64_t, Error> SimClock::NowNs() {
  if (next_ns_ > std::numeric_limits<std::int64_t>::max() - kStepNs) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
  }
  const auto result = next_ns_;
  next_ns_ += kStepNs;
  return result;
}

std::expected<void, Error> SimEntropy::Fill(std::span<std::byte> output) {
  if (state_ == 0) {
    state_ = 0x9e3779b97f4a7c15ULL;
  }
  for (auto& value : output) {
    state_ ^= state_ << 13U;
    state_ ^= state_ >> 7U;
    state_ ^= state_ << 17U;
    value = static_cast<std::byte>(state_ & 0xffU);
  }
  return {};
}

std::expected<core::StorageCommit, Error> SimulatedStorage::Append(
    std::span<const log::Frame> frames) {
  if (frames.empty()) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  std::vector<std::byte> encoded_batch;
  std::uint64_t expected_lsn = next_lsn_;
  for (const auto& frame : frames) {
    if (frame.header.lsn != expected_lsn) {
      return std::unexpected(
          Error{ErrorCode::kSequenceViolation, 0, frame.header.lsn, 0});
    }
    auto encoded = log::EncodeFrame(frame);
    if (!encoded) {
      return std::unexpected(encoded.error());
    }
    encoded_batch.insert(encoded_batch.end(), encoded->begin(), encoded->end());
    ++expected_lsn;
  }

  const auto chunk_size = std::max<std::size_t>(1, plan_.maximum_write_bytes);
  std::vector<std::byte> completed(encoded_batch.size());
  std::vector<std::size_t> offsets;
  for (std::size_t offset = 0; offset < encoded_batch.size(); offset += chunk_size) {
    offsets.push_back(offset);
  }
  if (plan_.reverse_write_completion) {
    std::reverse(offsets.begin(), offsets.end());
  }
  for (const auto offset : offsets) {
    const auto count = std::min(chunk_size, encoded_batch.size() - offset);
    std::copy_n(encoded_batch.begin() + static_cast<std::ptrdiff_t>(offset), count,
                completed.begin() + static_cast<std::ptrdiff_t>(offset));
  }

  const auto durable_count = std::min(plan_.torn_after_bytes, completed.size());
  if (durable_count != completed.size()) {
    durable_bytes_.insert(durable_bytes_.end(), completed.begin(),
                          completed.begin() + static_cast<std::ptrdiff_t>(durable_count));
    volatile_bytes_.clear();
    return std::unexpected(
        Error{ErrorCode::kProcessKilled, 0, frames.front().header.lsn, durable_count});
  }

  volatile_bytes_.insert(volatile_bytes_.end(), completed.begin(), completed.end());
  next_lsn_ = expected_lsn;
  if (!plan_.fsync_lies) {
    durable_bytes_.insert(durable_bytes_.end(), volatile_bytes_.begin(),
                          volatile_bytes_.end());
    volatile_bytes_.clear();
  }
  const core::StorageCommit commit{.first_lsn = frames.front().header.lsn,
                                   .last_lsn = frames.back().header.lsn};
  if (plan_.kill_after_durable_lsn >= commit.first_lsn &&
      plan_.kill_after_durable_lsn <= commit.last_lsn && !plan_.fsync_lies) {
    return std::unexpected(
        Error{ErrorCode::kProcessKilled, 0, plan_.kill_after_durable_lsn, 0});
  }
  return commit;
}

std::expected<std::vector<log::Frame>, Error> SimulatedStorage::ParseDurable(
    bool repair_tail) {
  std::vector<log::Frame> frames;
  std::size_t offset = 0;
  const auto read_size = std::max<std::size_t>(1, plan_.maximum_read_bytes);
  while (offset < durable_bytes_.size()) {
    std::vector<std::byte> prefix;
    const auto prefix_count = std::min<std::size_t>(sizeof(std::uint32_t),
                                                    durable_bytes_.size() - offset);
    for (std::size_t read_offset = 0; read_offset < prefix_count;
         read_offset += read_size) {
      const auto count = std::min(read_size, prefix_count - read_offset);
      prefix.insert(prefix.end(),
                    durable_bytes_.begin() + static_cast<std::ptrdiff_t>(offset + read_offset),
                    durable_bytes_.begin() +
                        static_cast<std::ptrdiff_t>(offset + read_offset + count));
    }
    auto length = log::ReadEncodedFrameLength(prefix);
    if (!length || static_cast<std::size_t>(*length) > durable_bytes_.size() - offset) {
      if (repair_tail) {
        durable_bytes_.resize(offset);
        break;
      }
      return std::unexpected(Error{ErrorCode::kTruncated, 0, 0, offset});
    }

    std::vector<std::byte> encoded;
    encoded.reserve(*length);
    for (std::size_t read_offset = 0; read_offset < *length; read_offset += read_size) {
      const auto count = std::min(read_size, static_cast<std::size_t>(*length) - read_offset);
      encoded.insert(encoded.end(),
                     durable_bytes_.begin() +
                         static_cast<std::ptrdiff_t>(offset + read_offset),
                     durable_bytes_.begin() +
                         static_cast<std::ptrdiff_t>(offset + read_offset + count));
    }
    auto frame = log::DecodeFrame(encoded);
    if (!frame) {
      const bool final_frame = offset + static_cast<std::size_t>(*length) ==
                               durable_bytes_.size();
      if (repair_tail && final_frame) {
        durable_bytes_.resize(offset);
        break;
      }
      return std::unexpected(
          Error{ErrorCode::kInteriorCorruption, frame.error().system_error,
                frame.error().lsn, offset});
    }
    if (frame->header.lsn != static_cast<std::uint64_t>(frames.size()) + 1) {
      return std::unexpected(Error{ErrorCode::kSequenceViolation, 0,
                                   frame->header.lsn, offset});
    }
    frames.push_back(std::move(*frame));
    offset += static_cast<std::size_t>(*length);
  }
  next_lsn_ = static_cast<std::uint64_t>(frames.size()) + 1;
  return frames;
}

std::expected<std::vector<log::Frame>, Error> SimulatedStorage::Recover() {
  return ParseDurable(true);
}

std::expected<std::vector<log::Frame>, Error> SimulatedStorage::ReadFrom(
    std::uint64_t first_lsn, std::size_t maximum_frames) {
  if (first_lsn == 0 || maximum_frames == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto frames = ParseDurable(false);
  if (!frames) {
    return std::unexpected(frames.error());
  }
  if (first_lsn > frames->size()) {
    return std::vector<log::Frame>{};
  }
  const auto begin = static_cast<std::size_t>(first_lsn - 1U);
  const auto count = std::min(maximum_frames, frames->size() - begin);
  std::vector<log::Frame> selected;
  selected.reserve(count);
  for (std::size_t index = begin; index < begin + count; ++index) {
    selected.push_back(std::move(frames->at(index)));
  }
  return selected;
}

void SimulatedStorage::Crash() {
  volatile_bytes_.clear();
  auto recovered = ParseDurable(true);
  if (!recovered) {
    next_lsn_ = 1;
  }
}

std::expected<void, Error> SimulatedStorage::CorruptDurableByte(
    std::size_t offset, std::byte mask) {
  if (offset >= durable_bytes_.size()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0, 0, offset});
  }
  durable_bytes_[offset] ^= mask;
  return {};
}

}  // namespace neocortex::sim
