#include "proj/entity/entity_index.h"

#include <algorithm>
#include <array>
#include <compare>
#include <cstring>
#include <memory>
#include <optional>
#include <set>
#include <utility>

#include <roaring/roaring64.h>

#include "schema/events.h"

namespace neocortex::proj {
namespace {

constexpr std::byte kPostingPrefix{0x45};

bool IsAsciiAlpha(char value) {
  return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z');
}

bool IsAsciiDigit(char value) { return value >= '0' && value <= '9'; }

bool IsAsciiAlnum(char value) {
  return IsAsciiAlpha(value) || IsAsciiDigit(value);
}

bool IsAsciiHex(char value) {
  return IsAsciiDigit(value) || (value >= 'a' && value <= 'f') ||
         (value >= 'A' && value <= 'F');
}

char LowerAscii(char value) {
  return value >= 'A' && value <= 'Z'
             ? static_cast<char>(value - 'A' + 'a')
             : value;
}

std::string Canonical(std::string_view value) {
  std::string output;
  output.reserve(value.size());
  for (const char character : value) {
    output.push_back(LowerAscii(character));
  }
  return output;
}

bool StartsWithInsensitive(std::string_view text, std::size_t offset,
                           std::string_view prefix) {
  if (prefix.size() > text.size() - std::min(offset, text.size())) {
    return false;
  }
  for (std::size_t index = 0; index < prefix.size(); ++index) {
    if (LowerAscii(text[offset + index]) != LowerAscii(prefix[index])) {
      return false;
    }
  }
  return true;
}

bool IsUrlTerminator(char value) {
  return value <= ' ' || value == '"' || value == '\'' || value == '<' ||
         value == '>' || value == '`';
}

bool IsTrailingPunctuation(char value) {
  return value == '.' || value == ',' || value == ';' || value == '!' ||
         value == '?' || value == ')' || value == ']' || value == '}';
}

bool IsDomain(std::string_view value) {
  if (value.size() < 4 || value.front() == '.' || value.back() == '.') {
    return false;
  }
  bool saw_dot = false;
  bool label_has_value = false;
  std::size_t last_dot = std::string_view::npos;
  for (std::size_t index = 0; index < value.size(); ++index) {
    const char character = value[index];
    if (character == '.') {
      if (!label_has_value ||
          (index != 0 && value[index - 1] == '-')) {
        return false;
      }
      saw_dot = true;
      label_has_value = false;
      last_dot = index;
      continue;
    }
    if (!IsAsciiAlnum(character) && character != '-') {
      return false;
    }
    if (!label_has_value && character == '-') {
      return false;
    }
    label_has_value = true;
  }
  if (!saw_dot || !label_has_value || value.back() == '-' ||
      last_dot == std::string_view::npos || value.size() - last_dot - 1U < 2U) {
    return false;
  }
  return std::all_of(value.begin() + static_cast<std::ptrdiff_t>(last_dot + 1U),
                     value.end(), IsAsciiAlpha);
}

bool IsUuid(std::string_view value) {
  constexpr std::array<std::size_t, 4> kHyphens = {8, 13, 18, 23};
  if (value.size() != 36) {
    return false;
  }
  for (std::size_t index = 0; index < value.size(); ++index) {
    if (std::find(kHyphens.begin(), kHyphens.end(), index) != kHyphens.end()) {
      if (value[index] != '-') {
        return false;
      }
    } else if (!IsAsciiHex(value[index])) {
      return false;
    }
  }
  return true;
}

bool IsHexIdentifier(std::string_view value) {
  std::size_t begin = 0;
  std::size_t minimum = 16;
  if (value.size() >= 2 && value[0] == '0' && LowerAscii(value[1]) == 'x') {
    begin = 2;
    minimum = 4;
  }
  if (value.size() - begin < minimum) {
    return false;
  }
  return std::all_of(value.begin() + static_cast<std::ptrdiff_t>(begin),
                     value.end(), IsAsciiHex);
}

bool IsStructuredIdentifier(std::string_view value) {
  if (value.size() < 6) {
    return false;
  }
  bool saw_alpha = false;
  bool saw_digit = false;
  bool saw_separator = false;
  for (const char character : value) {
    saw_alpha = saw_alpha || IsAsciiAlpha(character);
    saw_digit = saw_digit || IsAsciiDigit(character);
    saw_separator = saw_separator || character == '_' || character == ':' ||
                    character == '-';
    if (!IsAsciiAlnum(character) && character != '_' && character != ':' &&
        character != '-') {
      return false;
    }
  }
  return saw_alpha && saw_digit && saw_separator;
}

bool IsTokenCharacter(char value) {
  return IsAsciiAlnum(value) || value == '.' || value == '_' || value == '-' ||
         value == ':' || value == '@';
}

bool IsPathCharacter(char value) {
  return IsAsciiAlnum(value) || value == '/' || value == '.' || value == '_' ||
         value == '-' || value == '~' || value == ':' || value == '@' ||
         value == '+' || value == '=';
}

// Canonical values become LMDB posting keys; the bound keeps every derived key
// below the backend key-size limit for arbitrary committed text, symmetrically
// on the apply and query paths.
constexpr std::size_t kMaximumCanonicalBytes = events::kMaximumIdentifierBytes;

void Add(std::vector<ExtractedEntity>& output, EntityKind kind,
         std::string_view value) {
  if (!value.empty() && value.size() <= kMaximumCanonicalBytes) {
    output.push_back(ExtractedEntity{.kind = kind,
                                     .canonical = Canonical(value)});
  }
}

void AddDomainAndAlias(std::vector<ExtractedEntity>& output,
                       std::string_view domain) {
  if (!IsDomain(domain)) {
    return;
  }
  Add(output, EntityKind::kDomain, domain);
  std::size_t label_begin = 0;
  const auto first_dot = domain.find('.');
  if (first_dot == 3 &&
      StartsWithInsensitive(domain, 0, std::string_view("www"))) {
    label_begin = 4;
  }
  const auto label_end = domain.find('.', label_begin);
  const auto label = domain.substr(label_begin, label_end - label_begin);
  if (label.size() >= 3) {
    Add(output, EntityKind::kProperName, label);
  }
}

std::size_t ExtractUrl(std::string_view text, std::size_t begin,
                       std::vector<ExtractedEntity>& output) {
  const bool is_http = StartsWithInsensitive(text, begin, "http://");
  const bool is_https = StartsWithInsensitive(text, begin, "https://");
  if (!is_http && !is_https) {
    return begin;
  }
  std::size_t end = begin;
  while (end < text.size() && !IsUrlTerminator(text[end])) {
    ++end;
  }
  while (end > begin && IsTrailingPunctuation(text[end - 1])) {
    --end;
  }
  const auto url = text.substr(begin, end - begin);
  Add(output, EntityKind::kUrl, url);
  const std::size_t scheme_bytes = is_https ? 8U : 7U;
  const std::size_t host_begin = begin + scheme_bytes;
  std::size_t host_end = host_begin;
  while (host_end < end && text[host_end] != '/' && text[host_end] != ':' &&
         text[host_end] != '?' && text[host_end] != '#') {
    ++host_end;
  }
  AddDomainAndAlias(output, text.substr(host_begin, host_end - host_begin));
  return end;
}

void ExtractProperNames(std::string_view text,
                        std::vector<ExtractedEntity>& output) {
  std::optional<std::string> previous;
  std::size_t offset = 0;
  while (offset < text.size()) {
    bool breaks_sequence = false;
    while (offset < text.size() && !IsAsciiAlpha(text[offset])) {
      breaks_sequence = breaks_sequence ||
                        (text[offset] != ' ' && text[offset] != '\t' &&
                         text[offset] != '\n' && text[offset] != '\r');
      ++offset;
    }
    if (breaks_sequence) {
      previous.reset();
    }
    const std::size_t begin = offset;
    while (offset < text.size() &&
           (IsAsciiAlpha(text[offset]) || text[offset] == '\'' ||
            text[offset] == '-')) {
      ++offset;
    }
    if (begin == offset) {
      continue;
    }
    const auto word = text.substr(begin, offset - begin);
    const bool capitalized = word.size() >= 3 && word.front() >= 'A' &&
                             word.front() <= 'Z';
    if (!capitalized) {
      previous.reset();
      continue;
    }
    Add(output, EntityKind::kProperName, word);
    if (previous.has_value()) {
      std::string compound = *previous;
      compound.push_back(' ');
      compound.append(word);
      Add(output, EntityKind::kProperName, compound);
    }
    previous = std::string(word);
  }
}

void ExtractBase(std::string_view text, std::vector<ExtractedEntity>& output) {
  std::size_t offset = 0;
  while (offset < text.size()) {
    const std::size_t url_end = ExtractUrl(text, offset, output);
    if (url_end != offset) {
      offset = url_end;
      continue;
    }
    if (text[offset] == '/' && offset + 1U < text.size() &&
        IsPathCharacter(text[offset + 1U]) &&
        (offset == 0 ||
         (text[offset - 1U] != ':' && text[offset - 1U] != '/'))) {
      std::size_t end = offset + 1U;
      while (end < text.size() && IsPathCharacter(text[end])) {
        ++end;
      }
      while (end > offset + 1U && IsTrailingPunctuation(text[end - 1])) {
        --end;
      }
      Add(output, EntityKind::kPath, text.substr(offset, end - offset));
      offset = end;
      continue;
    }
    if (!IsTokenCharacter(text[offset])) {
      ++offset;
      continue;
    }
    const std::size_t begin = offset;
    while (offset < text.size() && IsTokenCharacter(text[offset])) {
      ++offset;
    }
    std::string_view token = text.substr(begin, offset - begin);
    while (!token.empty() &&
           (token.front() == '.' || token.front() == ':' ||
            token.front() == '@')) {
      token.remove_prefix(1);
    }
    while (!token.empty() &&
           (token.back() == '.' || token.back() == ':' ||
            token.back() == '@')) {
      token.remove_suffix(1);
    }
    const auto at = token.rfind('@');
    if (at != std::string_view::npos && at + 1U < token.size()) {
      AddDomainAndAlias(output, token.substr(at + 1U));
    } else if (IsDomain(token)) {
      AddDomainAndAlias(output, token);
    } else if (IsUuid(token) || IsStructuredIdentifier(token)) {
      Add(output, EntityKind::kStructuredId, token);
    } else if (IsHexIdentifier(token)) {
      Add(output, EntityKind::kHexId, token);
    }
  }
  ExtractProperNames(text, output);
}

void SortUnique(std::vector<ExtractedEntity>& entities) {
  std::sort(entities.begin(), entities.end());
  entities.erase(std::unique(entities.begin(), entities.end()), entities.end());
}

std::vector<std::byte> PostingKey(const ExtractedEntity& entity) {
  std::vector<std::byte> key;
  key.reserve(entity.canonical.size() + 2U);
  key.push_back(kPostingPrefix);
  key.push_back(static_cast<std::byte>(entity.kind));
  key.insert(key.end(), reinterpret_cast<const std::byte*>(entity.canonical.data()),
             reinterpret_cast<const std::byte*>(entity.canonical.data()) +
                 entity.canonical.size());
  return key;
}

struct BitmapDeleter final {
  void operator()(roaring64_bitmap_t* bitmap) const {
    if (bitmap != nullptr) {
      roaring64_bitmap_free(bitmap);
    }
  }
};

using Bitmap = std::unique_ptr<roaring64_bitmap_t, BitmapDeleter>;

std::expected<Bitmap, Error> DecodePosting(
    const std::optional<std::vector<std::byte>>& encoded) {
  if (!encoded.has_value()) {
    Bitmap created(roaring64_bitmap_create());
    if (!created) {
      return std::unexpected(Error{ErrorCode::kBackendUnavailable, 0});
    }
    return created;
  }
  const auto& bytes = *encoded;
  if (bytes.empty()) {
    return std::unexpected(Error{ErrorCode::kEntityIndexCorrupt, 0});
  }
  const auto size = roaring64_bitmap_portable_deserialize_size(
      reinterpret_cast<const char*>(bytes.data()), bytes.size());
  if (size != bytes.size()) {
    return std::unexpected(Error{ErrorCode::kEntityIndexCorrupt, 0});
  }
  Bitmap bitmap(roaring64_bitmap_portable_deserialize_safe(
      reinterpret_cast<const char*>(bytes.data()), bytes.size()));
  if (!bitmap) {
    return std::unexpected(Error{ErrorCode::kEntityIndexCorrupt, 0});
  }
  return bitmap;
}

std::vector<std::byte> EncodePosting(const roaring64_bitmap_t* bitmap) {
  const auto size = roaring64_bitmap_portable_size_in_bytes(bitmap);
  std::vector<std::byte> encoded(size);
  const auto written = roaring64_bitmap_portable_serialize(
      bitmap, reinterpret_cast<char*>(encoded.data()));
  encoded.resize(written);
  return encoded;
}

template <typename Scalar>
std::span<const std::byte> Bytes(const flatbuffers::Vector<Scalar>* value) {
  return {reinterpret_cast<const std::byte*>(value->Data()),
          static_cast<std::size_t>(value->size()) * sizeof(Scalar)};
}

std::span<const std::byte> Bytes(const flatbuffers::String* value) {
  return {reinterpret_cast<const std::byte*>(value->Data()),
          static_cast<std::size_t>(value->size())};
}

void AddText(std::vector<ExtractedEntity>& entities,
             std::span<const std::byte> text) {
  auto extracted = ExtractEntities(text);
  entities.insert(entities.end(), std::make_move_iterator(extracted.begin()),
                  std::make_move_iterator(extracted.end()));
}

void AddAssertionText(std::vector<ExtractedEntity>& entities,
                      const schema::Assertion& assertion) {
  AddText(entities, Bytes(assertion.belief_id()));
  AddText(entities, Bytes(assertion.canonical_identity()));
  AddText(entities, Bytes(assertion.value()));
  if (assertion.conflict_domain() != nullptr) {
    AddText(entities, Bytes(assertion.conflict_domain()));
  }
}

bool IsSemanticMemoryEvent(log::EventKind kind) {
  switch (kind) {
    case log::EventKind::kUserMsg:
    case log::EventKind::kDeliveredMsg:
    case log::EventKind::kAssertion:
    case log::EventKind::kConsolidation:
      return true;
    default:
      return false;
  }
}

std::vector<ExtractedEntity> EventEntities(
    const events::VerifiedEvent& event) {
  std::vector<ExtractedEntity> entities;
  if (!IsSemanticMemoryEvent(event.kind)) {
    return entities;
  }
  const auto* envelope = event.envelope;
  switch (event.kind) {
    case log::EventKind::kUserMsg:
      AddText(entities, Bytes(envelope->payload_as_UserMsg()->content()));
      break;
    case log::EventKind::kDeliveredMsg:
      AddText(entities, Bytes(envelope->payload_as_DeliveredMsg()->content()));
      break;
    case log::EventKind::kToolCall: {
      const auto* payload = envelope->payload_as_ToolCall();
      AddText(entities, Bytes(payload->call_id()));
      AddText(entities, Bytes(payload->tool_name()));
      AddText(entities, Bytes(payload->arguments()));
      break;
    }
    case log::EventKind::kToolResult: {
      const auto* payload = envelope->payload_as_ToolResult();
      AddText(entities, Bytes(payload->call_id()));
      AddText(entities, Bytes(payload->result()));
      break;
    }
    case log::EventKind::kReasoning:
      AddText(entities, Bytes(envelope->payload_as_Reasoning()->content()));
      break;
    case log::EventKind::kProviderFrame: {
      const auto* payload = envelope->payload_as_ProviderFrame();
      AddText(entities, Bytes(payload->provider()));
      AddText(entities, Bytes(payload->api_content()));
      break;
    }
    case log::EventKind::kMediaRef: {
      const auto* payload = envelope->payload_as_MediaRef();
      AddText(entities, Bytes(payload->uri()));
      AddText(entities, Bytes(payload->media_type()));
      break;
    }
    case log::EventKind::kEffect:
      AddText(entities, Bytes(envelope->payload_as_Effect()->effect_id()));
      break;
    case log::EventKind::kApproval:
      AddText(entities, Bytes(envelope->payload_as_Approval()->effect_id()));
      break;
    case log::EventKind::kOutcome: {
      const auto* payload = envelope->payload_as_Outcome();
      AddText(entities, Bytes(payload->effect_id()));
      AddText(entities, Bytes(payload->detail()));
      break;
    }
    case log::EventKind::kCheckpoint:
      AddText(entities, Bytes(envelope->payload_as_Checkpoint()->cursor()));
      break;
    case log::EventKind::kSupervisor: {
      const auto* payload = envelope->payload_as_Supervisor();
      AddText(entities, Bytes(payload->code()));
      AddText(entities, Bytes(payload->evidence()));
      break;
    }
    case log::EventKind::kRecovery:
      AddText(entities, Bytes(envelope->payload_as_Recovery()->code()));
      break;
    case log::EventKind::kIntentSet:
      AddText(entities, Bytes(envelope->payload_as_IntentSet()->objective()));
      break;
    case log::EventKind::kLoopOpened: {
      const auto* payload = envelope->payload_as_LoopOpened();
      AddText(entities, Bytes(payload->loop_id()));
      AddText(entities, Bytes(payload->objective()));
      break;
    }
    case log::EventKind::kLoopClosed: {
      const auto* payload = envelope->payload_as_LoopClosed();
      AddText(entities, Bytes(payload->loop_id()));
      AddText(entities, Bytes(payload->cause()));
      break;
    }
    case log::EventKind::kAssertion:
      AddAssertionText(entities, *envelope->payload_as_Assertion());
      break;
    case log::EventKind::kConsolidation:
      for (const auto* assertion :
           *envelope->payload_as_Consolidation()->assertions()) {
        AddAssertionText(entities, *assertion);
      }
      break;
    case log::EventKind::kEmbedding:
      break;
    case log::EventKind::kRetract:
      AddText(entities, Bytes(envelope->payload_as_Retract()->belief_id()));
      break;
    case log::EventKind::kAttestation:
      break;
  }
  SortUnique(entities);
  return entities;
}

std::vector<ExtractedEntity> QueryEntities(std::string_view query,
                                           std::string_view turn_text) {
  auto entities = ExtractEntities(query);
  auto turn_entities = ExtractEntities(turn_text);
  entities.insert(entities.end(),
                  std::make_move_iterator(turn_entities.begin()),
                  std::make_move_iterator(turn_entities.end()));
  for (const auto text : {query, turn_text}) {
    std::optional<std::string> previous;
    std::size_t offset = 0;
    while (offset < text.size()) {
      bool breaks_sequence = false;
      while (offset < text.size() && !IsAsciiAlpha(text[offset])) {
        breaks_sequence = breaks_sequence ||
                          (text[offset] != ' ' && text[offset] != '\t' &&
                           text[offset] != '\n' && text[offset] != '\r');
        ++offset;
      }
      if (breaks_sequence) {
        previous.reset();
      }
      const std::size_t begin = offset;
      while (offset < text.size() &&
             (IsAsciiAlnum(text[offset]) || text[offset] == '-' ||
              text[offset] == '\'')) {
        ++offset;
      }
      if (offset - begin >= 3U) {
        const auto word = text.substr(begin, offset - begin);
        Add(entities, EntityKind::kProperName, word);
        if (previous.has_value()) {
          std::string compound = *previous;
          compound.push_back(' ');
          compound.append(word);
          Add(entities, EntityKind::kProperName, compound);
        }
        previous = std::string(word);
      } else if (begin != offset) {
        previous.reset();
      }
    }
  }
  SortUnique(entities);
  return entities;
}

struct OwnedMutation final {
  std::vector<std::byte> key;
  std::vector<std::byte> value;
};

}  // namespace

std::vector<ExtractedEntity> ExtractEntities(
    std::span<const std::byte> text) {
  const std::string_view view(reinterpret_cast<const char*>(text.data()),
                              text.size());
  return ExtractEntities(view);
}

std::vector<ExtractedEntity> ExtractEntities(std::string_view text) {
  std::vector<ExtractedEntity> entities;
  ExtractBase(text, entities);
  SortUnique(entities);
  return entities;
}

std::expected<void, Error> EntityProjection::ApplyEvent(
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
  const auto entities = EventEntities(*verified);
  std::vector<OwnedMutation> owned;
  owned.reserve(entities.size());
  for (const auto& entity : entities) {
    auto key = PostingKey(entity);
    auto existing = snapshot->Get(ProjectionId::kEntityIndex, key);
    if (!existing) {
      return std::unexpected(existing.error());
    }
    auto bitmap = DecodePosting(*existing);
    if (!bitmap) {
      auto error = bitmap.error();
      error.lsn = frame.header.lsn;
      return std::unexpected(error);
    }
    roaring64_bitmap_add(bitmap->get(), frame.header.lsn);
    owned.push_back(OwnedMutation{.key = std::move(key),
                                  .value = EncodePosting(bitmap->get())});
  }
  std::vector<Mutation> mutations;
  mutations.reserve(owned.size());
  for (const auto& mutation : owned) {
    mutations.push_back(Mutation{.kind = MutationKind::kPut,
                                 .key = mutation.key,
                                 .value = mutation.value});
  }
  return store.Apply(ProjectionId::kEntityIndex, frame.header.lsn, mutations);
}

std::expected<EntityRebuildProgress, Error> EntityProjection::Rebuild(
    ProjectionStore& store, std::span<const log::Frame> frames, bool reset,
    std::size_t maximum_frames) {
  if (reset) {
    auto reset_result = store.Reset(ProjectionId::kEntityIndex);
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
    auto read_checkpoint = snapshot->Checkpoint(ProjectionId::kEntityIndex);
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
  return EntityRebuildProgress{.applied_lsn = applied_lsn,
                               .applied_frames = apply_count,
                               .complete = applied_lsn == frames.size()};
}

std::expected<std::vector<EntityHit>, Error> EntityProjection::Query(
    const ReadSnapshot& snapshot, std::string_view query,
    std::string_view turn_text) {
  const auto entities = QueryEntities(query, turn_text);
  std::vector<EntityHit> hits;
  for (const auto& entity : entities) {
    const auto key = PostingKey(entity);
    auto encoded = snapshot.Get(ProjectionId::kEntityIndex, key);
    if (!encoded) {
      return std::unexpected(encoded.error());
    }
    auto bitmap = DecodePosting(*encoded);
    if (!bitmap) {
      return std::unexpected(bitmap.error());
    }
    const auto cardinality = roaring64_bitmap_get_cardinality(bitmap->get());
    if (cardinality > std::numeric_limits<std::size_t>::max()) {
      return std::unexpected(Error{ErrorCode::kEntityIndexCorrupt, 0});
    }
    std::vector<std::uint64_t> lsns(static_cast<std::size_t>(cardinality));
    if (!lsns.empty()) {
      roaring64_bitmap_to_uint64_array(bitmap->get(), lsns.data());
    }
    for (const auto lsn : lsns) {
      hits.push_back(EntityHit{.lsn = lsn,
                               .kind = entity.kind,
                               .canonical = entity.canonical});
    }
  }
  std::sort(hits.begin(), hits.end());
  hits.erase(std::unique(hits.begin(), hits.end()), hits.end());
  return hits;
}

}  // namespace neocortex::proj
