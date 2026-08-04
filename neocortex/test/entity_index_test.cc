#include <algorithm>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <filesystem>
#include <span>
#include <string>
#include <string_view>
#include <vector>

#include <unistd.h>

#include "event_fixture.h"
#include "proj/entity/entity_index.h"

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

neocortex::log::Frame Message(std::uint64_t lsn, std::string_view content) {
  const auto bytes = Bytes(content);
  return neocortex::log::Frame{
      .header = neocortex::log::FrameHeader{
          .lsn = lsn,
          .kind = neocortex::log::EventKind::kUserMsg,
          .wall_timestamp_ns = static_cast<std::int64_t>(lsn) * 1000,
          .actor = 73,
          .conversation = {},
      },
      .sealed_payload = neocortex::test::BuildEvent(
          neocortex::log::EventKind::kUserMsg, lsn, bytes),
  };
}

neocortex::log::Frame Assertion(std::uint64_t lsn,
                                std::string_view content) {
  const auto bytes = Bytes(content);
  return neocortex::log::Frame{
      .header = neocortex::log::FrameHeader{
          .lsn = lsn,
          .kind = neocortex::log::EventKind::kAssertion,
          .wall_timestamp_ns = static_cast<std::int64_t>(lsn) * 1000,
          .actor = 73,
          .conversation = {},
      },
      .sealed_payload = neocortex::test::BuildEvent(
          neocortex::log::EventKind::kAssertion, lsn, bytes),
  };
}

bool HasHit(std::span<const neocortex::proj::EntityHit> hits,
            std::uint64_t lsn, neocortex::proj::EntityKind kind,
            std::string_view canonical) {
  return std::ranges::any_of(hits, [&](const auto& hit) {
    return hit.lsn == lsn && hit.kind == kind && hit.canonical == canonical;
  });
}

std::expected<std::vector<neocortex::proj::EntityHit>, neocortex::Error> Query(
    neocortex::proj::ProjectionStore& store, std::string_view query,
    std::string_view turn = {}) {
  auto snapshot = store.BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  return neocortex::proj::EntityProjection::Query(*snapshot, query, turn);
}

std::expected<std::vector<std::byte>, neocortex::Error> Dump(
    neocortex::proj::ProjectionStore& store) {
  auto snapshot = store.BeginSnapshot();
  if (!snapshot) {
    return std::unexpected(snapshot.error());
  }
  return snapshot->CanonicalDump(neocortex::proj::ProjectionId::kEntityIndex);
}

int TestTypedExtraction() {
  constexpr std::string_view text =
      "Alice Johnson opened https://Moltbook.com/post/ABC, then wrote "
      "/root/matrix/spec/neocortex for usr_12345, "
      "0xDEADBEEF, and 550e8400-e29b-41d4-a716-446655440000.";
  const auto first = neocortex::proj::ExtractEntities(text);
  const auto second = neocortex::proj::ExtractEntities(text);
  if (first != second || !std::ranges::is_sorted(first)) {
    return Fail("identifier extraction was not deterministic and canonical");
  }
  const auto contains = [&](neocortex::proj::EntityKind kind,
                            std::string_view value) {
    return std::ranges::any_of(first, [&](const auto& entity) {
      return entity.kind == kind && entity.canonical == value;
    });
  };
  if (!contains(neocortex::proj::EntityKind::kUrl,
                "https://moltbook.com/post/abc") ||
      !contains(neocortex::proj::EntityKind::kDomain, "moltbook.com") ||
      !contains(neocortex::proj::EntityKind::kPath,
                "/root/matrix/spec/neocortex") ||
      !contains(neocortex::proj::EntityKind::kStructuredId, "usr_12345") ||
      !contains(neocortex::proj::EntityKind::kHexId, "0xdeadbeef") ||
      !contains(neocortex::proj::EntityKind::kStructuredId,
                "550e8400-e29b-41d4-a716-446655440000") ||
      !contains(neocortex::proj::EntityKind::kProperName, "alice johnson") ||
      !contains(neocortex::proj::EntityKind::kProperName, "moltbook")) {
    return Fail("typed identifier extraction missed a required class");
  }
  return 0;
}

