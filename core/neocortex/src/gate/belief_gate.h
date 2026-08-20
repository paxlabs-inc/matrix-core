#pragma once

#include <cstddef>
#include <cstdint>
#include <expected>
#include <span>
#include <string_view>
#include <vector>

#include "core/error.h"
#include "schema/events_generated.h"

namespace neocortex::gate {

struct ExistingBelief final {
  schema::BeliefType type;
  std::string_view canonical_identity;
  std::string_view conflict_domain;
  bool tombstoned;
};

struct BeliefGateDecision final {
  std::vector<std::size_t> conflicting_existing;
};

[[nodiscard]] std::expected<BeliefGateDecision, Error> EvaluateBelief(
    const schema::Assertion& assertion, bool has_corroborating_tool_result,
    std::span<const ExistingBelief> existing, std::uint64_t transaction_lsn);

}  // namespace neocortex::gate
