#include <algorithm>
#include <array>
#include <cerrno>
#include <cstddef>
#include <cstdio>
#include <filesystem>
#include <fcntl.h>
#include <span>
#include <string>
#include <string_view>
#include <sys/wait.h>
#include <unistd.h>
#include <vector>

#include "log/frame.h"
#include "mmr/mmr.h"
#include "mmr/store.h"

namespace {

class TempDirectory final {
 public:
  static std::expected<TempDirectory, int> Create() {
    std::array<char, 40> pattern{};
    constexpr std::string_view value = "/tmp/neocortex-mmr-XXXXXX";
    std::copy(value.begin(), value.end(), pattern.begin());
    char* created = ::mkdtemp(pattern.data());
    if (created == nullptr) {
      return std::unexpected(errno);
    }
    return TempDirectory(std::filesystem::path(created));
  }

  ~TempDirectory() {
    std::error_code ignored;
    std::filesystem::remove_all(path_, ignored);
  }
  TempDirectory(TempDirectory&&) noexcept = default;
  TempDirectory& operator=(TempDirectory&&) noexcept = default;
  TempDirectory(const TempDirectory&) = delete;
  TempDirectory& operator=(const TempDirectory&) = delete;

  [[nodiscard]] const std::filesystem::path& path() const { return path_; }

