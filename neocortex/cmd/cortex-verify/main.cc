#include <array>
#include <cstddef>
#include <cstdio>
#include <filesystem>
#include <string>
#include <string_view>

#include "mmr/store.h"

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

bool ParsePublicKey(std::string_view encoded, neocortex::mmr::PublicKey& public_key) {
  if (encoded.size() != public_key.size() * 2U) {
    return false;
  }
  for (std::size_t index = 0; index < public_key.size(); ++index) {
    const int high = HexValue(encoded[index * 2U]);
    const int low = HexValue(encoded[index * 2U + 1U]);
    if (high < 0 || low < 0) {
      return false;
    }
    public_key[index] = static_cast<std::byte>((high << 4U) | low);
  }
  return true;
}

std::string Hex(const neocortex::mmr::Hash& hash) {
  constexpr std::string_view alphabet = "0123456789abcdef";
  std::string encoded(hash.size() * 2U, '0');
  for (std::size_t index = 0; index < hash.size(); ++index) {
    const auto value = std::to_integer<unsigned int>(hash[index]);
    encoded[index * 2U] = alphabet[value >> 4U];
    encoded[index * 2U + 1U] = alphabet[value & 0x0fU];
  }
  return encoded;
}

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

}  // namespace

int main(int argc, char** argv) {
  if (argc != 5) {
    return Fail("usage: cortex-verify <actor-dir> <actor-u16> <checkpoint> <public-key-hex>");
  }
  char* actor_end = nullptr;
  const unsigned long actor_value = std::strtoul(argv[2], &actor_end, 10);
  if (actor_end == argv[2] || *actor_end != '\0' || actor_value == 0 || actor_value > 65535U) {
    return Fail("invalid actor");
  }
  neocortex::mmr::PublicKey public_key{};
  if (!ParsePublicKey(argv[4], public_key)) {
    return Fail("invalid public key");
  }
  auto checkpoint = neocortex::mmr::LoadCheckpoint(std::filesystem::path(argv[3]));
  if (!checkpoint) {
    return Fail("checkpoint decode failed");
  }
  auto signature = neocortex::mmr::VerifyCheckpointSignature(*checkpoint, public_key);
  if (!signature) {
    return Fail("checkpoint signature failed");
  }
  auto store = neocortex::mmr::MmrStore::Open(
      std::filesystem::path(argv[1]), static_cast<std::uint16_t>(actor_value), public_key);
  if (!store) {
    return Fail("mmr boot verification failed");
  }
  auto root = store->RootAt(checkpoint->leaf_count);
  if (!root || checkpoint->actor != actor_value || checkpoint->lsn != checkpoint->leaf_count ||
      checkpoint->root != *root) {
    return Fail("checkpoint root failed");
  }
  const std::string root_hex = Hex(*root);
  std::printf("MEMORY VERIFIED root=%s lsn=%llu\n", root_hex.c_str(),
              static_cast<unsigned long long>(checkpoint->lsn));
  return 0;
}
