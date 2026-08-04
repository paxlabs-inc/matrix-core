#include "core/env.h"

#include <cerrno>
#include <limits>
#include <sys/random.h>
#include <time.h>

namespace neocortex::core {

std::expected<std::int64_t, Error> SystemClock::NowNs() {
  struct timespec value {};
  if (::clock_gettime(CLOCK_REALTIME, &value) != 0) {
    return std::unexpected(Error{ErrorCode::kReadFailed, errno});
  }
  constexpr std::int64_t kNanosecondsPerSecond = 1'000'000'000;
  if (value.tv_sec >
      std::numeric_limits<std::int64_t>::max() / kNanosecondsPerSecond) {
    return std::unexpected(Error{ErrorCode::kInvariantViolation, 0});
  }
  return static_cast<std::int64_t>(value.tv_sec) * kNanosecondsPerSecond +
         static_cast<std::int64_t>(value.tv_nsec);
}

std::expected<void, Error> SystemEntropy::Fill(std::span<std::byte> output) {
  std::size_t filled = 0;
  while (filled < output.size()) {
    const ssize_t result =
        ::getrandom(output.data() + filled, output.size() - filled, 0);
    if (result < 0) {
      if (errno == EINTR) {
        continue;
      }
      return std::unexpected(Error{ErrorCode::kReadFailed, errno});
    }
    if (result == 0) {
      return std::unexpected(Error{ErrorCode::kReadFailed, 0});
    }
    filled += static_cast<std::size_t>(result);
  }
  return {};
}

}  // namespace neocortex::core
