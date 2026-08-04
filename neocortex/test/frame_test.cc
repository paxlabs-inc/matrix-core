#include <array>
#include <cstddef>
#include <cstdio>
#include <span>
#include <string_view>

#include "log/frame.h"

namespace {

std::span<const std::byte> Bytes(std::string_view value) {
  return std::as_bytes(std::span(value));
}

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

}  // namespace

int main() {
  neocortex::log::ConversationId conversation{};
  conversation.bytes[0] = std::byte{0x31};
  for (std::uint8_t raw_kind = 1; raw_kind <= 21; ++raw_kind) {
    const neocortex::log::Frame input{
        .header = neocortex::log::FrameHeader{
            .lsn = raw_kind,
            .kind = static_cast<neocortex::log::EventKind>(raw_kind),
            .wall_timestamp_ns = -123456789 + raw_kind,
            .actor = 7,
            .conversation = conversation,
        },
        .sealed_payload = std::vector<std::byte>(Bytes("sealed-payload").begin(),
                                                 Bytes("sealed-payload").end()),
    };
    auto encoded = neocortex::log::EncodeFrame(input);
    if (!encoded) {
      return Fail("encode failed");
    }
    auto decoded = neocortex::log::DecodeFrame(*encoded);
    if (!decoded || *decoded != input) {
      return Fail("round trip failed");
    }
    (*encoded)[neocortex::log::kFrameHeaderSize] ^= std::byte{0x80};
    auto corrupt = neocortex::log::DecodeFrame(*encoded);
    if (corrupt || corrupt.error().code != neocortex::ErrorCode::kChecksumMismatch) {
      return Fail("checksum corruption accepted");
    }
  }

  std::array<std::byte, 3> short_prefix{};
  auto truncated = neocortex::log::DecodeFrame(short_prefix);
  if (truncated || truncated.error().code != neocortex::ErrorCode::kTruncated) {
    return Fail("truncated header accepted");
  }
  return 0;
}
