#include <array>
#include <cstddef>
#include <cstdint>
#include <filesystem>
#include <optional>
#include <span>
#include <string>

#include <unistd.h>

#include "seal/sealing.h"
#include "mmr/store.h"
#include "sim/sim_env.h"

namespace {

class FuzzContext final {
 public:
  FuzzContext() : entropy_(0x1937) {
    std::array<char, 64> path{};
    const std::string pattern = "/tmp/neocortex-unseal-fuzz-XXXXXX";
    std::copy(pattern.begin(), pattern.end(), path.begin());
    char* created = ::mkdtemp(path.data());
    if (created == nullptr) {
      return;
    }
    directory_ = created;
    neocortex::mmr::SigningSeed seed{};
    auto keys = neocortex::mmr::SigningKeyPairFromSeed(seed);
    neocortex::seal::UserId user{};
    neocortex::seal::KeyEncryptionKey kek{};
    auto hierarchy = keys ? neocortex::seal::KeyHierarchy::OpenOrCreate(
                                directory_, 1, user, kek, entropy_,
                                keys->public_key, true)
                          : std::expected<neocortex::seal::KeyHierarchy,
                                          neocortex::Error>(
                                std::unexpected(neocortex::Error{
                                    neocortex::ErrorCode::kInvariantViolation, 0}));
    if (hierarchy) {
      hierarchy_.emplace(std::move(*hierarchy));
    }
  }

  ~FuzzContext() {
    hierarchy_.reset();
    std::error_code ignored;
    std::filesystem::remove_all(directory_, ignored);
  }

  void Run(std::span<const std::byte> input) {
    if (!hierarchy_) {
      return;
    }
    neocortex::log::FrameHeader header{
        .lsn = 1,
        .kind = neocortex::log::EventKind::kUserMsg,
        .wall_timestamp_ns = 0,
        .actor = 1,
        .conversation = {},
    };
    if (input.size() >= 10) {
      header.kind = static_cast<neocortex::log::EventKind>(
          (std::to_integer<std::uint8_t>(input[0]) % 21U) + 1U);
      header.actor = static_cast<std::uint16_t>(
          std::to_integer<std::uint8_t>(input[1]));
      std::uint64_t lsn = 0;
      for (std::size_t index = 0; index < 8; ++index) {
        lsn |= std::to_integer<std::uint64_t>(input[index + 2]) << (index * 8U);
      }
      header.lsn = lsn == 0 ? 1 : lsn;
      input = input.subspan(10);
    }
    static_cast<void>(hierarchy_->Unseal(header, input));
  }

 private:
  std::filesystem::path directory_;
  neocortex::sim::SimEntropy entropy_;
  std::optional<neocortex::seal::KeyHierarchy> hierarchy_;
};

}  // namespace

extern "C" int LLVMFuzzerTestOneInput(const std::uint8_t* data, std::size_t size) {
  static FuzzContext context;
  context.Run(std::as_bytes(std::span(data, size)));
  return 0;
}
