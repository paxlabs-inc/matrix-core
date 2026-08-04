#include <algorithm>
#include <array>
#include <atomic>
#include <cerrno>
#include <chrono>
#include <cstddef>
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <deque>
#include <filesystem>
#include <fcntl.h>
#include <poll.h>
#include <optional>
#include <span>
#include <string>
#include <string_view>
#include <sys/inotify.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <sys/wait.h>
#include <thread>
#include <unistd.h>
#include <vector>

#include <bit>

#include <flatbuffers/flatbuffers.h>

#include "core/apply.h"
#include "event_fixture.h"
#include "log/storage_adapter.h"
#include "schema/protocol_generated.h"
#include "serve/actor_engine.h"
#include "serve/protocol.h"

namespace {

using neocortex::protocol::RequestPayload;
using neocortex::protocol::ResponsePayload;
using neocortex::protocol::WirePayload;

int Fail(const char* message) {
  std::fputs(message, stderr);
  std::fputc('\n', stderr);
  return 1;
}

std::filesystem::path TemporaryDirectory() {
  std::array<char, 64> path{};
  constexpr std::string_view pattern = "/tmp/neocortex-cortexd-XXXXXX";
  std::copy(pattern.begin(), pattern.end(), path.begin());
  const char* created = ::mkdtemp(path.data());
  return created == nullptr ? std::filesystem::path{}
                            : std::filesystem::path(created);
}

bool WriteAll(int descriptor, std::span<const std::byte> bytes) {
  std::size_t written = 0;
  while (written < bytes.size()) {
    const auto result =
        ::write(descriptor, bytes.data() + written, bytes.size() - written);
    if (result < 0 && errno == EINTR) {
      continue;
    }
    if (result <= 0) {
      return false;
    }
    written += static_cast<std::size_t>(result);
  }
  return true;
}

struct ConfigPaths final {
  std::filesystem::path path;
  std::filesystem::path socket;
  std::filesystem::path data;
};

bool WriteConfig(const ConfigPaths& paths) {
  const std::string contents =
      "socket=" + paths.socket.string() + "\n" +
      "data=" + paths.data.string() + "\n" +
      "user=0102030405060708090a0b0c0d0e0f10\n" +
      "kek=1111111111111111111111111111111111111111111111111111111111111111\n" +
      "signing_seed=2222222222222222222222222222222222222222222222222222222222222222\n" +
      "admin_token=aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa\n" +
      "actor=11:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb\n" +
      "actor=12:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc\n" +
      "maximum_connections=64\nmaximum_output_frames=512\n";
  const int descriptor =
      ::open(paths.path.c_str(), O_CREAT | O_EXCL | O_WRONLY | O_CLOEXEC, 0600);
  if (descriptor < 0) {
    return false;
  }
  const bool written = WriteAll(
      descriptor,
      std::as_bytes(std::span(contents.data(), contents.size())));
  const bool synced = written && ::fsync(descriptor) == 0;
  const bool closed = ::close(descriptor) == 0;
  return synced && closed;
}

pid_t StartDaemon(const std::filesystem::path& binary,
                  const std::filesystem::path& config,
                  const std::filesystem::path& directory,
                  const std::filesystem::path& socket) {
  const int notifier = ::inotify_init1(IN_CLOEXEC | IN_NONBLOCK);
  if (notifier < 0 ||
      ::inotify_add_watch(notifier, directory.c_str(), IN_CREATE | IN_MOVED_TO) < 0) {
    if (notifier >= 0) {
      ::close(notifier);
    }
    return -1;
  }
  const pid_t child = ::fork();
  if (child == 0) {
    ::execl(binary.c_str(), binary.c_str(), "--config", config.c_str(), nullptr);
    _exit(127);
  }
  if (child < 0) {
    ::close(notifier);
    return -1;
  }
  pollfd descriptor{.fd = notifier, .events = POLLIN, .revents = 0};
  std::array<std::byte, 4096> events{};
  const auto deadline =
      std::chrono::steady_clock::now() + std::chrono::seconds(120);
  bool ready = false;
  while (std::chrono::steady_clock::now() < deadline) {
    const auto remaining = std::chrono::duration_cast<std::chrono::milliseconds>(
        deadline - std::chrono::steady_clock::now());
    const int timeout = static_cast<int>(
        std::max<std::int64_t>(1, remaining.count()));
    const int status = ::poll(&descriptor, 1, timeout);
    if (status < 0 && errno == EINTR) {
      continue;
    }
    if (status <= 0) {
      break;
    }
    static_cast<void>(::read(notifier, events.data(), events.size()));
    struct stat metadata {};
    if (::lstat(socket.c_str(), &metadata) == 0 && S_ISSOCK(metadata.st_mode)) {
      ready = true;
      break;
    }
  }
  ::close(notifier);
  if (!ready) {
    ::kill(child, SIGKILL);
    static_cast<void>(::waitpid(child, nullptr, 0));
    return -1;
  }
  return child;
}

bool StopDaemon(pid_t process, int signal) {
  if (::kill(process, signal) != 0) {
    return false;
  }
  int status = 0;
  return ::waitpid(process, &status, 0) == process;
}

std::array<std::byte, 32> FilledToken(std::byte value) {
  std::array<std::byte, 32> token{};
  token.fill(value);
  return token;
}

std::array<std::byte, 16> ConnectionId(std::uint8_t value) {
  std::array<std::byte, 16> id{};
  id[0] = static_cast<std::byte>(value);
  id[15] = static_cast<std::byte>(value ^ 0xa5U);
  return id;
}

std::array<std::byte, 16> DurableConnectionId(std::uint64_t value) {
  std::array<std::byte, 16> id{};
  for (std::size_t index = 0; index < sizeof(value); ++index) {
    id[index] = static_cast<std::byte>(value & 0xffU);
    value >>= 8U;
  }
  id[15] = std::byte{0xa5};
  return id;
}

class Client final {
 public:
  Client() = default;
  ~Client() {
    if (descriptor_ >= 0) {
      ::close(descriptor_);
    }
  }
  Client(Client&& other) noexcept
      : descriptor_(other.descriptor_),
        parser_(std::move(other.parser_)),
        pending_(std::move(other.pending_)) {
    other.descriptor_ = -1;
  }
  Client& operator=(Client&& other) noexcept {
    if (this != &other) {
      if (descriptor_ >= 0) {
        ::close(descriptor_);
      }
      descriptor_ = other.descriptor_;
      parser_ = std::move(other.parser_);
      pending_ = std::move(other.pending_);
      other.descriptor_ = -1;
    }
    return *this;
  }
  Client(const Client&) = delete;
  Client& operator=(const Client&) = delete;

  static std::optional<Client> Connect(const std::filesystem::path& socket) {
    const int descriptor = ::socket(AF_UNIX, SOCK_STREAM | SOCK_CLOEXEC, 0);
    if (descriptor < 0) {
      return std::nullopt;
    }
    sockaddr_un address{};
    address.sun_family = AF_UNIX;
    if (socket.string().size() >= sizeof(address.sun_path)) {
      ::close(descriptor);
      return std::nullopt;
    }
    std::memcpy(address.sun_path, socket.c_str(), socket.string().size() + 1U);
    if (::connect(descriptor, reinterpret_cast<const sockaddr*>(&address),
                  sizeof(address)) != 0) {
      ::close(descriptor);
      return std::nullopt;
    }
    Client client;
    client.descriptor_ = descriptor;
    return client;
  }

  bool Send(std::span<const std::byte> payload) {
    auto frame = neocortex::serve::EncodeProtocolFrame(payload);
    return frame && WriteAll(descriptor_, *frame);
  }

  std::optional<std::vector<std::byte>> Receive() {
    if (!pending_.empty()) {
      auto frame = std::move(pending_.front());
      pending_.pop_front();
      return frame;
    }
    std::array<std::byte, static_cast<std::size_t>(64) * 1024U> buffer{};
    while (true) {
      pollfd descriptor{.fd = descriptor_, .events = POLLIN, .revents = 0};
      if (::poll(&descriptor, 1, 15000) <= 0) {
        return std::nullopt;
      }
      const auto count = ::read(descriptor_, buffer.data(), buffer.size());
      if (count < 0 && errno == EINTR) {
        continue;
      }
      if (count <= 0) {
        return std::nullopt;
      }
      auto frames = parser_.Push(
          std::span(buffer).first(static_cast<std::size_t>(count)));
      if (!frames || frames->empty()) {
        if (!frames) {
          return std::nullopt;
        }
        continue;
      }
      for (auto& frame : *frames) {
        pending_.push_back(std::move(frame));
      }
      auto frame = std::move(pending_.front());
      pending_.pop_front();
      return frame;
    }
  }

 private:
  int descriptor_ = -1;
  neocortex::serve::ProtocolFrameParser parser_;
  std::deque<std::vector<std::byte>> pending_;
};

std::vector<std::byte> FinishWire(flatbuffers::FlatBufferBuilder& builder,
                                  WirePayload type,
                                  flatbuffers::Offset<void> payload) {
  const auto envelope = neocortex::protocol::CreateWireEnvelope(
      builder, neocortex::serve::kProtocolVersion, type, payload);
  neocortex::protocol::FinishWireEnvelopeBuffer(builder, envelope);
  return {reinterpret_cast<const std::byte*>(builder.GetBufferPointer()),
              reinterpret_cast<const std::byte*>(builder.GetBufferPointer()) +
                  builder.GetSize()};
}

struct RequestSequence final {
  std::uint64_t request_id;
  std::uint64_t client_seq;
};

struct AssertionEventSpec final {
  std::uint8_t belief_id;
  std::string_view identity;
  std::string_view value;
  std::uint64_t provenance_first;
  std::uint64_t provenance_last;
  neocortex::schema::AssertionClaim claim;
};

struct RetractEventSpec final {
  std::uint8_t belief_id;
  std::uint64_t provenance_lsn;
};

struct ToolResultEventSpec final {
  std::uint8_t call_id;
  std::uint64_t tool_call_lsn;
};

std::vector<std::byte> FinishEvent(
    flatbuffers::FlatBufferBuilder& builder,
    neocortex::schema::EventPayload payload_type,
    flatbuffers::Offset<void> payload,
    neocortex::test::EventIdempotency idempotency) {
  const auto connection = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(idempotency.connection_id.data()),
      idempotency.connection_id.size());
  const auto envelope = neocortex::schema::CreateEventEnvelope(
      builder, 1, payload_type, payload, connection, idempotency.client_seq,
      idempotency.event_index, idempotency.event_count);
  neocortex::schema::FinishEventEnvelopeBuffer(builder, envelope);
  return {reinterpret_cast<const std::byte*>(builder.GetBufferPointer()),
          reinterpret_cast<const std::byte*>(builder.GetBufferPointer()) +
              builder.GetSize()};
}

std::vector<std::byte> BuildAssertionEvent(
    AssertionEventSpec spec,
    neocortex::test::EventIdempotency idempotency) {
  flatbuffers::FlatBufferBuilder builder(512);
  const auto id = builder.CreateVector(&spec.belief_id, 1);
  const auto identity_offset = builder.CreateString(spec.identity);
  const auto value_offset = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(spec.value.data()),
      spec.value.size());
  const neocortex::schema::ProvenanceRange range(spec.provenance_first,
                                                  spec.provenance_last);
  const auto provenance = builder.CreateVectorOfStructs(&range, 1);
  const auto assertion = neocortex::schema::CreateAssertion(
      builder, id, neocortex::schema::BeliefType::fact, identity_offset,
      value_offset, 1, 0, provenance, {}, spec.claim);
  return FinishEvent(builder, neocortex::schema::EventPayload::Assertion,
                     assertion.Union(), idempotency);
}

std::vector<std::byte> BuildRetractEvent(
    RetractEventSpec spec,
    neocortex::test::EventIdempotency idempotency) {
  flatbuffers::FlatBufferBuilder builder(256);
  const auto id = builder.CreateVector(&spec.belief_id, 1);
  const neocortex::schema::ProvenanceRange range(spec.provenance_lsn,
                                                  spec.provenance_lsn);
  const auto provenance = builder.CreateVectorOfStructs(&range, 1);
  const auto retract =
      neocortex::schema::CreateRetract(builder, id, provenance);
  return FinishEvent(builder, neocortex::schema::EventPayload::Retract,
                     retract.Union(), idempotency);
}

