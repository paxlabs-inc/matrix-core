#include <array>
#include <bit>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <span>
#include <string_view>
#include <vector>

#include <flatbuffers/flatbuffers.h>

#include "event_fixture.h"
#include "mmr/mmr.h"
#include "schema/events.h"

namespace {

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

neocortex::log::Frame MakeFrame(neocortex::log::EventKind kind,
                                std::uint64_t lsn,
                                std::vector<std::byte> payload) {
  return neocortex::log::Frame{
      .header = neocortex::log::FrameHeader{
          .lsn = lsn,
          .kind = kind,
          .wall_timestamp_ns = static_cast<std::int64_t>(lsn),
          .actor = 1,
          .conversation = {},
      },
      .sealed_payload = std::move(payload),
  };
}

struct ToolResultOptions final {
  std::uint64_t referenced_lsn;
  std::uint16_t schema_version = 1;
};

std::vector<std::byte> BuildToolResult(ToolResultOptions options) {
  flatbuffers::FlatBufferBuilder builder(128);
  const std::uint8_t byte = 0x41;
  const auto data = builder.CreateVector(&byte, 1);
  const auto result = neocortex::schema::CreateToolResult(
      builder, data, options.referenced_lsn,
      neocortex::schema::ResultStatus::ok, data);
  const auto envelope = neocortex::schema::CreateEventEnvelope(
      builder, options.schema_version,
      neocortex::schema::EventPayload::ToolResult,
      result.Union());
  neocortex::schema::FinishEventEnvelopeBuffer(builder, envelope);
  return {reinterpret_cast<const std::byte*>(builder.GetBufferPointer()),
          reinterpret_cast<const std::byte*>(builder.GetBufferPointer()) +
              builder.GetSize()};
}

std::vector<std::byte> BuildUnknownKind() {
  flatbuffers::FlatBufferBuilder builder(128);
  const std::uint8_t byte = 0x41;
  const auto data = builder.CreateVector(&byte, 1);
  const auto message = neocortex::schema::CreateUserMsg(builder, data);
  const auto envelope = neocortex::schema::CreateEventEnvelope(
      builder, 1,
      std::bit_cast<neocortex::schema::EventPayload>(std::uint8_t{22}),
      message.Union());
  neocortex::schema::FinishEventEnvelopeBuffer(builder, envelope);
  return {reinterpret_cast<const std::byte*>(builder.GetBufferPointer()),
          reinterpret_cast<const std::byte*>(builder.GetBufferPointer()) +
              builder.GetSize()};
}

int TestTaxonomyAndVerifier(std::vector<std::byte>& golden_material) {
  constexpr std::array<std::byte, 7> content = {
      std::byte{0x00}, std::byte{0x11}, std::byte{0x22}, std::byte{0x33},
      std::byte{0x44}, std::byte{0x55}, std::byte{0xff}};
  for (std::uint8_t raw_kind = 1; raw_kind <= 21; ++raw_kind) {
    const auto kind = static_cast<neocortex::log::EventKind>(raw_kind);
    const auto encoded = neocortex::test::BuildEvent(kind, raw_kind, content);
    for (const auto boundary : {neocortex::events::Boundary::kDisk,
                                neocortex::events::Boundary::kSocket,
                                neocortex::events::Boundary::kImport}) {
      auto verified = neocortex::events::VerifyEvent(encoded, kind, boundary);
      if (!verified || verified->kind != kind || verified->boundary != boundary) {
        return Fail("valid event failed a trust boundary");
      }
    }
    const auto wrong_kind = static_cast<neocortex::log::EventKind>(
        raw_kind == 21 ? 1 : raw_kind + 1U);
    if (neocortex::events::VerifyEvent(encoded, wrong_kind,
                                       neocortex::events::Boundary::kSocket)) {
      return Fail("frame kind and payload union mismatch was accepted");
    }
    auto parsed = neocortex::events::ParseEventKindName(
        neocortex::events::EventKindName(kind));
    if (!parsed || *parsed != kind) {
      return Fail("frozen kind name did not round trip");
    }
    for (std::uint32_t shift = 0; shift < 64; shift += 8) {
      golden_material.push_back(
          static_cast<std::byte>((encoded.size() >> shift) & 0xffU));
    }
    golden_material.insert(golden_material.end(), encoded.begin(), encoded.end());
  }

  constexpr std::array forbidden = {
      std::string_view("guidance"), std::string_view("doubt"),
      std::string_view("steering"), std::string_view("rejected_answer"),
      std::string_view("narrative_respawn_summary")};
  for (const auto name : forbidden) {
    auto parsed = neocortex::events::ParseEventKindName(name);
    if (parsed || parsed.error().code != neocortex::ErrorCode::kForbiddenKind) {
      return Fail("forbidden cognition label became representable");
    }
  }

  const auto unknown = BuildUnknownKind();
  auto unknown_result = neocortex::events::VerifyEvent(
      unknown, neocortex::log::EventKind::kUserMsg,
      neocortex::events::Boundary::kImport);
  if (unknown_result ||
      unknown_result.error().code != neocortex::ErrorCode::kForbiddenKind) {
    return Fail("unknown union variant was accepted");
  }
  const auto future = BuildToolResult(
      ToolResultOptions{.referenced_lsn = 3, .schema_version = 2});
  auto future_result = neocortex::events::VerifyEvent(
      future, neocortex::log::EventKind::kToolResult,
      neocortex::events::Boundary::kSocket);
  if (future_result ||
      future_result.error().code != neocortex::ErrorCode::kSchemaVersion) {
    return Fail("future schema version was trusted");
  }
  return 0;
}

int TestOrdering() {
  constexpr std::array<std::byte, 1> content = {std::byte{0x31}};
  std::vector<neocortex::log::Frame> frames;
  for (std::uint64_t lsn = 1; lsn <= 21; ++lsn) {
    const auto kind = static_cast<neocortex::log::EventKind>(lsn);
    frames.push_back(MakeFrame(
        kind, lsn, neocortex::test::BuildEvent(kind, lsn, content)));
  }
  neocortex::events::OrderingState ordering;
  auto valid = ordering.ValidateBatch(frames, neocortex::events::Boundary::kImport);
  if (!valid || !ordering.RecordBatch(frames) || ordering.applied_lsn() != 21) {
    return Fail("valid write-ahead stream was rejected");
  }

  const neocortex::events::OrderingState invalid_ordering;
  std::vector<neocortex::log::Frame> invalid;
  invalid.push_back(MakeFrame(
      neocortex::log::EventKind::kUserMsg, 1,
      neocortex::test::BuildEvent(neocortex::log::EventKind::kUserMsg, 1,
                                  content)));
  invalid.push_back(MakeFrame(neocortex::log::EventKind::kToolResult, 2,
                              BuildToolResult(
                                  ToolResultOptions{.referenced_lsn = 1})));
  auto rejected = invalid_ordering.ValidateBatch(
      invalid, neocortex::events::Boundary::kSocket);
  if (rejected ||
      rejected.error().code != neocortex::ErrorCode::kOrderingViolation) {
    return Fail("tool result without a durable tool call was accepted");
  }
  return 0;
}

int TestGolden(const std::vector<std::byte>& material) {
  constexpr neocortex::mmr::Hash kExpected = {
      std::byte{0x1e}, std::byte{0x3f}, std::byte{0xd0}, std::byte{0xfd},
      std::byte{0xec}, std::byte{0x0e}, std::byte{0x34}, std::byte{0x5f},
      std::byte{0x38}, std::byte{0xd5}, std::byte{0xec}, std::byte{0x06},
      std::byte{0x65}, std::byte{0x10}, std::byte{0x62}, std::byte{0x4f},
      std::byte{0x66}, std::byte{0x51}, std::byte{0x66}, std::byte{0x4a},
      std::byte{0x4b}, std::byte{0x9b}, std::byte{0xf8}, std::byte{0x05},
      std::byte{0x82}, std::byte{0x47}, std::byte{0x81}, std::byte{0x55},
      std::byte{0x33}, std::byte{0x73}, std::byte{0x60}, std::byte{0x32}};
  const auto actual = neocortex::mmr::HashBytes(material);
  if (actual != kExpected) {
    for (const auto value : actual) {
      std::fprintf(stderr, "%02x", std::to_integer<unsigned int>(value));
    }
    std::fputc('\n', stderr);
    return Fail("canonical event encoding changed");
  }
  return 0;
}

}  // namespace

int main() {
  static_assert(static_cast<std::uint8_t>(neocortex::schema::EventPayload::UserMsg) ==
                static_cast<std::uint8_t>(neocortex::log::EventKind::kUserMsg));
  static_assert(
      static_cast<std::uint8_t>(neocortex::schema::EventPayload::Attestation) ==
      static_cast<std::uint8_t>(neocortex::log::EventKind::kAttestation));
  std::vector<std::byte> golden_material;
  if (TestTaxonomyAndVerifier(golden_material) != 0 || TestOrdering() != 0 ||
      TestGolden(golden_material) != 0) {
    return 1;
  }
  return 0;
}
