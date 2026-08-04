#include "core/apply.h"

#include <array>
#include <limits>

namespace neocortex::core {
namespace {

constexpr std::size_t kRecoveryBatchSize = 1024;

void AppendU64(std::vector<std::byte>& output, std::uint64_t value) {
  for (std::uint32_t shift = 0; shift < 64; shift += 8) {
    output.push_back(static_cast<std::byte>((value >> shift) & 0xffU));
  }
}

std::expected<log::EventKind, Error> LookupKind(void* context,
                                                std::uint64_t lsn) {
  auto* storage = static_cast<Storage*>(context);
  auto frames = storage->ReadFrom(lsn, 1);
  if (!frames) {
    return std::unexpected(frames.error());
  }
  if (frames->size() != 1 || frames->front().header.lsn != lsn) {
    return std::unexpected(Error{ErrorCode::kOrderingViolation, 0, lsn});
  }
  return frames->front().header.kind;
}

}  // namespace

std::expected<void, Error> AppliedState::Apply(const log::Frame& frame) {
  if (last_lsn_ == std::numeric_limits<std::uint64_t>::max() ||
      frame.header.lsn != last_lsn_ + 1) {
    return std::unexpected(
        Error{ErrorCode::kSequenceViolation, 0, frame.header.lsn, 0});
  }
  const auto kind = static_cast<std::uint8_t>(frame.header.kind);
  if (!log::IsEventKind(kind)) {
    return std::unexpected(Error{ErrorCode::kInvalidKind, 0, frame.header.lsn, 0});
  }
  if (event_count_ == std::numeric_limits<std::uint64_t>::max() ||
      kind_counts_[static_cast<std::size_t>(kind - 1)] ==
          std::numeric_limits<std::uint64_t>::max()) {
    return std::unexpected(
        Error{ErrorCode::kInvariantViolation, 0, frame.header.lsn, 0});
  }

  const auto frame_hash =
      mmr::HashFramePlaintext(frame.header, frame.sealed_payload);
  std::array<std::byte, 65> material{};
  material[0] = std::byte{0x53};
  std::copy(digest_.begin(), digest_.end(), material.begin() + 1);
  std::copy(frame_hash.begin(), frame_hash.end(), material.begin() + 33);
  digest_ = mmr::HashBytes(material);
  last_lsn_ = frame.header.lsn;
  ++event_count_;
  ++kind_counts_[static_cast<std::size_t>(kind - 1)];
  return {};
}

std::vector<std::byte> AppliedState::CanonicalBytes() const {
  std::vector<std::byte> output;
  output.reserve(16 + (kEventKindCount * 8) + digest_.size());
  AppendU64(output, last_lsn_);
  AppendU64(output, event_count_);
  for (const auto count : kind_counts_) {
    AppendU64(output, count);
  }
  output.insert(output.end(), digest_.begin(), digest_.end());
  return output;
}

ApplyLoop::ApplyLoop(Environment environment, std::size_t maximum_batch_size,
                     std::thread::id writer_thread, AppliedState state,
                     events::OrderingState ordering_state)
    : environment_(environment),
      maximum_batch_size_(maximum_batch_size),
      writer_thread_(writer_thread),
      state_(state),
      ordering_state_(ordering_state) {}

std::expected<ApplyLoop, Error> ApplyLoop::Boot(Environment environment,
                                                std::size_t maximum_batch_size) {
  if (maximum_batch_size == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  AppliedState state;
  events::OrderingState ordering_state;
  std::uint64_t next_lsn = 1;
  while (true) {
    auto recovered = environment.storage.ReadFrom(next_lsn, kRecoveryBatchSize);
    if (!recovered) {
      return std::unexpected(recovered.error());
    }
    if (recovered->empty()) {
      break;
    }
    auto validated = ordering_state.ValidateBatch(
        *recovered, events::Boundary::kDisk,
        {.context = &environment.storage, .read = LookupKind});
    if (!validated) {
      return std::unexpected(validated.error());
    }
    for (const auto& frame : *recovered) {
      auto applied = state.Apply(frame);
      if (!applied) {
        return std::unexpected(applied.error());
      }
    }
    auto recorded = ordering_state.RecordBatch(*recovered);
    if (!recorded) {
      return std::unexpected(recorded.error());
    }
    if (recovered->back().header.lsn ==
        std::numeric_limits<std::uint64_t>::max()) {
      break;
    }
    next_lsn = recovered->back().header.lsn + 1U;
    if (recovered->size() < kRecoveryBatchSize) {
      break;
    }
  }
  return ApplyLoop(environment, maximum_batch_size, std::this_thread::get_id(), state,
                   ordering_state);
}

std::expected<std::vector<ApplyAck>, Error> ApplyLoop::ApplyBatch(
    std::span<const ApplyRequest> requests) {
  if (std::this_thread::get_id() != writer_thread_) {
    return std::unexpected(Error{ErrorCode::kWriterViolation, 0});
  }
  if (requests.empty() || requests.size() > maximum_batch_size_) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  if (state_.last_lsn() >
      std::numeric_limits<std::uint64_t>::max() - requests.size()) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
  }

  std::vector<log::Frame> frames;
  std::vector<ApplyAck> acknowledgements;
  frames.reserve(requests.size());
  acknowledgements.reserve(requests.size());
  std::uint64_t next_lsn = state_.last_lsn() + 1;
  for (const auto& request : requests) {
    if (!log::IsEventKind(static_cast<std::uint8_t>(request.kind))) {
      return std::unexpected(Error{ErrorCode::kInvalidKind, 0, next_lsn, 0});
    }
    auto timestamp = environment_.clock.NowNs();
    if (!timestamp) {
      return std::unexpected(timestamp.error());
    }
    frames.push_back(log::Frame{
        .header = log::FrameHeader{.lsn = next_lsn,
                                  .kind = request.kind,
                                  .wall_timestamp_ns = *timestamp,
                                  .actor = environment_.actor,
                                  .conversation = request.conversation},
        .sealed_payload = std::vector<std::byte>(request.plaintext_payload.begin(),
                                                 request.plaintext_payload.end())});
    acknowledgements.push_back(
        ApplyAck{.lsn = next_lsn, .wall_timestamp_ns = *timestamp});
    ++next_lsn;
  }

  for (std::size_t index = 1; index < requests.size(); ++index) {
    if (requests[index].boundary != requests.front().boundary) {
      return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
    }
  }
  auto validated =
      ordering_state_.ValidateBatch(
          frames, requests.front().boundary,
          {.context = &environment_.storage, .read = LookupKind});
  if (!validated) {
    return std::unexpected(validated.error());
  }

  auto commit = environment_.storage.Append(frames);
  if (!commit) {
    return std::unexpected(commit.error());
  }
  if (commit->first_lsn != frames.front().header.lsn ||
      commit->last_lsn != frames.back().header.lsn) {
    return std::unexpected(Error{ErrorCode::kDurabilityFailure, 0,
                                 commit->last_lsn, 0});
  }
  auto recorded = ordering_state_.RecordBatch(frames);
  if (!recorded) {
    return std::unexpected(recorded.error());
  }
  for (const auto& frame : frames) {
    auto applied = state_.Apply(frame);
    if (!applied) {
      return std::unexpected(applied.error());
    }
  }
  return acknowledgements;
}

}  // namespace neocortex::core
