#include "proj/beliefs/belief_store.h"

#include <algorithm>
#include <array>
#include <bit>
#include <limits>
#include <map>
#include <set>
#include <string_view>
#include <utility>

#include "gate/belief_gate.h"
#include "schema/events.h"

namespace neocortex::proj {
namespace {

constexpr std::array<std::byte, 8> kRecordMagic = {
    std::byte{'N'}, std::byte{'C'}, std::byte{'B'}, std::byte{'L'},
    std::byte{'F'}, std::byte{'0'}, std::byte{'0'}, std::byte{'2'}};
constexpr std::size_t kRecordHeaderBytes = 72;
constexpr std::byte kHeadPrefix{'H'};
constexpr std::byte kVersionPrefix{'V'};
constexpr std::byte kIdPrefix{'I'};
constexpr std::byte kDomainPrefix{'D'};
constexpr std::byte kConflictPrefix{'X'};
constexpr std::byte kToolResultPrefix{'T'};
constexpr std::uint8_t kTombstoneFlag = 1;

struct OwnedMutation final {
  MutationKind kind;
  std::vector<std::byte> key;
  std::vector<std::byte> value;
};

using StagedHeads =
    std::map<std::vector<std::byte>, std::optional<BeliefRecord>>;
using StagedIds = std::map<std::vector<std::byte>, std::vector<std::byte>>;

struct StoredConflict final {
  std::vector<std::byte> other_head_key;
  std::uint64_t created_lsn;
  std::uint64_t resolved_lsn;
};

using DomainMembers = std::vector<std::vector<std::byte>>;
using StoredConflicts = std::vector<StoredConflict>;
using StagedDomains = std::map<std::vector<std::byte>, DomainMembers>;
using StagedConflicts = std::map<std::vector<std::byte>, StoredConflicts>;

void AppendU32(std::vector<std::byte>& output, std::uint32_t value) {
  for (std::size_t index = 0; index < 4; ++index) {
    output.push_back(static_cast<std::byte>((value >> (index * 8U)) & 0xffU));
  }
}

void AppendU64(std::vector<std::byte>& output, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    output.push_back(static_cast<std::byte>((value >> (index * 8U)) & 0xffU));
  }
}

void AppendU64BigEndian(std::vector<std::byte>& output, std::uint64_t value) {
  for (std::size_t index = 0; index < 8; ++index) {
    output.push_back(
        static_cast<std::byte>((value >> ((7U - index) * 8U)) & 0xffU));
  }
}

std::expected<std::uint32_t, Error> ReadU32(std::span<const std::byte> input,
                                           std::size_t offset) {
  if (offset > input.size() || input.size() - offset < 4) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  std::uint32_t value = 0;
  for (std::size_t index = 0; index < 4; ++index) {
    value |= std::to_integer<std::uint32_t>(input[offset + index]) <<
             (index * 8U);
  }
  return value;
}

std::expected<std::uint64_t, Error> ReadU64(std::span<const std::byte> input,
                                           std::size_t offset) {
  if (offset > input.size() || input.size() - offset < 8) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  std::uint64_t value = 0;
  for (std::size_t index = 0; index < 8; ++index) {
    value |= std::to_integer<std::uint64_t>(input[offset + index]) <<
             (index * 8U);
  }
  return value;
}

std::expected<std::uint32_t, Error> CheckedU32(std::size_t value) {
  if (value > std::numeric_limits<std::uint32_t>::max()) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  return static_cast<std::uint32_t>(value);
}

std::vector<std::byte> HeadKey(schema::BeliefType type,
                               std::string_view canonical_identity) {
  std::vector<std::byte> key;
  key.reserve(6U + canonical_identity.size());
  key.push_back(kHeadPrefix);
  key.push_back(static_cast<std::byte>(type));
  AppendU32(key, static_cast<std::uint32_t>(canonical_identity.size()));
  key.insert(key.end(),
             reinterpret_cast<const std::byte*>(canonical_identity.data()),
             reinterpret_cast<const std::byte*>(canonical_identity.data()) +
                 canonical_identity.size());
  return key;
}

std::vector<std::byte> VersionKey(std::span<const std::byte> head_key,
                                  std::uint64_t version) {
  std::vector<std::byte> key(head_key.begin(), head_key.end());
  key.front() = kVersionPrefix;
  AppendU64(key, version);
  return key;
}

std::vector<std::byte> IdKey(std::span<const std::byte> belief_id) {
  std::vector<std::byte> key;
  key.reserve(5U + belief_id.size());
  key.push_back(kIdPrefix);
  AppendU32(key, static_cast<std::uint32_t>(belief_id.size()));
  key.insert(key.end(), belief_id.begin(), belief_id.end());
  return key;
}

std::vector<std::byte> DomainKey(schema::BeliefType type,
                                 std::string_view conflict_domain) {
  std::vector<std::byte> key;
  key.reserve(6U + conflict_domain.size());
  key.push_back(kDomainPrefix);
  key.push_back(static_cast<std::byte>(type));
  AppendU32(key, static_cast<std::uint32_t>(conflict_domain.size()));
  key.insert(key.end(),
             reinterpret_cast<const std::byte*>(conflict_domain.data()),
             reinterpret_cast<const std::byte*>(conflict_domain.data()) +
                 conflict_domain.size());
  return key;
}

std::vector<std::byte> ConflictKey(std::span<const std::byte> head_key) {
  std::vector<std::byte> key;
  key.reserve(1U + head_key.size());
  key.push_back(kConflictPrefix);
  key.insert(key.end(), head_key.begin(), head_key.end());
  return key;
}

std::vector<std::byte> ToolResultKey(std::uint64_t lsn) {
  std::vector<std::byte> key;
  key.reserve(9);
  key.push_back(kToolResultPrefix);
  AppendU64BigEndian(key, lsn);
  return key;
}

struct DecodedHeadKey final {
  schema::BeliefType type;
  std::string canonical_identity;
};

std::expected<DecodedHeadKey, Error> DecodeHeadKey(
    std::span<const std::byte> key) {
  if (key.size() < 6 || key[0] != kHeadPrefix) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  const auto raw_type = std::to_integer<std::uint8_t>(key[1]);
  auto identity_size = ReadU32(key, 2);
  if (raw_type > static_cast<std::uint8_t>(schema::BeliefType::identity) ||
      !identity_size || *identity_size == 0 ||
      key.size() - 6U != *identity_size) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  return DecodedHeadKey{
      .type = static_cast<schema::BeliefType>(raw_type),
      .canonical_identity = std::string(
          reinterpret_cast<const char*>(key.data() + 6), *identity_size),
  };
}

std::vector<std::byte> EncodeHead(std::uint64_t version) {
  std::vector<std::byte> value;
  value.reserve(8);
  AppendU64(value, version);
  return value;
}

std::expected<std::vector<std::byte>, Error> EncodeDomainMembers(
    const DomainMembers& members) {
  auto count = CheckedU32(members.size());
  if (!count) {
    return std::unexpected(count.error());
  }
  std::vector<std::byte> encoded;
  AppendU32(encoded, *count);
  for (const auto& member : members) {
    auto size = CheckedU32(member.size());
    if (!size || !DecodeHeadKey(member)) {
      return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
    }
    AppendU32(encoded, *size);
    encoded.insert(encoded.end(), member.begin(), member.end());
  }
  return encoded;
}

std::expected<DomainMembers, Error> DecodeDomainMembers(
    std::span<const std::byte> encoded) {
  auto count = ReadU32(encoded, 0);
  if (!count) {
    return std::unexpected(count.error());
  }
  DomainMembers members;
  members.reserve(*count);
  std::size_t cursor = 4;
  for (std::uint32_t index = 0; index < *count; ++index) {
    auto size = ReadU32(encoded, cursor);
    if (!size || *size == 0 || cursor + 4U > encoded.size() ||
        encoded.size() - (cursor + 4U) < *size) {
      return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
    }
    cursor += 4U;
    std::vector<std::byte> member(
        encoded.begin() + static_cast<std::ptrdiff_t>(cursor),
        encoded.begin() + static_cast<std::ptrdiff_t>(cursor + *size));
    if (!DecodeHeadKey(member) ||
        (!members.empty() && !(members.back() < member))) {
      return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
    }
    members.push_back(std::move(member));
    cursor += *size;
  }
  if (cursor != encoded.size()) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  return members;
}

bool ConflictLess(const StoredConflict& left, const StoredConflict& right) {
  if (left.other_head_key != right.other_head_key) {
    return left.other_head_key < right.other_head_key;
  }
  return left.created_lsn < right.created_lsn;
}

std::expected<std::vector<std::byte>, Error> EncodeStoredConflicts(
    const StoredConflicts& conflicts) {
  auto count = CheckedU32(conflicts.size());
  if (!count) {
    return std::unexpected(count.error());
  }
  std::vector<std::byte> encoded;
  AppendU32(encoded, *count);
  for (const auto& conflict : conflicts) {
    auto key_size = CheckedU32(conflict.other_head_key.size());
    if (!key_size || !DecodeHeadKey(conflict.other_head_key) ||
        conflict.created_lsn == 0 ||
        (conflict.resolved_lsn != 0 &&
         conflict.resolved_lsn <= conflict.created_lsn)) {
      return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
    }
    AppendU64(encoded, conflict.created_lsn);
    AppendU64(encoded, conflict.resolved_lsn);
    AppendU32(encoded, *key_size);
    encoded.insert(encoded.end(), conflict.other_head_key.begin(),
                   conflict.other_head_key.end());
  }
  return encoded;
}

std::expected<StoredConflicts, Error> DecodeStoredConflicts(
    std::span<const std::byte> encoded) {
  auto count = ReadU32(encoded, 0);
  if (!count) {
    return std::unexpected(count.error());
  }
  StoredConflicts conflicts;
  conflicts.reserve(*count);
  std::size_t cursor = 4;
  for (std::uint32_t index = 0; index < *count; ++index) {
    auto created = ReadU64(encoded, cursor);
    auto resolved = ReadU64(encoded, cursor + 8U);
    auto key_size = ReadU32(encoded, cursor + 16U);
    if (!created || !resolved || !key_size || *created == 0 || *key_size == 0 ||
        (*resolved != 0 && *resolved <= *created) || cursor + 20U > encoded.size() ||
        encoded.size() - (cursor + 20U) < *key_size) {
      return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
    }
    cursor += 20U;
    StoredConflict conflict{
        .other_head_key = {
            encoded.begin() + static_cast<std::ptrdiff_t>(cursor),
            encoded.begin() + static_cast<std::ptrdiff_t>(cursor + *key_size)},
        .created_lsn = *created,
        .resolved_lsn = *resolved,
    };
    if (!DecodeHeadKey(conflict.other_head_key) ||
        (!conflicts.empty() && !ConflictLess(conflicts.back(), conflict))) {
      return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
    }
    conflicts.push_back(std::move(conflict));
    cursor += *key_size;
  }
  if (cursor != encoded.size()) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  return conflicts;
}

std::expected<DomainMembers*, Error> MutableDomain(
    const ReadSnapshot& snapshot, const std::vector<std::byte>& domain_key,
    StagedDomains& domains) {
  const auto staged = domains.find(domain_key);
  if (staged != domains.end()) {
    return &staged->second;
  }
  auto stored = snapshot.Get(ProjectionId::kBeliefStore, domain_key);
  if (!stored) {
    return std::unexpected(stored.error());
  }
  DomainMembers members;
  auto maybe_stored = std::move(stored).value();
  if (maybe_stored.has_value()) {
    auto decoded = DecodeDomainMembers(
        std::move(maybe_stored).value_or(std::vector<std::byte>{}));
    if (!decoded) {
      return std::unexpected(decoded.error());
    }
    members = std::move(*decoded);
  }
  const auto inserted = domains.emplace(domain_key, std::move(members));
  return &inserted.first->second;
}

std::expected<StoredConflicts*, Error> MutableConflicts(
    const ReadSnapshot& snapshot, const std::vector<std::byte>& head_key,
    StagedConflicts& conflicts) {
  const auto staged = conflicts.find(head_key);
  if (staged != conflicts.end()) {
    return &staged->second;
  }
  auto stored = snapshot.Get(ProjectionId::kBeliefStore, ConflictKey(head_key));
  if (!stored) {
    return std::unexpected(stored.error());
  }
  StoredConflicts values;
  auto maybe_stored = std::move(stored).value();
  if (maybe_stored.has_value()) {
    auto decoded = DecodeStoredConflicts(
        std::move(maybe_stored).value_or(std::vector<std::byte>{}));
    if (!decoded) {
      return std::unexpected(decoded.error());
    }
    values = std::move(*decoded);
  }
  const auto inserted = conflicts.emplace(head_key, std::move(values));
  return &inserted.first->second;
}

bool AddConflict(StoredConflicts& conflicts,
                 const std::vector<std::byte>& other_head_key,
                 std::uint64_t transaction_lsn) {
  for (const auto& conflict : conflicts) {
    if (conflict.other_head_key == other_head_key && conflict.resolved_lsn == 0) {
      return false;
    }
  }
  conflicts.push_back(StoredConflict{.other_head_key = other_head_key,
                                     .created_lsn = transaction_lsn,
                                     .resolved_lsn = 0});
  std::sort(conflicts.begin(), conflicts.end(), ConflictLess);
  return true;
}

bool ResolveConflicts(StoredConflicts& conflicts, std::uint64_t transaction_lsn) {
  bool changed = false;
  for (auto& conflict : conflicts) {
    if (conflict.resolved_lsn == 0) {
      conflict.resolved_lsn = transaction_lsn;
      changed = true;
    }
  }
  return changed;
}

bool ResolveConflictWith(StoredConflicts& conflicts,
                         const std::vector<std::byte>& other_head_key,
                         std::uint64_t transaction_lsn) {
  bool changed = false;
  for (auto& conflict : conflicts) {
    if (conflict.other_head_key == other_head_key && conflict.resolved_lsn == 0) {
      conflict.resolved_lsn = transaction_lsn;
      changed = true;
    }
  }
  return changed;
}

std::expected<bool, Error> HasCorroboratingToolResult(
    const ReadSnapshot& snapshot,
    const flatbuffers::Vector<const schema::ProvenanceRange*>* provenance,
    std::uint64_t transaction_lsn) {
  for (const auto* range : *provenance) {
    if (range->first_lsn() >= transaction_lsn) {
      continue;
    }
    const auto last = std::min(range->last_lsn(), transaction_lsn - 1U);
    const auto lower = ToolResultKey(range->first_lsn());
    const auto upper = ToolResultKey(last);
    auto candidate =
        snapshot.FirstAtOrAfter(ProjectionId::kBeliefStore, lower);
    if (!candidate) {
      return std::unexpected(candidate.error());
    }
    auto maybe_candidate = std::move(candidate).value();
    if (maybe_candidate.has_value()) {
      const auto found = std::move(maybe_candidate).value_or(KeyValue{});
      if (found.key <= upper && found.key.size() == 9 &&
          found.key.front() == kToolResultPrefix) {
        return true;
      }
    }
  }
  return false;
}

std::expected<std::vector<std::byte>, Error> EncodeRecord(
    const BeliefRecord& record) {
  auto id_size = CheckedU32(record.belief_id.size());
  auto identity_size = CheckedU32(record.canonical_identity.size());
  auto domain_size = CheckedU32(record.conflict_domain.size());
  auto value_size = CheckedU32(record.value.size());
  auto provenance_size = CheckedU32(record.provenance.size());
  if (!id_size || !identity_size || !domain_size || !value_size ||
      !provenance_size ||
      record.belief_id.empty() || record.canonical_identity.empty() ||
      record.provenance.empty() || record.version == 0 ||
      record.transaction_lsn == 0 ||
      static_cast<std::uint8_t>(record.claim) >
          static_cast<std::uint8_t>(schema::AssertionClaim::negative_existence) ||
      (record.valid_to_ns != 0 && record.valid_to_ns < record.valid_from_ns)) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0,
                                 record.transaction_lsn});
  }
  std::vector<std::byte> output;
  output.reserve(kRecordHeaderBytes + record.belief_id.size() +
                 record.canonical_identity.size() + record.conflict_domain.size() +
                 record.value.size() +
                 record.provenance.size() * 16U);
  output.insert(output.end(), kRecordMagic.begin(), kRecordMagic.end());
  output.push_back(static_cast<std::byte>(record.type));
  output.push_back(static_cast<std::byte>(record.tombstoned ? kTombstoneFlag : 0));
  output.push_back(static_cast<std::byte>(record.claim));
  output.push_back(std::byte{0});
  AppendU64(output, record.version);
  AppendU64(output, record.transaction_lsn);
  AppendU64(output, std::bit_cast<std::uint64_t>(record.valid_from_ns));
  AppendU64(output, std::bit_cast<std::uint64_t>(record.valid_to_ns));
  AppendU64(output, record.supersedes_version);
  AppendU32(output, *id_size);
  AppendU32(output, *identity_size);
  AppendU32(output, *value_size);
  AppendU32(output, *provenance_size);
  AppendU32(output, *domain_size);
  output.insert(output.end(), record.belief_id.begin(), record.belief_id.end());
  output.insert(output.end(),
                reinterpret_cast<const std::byte*>(record.canonical_identity.data()),
                reinterpret_cast<const std::byte*>(record.canonical_identity.data()) +
                    record.canonical_identity.size());
  output.insert(output.end(), record.value.begin(), record.value.end());
  output.insert(output.end(),
                reinterpret_cast<const std::byte*>(record.conflict_domain.data()),
                reinterpret_cast<const std::byte*>(record.conflict_domain.data()) +
                    record.conflict_domain.size());
  for (const auto& range : record.provenance) {
    if (range.first_lsn == 0 || range.last_lsn < range.first_lsn) {
      return std::unexpected(Error{ErrorCode::kInvalidArgument, 0,
                                   record.transaction_lsn});
    }
    AppendU64(output, range.first_lsn);
    AppendU64(output, range.last_lsn);
  }
  return output;
}