int TestGuaranteedRecallAndReplay(const std::filesystem::path& path) {
  auto store = neocortex::proj::ProjectionStore::Open(path);
  if (!store) {
    return Fail("entity projection open failed");
  }
  std::vector<neocortex::log::Frame> frames;
  frames.reserve(99);
  frames.push_back(Message(
      1, "Alice Johnson confirmed https://Moltbook.com/post/ABC and "
         "/root/matrix/spec/neocortex for usr_12345."));
  frames.push_back(Message(
      2, "Grace Hopper inspected 0xDEADBEEF and "
         "550e8400-e29b-41d4-a716-446655440000."));
  frames.push_back(Assertion(
      3, "The durable belief references belief.example.net exactly."));
  for (std::uint64_t index = 0; index < 96; ++index) {
    const auto lsn = index + 4U;
    const auto content =
        std::string("Record https://entity") + std::to_string(index) +
        ".example.com/items/item_" + std::to_string(index) +
        "x is stored at /srv/actors/actor_" + std::to_string(index) + ".";
    frames.push_back(Message(lsn, content));
  }

  auto prefix = neocortex::proj::EntityProjection::Rebuild(
      *store, frames, false, 37);
  auto resumed = neocortex::proj::EntityProjection::Rebuild(
      *store, frames, false);
  if (!prefix || prefix->complete || prefix->applied_lsn != 37 || !resumed ||
      !resumed->complete || resumed->applied_lsn != frames.size()) {
    return Fail("entity projection bounded rebuild and resume failed");
  }

  auto domain = Query(*store, "moltbook.com");
  auto url = Query(*store, "https://Moltbook.com/post/ABC");
  auto path_hits = Query(*store, "/root/matrix/spec/neocortex");
  auto structured = Query(*store, "usr_12345");
  auto hex = Query(*store, "0xDEADBEEF");
  auto uuid = Query(*store, "550e8400-e29b-41d4-a716-446655440000");
  auto belief = Query(*store, "belief.example.net");
  auto proper = Query(*store, "alice johnson");
  auto entity_alias = Query(*store, "moltbook");
  auto turn_match = Query(*store, "unrelated query", "Grace Hopper");
  if (!domain || !url || !path_hits || !structured || !hex || !uuid ||
      !belief ||
      !proper || !entity_alias || !turn_match ||
      !HasHit(*domain, 1, neocortex::proj::EntityKind::kDomain,
              "moltbook.com") ||
      !HasHit(*url, 1, neocortex::proj::EntityKind::kUrl,
              "https://moltbook.com/post/abc") ||
      !HasHit(*path_hits, 1, neocortex::proj::EntityKind::kPath,
              "/root/matrix/spec/neocortex") ||
      !HasHit(*structured, 1, neocortex::proj::EntityKind::kStructuredId,
              "usr_12345") ||
      !HasHit(*hex, 2, neocortex::proj::EntityKind::kHexId, "0xdeadbeef") ||
      !HasHit(*uuid, 2, neocortex::proj::EntityKind::kStructuredId,
              "550e8400-e29b-41d4-a716-446655440000") ||
      !HasHit(*belief, 3, neocortex::proj::EntityKind::kDomain,
              "belief.example.net") ||
      !HasHit(*proper, 1, neocortex::proj::EntityKind::kProperName,
              "alice johnson") ||
      !HasHit(*entity_alias, 1, neocortex::proj::EntityKind::kProperName,
              "moltbook") ||
      !HasHit(*turn_match, 2, neocortex::proj::EntityKind::kProperName,
              "grace hopper")) {
    return Fail("stored identifier or entity did not receive guaranteed recall");
  }

  for (std::uint64_t index = 0; index < 96; ++index) {
    const auto domain_query =
        std::string("entity") + std::to_string(index) + ".example.com";
    auto hits = Query(*store, domain_query);
    if (!hits || !HasHit(*hits, index + 4U,
                         neocortex::proj::EntityKind::kDomain,
                         domain_query)) {
      return Fail("property corpus violated verbatim identifier recall");
    }
  }

  auto original = Dump(*store);
  auto replayed = neocortex::proj::EntityProjection::Rebuild(
      *store, frames, true);
  auto rebuilt = Dump(*store);
  if (!original || !replayed || !replayed->complete || !rebuilt ||
      *original != *rebuilt) {
    return Fail("entity projection replay was not byte-identical");
  }
  return 0;
}

}  // namespace

int main() {
  if (TestTypedExtraction() != 0) {
    return 1;
  }
  const auto path = std::filesystem::temp_directory_path() /
                    ("neocortex-entity-" + std::to_string(::getpid()));
  std::error_code cleanup_error;
  std::filesystem::remove_all(path, cleanup_error);
  const int result = TestGuaranteedRecallAndReplay(path);
  std::filesystem::remove_all(path, cleanup_error);
  return result;
}
