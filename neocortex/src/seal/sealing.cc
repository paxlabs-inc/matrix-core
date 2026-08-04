#include "seal/sealing.h"

#include <array>
#include <cerrno>
#include <cstring>
#include <limits>
#include <string_view>

#include <fcntl.h>
#include <sodium.h>
#include <sys/stat.h>
#include <unistd.h>

namespace neocortex::seal {
namespace {

constexpr std::string_view kKeyMagic = "NCKEY001";
constexpr std::string_view kSealMagic = "NCSEAL01";
constexpr std::string_view kReceiptMagic = "NCDEL001";
constexpr std::string_view kUserKeyDomain = "neocortex.user-key.v1";
constexpr std::string_view kDataKeyDomain = "neocortex.data-key.v1";
constexpr std::string_view kRecordDomain = "neocortex.record.v1";
constexpr std::size_t kNonceBytes = crypto_aead_xchacha20poly1305_ietf_NPUBBYTES;
constexpr std::size_t kTagBytes = crypto_aead_xchacha20poly1305_ietf_ABYTES;
constexpr std::size_t kWrappedKeyBytes = 32U + kTagBytes;
constexpr std::size_t kKeyringBytes = 176;
constexpr std::size_t kReceiptBytes = 200;

void PutU16(std::span<std::byte> output, std::size_t offset, std::uint16_t value) {
  output[offset] = static_cast<std::byte>(value & 0xffU);
  output[offset + 1] = static_cast<std::byte>((value >> 8U) & 0xffU);
}

void PutU32(std::span<std::byte> output, std::size_t offset, std::uint32_t value) {
  for (std::size_t index = 0; index < 4; ++index) {
    output[offset + index] =
        static_cast<std::byte>((value >> (index * 8U)) & 0xffU);
  }
}

void PutU64(std::span<std::byte> output, std::size_t offset, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    output[offset + index] =
        static_cast<std::byte>((value >> (index * 8U)) & 0xffU);
  }
}

std::uint16_t GetU16(std::span<const std::byte> input, std::size_t offset) {
  return static_cast<std::uint16_t>(
      std::to_integer<std::uint16_t>(input[offset]) |
      (std::to_integer<std::uint16_t>(input[offset + 1]) << 8U));
}

std::uint32_t GetU32(std::span<const std::byte> input, std::size_t offset) {
  std::uint32_t value = 0;
  for (std::size_t index = 0; index < 4; ++index) {
    value |= std::to_integer<std::uint32_t>(input[offset + index]) << (index * 8U);
  }
  return value;
}

std::uint64_t GetU64(std::span<const std::byte> input, std::size_t offset) {
  std::uint64_t value = 0;
  for (std::size_t index = 0; index < 8; ++index) {
    value |= std::to_integer<std::uint64_t>(input[offset + index]) << (index * 8U);
  }
  return value;
}

class FileDescriptor final {
 public:
  explicit FileDescriptor(int descriptor) : descriptor_(descriptor) {}
  ~FileDescriptor() {
    if (descriptor_ >= 0) {
      ::close(descriptor_);
    }
  }
  FileDescriptor(const FileDescriptor&) = delete;
  FileDescriptor& operator=(const FileDescriptor&) = delete;
  [[nodiscard]] int get() const { return descriptor_; }