std::expected<BeliefRecord, Error> DecodeRecord(
    std::span<const std::byte> encoded) {
  if (encoded.size() < kRecordHeaderBytes ||
      !std::equal(kRecordMagic.begin(), kRecordMagic.end(), encoded.begin())) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  const auto raw_type = std::to_integer<std::uint8_t>(encoded[8]);
  const auto flags = std::to_integer<std::uint8_t>(encoded[9]);
  const auto raw_claim = std::to_integer<std::uint8_t>(encoded[10]);
  if (raw_type > static_cast<std::uint8_t>(schema::BeliefType::identity) ||
      raw_claim >
          static_cast<std::uint8_t>(schema::AssertionClaim::negative_existence) ||
      (flags & static_cast<std::uint8_t>(~kTombstoneFlag)) != 0 ||
      encoded[11] != std::byte{0}) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  auto version = ReadU64(encoded, 12);
  auto transaction_lsn = ReadU64(encoded, 20);
  auto valid_from = ReadU64(encoded, 28);
  auto valid_to = ReadU64(encoded, 36);
  auto supersedes = ReadU64(encoded, 44);
  auto id_size = ReadU32(encoded, 52);
  auto identity_size = ReadU32(encoded, 56);
  auto value_size = ReadU32(encoded, 60);
  auto provenance_size = ReadU32(encoded, 64);
  auto domain_size = ReadU32(encoded, 68);
  if (!version || !transaction_lsn || !valid_from || !valid_to || !supersedes ||
      !id_size || !identity_size || !value_size || !provenance_size ||
      !domain_size ||
      *version == 0 || *transaction_lsn == 0 || *id_size == 0 ||
      *identity_size == 0 || *provenance_size == 0) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  const std::uint64_t variable_bytes =
      static_cast<std::uint64_t>(*id_size) + *identity_size + *value_size +
      *domain_size +
      static_cast<std::uint64_t>(*provenance_size) * 16U;
  if (variable_bytes != encoded.size() - kRecordHeaderBytes) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  std::size_t cursor = kRecordHeaderBytes;
  BeliefRecord record{
      .type = static_cast<schema::BeliefType>(raw_type),
      .belief_id = {encoded.begin() + static_cast<std::ptrdiff_t>(cursor),
                    encoded.begin() + static_cast<std::ptrdiff_t>(cursor + *id_size)},
      .canonical_identity = {},
      .conflict_domain = {},
      .value = {},
      .claim = static_cast<schema::AssertionClaim>(raw_claim),
      .valid_from_ns = std::bit_cast<std::int64_t>(*valid_from),
      .valid_to_ns = std::bit_cast<std::int64_t>(*valid_to),
      .transaction_lsn = *transaction_lsn,
      .version = *version,
      .supersedes_version = *supersedes,
      .provenance = {},
      .conflict_edges = {},
      .tombstoned = (flags & kTombstoneFlag) != 0,
  };
  cursor += *id_size;
  record.canonical_identity.assign(
      reinterpret_cast<const char*>(encoded.data() + cursor), *identity_size);
  cursor += *identity_size;
  record.value.assign(
      encoded.begin() + static_cast<std::ptrdiff_t>(cursor),
      encoded.begin() + static_cast<std::ptrdiff_t>(cursor + *value_size));
  cursor += *value_size;
  record.conflict_domain.assign(
      reinterpret_cast<const char*>(encoded.data() + cursor), *domain_size);
  cursor += *domain_size;
  record.provenance.reserve(*provenance_size);
  for (std::uint32_t index = 0; index < *provenance_size; ++index) {
    auto first = ReadU64(encoded, cursor);
    auto last = ReadU64(encoded, cursor + 8U);
    if (!first || !last || *first == 0 || *last < *first) {
      return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
    }
    record.provenance.push_back(
        BeliefProvenance{.first_lsn = *first, .last_lsn = *last});
    cursor += 16U;
  }
  if ((record.valid_to_ns != 0 &&
       record.valid_to_ns < record.valid_from_ns) ||
      (record.version == 1 && record.supersedes_version != 0) ||
      (record.version > 1 &&
       record.supersedes_version != record.version - 1U) ||
      (record.tombstoned && !record.value.empty())) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  return record;
}