std::vector<std::byte> BuildToolCallEvent(
    std::uint8_t call_id, neocortex::test::EventIdempotency idempotency) {
  flatbuffers::FlatBufferBuilder builder(256);
  const auto id = builder.CreateVector(&call_id, 1);
  const auto name = builder.CreateString("admission-probe");
  const std::uint8_t argument = 1;
  const auto arguments = builder.CreateVector(&argument, 1);
  const auto call =
      neocortex::schema::CreateToolCall(builder, id, name, arguments);
  return FinishEvent(builder, neocortex::schema::EventPayload::ToolCall,
                     call.Union(), idempotency);
}

std::vector<std::byte> BuildToolResultEvent(
    ToolResultEventSpec spec,
    neocortex::test::EventIdempotency idempotency) {
  flatbuffers::FlatBufferBuilder builder(256);
  const auto id = builder.CreateVector(&spec.call_id, 1);
  const std::uint8_t result_value = 1;
  const auto result = builder.CreateVector(&result_value, 1);
  const auto tool_result = neocortex::schema::CreateToolResult(
      builder, id, spec.tool_call_lsn,
      neocortex::schema::ResultStatus::ok, result);
  return FinishEvent(builder, neocortex::schema::EventPayload::ToolResult,
                     tool_result.Union(), idempotency);
}

struct SocketAppendEvent final {
  neocortex::log::EventKind kind;
  std::vector<std::byte> payload;
};

std::vector<std::byte> AppendEventsWire(
    RequestSequence sequence, std::span<const SocketAppendEvent> values) {
  flatbuffers::FlatBufferBuilder builder(4096);
  std::array<std::uint8_t, 16> conversation{};
  conversation[0] = 1;
  const auto conversation_offset =
      builder.CreateVector(conversation.data(), conversation.size());
  std::vector<flatbuffers::Offset<neocortex::protocol::AppendEvent>> events;
  events.reserve(values.size());
  for (const auto& value : values) {
    const auto payload = builder.CreateVector(
        reinterpret_cast<const std::uint8_t*>(value.payload.data()),
        value.payload.size());
    events.push_back(neocortex::protocol::CreateAppendEvent(
        builder, static_cast<std::uint8_t>(value.kind), conversation_offset,
        payload));
  }
  const auto events_offset = builder.CreateVector(events);
  const auto append = neocortex::protocol::CreateAppend(
      builder, sequence.client_seq, events_offset);
  const auto request = neocortex::protocol::CreateRequest(
      builder, sequence.request_id, RequestPayload::Append, append.Union());
  return FinishWire(builder, WirePayload::Request, request.Union());
}

bool Handshake(Client& client, const std::array<std::byte, 16>& connection,
               const std::array<std::byte, 32>& token,
               std::uint16_t expected_actor, bool expected_admin) {
  flatbuffers::FlatBufferBuilder builder(256);
  const auto id = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(connection.data()), connection.size());
  const auto capability = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(token.data()), token.size());
  const auto hello = neocortex::protocol::CreateHello(
      builder, neocortex::serve::kProtocolVersion, id, capability);
  const auto wire = FinishWire(builder, WirePayload::Hello, hello.Union());
  auto response = client.Send(wire) ? client.Receive() : std::nullopt;
  if (!response) {
    return false;
  }
  auto verified = neocortex::serve::VerifyProtocolPayload(*response);
  if (!verified || (*verified)->payload_type() != WirePayload::Welcome) {
    return false;
  }
  const auto* welcome = (*verified)->payload_as_Welcome();
  return welcome->actor_ns() == expected_actor &&
         welcome->admin() == expected_admin;
}

std::vector<std::byte> AppendWire(
    RequestSequence sequence,
    const std::array<std::byte, 16>& connection, std::string_view content) {
  const auto event = neocortex::test::BuildEvent(
      neocortex::log::EventKind::kUserMsg, 1,
      std::as_bytes(std::span(content.data(), content.size())),
      neocortex::test::EventIdempotency{
          .connection_id = connection,
          .client_seq = sequence.client_seq,
          .event_index = 0,
          .event_count = 1});
  std::array<std::uint8_t, 16> conversation{};
  conversation[0] = connection[0] == std::byte{0}
                        ? 1U
                        : std::to_integer<std::uint8_t>(connection[0]);
  flatbuffers::FlatBufferBuilder builder(event.size() + 512U);
  const auto conversation_offset =
      builder.CreateVector(conversation.data(), conversation.size());
  const auto payload = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(event.data()), event.size());
  const auto append_event = neocortex::protocol::CreateAppendEvent(
      builder, static_cast<std::uint8_t>(neocortex::log::EventKind::kUserMsg),
      conversation_offset, payload);
  const auto events = builder.CreateVector(&append_event, 1);
  const auto append =
      neocortex::protocol::CreateAppend(builder, sequence.client_seq, events);
  const auto request = neocortex::protocol::CreateRequest(
      builder, sequence.request_id, RequestPayload::Append, append.Union());
  return FinishWire(builder, WirePayload::Request, request.Union());
}

std::vector<std::byte> AppendEventWire(
    RequestSequence sequence, const std::array<std::byte, 16>& connection,
    neocortex::log::EventKind kind, std::uint64_t fixture_lsn,
    std::string_view content, std::uint8_t conversation_byte) {
  const auto event = neocortex::test::BuildEvent(
      kind, fixture_lsn,
      std::as_bytes(std::span(content.data(), content.size())),
      neocortex::test::EventIdempotency{
          .connection_id = connection,
          .client_seq = sequence.client_seq,
          .event_index = 0,
          .event_count = 1});
  std::array<std::uint8_t, 16> conversation{};
  conversation[0] = conversation_byte;
  flatbuffers::FlatBufferBuilder builder(event.size() + 512U);
  const auto conversation_offset =
      builder.CreateVector(conversation.data(), conversation.size());
  const auto payload = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(event.data()), event.size());
  const auto append_event = neocortex::protocol::CreateAppendEvent(
      builder, static_cast<std::uint8_t>(kind), conversation_offset, payload);
  const auto events = builder.CreateVector(&append_event, 1);
  const auto append =
      neocortex::protocol::CreateAppend(builder, sequence.client_seq, events);
  const auto request = neocortex::protocol::CreateRequest(
      builder, sequence.request_id, RequestPayload::Append, append.Union());
  return FinishWire(builder, WirePayload::Request, request.Union());
}

struct ConversationQuery final {
  std::uint64_t request_id;
  std::uint8_t conversation;
};

struct TranscriptQuery final {
  std::uint64_t request_id;
  std::uint8_t conversation;
  std::uint64_t since_lsn;
  std::uint32_t limit;
};

std::vector<std::byte> ActivateWire(ConversationQuery value,
                                    std::string_view query) {
  flatbuffers::FlatBufferBuilder builder(2048);
  std::array<std::uint8_t, 16> conversation{};
  conversation[0] = value.conversation;
  const auto conversation_offset =
      builder.CreateVector(conversation.data(), conversation.size());
  const auto query_offset = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(query.data()), query.size());
  const std::vector<std::uint16_t> weights(256, 256);
  const auto weights_offset = builder.CreateVector(weights);
  const auto activate = neocortex::protocol::CreateActivate(
      builder, conversation_offset, query_offset, 0, 1'000'000, 0, 0, 0,
      4'000'000'000'000'000'000LL, weights_offset, 1);
  const auto request = neocortex::protocol::CreateRequest(
      builder, value.request_id, RequestPayload::Activate, activate.Union());
  return FinishWire(builder, WirePayload::Request, request.Union());
}

std::vector<std::byte> TranscriptWire(TranscriptQuery value) {
  flatbuffers::FlatBufferBuilder builder(512);
  std::array<std::uint8_t, 16> conversation{};
  conversation[0] = value.conversation;
  const auto conversation_offset =
      builder.CreateVector(conversation.data(), conversation.size());
  const auto transcript = neocortex::protocol::CreateTranscript(
      builder, conversation_offset, value.since_lsn, value.limit);
  const auto request = neocortex::protocol::CreateRequest(
      builder, value.request_id, RequestPayload::Transcript,
      transcript.Union());
  return FinishWire(builder, WirePayload::Request, request.Union());
}

std::vector<std::byte> RecallWire(std::uint64_t request_id,
                                  neocortex::protocol::RecallMode mode,
                                  std::uint8_t level, std::int64_t start_ns,
                                  std::int64_t end_ns, std::uint32_t limit) {
  flatbuffers::FlatBufferBuilder builder(512);
  const std::uint8_t query_value = 'q';
  const auto query = builder.CreateVector(&query_value, 1);
  const auto recall = neocortex::protocol::CreateRecall(
      builder, query, limit, mode, level, start_ns, end_ns);
  const auto request = neocortex::protocol::CreateRequest(
      builder, request_id, RequestPayload::Recall, recall.Union());
  return FinishWire(builder, WirePayload::Request, request.Union());
}

std::vector<std::byte> AsOfWire(std::uint64_t request_id,
                                std::string_view identity,
                                std::int64_t valid_time_ns,
                                std::uint64_t transaction_lsn) {
  flatbuffers::FlatBufferBuilder builder(512);
  const auto identity_offset = builder.CreateString(identity);
  const auto as_of = neocortex::protocol::CreateAsOf(
      builder, 0, identity_offset, valid_time_ns, transaction_lsn);
  const auto request = neocortex::protocol::CreateRequest(
      builder, request_id, RequestPayload::AsOf, as_of.Union());
  return FinishWire(builder, WirePayload::Request, request.Union());
}

std::vector<std::byte> CheckpointWire(RequestSequence sequence,
                                      std::string_view turn_id,
                                      std::string_view blob) {
  flatbuffers::FlatBufferBuilder builder(512);
  const auto turn_offset = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(turn_id.data()), turn_id.size());
  const auto blob_offset = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(blob.data()), blob.size());
  const auto checkpoint = neocortex::protocol::CreateCheckpoint(
      builder, turn_offset, blob_offset, sequence.client_seq);
  const auto request = neocortex::protocol::CreateRequest(
      builder, sequence.request_id, RequestPayload::Checkpoint,
      checkpoint.Union());
  return FinishWire(builder, WirePayload::Request, request.Union());
}

std::vector<std::byte> LatestCheckpointWire(std::uint64_t request_id,
                                            std::string_view turn_id) {
  flatbuffers::FlatBufferBuilder builder(512);
  const auto turn_offset = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(turn_id.data()), turn_id.size());
  const auto latest =
      neocortex::protocol::CreateLatestCheckpoint(builder, turn_offset);
  const auto request = neocortex::protocol::CreateRequest(
      builder, request_id, RequestPayload::LatestCheckpoint, latest.Union());
  return FinishWire(builder, WirePayload::Request, request.Union());
}

std::vector<std::byte> AttestWire(RequestSequence sequence,
                                  std::span<const std::uint64_t> used,
                                  std::span<const std::uint64_t> ignored) {
  flatbuffers::FlatBufferBuilder builder(512);
  const auto used_offset =
      used.empty() ? flatbuffers::Offset<flatbuffers::Vector<std::uint64_t>>{}
                   : builder.CreateVector(used.data(), used.size());
  const auto ignored_offset =
      ignored.empty()
          ? flatbuffers::Offset<flatbuffers::Vector<std::uint64_t>>{}
          : builder.CreateVector(ignored.data(), ignored.size());
  const auto attest = neocortex::protocol::CreateAttest(
      builder, used_offset, ignored_offset, sequence.client_seq);
  const auto request = neocortex::protocol::CreateRequest(
      builder, sequence.request_id, RequestPayload::Attest, attest.Union());
  return FinishWire(builder, WirePayload::Request, request.Union());
}

