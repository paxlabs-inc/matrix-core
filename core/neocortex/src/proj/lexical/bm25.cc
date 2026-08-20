#include "proj/lexical/bm25.h"

#include <algorithm>
#include <array>
#include <map>
#include <memory>
#include <set>
#include <string>
#include <utility>

#include <roaring/roaring64.h>

#include "schema/events.h"

namespace neocortex::proj {
namespace {

bool IsAlpha(char value) {
  return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z');
}

bool IsDigit(char value) { return value >= '0' && value <= '9'; }

char Lower(char value) {
  return value >= 'A' && value <= 'Z'
             ? static_cast<char>(value - 'A' + 'a')
             : value;
}

// Terms become LMDB posting keys; the bound keeps every derived key below the
// backend key-size limit for arbitrary committed text, on both the apply and
// query paths.
constexpr std::size_t kMaximumTermBytes = 64;

std::vector<std::string> Tokenize(std::string_view text) {
  std::vector<std::string> terms;
  std::size_t offset = 0;
  while (offset < text.size()) {
    while (offset < text.size() && !IsAlpha(text[offset]) &&
           !IsDigit(text[offset])) {
      ++offset;
    }
    const std::size_t begin = offset;
    while (offset < text.size() &&
           (IsAlpha(text[offset]) || IsDigit(text[offset]) ||
            text[offset] == '_')) {
      ++offset;
    }
    if (offset - begin >= 2U && offset - begin <= kMaximumTermBytes) {
      std::string term;
      term.reserve(offset - begin);
      for (const char character : text.substr(begin, offset - begin)) {
        term.push_back(Lower(character));
      }
      terms.push_back(std::move(term));
    }
  }
  return terms;
}

template <typename Scalar>
std::string_view Text(const flatbuffers::Vector<Scalar>* value) {
  return {reinterpret_cast<const char*>(value->Data()),
          static_cast<std::size_t>(value->size()) * sizeof(Scalar)};
}

std::string_view Text(const flatbuffers::String* value) {
  return {reinterpret_cast<const char*>(value->Data()),
          static_cast<std::size_t>(value->size())};
}

void AppendText(std::string& output, std::string_view value) {
  if (!output.empty()) {
    output.push_back(' ');
  }
  output.append(value);
}

void AppendAssertion(std::string& output, const schema::Assertion& assertion) {
  AppendText(output, Text(assertion.canonical_identity()));
  AppendText(output, Text(assertion.value()));
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

std::string DocumentText(const events::VerifiedEvent& event) {
  std::string output;
  if (!IsSemanticMemoryEvent(event.kind)) {
    return output;
  }
  switch (event.kind) {
    case log::EventKind::kUserMsg:
      AppendText(output, Text(event.envelope->payload_as_UserMsg()->content()));
      break;
    case log::EventKind::kDeliveredMsg:
      AppendText(output,
                 Text(event.envelope->payload_as_DeliveredMsg()->content()));
      break;
    case log::EventKind::kAssertion:
      AppendAssertion(output, *event.envelope->payload_as_Assertion());
      break;
    case log::EventKind::kConsolidation:
      for (const auto* assertion :
           *event.envelope->payload_as_Consolidation()->assertions()) {
        AppendAssertion(output, *assertion);
      }
      break;
    case log::EventKind::kToolCall:
      AppendText(output, Text(event.envelope->payload_as_ToolCall()->arguments()));
      break;
    case log::EventKind::kToolResult:
      AppendText(output, Text(event.envelope->payload_as_ToolResult()->result()));
      break;
    case log::EventKind::kReasoning:
      AppendText(output, Text(event.envelope->payload_as_Reasoning()->content()));
      break;
    case log::EventKind::kProviderFrame:
      AppendText(output,
                 Text(event.envelope->payload_as_ProviderFrame()->api_content()));
      break;
    case log::EventKind::kOutcome:
      AppendText(output, Text(event.envelope->payload_as_Outcome()->detail()));
      break;
    case log::EventKind::kSupervisor:
      AppendText(output, Text(event.envelope->payload_as_Supervisor()->evidence()));
      break;
    case log::EventKind::kIntentSet:
      AppendText(output, Text(event.envelope->payload_as_IntentSet()->objective()));
      break;
    case log::EventKind::kLoopOpened:
      AppendText(output, Text(event.envelope->payload_as_LoopOpened()->objective()));
      break;
    case log::EventKind::kLoopClosed:
      AppendText(output, Text(event.envelope->payload_as_LoopClosed()->cause()));
      break;
    case log::EventKind::kMediaRef:
    case log::EventKind::kEffect:
    case log::EventKind::kApproval:
    case log::EventKind::kCheckpoint:
    case log::EventKind::kRecovery:
    case log::EventKind::kEmbedding:
    case log::EventKind::kRetract:
    case log::EventKind::kAttestation:
      break;
  }
  return output;
}

void AppendU32(std::vector<std::byte>& output, std::uint32_t value) {
  for (std::uint32_t shift = 0; shift < 32; shift += 8) {
    output.push_back(static_cast<std::byte>((value >> shift) & 0xffU));
  }
}

void AppendU64(std::vector<std::byte>& output, std::uint64_t value) {
  for (std::uint32_t shift = 0; shift < 64; shift += 8) {
    output.push_back(static_cast<std::byte>((value >> shift) & 0xffU));
  }
}

std::uint32_t DecodeU32(std::span<const std::byte> value) {
  std::uint32_t decoded = 0;
  for (std::size_t index = 0; index < 4; ++index) {
    decoded |= std::to_integer<std::uint32_t>(value[index]) << (index * 8U);
  }
  return decoded;
}

std::uint64_t DecodeU64(std::span<const std::byte> value,
                        std::size_t offset = 0) {
  std::uint64_t decoded = 0;
  for (std::size_t index = 0; index < 8; ++index) {
    decoded |= std::to_integer<std::uint64_t>(value[offset + index])
               << (index * 8U);
  }
  return decoded;
}

std::vector<std::byte> PostingKey(std::string_view term) {
  std::vector<std::byte> key = {std::byte{'P'}};
  key.insert(key.end(), reinterpret_cast<const std::byte*>(term.data()),
             reinterpret_cast<const std::byte*>(term.data()) + term.size());
  return key;
}

std::vector<std::byte> FrequencyKey(std::string_view term, std::uint64_t lsn) {
  auto key = PostingKey(term);
  key.front() = std::byte{'F'};
  key.push_back(std::byte{0});
  for (std::int32_t shift = 56; shift >= 0; shift -= 8) {
    key.push_back(static_cast<std::byte>((lsn >> shift) & 0xffU));
  }
  return key;
}

std::array<std::byte, 9> DocumentKey(std::uint64_t lsn) {
  std::array<std::byte, 9> key{};
  key[0] = std::byte{'D'};
  for (std::size_t index = 0; index < 8; ++index) {
    key[index + 1U] =
        static_cast<std::byte>((lsn >> ((7U - index) * 8U)) & 0xffU);
  }
  return key;
}

constexpr std::array<std::byte, 1> kStatsKey = {std::byte{'S'}};

struct BitmapDeleter final {
  void operator()(roaring64_bitmap_t* bitmap) const {
    if (bitmap != nullptr) {
      roaring64_bitmap_free(bitmap);
    }
  }
};
using Bitmap = std::unique_ptr<roaring64_bitmap_t, BitmapDeleter>;

std::expected<Bitmap, Error> DecodeBitmap(
    const std::optional<std::vector<std::byte>>& value) {
  if (!value.has_value()) {
    Bitmap bitmap(roaring64_bitmap_create());
    if (!bitmap) {
      return std::unexpected(Error{ErrorCode::kBackendUnavailable, 0});
    }
    return bitmap;
  }
  const auto& bytes = *value;
  if (bytes.empty() || roaring64_bitmap_portable_deserialize_size(
                           reinterpret_cast<const char*>(bytes.data()),
                           bytes.size()) != bytes.size()) {
      return std::unexpected(Error{ErrorCode::kLexicalIndexCorrupt, 0});
  }
  Bitmap bitmap(roaring64_bitmap_portable_deserialize_safe(
      reinterpret_cast<const char*>(bytes.data()), bytes.size()));
  if (!bitmap) {
    return std::unexpected(Error{ErrorCode::kLexicalIndexCorrupt, 0});
  }
  return bitmap;
}

std::vector<std::byte> EncodeBitmap(const roaring64_bitmap_t* bitmap) {
  std::vector<std::byte> bytes(
      roaring64_bitmap_portable_size_in_bytes(bitmap));
  bytes.resize(roaring64_bitmap_portable_serialize(
      bitmap, reinterpret_cast<char*>(bytes.data())));
  return bytes;
}

struct Ratio final {
  std::uint64_t numerator;
  std::uint64_t denominator;
};

std::uint64_t FixedLnRatio(Ratio ratio) {
  using Wide = unsigned __int128;
  const Wide scaled_numerator = ratio.numerator;
  Wide scaled_denominator = ratio.denominator;
  std::uint32_t exponent = 0;
  while (scaled_numerator >= scaled_denominator * 2U) {
    scaled_denominator *= 2U;
    ++exponent;
  }
  constexpr std::uint64_t kOne = std::uint64_t{1} << 32U;
  constexpr std::uint64_t kLn2 = 2'977'044'471ULL;
  const auto y = static_cast<std::uint64_t>(
      ((scaled_numerator - scaled_denominator) << 32U) /
      (scaled_numerator + scaled_denominator));
  const auto y_squared = static_cast<std::uint64_t>(
      (static_cast<Wide>(y) * y) >> 32U);
  std::uint64_t power = y;
  std::uint64_t series = y;
  for (std::uint64_t divisor = 3; divisor <= 15; divisor += 2) {
    power = static_cast<std::uint64_t>(
        (static_cast<Wide>(power) * y_squared) >> 32U);
    series += power / divisor;
  }
  const auto fractional = series * 2U;
  return static_cast<std::uint64_t>(exponent) * kLn2 +
         std::min(fractional, kOne * 2U);
}

struct OwnedMutation final {
  std::vector<std::byte> key;
  std::vector<std::byte> value;
};

void Put(std::vector<OwnedMutation>& owned, std::vector<std::byte> key,
         std::vector<std::byte> value) {
  owned.push_back(
      OwnedMutation{.key = std::move(key), .value = std::move(value)});
}

}  // namespace

std::expected<void, Error> LexicalProjection::ApplyEvent(
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
  const auto terms = Tokenize(DocumentText(*verified));
  std::vector<OwnedMutation> owned;
  if (!terms.empty()) {
    std::map<std::string, std::uint32_t> frequencies;
    for (const auto& term : terms) {
      ++frequencies[term];
    }
    auto stats = snapshot->Get(ProjectionId::kBm25, kStatsKey);
    if (!stats) {
      return std::unexpected(stats.error());
    }
    std::uint64_t document_count = 0;
    std::uint64_t total_terms = 0;
    auto stats_bytes = std::move(*stats).value_or(std::vector<std::byte>{});
    if (!stats_bytes.empty()) {
      if (stats_bytes.size() != 16) {
        return std::unexpected(Error{ErrorCode::kLexicalIndexCorrupt, 0});
      }
      document_count = DecodeU64(stats_bytes);
      total_terms = DecodeU64(stats_bytes, 8);
    }
    for (const auto& [term, frequency] : frequencies) {
      const auto posting_key = PostingKey(term);
      auto posting = snapshot->Get(ProjectionId::kBm25, posting_key);
      if (!posting) {
        return std::unexpected(posting.error());
      }
      auto bitmap = DecodeBitmap(*posting);
      if (!bitmap) {
        return std::unexpected(bitmap.error());
      }
      roaring64_bitmap_add(bitmap->get(), frame.header.lsn);
      Put(owned, posting_key, EncodeBitmap(bitmap->get()));
      std::vector<std::byte> encoded_frequency;
      AppendU32(encoded_frequency, frequency);
      Put(owned, FrequencyKey(term, frame.header.lsn),
          std::move(encoded_frequency));
    }
    std::vector<std::byte> document_length;
    AppendU32(document_length, static_cast<std::uint32_t>(terms.size()));
    const auto document_key = DocumentKey(frame.header.lsn);
    Put(owned, std::vector<std::byte>(document_key.begin(), document_key.end()),
        std::move(document_length));
    std::vector<std::byte> encoded_stats;
    AppendU64(encoded_stats, document_count + 1U);
    AppendU64(encoded_stats, total_terms + terms.size());
    Put(owned, std::vector<std::byte>(kStatsKey.begin(), kStatsKey.end()),
        std::move(encoded_stats));
  }
  std::vector<Mutation> mutations;
  mutations.reserve(owned.size());
  for (const auto& mutation : owned) {
    mutations.push_back(Mutation{.kind = MutationKind::kPut,
                                 .key = mutation.key,
                                 .value = mutation.value});
  }
  return store.Apply(ProjectionId::kBm25, frame.header.lsn, mutations);
}

std::expected<LexicalRebuildProgress, Error> LexicalProjection::Rebuild(
    ProjectionStore& store, std::span<const log::Frame> frames, bool reset,
    std::size_t maximum_frames) {
  if (reset) {
    auto reset_result = store.Reset(ProjectionId::kBm25);
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
    auto read_checkpoint = snapshot->Checkpoint(ProjectionId::kBm25);
    if (!read_checkpoint) {
      return std::unexpected(read_checkpoint.error());
    }
    checkpoint = *read_checkpoint;
  }
  if (checkpoint > frames.size()) {
    return std::unexpected(Error{ErrorCode::kProjectionCheckpoint, 0,
                                 checkpoint, frames.size()});
  }
  const auto apply_count = std::min(
      frames.size() - static_cast<std::size_t>(checkpoint), maximum_frames);
  const auto begin = static_cast<std::size_t>(checkpoint);
  for (std::size_t index = begin; index < begin + apply_count; ++index) {
    auto applied = ApplyEvent(store, frames[index]);
    if (!applied) {
      return std::unexpected(applied.error());
    }
  }
  const auto applied_lsn = checkpoint + static_cast<std::uint64_t>(apply_count);
  return LexicalRebuildProgress{.applied_lsn = applied_lsn,
                                .applied_frames = apply_count,
                                .complete = applied_lsn == frames.size()};
}

std::expected<std::vector<LexicalHit>, Error> LexicalProjection::Query(
    const ReadSnapshot& snapshot, std::string_view query, std::size_t limit) {
  auto query_terms = Tokenize(query);
  std::sort(query_terms.begin(), query_terms.end());
  query_terms.erase(std::unique(query_terms.begin(), query_terms.end()),
                    query_terms.end());
  if (query_terms.empty() || limit == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto stats = snapshot.Get(ProjectionId::kBm25, kStatsKey);
  if (!stats) {
    return std::unexpected(stats.error());
  }
  auto stats_bytes = std::move(*stats).value_or(std::vector<std::byte>{});
  if (stats_bytes.empty()) {
    return std::vector<LexicalHit>{};
  }
  if (stats_bytes.size() != 16) {
    return std::unexpected(Error{ErrorCode::kLexicalIndexCorrupt, 0});
  }
  const auto document_count = DecodeU64(stats_bytes);
  const auto total_terms = DecodeU64(stats_bytes, 8);
  std::map<std::uint64_t, std::uint64_t> scores;
  for (const auto& term : query_terms) {
    auto encoded = snapshot.Get(ProjectionId::kBm25, PostingKey(term));
    if (!encoded) {
      return std::unexpected(encoded.error());
    }
    auto bitmap = DecodeBitmap(*encoded);
    if (!bitmap) {
      return std::unexpected(bitmap.error());
    }
    const auto frequency = roaring64_bitmap_get_cardinality(bitmap->get());
    if (frequency == 0) {
      continue;
    }
    const auto idf = FixedLnRatio(
        Ratio{.numerator = document_count * 2U + 2U,
              .denominator = frequency * 2U + 1U});
    std::vector<std::uint64_t> documents(static_cast<std::size_t>(frequency));
    roaring64_bitmap_to_uint64_array(bitmap->get(), documents.data());
    for (const auto lsn : documents) {
      auto tf = snapshot.Get(ProjectionId::kBm25, FrequencyKey(term, lsn));
      const auto document_key = DocumentKey(lsn);
      auto length = snapshot.Get(ProjectionId::kBm25, document_key);
      if (!tf || !length) {
        return std::unexpected(!tf ? tf.error() : length.error());
      }
      auto tf_bytes = std::move(*tf).value_or(std::vector<std::byte>{});
      auto length_bytes =
          std::move(*length).value_or(std::vector<std::byte>{});
      if (tf_bytes.size() != 4 || length_bytes.size() != 4) {
        return std::unexpected(Error{ErrorCode::kLexicalIndexCorrupt, 0});
      }
      const auto term_frequency = DecodeU32(tf_bytes);
      const auto document_length = DecodeU32(length_bytes);
      const auto numerator = static_cast<unsigned __int128>(idf) * 44U *
                             term_frequency * total_terms;
      const auto denominator =
          std::uint64_t{20} * term_frequency * total_terms +
          std::uint64_t{6} * total_terms +
          std::uint64_t{18} * document_length * document_count;
      scores[lsn] += static_cast<std::uint64_t>(numerator / denominator);
    }
  }
  std::vector<LexicalHit> hits;
  hits.reserve(scores.size());
  for (const auto& [lsn, score] : scores) {
    hits.push_back(LexicalHit{.lsn = lsn, .score_q32 = score});
  }
  std::sort(hits.begin(), hits.end(), [](const LexicalHit& first,
                                         const LexicalHit& second) {
    return first.score_q32 != second.score_q32
               ? first.score_q32 > second.score_q32
               : first.lsn < second.lsn;
  });
  if (hits.size() > limit) {
    hits.resize(limit);
  }
  return hits;
}

std::vector<FusedHit> LexicalProjection::Fuse(
    std::span<const VectorHit> vector_hits,
    std::span<const LexicalHit> lexical_hits, FusionOptions options) {
  std::map<std::uint64_t, FusedHit> fused;
  constexpr std::uint64_t kScale = 1'000'000'000ULL;
  for (std::size_t index = 0; index < vector_hits.size(); ++index) {
    auto& hit = fused[vector_hits[index].target_lsn];
    hit.lsn = vector_hits[index].target_lsn;
    hit.vector_rank = static_cast<std::uint32_t>(index + 1U);
    hit.rrf_score += kScale / (options.rank_constant + hit.vector_rank);
  }
  for (std::size_t index = 0; index < lexical_hits.size(); ++index) {
    auto& hit = fused[lexical_hits[index].lsn];
    hit.lsn = lexical_hits[index].lsn;
    hit.lexical_rank = static_cast<std::uint32_t>(index + 1U);
    hit.rrf_score += kScale / (options.rank_constant + hit.lexical_rank);
  }
  std::vector<FusedHit> output;
  output.reserve(fused.size());
  for (const auto& [lsn, hit] : fused) {
    static_cast<void>(lsn);
    output.push_back(hit);
  }
  std::sort(output.begin(), output.end(), [](const FusedHit& first,
                                             const FusedHit& second) {
    return first.rrf_score != second.rrf_score
               ? first.rrf_score > second.rrf_score
               : first.lsn < second.lsn;
  });
  if (output.size() > options.limit) {
    output.resize(options.limit);
  }
  return output;
}

}  // namespace neocortex::proj