std::expected<std::optional<BeliefRecord>, Error> LoadHead(
    const ReadSnapshot& snapshot, std::span<const std::byte> head_key) {
  auto encoded_version = snapshot.Get(ProjectionId::kBeliefStore, head_key);
  if (!encoded_version) {
    return std::unexpected(encoded_version.error());
  }
  auto maybe_head_value = std::move(encoded_version).value();
  if (!maybe_head_value.has_value()) {
    return std::optional<BeliefRecord>{};
  }
  const auto head_value =
      std::move(maybe_head_value).value_or(std::vector<std::byte>{});
  auto version = ReadU64(head_value, 0);
  if (!version || head_value.size() != 8 || *version == 0) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  const auto version_key = VersionKey(head_key, *version);
  auto encoded_record =
      snapshot.Get(ProjectionId::kBeliefStore, version_key);
  if (!encoded_record) {
    return std::unexpected(encoded_record.error());
  }
  auto maybe_record = std::move(encoded_record).value();
  if (!maybe_record.has_value()) {
    return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
  }
  auto record = DecodeRecord(
      std::move(maybe_record).value_or(std::vector<std::byte>{}));
  if (!record || record->version != *version ||
      !std::ranges::equal(HeadKey(record->type, record->canonical_identity),
                          head_key)) {
    return std::unexpected(record ? Error{ErrorCode::kBeliefCorrupt, 0}
                                  : record.error());
  }
  return std::optional<BeliefRecord>(std::move(*record));
}