 private:
  int descriptor_;
};

std::expected<void, Error> WriteAll(int descriptor,
                                    std::span<const std::byte> bytes) {
  std::size_t written = 0;
  while (written < bytes.size()) {
    const auto result = ::write(descriptor, bytes.data() + written,
                                bytes.size() - written);
    if (result < 0) {
      if (errno == EINTR) {
        continue;
      }
      return std::unexpected(Error{ErrorCode::kWriteFailed, errno, 0, written});
    }
    if (result == 0) {
      return std::unexpected(Error{ErrorCode::kWriteFailed, 0, 0, written});
    }
    written += static_cast<std::size_t>(result);
  }
  return {};
}

std::expected<std::vector<std::byte>, Error> ReadExact(
    const std::filesystem::path& path, std::size_t expected_size) {
  const FileDescriptor descriptor(::open(path.c_str(), O_RDONLY | O_CLOEXEC));
  if (descriptor.get() < 0) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  struct stat status {};
  if (::fstat(descriptor.get(), &status) != 0) {
    return std::unexpected(Error{ErrorCode::kReadFailed, errno});
  }
  if (status.st_size < 0 || static_cast<std::uint64_t>(status.st_size) != expected_size) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  std::vector<std::byte> bytes(expected_size);
  std::size_t read_bytes = 0;
  while (read_bytes < bytes.size()) {
    const auto result = ::read(descriptor.get(), bytes.data() + read_bytes,
                               bytes.size() - read_bytes);
    if (result < 0) {
      if (errno == EINTR) {
        continue;
      }
      return std::unexpected(Error{ErrorCode::kReadFailed, errno, 0, read_bytes});
    }
    if (result == 0) {
      return std::unexpected(Error{ErrorCode::kTruncated, 0, 0, read_bytes});
    }
    read_bytes += static_cast<std::size_t>(result);
  }
  return bytes;
}

std::expected<void, Error> SyncDirectory(const std::filesystem::path& directory) {
  const FileDescriptor descriptor(
      ::open(directory.c_str(), O_RDONLY | O_DIRECTORY | O_CLOEXEC));
  if (descriptor.get() < 0) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  if (::fsync(descriptor.get()) != 0) {
    return std::unexpected(Error{ErrorCode::kSyncFailed, errno});
  }
  return {};
}

std::expected<void, Error> CreateDurableFile(
    const std::filesystem::path& path, std::span<const std::byte> bytes) {
  const FileDescriptor descriptor(
      ::open(path.c_str(), O_WRONLY | O_CREAT | O_EXCL | O_CLOEXEC, 0600));
  if (descriptor.get() < 0) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  auto written = WriteAll(descriptor.get(), bytes);
  if (!written) {
    return std::unexpected(written.error());
  }
  if (::fdatasync(descriptor.get()) != 0) {
    return std::unexpected(Error{ErrorCode::kSyncFailed, errno});
  }
  return SyncDirectory(path.parent_path());
}

std::vector<std::byte> KeyAssociatedData(std::string_view domain,
                                         std::uint16_t actor,
                                         const UserId& user) {
  std::vector<std::byte> associated(domain.size() + 2U + user.size());
  std::memcpy(associated.data(), domain.data(), domain.size());
  PutU16(associated, domain.size(), actor);
  std::memcpy(associated.data() + domain.size() + 2U, user.data(), user.size());
  return associated;
}

std::vector<std::byte> RecordAssociatedData(const log::FrameHeader& header,
                                            const UserId& user) {
  std::vector<std::byte> associated(kRecordDomain.size() + 2U + 8U + 1U + user.size());
  std::memcpy(associated.data(), kRecordDomain.data(), kRecordDomain.size());
  std::size_t offset = kRecordDomain.size();
  PutU16(associated, offset, header.actor);
  offset += 2U;
  PutU64(associated, offset, header.lsn);
  offset += 8U;
  associated[offset] = static_cast<std::byte>(header.kind);
  ++offset;
  std::memcpy(associated.data() + offset, user.data(), user.size());
  return associated;
}

std::expected<std::vector<std::byte>, Error> Encrypt(
    std::span<const std::byte> plaintext, std::span<const std::byte> associated,
    std::span<const std::byte, kNonceBytes> nonce,
    std::span<const std::byte, 32> key) {
  if (plaintext.size() >
      static_cast<std::size_t>(std::numeric_limits<unsigned long long>::max()) -
          kTagBytes) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  std::vector<std::byte> ciphertext(plaintext.size() + kTagBytes);
  unsigned long long ciphertext_size = 0;
  const int status = ::crypto_aead_xchacha20poly1305_ietf_encrypt(
      reinterpret_cast<unsigned char*>(ciphertext.data()), &ciphertext_size,
      reinterpret_cast<const unsigned char*>(plaintext.data()), plaintext.size(),
      reinterpret_cast<const unsigned char*>(associated.data()), associated.size(),
      nullptr, reinterpret_cast<const unsigned char*>(nonce.data()),
      reinterpret_cast<const unsigned char*>(key.data()));
  if (status != 0 || ciphertext_size != ciphertext.size()) {
    return std::unexpected(Error{ErrorCode::kCryptoAuthentication, status});
  }
  return ciphertext;
}

std::expected<std::vector<std::byte>, Error> Decrypt(
    std::span<const std::byte> ciphertext, std::span<const std::byte> associated,
    std::span<const std::byte, kNonceBytes> nonce,
    std::span<const std::byte, 32> key) {
  if (ciphertext.size() < kTagBytes) {
    return std::unexpected(Error{ErrorCode::kCryptoAuthentication, 0});
  }
  std::vector<std::byte> plaintext(ciphertext.size() - kTagBytes);
  unsigned long long plaintext_size = 0;
  const int status = ::crypto_aead_xchacha20poly1305_ietf_decrypt(
      reinterpret_cast<unsigned char*>(plaintext.data()), &plaintext_size, nullptr,
      reinterpret_cast<const unsigned char*>(ciphertext.data()), ciphertext.size(),
      reinterpret_cast<const unsigned char*>(associated.data()), associated.size(),
      reinterpret_cast<const unsigned char*>(nonce.data()),
      reinterpret_cast<const unsigned char*>(key.data()));
  if (status != 0 || plaintext_size != plaintext.size()) {
    return std::unexpected(Error{ErrorCode::kCryptoAuthentication, status});
  }
  return plaintext;
}

std::vector<std::byte> ReceiptMessage(const DeletionReceipt& receipt) {
  std::vector<std::byte> encoded(136);
  std::memcpy(encoded.data(), kReceiptMagic.data(), kReceiptMagic.size());
  PutU32(encoded, 8, 1);
  PutU16(encoded, 12, receipt.actor);
  std::memcpy(encoded.data() + 16, receipt.user.data(), receipt.user.size());
  PutU64(encoded, 32, receipt.deleted_at_lsn);
  std::memcpy(encoded.data() + 40, receipt.key_fingerprint.data(),
              receipt.key_fingerprint.size());
  std::memcpy(encoded.data() + 72, receipt.checkpoint_root.data(),
              receipt.checkpoint_root.size());
  std::memcpy(encoded.data() + 104, receipt.public_key.data(),
              receipt.public_key.size());
  return encoded;
}

std::vector<std::byte> EncodeReceipt(const DeletionReceipt& receipt) {
  auto encoded = ReceiptMessage(receipt);
  encoded.resize(kReceiptBytes);
  std::memcpy(encoded.data() + 136, receipt.signature.data(), receipt.signature.size());
  return encoded;
}

std::expected<DeletionReceipt, Error> DecodeReceipt(
    std::span<const std::byte> encoded) {
  if (encoded.size() != kReceiptBytes ||
      std::memcmp(encoded.data(), kReceiptMagic.data(), kReceiptMagic.size()) != 0 ||
      GetU32(encoded, 8) != 1) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  DeletionReceipt receipt{};
  receipt.actor = GetU16(encoded, 12);
  std::memcpy(receipt.user.data(), encoded.data() + 16, receipt.user.size());
  receipt.deleted_at_lsn = GetU64(encoded, 32);
  std::memcpy(receipt.key_fingerprint.data(), encoded.data() + 40,
              receipt.key_fingerprint.size());
  std::memcpy(receipt.checkpoint_root.data(), encoded.data() + 72,
              receipt.checkpoint_root.size());
  std::memcpy(receipt.public_key.data(), encoded.data() + 104,
              receipt.public_key.size());
  std::memcpy(receipt.signature.data(), encoded.data() + 136,
              receipt.signature.size());
  return receipt;
}

}  // namespace

