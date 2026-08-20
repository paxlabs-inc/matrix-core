#include "serve/config.h"

#include <algorithm>
#include <cerrno>
#include <charconv>
#include <fstream>
#include <limits>
#include <optional>
#include <string>
#include <string_view>
#include <sys/stat.h>

namespace neocortex::serve {
namespace {

int HexValue(char value) {
  if (value >= '0' && value <= '9') {
    return value - '0';
  }
  if (value >= 'a' && value <= 'f') {
    return value - 'a' + 10;
  }
  if (value >= 'A' && value <= 'F') {
    return value - 'A' + 10;
  }
  return -1;
}

template <std::size_t Size>
std::expected<std::array<std::byte, Size>, Error> DecodeHex(
    std::string_view encoded) {
  if (encoded.size() != Size * 2U) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  std::array<std::byte, Size> decoded{};
  for (std::size_t index = 0; index < Size; ++index) {
    const int high = HexValue(encoded[index * 2U]);
    const int low = HexValue(encoded[index * 2U + 1U]);
    if (high < 0 || low < 0) {
      return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
    }
    decoded[index] = static_cast<std::byte>((high << 4U) | low);
  }
  return decoded;
}

std::expected<std::size_t, Error> ParseSize(std::string_view value,
                                            std::size_t minimum,
                                            std::size_t maximum) {
  std::size_t parsed = 0;
  const auto result =
      std::from_chars(value.data(), value.data() + value.size(), parsed);
  if (result.ec != std::errc{} || result.ptr != value.data() + value.size() ||
      parsed < minimum || parsed > maximum) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  return parsed;
}

}  // namespace

std::expected<ServerConfig, Error> LoadServerConfig(
    const std::filesystem::path& path) {
  struct stat metadata {};
  if (::stat(path.c_str(), &metadata) != 0) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  if (!S_ISREG(metadata.st_mode) || (metadata.st_mode & 0077) != 0) {
    return std::unexpected(Error{ErrorCode::kCapabilityDenied, 0});
  }
  std::ifstream input(path);
  if (!input) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }

  ServerConfig config{};
  std::optional<mmr::SigningSeed> signing_seed;
  bool has_user = false;
  bool has_kek = false;
  bool has_admin = false;
  std::string line;
  while (std::getline(input, line)) {
    if (line.empty()) {
      continue;
    }
    const auto separator = line.find('=');
    if (separator == std::string::npos || separator == 0 ||
        separator + 1U == line.size()) {
      return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
    }
    const std::string_view key(line.data(), separator);
    const std::string_view value(line.data() + separator + 1U,
                                 line.size() - separator - 1U);
    if (key == "socket") {
      config.socket_path = value;
    } else if (key == "data") {
      config.data_directory = value;
    } else if (key == "user") {
      auto decoded = DecodeHex<16>(value);
      if (!decoded) return std::unexpected(decoded.error());
      config.user = *decoded;
      has_user = true;
    } else if (key == "kek") {
      auto decoded = DecodeHex<32>(value);
      if (!decoded) return std::unexpected(decoded.error());
      config.kek = *decoded;
      has_kek = true;
    } else if (key == "signing_seed") {
      auto decoded = DecodeHex<32>(value);
      if (!decoded) return std::unexpected(decoded.error());
      signing_seed = *decoded;
    } else if (key == "admin_token") {
      auto decoded = DecodeHex<32>(value);
      if (!decoded) return std::unexpected(decoded.error());
      config.admin_token = *decoded;
      has_admin = true;
    } else if (key == "actor") {
      const auto colon = value.find(':');
      if (colon == std::string_view::npos) {
        return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
      }
      unsigned int actor_value = 0;
      const auto actor_text = value.substr(0, colon);
      const auto parsed = std::from_chars(actor_text.data(),
                                          actor_text.data() + actor_text.size(),
                                          actor_value);
      auto token = DecodeHex<32>(value.substr(colon + 1U));
      if (parsed.ec != std::errc{} ||
          parsed.ptr != actor_text.data() + actor_text.size() ||
          actor_value == 0 ||
          actor_value > std::numeric_limits<std::uint16_t>::max() || !token) {
        return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
      }
      config.actors.push_back(ActorCapability{
          .actor = static_cast<std::uint16_t>(actor_value), .token = *token});
    } else if (key == "maximum_connections") {
      auto parsed = ParseSize(value, 1, 4096);
      if (!parsed) return std::unexpected(parsed.error());
      config.maximum_connections = *parsed;
    } else if (key == "maximum_output_frames") {
      auto parsed = ParseSize(value, 1, 4096);
      if (!parsed) return std::unexpected(parsed.error());
      config.maximum_output_frames = *parsed;
    } else if (key == "maximum_output_bytes") {
      auto parsed = ParseSize(value, static_cast<std::size_t>(1024) * 1024U,
                              static_cast<std::size_t>(1024) * 1024U * 1024U);
      if (!parsed) return std::unexpected(parsed.error());
      config.maximum_output_bytes = *parsed;
    } else if (key == "projection_map_bytes") {
      auto parsed = ParseSize(value, static_cast<std::size_t>(1024) * 1024U,
                              std::numeric_limits<std::size_t>::max());
      if (!parsed) return std::unexpected(parsed.error());
      config.projection_map_bytes = *parsed;
    } else {
      return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
    }
  }
  if (!input.eof() || config.socket_path.empty() ||
      config.data_directory.empty() || !has_user || !has_kek || !has_admin ||
      !signing_seed || config.actors.empty()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  const CapabilityToken zero_token{};
  if (CapabilityTokenEqual(config.admin_token, zero_token) ||
      std::any_of(config.actors.begin(), config.actors.end(),
                  [&zero_token](const ActorCapability& capability) {
                    return CapabilityTokenEqual(capability.token, zero_token);
                  })) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  std::sort(config.actors.begin(), config.actors.end(),
            [](const ActorCapability& left, const ActorCapability& right) {
              return left.actor < right.actor;
            });
  for (std::size_t index = 1; index < config.actors.size(); ++index) {
    if (config.actors[index - 1U].actor == config.actors[index].actor) {
      return std::unexpected(Error{ErrorCode::kAlreadyExists, 0});
    }
  }
  for (std::size_t index = 0; index < config.actors.size(); ++index) {
    if (CapabilityTokenEqual(config.admin_token, config.actors[index].token)) {
      return std::unexpected(Error{ErrorCode::kAlreadyExists, 0});
    }
    for (std::size_t other = index + 1U; other < config.actors.size(); ++other) {
      if (CapabilityTokenEqual(config.actors[index].token,
                               config.actors[other].token)) {
        return std::unexpected(Error{ErrorCode::kAlreadyExists, 0});
      }
    }
  }
  auto key_pair = mmr::SigningKeyPairFromSeed(*signing_seed);
  if (!key_pair) {
    return std::unexpected(key_pair.error());
  }
  config.signing_key = *key_pair;
  return config;
}

bool CapabilityTokenEqual(const CapabilityToken& left,
                          std::span<const std::byte> right) {
  if (right.size() != left.size()) {
    return false;
  }
  std::uint8_t difference = 0;
  for (std::size_t index = 0; index < left.size(); ++index) {
    difference |= std::to_integer<std::uint8_t>(left[index] ^ right[index]);
  }
  return difference == 0;
}

}  // namespace neocortex::serve