std::expected<std::optional<BeliefRecord>, Error> CurrentHead(
    const ReadSnapshot& snapshot, const std::vector<std::byte>& head_key,
    StagedHeads& staged_heads) {
  const auto staged = staged_heads.find(head_key);
  if (staged != staged_heads.end()) {
    return staged->second;
  }
  auto loaded = LoadHead(snapshot, head_key);
  if (!loaded) {
    return std::unexpected(loaded.error());
  }
  staged_heads.emplace(head_key, *loaded);
  return *loaded;
}

std::vector<BeliefProvenance> CopyProvenance(
    const flatbuffers::Vector<const schema::ProvenanceRange*>* provenance) {
  std::vector<BeliefProvenance> copied;
  copied.reserve(provenance->size());
  for (const auto* range : *provenance) {
    copied.push_back(BeliefProvenance{.first_lsn = range->first_lsn(),
                                      .last_lsn = range->last_lsn()});
  }
  return copied;
}

bool SameAssertion(const BeliefRecord& current,
                   const schema::Assertion& assertion) {
  if (current.tombstoned || current.type != assertion.belief_type() ||
      current.canonical_identity !=
          assertion.canonical_identity()->string_view() ||
      current.conflict_domain !=
          (assertion.conflict_domain() == nullptr
               ? std::string_view{}
               : assertion.conflict_domain()->string_view()) ||
      current.claim != assertion.claim() ||
      current.valid_from_ns != assertion.valid_from_ns() ||
      current.valid_to_ns != assertion.valid_to_ns() ||
      current.belief_id.size() != assertion.belief_id()->size() ||
      current.value.size() != assertion.value()->size() ||
      current.provenance.size() != assertion.provenance()->size()) {
    return false;
  }
  if (!std::equal(current.belief_id.begin(), current.belief_id.end(),
                  reinterpret_cast<const std::byte*>(
                      assertion.belief_id()->data())) ||
      !std::equal(current.value.begin(), current.value.end(),
                  reinterpret_cast<const std::byte*>(assertion.value()->data()))) {
    return false;
  }
  for (flatbuffers::uoffset_t index = 0;
       index < assertion.provenance()->size(); ++index) {
    const auto* range = assertion.provenance()->Get(index);
    const auto current_index = static_cast<std::size_t>(index);
    if (current.provenance[current_index].first_lsn != range->first_lsn() ||
        current.provenance[current_index].last_lsn != range->last_lsn()) {
      return false;
    }
  }
  return true;
}

