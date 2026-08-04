#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <expected>
#include <filesystem>
#include <map>
#include <memory>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include "core/apply.h"
#include "core/env.h"
#include "log/storage_adapter.h"
#include "mmr/store.h"
#include "proj/beliefs/belief_store.h"
#include "proj/ladder/temporal_ladder.h"
#include "proj/store.h"
#include "proj/vectors/vector_lane.h"
#include "schema/protocol_generated.h"
#include "seal/sealing.h"

namespace neocortex::serve {

struct ActorEngineConfig final {
  std::filesystem::path actor_directory;
  std::uint16_t actor;
  seal::UserId user;
  seal::KeyEncryptionKey kek;
  mmr::SigningKeyPair signing_key;
  std::size_t projection_map_bytes =
      static_cast<std::size_t>(256) * 1024U * 1024U;
};

struct AppendResult final {
  std::uint64_t client_seq;
  std::uint64_t first_lsn;
  std::uint64_t last_lsn;
  bool duplicate;
  std::uint64_t leaf_count;
  mmr::Hash last_leaf_hash;
  mmr::Hash root;
  std::vector<log::Frame> frames;
};

struct ProjectionStat final {
  std::string name;
  std::uint64_t applied_lsn;
};

struct ActorStats final {
  std::uint16_t actor;
  std::uint64_t log_events;
  std::uint64_t log_bytes;
  std::vector<ProjectionStat> projections;
};

struct RecallOutput final {
  std::vector<proj::TemporalWindow> windows;
  std::vector<std::uint64_t> members;
};

struct FrameScan final {
  std::vector<log::Frame> frames;
  bool exhausted;
};

struct FrameScanLimits final {
  std::size_t maximum_frames;
  std::size_t maximum_payload_bytes;
  std::size_t maximum_scanned_frames;
};

struct CheckpointWrite final {
  std::uint64_t lsn;
  std::vector<log::Frame> frames;
};

struct CheckpointRead final {
  std::uint64_t lsn;
  std::vector<std::byte> blob;
};

struct AttestWrite final {
  std::uint64_t first_lsn;
  std::uint64_t last_lsn;
  std::uint32_t count;
  std::vector<log::Frame> frames;
};

class ActorEngine final {
 public:
  static std::expected<std::unique_ptr<ActorEngine>, Error> Open(
      ActorEngineConfig config);

  ActorEngine(const ActorEngine&) = delete;
  ActorEngine& operator=(const ActorEngine&) = delete;

  [[nodiscard]] std::expected<AppendResult, Error> Append(
      std::span<const std::byte> connection_id,
      const protocol::Append& request);
  [[nodiscard]] std::expected<std::uint64_t, Error> NextClientSequence(
      std::span<const std::byte> connection_id);
  [[nodiscard]] std::expected<FrameScan, Error> FramesSince(
      std::uint64_t since_lsn,
      const std::optional<log::ConversationId>& conversation,
      FrameScanLimits limits);
  [[nodiscard]] std::expected<std::vector<std::byte>, Error> Activate(
      const protocol::Activate& request);
  [[nodiscard]] std::expected<RecallOutput, Error> Recall(
      const protocol::Recall& request);
  [[nodiscard]] std::expected<std::optional<proj::BeliefRecord>, Error> AsOf(
      const protocol::AsOf& request);
  [[nodiscard]] std::expected<CheckpointWrite, Error> WriteCheckpoint(
      std::span<const std::byte> connection_id, std::uint64_t client_seq,
      std::span<const std::byte> turn_id, std::span<const std::byte> blob);
  [[nodiscard]] std::expected<std::optional<CheckpointRead>, Error>
  LatestCheckpoint(std::span<const std::byte> turn_id);
  [[nodiscard]] std::expected<AttestWrite, Error> Attest(
      std::span<const std::byte> connection_id, std::uint64_t client_seq,
      std::span<const std::uint64_t> used,
      std::span<const std::uint64_t> ignored);
  [[nodiscard]] std::expected<ActorStats, Error> Stats();
  [[nodiscard]] mmr::VerificationStatus VerificationStatus() const;
  [[nodiscard]] std::expected<std::uint64_t, Error> RebuildProjection(
      std::string_view name);
  [[nodiscard]] std::expected<std::vector<std::byte>, Error> CryptoDelete();

  [[nodiscard]] std::uint16_t actor() const { return config_.actor; }
  [[nodiscard]] bool ready() const { return ready_; }

 private:
  using ConnectionId = std::array<std::byte, 16>;

  struct DedupState final {
    std::uint64_t client_seq;
    std::uint64_t first_lsn;
    std::uint64_t last_lsn;
    mmr::Hash digest;
  };

  explicit ActorEngine(ActorEngineConfig config);

  [[nodiscard]] std::expected<void, Error> Initialize();
  [[nodiscard]] std::expected<void, Error> RollBackTornBatch();
  [[nodiscard]] std::expected<void, Error> EnsureReady();
  [[nodiscard]] std::expected<std::vector<log::Frame>, Error>
  CommitApplyRequests(std::span<const core::ApplyRequest> requests);
  [[nodiscard]] std::expected<void, Error> ApplyFrame(
      const log::Frame& frame);
  [[nodiscard]] std::expected<void, Error> ApplyProjection(
      proj::ProjectionId projection, const log::Frame& frame);
  [[nodiscard]] std::expected<void, Error> ResetProjection(
      proj::ProjectionId projection);
  [[nodiscard]] std::expected<void, Error> RebuildProjectionStream(
      std::optional<proj::ProjectionId> projection, bool reset);
  [[nodiscard]] std::expected<void, Error> RebuildAll(bool reset);
  [[nodiscard]] std::expected<void, Error> RebuildDedup();
  [[nodiscard]] std::expected<mmr::Hash, Error> RequestDigest(
      const protocol::Append& request) const;
  [[nodiscard]] std::expected<mmr::Hash, Error> FrameDigest(
      std::span<const log::Frame> frames) const;
  [[nodiscard]] std::expected<mmr::Hash, Error> ApplyDigest(
      std::span<const core::ApplyRequest> requests) const;

  ActorEngineConfig config_;
  core::SystemClock clock_;
  core::SystemEntropy entropy_;
  std::unique_ptr<log::SegmentStorage> storage_;
  std::unique_ptr<core::ApplyLoop> apply_loop_;
  std::unique_ptr<proj::ProjectionStore> projection_store_;
  std::unique_ptr<proj::VectorLane> vector_lane_;
  std::map<ConnectionId, DedupState> dedup_;
  bool ready_ = false;
};

}  // namespace neocortex::serve