 private:
  explicit TempDirectory(std::filesystem::path path) : path_(std::move(path)) {}
  std::filesystem::path path_;
};

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

std::uint64_t NextRandom(std::uint64_t& state) {
  state ^= state << 13U;
  state ^= state >> 7U;
  state ^= state << 17U;
  return state;
}

neocortex::mmr::Hash RandomHash(std::uint64_t& state) {
  neocortex::mmr::Hash hash{};
  for (std::byte& value : hash) {
    value = static_cast<std::byte>(NextRandom(state) & 0xffU);
  }
  return hash;
}

std::string Hex(const neocortex::mmr::PublicKey& key) {
  constexpr std::string_view alphabet = "0123456789abcdef";
  std::string encoded(key.size() * 2U, '0');
  for (std::size_t index = 0; index < key.size(); ++index) {
    const auto value = std::to_integer<unsigned int>(key[index]);
    encoded[index * 2U] = alphabet[value >> 4U];
    encoded[index * 2U + 1U] = alphabet[value & 0x0fU];
  }
  return encoded;
}

int RangeProofProperty() {
  std::uint64_t random = 0xd1b54a32d192ed03ULL;
  neocortex::mmr::Mmr mmr;
  neocortex::mmr::Mmr replay;
  std::vector<neocortex::mmr::Hash> leaves;
  leaves.reserve(1024);
  for (std::size_t index = 0; index < 1024; ++index) {
    leaves.push_back(RandomHash(random));
    const auto append = mmr.Append(leaves.back());
    const auto replay_append = replay.Append(leaves.back());
    auto prefix = mmr.RootAt(index + 1U);
    if (!prefix || *prefix != append.root || replay_append.root != append.root) {
      return Fail("MMR append/replay convergence failed");
    }
  }

  for (std::size_t iteration = 0; iteration < 2048; ++iteration) {
    const std::uint64_t start = NextRandom(random) % leaves.size();
    const std::uint64_t count =
        1U + NextRandom(random) % (static_cast<std::uint64_t>(leaves.size()) - start);
    auto proof = mmr.ProveRange(start, count);
    if (!proof || proof->boundary_nodes.size() > 128U) {
      return Fail("MMR range proof construction failed");
    }
    const auto range = std::span<const neocortex::mmr::Hash>(leaves).subspan(start, count);
    auto verified = neocortex::mmr::Mmr::VerifyRange(range, *proof);
    if (!verified || *verified != mmr.Root()) {
      return Fail("MMR range proof verification failed");
    }
    std::vector<neocortex::mmr::Hash> forged(range.begin(), range.end());
    forged.front()[0] ^= std::byte{0x80};
    if (neocortex::mmr::Mmr::VerifyRange(forged, *proof)) {
      return Fail("forged range verified");
    }
    if (forged.size() > 1) {
      forged.assign(range.begin(), range.end());
      std::swap(forged.front(), forged.back());
      if (neocortex::mmr::Mmr::VerifyRange(forged, *proof)) {
        return Fail("reordered range verified");
      }
    }
  }
  return 0;
}

int RunVerifier(const std::filesystem::path& verifier,
                const std::filesystem::path& actor_directory,
                const std::filesystem::path& checkpoint,
                const neocortex::mmr::PublicKey& public_key) {
  std::array<int, 2> descriptors{};
  if (::pipe(descriptors.data()) != 0) {
    return Fail("pipe failed");
  }
  const pid_t child = ::fork();
  if (child < 0) {
    static_cast<void>(::close(descriptors[0]));
    static_cast<void>(::close(descriptors[1]));
    return Fail("fork failed");
  }
  if (child == 0) {
    static_cast<void>(::close(descriptors[0]));
    if (::dup2(descriptors[1], STDOUT_FILENO) < 0) {
      _exit(90);
    }
    static_cast<void>(::close(descriptors[1]));
    const std::string public_hex = Hex(public_key);
    ::execl(verifier.c_str(), verifier.c_str(), actor_directory.c_str(), "7",
            checkpoint.c_str(), public_hex.c_str(), static_cast<char*>(nullptr));
    _exit(91);
  }
  static_cast<void>(::close(descriptors[1]));
  std::array<char, 256> output{};
  const ssize_t output_bytes = ::read(descriptors[0], output.data(), output.size() - 1U);
  static_cast<void>(::close(descriptors[0]));
  int status = 0;
  if (::waitpid(child, &status, 0) != child || !WIFEXITED(status) || WEXITSTATUS(status) != 0 ||
      output_bytes <= 0 || std::string_view(output.data()).find("MEMORY VERIFIED root=") != 0) {
    return Fail("offline verifier failed");
  }
  return 0;
}

int PersistentStoreAndCheckpoint(const std::filesystem::path& verifier) {
  auto temporary = TempDirectory::Create();
  if (!temporary) {
    return Fail("temp directory failed");
  }
  neocortex::mmr::SigningSeed seed{};
  for (std::size_t index = 0; index < seed.size(); ++index) {
    seed[index] = static_cast<std::byte>(index);
  }
  auto key_pair = neocortex::mmr::SigningKeyPairFromSeed(seed);
  if (!key_pair) {
    return Fail("key derivation failed");
  }
  const auto actor_directory = temporary->path() / "actor-7";
  auto store = neocortex::mmr::MmrStore::Open(actor_directory, 7, key_pair->public_key);
  if (!store) {
    return Fail("MMR store open failed");
  }

  for (std::uint64_t lsn = 1; lsn <= 257; ++lsn) {
    neocortex::log::ConversationId conversation{};
    conversation.bytes[0] = static_cast<std::byte>(lsn & 0xffU);
    const neocortex::log::FrameHeader header{
        .lsn = lsn,
        .kind = neocortex::log::EventKind::kUserMsg,
        .wall_timestamp_ns = static_cast<std::int64_t>(lsn * 1000U),
        .actor = 7,
        .conversation = conversation,
    };
    const std::array payload{static_cast<std::byte>(lsn & 0xffU),
                             static_cast<std::byte>((lsn >> 8U) & 0xffU)};
    if (!store->AppendFrame(header, payload)) {
      return Fail("persistent MMR append failed");
    }
    if (lsn == 128) {
      if (!store->CreateCheckpoint(*key_pair)) {
        return Fail("first checkpoint failed");
      }
    }
  }
  auto checkpoint = store->CreateCheckpoint(*key_pair);
  if (!checkpoint || !store->verification_status().verified ||
      store->verification_status().last_checkpoint_lsn != 257) {
    return Fail("final checkpoint failed");
  }
  auto signature = neocortex::mmr::VerifyCheckpointSignature(*checkpoint,
                                                              key_pair->public_key);
  if (!signature) {
    return Fail("checkpoint signature verification failed");
  }
  auto forged = *checkpoint;
  forged.signature[0] ^= std::byte{0x01};
  if (neocortex::mmr::VerifyCheckpointSignature(forged, key_pair->public_key)) {
    return Fail("forged checkpoint verified");
  }

  auto reopened = neocortex::mmr::MmrStore::Open(actor_directory, 7,
                                                  key_pair->public_key);
  if (!reopened || reopened->verification_status().root != checkpoint->root ||
      reopened->verification_status().leaf_count != 257 ||
      reopened->verification_status().last_checkpoint_lsn != 257 ||
      !reopened->verification_status().verified) {
    return Fail("MMR restart verification failed");
  }
  const auto checkpoint_path =
      actor_directory / "mmr" / "checkpoints" / "00000000000000000257.ckpt";
  if (RunVerifier(verifier, actor_directory, checkpoint_path, key_pair->public_key) != 0) {
    return 1;
  }

  const auto peaks = actor_directory / "mmr" / "peaks";
  const int descriptor = ::open(peaks.c_str(), O_RDWR | O_CLOEXEC);
  std::array<std::byte, 120> first_two{};
  if (descriptor < 0 || ::pread(descriptor, first_two.data(), first_two.size(), 0) !=
                            static_cast<ssize_t>(first_two.size())) {
    if (descriptor >= 0) {
      static_cast<void>(::close(descriptor));
    }
    return Fail("node read failed");
  }
  std::swap_ranges(first_two.begin(), first_two.begin() + 60, first_two.begin() + 60);
  if (::pwrite(descriptor, first_two.data(), first_two.size(), 0) !=
          static_cast<ssize_t>(first_two.size()) ||
      ::fdatasync(descriptor) != 0 || ::close(descriptor) != 0) {
    return Fail("node reorder failed");
  }
  auto reordered = neocortex::mmr::MmrStore::Open(actor_directory, 7,
                                                   key_pair->public_key);
  if (reordered || reordered.error().code != neocortex::ErrorCode::kCheckpointMismatch) {
    return Fail("reordered MMR nodes were accepted");
  }
  return 0;
}

}  // namespace

int main(int argc, char** argv) {
  if (argc != 2) {
    return Fail("mmr test requires cortex-verify path");
  }
  if (const int result = RangeProofProperty(); result != 0) {
    return result;
  }
  return PersistentStoreAndCheckpoint(std::filesystem::path(argv[1]));
}