void Put(std::vector<OwnedMutation>& mutations, std::vector<std::byte> key,
         std::vector<std::byte> value) {
  mutations.push_back(OwnedMutation{.kind = MutationKind::kPut,
                                    .key = std::move(key),
                                    .value = std::move(value)});
}

// PolicyRejection classifies write-gate outcomes that are input policy, not
// store corruption. At the socket admission boundary they surface as typed
// rejections before commit; at apply time they are deterministic skips so a
// legacy or imported log can never make replay unbootable.
bool PolicyRejection(ErrorCode code) {
  return code == ErrorCode::kNegativeExistenceUncorroborated ||
         code == ErrorCode::kBeliefIdConflict ||
         code == ErrorCode::kBeliefNotFound;
}

bool ProvenanceCoversAny(
    const flatbuffers::Vector<const schema::ProvenanceRange*>* provenance,
    std::span<const std::uint64_t> lsns) {
  for (const auto* range : *provenance) {
    for (const auto lsn : lsns) {
      if (range->first_lsn() <= lsn && lsn <= range->last_lsn()) {
        return true;
      }
    }
  }
  return false;
}

std::expected<void, Error> ApplyAssertion(
    const ReadSnapshot& snapshot, const schema::Assertion& assertion,
    std::uint64_t transaction_lsn, StagedHeads& staged_heads,
    StagedIds& staged_ids, StagedDomains& staged_domains,
    StagedConflicts& staged_conflicts,
    std::set<std::vector<std::byte>>& dirty_domains,
    std::set<std::vector<std::byte>>& dirty_conflicts,
    std::vector<OwnedMutation>& mutations,
    std::span<const std::uint64_t> in_batch_tool_results = {}) {
  const auto identity = assertion.canonical_identity()->string_view();
  auto identity_size = CheckedU32(identity.size());
  auto belief_id_size = CheckedU32(assertion.belief_id()->size());
  if (!identity_size || !belief_id_size) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0, transaction_lsn});
  }
  const auto head_key = HeadKey(assertion.belief_type(), identity);
  const std::span<const std::byte> belief_id(
      reinterpret_cast<const std::byte*>(assertion.belief_id()->data()),
      assertion.belief_id()->size());
  const auto id_key = IdKey(belief_id);
  std::optional<std::vector<std::byte>> indexed_head;
  const auto staged_id = staged_ids.find(id_key);
  if (staged_id != staged_ids.end()) {
    indexed_head = staged_id->second;
  } else {
    auto stored_id = snapshot.Get(ProjectionId::kBeliefStore, id_key);
    if (!stored_id) {
      return std::unexpected(stored_id.error());
    }
    indexed_head = std::move(*stored_id);
  }
  if (indexed_head.has_value() && indexed_head.value() != head_key) {
    return std::unexpected(
        Error{ErrorCode::kBeliefIdConflict, 0, transaction_lsn});
  }
  auto current = CurrentHead(snapshot, head_key, staged_heads);
  if (!current) {
    return std::unexpected(current.error());
  }
  auto current_head = std::move(current).value();
  const auto incoming_domain =
      assertion.conflict_domain() == nullptr
          ? std::string_view{}
          : assertion.conflict_domain()->string_view();
  if (current_head.has_value() &&
      current_head.value().conflict_domain != incoming_domain) {
    return std::unexpected(
        Error{ErrorCode::kBeliefIdConflict, 0, transaction_lsn});
  }
  auto corroborated = HasCorroboratingToolResult(
      snapshot, assertion.provenance(), transaction_lsn);
  if (!corroborated) {
    return std::unexpected(corroborated.error());
  }
  if (!*corroborated && !in_batch_tool_results.empty() &&
      ProvenanceCoversAny(assertion.provenance(), in_batch_tool_results)) {
    corroborated = true;
  }
  std::vector<std::vector<std::byte>> existing_keys;
  std::vector<BeliefRecord> existing_records;
  std::vector<gate::ExistingBelief> existing_beliefs;
  std::optional<std::vector<std::byte>> domain_key;
  DomainMembers* domain_members = nullptr;
  if (assertion.conflict_domain() != nullptr) {
    domain_key = DomainKey(assertion.belief_type(),
                           assertion.conflict_domain()->string_view());
    auto loaded_members =
        MutableDomain(snapshot, domain_key.value(), staged_domains);
    if (!loaded_members) {
      return std::unexpected(loaded_members.error());
    }
    domain_members = *loaded_members;
    existing_keys.reserve(domain_members->size());
    existing_records.reserve(domain_members->size());
    for (const auto& member : *domain_members) {
      if (member == head_key) {
        continue;
      }
      auto existing = CurrentHead(snapshot, member, staged_heads);
      if (!existing) {
        return std::unexpected(existing.error());
      }
      auto maybe_existing = std::move(existing).value();
      if (!maybe_existing.has_value()) {
        return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0,
                                     transaction_lsn});
      }
      auto record =
          std::move(maybe_existing).value_or(BeliefRecord{});
      if (record.type != assertion.belief_type() ||
          record.conflict_domain !=
              assertion.conflict_domain()->string_view()) {
        return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0,
                                     transaction_lsn});
      }
      existing_keys.push_back(member);
      existing_records.push_back(std::move(record));
    }
    existing_beliefs.reserve(existing_records.size());
    for (const auto& record : existing_records) {
      existing_beliefs.push_back(gate::ExistingBelief{
          .type = record.type,
          .canonical_identity = record.canonical_identity,
          .conflict_domain = record.conflict_domain,
          .tombstoned = record.tombstoned,
      });
    }
  }
  auto gate_decision = gate::EvaluateBelief(
      assertion, *corroborated, existing_beliefs, transaction_lsn);
  if (!gate_decision) {
    return std::unexpected(gate_decision.error());
  }
  if (current_head.has_value() &&
      current_head.value().belief_id.size() == belief_id.size() &&
      std::equal(current_head.value().belief_id.begin(),
                 current_head.value().belief_id.end(), belief_id.begin())) {
    if (!SameAssertion(current_head.value(), assertion)) {
      return std::unexpected(
          Error{ErrorCode::kBeliefIdConflict, 0, transaction_lsn});
    }
    if (domain_key.has_value() && domain_members != nullptr &&
        !std::binary_search(domain_members->begin(), domain_members->end(),
                            head_key)) {
      domain_members->push_back(head_key);
      std::sort(domain_members->begin(), domain_members->end());
      dirty_domains.insert(domain_key.value());
    }
    return {};
  }
  if (indexed_head.has_value()) {
    return std::unexpected(
        Error{ErrorCode::kBeliefIdConflict, 0, transaction_lsn});
  }
  const BeliefRecord next{
      .type = assertion.belief_type(),
      .belief_id = {belief_id.begin(), belief_id.end()},
      .canonical_identity = std::string(identity),
      .conflict_domain = assertion.conflict_domain() == nullptr
                             ? std::string{}
                             : std::string(
                                   assertion.conflict_domain()->string_view()),
      .value = {reinterpret_cast<const std::byte*>(assertion.value()->data()),
                reinterpret_cast<const std::byte*>(assertion.value()->data()) +
                    assertion.value()->size()},
      .claim = assertion.claim(),
      .valid_from_ns = assertion.valid_from_ns(),
      .valid_to_ns = assertion.valid_to_ns(),
      .transaction_lsn = transaction_lsn,
      .version = current_head.has_value() ? current_head.value().version + 1U : 1U,
      .supersedes_version =
          current_head.has_value() ? current_head.value().version : 0,
      .provenance = CopyProvenance(assertion.provenance()),
      .conflict_edges = {},
      .tombstoned = false,
  };
  auto encoded = EncodeRecord(next);
  if (!encoded) {
    return std::unexpected(encoded.error());
  }
  Put(mutations, VersionKey(head_key, next.version), std::move(*encoded));
  Put(mutations, head_key, EncodeHead(next.version));
  Put(mutations, id_key, head_key);
  staged_heads[head_key] = next;
  staged_ids[id_key] = head_key;
  if (domain_key.has_value() && domain_members != nullptr) {
    if (!std::binary_search(domain_members->begin(), domain_members->end(),
                            head_key)) {
      domain_members->push_back(head_key);
      std::sort(domain_members->begin(), domain_members->end());
      dirty_domains.insert(domain_key.value());
    }
    for (const auto conflict_index :
         gate_decision->conflicting_existing) {
      if (conflict_index >= existing_keys.size()) {
        return std::unexpected(Error{ErrorCode::kInvariantViolation, 0,
                                     transaction_lsn});
      }
      const auto& other_key = existing_keys[conflict_index];
      auto own_conflicts =
          MutableConflicts(snapshot, head_key, staged_conflicts);
      auto other_conflicts =
          MutableConflicts(snapshot, other_key, staged_conflicts);
      if (!own_conflicts || !other_conflicts) {
        return std::unexpected(own_conflicts ? other_conflicts.error()
                                             : own_conflicts.error());
      }
      if (AddConflict(**own_conflicts, other_key, transaction_lsn)) {
        dirty_conflicts.insert(head_key);
      }
      if (AddConflict(**other_conflicts, head_key, transaction_lsn)) {
        dirty_conflicts.insert(other_key);
      }
    }
  }
  return {};
}