std::expected<void, Error> VerifyDeletionReceipt(
    const DeletionReceipt& receipt,
    const mmr::PublicKey& trusted_public_key) {
  if (receipt.public_key != trusted_public_key) {
    return std::unexpected(Error{ErrorCode::kSignatureInvalid, 0});
  }
  const auto message = ReceiptMessage(receipt);
  if (::crypto_sign_verify_detached(
          reinterpret_cast<const unsigned char*>(receipt.signature.data()),
          reinterpret_cast<const unsigned char*>(message.data()), message.size(),
          reinterpret_cast<const unsigned char*>(trusted_public_key.data())) != 0) {
    return std::unexpected(Error{ErrorCode::kSignatureInvalid, 0});
  }
  return {};
}

std::expected<DeletionReceipt, Error> LoadDeletionReceipt(
    const std::filesystem::path& actor_directory,
    const mmr::PublicKey& trusted_public_key) {
  auto encoded = ReadExact(actor_directory / "keys" / "DELETION_RECEIPT",
                           kReceiptBytes);
  if (!encoded) {
    return std::unexpected(encoded.error());
  }
  auto receipt = DecodeReceipt(*encoded);
  if (!receipt) {
    return std::unexpected(receipt.error());
  }
  auto verified = VerifyDeletionReceipt(*receipt, trusted_public_key);
  if (!verified) {
    return std::unexpected(verified.error());
  }
  return receipt;
}

