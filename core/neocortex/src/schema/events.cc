#include "schema/events.h"

#include <array>
#include <limits>

#include <flatbuffers/flatbuffers.h>

namespace neocortex::events {
namespace {

constexpr std::array<std::string_view, 21> kKindNames = {
    "user_msg",       "delivered_msg", "tool_call",     "tool_result",
    "reasoning",      "provider_frame","media_ref",     "effect",
    "approval",       "outcome",       "checkpoint",    "supervisor",
    "recovery",       "intent_set",    "loop_opened",   "loop_closed",
    "assertion",      "consolidation", "embedding",     "retract",
    "attestation",
};

template <typename Enum>
struct EnumBounds final {
  Enum minimum;
  Enum maximum;
};

template <typename Enum>
bool EnumInRange(Enum value, EnumBounds<Enum> bounds) {
  const auto raw = static_cast<std::uint8_t>(value);
  return raw >= static_cast<std::uint8_t>(bounds.minimum) &&
         raw <= static_cast<std::uint8_t>(bounds.maximum);
}

template <typename Vector>
bool PresentAndNonempty(const Vector* value) {
  return value != nullptr && !value->empty();
}

template <typename Vector>
bool BoundedIdentifier(const Vector* value) {
  return PresentAndNonempty(value) &&
         value->size() <= kMaximumIdentifierBytes;
}

bool BoundedIdentifier(const flatbuffers::String* value) {
  return value != nullptr && !value->string_view().empty() &&
         value->string_view().size() <= kMaximumIdentifierBytes;
}

bool ValidProvenance(
    const flatbuffers::Vector<const schema::ProvenanceRange*>* provenance) {
  if (!PresentAndNonempty(provenance)) {
    return false;
  }
  for (const auto* range : *provenance) {
    if (range == nullptr || range->first_lsn() == 0 ||
        range->last_lsn() < range->first_lsn()) {
      return false;
    }
  }
  return true;
}

bool ValidAssertion(const schema::Assertion* assertion) {
  return assertion != nullptr && BoundedIdentifier(assertion->belief_id()) &&
         BoundedIdentifier(assertion->canonical_identity()) &&
         assertion->value() != nullptr &&
         EnumInRange(assertion->belief_type(),
                     EnumBounds{schema::BeliefType::fact,
                                schema::BeliefType::identity}) &&
         EnumInRange(assertion->claim(),
                     EnumBounds{schema::AssertionClaim::affirmative,
                                schema::AssertionClaim::negative_existence}) &&
         (assertion->conflict_domain() == nullptr ||
          BoundedIdentifier(assertion->conflict_domain())) &&
         (assertion->valid_to_ns() == 0 ||
          assertion->valid_to_ns() >= assertion->valid_from_ns()) &&
         ValidProvenance(assertion->provenance());
}

std::expected<void, Error> ValidateSemantics(const VerifiedEvent& event) {
  const auto* envelope = event.envelope;
  switch (event.kind) {
    case log::EventKind::kUserMsg:
      return envelope->payload_as_UserMsg()->content() != nullptr
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
    case log::EventKind::kDeliveredMsg:
      return envelope->payload_as_DeliveredMsg()->content() != nullptr
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
    case log::EventKind::kToolCall: {
      const auto* value = envelope->payload_as_ToolCall();
      if (!BoundedIdentifier(value->call_id()) || value->tool_name() == nullptr ||
          value->tool_name()->string_view().empty() || value->arguments() == nullptr) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kToolResult: {
      const auto* value = envelope->payload_as_ToolResult();
      if (!BoundedIdentifier(value->call_id()) || value->tool_call_lsn() == 0 ||
          value->result() == nullptr ||
          !EnumInRange(value->status(),
                       EnumBounds{schema::ResultStatus::ok,
                                  schema::ResultStatus::outcome_unknown})) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kReasoning:
      return envelope->payload_as_Reasoning()->content() != nullptr
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
    case log::EventKind::kProviderFrame: {
      const auto* value = envelope->payload_as_ProviderFrame();
      if (value->provider() == nullptr ||
          value->provider()->string_view().empty() || value->api_content() == nullptr) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kMediaRef: {
      const auto* value = envelope->payload_as_MediaRef();
      if (value->uri() == nullptr || value->uri()->string_view().empty() ||
          value->media_type() == nullptr ||
          value->media_type()->string_view().empty() ||
          !PresentAndNonempty(value->digest())) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kEffect: {
      const auto* value = envelope->payload_as_Effect();
      if (!BoundedIdentifier(value->effect_id()) || value->tool_call_lsn() == 0 ||
          !EnumInRange(value->state(),
                       EnumBounds{schema::EffectState::dispatched,
                                  schema::EffectState::outcome_unknown})) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kApproval: {
      const auto* value = envelope->payload_as_Approval();
      if (!BoundedIdentifier(value->effect_id()) ||
          !EnumInRange(value->decision(),
                       EnumBounds{schema::ApprovalDecision::approved,
                                  schema::ApprovalDecision::denied})) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kOutcome: {
      const auto* value = envelope->payload_as_Outcome();
      if (!BoundedIdentifier(value->effect_id()) || value->detail() == nullptr ||
          !EnumInRange(value->status(),
                       EnumBounds{schema::ResultStatus::ok,
                                  schema::ResultStatus::outcome_unknown})) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kCheckpoint:
      return PresentAndNonempty(envelope->payload_as_Checkpoint()->cursor())
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
    case log::EventKind::kSupervisor: {
      const auto* value = envelope->payload_as_Supervisor();
      if (value->code() == nullptr || value->code()->string_view().empty() ||
          value->evidence() == nullptr) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kRecovery: {
      const auto* value = envelope->payload_as_Recovery();
      if (value->code() == nullptr || value->code()->string_view().empty() ||
          value->target_lsn() == 0) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kIntentSet:
      return PresentAndNonempty(envelope->payload_as_IntentSet()->objective())
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
    case log::EventKind::kLoopOpened: {
      const auto* value = envelope->payload_as_LoopOpened();
      if (!BoundedIdentifier(value->loop_id()) ||
          !PresentAndNonempty(value->objective())) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kLoopClosed: {
      const auto* value = envelope->payload_as_LoopClosed();
      if (!BoundedIdentifier(value->loop_id()) || value->cause() == nullptr ||
          !EnumInRange(value->reason(),
                       EnumBounds{schema::LoopCloseReason::done,
                                  schema::LoopCloseReason::superseded})) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kAssertion:
      return ValidAssertion(envelope->payload_as_Assertion())
                 ? std::expected<void, Error>{}
                 : std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
    case log::EventKind::kConsolidation: {
      const auto* assertions = envelope->payload_as_Consolidation()->assertions();
      if (!PresentAndNonempty(assertions)) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      for (const auto* assertion : *assertions) {
        if (!ValidAssertion(assertion)) {
          return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
        }
      }
      return {};
    }
    case log::EventKind::kEmbedding: {
      const auto* value = envelope->payload_as_Embedding();
      const auto dimension = static_cast<std::size_t>(value->dimension());
      if (value->target_lsn() == 0 || dimension == 0 ||
          dimension > kMaximumEmbeddingDimension ||
          value->quantized() == nullptr || value->quantized()->size() != dimension ||
          value->binary_prefilter() == nullptr ||
          value->binary_prefilter()->size() != (dimension + 7U) / 8U) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kRetract: {
      const auto* value = envelope->payload_as_Retract();
      if (!BoundedIdentifier(value->belief_id()) ||
          !ValidProvenance(value->provenance())) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
    case log::EventKind::kAttestation: {
      const auto* value = envelope->payload_as_Attestation();
      if (value->target_lsn() == 0 ||
          !EnumInRange(value->disposition(),
                       EnumBounds{schema::AttestationDisposition::used,
                                  schema::AttestationDisposition::ignored})) {
        return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
      }
      return {};
    }
  }
  return std::unexpected(Error{ErrorCode::kForbiddenKind, 0});
}

}  // namespace

std::expected<VerifiedEvent, Error> VerifyEvent(
    std::span<const std::byte> encoded, log::EventKind expected_kind,
    Boundary boundary) {
  if (encoded.empty() || encoded.size() > log::kDefaultMaximumFrameBytes ||
      !log::IsEventKind(static_cast<std::uint8_t>(expected_kind))) {
    return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
  }
  flatbuffers::Verifier verifier(
      reinterpret_cast<const std::uint8_t*>(encoded.data()), encoded.size(), 64, 4096);
  if (!schema::VerifyEventEnvelopeBuffer(verifier)) {
    return std::unexpected(Error{ErrorCode::kSchemaInvalid, 0});
  }
  const auto* envelope = schema::GetEventEnvelope(encoded.data());
  if (envelope->schema_version() == 0 ||
      envelope->schema_version() > kCurrentSchemaVersion) {
    return std::unexpected(Error{ErrorCode::kSchemaVersion, 0});
  }
  const auto raw_kind = static_cast<std::uint8_t>(envelope->payload_type());
  if (!log::IsEventKind(raw_kind) ||
      raw_kind != static_cast<std::uint8_t>(expected_kind) ||
      envelope->payload() == nullptr) {
    return std::unexpected(Error{ErrorCode::kForbiddenKind, 0});
  }
  VerifiedEvent event{.envelope = envelope,
                      .kind = expected_kind,
                      .boundary = boundary};
  auto valid = ValidateSemantics(event);
  if (!valid) {
    return std::unexpected(valid.error());
  }
  return event;
}

std::expected<log::EventKind, Error> ParseEventKindName(std::string_view name) {
  for (std::size_t index = 0; index < kKindNames.size(); ++index) {
    if (name == kKindNames[index]) {
      return static_cast<log::EventKind>(index + 1U);
    }
  }
  return std::unexpected(Error{ErrorCode::kForbiddenKind, 0});
}

std::string_view EventKindName(log::EventKind kind) {
  const auto raw = static_cast<std::uint8_t>(kind);
  return log::IsEventKind(raw) ? kKindNames[static_cast<std::size_t>(raw - 1U)]
                               : std::string_view{};
}

std::expected<log::EventKind, Error> OrderingState::KindAt(
    std::uint64_t lsn, std::span<const log::Frame> batch,
    std::size_t current_index, KindLookup lookup) const {
  if (lsn == 0) {
    return std::unexpected(Error{ErrorCode::kOrderingViolation, 0});
  }
  if (lsn <= applied_lsn_) {
    if (lookup.read == nullptr) {
      return std::unexpected(Error{ErrorCode::kOrderingViolation, 0, lsn});
    }
    return lookup.read(lookup.context, lsn);
  }
  const auto base_lsn = applied_lsn_ + 1U;
  if (lsn < base_lsn) {
    return std::unexpected(Error{ErrorCode::kOrderingViolation, 0, lsn});
  }
  const auto batch_index = static_cast<std::size_t>(lsn - base_lsn);
  if (batch_index >= current_index || batch_index >= batch.size()) {
    return std::unexpected(Error{ErrorCode::kOrderingViolation, 0, lsn});
  }
  return batch[batch_index].header.kind;
}

std::expected<void, Error> OrderingState::ValidateBatch(
    std::span<const log::Frame> frames, Boundary boundary,
    KindLookup lookup) const {
  if (frames.empty()) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  if (applied_lsn_ == std::numeric_limits<std::uint64_t>::max()) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
  }
  const auto first_lsn = applied_lsn_ + 1U;
  for (std::size_t index = 0; index < frames.size(); ++index) {
    const auto& frame = frames[index];
    if (frame.header.lsn != first_lsn + index) {
      return std::unexpected(Error{ErrorCode::kSequenceViolation, 0,
                                   frame.header.lsn, index});
    }
    auto event = VerifyEvent(frame.sealed_payload, frame.header.kind, boundary);
    if (!event) {
      auto error = event.error();
      error.lsn = frame.header.lsn;
      return std::unexpected(error);
    }
    std::uint64_t referenced_tool_call = 0;
    if (frame.header.kind == log::EventKind::kToolResult) {
      referenced_tool_call =
          event->envelope->payload_as_ToolResult()->tool_call_lsn();
    } else if (frame.header.kind == log::EventKind::kEffect) {
      referenced_tool_call = event->envelope->payload_as_Effect()->tool_call_lsn();
    }
    if (referenced_tool_call != 0) {
      auto prior_kind = KindAt(referenced_tool_call, frames, index, lookup);
      if (!prior_kind || *prior_kind != log::EventKind::kToolCall) {
        return std::unexpected(Error{ErrorCode::kOrderingViolation, 0,
                                     frame.header.lsn, referenced_tool_call});
      }
    }
  }
  return {};
}

std::expected<void, Error> OrderingState::RecordBatch(
    std::span<const log::Frame> frames) {
  if (frames.empty() ||
      frames.front().header.lsn != applied_lsn_ + 1U ||
      frames.size() > std::numeric_limits<std::uint64_t>::max() - applied_lsn_) {
    return std::unexpected(Error{ErrorCode::kSequenceViolation, 0});
  }
  const auto first_lsn = frames.front().header.lsn;
  for (std::size_t index = 0; index < frames.size(); ++index) {
    if (frames[index].header.lsn != first_lsn + index) {
      return std::unexpected(Error{ErrorCode::kSequenceViolation, 0,
                                   frames[index].header.lsn, index});
    }
  }
  applied_lsn_ += static_cast<std::uint64_t>(frames.size());
  return {};
}

}  // namespace neocortex::events