std::expected<void, Error> ApplyRetraction(
    const ReadSnapshot& snapshot, const schema::Retract& retraction,
    std::uint64_t transaction_lsn, StagedHeads& staged_heads,
    StagedIds& staged_ids, StagedConflicts& staged_conflicts,
    std::set<std::vector<std::byte>>& dirty_conflicts,
    std::vector<OwnedMutation>& mutations) {
  const std::span<const std::byte> belief_id(
      reinterpret_cast<const std::byte*>(retraction.belief_id()->data()),
      retraction.belief_id()->size());
  const auto id_key = IdKey(belief_id);
  std::optional<std::vector<std::byte>> head_key;
  const auto staged_id = staged_ids.find(id_key);
  if (staged_id != staged_ids.end()) {
    head_key = staged_id->second;
  } else {
    auto stored_id = snapshot.Get(ProjectionId::kBeliefStore, id_key);
    if (!stored_id) {
      return std::unexpected(stored_id.error());
    }
    head_key = std::move(*stored_id);
  }
  if (!head_key.has_value()) {
    return std::unexpected(
        Error{ErrorCode::kBeliefNotFound, 0, transaction_lsn});
  }
  auto current = CurrentHead(snapshot, head_key.value(), staged_heads);
  if (!current) {
    return std::unexpected(current.error());
  }
  auto current_head = std::move(current).value();
  if (!current_head.has_value()) {
    return std::unexpected(
        Error{ErrorCode::kBeliefCorrupt, 0, transaction_lsn});
  }
  if (current_head.value().belief_id.size() != belief_id.size() ||
      !std::equal(current_head.value().belief_id.begin(),
                  current_head.value().belief_id.end(), belief_id.begin())) {
    return std::unexpected(
        Error{ErrorCode::kBeliefIdConflict, 0, transaction_lsn});
  }
  if (current_head.value().tombstoned) {
    return {};
  }
  BeliefRecord tombstone = current_head.value();
  tombstone.value.clear();
  tombstone.transaction_lsn = transaction_lsn;
  tombstone.version += 1U;
  tombstone.supersedes_version = current_head.value().version;
  tombstone.provenance = CopyProvenance(retraction.provenance());
  tombstone.tombstoned = true;
  auto encoded = EncodeRecord(tombstone);
  if (!encoded) {
    return std::unexpected(encoded.error());
  }
  Put(mutations, VersionKey(head_key.value(), tombstone.version),
      std::move(*encoded));
  Put(mutations, head_key.value(), EncodeHead(tombstone.version));
  staged_heads[head_key.value()] = tombstone;
  auto own_conflicts =
      MutableConflicts(snapshot, head_key.value(), staged_conflicts);
  if (!own_conflicts) {
    return std::unexpected(own_conflicts.error());
  }
  std::vector<std::vector<std::byte>> active_peers;
  for (const auto& conflict : **own_conflicts) {
    if (conflict.resolved_lsn == 0) {
      active_peers.push_back(conflict.other_head_key);
    }
  }
  if (ResolveConflicts(**own_conflicts, transaction_lsn)) {
    dirty_conflicts.insert(head_key.value());
  }
  for (const auto& peer : active_peers) {
    auto peer_conflicts =
        MutableConflicts(snapshot, peer, staged_conflicts);
    if (!peer_conflicts) {
      return std::unexpected(peer_conflicts.error());
    }
    if (!ResolveConflictWith(**peer_conflicts, head_key.value(),
                             transaction_lsn)) {
      return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0,
                                   transaction_lsn});
    }
    dirty_conflicts.insert(peer);
  }
  return {};
}