KeyHierarchy::KeyHierarchy(std::filesystem::path actor_directory,
                           std::filesystem::path key_directory,
                           std::uint16_t actor, UserId user,
                           KeyMaterial key_material,
                           mmr::PublicKey trusted_deletion_public_key)
    : actor_directory_(std::move(actor_directory)),
      key_directory_(std::move(key_directory)),
      actor_(actor),
      user_(user),
      user_key_(key_material.user_key),
      data_key_(key_material.data_key),
      trusted_deletion_public_key_(trusted_deletion_public_key) {}

KeyHierarchy::~KeyHierarchy() {
  ::sodium_memzero(user_key_.data(), user_key_.size());
  ::sodium_memzero(data_key_.data(), data_key_.size());
}

KeyHierarchy::KeyHierarchy(KeyHierarchy&& other) noexcept
    : actor_directory_(std::move(other.actor_directory_)),
      key_directory_(std::move(other.key_directory_)),
      actor_(other.actor_),
      user_(other.user_),
      user_key_(other.user_key_),
      data_key_(other.data_key_),
      trusted_deletion_public_key_(other.trusted_deletion_public_key_),
      destroyed_(other.destroyed_) {
  ::sodium_memzero(other.user_key_.data(), other.user_key_.size());
  ::sodium_memzero(other.data_key_.data(), other.data_key_.size());
  other.destroyed_ = true;
}

