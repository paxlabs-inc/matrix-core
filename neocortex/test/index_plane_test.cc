#include <algorithm>
#include <array>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <filesystem>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include <flatbuffers/flatbuffers.h>
#include <unistd.h>

#include "event_fixture.h"
#include "proj/lexical/bm25.h"
#include "proj/vectors/vector_lane.h"

namespace {

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

std::vector<std::byte> Bytes(std::string_view value) {
  return {reinterpret_cast<const std::byte*>(value.data()),
          reinterpret_cast<const std::byte*>(value.data()) + value.size()};
}

neocortex::log::Frame Message(std::uint64_t lsn, std::string_view text) {
  const auto content = Bytes(text);
  return neocortex::log::Frame{
      .header = neocortex::log::FrameHeader{
          .lsn = lsn,
          .kind = neocortex::log::EventKind::kUserMsg,
          .wall_timestamp_ns = static_cast<std::int64_t>(lsn),
          .actor = 91,
          .conversation = {},
      },
      .sealed_payload = neocortex::test::BuildEvent(
          neocortex::log::EventKind::kUserMsg, lsn, content),
  };
}

struct EmbeddingPosition final {
  std::uint64_t lsn;
  std::uint64_t target_lsn;
};

neocortex::log::Frame Embedding(EmbeddingPosition position,
                                std::span<const std::int8_t> quantized,
                                std::span<const std::uint8_t> binary) {
  flatbuffers::FlatBufferBuilder builder(512);
  const auto vector = builder.CreateVector(quantized.data(), quantized.size());
  const auto prefix = builder.CreateVector(binary.data(), binary.size());
  const auto embedding = neocortex::schema::CreateEmbedding(
      builder, position.target_lsn,
      static_cast<std::uint32_t>(quantized.size()), vector,
      prefix);
  const auto envelope = neocortex::schema::CreateEventEnvelope(
      builder, 1, neocortex::schema::EventPayload::Embedding,
      embedding.Union());
  neocortex::schema::FinishEventEnvelopeBuffer(builder, envelope);
  return neocortex::log::Frame{
      .header = neocortex::log::FrameHeader{
          .lsn = position.lsn,
          .kind = neocortex::log::EventKind::kEmbedding,
          .wall_timestamp_ns = static_cast<std::int64_t>(position.lsn),
          .actor = 91,
          .conversation = {},
      },
      .sealed_payload = {
          reinterpret_cast<const std::byte*>(builder.GetBufferPointer()),
          reinterpret_cast<const std::byte*>(builder.GetBufferPointer()) +
              builder.GetSize()},
  };
}

int TestSimdExactness() {
  for (std::size_t dimension = 1; dimension <= 513; ++dimension) {
    std::vector<std::int8_t> first(dimension);
    std::vector<std::int8_t> second(dimension);
    for (std::size_t index = 0; index < dimension; ++index) {
      first[index] = static_cast<std::int8_t>(
          static_cast<std::int32_t>((dimension * 13U + index * 17U) % 255U) -
          127);
      second[index] = static_cast<std::int8_t>(
          static_cast<std::int32_t>((dimension * 29U + index * 7U) % 255U) -
          127);
    }
    if (neocortex::proj::VectorLane::ScalarDot(first, second) !=
        neocortex::proj::VectorLane::SimdDot(first, second)) {
      return Fail("Highway int8 dot diverged from the scalar reference");
    }
  }
  return 0;
}

int TestIndexPlane(const std::filesystem::path& path) {
  auto store = neocortex::proj::ProjectionStore::Open(path);
  auto lane = neocortex::proj::VectorLane::Open(path);
  if (!store || !lane) {
    return Fail("index-plane stores did not open");
  }
  constexpr std::array<std::int8_t, 8> first = {8, 7, 6, 5, 4, 3, 2, 1};
  constexpr std::array<std::int8_t, 8> second = {-8, 7, -6, 5,
                                                 -4, 3, -2, 1};
  constexpr std::array<std::uint8_t, 1> first_binary = {0xff};
  constexpr std::array<std::uint8_t, 1> second_binary = {0x55};
  std::vector<neocortex::log::Frame> frames;
  frames.push_back(Message(1, "alpha alpha durable vector record"));
  frames.push_back(Message(2, "alpha beta second record"));
  frames.push_back(Embedding(EmbeddingPosition{.lsn = 3, .target_lsn = 1},
                             first, first_binary));
  frames.push_back(Embedding(EmbeddingPosition{.lsn = 4, .target_lsn = 2},
                             second, second_binary));
  frames.push_back(Message(5, "rareterm lexical only no embedding"));

  auto vectors = lane->Rebuild(*store, frames, false);
  auto lexical = neocortex::proj::LexicalProjection::Rebuild(*store, frames);
  if (!vectors || !vectors->complete || !lexical || !lexical->complete) {
    if (!vectors) {
      std::fprintf(stderr, "vector error %u lsn %llu\n",
                   static_cast<unsigned>(vectors.error().code),
                   static_cast<unsigned long long>(vectors.error().lsn));
    }
    if (!lexical) {
      std::fprintf(stderr, "lexical error %u lsn %llu\n",
                   static_cast<unsigned>(lexical.error().code),
                   static_cast<unsigned long long>(lexical.error().lsn));
    }
    return Fail("vector or lexical replay failed");
  }
  const std::array<std::byte, 1> binary_query = {std::byte{0xff}};
  auto vector_hits = lane->Search(first, binary_query, 8);
  if (!vector_hits || vector_hits->size() != 2 ||
      vector_hits->front().target_lsn != 1 ||
      vector_hits->front().score !=
          neocortex::proj::VectorLane::ScalarDot(first, first)) {
    return Fail("exact vector scan returned an inexact ranking");
  }
  auto snapshot = store->BeginSnapshot();
  if (!snapshot) {
    return Fail("index-plane snapshot failed");
  }
  auto alpha = neocortex::proj::LexicalProjection::Query(*snapshot, "alpha", 8);
  auto rare = neocortex::proj::LexicalProjection::Query(*snapshot, "rareterm", 8);
  if (!alpha || alpha->size() != 2 || alpha->front().lsn != 1 || !rare ||
      rare->size() != 1 || rare->front().lsn != 5) {
    return Fail("BM25 failed ranking or embedding-independent reachability");
  }
  const auto fused = neocortex::proj::LexicalProjection::Fuse(
      *vector_hits, *alpha, neocortex::proj::FusionOptions{.limit = 8});
  if (fused.empty() || fused.front().lsn != 1 ||
      fused.front().vector_rank != 1 || fused.front().lexical_rank != 1) {
    return Fail("deterministic reciprocal-rank fusion failed");
  }

  auto vector_bytes = lane->CanonicalBytes();
  auto lexical_bytes = snapshot->CanonicalDump(
      neocortex::proj::ProjectionId::kBm25);
  if (!vector_bytes || !lexical_bytes) {
    return Fail("canonical index state was unavailable");
  }
  snapshot = std::unexpected(neocortex::Error{
      neocortex::ErrorCode::kBackendUnavailable, 0});
  auto rebuilt_vectors = lane->Rebuild(*store, frames, true);
  auto rebuilt_lexical =
      neocortex::proj::LexicalProjection::Rebuild(*store, frames, true);
  auto rebuilt_vector_bytes = lane->CanonicalBytes();
  auto rebuilt_snapshot = store->BeginSnapshot();
  auto rebuilt_lexical_bytes =
      rebuilt_snapshot
          ? rebuilt_snapshot->CanonicalDump(neocortex::proj::ProjectionId::kBm25)
          : std::expected<std::vector<std::byte>, neocortex::Error>(
                std::unexpected(rebuilt_snapshot.error()));
  if (!rebuilt_vectors || !rebuilt_lexical || !rebuilt_vector_bytes ||
      !rebuilt_lexical_bytes || *vector_bytes != *rebuilt_vector_bytes ||
      *lexical_bytes != *rebuilt_lexical_bytes) {
    return Fail("vector or lexical replay was not byte-identical");
  }
  rebuilt_snapshot = std::unexpected(neocortex::Error{
      neocortex::ErrorCode::kBackendUnavailable, 0});

  constexpr std::size_t kCorpusRecords = 1024;
  constexpr std::size_t kDimension = 128;
  frames.reserve(frames.size() + kCorpusRecords * 2U);
  std::vector<std::int8_t> corpus_vector(kDimension, -1);
  std::array<std::uint8_t, kDimension / 8U> corpus_binary{};
  for (std::size_t index = 0; index < kCorpusRecords; ++index) {
    const auto message_lsn = static_cast<std::uint64_t>(frames.size()) + 1U;
    frames.push_back(Message(message_lsn, "realistic corpus vector record"));
    std::fill(corpus_vector.begin(), corpus_vector.end(), -1);
    corpus_vector[index % kDimension] = 127;
    std::fill(corpus_binary.begin(), corpus_binary.end(), 0);
    corpus_binary[(index % kDimension) / 8U] = static_cast<std::uint8_t>(
        1U << ((index % kDimension) % 8U));
    const auto embedding_lsn = static_cast<std::uint64_t>(frames.size()) + 1U;
    frames.push_back(Embedding(
        EmbeddingPosition{.lsn = embedding_lsn, .target_lsn = message_lsn},
        corpus_vector, corpus_binary));
  }
  auto corpus_rebuild = lane->Rebuild(*store, frames, false);
  std::fill(corpus_vector.begin(), corpus_vector.end(), -1);
  corpus_vector[0] = 127;
  std::fill(corpus_binary.begin(), corpus_binary.end(), 0);
  corpus_binary[0] = 1;
  const auto started = std::chrono::steady_clock::now();
  auto corpus_hits = lane->Search(
      corpus_vector,
      std::span<const std::byte>(
          reinterpret_cast<const std::byte*>(corpus_binary.data()),
          corpus_binary.size()),
      kCorpusRecords);
  const auto elapsed = std::chrono::steady_clock::now() - started;
  const auto elapsed_us =
      std::chrono::duration_cast<std::chrono::microseconds>(elapsed).count();
  if (!corpus_rebuild || !corpus_rebuild->complete || !corpus_hits ||
      corpus_hits->size() != kCorpusRecords || elapsed_us > 500'000) {
    std::fprintf(stderr,
                 "corpus rebuild=%d complete=%d hits=%zu elapsed_us=%lld\n",
                 corpus_rebuild.has_value() ? 1 : 0,
                 corpus_rebuild && corpus_rebuild->complete ? 1 : 0,
                 corpus_hits ? corpus_hits->size() : 0U,
                 static_cast<long long>(elapsed_us));
    if (!corpus_rebuild) {
      std::fprintf(stderr, "corpus error=%u lsn=%llu offset=%llu\n",
                   static_cast<unsigned>(corpus_rebuild.error().code),
                   static_cast<unsigned long long>(corpus_rebuild.error().lsn),
                   static_cast<unsigned long long>(
                       corpus_rebuild.error().offset));
    }
    return Fail("realistic exact-scan latency envelope failed");
  }
  std::fprintf(stdout, "exact_scan records=%zu dimension=%zu elapsed_us=%lld\n",
               kCorpusRecords, kDimension,
               static_cast<long long>(elapsed_us));
  return 0;
}

}  // namespace

int main() {
  if (TestSimdExactness() != 0) {
    return 1;
  }
  const auto path = std::filesystem::temp_directory_path() /
                    ("neocortex-index-plane-" + std::to_string(::getpid()));
  std::error_code error;
  std::filesystem::remove_all(path, error);
  const int result = TestIndexPlane(path);
  std::filesystem::remove_all(path, error);
  return result;
}
