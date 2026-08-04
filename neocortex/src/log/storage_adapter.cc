#include "log/storage_adapter.h"

#include <limits>

namespace neocortex::log {
namespace {

constexpr std::size_t kVerificationBatchSize = 1024;

}  // namespace

SegmentStorage::SegmentStorage(SegmentLog log, mmr::MmrStore mmr_store,
                               seal::KeyHierarchy key_hierarchy,
                               core::Entropy& entropy)
    : log_(std::move(log)),
      mmr_store_(std::move(mmr_store)),
      key_hierarchy_(std::move(key_hierarchy)),
      entropy_(entropy) {}

std::expected<SegmentStorage, Error> SegmentStorage::Open(
    const std::filesystem::path& actor_directory, std::uint16_t actor,
    const mmr::PublicKey& trusted_public_key, const seal::UserId& user,
    const seal::KeyEncryptionKey& kek, core::Entropy& entropy,
    SegmentLogOptions options) {
  auto log = SegmentLog::Open(actor_directory, actor, options);
  if (!log) {
    return std::unexpected(log.error());
  }
  auto key_hierarchy = seal::KeyHierarchy::OpenOrCreate(
      actor_directory, actor, user, kek, entropy, trusted_public_key,
      log->next_lsn() == 1U);
  if (!key_hierarchy) {
    return std::unexpected(key_hierarchy.error());
  }
  auto store = mmr::MmrStore::Open(actor_directory, actor, trusted_public_key);
  if (!store) {
    return std::unexpected(store.error());
  }
  SegmentStorage result(std::move(*log), std::move(*store),
                        std::move(*key_hierarchy), entropy);
  auto recovered = result.VerifyAndRepairBounded();
  if (!recovered) {
    return std::unexpected(recovered.error());
  }
  return result;
}

std::expected<std::vector<Frame>, Error> SegmentStorage::VerifyAndRepair() {
  auto disk_frames = log_.ReadAll();
  if (!disk_frames) {
    return std::unexpected(disk_frames.error());
  }
  std::vector<Frame> frames;
  frames.reserve(disk_frames->size());
  for (const auto& disk_frame : *disk_frames) {
    auto plaintext = key_hierarchy_.Unseal(disk_frame.header,
                                           disk_frame.sealed_payload);
    if (!plaintext) {
      return std::unexpected(plaintext.error());
    }
    frames.push_back(Frame{.header = disk_frame.header,
                           .sealed_payload = std::move(*plaintext)});
  }
  const auto leaf_count = mmr_store_.mmr().leaf_count();
  if (leaf_count > frames.size()) {
    return std::unexpected(
        Error{ErrorCode::kCheckpointMismatch, 0, leaf_count, frames.size()});
  }
  for (std::size_t begin = 0; begin < leaf_count;
       begin += kVerificationBatchSize) {
    const auto count = std::min<std::size_t>(
        kVerificationBatchSize, static_cast<std::size_t>(leaf_count) - begin);
    std::vector<mmr::Hash> hashes;
    hashes.reserve(count);
    for (std::size_t index = begin; index < begin + count; ++index) {
      hashes.push_back(mmr::HashFramePlaintext(frames[index].header,
                                               frames[index].sealed_payload));
    }
    auto verified = mmr_store_.VerifyLeafHashes(begin, hashes);
    if (!verified) {
      return std::unexpected(verified.error());
    }
  }
  for (std::size_t index = static_cast<std::size_t>(leaf_count);
       index < frames.size(); ++index) {
    auto appended = mmr_store_.AppendFrame(frames[index].header,
                                           frames[index].sealed_payload);
    if (!appended) {
      return std::unexpected(appended.error());
    }
  }
  return frames;
}

std::expected<void, Error> SegmentStorage::VerifyAndRepairBounded() {
  const auto leaf_count = mmr_store_.mmr().leaf_count();
  std::uint64_t next_lsn = 1;
  std::uint64_t frame_count = 0;
  while (true) {
    auto disk_frames = log_.ReadFrom(next_lsn, kVerificationBatchSize);
    if (!disk_frames) {
      return std::unexpected(disk_frames.error());
    }
    if (disk_frames->empty()) {
      break;
    }
    std::vector<mmr::Hash> existing_hashes;
    const auto existing_count = frame_count >= leaf_count
                                    ? 0U
                                    : std::min<std::uint64_t>(
                                          disk_frames->size(),
                                          leaf_count - frame_count);
    existing_hashes.reserve(static_cast<std::size_t>(existing_count));
    const auto batch_first = frame_count;
    for (auto& disk_frame : *disk_frames) {
      auto plaintext =
          key_hierarchy_.Unseal(disk_frame.header, disk_frame.sealed_payload);
      if (!plaintext) {
        return std::unexpected(plaintext.error());
      }
      const auto expected =
          mmr::HashFramePlaintext(disk_frame.header, *plaintext);
      if (frame_count < leaf_count) {
        existing_hashes.push_back(expected);
      } else {
        auto appended = mmr_store_.AppendFrame(disk_frame.header, *plaintext);
        if (!appended) {
          return std::unexpected(appended.error());
        }
      }
      ++frame_count;
    }
    auto verified = mmr_store_.VerifyLeafHashes(batch_first, existing_hashes);
    if (!verified) {
      return std::unexpected(verified.error());
    }
    if (disk_frames->back().header.lsn ==
        std::numeric_limits<std::uint64_t>::max()) {
      break;
    }
    next_lsn = disk_frames->back().header.lsn + 1U;
    if (disk_frames->size() < kVerificationBatchSize) {
      break;
    }
  }
  if (leaf_count > frame_count) {
    return std::unexpected(
        Error{ErrorCode::kCheckpointMismatch, 0, leaf_count, frame_count});
  }
  return {};
}

std::expected<core::StorageCommit, Error> SegmentStorage::Append(
    std::span<const Frame> frames) {
  if (frames.empty()) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  if (frames.front().header.lsn != log_.next_lsn()) {
    return std::unexpected(Error{ErrorCode::kSequenceViolation, 0,
                                 frames.front().header.lsn, 0});
  }
  std::vector<AppendRequest> requests;
  std::vector<std::vector<std::byte>> sealed_payloads;
  requests.reserve(frames.size());
  sealed_payloads.reserve(frames.size());
  std::uint64_t expected_lsn = frames.front().header.lsn;
  for (const auto& frame : frames) {
    if (frame.header.lsn != expected_lsn) {
      return std::unexpected(
          Error{ErrorCode::kSequenceViolation, 0, frame.header.lsn, 0});
    }
    auto sealed = key_hierarchy_.Seal(frame.header, frame.sealed_payload, entropy_);
    if (!sealed) {
      return std::unexpected(sealed.error());
    }
    sealed_payloads.push_back(std::move(*sealed));
    requests.push_back(AppendRequest{.kind = frame.header.kind,
                                     .wall_timestamp_ns = frame.header.wall_timestamp_ns,
                                     .conversation = frame.header.conversation,
                                     .sealed_payload = sealed_payloads.back()});
    if (expected_lsn != std::numeric_limits<std::uint64_t>::max()) {
      ++expected_lsn;
    }
  }
  auto committed = log_.AppendBatch(requests);
  if (!committed) {
    return std::unexpected(committed.error());
  }
  for (const auto& frame : frames) {
    auto appended = mmr_store_.AppendFrame(frame.header, frame.sealed_payload);
    if (!appended) {
      return std::unexpected(Error{ErrorCode::kDurabilityFailure,
                                   appended.error().system_error,
                                   frame.header.lsn,
                                   appended.error().offset});
    }
  }
  return core::StorageCommit{.first_lsn = committed->first_lsn,
                             .last_lsn = committed->last_lsn};
}

std::expected<std::vector<Frame>, Error> SegmentStorage::Recover() {
  return VerifyAndRepair();
}

std::expected<std::vector<Frame>, Error> SegmentStorage::ReadFrom(
    std::uint64_t first_lsn, std::size_t maximum_frames) {
  auto disk_frames = log_.ReadFrom(first_lsn, maximum_frames);
  if (!disk_frames) {
    return std::unexpected(disk_frames.error());
  }
  std::vector<Frame> frames;
  frames.reserve(disk_frames->size());
  for (auto& disk_frame : *disk_frames) {
    auto plaintext =
        key_hierarchy_.Unseal(disk_frame.header, disk_frame.sealed_payload);
    if (!plaintext) {
      return std::unexpected(plaintext.error());
    }
    frames.push_back(Frame{.header = disk_frame.header,
                           .sealed_payload = std::move(*plaintext)});
  }
  return frames;
}

std::expected<void, Error> SegmentStorage::TruncateTo(std::uint64_t last_lsn) {
  auto rewound = mmr_store_.RewindTo(last_lsn);
  if (!rewound) {
    return std::unexpected(rewound.error());
  }
  return log_.TruncateTo(last_lsn);
}

std::expected<seal::DeletionReceipt, Error> SegmentStorage::DestroyKey(
    const mmr::SigningKeyPair& signing_key) {
  const auto last_lsn = log_.next_lsn() - 1U;
  return key_hierarchy_.Destroy(last_lsn, mmr_store_.mmr().Root(), signing_key);
}

}  // namespace neocortex::log