std::expected<KeyHierarchy, Error> KeyHierarchy::OpenOrCreate(
    const std::filesystem::path& actor_directory, std::uint16_t actor,
    const UserId& user, const KeyEncryptionKey& kek, core::Entropy& entropy,
    const mmr::PublicKey& trusted_deletion_public_key, bool allow_create) {
  if (::sodium_init() < 0) {
    return std::unexpected(Error{ErrorCode::kBackendUnavailable, 0});
  }
  const auto key_directory = actor_directory / "keys";
  std::error_code directory_error;
  std::filesystem::create_directories(key_directory, directory_error);
  if (directory_error) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, directory_error.value()});
  }
  const auto receipt_path = key_directory / "DELETION_RECEIPT";
  if (std::filesystem::exists(receipt_path, directory_error)) {
    auto receipt = LoadDeletionReceipt(actor_directory, trusted_deletion_public_key);
    if (!receipt) {
      return std::unexpected(receipt.error());
    }
    return std::unexpected(
        Error{ErrorCode::kKeyDestroyed, 0, receipt->deleted_at_lsn, 0});
  }
  if (directory_error) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, directory_error.value()});
  }

  const auto pending_receipt_path = key_directory / "DELETION_RECEIPT.pending";
  if (std::filesystem::exists(pending_receipt_path, directory_error)) {
    auto pending_bytes = ReadExact(pending_receipt_path, kReceiptBytes);
    auto pending_receipt =
        pending_bytes ? DecodeReceipt(*pending_bytes)
                      : std::expected<DeletionReceipt, Error>(
                            std::unexpected(pending_bytes.error()));
    if (!pending_receipt) {
      return std::unexpected(pending_receipt.error());
    }
    auto verified =
        VerifyDeletionReceipt(*pending_receipt, trusted_deletion_public_key);
    if (!verified || pending_receipt->actor != actor || pending_receipt->user != user) {
      return std::unexpected(!verified ? verified.error()
                                       : Error{ErrorCode::kCryptoAuthentication, 0});
    }
    const auto keyring_path = key_directory / "KEYRING";
    if (::unlink(keyring_path.c_str()) != 0 && errno != ENOENT) {
      return std::unexpected(Error{ErrorCode::kWriteFailed, errno});
    }
    if (auto synced = SyncDirectory(key_directory); !synced) {
      return std::unexpected(synced.error());
    }
    if (::rename(pending_receipt_path.c_str(), receipt_path.c_str()) != 0) {
      return std::unexpected(Error{ErrorCode::kWriteFailed, errno});
    }
    if (auto synced = SyncDirectory(key_directory); !synced) {
      return std::unexpected(synced.error());
    }
    return std::unexpected(
        Error{ErrorCode::kKeyDestroyed, 0, pending_receipt->deleted_at_lsn, 0});
  }
  if (directory_error) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, directory_error.value()});
  }

  const auto keyring_path = key_directory / "KEYRING";
  if (std::filesystem::exists(keyring_path, directory_error)) {
    auto encoded = ReadExact(keyring_path, kKeyringBytes);
    if (!encoded) {
      return std::unexpected(encoded.error());
    }
    if (std::memcmp(encoded->data(), kKeyMagic.data(), kKeyMagic.size()) != 0 ||
        GetU32(*encoded, 8) != 1 || GetU16(*encoded, 12) != actor ||
        !std::equal(user.begin(), user.end(), encoded->begin() + 16)) {
      return std::unexpected(Error{ErrorCode::kCryptoAuthentication, 0});
    }
    const auto user_associated = KeyAssociatedData(kUserKeyDomain, actor, user);
    const auto user_key = Decrypt(
        std::span(*encoded).subspan(56, kWrappedKeyBytes), user_associated,
        std::span<const std::byte, kNonceBytes>(encoded->data() + 32, kNonceBytes),
        kek);
    if (!user_key || user_key->size() != 32) {
      return std::unexpected(Error{ErrorCode::kCryptoAuthentication, 0});
    }
    std::array<std::byte, 32> fixed_user_key{};
    std::copy(user_key->begin(), user_key->end(), fixed_user_key.begin());
    const auto data_associated = KeyAssociatedData(kDataKeyDomain, actor, user);
    const auto data_key = Decrypt(
        std::span(*encoded).subspan(128, kWrappedKeyBytes), data_associated,
        std::span<const std::byte, kNonceBytes>(encoded->data() + 104, kNonceBytes),
        fixed_user_key);
    if (!data_key || data_key->size() != 32) {
      ::sodium_memzero(fixed_user_key.data(), fixed_user_key.size());
      return std::unexpected(Error{ErrorCode::kCryptoAuthentication, 0});
    }
    std::array<std::byte, 32> fixed_data_key{};
    std::copy(data_key->begin(), data_key->end(), fixed_data_key.begin());
    return KeyHierarchy(actor_directory, key_directory, actor, user,
                        KeyMaterial{.user_key = fixed_user_key,
                                    .data_key = fixed_data_key},
                        trusted_deletion_public_key);
  }
  if (directory_error) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, directory_error.value()});
  }
  if (!allow_create) {
    return std::unexpected(Error{ErrorCode::kLegacyPlaintext, 0});
  }

  std::array<std::byte, 32> user_key{};
  std::array<std::byte, 32> data_key{};
  std::array<std::byte, kNonceBytes> user_nonce{};
  std::array<std::byte, kNonceBytes> data_nonce{};
  if (auto filled = entropy.Fill(user_key); !filled) {
    return std::unexpected(filled.error());
  }
  if (auto filled = entropy.Fill(data_key); !filled) {
    return std::unexpected(filled.error());
  }
  if (auto filled = entropy.Fill(user_nonce); !filled) {
    return std::unexpected(filled.error());
  }
  if (auto filled = entropy.Fill(data_nonce); !filled) {
    return std::unexpected(filled.error());
  }
  const auto wrapped_user = Encrypt(
      user_key, KeyAssociatedData(kUserKeyDomain, actor, user), user_nonce, kek);
  const auto wrapped_data = Encrypt(
      data_key, KeyAssociatedData(kDataKeyDomain, actor, user), data_nonce, user_key);
  if (!wrapped_user || !wrapped_data) {
    return std::unexpected(!wrapped_user ? wrapped_user.error() : wrapped_data.error());
  }
  std::vector<std::byte> encoded(kKeyringBytes);
  std::memcpy(encoded.data(), kKeyMagic.data(), kKeyMagic.size());
  PutU32(encoded, 8, 1);
  PutU16(encoded, 12, actor);
  std::memcpy(encoded.data() + 16, user.data(), user.size());
  std::memcpy(encoded.data() + 32, user_nonce.data(), user_nonce.size());
  std::memcpy(encoded.data() + 56, wrapped_user->data(), wrapped_user->size());
  std::memcpy(encoded.data() + 104, data_nonce.data(), data_nonce.size());
  std::memcpy(encoded.data() + 128, wrapped_data->data(), wrapped_data->size());
  auto created = CreateDurableFile(keyring_path, encoded);
  if (!created) {
    return std::unexpected(created.error());
  }
  return KeyHierarchy(actor_directory, key_directory, actor, user,
                      KeyMaterial{.user_key = user_key, .data_key = data_key},
                      trusted_deletion_public_key);
}

