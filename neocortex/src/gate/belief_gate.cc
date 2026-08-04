#include "gate/belief_gate.h"

namespace neocortex::gate {

std::expected<BeliefGateDecision, Error> EvaluateBelief(
    const schema::Assertion& assertion, bool has_corroborating_tool_result,
    std::span<const ExistingBelief> existing, std::uint64_t transaction_lsn) {
  if (assertion.claim() == schema::AssertionClaim::negative_existence &&
      !has_corroborating_tool_result) {
    return std::unexpected(Error{ErrorCode::kNegativeExistenceUncorroborated, 0,
                                 transaction_lsn});
  }
  BeliefGateDecision decision;
  const auto* domain = assertion.conflict_domain();
  if (domain == nullptr) {
    return decision;
  }
  const auto domain_name = domain->string_view();
  for (std::size_t index = 0; index < existing.size(); ++index) {
    const auto& candidate = existing[index];
    if (!candidate.tombstoned && candidate.type == assertion.belief_type() &&
        candidate.conflict_domain == domain_name &&
        candidate.canonical_identity !=
            assertion.canonical_identity()->string_view()) {
      decision.conflicting_existing.push_back(index);
    }
  }
  return decision;
}

}  // namespace neocortex::gate