struct SubscriptionRequest final {
  std::uint64_t request_id;
  std::uint64_t since_lsn;
};

std::vector<std::byte> SubscribeWire(SubscriptionRequest value) {
  flatbuffers::FlatBufferBuilder builder(256);
  const auto subscribe =
      neocortex::protocol::CreateSubscribe(builder, 0, value.since_lsn);
  const auto request = neocortex::protocol::CreateRequest(
      builder, value.request_id, RequestPayload::Subscribe, subscribe.Union());
  return FinishWire(builder, WirePayload::Request, request.Union());
}

std::vector<std::byte> AdminWire(std::uint64_t request_id,
                                 RequestPayload type, std::uint16_t actor,
                                 std::string_view projection = {}) {
  flatbuffers::FlatBufferBuilder builder(512);
  flatbuffers::Offset<void> payload;
  switch (type) {
    case RequestPayload::Health:
      payload = neocortex::protocol::CreateHealth(builder).Union();
      break;
    case RequestPayload::Stats:
      payload = neocortex::protocol::CreateStats(builder, actor).Union();
      break;
    case RequestPayload::LatencyHistograms:
      payload = neocortex::protocol::CreateLatencyHistograms(builder).Union();
      break;
    case RequestPayload::VerifyStatus:
      payload = neocortex::protocol::CreateVerifyStatus(builder, actor).Union();
      break;
    case RequestPayload::RebuildProjection:
      payload = neocortex::protocol::CreateRebuildProjection(
                    builder, actor, builder.CreateString(projection))
                    .Union();
      break;
    default:
      return {};
  }
  const auto request =
      neocortex::protocol::CreateRequest(builder, request_id, type, payload);
  return FinishWire(builder, WirePayload::Request, request.Union());
}

const neocortex::protocol::Response* Response(
    const std::vector<std::byte>& payload) {
  auto verified = neocortex::serve::VerifyProtocolPayload(payload);
  return verified && (*verified)->payload_type() == WirePayload::Response
             ? (*verified)->payload_as_Response()
             : nullptr;
}

bool AppendAndExpect(Client& client, RequestSequence sequence,
                     const std::array<std::byte, 16>& connection,
                     std::string_view content, bool duplicate) {
  const auto wire = AppendWire(sequence, connection, content);
  auto payload = client.Send(wire) ? client.Receive() : std::nullopt;
  if (!payload) {
    return false;
  }
  const auto* response = Response(*payload);
  const auto* ack = response == nullptr ? nullptr : response->payload_as_AppendAck();
  return response != nullptr &&
         response->status() == neocortex::protocol::ResponseStatus::ok &&
         ack != nullptr && ack->client_seq() == sequence.client_seq &&
         ack->first_lsn() > 0 && ack->last_lsn() >= ack->first_lsn() &&
         ack->duplicate() == duplicate && ack->leaf_count() >= ack->last_lsn() &&
         ack->last_leaf_hash() != nullptr &&
         ack->last_leaf_hash()->size() == 32 && ack->mmr_root() != nullptr &&
         ack->mmr_root()->size() == 32;
}

std::expected<std::unique_ptr<neocortex::serve::ActorEngine>, neocortex::Error>
OpenEngine(const std::filesystem::path& actor_directory) {
  neocortex::seal::UserId user{};
  for (std::size_t index = 0; index < user.size(); ++index) {
    user[index] = static_cast<std::byte>(index + 1U);
  }
  neocortex::seal::KeyEncryptionKey kek{};
  kek.fill(std::byte{0x11});
  neocortex::mmr::SigningSeed seed{};
  seed.fill(std::byte{0x22});
  auto signing = neocortex::mmr::SigningKeyPairFromSeed(seed);
  if (!signing) {
    return std::unexpected(signing.error());
  }
  return neocortex::serve::ActorEngine::Open(
      {.actor_directory = actor_directory,
       .actor = 11,
       .user = user,
       .kek = kek,
       .signing_key = *signing});
}

bool SeedDurableConnections(const std::filesystem::path& actor_directory,
                            std::uint64_t connection_count) {
  neocortex::seal::UserId user{};
  for (std::size_t index = 0; index < user.size(); ++index) {
    user[index] = static_cast<std::byte>(index + 1U);
  }
  neocortex::seal::KeyEncryptionKey kek{};
  kek.fill(std::byte{0x11});
  neocortex::mmr::SigningSeed seed{};
  seed.fill(std::byte{0x22});
  auto signing = neocortex::mmr::SigningKeyPairFromSeed(seed);
  if (!signing) {
    return false;
  }
  neocortex::core::SystemEntropy entropy;
  auto storage = neocortex::log::SegmentStorage::Open(
      actor_directory, 11, signing->public_key, user, kek, entropy,
      {.backend = neocortex::log::WriteBackend::kPwrite});
  if (!storage) {
    return false;
  }
  neocortex::core::SystemClock clock;
  auto loop = neocortex::core::ApplyLoop::Boot(
      {.clock = clock, .entropy = entropy, .storage = *storage, .actor = 11},
      connection_count);
  if (!loop) {
    return false;
  }
  const std::string content = "durable-sequence";
  std::vector<std::vector<std::byte>> payloads;
  payloads.reserve(connection_count);
  for (std::uint64_t ordinal = 0; ordinal < connection_count; ++ordinal) {
    payloads.push_back(neocortex::test::BuildEvent(
        neocortex::log::EventKind::kUserMsg, ordinal + 1U,
        std::as_bytes(std::span(content.data(), content.size())),
        neocortex::test::EventIdempotency{
            .connection_id = DurableConnectionId(ordinal),
            .client_seq = 1,
            .event_index = 0,
            .event_count = 1}));
  }
  std::vector<neocortex::core::ApplyRequest> requests;
  requests.reserve(connection_count);
  neocortex::log::ConversationId conversation{};
  conversation.bytes[0] = std::byte{7};
  for (const auto& payload : payloads) {
    requests.push_back(neocortex::core::ApplyRequest{
        .kind = neocortex::log::EventKind::kUserMsg,
        .conversation = conversation,
        .plaintext_payload = payload,
        .boundary = neocortex::events::Boundary::kSocket});
  }
  return loop->ApplyBatch(requests).has_value();
}

std::vector<std::byte> BatchAppendBuffer(
    const std::array<std::byte, 16>& connection, std::uint64_t client_seq,
    std::span<const std::string> contents) {
  flatbuffers::FlatBufferBuilder builder(16384);
  std::vector<flatbuffers::Offset<neocortex::protocol::AppendEvent>> events;
  events.reserve(contents.size());
  std::array<std::uint8_t, 16> conversation{};
  conversation[0] = 7;
  for (std::size_t index = 0; index < contents.size(); ++index) {
    const auto event = neocortex::test::BuildEvent(
        neocortex::log::EventKind::kUserMsg, index + 1U,
        std::as_bytes(std::span(contents[index].data(), contents[index].size())),
        neocortex::test::EventIdempotency{
            .connection_id = connection,
            .client_seq = client_seq,
            .event_index = static_cast<std::uint32_t>(index),
            .event_count = static_cast<std::uint32_t>(contents.size())});
    const auto conversation_offset =
        builder.CreateVector(conversation.data(), conversation.size());
    const auto payload = builder.CreateVector(
        reinterpret_cast<const std::uint8_t*>(event.data()), event.size());
    events.push_back(neocortex::protocol::CreateAppendEvent(
        builder, static_cast<std::uint8_t>(neocortex::log::EventKind::kUserMsg),
        conversation_offset, payload));
  }
  const auto events_offset = builder.CreateVector(events);
  const auto append =
      neocortex::protocol::CreateAppend(builder, client_seq, events_offset);
  builder.Finish(append);
  return {reinterpret_cast<const std::byte*>(builder.GetBufferPointer()),
          reinterpret_cast<const std::byte*>(builder.GetBufferPointer()) +
              builder.GetSize()};
}

std::expected<neocortex::serve::AppendResult, neocortex::Error> EngineAppend(
    neocortex::serve::ActorEngine& engine,
    const std::array<std::byte, 16>& connection,
    std::span<const std::byte> buffer) {
  const auto* request = flatbuffers::GetRoot<neocortex::protocol::Append>(
      reinterpret_cast<const std::uint8_t*>(buffer.data()));
  return engine.Append(connection, *request);
}

std::filesystem::path TailSegment(const std::filesystem::path& actor_directory) {
  std::filesystem::path tail;
  std::error_code error;
  std::filesystem::directory_iterator iterator(actor_directory / "log", error);
  const std::filesystem::directory_iterator end;
  while (!error && iterator != end) {
    const auto name = iterator->path().filename().string();
    if (name.ends_with(".seg") &&
        (tail.empty() || name > tail.filename().string())) {
      tail = iterator->path();
    }
    iterator.increment(error);
  }
  return error ? std::filesystem::path{} : tail;
}

bool ShrinkFile(const std::filesystem::path& path, std::uint64_t remove_bytes) {
  std::error_code error;
  const auto size = std::filesystem::file_size(path, error);
  if (error || size <= remove_bytes) {
    return false;
  }
  return ::truncate(path.c_str(), static_cast<off_t>(size - remove_bytes)) == 0;
}

bool SetPeaksLeafCount(const std::filesystem::path& actor_directory,
                       std::uint64_t leaf_count) {
  const auto record_count =
      leaf_count * 2U - static_cast<std::uint64_t>(std::popcount(leaf_count));
  return ::truncate((actor_directory / "mmr" / "peaks").c_str(),
                    static_cast<off_t>(record_count * 60U)) == 0;
}

bool DropDerivedState(const std::filesystem::path& actor_directory) {
  std::error_code error;
  std::filesystem::remove_all(actor_directory / "projections", error);
  if (error) {
    return false;
  }
  std::filesystem::remove(actor_directory / "vectors.flat", error);
  return !error;
}

std::size_t CountOccurrences(std::span<const std::byte> haystack,
                             std::string_view needle) {
  if (needle.empty() || haystack.size() < needle.size()) {
    return 0;
  }
  std::size_t count = 0;
  for (std::size_t offset = 0; offset + needle.size() <= haystack.size();
       ++offset) {
    if (std::memcmp(haystack.data() + offset, needle.data(), needle.size()) ==
        0) {
      ++count;
    }
  }
  return count;
}