std::expected<std::vector<std::byte>, Error> KeyHierarchy::Seal(
    const log::FrameHeader& header, std::span<const std::byte> plaintext,
    core::Entropy& entropy) const {
  if (destroyed_) {
    return std::unexpected(Error{ErrorCode::kKeyDestroyed, 0, header.lsn});
  }
  if (header.actor != actor_) {
    return std::unexpected(Error{ErrorCode::kCryptoAuthentication, 0, header.lsn});
  }
  std::array<std::byte, kNonceBytes> nonce{};
  auto filled = entropy.Fill(nonce);
  if (!filled) {
    return std::unexpected(filled.error());
  }
  auto ciphertext = Encrypt(plaintext, RecordAssociatedData(header, user_), nonce,
                            data_key_);
  if (!ciphertext) {
    return std::unexpected(ciphertext.error());
  }
  std::vector<std::byte> sealed(kSealMagic.size() + nonce.size() + ciphertext->size());
  std::memcpy(sealed.data(), kSealMagic.data(), kSealMagic.size());
  std::memcpy(sealed.data() + kSealMagic.size(), nonce.data(), nonce.size());
  std::memcpy(sealed.data() + kSealMagic.size() + nonce.size(), ciphertext->data(),
              ciphertext->size());
  return sealed;
}

