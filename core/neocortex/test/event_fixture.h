#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <span>
#include <vector>

#include <flatbuffers/flatbuffers.h>

#include "log/frame.h"
#include "schema/events_generated.h"

namespace neocortex::test {

struct EventIdempotency final {
  std::span<const std::byte> connection_id{};
  std::uint64_t client_seq = 0;
  std::uint32_t event_index = 0;
  std::uint32_t event_count = 0;
};

inline std::vector<std::byte> BuildEvent(log::EventKind kind, std::uint64_t lsn,
                                         std::span<const std::byte> content,
                                         EventIdempotency idempotency = {}) {
  flatbuffers::FlatBufferBuilder builder(512);
  const auto bytes = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(content.data()), content.size());
  const std::uint8_t identifier_value =
      static_cast<std::uint8_t>((lsn % 251U) + 1U);
  const auto identifier = builder.CreateVector(&identifier_value, 1);
  const auto text = builder.CreateString("canonical");
  const schema::ProvenanceRange provenance_range(1, lsn);
  const auto provenance = builder.CreateVectorOfStructs(&provenance_range, 1);

  schema::EventPayload payload_type = schema::EventPayload::NONE;
  flatbuffers::Offset<void> payload;
  switch (kind) {
    case log::EventKind::kUserMsg:
      payload_type = schema::EventPayload::UserMsg;
      payload = schema::CreateUserMsg(builder, bytes).Union();
      break;
    case log::EventKind::kDeliveredMsg:
      payload_type = schema::EventPayload::DeliveredMsg;
      payload = schema::CreateDeliveredMsg(builder, bytes).Union();
      break;
    case log::EventKind::kToolCall:
      payload_type = schema::EventPayload::ToolCall;
      payload = schema::CreateToolCall(builder, identifier, text, bytes).Union();
      break;
    case log::EventKind::kToolResult:
      payload_type = schema::EventPayload::ToolResult;
      payload = schema::CreateToolResult(builder, identifier, lsn - 1U,
                                         schema::ResultStatus::ok, bytes)
                    .Union();
      break;
    case log::EventKind::kReasoning:
      payload_type = schema::EventPayload::Reasoning;
      payload = schema::CreateReasoning(builder, bytes).Union();
      break;
    case log::EventKind::kProviderFrame:
      payload_type = schema::EventPayload::ProviderFrame;
      payload = schema::CreateProviderFrame(builder, text, bytes).Union();
      break;
    case log::EventKind::kMediaRef:
      payload_type = schema::EventPayload::MediaRef;
      payload = schema::CreateMediaRef(builder, text, text, identifier).Union();
      break;
    case log::EventKind::kEffect:
      payload_type = schema::EventPayload::Effect;
      payload = schema::CreateEffect(builder, identifier, lsn - 5U,
                                     schema::EffectState::dispatched)
                    .Union();
      break;
    case log::EventKind::kApproval:
      payload_type = schema::EventPayload::Approval;
      payload = schema::CreateApproval(builder, identifier,
                                       schema::ApprovalDecision::approved)
                    .Union();
      break;
    case log::EventKind::kOutcome:
      payload_type = schema::EventPayload::Outcome;
      payload = schema::CreateOutcome(builder, identifier, schema::ResultStatus::ok,
                                      bytes)
                    .Union();
      break;
    case log::EventKind::kCheckpoint:
      payload_type = schema::EventPayload::Checkpoint;
      payload = schema::CreateCheckpoint(builder, identifier).Union();
      break;
    case log::EventKind::kSupervisor:
      payload_type = schema::EventPayload::Supervisor;
      payload = schema::CreateSupervisor(builder, text, bytes).Union();
      break;
    case log::EventKind::kRecovery:
      payload_type = schema::EventPayload::Recovery;
      payload = schema::CreateRecovery(builder, text, 1).Union();
      break;
    case log::EventKind::kIntentSet:
      payload_type = schema::EventPayload::IntentSet;
      payload = schema::CreateIntentSet(builder, identifier).Union();
      break;
    case log::EventKind::kLoopOpened:
      payload_type = schema::EventPayload::LoopOpened;
      payload = schema::CreateLoopOpened(builder, identifier, bytes).Union();
      break;
    case log::EventKind::kLoopClosed:
      payload_type = schema::EventPayload::LoopClosed;
      payload = schema::CreateLoopClosed(builder, identifier,
                                         schema::LoopCloseReason::done, bytes)
                    .Union();
      break;
    case log::EventKind::kAssertion:
      payload_type = schema::EventPayload::Assertion;
      payload = schema::CreateAssertion(builder, identifier, schema::BeliefType::fact,
                                        text, bytes, 1, 0, provenance)
                    .Union();
      break;
    case log::EventKind::kConsolidation: {
      payload_type = schema::EventPayload::Consolidation;
      const auto assertion = schema::CreateAssertion(
          builder, identifier, schema::BeliefType::fact, text, bytes, 1, 0,
          provenance);
      const auto assertions = builder.CreateVector(&assertion, 1);
      payload = schema::CreateConsolidation(builder, assertions).Union();
      break;
    }
    case log::EventKind::kEmbedding: {
      payload_type = schema::EventPayload::Embedding;
      const std::array<std::int8_t, 8> quantized = {1, -2, 3, -4, 5, -6, 7, -8};
      const std::uint8_t binary_value = 0xa5;
      const auto vector = builder.CreateVector(quantized.data(), quantized.size());
      const auto binary = builder.CreateVector(&binary_value, 1);
      payload = schema::CreateEmbedding(builder, 1, 8, vector, binary).Union();
      break;
    }
    case log::EventKind::kRetract:
      payload_type = schema::EventPayload::Retract;
      payload = schema::CreateRetract(builder, identifier, provenance).Union();
      break;
    case log::EventKind::kAttestation:
      payload_type = schema::EventPayload::Attestation;
      payload = schema::CreateAttestation(builder, 1,
                                          schema::AttestationDisposition::used)
                    .Union();
      break;
  }
  const auto connection_id = idempotency.connection_id.empty()
                                 ? flatbuffers::Offset<flatbuffers::Vector<std::uint8_t>>{}
                                 : builder.CreateVector(
                                       reinterpret_cast<const std::uint8_t*>(
                                           idempotency.connection_id.data()),
                                       idempotency.connection_id.size());
  const auto envelope = schema::CreateEventEnvelope(
      builder, 1, payload_type, payload, connection_id,
      idempotency.client_seq, idempotency.event_index,
      idempotency.event_count);
  schema::FinishEventEnvelopeBuffer(builder, envelope);
  return {reinterpret_cast<const std::byte*>(builder.GetBufferPointer()),
          reinterpret_cast<const std::byte*>(builder.GetBufferPointer()) +
              builder.GetSize()};
}

}  // namespace neocortex::test