std::expected<std::vector<BeliefConflictEdge>, Error> LoadConflictEdges(
    const ReadSnapshot& snapshot, const std::vector<std::byte>& head_key,
    std::uint64_t transaction_lsn) {
  auto stored =
      snapshot.Get(ProjectionId::kBeliefStore, ConflictKey(head_key));
  if (!stored) {
    return std::unexpected(stored.error());
  }
  auto maybe_stored = std::move(stored).value();
  if (!maybe_stored.has_value()) {
    return std::vector<BeliefConflictEdge>{};
  }
  auto decoded = DecodeStoredConflicts(
      std::move(maybe_stored).value_or(std::vector<std::byte>{}));
  if (!decoded) {
    return std::unexpected(decoded.error());
  }
  std::vector<BeliefConflictEdge> edges;
  for (const auto& conflict : *decoded) {
    if (conflict.created_lsn > transaction_lsn ||
        (conflict.resolved_lsn != 0 &&
         conflict.resolved_lsn <= transaction_lsn)) {
      continue;
    }
    auto other = DecodeHeadKey(conflict.other_head_key);
    if (!other) {
      return std::unexpected(other.error());
    }
    edges.push_back(BeliefConflictEdge{
        .other_type = other->type,
        .other_canonical_identity = std::move(other->canonical_identity),
        .created_lsn = conflict.created_lsn,
        .resolved_lsn = conflict.resolved_lsn,
        .obligated_surfacing = true,
    });
  }
  return edges;
}

bool ValidAt(const BeliefRecord& record, std::int64_t valid_time_ns) {
  return record.valid_from_ns <= valid_time_ns &&
         (record.valid_to_ns == 0 || valid_time_ns <= record.valid_to_ns);
}

}  // namespace

std::expected<void, Error> BeliefProjection::ApplyEvent(
    ProjectionStore& store, const log::Frame& frame) {
  auto verified = events::VerifyEvent(frame.sealed_payload, frame.header.kind,
                                      events::Boundary::kDisk);
  if (!verified) {
    auto error = verified.error();
    error.lsn = frame.header.lsn;
    return std::unexpected(error);
  }
  auto snapshot = store.BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  StagedHeads staged_heads;
  StagedIds staged_ids;
  StagedDomains staged_domains;
  StagedConflicts staged_conflicts;
  std::set<std::vector<std::byte>> dirty_domains;
  std::set<std::vector<std::byte>> dirty_conflicts;
  std::vector<OwnedMutation> owned;
  if (frame.header.kind == log::EventKind::kToolResult &&
      verified->envelope->payload_as_ToolResult()->status() ==
          schema::ResultStatus::ok) {
    Put(owned, ToolResultKey(frame.header.lsn), {std::byte{1}});
  }
  if (frame.header.kind == log::EventKind::kAssertion) {
    owned.reserve(8);
    auto applied = ApplyAssertion(
        *snapshot, *verified->envelope->payload_as_Assertion(), frame.header.lsn,
        staged_heads, staged_ids, staged_domains, staged_conflicts,
        dirty_domains, dirty_conflicts, owned);
    if (!applied && !PolicyRejection(applied.error().code)) {
      return std::unexpected(applied.error());
    }
  } else if (frame.header.kind == log::EventKind::kConsolidation) {
    const auto* assertions =
        verified->envelope->payload_as_Consolidation()->assertions();
    owned.reserve(static_cast<std::size_t>(assertions->size()) * 8U);
    bool policy_rejected = false;
    for (const auto* assertion : *assertions) {
      auto applied = ApplyAssertion(*snapshot, *assertion, frame.header.lsn,
                                    staged_heads, staged_ids, staged_domains,
                                    staged_conflicts, dirty_domains,
                                    dirty_conflicts, owned);
      if (!applied) {
        if (!PolicyRejection(applied.error().code)) {
          return std::unexpected(applied.error());
        }
        policy_rejected = true;
        break;
      }
    }
    if (policy_rejected) {
      owned.clear();
      dirty_domains.clear();
      dirty_conflicts.clear();
    }
  } else if (frame.header.kind == log::EventKind::kRetract) {
    owned.reserve(2);
    auto applied = ApplyRetraction(
        *snapshot, *verified->envelope->payload_as_Retract(), frame.header.lsn,
        staged_heads, staged_ids, staged_conflicts, dirty_conflicts, owned);
    if (!applied && !PolicyRejection(applied.error().code)) {
      return std::unexpected(applied.error());
    }
  }
  for (const auto& key : dirty_domains) {
    auto encoded = EncodeDomainMembers(staged_domains[key]);
    if (!encoded) {
      return std::unexpected(encoded.error());
    }
    Put(owned, key, std::move(*encoded));
  }
  for (const auto& head_key : dirty_conflicts) {
    auto encoded = EncodeStoredConflicts(staged_conflicts[head_key]);
    if (!encoded) {
      return std::unexpected(encoded.error());
    }
    Put(owned, ConflictKey(head_key), std::move(*encoded));
  }
  std::vector<Mutation> mutations;
  mutations.reserve(owned.size());
  for (const auto& mutation : owned) {
    mutations.push_back(Mutation{.kind = mutation.kind,
                                 .key = mutation.key,
                                 .value = mutation.value});
  }
  return store.ApplyInternal(ProjectionId::kBeliefStore, frame.header.lsn,
                             mutations);
}