bool TestTornBatchRollback() {
  const auto directory = TemporaryDirectory();
  if (directory.empty()) {
    return false;
  }
  const auto connection = ConnectionId(9);
  const std::vector<std::string> solo = {"torn-solo"};
  const std::vector<std::string> batch = {
      "torn-batch-a", "torn-batch-b",
      "torn-batch-c" + std::string(8192, 'z')};
  // The oversized tail event guarantees the shrink below tears the final
  // frame even when the group write pads to the 4096-byte direct alignment.
  constexpr std::uint64_t kTearBytes = 4096U + 5U;

  const auto torn_directory = directory / "torn";
  {
    auto engine = OpenEngine(torn_directory);
    if (!engine) {
      return false;
    }
    auto first =
        EngineAppend(**engine, connection, BatchAppendBuffer(connection, 1, solo));
    if (!first || first->first_lsn != 1 || first->last_lsn != 1) {
      return false;
    }
    auto second = EngineAppend(**engine, connection,
                               BatchAppendBuffer(connection, 2, batch));
    if (!second || second->duplicate || second->first_lsn != 2 ||
        second->last_lsn != 4) {
      return false;
    }
  }
  if (!SetPeaksLeafCount(torn_directory, 1) ||
      !ShrinkFile(TailSegment(torn_directory), kTearBytes) ||
      !DropDerivedState(torn_directory)) {
    return false;
  }
  {
    auto engine = OpenEngine(torn_directory);
    if (!engine) {
      return false;
    }
    auto stats = (*engine)->Stats();
    if (!stats || stats->log_events != 1 ||
        (*engine)->VerificationStatus().leaf_count != 1) {
      return false;
    }
    auto resent = EngineAppend(**engine, connection,
                               BatchAppendBuffer(connection, 2, batch));
    if (!resent || resent->duplicate || resent->first_lsn != 2 ||
        resent->last_lsn != 4 ||
        (*engine)->VerificationStatus().leaf_count != 4) {
      return false;
    }
    auto scan = (*engine)->FramesSince(
        1, std::nullopt,
        neocortex::serve::FrameScanLimits{
            .maximum_frames = 16,
            .maximum_payload_bytes =
                static_cast<std::size_t>(8) * 1024U * 1024U,
            .maximum_scanned_frames = 1024});
    if (!scan || !scan->exhausted || scan->frames.size() != 3) {
      return false;
    }
    for (const auto& content : batch) {
      std::size_t hits = 0;
      for (const auto& frame : scan->frames) {
        hits += CountOccurrences(frame.sealed_payload, content);
      }
      if (hits != 1) {
        return false;
      }
    }
  }
  {
    auto engine = OpenEngine(torn_directory);
    if (!engine) {
      return false;
    }
    auto stats = (*engine)->Stats();
    if (!stats || stats->log_events != 4) {
      return false;
    }
    auto duplicate = EngineAppend(**engine, connection,
                                  BatchAppendBuffer(connection, 2, batch));
    if (!duplicate || !duplicate->duplicate || duplicate->first_lsn != 2 ||
        duplicate->last_lsn != 4) {
      return false;
    }
  }

  const auto empty_directory = directory / "empty";
  {
    auto engine = OpenEngine(empty_directory);
    if (!engine) {
      return false;
    }
    auto first = EngineAppend(**engine, connection,
                              BatchAppendBuffer(connection, 1, batch));
    if (!first || first->first_lsn != 1 || first->last_lsn != 3) {
      return false;
    }
  }
  if (!SetPeaksLeafCount(empty_directory, 0) ||
      !ShrinkFile(TailSegment(empty_directory), kTearBytes) ||
      !DropDerivedState(empty_directory)) {
    return false;
  }
  {
    auto engine = OpenEngine(empty_directory);
    if (!engine) {
      return false;
    }
    auto stats = (*engine)->Stats();
    if (!stats || stats->log_events != 0 ||
        (*engine)->VerificationStatus().leaf_count != 0) {
      return false;
    }
    auto resent = EngineAppend(**engine, connection,
                               BatchAppendBuffer(connection, 1, batch));
    if (!resent || resent->duplicate || resent->first_lsn != 1 ||
        resent->last_lsn != 3 ||
        (*engine)->VerificationStatus().leaf_count != 3) {
      return false;
    }
  }

  const auto complete_directory = directory / "complete";
  {
    auto engine = OpenEngine(complete_directory);
    if (!engine) {
      return false;
    }
    auto first =
        EngineAppend(**engine, connection, BatchAppendBuffer(connection, 1, solo));
    auto second = EngineAppend(**engine, connection,
                               BatchAppendBuffer(connection, 2, batch));
    if (!first || !second || second->first_lsn != 2 || second->last_lsn != 4) {
      return false;
    }
  }
  if (!SetPeaksLeafCount(complete_directory, 1)) {
    return false;
  }
  {
    auto engine = OpenEngine(complete_directory);
    if (!engine) {
      return false;
    }
    auto stats = (*engine)->Stats();
    if (!stats || stats->log_events != 4 ||
        (*engine)->VerificationStatus().leaf_count != 4) {
      return false;
    }
    auto duplicate = EngineAppend(**engine, connection,
                                  BatchAppendBuffer(connection, 2, batch));
    if (!duplicate || !duplicate->duplicate || duplicate->first_lsn != 2 ||
        duplicate->last_lsn != 4) {
      return false;
    }
  }

  std::error_code cleanup_error;
  std::filesystem::remove_all(directory, cleanup_error);
  return !cleanup_error;
}

bool TestDurableSequenceAuthority() {
  const auto directory = TemporaryDirectory();
  if (directory.empty()) {
    return false;
  }
  const auto actor_directory = directory / "durable-sequence";
  const auto first_connection = DurableConnectionId(0);
  const std::vector<std::string> contents = {"durable-sequence"};
  constexpr std::uint64_t kExistingConnections = 4096;
  if (!SeedDurableConnections(actor_directory, kExistingConnections)) {
    return false;
  }
  {
    auto engine = OpenEngine(actor_directory);
    if (!engine) {
      return false;
    }
    auto next = (*engine)->NextClientSequence(first_connection);
    if (!next || *next != 2) {
      return false;
    }
    const auto boundary_connection =
        DurableConnectionId(kExistingConnections);
    auto boundary = EngineAppend(
        **engine, boundary_connection,
        BatchAppendBuffer(boundary_connection, 1, contents));
    if (boundary ||
        boundary.error().code != neocortex::ErrorCode::kCapacityExceeded) {
      return false;
    }
    auto boundary_next =
        (*engine)->NextClientSequence(boundary_connection);
    if (boundary_next || boundary_next.error().code !=
                             neocortex::ErrorCode::kCapacityExceeded) {
      return false;
    }
    next = (*engine)->NextClientSequence(first_connection);
    if (!next || *next != 2) {
      return false;
    }
  }
  {
    auto engine = OpenEngine(actor_directory);
    if (!engine) {
      return false;
    }
    auto next = (*engine)->NextClientSequence(first_connection);
    if (!next || *next != 2) {
      return false;
    }
    auto duplicate = EngineAppend(
        **engine, first_connection,
        BatchAppendBuffer(first_connection, 1, contents));
    if (!duplicate || !duplicate->duplicate || duplicate->client_seq != 1) {
      return false;
    }
    auto appended = EngineAppend(
        **engine, first_connection,
        BatchAppendBuffer(first_connection, 2, contents));
    if (!appended || appended->duplicate || appended->client_seq != 2) {
      return false;
    }
  }
  std::error_code cleanup_error;
  std::filesystem::remove_all(directory, cleanup_error);
  return !cleanup_error;
}

