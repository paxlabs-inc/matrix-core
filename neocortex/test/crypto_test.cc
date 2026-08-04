#include <algorithm>
#include <array>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <filesystem>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include <unistd.h>

#include "seal/sealing.h"
#include "log/segment_log.h"
#include "log/storage_adapter.h"
#include "mmr/store.h"
#include "sim/sim_env.h"

namespace {

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

std::filesystem::path TemporaryDirectory(std::string_view prefix) {
  std::array<char, 96> path{};
  const std::string pattern = "/tmp/" + std::string(prefix) + "-XXXXXX";
  std::copy(pattern.begin(), pattern.end(), path.begin());
  char* created = ::mkdtemp(path.data());
  return created == nullptr ? std::filesystem::path{} : std::filesystem::path(created);
}

neocortex::mmr::SigningKeyPair Keys() {
  neocortex::mmr::SigningSeed seed{};
  seed[0] = std::byte{0x5a};
  auto keys = neocortex::mmr::SigningKeyPairFromSeed(seed);
  if (!keys) {
    std::abort();
  }
  return *keys;
}

neocortex::seal::UserId User() {
  neocortex::seal::UserId user{};
  user[0] = std::byte{0x41};
  user[15] = std::byte{0x9d};
  return user;
}

neocortex::seal::KeyEncryptionKey Kek() {
  neocortex::seal::KeyEncryptionKey kek{};
  for (std::size_t index = 0; index < kek.size(); ++index) {
    kek[index] = static_cast<std::byte>(index * 7U + 3U);
  }
  return kek;
}

struct PayloadSize final {
  std::size_t value;
};

neocortex::log::Frame Frame(std::uint64_t lsn, PayloadSize payload_size) {
  neocortex::log::ConversationId conversation{};
  conversation.bytes[0] = static_cast<std::byte>(lsn & 0xffU);
  std::vector<std::byte> payload(payload_size.value);
  for (std::size_t index = 0; index < payload.size(); ++index) {
    payload[index] = static_cast<std::byte>((lsn * 31U + index) & 0xffU);
  }
  return neocortex::log::Frame{
      .header = neocortex::log::FrameHeader{
          .lsn = lsn,
          .kind = static_cast<neocortex::log::EventKind>(((lsn - 1U) % 21U) + 1U),
          .wall_timestamp_ns = static_cast<std::int64_t>(lsn) * 1'000,
          .actor = 23,
          .conversation = conversation,
      },
      .sealed_payload = std::move(payload),
  };
}

int TestRoundTripsAndBinding() {
  const auto path = TemporaryDirectory("neocortex-crypto-roundtrip");
  if (path.empty()) {
    return Fail("crypto temp directory failed");
  }
  const auto keys = Keys();
  const auto user = User();
  const auto kek = Kek();
  neocortex::sim::SimEntropy entropy(0x8128);
  auto hierarchy = neocortex::seal::KeyHierarchy::OpenOrCreate(
      path, 23, user, kek, entropy, keys.public_key, true);
  if (!hierarchy) {
    return Fail("key hierarchy creation failed");
  }

  for (std::uint64_t lsn = 1; lsn <= 512; ++lsn) {
    const auto frame = Frame(
        lsn, PayloadSize{static_cast<std::size_t>((lsn * 37U) % 2049U)});
    auto sealed = hierarchy->Seal(frame.header, frame.sealed_payload, entropy);
    auto plaintext = sealed ? hierarchy->Unseal(frame.header, *sealed)
                            : std::expected<std::vector<std::byte>, neocortex::Error>(
                                  std::unexpected(neocortex::Error{
                                      neocortex::ErrorCode::kInvariantViolation, 0}));
    if (!sealed || !plaintext || *plaintext != frame.sealed_payload) {
      return Fail("AEAD round trip failed");
    }
    auto tampered = *sealed;
    tampered.back() ^= std::byte{0x80};
    auto rejected = hierarchy->Unseal(frame.header, tampered);
    if (rejected ||
        rejected.error().code != neocortex::ErrorCode::kCryptoAuthentication) {
      return Fail("tampered ciphertext was accepted");
    }
    auto wrong_header = frame.header;
    ++wrong_header.lsn;
    rejected = hierarchy->Unseal(wrong_header, *sealed);
    if (rejected ||
        rejected.error().code != neocortex::ErrorCode::kCryptoAuthentication) {
      return Fail("LSN associated-data substitution was accepted");
    }
    wrong_header = frame.header;
    wrong_header.kind = wrong_header.kind == neocortex::log::EventKind::kUserMsg
                            ? neocortex::log::EventKind::kDeliveredMsg
                            : neocortex::log::EventKind::kUserMsg;
    rejected = hierarchy->Unseal(wrong_header, *sealed);
    if (rejected ||
        rejected.error().code != neocortex::ErrorCode::kCryptoAuthentication) {
      return Fail("kind associated-data substitution was accepted");
    }
  }

  auto legacy = hierarchy->Unseal(Frame(1, PayloadSize{4}).header,
                                  Frame(1, PayloadSize{4}).sealed_payload);
  if (legacy || legacy.error().code != neocortex::ErrorCode::kLegacyPlaintext) {
    return Fail("legacy plaintext was accepted by unseal");
  }
  neocortex::seal::UserId wrong_user = user;
  wrong_user[0] ^= std::byte{0x01};
  neocortex::sim::SimEntropy reopen_entropy(1);
  auto rejected_user = neocortex::seal::KeyHierarchy::OpenOrCreate(
      path, 23, wrong_user, kek, reopen_entropy, keys.public_key, false);
  if (rejected_user ||
      rejected_user.error().code != neocortex::ErrorCode::kCryptoAuthentication) {
    return Fail("wrong user opened a key hierarchy");
  }
  auto wrong_kek = kek;
  wrong_kek[4] ^= std::byte{0x20};
  auto rejected_kek = neocortex::seal::KeyHierarchy::OpenOrCreate(
      path, 23, user, wrong_kek, reopen_entropy, keys.public_key, false);
  if (rejected_kek ||
      rejected_kek.error().code != neocortex::ErrorCode::kCryptoAuthentication) {
    return Fail("wrong KEK opened a key hierarchy");
  }
  std::error_code cleanup_error;
  std::filesystem::remove_all(path, cleanup_error);
  return cleanup_error ? Fail("crypto roundtrip cleanup failed") : 0;
}

int TestHashBoundaryAndDeletion() {
  const auto path_a = TemporaryDirectory("neocortex-crypto-a");
  const auto path_b = TemporaryDirectory("neocortex-crypto-b");
  if (path_a.empty() || path_b.empty()) {
    return Fail("hash boundary temp directory failed");
  }
  const auto keys = Keys();
  const auto user = User();
  const auto kek = Kek();
  neocortex::sim::SimEntropy entropy_a(11);
  neocortex::sim::SimEntropy entropy_b(99);
  auto storage_a = neocortex::log::SegmentStorage::Open(
      path_a, 23, keys.public_key, user, kek, entropy_a,
      {.backend = neocortex::log::WriteBackend::kPwrite});
  auto storage_b = neocortex::log::SegmentStorage::Open(
      path_b, 23, keys.public_key, user, kek, entropy_b,
      {.backend = neocortex::log::WriteBackend::kPwrite});
  if (!storage_a || !storage_b) {
    return Fail("sealed segment storage open failed");
  }
  std::vector<neocortex::log::Frame> frames;
  for (std::uint64_t lsn = 1; lsn <= 41; ++lsn) {
    frames.push_back(Frame(lsn, PayloadSize{static_cast<std::size_t>(lsn * 3U)}));
  }
  auto commit_a = storage_a->Append(frames);
  auto commit_b = storage_b->Append(frames);
  if (!commit_a || !commit_b ||
      storage_a->mmr_store().mmr().Root() !=
          storage_b->mmr_store().mmr().Root()) {
    return Fail("plaintext MMR boundary diverged under different sealing keys");
  }
  auto raw_a = storage_a->log().ReadAll();
  auto raw_b = storage_b->log().ReadAll();
  if (!raw_a || !raw_b || raw_a->front().sealed_payload == frames.front().sealed_payload ||
      raw_a->front().sealed_payload == raw_b->front().sealed_payload) {
    return Fail("log payload was not independently sealed");
  }
  auto recovered = storage_a->Recover();
  if (!recovered || *recovered != frames) {
    return Fail("sealed storage recovery changed plaintext");
  }

  auto receipt = storage_a->DestroyKey(keys);
  if (!receipt || !neocortex::seal::VerifyDeletionReceipt(*receipt,
                                                             keys.public_key)) {
    return Fail("deletion receipt did not verify");
  }
  auto forged = *receipt;
  forged.checkpoint_root[0] ^= std::byte{0x40};
  if (neocortex::seal::VerifyDeletionReceipt(forged, keys.public_key)) {
    return Fail("forged deletion receipt verified");
  }
  auto after_delete = storage_a->Recover();
  if (after_delete ||
      after_delete.error().code != neocortex::ErrorCode::kKeyDestroyed) {
    return Fail("post-destruction read did not fail closed");
  }
  neocortex::sim::SimEntropy reopen_entropy(3);
  auto reopened = neocortex::log::SegmentStorage::Open(
      path_a, 23, keys.public_key, user, kek, reopen_entropy,
      {.backend = neocortex::log::WriteBackend::kPwrite});
  if (reopened || reopened.error().code != neocortex::ErrorCode::kKeyDestroyed) {
    return Fail("destroyed hierarchy reopened");
  }
  auto persisted_receipt =
      neocortex::seal::LoadDeletionReceipt(path_a, keys.public_key);
  if (!persisted_receipt || *persisted_receipt != *receipt) {
    return Fail("persisted deletion receipt diverged");
  }

  std::error_code cleanup_error;
  std::filesystem::remove_all(path_a, cleanup_error);
  if (cleanup_error) {
    return Fail("crypto deletion cleanup failed");
  }
  std::filesystem::remove_all(path_b, cleanup_error);
  return cleanup_error ? Fail("crypto boundary cleanup failed") : 0;
}

int TestLegacyLogRefusal() {
  const auto path = TemporaryDirectory("neocortex-legacy-log");
  if (path.empty()) {
    return Fail("legacy temp directory failed");
  }
  auto log = neocortex::log::SegmentLog::Open(
      path, 23, {.backend = neocortex::log::WriteBackend::kPwrite});
  const auto frame = Frame(1, PayloadSize{17});
  const neocortex::log::AppendRequest request{
      .kind = frame.header.kind,
      .wall_timestamp_ns = frame.header.wall_timestamp_ns,
      .conversation = frame.header.conversation,
      .sealed_payload = frame.sealed_payload,
  };
  if (!log || !log->AppendBatch(std::span(&request, 1))) {
    return Fail("legacy fixture append failed");
  }
  const auto keys = Keys();
  neocortex::sim::SimEntropy entropy(1);
  auto storage = neocortex::log::SegmentStorage::Open(
      path, 23, keys.public_key, User(), Kek(), entropy,
      {.backend = neocortex::log::WriteBackend::kPwrite});
  if (storage || storage.error().code != neocortex::ErrorCode::kLegacyPlaintext) {
    return Fail("legacy plaintext log was accepted");
  }
  std::error_code cleanup_error;
  std::filesystem::remove_all(path, cleanup_error);
  return cleanup_error ? Fail("legacy cleanup failed") : 0;
}

}  // namespace

int main() {
  if (TestRoundTripsAndBinding() != 0 || TestHashBoundaryAndDeletion() != 0 ||
      TestLegacyLogRefusal() != 0) {
    return 1;
  }
  return 0;
}
