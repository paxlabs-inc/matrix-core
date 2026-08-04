#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <expected>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include "core/error.h"
#include "log/frame.h"
#include "proj/store.h"
#include "proj/vectors/vector_lane.h"
#include "schema/events_generated.h"

namespace neocortex::compose {

enum class Tier : std::uint8_t {
  kResident = 0,
  kIntent = 1,
  kWorkLedger = 2,
  kConversation = 3,
  kEntity = 4,
  kConflicts = 5,
  kFused = 6,
  kTemporal = 7,
};

struct TokenModel final {
  void* context;
  std::expected<std::size_t, Error> (*count)(
      void* context, Tier tier, std::string_view uri,
      std::span<const std::uint64_t> provenance,
      std::span<const std::byte> content);
};

struct ActivationRequest final {
  log::ConversationId conversation;
  std::string_view query;
  std::string_view turn_text;
  std::size_t budget_tokens;
  TokenModel token_model;
  std::span<const std::int8_t> query_embedding;
  std::span<const std::byte> query_binary_prefilter;
  std::int64_t temporal_from_ns;
  std::int64_t temporal_to_ns;
  std::size_t maximum_candidates = 256;
  std::size_t maximum_conversation_records = 4096;
};

struct ActivationItem final {
  Tier tier;
  std::string uri;
  std::vector<std::uint64_t> provenance;
  std::vector<std::byte> content;
  std::size_t tokens;
  bool coarsened;

  bool operator==(const ActivationItem&) const = default;
};

struct ActivationSection final {
  Tier tier;
  std::vector<ActivationItem> items;
  std::size_t tokens;
  std::size_t trimmed_items;
  std::size_t coarsened_items;

  bool operator==(const ActivationSection&) const = default;
};

struct TrimmedItem final {
  Tier tier;
  std::string uri;

  bool operator==(const TrimmedItem&) const = default;
};

struct ActivationBundle final {
  std::uint64_t snapshot_epoch;
  std::size_t budget_tokens;
  std::size_t spent_tokens;
  std::array<ActivationSection, 8> sections;
  std::vector<TrimmedItem> trimmed;

  bool operator==(const ActivationBundle&) const = default;
};

struct AttestationRequest final {
  std::uint64_t first_lsn;
  std::uint16_t actor;
  log::ConversationId conversation;
  std::int64_t wall_timestamp_ns;
  schema::AttestationDisposition disposition;
};

class Composer final {
 public:
  [[nodiscard]] static std::expected<ActivationBundle, Error> Activate(
      const proj::ReadSnapshot& snapshot, const ActivationRequest& request,
      const proj::VectorLane* vector_lane = nullptr);

  [[nodiscard]] static std::expected<std::vector<std::byte>, Error>
  CanonicalBytes(const ActivationBundle& bundle);

  [[nodiscard]] static std::expected<std::vector<log::Frame>, Error>
  BuildAttestations(const ActivationBundle& bundle,
                    const AttestationRequest& request);
};

}  // namespace neocortex::compose