std::expected<void, Error> BeliefProjection::AdmitBatch(
    ProjectionStore& store, std::span<const AdmissionEvent> events) {
  auto snapshot = store.BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  StagedHeads staged_heads;
  StagedIds staged_ids;
  StagedDomains staged_domains;
  StagedConflicts staged_conflicts;
  std::set<std::vector<std::byte>> dirty_domains;
  std::set<std::vector<std::byte>> dirty_conflicts;
  std::vector<OwnedMutation> discarded;
  std::vector<std::uint64_t> in_batch_tool_results;
  for (const auto& event : events) {
    switch (event.kind) {
      case log::EventKind::kToolResult:
        if (event.envelope->payload_as_ToolResult()->status() ==
            schema::ResultStatus::ok) {
          in_batch_tool_results.push_back(event.assigned_lsn);
        }
        break;
      case log::EventKind::kAssertion: {
        auto admitted = ApplyAssertion(
            *snapshot, *event.envelope->payload_as_Assertion(),
            event.assigned_lsn, staged_heads, staged_ids, staged_domains,
            staged_conflicts, dirty_domains, dirty_conflicts, discarded,
            in_batch_tool_results);
        if (!admitted) {
          return std::unexpected(admitted.error());
        }
        break;
      }
      case log::EventKind::kConsolidation: {
        const auto* assertions =
            event.envelope->payload_as_Consolidation()->assertions();
        for (const auto* assertion : *assertions) {
          auto admitted = ApplyAssertion(
              *snapshot, *assertion, event.assigned_lsn, staged_heads,
              staged_ids, staged_domains, staged_conflicts, dirty_domains,
              dirty_conflicts, discarded, in_batch_tool_results);
          if (!admitted) {
            return std::unexpected(admitted.error());
          }
        }
        break;
      }
      case log::EventKind::kRetract: {
        auto admitted = ApplyRetraction(
            *snapshot, *event.envelope->payload_as_Retract(),
            event.assigned_lsn, staged_heads, staged_ids, staged_conflicts,
            dirty_conflicts, discarded);
        if (!admitted) {
          return std::unexpected(admitted.error());
        }
        break;
      }
      default:
        break;
    }
  }
  return {};
}

std::expected<BeliefRebuildProgress, Error> BeliefProjection::Rebuild(
    ProjectionStore& store, std::span<const log::Frame> frames, bool reset,
    std::size_t maximum_frames) {
  if (reset) {
    auto reset_result = store.Reset(ProjectionId::kBeliefStore);
    if (!reset_result) {
      return std::unexpected(reset_result.error());
    }
  }
  std::uint64_t checkpoint = 0;
  {
    auto snapshot = store.BeginSnapshot();
    if (!snapshot) {
      return std::unexpected(snapshot.error());
    }
    auto read_checkpoint = snapshot->Checkpoint(ProjectionId::kBeliefStore);
    if (!read_checkpoint) {
      return std::unexpected(read_checkpoint.error());
    }
    checkpoint = *read_checkpoint;
  }
  if (checkpoint > frames.size()) {
    return std::unexpected(
        Error{ErrorCode::kProjectionCheckpoint, 0, checkpoint, frames.size()});
  }
  const auto remaining = frames.size() - static_cast<std::size_t>(checkpoint);
  const auto apply_count = std::min(remaining, maximum_frames);
  const auto begin = static_cast<std::size_t>(checkpoint);
  for (std::size_t index = begin; index < begin + apply_count; ++index) {
    if (frames[index].header.lsn != static_cast<std::uint64_t>(index) + 1U) {
      return std::unexpected(Error{ErrorCode::kSequenceViolation, 0,
                                   frames[index].header.lsn, index});
    }
    auto applied = ApplyEvent(store, frames[index]);
    if (!applied) {
      return std::unexpected(applied.error());
    }
  }
  const auto applied_lsn = checkpoint + static_cast<std::uint64_t>(apply_count);
  return BeliefRebuildProgress{.applied_lsn = applied_lsn,
                               .applied_frames = apply_count,
                               .complete = applied_lsn == frames.size()};
}

std::expected<std::optional<BeliefRecord>, Error> BeliefProjection::ReadAsOf(
    const ReadSnapshot& snapshot, schema::BeliefType type,
    std::string_view canonical_identity, BeliefAsOf as_of) {
  if (canonical_identity.empty() ||
      canonical_identity.size() > std::numeric_limits<std::uint32_t>::max() ||
      as_of.transaction_lsn == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  const auto head_key = HeadKey(type, canonical_identity);
  auto head = LoadHead(snapshot, head_key);
  if (!head) {
    return std::unexpected(head.error());
  }
  auto maybe_head = std::move(head).value();
  if (!maybe_head.has_value()) {
    return std::optional<BeliefRecord>{};
  }
  const auto initial_head =
      std::move(maybe_head).value_or(BeliefRecord{});
  std::uint64_t version = initial_head.version;
  while (version != 0) {
    const auto key = VersionKey(head_key, version);
    auto encoded = snapshot.Get(ProjectionId::kBeliefStore, key);
    if (!encoded) {
      return std::unexpected(encoded.error());
    }
    auto maybe_record = std::move(encoded).value();
    if (!maybe_record.has_value()) {
      return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
    }
    auto record = DecodeRecord(
        std::move(maybe_record).value_or(std::vector<std::byte>{}));
    if (!record || record->type != type ||
        record->canonical_identity != canonical_identity ||
        record->version != version) {
      return std::unexpected(record ? Error{ErrorCode::kBeliefCorrupt, 0}
                                    : record.error());
    }
    if (record->transaction_lsn <= as_of.transaction_lsn) {
      if (record->tombstoned) {
        return std::optional<BeliefRecord>{};
      }
      if (ValidAt(*record, as_of.valid_time_ns)) {
        auto conflicts =
            LoadConflictEdges(snapshot, head_key, as_of.transaction_lsn);
        if (!conflicts) {
          return std::unexpected(conflicts.error());
        }
        record->conflict_edges = std::move(*conflicts);
        return std::optional<BeliefRecord>(std::move(*record));
      }
    }
    version = record->supersedes_version;
  }
  return std::optional<BeliefRecord>{};
}

std::expected<std::vector<BeliefRecord>, Error> BeliefProjection::ReadHeads(
    const ReadSnapshot& snapshot, std::size_t limit) {
  if (limit == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  const std::array<std::byte, 1> prefix = {kHeadPrefix};
  auto items = snapshot.ScanPrefix(ProjectionId::kBeliefStore, prefix, limit);
  if (!items) {
    return std::unexpected(items.error());
  }
  auto checkpoint = snapshot.Checkpoint(ProjectionId::kBeliefStore);
  if (!checkpoint) {
    return std::unexpected(checkpoint.error());
  }
  std::vector<BeliefRecord> heads;
  heads.reserve(items->size());
  for (const auto& item : *items) {
    auto record = LoadHead(snapshot, item.key);
    if (!record) {
      return std::unexpected(record.error());
    }
    auto maybe_record = std::move(*record);
    if (!maybe_record.has_value()) {
      return std::unexpected(Error{ErrorCode::kBeliefCorrupt, 0});
    }
    auto value = std::move(*maybe_record);
    if (!value.tombstoned) {
      auto conflicts = LoadConflictEdges(snapshot, item.key, *checkpoint);
      if (!conflicts) {
        return std::unexpected(conflicts.error());
      }
      value.conflict_edges = std::move(*conflicts);
      heads.push_back(std::move(value));
    }
  }
  return heads;
}

}  // namespace neocortex::proj
