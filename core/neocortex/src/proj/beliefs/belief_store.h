#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <optional>
#include <span>
#include <string>
#include <vector>

#include "core/error.h"
#include "log/frame.h"
#include "proj/store.h"
#include "schema/events_generated.h"

namespace neocortex::proj {

struct BeliefProvenance final {
  std::uint64_t first_lsn;
  std::uint64_t last_lsn;

  bool operator==(const BeliefProvenance&) const = default;
};

struct BeliefConflictEdge final {
  schema::BeliefType other_type;
  std::string other_canonical_identity;
  std::uint64_t created_lsn;
  std::uint64_t resolved_lsn;
  bool obligated_surfacing;

  bool operator==(const BeliefConflictEdge&) const = default;
};

struct BeliefRecord final {
  schema::BeliefType type = schema::BeliefType::fact;
  std::vector<std::byte> belief_id;
  std::string canonical_identity;
  std::string conflict_domain;
  std::vector<std::byte> value;
  schema::AssertionClaim claim = schema::AssertionClaim::affirmative;
  std::int64_t valid_from_ns = 0;
  std::int64_t valid_to_ns = 0;
  std::uint64_t transaction_lsn = 0;
  std::uint64_t version = 0;
  std::uint64_t supersedes_version = 0;
  std::vector<BeliefProvenance> provenance;
  std::vector<BeliefConflictEdge> conflict_edges;
  bool tombstoned = false;

  bool operator==(const BeliefRecord&) const = default;
};

struct BeliefAsOf final {
  std::int64_t valid_time_ns;
  std::uint64_t transaction_lsn;
};

struct BeliefRebuildProgress final {
  std::uint64_t applied_lsn;
  std::size_t applied_frames;
  bool complete;
};

// AdmissionEvent is one socket-batch event probed by AdmitBatch before the
// batch commits. The envelope pointer must outlive the call.
struct AdmissionEvent final {
  log::EventKind kind;
  const schema::EventEnvelope* envelope;
  std::uint64_t assigned_lsn;
};

class BeliefProjection final {
 public:
  [[nodiscard]] static std::expected<void, Error> ApplyEvent(
      ProjectionStore& store, const log::Frame& frame);
  // AdmitBatch probes a socket batch against the current snapshot plus the
  // batch's own staged effects, returning the typed write-gate rejection a
  // client can surface BEFORE anything commits. Apply-time gate rejections
  // are deterministic skips (replay safety for legacy or imported logs);
  // this admission boundary is where policy violations become errors.
  [[nodiscard]] static std::expected<void, Error> AdmitBatch(
      ProjectionStore& store, std::span<const AdmissionEvent> events);
  [[nodiscard]] static std::expected<BeliefRebuildProgress, Error> Rebuild(
      ProjectionStore& store, std::span<const log::Frame> frames, bool reset,
      std::size_t maximum_frames = static_cast<std::size_t>(-1));
  [[nodiscard]] static std::expected<std::optional<BeliefRecord>, Error> ReadAsOf(
      const ReadSnapshot& snapshot, schema::BeliefType type,
      std::string_view canonical_identity, BeliefAsOf as_of);
  [[nodiscard]] static std::expected<std::vector<BeliefRecord>, Error> ReadHeads(
      const ReadSnapshot& snapshot, std::size_t limit = 4096);
};

}  // namespace neocortex::proj