bool TestHandshakeSequenceFailureIsAtomic(
    const std::filesystem::path& binary) {
  const auto directory = TemporaryDirectory();
  if (directory.empty()) {
    return false;
  }
  const auto socket = directory / "cortexd.sock";
  const auto data = directory / "data";
  const auto config = directory / "cortexd.conf";
  constexpr std::uint64_t kExistingConnections = 4096;
  if (!SeedDurableConnections(data / "11", kExistingConnections) ||
      !WriteConfig({.path = config, .socket = socket, .data = data})) {
    return false;
  }
  const pid_t daemon = StartDaemon(binary, config, directory, socket);
  auto client = daemon < 0 ? std::optional<Client>{} : Client::Connect(socket);
  if (daemon < 0 || !client) {
    if (daemon >= 0) {
      StopDaemon(daemon, SIGKILL);
    }
    return false;
  }
  const auto actor_token = FilledToken(std::byte{0xbb});
  const auto unknown = DurableConnectionId(kExistingConnections);
  flatbuffers::FlatBufferBuilder builder(256);
  const auto id = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(unknown.data()), unknown.size());
  const auto capability = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(actor_token.data()),
      actor_token.size());
  const auto hello = neocortex::protocol::CreateHello(
      builder, neocortex::serve::kProtocolVersion, id, capability);
  const auto wire = FinishWire(builder, WirePayload::Hello, hello.Union());
  auto payload = client->Send(wire) ? client->Receive() : std::nullopt;
  const auto* response = payload ? Response(*payload) : nullptr;
  if (response == nullptr ||
      response->status() != neocortex::protocol::ResponseStatus::error ||
      response->payload_as_ErrorDetail() == nullptr ||
      response->payload_as_ErrorDetail()->code() !=
          static_cast<std::uint8_t>(neocortex::ErrorCode::kCapacityExceeded) ||
      !Handshake(*client, DurableConnectionId(0), actor_token, 11, false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  if (!StopDaemon(daemon, SIGTERM)) {
    return false;
  }
  std::error_code cleanup_error;
  std::filesystem::remove_all(directory, cleanup_error);
  return !cleanup_error;
}

bool TestLatestCheckpointDeepScan() {
  const auto directory = TemporaryDirectory();
  if (directory.empty()) {
    return false;
  }
  auto engine = OpenEngine(directory / "deep");
  if (!engine) {
    return false;
  }
  const auto connection = ConnectionId(21);
  const std::string_view turn = "turn-deep";
  const auto turn_bytes = std::as_bytes(std::span(turn.data(), turn.size()));
  const auto blob_bytes = [](std::string_view blob) {
    return std::as_bytes(std::span(blob.data(), blob.size()));
  };
  const auto blob_equals = [](const std::vector<std::byte>& stored,
                              std::string_view expected) {
    return stored.size() == expected.size() &&
           std::memcmp(stored.data(), expected.data(), expected.size()) == 0;
  };
  auto first = (*engine)->WriteCheckpoint(connection, 1, turn_bytes,
                                          blob_bytes("deep-blob-one"));
  if (!first) {
    return false;
  }
  std::uint64_t sequence = 2;
  const auto append_filler = [&](std::size_t batches) {
    for (std::size_t batch = 0; batch < batches; ++batch) {
      std::vector<std::string> contents;
      contents.reserve(256);
      for (std::size_t index = 0; index < 256; ++index) {
        contents.push_back("filler-" + std::to_string(sequence) + "-" +
                           std::to_string(index));
      }
      auto appended = EngineAppend(
          **engine, connection,
          BatchAppendBuffer(connection, sequence, contents));
      if (!appended || appended->duplicate) {
        return false;
      }
      ++sequence;
    }
    return true;
  };
  if (!append_filler(5)) {
    return false;
  }
  auto read_one = (*engine)->LatestCheckpoint(turn_bytes);
  if (!read_one) {
    return false;
  }
  const auto checkpoint_one =
      read_one->value_or(neocortex::serve::CheckpointRead{});
  if (!read_one->has_value() ||
      !blob_equals(checkpoint_one.blob, "deep-blob-one") ||
      checkpoint_one.lsn != first->lsn) {
    return false;
  }
  auto second = (*engine)->WriteCheckpoint(connection, sequence, turn_bytes,
                                           blob_bytes("deep-blob-two"));
  if (!second) {
    return false;
  }
  ++sequence;
  if (!append_filler(2)) {
    return false;
  }
  auto read_two = (*engine)->LatestCheckpoint(turn_bytes);
  if (!read_two) {
    return false;
  }
  const auto checkpoint_two =
      read_two->value_or(neocortex::serve::CheckpointRead{});
  if (!read_two->has_value() ||
      !blob_equals(checkpoint_two.blob, "deep-blob-two") ||
      checkpoint_two.lsn != second->lsn) {
    return false;
  }
  std::error_code cleanup_error;
  std::filesystem::remove_all(directory, cleanup_error);
  return !cleanup_error;
}

bool TestBeliefAdmission(const std::filesystem::path& binary) {
  const auto directory = TemporaryDirectory();
  if (directory.empty()) {
    return false;
  }
  const auto socket = directory / "cortexd.sock";
  const auto data = directory / "data";
  const auto config = directory / "cortexd.conf";
  if (!WriteConfig({.path = config, .socket = socket, .data = data})) {
    return false;
  }
  pid_t daemon = StartDaemon(binary, config, directory, socket);
  if (daemon < 0) {
    return false;
  }
  const auto actor_token = FilledToken(std::byte{0xbb});
  const auto admin_token = FilledToken(std::byte{0xaa});
  const auto actor_id = ConnectionId(30);
  auto actor = Client::Connect(socket);
  auto admin = Client::Connect(socket);
  if (!actor || !admin ||
      !Handshake(*actor, actor_id, actor_token, 11, false) ||
      !Handshake(*admin, ConnectionId(31), admin_token, 0, true)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto exchange = [](Client& client, const std::vector<std::byte>& wire) {
    return client.Send(wire) ? client.Receive() : std::nullopt;
  };
  const auto log_events = [&](Client& client,
                              std::uint64_t request_id)
      -> std::optional<std::uint64_t> {
    auto payload = exchange(
        client, AdminWire(request_id, RequestPayload::Stats, 11));
    const auto* response = payload ? Response(*payload) : nullptr;
    const auto* stats =
        response == nullptr ? nullptr : response->payload_as_StatsResult();
    return stats == nullptr ? std::nullopt
                            : std::optional(stats->log_events());
  };
  const auto append_error = [&](std::uint64_t request_id,
                                std::uint64_t client_seq,
                                std::span<const SocketAppendEvent> events,
                                neocortex::ErrorCode code) {
    auto payload = exchange(
        *actor, AppendEventsWire({request_id, client_seq}, events));
    const auto* response = payload ? Response(*payload) : nullptr;
    const auto* detail =
        response == nullptr ? nullptr : response->payload_as_ErrorDetail();
    return response != nullptr &&
           response->status() == neocortex::protocol::ResponseStatus::error &&
           detail != nullptr && detail->code() == static_cast<std::uint8_t>(code);
  };
  const auto append_range = [&](std::uint64_t request_id,
                                std::uint64_t client_seq,
                                std::span<const SocketAppendEvent> events)
      -> std::optional<std::array<std::uint64_t, 2>> {
    auto payload = exchange(
        *actor, AppendEventsWire({request_id, client_seq}, events));
    const auto* response = payload ? Response(*payload) : nullptr;
    const auto* ack =
        response == nullptr ? nullptr : response->payload_as_AppendAck();
    return ack == nullptr
               ? std::nullopt
               : std::optional(std::array{ack->first_lsn(), ack->last_lsn()});
  };
  const auto idempotency = [&](std::uint64_t client_seq,
                               std::uint32_t event_index,
                               std::uint32_t event_count) {
    return neocortex::test::EventIdempotency{
        .connection_id = actor_id,
        .client_seq = client_seq,
        .event_index = event_index,
        .event_count = event_count};
  };

  auto initial_events = log_events(*admin, 1);
  const std::array uncorroborated = {SocketAppendEvent{
      .kind = neocortex::log::EventKind::kAssertion,
      .payload = BuildAssertionEvent(
          {.belief_id = 0x10,
           .identity = "admission:uncorroborated",
           .value = "nonexistent",
           .provenance_first = 1,
           .provenance_last = 1,
           .claim = neocortex::schema::AssertionClaim::negative_existence},
          idempotency(1, 0, 1))}};
  if (!initial_events || *initial_events != 0 ||
      !append_error(2, 1, uncorroborated,
                    neocortex::ErrorCode::kNegativeExistenceUncorroborated) ||
      log_events(*admin, 3) != initial_events) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const std::array seed = {SocketAppendEvent{
      .kind = neocortex::log::EventKind::kAssertion,
      .payload = BuildAssertionEvent(
          {.belief_id = 0x11,
           .identity = "admission:seed",
           .value = "present",
           .provenance_first = 1,
           .provenance_last = 1,
           .claim = neocortex::schema::AssertionClaim::affirmative},
          idempotency(1, 0, 1))}};
  auto seed_range = append_range(4, 1, seed);
  if (!seed_range || *seed_range != std::array<std::uint64_t, 2>{1, 1}) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const std::array unknown_retract = {SocketAppendEvent{
      .kind = neocortex::log::EventKind::kRetract,
      .payload = BuildRetractEvent(
          {.belief_id = 0x99, .provenance_lsn = 2},
          idempotency(2, 0, 1))}};
  if (!append_error(5, 2, unknown_retract,
                    neocortex::ErrorCode::kBeliefNotFound) ||
      log_events(*admin, 6) != std::optional<std::uint64_t>(1)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const std::array conflicting_id = {SocketAppendEvent{
      .kind = neocortex::log::EventKind::kAssertion,
      .payload = BuildAssertionEvent(
          {.belief_id = 0x11,
           .identity = "admission:conflicting-id",
           .value = "other",
           .provenance_first = 2,
           .provenance_last = 2,
           .claim = neocortex::schema::AssertionClaim::affirmative},
          idempotency(2, 0, 1))}};
  if (!append_error(7, 2, conflicting_id,
                    neocortex::ErrorCode::kBeliefIdConflict) ||
      log_events(*admin, 8) != std::optional<std::uint64_t>(1)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  constexpr std::uint8_t kCallId = 0x41;
  std::array corroborated = {
      SocketAppendEvent{
          .kind = neocortex::log::EventKind::kToolCall,
          .payload = BuildToolCallEvent(kCallId, idempotency(2, 0, 3))},
      SocketAppendEvent{
          .kind = neocortex::log::EventKind::kToolResult,
          .payload = BuildToolResultEvent(
              {.call_id = kCallId, .tool_call_lsn = 2},
              idempotency(2, 1, 3))},
      SocketAppendEvent{
          .kind = neocortex::log::EventKind::kAssertion,
          .payload = BuildAssertionEvent(
              {.belief_id = 0x12,
               .identity = "admission:negative",
               .value = "nonexistent",
               .provenance_first = 3,
               .provenance_last = 3,
               .claim =
                   neocortex::schema::AssertionClaim::negative_existence},
              idempotency(2, 2, 3))}};
  auto corroborated_range = append_range(9, 2, corroborated);
  if (!corroborated_range ||
      *corroborated_range != std::array<std::uint64_t, 2>{2, 4} ||
      log_events(*admin, 10) != std::optional<std::uint64_t>(4)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  if (!StopDaemon(daemon, SIGKILL)) {
    return false;
  }
  daemon = StartDaemon(binary, config, directory, socket);
  auto reader = daemon < 0 ? std::optional<Client>{} : Client::Connect(socket);
  auto post_admin = daemon < 0 ? std::optional<Client>{} : Client::Connect(socket);
  if (daemon < 0 || !reader || !post_admin ||
      !Handshake(*reader, ConnectionId(32), actor_token, 11, false) ||
      !Handshake(*post_admin, ConnectionId(33), admin_token, 0, true) ||
      log_events(*post_admin, 11) != std::optional<std::uint64_t>(4)) {
    if (daemon >= 0) {
      StopDaemon(daemon, SIGKILL);
    }
    return false;
  }
  auto belief_payload = exchange(
      *reader, AsOfWire(12, "admission:negative", 1, 0));
  const auto* belief_response =
      belief_payload ? Response(*belief_payload) : nullptr;
  const auto* belief = belief_response == nullptr
                           ? nullptr
                           : belief_response->payload_as_BeliefResult();
  if (belief == nullptr || !belief->present() || belief->transaction_lsn() != 4 ||
      belief->claim() != static_cast<std::uint8_t>(
                             neocortex::schema::AssertionClaim::negative_existence) ||
      !StopDaemon(daemon, SIGTERM)) {
    if (belief == nullptr || daemon >= 0) {
      StopDaemon(daemon, SIGKILL);
    }
    return false;
  }
  std::error_code cleanup_error;
  std::filesystem::remove_all(directory, cleanup_error);
  return !cleanup_error;
}

bool TestDaemon(const std::filesystem::path& binary) {
  const auto directory = TemporaryDirectory();
  if (directory.empty()) {
    return false;
  }
  const auto socket = directory / "cortexd.sock";
  const auto data = directory / "data";
  const auto config = directory / "cortexd.conf";
  if (!WriteConfig({.path = config, .socket = socket, .data = data})) {
    return false;
  }
  pid_t daemon = StartDaemon(binary, config, directory, socket);
  if (daemon < 0) {
    return false;
  }
  const auto actor_one_token = FilledToken(std::byte{0xbb});
  const auto actor_two_token = FilledToken(std::byte{0xcc});
  const auto admin_token = FilledToken(std::byte{0xaa});
  const auto primary_id = ConnectionId(1);
  const auto subscription_id = ConnectionId(2);

  auto primary = Client::Connect(socket);
  auto subscriber = Client::Connect(socket);
  auto admin = Client::Connect(socket);
  if (!primary || !subscriber || !admin ||
      !Handshake(*primary, primary_id, actor_one_token, 11, false) ||
      !Handshake(*subscriber, subscription_id, actor_one_token, 11, false) ||
      !Handshake(*admin, ConnectionId(3), admin_token, 0, true)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  {
    auto bad_version = Client::Connect(socket);
    if (!bad_version) {
      StopDaemon(daemon, SIGKILL);
      return false;
    }
    flatbuffers::FlatBufferBuilder builder(256);
    const auto rejected_id = ConnectionId(14);
    const auto id = builder.CreateVector(
        reinterpret_cast<const std::uint8_t*>(rejected_id.data()),
        rejected_id.size());
    const auto capability = builder.CreateVector(
        reinterpret_cast<const std::uint8_t*>(actor_one_token.data()),
        actor_one_token.size());
    const auto hello =
        neocortex::protocol::CreateHello(builder, 99, id, capability);
    const auto envelope = neocortex::protocol::CreateWireEnvelope(
        builder, 99, WirePayload::Hello, hello.Union());
    neocortex::protocol::FinishWireEnvelopeBuffer(builder, envelope);
    const std::vector<std::byte> wire{
        reinterpret_cast<const std::byte*>(builder.GetBufferPointer()),
        reinterpret_cast<const std::byte*>(builder.GetBufferPointer()) +
            builder.GetSize()};
    auto payload =
        bad_version->Send(wire) ? bad_version->Receive() : std::nullopt;
    const auto* response = payload ? Response(*payload) : nullptr;
    if (response == nullptr ||
        response->status() != neocortex::protocol::ResponseStatus::error ||
        response->payload_as_ErrorDetail() == nullptr ||
        response->payload_as_ErrorDetail()->code() !=
            static_cast<std::uint8_t>(neocortex::ErrorCode::kProtocolVersion)) {
      StopDaemon(daemon, SIGKILL);
      return false;
    }
  }
  {
    auto wrong_token = Client::Connect(socket);
    if (!wrong_token) {
      StopDaemon(daemon, SIGKILL);
      return false;
    }
    flatbuffers::FlatBufferBuilder builder(256);
    const auto rejected_id = ConnectionId(15);
    const auto invalid_token = FilledToken(std::byte{0x99});
    const auto id = builder.CreateVector(
        reinterpret_cast<const std::uint8_t*>(rejected_id.data()),
        rejected_id.size());
    const auto capability = builder.CreateVector(
        reinterpret_cast<const std::uint8_t*>(invalid_token.data()),
        invalid_token.size());
    const auto hello = neocortex::protocol::CreateHello(
        builder, neocortex::serve::kProtocolVersion, id, capability);
    const auto wire = FinishWire(builder, WirePayload::Hello, hello.Union());
    auto payload =
        wrong_token->Send(wire) ? wrong_token->Receive() : std::nullopt;
    const auto* response = payload ? Response(*payload) : nullptr;
    if (response == nullptr ||
        response->status() != neocortex::protocol::ResponseStatus::error ||
        response->payload_as_ErrorDetail() == nullptr ||
        response->payload_as_ErrorDetail()->code() !=
            static_cast<std::uint8_t>(
                neocortex::ErrorCode::kCapabilityDenied)) {
      StopDaemon(daemon, SIGKILL);
      return false;
    }
  }
  const auto subscribe = SubscribeWire({.request_id = 1, .since_lsn = 0});
  auto subscribed = subscriber->Send(subscribe) ? subscriber->Receive()
                                                 : std::nullopt;
  const auto* subscribe_response = subscribed ? Response(*subscribed) : nullptr;
  if (subscribe_response == nullptr ||
      subscribe_response->payload_as_SubscriptionAck() == nullptr ||
      !AppendAndExpect(*primary, {2, 1}, primary_id, "first", false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  auto first_event = subscriber->Receive();
  auto first_verified = first_event
                            ? neocortex::serve::VerifyProtocolPayload(*first_event)
                            : std::expected<const neocortex::protocol::WireEnvelope*,
                                            neocortex::Error>(
                                  std::unexpected(neocortex::Error{
                                      neocortex::ErrorCode::kReadFailed, 0}));
  if (!first_verified ||
      (*first_verified)->payload_type() != WirePayload::Event) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  auto retry = Client::Connect(socket);
  if (!retry || !Handshake(*retry, primary_id, actor_one_token, 11, false) ||
      !AppendAndExpect(*retry, {3, 1}, primary_id, "first", true)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto second =
      AppendWire({4, 2}, primary_id, "second-unacknowledged");
  if (!retry->Send(second)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  auto second_event = subscriber->Receive();
  auto second_verified = second_event
                             ? neocortex::serve::VerifyProtocolPayload(*second_event)
                             : std::expected<const neocortex::protocol::WireEnvelope*,
                                             neocortex::Error>(
                                   std::unexpected(neocortex::Error{
                                       neocortex::ErrorCode::kReadFailed, 0}));
  if (!second_verified ||
      (*second_verified)->payload_type() != WirePayload::Event ||
      !StopDaemon(daemon, SIGKILL)) {
    return false;
  }

  daemon = StartDaemon(binary, config, directory, socket);
  auto recovered = daemon < 0 ? std::optional<Client>{} : Client::Connect(socket);
  if (daemon < 0 || !recovered ||
      !Handshake(*recovered, primary_id, actor_one_token, 11, false) ||
      !AppendAndExpect(*recovered, {5, 2}, primary_id,
                       "second-unacknowledged", true)) {
    if (daemon >= 0) StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto forbidden = AdminWire(6, RequestPayload::Stats, 12);
  auto forbidden_response = recovered->Send(forbidden) ? recovered->Receive()
                                                        : std::nullopt;
  const auto* denied = forbidden_response ? Response(*forbidden_response) : nullptr;
  if (denied == nullptr || denied->status() != neocortex::protocol::ResponseStatus::error ||
      denied->payload_as_ErrorDetail() == nullptr ||
      denied->payload_as_ErrorDetail()->code() !=
          static_cast<std::uint8_t>(neocortex::ErrorCode::kCapabilityDenied)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  auto actor_two = Client::Connect(socket);
  const auto actor_two_id = ConnectionId(4);
  if (!actor_two ||
      !Handshake(*actor_two, actor_two_id, actor_two_token, 12, false) ||
      !AppendAndExpect(*actor_two, {7, 1}, actor_two_id, "isolated", false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  constexpr std::size_t kThreads = 4;
  constexpr std::size_t kAppends = 16;
  auto soak_subscriber = Client::Connect(socket);
  if (!soak_subscriber ||
      !Handshake(*soak_subscriber, ConnectionId(16), actor_one_token, 11,
                 false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto soak_subscribe = SubscribeWire({.request_id = 16, .since_lsn = 2});
  auto soak_subscribed = soak_subscriber->Send(soak_subscribe)
                             ? soak_subscriber->Receive()
                             : std::nullopt;
  const auto* soak_subscribe_response =
      soak_subscribed ? Response(*soak_subscribed) : nullptr;
  if (soak_subscribe_response == nullptr ||
      soak_subscribe_response->payload_as_SubscriptionAck() == nullptr) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  std::atomic<bool> soak_ok = true;
  std::array<std::uint64_t, kThreads> soak_checkpoint_lsns{};
  std::array<std::thread, kThreads> threads;
  for (std::size_t thread = 0; thread < threads.size(); ++thread) {
    threads[thread] = std::thread([&, thread] {
      const auto id = ConnectionId(static_cast<std::uint8_t>(20U + thread));
      auto client = Client::Connect(socket);
      if (!client || !Handshake(*client, id, actor_one_token, 11, false)) {
        soak_ok.store(false);
        return;
      }
      for (std::uint64_t sequence = 1; sequence <= kAppends; ++sequence) {
        const auto content = std::to_string(thread) + ":" +
                             std::to_string(sequence);
        if (!AppendAndExpect(*client, {sequence, sequence}, id, content, false)) {
          soak_ok.store(false);
          return;
        }
      }
      const auto turn = "soak-turn-" + std::to_string(thread);
      const auto blob = "soak-blob-" + std::to_string(thread);
      const auto request = [&](const std::vector<std::byte>& wire)
          -> std::optional<std::vector<std::byte>> {
        return client->Send(wire) ? client->Receive() : std::nullopt;
      };
      auto checkpoint_payload =
          request(CheckpointWire({100, kAppends + 1U}, turn, blob));
      const auto* checkpoint_response =
          checkpoint_payload ? Response(*checkpoint_payload) : nullptr;
      if (checkpoint_response == nullptr ||
          checkpoint_response->payload_as_CheckpointAck() == nullptr) {
        soak_ok.store(false);
        return;
      }
      soak_checkpoint_lsns[thread] =
          checkpoint_response->payload_as_CheckpointAck()->lsn();
      auto latest_payload = request(LatestCheckpointWire(101, turn));
      const auto* latest_response =
          latest_payload ? Response(*latest_payload) : nullptr;
      const auto* latest_result =
          latest_response == nullptr
              ? nullptr
              : latest_response->payload_as_CheckpointResult();
      if (latest_result == nullptr || !latest_result->present() ||
          latest_result->blob()->size() != blob.size() ||
          !std::equal(blob.begin(), blob.end(),
                      latest_result->blob()->begin())) {
        soak_ok.store(false);
        return;
      }
      auto activate_payload = request(ActivateWire(
          {.request_id = 102,
           .conversation = static_cast<std::uint8_t>(20U + thread)},
          "canonical"));
      const auto* activate_response =
          activate_payload ? Response(*activate_payload) : nullptr;
      if (activate_response == nullptr ||
          activate_response->payload_as_BytesResult() == nullptr) {
        soak_ok.store(false);
        return;
      }
    });
  }
  for (auto& thread : threads) {
    thread.join();
  }
  if (!soak_ok.load()) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  std::uint64_t drained_lsn = 2;
  std::size_t drained_user_events = 0;
  std::size_t drained_checkpoint_events = 0;
  for (std::size_t received = 0; received < kThreads * kAppends + kThreads;
       ++received) {
    auto event_payload = soak_subscriber->Receive();
    auto event_verified =
        event_payload
            ? neocortex::serve::VerifyProtocolPayload(*event_payload)
            : std::expected<const neocortex::protocol::WireEnvelope*,
                            neocortex::Error>(
                  std::unexpected(neocortex::Error{
                      neocortex::ErrorCode::kReadFailed, 0}));
    if (!event_verified ||
        (*event_verified)->payload_type() != WirePayload::Event) {
      StopDaemon(daemon, SIGKILL);
      return false;
    }
    const auto* event = (*event_verified)->payload_as_Event();
    if (event->lsn() != drained_lsn + 1U) {
      StopDaemon(daemon, SIGKILL);
      return false;
    }
    drained_lsn = event->lsn();
    if (event->kind() ==
        static_cast<std::uint8_t>(neocortex::log::EventKind::kUserMsg)) {
      ++drained_user_events;
    } else if (event->kind() ==
               static_cast<std::uint8_t>(
                   neocortex::log::EventKind::kCheckpoint)) {
      ++drained_checkpoint_events;
    } else {
      StopDaemon(daemon, SIGKILL);
      return false;
    }
  }
  if (drained_user_events != kThreads * kAppends ||
      drained_checkpoint_events != kThreads) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  admin = Client::Connect(socket);
  if (!admin || !Handshake(*admin, ConnectionId(5), admin_token, 0, true)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  for (const auto& request : {
           AdminWire(8, RequestPayload::Health, 0),
           AdminWire(9, RequestPayload::Stats, 11),
           AdminWire(10, RequestPayload::Stats, 12),
           AdminWire(11, RequestPayload::VerifyStatus, 11),
           AdminWire(12, RequestPayload::LatencyHistograms, 0),
           AdminWire(13, RequestPayload::RebuildProjection, 11, "all")}) {
    auto payload = admin->Send(request) ? admin->Receive() : std::nullopt;
    const auto* response = payload ? Response(*payload) : nullptr;
    if (response == nullptr ||
        response->status() != neocortex::protocol::ResponseStatus::ok) {
      StopDaemon(daemon, SIGKILL);
      return false;
    }
  }

  auto query_client = Client::Connect(socket);
  const auto query_id = ConnectionId(6);
  if (!query_client ||
      !Handshake(*query_client, query_id, actor_one_token, 11, false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  std::deque<std::vector<std::byte>> response_holders;
  const auto exchange = [&](Client& client, const std::vector<std::byte>& wire)
      -> const neocortex::protocol::Response* {
    auto payload = client.Send(wire) ? client.Receive() : std::nullopt;
    if (!payload) {
      return nullptr;
    }
    response_holders.push_back(std::move(*payload));
    return Response(response_holders.back());
  };

  const auto probe = AppendEventWire({.request_id = 20, .client_seq = 1},
                                     query_id,
                                     neocortex::log::EventKind::kUserMsg, 500,
                                     "probe", 1);
  const auto* probe_response = exchange(*query_client, probe);
  if (probe_response == nullptr ||
      probe_response->payload_as_AppendAck() == nullptr) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto probe_lsn = probe_response->payload_as_AppendAck()->first_lsn();

  const auto call_fixture = probe_lsn + 2U;
  const auto* call_response = exchange(
      *query_client,
      AppendEventWire({.request_id = 21, .client_seq = 2}, query_id,
                      neocortex::log::EventKind::kToolCall, call_fixture,
                      "tool-args", 1));
  if (call_response == nullptr ||
      call_response->payload_as_AppendAck() == nullptr ||
      call_response->payload_as_AppendAck()->first_lsn() != probe_lsn + 1U) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* result_response = exchange(
      *query_client,
      AppendEventWire({.request_id = 22, .client_seq = 3}, query_id,
                      neocortex::log::EventKind::kToolResult, call_fixture,
                      "tool-output", 1));
  const auto* assertion_response = exchange(
      *query_client,
      AppendEventWire({.request_id = 23, .client_seq = 4}, query_id,
                      neocortex::log::EventKind::kAssertion, probe_lsn + 3U,
                      "belief-value", 1));
  if (result_response == nullptr ||
      result_response->status() != neocortex::protocol::ResponseStatus::ok ||
      assertion_response == nullptr ||
      assertion_response->status() != neocortex::protocol::ResponseStatus::ok) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* assertion_ack = assertion_response->payload_as_AppendAck();
  const auto* verify_response =
      exchange(*admin, AdminWire(50, RequestPayload::VerifyStatus, 11));
  const auto* verify_result = verify_response == nullptr
                                  ? nullptr
                                  : verify_response->payload_as_VerifyResult();
  if (assertion_ack == nullptr || verify_result == nullptr ||
      verify_result->leaf_count() != assertion_ack->leaf_count() ||
      verify_result->root()->size() != assertion_ack->mmr_root()->size() ||
      !std::equal(verify_result->root()->begin(), verify_result->root()->end(),
                  assertion_ack->mmr_root()->begin())) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto* transcript_response =
      exchange(*query_client,
               TranscriptWire({.request_id = 24,
                               .conversation = 1,
                               .since_lsn = probe_lsn,
                               .limit = 16}));
  const auto* transcript = transcript_response == nullptr
                               ? nullptr
                               : transcript_response->payload_as_TranscriptResult();
  if (transcript == nullptr || transcript->truncated() ||
      transcript->records()->size() != 3 ||
      transcript->records()->Get(0)->lsn() != probe_lsn + 1U ||
      transcript->records()->Get(0)->kind() !=
          static_cast<std::uint8_t>(neocortex::log::EventKind::kToolCall) ||
      transcript->records()->Get(1)->kind() !=
          static_cast<std::uint8_t>(neocortex::log::EventKind::kToolResult) ||
      transcript->records()->Get(2)->kind() !=
          static_cast<std::uint8_t>(neocortex::log::EventKind::kAssertion)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto activate_wire = ActivateWire({.request_id = 25, .conversation = 1}, "canonical");
  const auto* first_activate = exchange(*query_client, activate_wire);
  const auto* first_bytes = first_activate == nullptr
                                ? nullptr
                                : first_activate->payload_as_BytesResult();
  std::vector<std::uint8_t> first_bundle;
  if (first_bytes == nullptr || first_bytes->bytes()->size() < 12U ||
      first_bytes->bytes()->Get(0) != 'N' || first_bytes->bytes()->Get(1) != 'C' ||
      first_bytes->bytes()->Get(2) != 'A' || first_bytes->bytes()->Get(3) != '1') {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  first_bundle.assign(first_bytes->bytes()->begin(),
                      first_bytes->bytes()->end());
  const auto* second_activate = exchange(*query_client, activate_wire);
  const auto* second_bytes = second_activate == nullptr
                                 ? nullptr
                                 : second_activate->payload_as_BytesResult();
  if (second_bytes == nullptr ||
      !std::equal(first_bundle.begin(), first_bundle.end(),
                  second_bytes->bytes()->begin(), second_bytes->bytes()->end())) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto* asof_response =
      exchange(*query_client, AsOfWire(26, "canonical", 1, 0));
  const auto* belief = asof_response == nullptr
                           ? nullptr
                           : asof_response->payload_as_BeliefResult();
  if (belief == nullptr || !belief->present() || belief->version() == 0 ||
      belief->provenance() == nullptr || belief->provenance()->size() == 0) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* missing_response =
      exchange(*query_client, AsOfWire(27, "missing-identity", 1, 0));
  const auto* missing = missing_response == nullptr
                            ? nullptr
                            : missing_response->payload_as_BeliefResult();
  if (missing == nullptr || missing->present()) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  auto asof_client = Client::Connect(socket);
  const auto asof_id = ConnectionId(10);
  if (!asof_client ||
      !Handshake(*asof_client, asof_id, actor_one_token, 11, false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const std::uint64_t first_assertion_lsn = assertion_ack->first_lsn();
  const auto* second_assertion_response = exchange(
      *asof_client,
      AppendEventWire({.request_id = 95, .client_seq = 1}, asof_id,
                      neocortex::log::EventKind::kAssertion, probe_lsn + 40U,
                      "belief-value-two", 1));
  if (second_assertion_response == nullptr ||
      second_assertion_response->payload_as_AppendAck() == nullptr) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto value_matches = [](const neocortex::protocol::BeliefResult* record,
                                std::string_view expected) {
    return record != nullptr && record->present() &&
           record->value() != nullptr &&
           record->value()->size() == expected.size() &&
           std::equal(expected.begin(), expected.end(),
                      record->value()->begin());
  };
  const auto* latest_version_response =
      exchange(*query_client, AsOfWire(96, "canonical", 1, 0));
  const auto* latest_version =
      latest_version_response == nullptr
          ? nullptr
          : latest_version_response->payload_as_BeliefResult();
  if (!value_matches(latest_version, "belief-value-two") ||
      latest_version->version() != belief->version() + 1U) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* first_version_response = exchange(
      *query_client, AsOfWire(97, "canonical", 1, first_assertion_lsn));
  const auto* first_version =
      first_version_response == nullptr
          ? nullptr
          : first_version_response->payload_as_BeliefResult();
  if (!value_matches(first_version, "belief-value") ||
      first_version->version() != belief->version() ||
      first_version->transaction_lsn() != first_assertion_lsn) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto* limited_transcript_response =
      exchange(*query_client,
               TranscriptWire({.request_id = 98,
                               .conversation = 1,
                               .since_lsn = probe_lsn,
                               .limit = 2}));
  const auto* limited_transcript =
      limited_transcript_response == nullptr
          ? nullptr
          : limited_transcript_response->payload_as_TranscriptResult();
  if (limited_transcript == nullptr || !limited_transcript->truncated() ||
      limited_transcript->records()->size() != 2 ||
      limited_transcript->records()->Get(0)->lsn() != probe_lsn + 1U) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* full_transcript_response =
      exchange(*query_client,
               TranscriptWire({.request_id = 99,
                               .conversation = 1,
                               .since_lsn = probe_lsn,
                               .limit = 16}));
  const auto* full_transcript =
      full_transcript_response == nullptr
          ? nullptr
          : full_transcript_response->payload_as_TranscriptResult();
  if (full_transcript == nullptr || full_transcript->truncated() ||
      full_transcript->records()->size() != 4) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto* list_response = exchange(
      *query_client,
      RecallWire(28, neocortex::protocol::RecallMode::list_windows, 0, 0,
                 4'000'000'000'000'000'000LL, 64));
  const auto* windows = list_response == nullptr
                            ? nullptr
                            : list_response->payload_as_RecallResult();
  if (windows == nullptr || windows->windows() == nullptr ||
      windows->windows()->size() == 0) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* minute = windows->windows()->Get(0);
  const auto* members_response = exchange(
      *query_client,
      RecallWire(29, neocortex::protocol::RecallMode::resolve_members,
                 minute->level(), minute->start_ns(), minute->end_ns(), 64));
  const auto* members = members_response == nullptr
                            ? nullptr
                            : members_response->payload_as_RecallResult();
  if (members == nullptr || members->members() == nullptr ||
      members->members()->size() == 0) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* hour_response = exchange(
      *query_client,
      RecallWire(91, neocortex::protocol::RecallMode::list_windows, 1, 0,
                 4'000'000'000'000'000'000LL, 64));
  const auto* hours = hour_response == nullptr
                          ? nullptr
                          : hour_response->payload_as_RecallResult();
  if (hours == nullptr || hours->windows() == nullptr ||
      hours->windows()->size() == 0 || hours->windows()->Get(0)->level() != 1) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* hour = hours->windows()->Get(0);
  const auto* opened_response = exchange(
      *query_client,
      RecallWire(92, neocortex::protocol::RecallMode::open_window,
                 hour->level(), hour->start_ns(), hour->end_ns(), 64));
  const auto* opened = opened_response == nullptr
                           ? nullptr
                           : opened_response->payload_as_RecallResult();
  if (opened == nullptr || opened->windows() == nullptr ||
      opened->windows()->size() == 0) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  for (flatbuffers::uoffset_t index = 0; index < opened->windows()->size();
       ++index) {
    const auto* child = opened->windows()->Get(index);
    if (child->level() != 0 || child->start_ns() < hour->start_ns() ||
        child->end_ns() > hour->end_ns()) {
      StopDaemon(daemon, SIGKILL);
      return false;
    }
  }
  const auto* limited_members_response = exchange(
      *query_client,
      RecallWire(93, neocortex::protocol::RecallMode::resolve_members,
                 minute->level(), minute->start_ns(), minute->end_ns(), 1));
  const auto* limited_members =
      limited_members_response == nullptr
          ? nullptr
          : limited_members_response->payload_as_RecallResult();
  if (limited_members == nullptr || limited_members->members() == nullptr ||
      limited_members->members()->size() != 1) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto* first_checkpoint =
      exchange(*query_client, CheckpointWire({30, 5}, "turn-a", "cp-one"));
  const auto* second_checkpoint =
      exchange(*query_client, CheckpointWire({31, 6}, "turn-a", "cp-two"));
  if (first_checkpoint == nullptr ||
      first_checkpoint->payload_as_CheckpointAck() == nullptr ||
      second_checkpoint == nullptr ||
      second_checkpoint->payload_as_CheckpointAck() == nullptr ||
      second_checkpoint->payload_as_CheckpointAck()->lsn() <=
          first_checkpoint->payload_as_CheckpointAck()->lsn()) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* latest_response =
      exchange(*query_client, LatestCheckpointWire(32, "turn-a"));
  const auto* latest = latest_response == nullptr
                           ? nullptr
                           : latest_response->payload_as_CheckpointResult();
  const std::string_view expected_blob = "cp-two";
  if (latest == nullptr || !latest->present() ||
      latest->blob() == nullptr ||
      latest->blob()->size() != expected_blob.size() ||
      !std::equal(expected_blob.begin(), expected_blob.end(),
                  latest->blob()->begin()) ||
      latest->lsn() != second_checkpoint->payload_as_CheckpointAck()->lsn()) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* absent_response =
      exchange(*query_client, LatestCheckpointWire(33, "turn-b"));
  const auto* absent = absent_response == nullptr
                           ? nullptr
                           : absent_response->payload_as_CheckpointResult();
  if (absent == nullptr || absent->present()) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* isolated_latest =
      exchange(*actor_two, LatestCheckpointWire(34, "turn-a"));
  const auto* isolated_checkpoint =
      isolated_latest == nullptr
          ? nullptr
          : isolated_latest->payload_as_CheckpointResult();
  if (isolated_checkpoint == nullptr || isolated_checkpoint->present()) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto* stats_before_duplicates =
      exchange(*admin, AdminWire(60, RequestPayload::Stats, 11));
  const auto* stats_before = stats_before_duplicates == nullptr
                                 ? nullptr
                                 : stats_before_duplicates->payload_as_StatsResult();
  const auto* duplicate_checkpoint =
      exchange(*query_client, CheckpointWire({61, 6}, "turn-a", "cp-two"));
  const auto* duplicate_checkpoint_ack =
      duplicate_checkpoint == nullptr
          ? nullptr
          : duplicate_checkpoint->payload_as_CheckpointAck();
  if (stats_before == nullptr || duplicate_checkpoint_ack == nullptr ||
      duplicate_checkpoint_ack->lsn() !=
          second_checkpoint->payload_as_CheckpointAck()->lsn()) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* conflicting_checkpoint =
      exchange(*query_client,
               CheckpointWire({62, 6}, "turn-a", "cp-conflict"));
  if (conflicting_checkpoint == nullptr ||
      conflicting_checkpoint->status() !=
          neocortex::protocol::ResponseStatus::error ||
      conflicting_checkpoint->payload_as_ErrorDetail() == nullptr ||
      conflicting_checkpoint->payload_as_ErrorDetail()->code() !=
          static_cast<std::uint8_t>(
              neocortex::ErrorCode::kIdempotencyConflict)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* wrong_sequence_checkpoint =
      exchange(*query_client, CheckpointWire({63, 99}, "turn-x", "cp-x"));
  if (wrong_sequence_checkpoint == nullptr ||
      wrong_sequence_checkpoint->status() !=
          neocortex::protocol::ResponseStatus::error ||
      wrong_sequence_checkpoint->payload_as_ErrorDetail() == nullptr ||
      wrong_sequence_checkpoint->payload_as_ErrorDetail()->code() !=
          static_cast<std::uint8_t>(neocortex::ErrorCode::kSequenceViolation)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* stats_after_duplicates =
      exchange(*admin, AdminWire(64, RequestPayload::Stats, 11));
  const auto* stats_after = stats_after_duplicates == nullptr
                                ? nullptr
                                : stats_after_duplicates->payload_as_StatsResult();
  if (stats_after == nullptr ||
      stats_after->log_events() != stats_before->log_events()) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  auto attest_subscriber = Client::Connect(socket);
  if (!attest_subscriber ||
      !Handshake(*attest_subscriber, ConnectionId(7), actor_one_token, 11,
                 false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto attest_since =
      second_checkpoint->payload_as_CheckpointAck()->lsn();
  const auto* attest_subscribe_response = exchange(
      *attest_subscriber,
      SubscribeWire({.request_id = 35, .since_lsn = attest_since}));
  if (attest_subscribe_response == nullptr ||
      attest_subscribe_response->payload_as_SubscriptionAck() == nullptr) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const std::array<std::uint64_t, 1> used = {probe_lsn + 1U};
  const std::array<std::uint64_t, 1> ignored = {probe_lsn + 2U};
  const auto* attest_response =
      exchange(*query_client, AttestWire({36, 7}, used, ignored));
  const auto* attest_ack = attest_response == nullptr
                               ? nullptr
                               : attest_response->payload_as_AttestAck();
  if (attest_ack == nullptr || attest_ack->count() != 2 ||
      attest_ack->last_lsn() != attest_ack->first_lsn() + 1U) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  for (std::size_t received = 0; received < 2; ++received) {
    auto event_payload = attest_subscriber->Receive();
    auto event_verified =
        event_payload
            ? neocortex::serve::VerifyProtocolPayload(*event_payload)
            : std::expected<const neocortex::protocol::WireEnvelope*,
                            neocortex::Error>(
                  std::unexpected(neocortex::Error{
                      neocortex::ErrorCode::kReadFailed, 0}));
    if (!event_verified ||
        (*event_verified)->payload_type() != WirePayload::Event ||
        (*event_verified)->payload_as_Event()->kind() !=
            static_cast<std::uint8_t>(
                neocortex::log::EventKind::kAttestation)) {
      StopDaemon(daemon, SIGKILL);
      return false;
    }
  }
  const std::array<std::uint64_t, 1> bad_target = {1'000'000'000ULL};
  const auto* bad_attest =
      exchange(*query_client, AttestWire({37, 8}, bad_target, {}));
  if (bad_attest == nullptr ||
      bad_attest->status() != neocortex::protocol::ResponseStatus::error) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto* attest_stats_before_response =
      exchange(*admin, AdminWire(65, RequestPayload::Stats, 11));
  const auto* attest_stats_before =
      attest_stats_before_response == nullptr
          ? nullptr
          : attest_stats_before_response->payload_as_StatsResult();
  const auto* duplicate_attest =
      exchange(*query_client, AttestWire({66, 7}, used, ignored));
  const auto* duplicate_attest_ack =
      duplicate_attest == nullptr ? nullptr
                                  : duplicate_attest->payload_as_AttestAck();
  const auto* attest_stats_after_response =
      exchange(*admin, AdminWire(67, RequestPayload::Stats, 11));
  const auto* attest_stats_after =
      attest_stats_after_response == nullptr
          ? nullptr
          : attest_stats_after_response->payload_as_StatsResult();
  if (attest_stats_before == nullptr || duplicate_attest_ack == nullptr ||
      duplicate_attest_ack->first_lsn() != attest_ack->first_lsn() ||
      duplicate_attest_ack->last_lsn() != attest_ack->last_lsn() ||
      duplicate_attest_ack->count() != 2 || attest_stats_after == nullptr ||
      attest_stats_after->log_events() != attest_stats_before->log_events()) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  if (attest_stats_after->log_events() != attest_ack->last_lsn() ||
      attest_stats_after->log_bytes() == 0) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  if (!AppendAndExpect(*recovered, {82, 2}, primary_id,
                       "second-unacknowledged", true)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* duplicate_append_stats_response =
      exchange(*admin, AdminWire(83, RequestPayload::Stats, 11));
  const auto* duplicate_append_stats =
      duplicate_append_stats_response == nullptr
          ? nullptr
          : duplicate_append_stats_response->payload_as_StatsResult();
  if (duplicate_append_stats == nullptr ||
      duplicate_append_stats->log_events() !=
          attest_stats_after->log_events()) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* histograms_response =
      exchange(*admin, AdminWire(84, RequestPayload::LatencyHistograms, 0));
  const auto* histograms =
      histograms_response == nullptr
          ? nullptr
          : histograms_response->payload_as_LatencyResult();
  if (histograms == nullptr || histograms->buckets() == nullptr) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  std::uint64_t append_latency_count = 0;
  for (flatbuffers::uoffset_t index = 0; index < histograms->buckets()->size();
       ++index) {
    const auto* bucket = histograms->buckets()->Get(index);
    if (bucket->operation() != nullptr &&
        bucket->operation()->string_view() == "Append") {
      append_latency_count += bucket->count();
    }
  }
  if (append_latency_count == 0) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  auto actor_two_subscriber = Client::Connect(socket);
  if (!actor_two_subscriber ||
      !Handshake(*actor_two_subscriber, ConnectionId(17), actor_two_token, 12,
                 false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* actor_two_subscribe_response = exchange(
      *actor_two_subscriber,
      SubscribeWire({.request_id = 85, .since_lsn = 0}));
  if (actor_two_subscribe_response == nullptr ||
      actor_two_subscribe_response->payload_as_SubscriptionAck() == nullptr) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto expect_event = [&](Client& client, std::uint64_t lsn,
                                neocortex::log::EventKind kind) {
    auto event_payload = client.Receive();
    auto event_verified =
        event_payload
            ? neocortex::serve::VerifyProtocolPayload(*event_payload)
            : std::expected<const neocortex::protocol::WireEnvelope*,
                            neocortex::Error>(
                  std::unexpected(neocortex::Error{
                      neocortex::ErrorCode::kReadFailed, 0}));
    if (!event_verified ||
        (*event_verified)->payload_type() != WirePayload::Event) {
      return false;
    }
    const auto* event = (*event_verified)->payload_as_Event();
    return event->lsn() == lsn &&
           event->kind() == static_cast<std::uint8_t>(kind);
  };
  if (!expect_event(*actor_two_subscriber, 1,
                    neocortex::log::EventKind::kUserMsg)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  auto replay_subscriber = Client::Connect(socket);
  if (!replay_subscriber ||
      !Handshake(*replay_subscriber, ConnectionId(18), actor_one_token, 11,
                 false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto replay_tail = attest_ack->last_lsn();
  const auto* replay_subscribe_response = exchange(
      *replay_subscriber,
      SubscribeWire({.request_id = 86, .since_lsn = replay_tail - 3U}));
  if (replay_subscribe_response == nullptr ||
      replay_subscribe_response->payload_as_SubscriptionAck() == nullptr ||
      !expect_event(*replay_subscriber, replay_tail - 2U,
                    neocortex::log::EventKind::kCheckpoint) ||
      !expect_event(*replay_subscriber, replay_tail - 1U,
                    neocortex::log::EventKind::kAttestation) ||
      !expect_event(*replay_subscriber, replay_tail,
                    neocortex::log::EventKind::kAttestation)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  auto live_client = Client::Connect(socket);
  const auto live_id = ConnectionId(13);
  if (!live_client ||
      !Handshake(*live_client, live_id, actor_one_token, 11, false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* live_response = exchange(
      *live_client,
      AppendEventWire({.request_id = 87, .client_seq = 1}, live_id,
                      neocortex::log::EventKind::kUserMsg, 900,
                      "live-replay", 2));
  const auto* live_ack = live_response == nullptr
                             ? nullptr
                             : live_response->payload_as_AppendAck();
  if (live_ack == nullptr || live_ack->first_lsn() != replay_tail + 1U ||
      !expect_event(*replay_subscriber, replay_tail + 1U,
                    neocortex::log::EventKind::kUserMsg)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  if (!AppendAndExpect(*actor_two, {88, 2}, actor_two_id, "isolated-2",
                       false) ||
      !expect_event(*actor_two_subscriber, 2,
                    neocortex::log::EventKind::kUserMsg)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto* invalid_recall = exchange(
      *query_client,
      RecallWire(38, static_cast<neocortex::protocol::RecallMode>(7), 0, 0, 1,
                 4));
  if (invalid_recall == nullptr ||
      invalid_recall->status() != neocortex::protocol::ResponseStatus::error) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  const auto* pre_restart_activate =
      exchange(*query_client, ActivateWire({.request_id = 41, .conversation = 1}, "canonical"));
  const auto* pre_restart_bytes =
      pre_restart_activate == nullptr
          ? nullptr
          : pre_restart_activate->payload_as_BytesResult();
  if (pre_restart_bytes == nullptr || pre_restart_bytes->bytes()->size() < 12U) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const std::vector<std::uint8_t> pre_restart_bundle(
      pre_restart_bytes->bytes()->begin(), pre_restart_bytes->bytes()->end());
  const std::uint64_t attest_first_lsn = attest_ack->first_lsn();
  const std::uint64_t attest_last_lsn = attest_ack->last_lsn();
  const auto* pre_restart_stats_response =
      exchange(*admin, AdminWire(89, RequestPayload::Stats, 11));
  const auto* pre_restart_stats =
      pre_restart_stats_response == nullptr
          ? nullptr
          : pre_restart_stats_response->payload_as_StatsResult();
  if (pre_restart_stats == nullptr ||
      pre_restart_stats->log_events() != replay_tail + 1U) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const std::uint64_t pre_restart_log_events = pre_restart_stats->log_events();

  if (!StopDaemon(daemon, SIGKILL)) {
    return false;
  }
  daemon = StartDaemon(binary, config, directory, socket);
  auto post_restart = daemon < 0 ? std::optional<Client>{} : Client::Connect(socket);
  if (daemon < 0 || !post_restart ||
      !Handshake(*post_restart, ConnectionId(8), actor_one_token, 11, false)) {
    if (daemon >= 0) StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* recovered_latest =
      exchange(*post_restart, LatestCheckpointWire(39, "turn-a"));
  const auto* recovered_checkpoint =
      recovered_latest == nullptr
          ? nullptr
          : recovered_latest->payload_as_CheckpointResult();
  if (recovered_checkpoint == nullptr || !recovered_checkpoint->present() ||
      recovered_checkpoint->blob()->size() != expected_blob.size() ||
      !std::equal(expected_blob.begin(), expected_blob.end(),
                  recovered_checkpoint->blob()->begin())) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* recovered_activate =
      exchange(*post_restart, ActivateWire({.request_id = 40, .conversation = 1}, "canonical"));
  const auto* recovered_bytes =
      recovered_activate == nullptr
          ? nullptr
          : recovered_activate->payload_as_BytesResult();
  if (recovered_bytes == nullptr ||
      !std::equal(pre_restart_bundle.begin(), pre_restart_bundle.end(),
                  recovered_bytes->bytes()->begin(),
                  recovered_bytes->bytes()->end())) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  auto resend_query = Client::Connect(socket);
  if (!resend_query ||
      !Handshake(*resend_query, query_id, actor_one_token, 11, false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* resent_attest =
      exchange(*resend_query, AttestWire({70, 7}, used, ignored));
  const auto* resent_attest_ack =
      resent_attest == nullptr ? nullptr
                               : resent_attest->payload_as_AttestAck();
  if (resent_attest_ack == nullptr ||
      resent_attest_ack->first_lsn() != attest_first_lsn ||
      resent_attest_ack->last_lsn() != attest_last_lsn ||
      resent_attest_ack->count() != 2) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  auto resend_soak = Client::Connect(socket);
  if (!resend_soak ||
      !Handshake(*resend_soak, ConnectionId(20), actor_one_token, 11, false)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* resent_checkpoint = exchange(
      *resend_soak,
      CheckpointWire({71, kAppends + 1U}, "soak-turn-0", "soak-blob-0"));
  const auto* resent_checkpoint_ack =
      resent_checkpoint == nullptr
          ? nullptr
          : resent_checkpoint->payload_as_CheckpointAck();
  if (resent_checkpoint_ack == nullptr ||
      resent_checkpoint_ack->lsn() != soak_checkpoint_lsns[0]) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  auto post_admin = Client::Connect(socket);
  if (!post_admin ||
      !Handshake(*post_admin, ConnectionId(9), admin_token, 0, true)) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }
  const auto* post_stats_response =
      exchange(*post_admin, AdminWire(72, RequestPayload::Stats, 11));
  const auto* post_stats = post_stats_response == nullptr
                               ? nullptr
                               : post_stats_response->payload_as_StatsResult();
  if (post_stats == nullptr ||
      post_stats->log_events() != pre_restart_log_events) {
    StopDaemon(daemon, SIGKILL);
    return false;
  }

  if (!StopDaemon(daemon, SIGTERM)) {
    return false;
  }
  std::error_code cleanup_error;
  std::filesystem::remove_all(directory, cleanup_error);
  return !cleanup_error;
}

}  // namespace

int main(int argc, char** argv) {
  if (argc != 2) {
    return Fail("cortexd test needs daemon path");
  }
  if (!TestTornBatchRollback()) {
    return Fail("torn batch rollback test failed");
  }
  if (!TestDurableSequenceAuthority()) {
    return Fail("durable sequence authority test failed");
  }
  if (!TestHandshakeSequenceFailureIsAtomic(std::filesystem::path(argv[1]))) {
    return Fail("handshake sequence failure atomicity test failed");
  }
  if (!TestLatestCheckpointDeepScan()) {
    return Fail("latest checkpoint deep scan test failed");
  }
  if (!TestBeliefAdmission(std::filesystem::path(argv[1]))) {
    return Fail("belief socket admission test failed");
  }
  return TestDaemon(std::filesystem::path(argv[1]))
             ? 0
             : Fail("real cortexd protocol test failed");
}