std::expected<std::vector<std::byte>, Error> KeyHierarchy::Unseal(
    const log::FrameHeader& header,
    std::span<const std::byte> sealed_payload) const {
  if (destroyed_) {
    return std::unexpected(Error{ErrorCode::kKeyDestroyed, 0, header.lsn});
  }
  if (header.actor != actor_) {
    return std::unexpected(Error{ErrorCode::kCryptoAuthentication, 0, header.lsn});
  }
  if (sealed_payload.size() < kSealMagic.size() + kNonceBytes + kTagBytes) {
    return std::unexpected(Error{ErrorCode::kLegacyPlaintext, 0, header.lsn});
  }
  if (std::memcmp(sealed_payload.data(), kSealMagic.data(), kSealMagic.size()) != 0) {
    return std::unexpected(Error{ErrorCode::kLegacyPlaintext, 0, header.lsn});
  }
  const auto nonce_offset = kSealMagic.size();
  const auto ciphertext_offset = nonce_offset + kNonceBytes;
  return Decrypt(sealed_payload.subspan(ciphertext_offset),
                 RecordAssociatedData(header, user_),
                 std::span<const std::byte, kNonceBytes>(
                     sealed_payload.data() + nonce_offset, kNonceBytes),
                 data_key_);
}

std::expected<DeletionReceipt, Error> KeyHierarchy::Destroy(
    std::uint64_t deleted_at_lsn, const mmr::Hash& checkpoint_root,
    const mmr::SigningKeyPair& signing_key) {
  if (destroyed_) {
    return std::unexpected(Error{ErrorCode::kKeyDestroyed, 0, deleted_at_lsn});
  }
  if (signing_key.public_key != trusted_deletion_public_key_) {
    return std::unexpected(Error{ErrorCode::kSignatureInvalid, 0});
  }
  DeletionReceipt receipt{
      .actor = actor_,
      .user = user_,
      .deleted_at_lsn = deleted_at_lsn,
      .key_fingerprint = mmr::HashBytes(data_key_),
      .checkpoint_root = checkpoint_root,
      .public_key = signing_key.public_key,
      .signature = {},
  };
  const auto message = ReceiptMessage(receipt);
  unsigned long long signature_size = 0;
  if (::crypto_sign_detached(
          reinterpret_cast<unsigned char*>(receipt.signature.data()),
          &signature_size,
          reinterpret_cast<const unsigned char*>(message.data()), message.size(),
          reinterpret_cast<const unsigned char*>(signing_key.secret_key.data())) != 0 ||
      signature_size != receipt.signature.size()) {
    return std::unexpected(Error{ErrorCode::kSignatureInvalid, 0});
  }
  const auto encoded = EncodeReceipt(receipt);
  const auto pending_path = key_directory_ / "DELETION_RECEIPT.pending";
  auto prepared = CreateDurableFile(pending_path, encoded);
  if (!prepared) {
    return std::unexpected(prepared.error());
  }
  if (::unlink((key_directory_ / "KEYRING").c_str()) != 0) {
    const int unlink_error = errno;
    static_cast<void>(::unlink(pending_path.c_str()));
    return std::unexpected(Error{ErrorCode::kWriteFailed, unlink_error});
  }
  ::sodium_memzero(user_key_.data(), user_key_.size());
  ::sodium_memzero(data_key_.data(), data_key_.size());
  destroyed_ = true;
  auto synced = SyncDirectory(key_directory_);
  if (!synced) {
    return std::unexpected(synced.error());
  }
  const auto receipt_path = key_directory_ / "DELETION_RECEIPT";
  const int rename_status = ::rename(pending_path.c_str(), receipt_path.c_str());
  auto persisted = rename_status == 0
                       ? SyncDirectory(key_directory_)
                       : std::expected<void, Error>(std::unexpected(
                             Error{ErrorCode::kWriteFailed, errno}));
  if (!persisted) {
    return std::unexpected(persisted.error());
  }
  return receipt;
}

}  // namespace neocortex::seal
