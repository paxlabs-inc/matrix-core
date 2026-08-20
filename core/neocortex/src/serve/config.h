#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <expected>
#include <filesystem>
#include <span>
#include <vector>

#include "core/error.h"
#include "mmr/store.h"
#include "seal/sealing.h"

namespace neocortex::serve {

using CapabilityToken = std::array<std::byte, 32>;

struct ActorCapability final {
  std::uint16_t actor;
  CapabilityToken token;
};

struct ServerConfig final {
  std::filesystem::path socket_path;
  std::filesystem::path data_directory;
  seal::UserId user;
  seal::KeyEncryptionKey kek;
  mmr::SigningKeyPair signing_key;
  CapabilityToken admin_token;
  std::vector<ActorCapability> actors;
  std::size_t maximum_connections = 64;
  std::size_t maximum_output_frames = 128;
  std::size_t maximum_output_bytes =
      static_cast<std::size_t>(64) * 1024U * 1024U;
  std::size_t projection_map_bytes =
      static_cast<std::size_t>(256) * 1024U * 1024U;
};

[[nodiscard]] std::expected<ServerConfig, Error> LoadServerConfig(
    const std::filesystem::path& path);

[[nodiscard]] bool CapabilityTokenEqual(const CapabilityToken& left,
                                        std::span<const std::byte> right);

}  // namespace neocortex::serve
