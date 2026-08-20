#include "proj/ladder/temporal_ladder.h"

#include <algorithm>
#include <array>
#include <limits>
#include <memory>
#include <optional>
#include <utility>

#include <roaring/roaring64.h>

#include "schema/events.h"

namespace neocortex::proj {
namespace {

constexpr std::byte kWindowPrefix{0x57};
constexpr std::array<std::int64_t, 4> kDurationsNs = {
    60'000'000'000LL,
    3'600'000'000'000LL,
    86'400'000'000'000LL,
    604'800'000'000'000LL,
};

struct BitmapDeleter final {
  void operator()(roaring64_bitmap_t* bitmap) const {
    if (bitmap != nullptr) {
      roaring64_bitmap_free(bitmap);
    }
  }
};

using Bitmap = std::unique_ptr<roaring64_bitmap_t, BitmapDeleter>;

struct OwnedMutation final {
  std::vector<std::byte> key;
  std::vector<std::byte> value;
};

std::expected<std::int64_t, Error> Duration(TemporalLevel level) {
  const auto index = static_cast<std::size_t>(level);
  if (index >= kDurationsNs.size()) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  return kDurationsNs[index];
}

std::expected<std::int64_t, Error> WindowStart(std::int64_t timestamp_ns,
                                               std::int64_t duration_ns) {
  std::int64_t quotient = timestamp_ns / duration_ns;
  const std::int64_t remainder = timestamp_ns % duration_ns;
  if (remainder < 0) {
    --quotient;
  }
  if (quotient < std::numeric_limits<std::int64_t>::min() / duration_ns ||
      quotient > std::numeric_limits<std::int64_t>::max() / duration_ns) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  return quotient * duration_ns;
}

std::expected<std::int64_t, Error> WindowEnd(std::int64_t start_ns,
                                             std::int64_t duration_ns) {
  if (start_ns > std::numeric_limits<std::int64_t>::max() - duration_ns) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  return start_ns + duration_ns;
}

std::array<std::byte, 10> WindowKey(TemporalLevel level,
                                    std::int64_t start_ns) {
  std::array<std::byte, 10> key{};
  key[0] = kWindowPrefix;
  key[1] = static_cast<std::byte>(level);
  const std::uint64_t sortable =
      static_cast<std::uint64_t>(start_ns) ^ (std::uint64_t{1} << 63U);
  for (std::size_t index = 0; index < 8; ++index) {
    key[index + 2U] =
        static_cast<std::byte>((sortable >> ((7U - index) * 8U)) & 0xffU);
  }
  return key;
}

std::expected<std::int64_t, Error> DecodeStart(
    std::span<const std::byte> key, TemporalLevel expected_level) {
  if (key.size() != 10 || key[0] != kWindowPrefix ||
      key[1] != static_cast<std::byte>(expected_level)) {
    return std::unexpected(Error{ErrorCode::kTemporalLadderCorrupt, 0});
  }
  std::uint64_t sortable = 0;
  for (std::size_t index = 0; index < 8; ++index) {
    sortable = (sortable << 8U) |
               std::to_integer<std::uint64_t>(key[index + 2U]);
  }
  return static_cast<std::int64_t>(sortable ^ (std::uint64_t{1} << 63U));
}

std::expected<Bitmap, Error> DecodeBitmap(
    const std::optional<std::vector<std::byte>>& encoded) {
  if (!encoded.has_value()) {
    Bitmap created(roaring64_bitmap_create());
    if (!created) {
      return std::unexpected(Error{ErrorCode::kBackendUnavailable, 0});
    }
    return created;
  }
  const auto& bytes = *encoded;
  if (bytes.empty() ||
      roaring64_bitmap_portable_deserialize_size(
          reinterpret_cast<const char*>(bytes.data()), bytes.size()) !=
          bytes.size()) {
    return std::unexpected(Error{ErrorCode::kTemporalLadderCorrupt, 0});
  }
  Bitmap bitmap(roaring64_bitmap_portable_deserialize_safe(
      reinterpret_cast<const char*>(bytes.data()), bytes.size()));
  if (!bitmap) {
    return std::unexpected(Error{ErrorCode::kTemporalLadderCorrupt, 0});
  }
  return bitmap;
}

std::vector<std::byte> EncodeBitmap(const roaring64_bitmap_t* bitmap) {
  std::vector<std::byte> encoded(
      roaring64_bitmap_portable_size_in_bytes(bitmap));
  const auto written = roaring64_bitmap_portable_serialize(
      bitmap, reinterpret_cast<char*>(encoded.data()));
  encoded.resize(written);
  return encoded;
}

std::expected<TemporalWindow, Error> DecodeWindow(const KeyValue& item,
                                                   TemporalLevel level) {
  auto duration = Duration(level);
  auto start = DecodeStart(item.key, level);
  if (!duration || !start) {
    return std::unexpected(duration ? start.error() : duration.error());
  }
  auto end = WindowEnd(*start, *duration);
  if (!end) {
    return std::unexpected(Error{ErrorCode::kTemporalLadderCorrupt, 0});
  }
  auto bitmap = DecodeBitmap(std::optional<std::vector<std::byte>>(item.value));
  if (!bitmap) {
    return std::unexpected(bitmap.error());
  }
  return TemporalWindow{
      .level = level,
      .start_ns = *start,
      .end_ns = *end,
      .member_count = roaring64_bitmap_get_cardinality(bitmap->get()),
  };
}

}  // namespace

std::expected<void, Error> TemporalLadder::ApplyEvent(
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
  std::vector<OwnedMutation> owned;
  owned.reserve(kDurationsNs.size());
  for (std::size_t index = 0; index < kDurationsNs.size(); ++index) {
    const auto level = static_cast<TemporalLevel>(index);
    auto start = WindowStart(frame.header.wall_timestamp_ns,
                             kDurationsNs[index]);
    if (!start) {
      auto error = start.error();
      error.lsn = frame.header.lsn;
      return std::unexpected(error);
    }
    auto key = WindowKey(level, *start);
    auto existing = snapshot->Get(ProjectionId::kTemporalLadder, key);
    if (!existing) {
      return std::unexpected(existing.error());
    }
    auto bitmap = DecodeBitmap(*existing);
    if (!bitmap) {
      auto error = bitmap.error();
      error.lsn = frame.header.lsn;
      return std::unexpected(error);
    }
    roaring64_bitmap_add(bitmap->get(), frame.header.lsn);
    owned.push_back(
        OwnedMutation{.key = std::vector<std::byte>(key.begin(), key.end()),
                      .value = EncodeBitmap(bitmap->get())});
  }
  std::vector<Mutation> mutations;
  mutations.reserve(owned.size());
  for (const auto& mutation : owned) {
    mutations.push_back(Mutation{.kind = MutationKind::kPut,
                                 .key = mutation.key,
                                 .value = mutation.value});
  }
  return store.Apply(ProjectionId::kTemporalLadder, frame.header.lsn,
                     mutations);
}

std::expected<TemporalRebuildProgress, Error> TemporalLadder::Rebuild(
    ProjectionStore& store, std::span<const log::Frame> frames, bool reset,
    std::size_t maximum_frames) {
  if (reset) {
    auto reset_result = store.Reset(ProjectionId::kTemporalLadder);
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
    auto read_checkpoint = snapshot->Checkpoint(ProjectionId::kTemporalLadder);
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
  return TemporalRebuildProgress{.applied_lsn = applied_lsn,
                                 .applied_frames = apply_count,
                                 .complete = applied_lsn == frames.size()};
}

std::expected<std::vector<TemporalWindow>, Error> TemporalLadder::ListWindows(
    const ReadSnapshot& snapshot, TemporalLevel level, std::int64_t from_ns,
    std::int64_t to_ns, std::size_t limit) {
  if (from_ns >= to_ns || limit == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  auto duration = Duration(level);
  if (!duration) {
    return std::unexpected(duration.error());
  }
  auto first_start = WindowStart(from_ns, *duration);
  if (!first_start) {
    return std::unexpected(first_start.error());
  }
  const std::array<std::byte, 2> prefix = {kWindowPrefix,
                                           static_cast<std::byte>(level)};
  const auto first_key = WindowKey(level, *first_start);
  auto items = snapshot.ScanPrefixFrom(ProjectionId::kTemporalLadder, prefix,
                                       first_key, limit);
  if (!items) {
    return std::unexpected(items.error());
  }
  std::vector<TemporalWindow> windows;
  windows.reserve(items->size());
  for (const auto& item : *items) {
    auto window = DecodeWindow(item, level);
    if (!window) {
      return std::unexpected(window.error());
    }
    if (window->start_ns >= to_ns) {
      break;
    }
    if (window->end_ns > from_ns) {
      windows.push_back(*window);
    }
  }
  return windows;
}

std::expected<std::vector<TemporalWindow>, Error> TemporalLadder::OpenWindow(
    const ReadSnapshot& snapshot, const TemporalWindow& window,
    std::size_t limit) {
  auto duration = Duration(window.level);
  if (!duration || window.end_ns <= window.start_ns ||
      window.end_ns - window.start_ns != *duration || limit == 0) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  if (window.level == TemporalLevel::kMinute) {
    return std::vector<TemporalWindow>{};
  }
  const auto child =
      static_cast<TemporalLevel>(static_cast<std::uint8_t>(window.level) - 1U);
  return ListWindows(snapshot, child, window.start_ns, window.end_ns, limit);
}

std::expected<std::vector<std::uint64_t>, Error>
TemporalLadder::ResolveMembers(const ReadSnapshot& snapshot,
                               const TemporalWindow& window,
                               std::size_t limit) {
  auto duration = Duration(window.level);
  if (!duration || limit == 0 || window.end_ns <= window.start_ns ||
      window.end_ns - window.start_ns != *duration) {
    return std::unexpected(Error{ErrorCode::kInvalidArgument, 0});
  }
  const auto key = WindowKey(window.level, window.start_ns);
  auto encoded = snapshot.Get(ProjectionId::kTemporalLadder, key);
  if (!encoded) {
    return std::unexpected(encoded.error());
  }
  if (!encoded->has_value()) {
    return std::vector<std::uint64_t>{};
  }
  auto bitmap = DecodeBitmap(*encoded);
  if (!bitmap) {
    return std::unexpected(bitmap.error());
  }
  const auto count = roaring64_bitmap_get_cardinality(bitmap->get());
  if (count > std::numeric_limits<std::size_t>::max()) {
    return std::unexpected(Error{ErrorCode::kTemporalLadderCorrupt, 0});
  }
  const auto selected =
      std::min<std::uint64_t>(count, static_cast<std::uint64_t>(limit));
  std::vector<std::uint64_t> members(static_cast<std::size_t>(selected));
  if (members.empty()) {
    return members;
  }
  if (selected == count) {
    roaring64_bitmap_to_uint64_array(bitmap->get(), members.data());
    return members;
  }
  struct IteratorDeleter final {
    void operator()(roaring64_iterator_t* iterator) const {
      if (iterator != nullptr) {
        roaring64_iterator_free(iterator);
      }
    }
  };
  const std::unique_ptr<roaring64_iterator_t, IteratorDeleter> iterator(
      roaring64_iterator_create(bitmap->get()));
  if (iterator == nullptr) {
    return std::unexpected(Error{ErrorCode::kTemporalLadderCorrupt, 0});
  }
  const auto read = roaring64_iterator_read(iterator.get(), members.data(),
                                            selected);
  if (read != selected) {
    return std::unexpected(Error{ErrorCode::kTemporalLadderCorrupt, 0});
  }
  return members;
}

}  // namespace neocortex::proj
