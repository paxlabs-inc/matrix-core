#include "serve/server.h"

#include <algorithm>
#include <array>
#include <cerrno>
#include <chrono>
#include <cstring>
#include <limits>
#include <memory_resource>
#include <poll.h>
#include <signal.h>
#include <string_view>
#include <sys/signalfd.h>
#include <sys/socket.h>
#include <sys/stat.h>
#include <sys/un.h>
#include <unistd.h>

#include <flatbuffers/flatbuffers.h>

namespace neocortex::serve {
namespace {

constexpr std::array<std::uint64_t, 8> kLatencyBoundsNs = {
    10'000U, 50'000U, 100'000U, 500'000U,
    1'000'000U, 5'000'000U, 50'000'000U,
    std::numeric_limits<std::uint64_t>::max()};
constexpr std::size_t kRequestArenaBytes =
    static_cast<std::size_t>(128) * 1024U;
constexpr std::size_t kTranscriptRecordOverhead = 128;
constexpr std::size_t kMaximumTranscriptBytes =
    static_cast<std::size_t>(8) * 1024U * 1024U;
constexpr std::size_t kMaximumTranscriptScan =
    static_cast<std::size_t>(64) * 1024U;
constexpr std::size_t kMaximumResponseSlopBytes = 4096;

class ArenaAllocator final : public flatbuffers::Allocator {
 public:
  explicit ArenaAllocator(std::pmr::memory_resource& resource)
      : resource_(resource) {}

  std::uint8_t* allocate(std::size_t size) override {
    return static_cast<std::uint8_t*>(
        resource_.allocate(size, alignof(std::max_align_t)));
  }

  void deallocate(std::uint8_t*, std::size_t) override {}

 private:
  std::pmr::memory_resource& resource_;
};

class RequestArena final {
 public:
  RequestArena()
      : resource_(storage_.data(), storage_.size(),
                  std::pmr::null_memory_resource()),
        allocator_(resource_),
        builder_(4096, &allocator_) {}

  [[nodiscard]] flatbuffers::FlatBufferBuilder& builder() { return builder_; }

 private:
  std::array<std::byte, kRequestArenaBytes> storage_{};
  std::pmr::monotonic_buffer_resource resource_;
  ArenaAllocator allocator_;
  flatbuffers::FlatBufferBuilder builder_;
};

bool IsAdminRequest(protocol::RequestPayload type) {
  return type == protocol::RequestPayload::Health ||
         type == protocol::RequestPayload::Stats ||
         type == protocol::RequestPayload::LatencyHistograms ||
         type == protocol::RequestPayload::VerifyStatus ||
         type == protocol::RequestPayload::RebuildProjection ||
         type == protocol::RequestPayload::CryptoDelete;
}

std::span<const std::byte> Bytes(
    const flatbuffers::Vector<std::uint8_t>& value) {
  return std::as_bytes(std::span(value.data(), value.size()));
}

std::vector<std::byte> BuilderBytes(flatbuffers::FlatBufferBuilder& builder) {
  const auto bytes = std::span(builder.GetBufferPointer(), builder.GetSize());
  const auto encoded = std::as_bytes(bytes);
  return std::vector<std::byte>(encoded.begin(), encoded.end());
}

std::vector<std::byte> FinishWire(
    flatbuffers::FlatBufferBuilder& builder, protocol::WirePayload type,
    flatbuffers::Offset<void> payload) {
  const auto envelope =
      protocol::CreateWireEnvelope(builder, kProtocolVersion, type, payload);
  protocol::FinishWireEnvelopeBuffer(builder, envelope);
  return BuilderBytes(builder);
}

std::vector<std::byte> FinishResponse(
    flatbuffers::FlatBufferBuilder& builder, std::uint64_t request_id,
    protocol::ResponseStatus status, protocol::ResponsePayload type,
    flatbuffers::Offset<void> payload) {
  const auto response =
      protocol::CreateResponse(builder, request_id, status, type, payload);
  return FinishWire(builder, protocol::WirePayload::Response, response.Union());
}

std::optional<log::ConversationId> ConversationFilter(
    const flatbuffers::Vector<std::uint8_t>* value) {
  if (value == nullptr || value->empty()) {
    return std::nullopt;
  }
  log::ConversationId conversation{};
  std::transform(value->begin(), value->end(), conversation.bytes.begin(),
                 [](std::uint8_t byte) { return static_cast<std::byte>(byte); });
  return conversation;
}

}  // namespace

Server::Server(ServerConfig config) : config_(std::move(config)) {}

std::expected<std::unique_ptr<Server>, Error> Server::Open(ServerConfig config) {
  auto server = std::unique_ptr<Server>(new Server(std::move(config)));
  auto initialized = server->Initialize();
  if (!initialized) {
    return std::unexpected(initialized.error());
  }
  return server;
}

Server::~Server() {
  for (auto& connection : connections_) {
    if (connection.descriptor >= 0) {
      ::close(connection.descriptor);
    }
  }
  if (signal_descriptor_ >= 0) {
    ::close(signal_descriptor_);
  }
  if (listen_descriptor_ >= 0) {
    ::close(listen_descriptor_);
  }
  struct stat metadata {};
  if (::lstat(config_.socket_path.c_str(), &metadata) == 0 &&
      S_ISSOCK(metadata.st_mode)) {
    static_cast<void>(::unlink(config_.socket_path.c_str()));
  }
}

std::expected<void, Error> Server::Initialize() {
  sockaddr_un address{};
  if (config_.socket_path.string().size() >= sizeof(address.sun_path)) {
    return std::unexpected(Error{ErrorCode::kInvalidLength, 0});
  }
  std::error_code directory_error;
  std::filesystem::create_directories(config_.data_directory, directory_error);
  if (directory_error) {
    return std::unexpected(
        Error{ErrorCode::kOpenFailed, directory_error.value()});
  }
  for (const auto& actor : config_.actors) {
    auto engine = ActorEngine::Open(ActorEngineConfig{
        .actor_directory = config_.data_directory / std::to_string(actor.actor),
        .actor = actor.actor,
        .user = config_.user,
        .kek = config_.kek,
        .signing_key = config_.signing_key,
        .projection_map_bytes = config_.projection_map_bytes});
    if (!engine) {
      return std::unexpected(engine.error());
    }
    actors_.emplace(actor.actor, std::move(*engine));
  }

  const auto socket_parent = config_.socket_path.parent_path();
  if (!socket_parent.empty()) {
    std::filesystem::create_directories(socket_parent, directory_error);
    if (directory_error) {
      return std::unexpected(
          Error{ErrorCode::kOpenFailed, directory_error.value()});
    }
  }
  struct stat existing {};
  if (::lstat(config_.socket_path.c_str(), &existing) == 0) {
    if (!S_ISSOCK(existing.st_mode) ||
        ::unlink(config_.socket_path.c_str()) != 0) {
      return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
    }
  } else if (errno != ENOENT) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }

  listen_descriptor_ =
      ::socket(AF_UNIX, SOCK_STREAM | SOCK_NONBLOCK | SOCK_CLOEXEC, 0);
  if (listen_descriptor_ < 0) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  address.sun_family = AF_UNIX;
  std::memcpy(address.sun_path, config_.socket_path.c_str(),
              config_.socket_path.string().size() + 1U);
  if (::bind(listen_descriptor_, reinterpret_cast<const sockaddr*>(&address),
             sizeof(address)) != 0 ||
      ::chmod(config_.socket_path.c_str(), 0600) != 0 ||
      ::listen(listen_descriptor_, 128) != 0) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }

  sigset_t mask;
  ::sigemptyset(&mask);
  ::sigaddset(&mask, SIGINT);
  ::sigaddset(&mask, SIGTERM);
  if (::pthread_sigmask(SIG_BLOCK, &mask, nullptr) != 0) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  signal_descriptor_ = ::signalfd(-1, &mask, SFD_NONBLOCK | SFD_CLOEXEC);
  if (signal_descriptor_ < 0) {
    return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
  }
  connections_.reserve(config_.maximum_connections);
  return {};
}

ActorEngine* Server::FindActor(std::uint16_t actor) {
  const auto found = actors_.find(actor);
  return found == actors_.end() ? nullptr : found->second.get();
}

std::expected<void, Error> Server::AcceptConnections() {
  while (connections_.size() < config_.maximum_connections) {
    const int descriptor =
        ::accept4(listen_descriptor_, nullptr, nullptr, SOCK_NONBLOCK | SOCK_CLOEXEC);
    if (descriptor < 0) {
      if (errno == EAGAIN || errno == EWOULDBLOCK) {
        return {};
      }
      if (errno == EINTR) {
        continue;
      }
      return std::unexpected(Error{ErrorCode::kOpenFailed, errno});
    }
    ucred credentials{};
    socklen_t credentials_length = sizeof(credentials);
    if (::getsockopt(descriptor, SOL_SOCKET, SO_PEERCRED, &credentials,
                     &credentials_length) != 0 ||
        credentials.uid != ::geteuid()) {
      ::close(descriptor);
      continue;
    }
    connections_.push_back(Connection{.descriptor = descriptor,
                                      .parser = ProtocolFrameParser(),
                                      .output = {},
                                      .output_bytes = 0,
                                      .handshaken = false,
                                      .admin = false,
                                      .actor = 0,
                                      .connection_id = {},
                                      .subscriptions = {},
                                      .next_subscription_id = 1});
    connections_.back().subscriptions.reserve(kMaximumSubscriptions);
  }
  return {};
}

std::expected<void, Error> Server::QueuePayload(
    Connection& connection, std::span<const std::byte> payload) {
  auto framed = EncodeProtocolFrame(payload);
  if (!framed) {
    return std::unexpected(framed.error());
  }
  if (connection.output.size() >= config_.maximum_output_frames ||
      framed->size() > config_.maximum_output_bytes -
                           std::min(connection.output_bytes,
                                    config_.maximum_output_bytes)) {
    return std::unexpected(Error{ErrorCode::kCapacityExceeded, 0});
  }
  connection.output_bytes += framed->size();
  connection.output.push_back(
      QueuedFrame{.bytes = std::move(*framed), .offset = 0});
  return {};
}

std::expected<void, Error> Server::QueueError(
    Connection& connection, std::uint64_t request_id, Error error) {
  RequestArena arena;
  auto& builder = arena.builder();
  const auto detail = protocol::CreateErrorDetail(
      builder, static_cast<std::uint8_t>(error.code), error.system_error,
      error.lsn, error.offset);
  const auto bytes = FinishResponse(builder, request_id,
                                    protocol::ResponseStatus::error,
                                    protocol::ResponsePayload::ErrorDetail,
                                    detail.Union());
  return QueuePayload(connection, bytes);
}

std::expected<void, Error> Server::HandleHello(
    Connection& connection, const protocol::Hello& hello) {
  if (hello.proto_version() != kProtocolVersion ||
      hello.connection_id() == nullptr || hello.connection_id()->size() != 16 ||
      hello.capability_token() == nullptr ||
      hello.capability_token()->size() != config_.admin_token.size()) {
    return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
  }
  const auto token = Bytes(*hello.capability_token());
  if (CapabilityTokenEqual(config_.admin_token, token)) {
    connection.admin = true;
  } else {
    const auto found = std::find_if(
        config_.actors.begin(), config_.actors.end(),
        [token](const ActorCapability& capability) {
          return CapabilityTokenEqual(capability.token, token);
        });
    if (found == config_.actors.end() || FindActor(found->actor) == nullptr) {
      return std::unexpected(Error{ErrorCode::kCapabilityDenied, 0});
    }
    connection.actor = found->actor;
  }
  std::transform(hello.connection_id()->begin(), hello.connection_id()->end(),
                 connection.connection_id.begin(), [](std::uint8_t byte) {
                   return static_cast<std::byte>(byte);
                 });
  std::uint64_t next_client_seq = 0;
  if (!connection.admin) {
    auto* actor = FindActor(connection.actor);
    if (actor == nullptr) {
      return std::unexpected(Error{ErrorCode::kOperationUnavailable, 0});
    }
    auto next = actor->NextClientSequence(connection.connection_id);
    if (!next) {
      return std::unexpected(next.error());
    }
    next_client_seq = *next;
  }
  RequestArena arena;
  auto& builder = arena.builder();
  const auto welcome = protocol::CreateWelcome(
      builder, kProtocolVersion, connection.actor, connection.admin,
      kMaximumProtocolPayloadBytes,
      static_cast<std::uint32_t>(kMaximumBatchEvents),
      static_cast<std::uint32_t>(kMaximumSubscriptions), next_client_seq);
  const auto bytes = FinishWire(builder, protocol::WirePayload::Welcome,
                                welcome.Union());
  auto queued = QueuePayload(connection, bytes);
  if (!queued) {
    return std::unexpected(queued.error());
  }
  connection.handshaken = true;
  return {};
}

std::expected<void, Error> Server::HandlePayload(
    Connection& connection, std::span<const std::byte> payload) {
  auto envelope = VerifyProtocolPayload(payload);
  if (!envelope) {
    return QueueError(connection, 0, envelope.error());
  }
  if (!connection.handshaken) {
    if ((*envelope)->payload_type() != protocol::WirePayload::Hello) {
      return std::unexpected(Error{ErrorCode::kCapabilityDenied, 0});
    }
    auto hello = HandleHello(connection, *(*envelope)->payload_as_Hello());
    return hello ? std::expected<void, Error>{}
                 : QueueError(connection, 0, hello.error());
  }
  if ((*envelope)->payload_type() != protocol::WirePayload::Request) {
    return std::unexpected(Error{ErrorCode::kProtocolInvalid, 0});
  }
  return HandleRequest(connection, *(*envelope)->payload_as_Request());
}

void Server::RecordLatency(protocol::RequestPayload operation,
                           std::uint64_t elapsed_ns) {
  const auto operation_index = static_cast<std::size_t>(operation);
  if (operation_index >= latency_counts_.size()) {
    return;
  }
  const auto bound = std::lower_bound(kLatencyBoundsNs.begin(),
                                      kLatencyBoundsNs.end(), elapsed_ns);
  const auto bucket = static_cast<std::size_t>(
      std::distance(kLatencyBoundsNs.begin(), bound));
  if (bucket < latency_counts_[operation_index].size()) {
    ++latency_counts_[operation_index][bucket];
  }
}

std::expected<void, Error> Server::HandleRequest(
    Connection& connection, const protocol::Request& request) {
  auto valid = ValidateRequest(request);
  if (!valid) {
    return QueueError(connection, request.request_id(), valid.error());
  }
  const bool admin_request = IsAdminRequest(request.payload_type());
  if (admin_request != connection.admin) {
    return QueueError(connection, request.request_id(),
                      Error{ErrorCode::kCapabilityDenied, 0});
  }
  const auto started = std::chrono::steady_clock::now();
  auto handled = connection.admin ? HandleAdminRequest(connection, request)
                                  : HandleActorRequest(connection, request);
  const auto elapsed = std::chrono::duration_cast<std::chrono::nanoseconds>(
                           std::chrono::steady_clock::now() - started)
                           .count();
  RecordLatency(request.payload_type(), static_cast<std::uint64_t>(elapsed));
  return handled;
}

std::expected<void, Error> Server::HandleActorRequest(
    Connection& connection, const protocol::Request& request) {
  auto* actor = FindActor(connection.actor);
  if (actor == nullptr) {
    return QueueError(connection, request.request_id(),
                      Error{ErrorCode::kCapabilityDenied, 0});
  }
  if (request.payload_type() == protocol::RequestPayload::Append) {
    auto result = actor->Append(connection.connection_id,
                                *request.payload_as_Append());
    if (!result) {
      return QueueError(connection, request.request_id(), result.error());
    }
    RequestArena arena;
    auto& builder = arena.builder();
    const auto leaf_hash = builder.CreateVector(
        reinterpret_cast<const std::uint8_t*>(result->last_leaf_hash.data()),
        result->last_leaf_hash.size());
    const auto root = builder.CreateVector(
        reinterpret_cast<const std::uint8_t*>(result->root.data()),
        result->root.size());
    const auto ack = protocol::CreateAppendAck(
        builder, result->client_seq, result->first_lsn, result->last_lsn,
        result->duplicate, result->leaf_count, leaf_hash, root);
    const auto bytes = FinishResponse(builder, request.request_id(),
                                      protocol::ResponseStatus::ok,
                                      protocol::ResponsePayload::AppendAck,
                                      ack.Union());
    auto queued = QueuePayload(connection, bytes);
    if (!queued) {
      return std::unexpected(queued.error());
    }
    return result->duplicate ? std::expected<void, Error>{}
                             : Broadcast(connection.actor, result->frames);
  }
  if (request.payload_type() == protocol::RequestPayload::Subscribe) {
    if (connection.subscriptions.size() >= kMaximumSubscriptions) {
      return QueueError(connection, request.request_id(),
                        Error{ErrorCode::kCapacityExceeded, 0});
    }
    const auto* subscribe = request.payload_as_Subscribe();
    const auto filter = ConversationFilter(subscribe->conversation());
    if (connection.output.size() + 1U >= config_.maximum_output_frames) {
      return QueueError(connection, request.request_id(),
                        Error{ErrorCode::kCapacityExceeded, 0});
    }
    const auto available_frames = config_.maximum_output_frames -
                                  connection.output.size() - 1U;
    auto replay = actor->FramesSince(
        subscribe->since_lsn(), filter,
        FrameScanLimits{
            .maximum_frames = available_frames + 1U,
            .maximum_payload_bytes = std::numeric_limits<std::size_t>::max(),
            .maximum_scanned_frames = std::numeric_limits<std::size_t>::max()});
    if (!replay) {
      return QueueError(connection, request.request_id(), replay.error());
    }
    if (replay->frames.size() > available_frames) {
      return QueueError(connection, request.request_id(),
                        Error{ErrorCode::kCapacityExceeded, 0});
    }
    const auto subscription_id = connection.next_subscription_id++;
    RequestArena arena;
    auto& builder = arena.builder();
    const auto ack = protocol::CreateSubscriptionAck(builder, subscription_id);
    const auto bytes = FinishResponse(
        builder, request.request_id(), protocol::ResponseStatus::ok,
        protocol::ResponsePayload::SubscriptionAck, ack.Union());
    auto queued = QueuePayload(connection, bytes);
    if (!queued) {
      return std::unexpected(queued.error());
    }
    connection.subscriptions.push_back(
        Subscription{.id = subscription_id, .conversation = filter});
    for (const auto& frame : replay->frames) {
      auto event = QueueEvent(connection, subscription_id, frame);
      if (!event) {
        return std::unexpected(event.error());
      }
    }
    return {};
  }
  if (request.payload_type() == protocol::RequestPayload::Activate) {
    auto bundle = actor->Activate(*request.payload_as_Activate());
    if (!bundle) {
      return QueueError(connection, request.request_id(), bundle.error());
    }
    if (bundle->size() + kMaximumResponseSlopBytes >
        kMaximumProtocolPayloadBytes) {
      return QueueError(connection, request.request_id(),
                        Error{ErrorCode::kCapacityExceeded, 0});
    }
    flatbuffers::FlatBufferBuilder builder(bundle->size() + 256U);
    const auto bytes_offset = builder.CreateVector(
        reinterpret_cast<const std::uint8_t*>(bundle->data()), bundle->size());
    const auto result = protocol::CreateBytesResult(builder, bytes_offset);
    const auto bytes = FinishResponse(builder, request.request_id(),
                                      protocol::ResponseStatus::ok,
                                      protocol::ResponsePayload::BytesResult,
                                      result.Union());
    return QueuePayload(connection, bytes);
  }
  if (request.payload_type() == protocol::RequestPayload::Transcript) {
    const auto* transcript = request.payload_as_Transcript();
    const auto filter = ConversationFilter(transcript->conversation());
    auto scan = actor->FramesSince(
        transcript->since_lsn(), filter,
        FrameScanLimits{.maximum_frames = transcript->limit(),
                        .maximum_payload_bytes = kMaximumTranscriptBytes,
                        .maximum_scanned_frames = kMaximumTranscriptScan});
    if (!scan) {
      return QueueError(connection, request.request_id(), scan.error());
    }
    const auto& frames = scan->frames;
    std::size_t payload_bytes = 0;
    std::size_t included = 0;
    while (included < frames.size() &&
           (included == 0 ||
            payload_bytes + frames.at(included).sealed_payload.size() +
                    kTranscriptRecordOverhead <=
                kMaximumTranscriptBytes)) {
      payload_bytes += frames.at(included).sealed_payload.size() +
                       kTranscriptRecordOverhead;
      ++included;
    }
    flatbuffers::FlatBufferBuilder builder(payload_bytes + 1024U);
    std::vector<flatbuffers::Offset<protocol::FrameRecord>> records;
    records.reserve(included);
    for (std::size_t index = 0; index < included; ++index) {
      const auto& frame = frames.at(index);
      const auto conversation = builder.CreateVector(
          reinterpret_cast<const std::uint8_t*>(
              frame.header.conversation.bytes.data()),
          frame.header.conversation.bytes.size());
      const auto payload = builder.CreateVector(
          reinterpret_cast<const std::uint8_t*>(frame.sealed_payload.data()),
          frame.sealed_payload.size());
      records.push_back(protocol::CreateFrameRecord(
          builder, frame.header.lsn,
          static_cast<std::uint8_t>(frame.header.kind),
          frame.header.wall_timestamp_ns, frame.header.actor, conversation,
          payload));
    }
    const auto result = protocol::CreateTranscriptResult(
        builder, builder.CreateVector(records),
        !scan->exhausted || included < frames.size());
    const auto bytes = FinishResponse(
        builder, request.request_id(), protocol::ResponseStatus::ok,
        protocol::ResponsePayload::TranscriptResult, result.Union());
    return QueuePayload(connection, bytes);
  }
  if (request.payload_type() == protocol::RequestPayload::Recall) {
    auto output = actor->Recall(*request.payload_as_Recall());
    if (!output) {
      return QueueError(connection, request.request_id(), output.error());
    }
    flatbuffers::FlatBufferBuilder builder(
        output->windows.size() * 48U + output->members.size() * 8U + 1024U);
    std::vector<flatbuffers::Offset<protocol::TemporalWindowRecord>> windows;
    windows.reserve(output->windows.size());
    for (const auto& window : output->windows) {
      windows.push_back(protocol::CreateTemporalWindowRecord(
          builder, static_cast<std::uint8_t>(window.level), window.start_ns,
          window.end_ns, window.member_count));
    }
    const auto result = protocol::CreateRecallResult(
        builder, builder.CreateVector(windows),
        builder.CreateVector(output->members));
    const auto bytes = FinishResponse(builder, request.request_id(),
                                      protocol::ResponseStatus::ok,
                                      protocol::ResponsePayload::RecallResult,
                                      result.Union());
    return QueuePayload(connection, bytes);
  }
  if (request.payload_type() == protocol::RequestPayload::AsOf) {
    auto record = actor->AsOf(*request.payload_as_AsOf());
    if (!record) {
      return QueueError(connection, request.request_id(), record.error());
    }
    flatbuffers::FlatBufferBuilder builder(4096);
    flatbuffers::Offset<protocol::BeliefResult> result;
    if (!record->has_value()) {
      result = protocol::CreateBeliefResult(builder, false);
    } else {
      const auto& belief = **record;
      const auto belief_id = builder.CreateVector(
          reinterpret_cast<const std::uint8_t*>(belief.belief_id.data()),
          belief.belief_id.size());
      const auto identity = builder.CreateString(belief.canonical_identity);
      const auto domain = builder.CreateString(belief.conflict_domain);
      const auto value = builder.CreateVector(
          reinterpret_cast<const std::uint8_t*>(belief.value.data()),
          belief.value.size());
      std::vector<flatbuffers::Offset<protocol::BeliefProvenanceRecord>>
          provenance;
      provenance.reserve(belief.provenance.size());
      for (const auto& range : belief.provenance) {
        provenance.push_back(protocol::CreateBeliefProvenanceRecord(
            builder, range.first_lsn, range.last_lsn));
      }
      std::vector<flatbuffers::Offset<protocol::BeliefConflictRecord>> edges;
      edges.reserve(belief.conflict_edges.size());
      for (const auto& edge : belief.conflict_edges) {
        edges.push_back(protocol::CreateBeliefConflictRecord(
            builder, static_cast<std::uint8_t>(edge.other_type),
            builder.CreateString(edge.other_canonical_identity),
            edge.created_lsn, edge.resolved_lsn, edge.obligated_surfacing));
      }
      result = protocol::CreateBeliefResult(
          builder, true, static_cast<std::uint8_t>(belief.type), belief_id,
          identity, domain, value, static_cast<std::uint8_t>(belief.claim),
          belief.valid_from_ns, belief.valid_to_ns, belief.transaction_lsn,
          belief.version, belief.supersedes_version,
          builder.CreateVector(provenance), builder.CreateVector(edges),
          belief.tombstoned);
    }
    const auto bytes = FinishResponse(builder, request.request_id(),
                                      protocol::ResponseStatus::ok,
                                      protocol::ResponsePayload::BeliefResult,
                                      result.Union());
    return QueuePayload(connection, bytes);
  }
  if (request.payload_type() == protocol::RequestPayload::Checkpoint) {
    const auto* checkpoint = request.payload_as_Checkpoint();
    auto written = actor->WriteCheckpoint(connection.connection_id,
                                          checkpoint->client_seq(),
                                          Bytes(*checkpoint->turn_id()),
                                          Bytes(*checkpoint->blob()));
    if (!written) {
      return QueueError(connection, request.request_id(), written.error());
    }
    RequestArena arena;
    auto& builder = arena.builder();
    const auto ack = protocol::CreateCheckpointAck(builder, written->lsn);
    const auto bytes = FinishResponse(builder, request.request_id(),
                                      protocol::ResponseStatus::ok,
                                      protocol::ResponsePayload::CheckpointAck,
                                      ack.Union());
    auto queued = QueuePayload(connection, bytes);
    if (!queued) {
      return std::unexpected(queued.error());
    }
    return Broadcast(connection.actor, written->frames);
  }
  if (request.payload_type() == protocol::RequestPayload::LatestCheckpoint) {
    const auto* latest = request.payload_as_LatestCheckpoint();
    auto read = actor->LatestCheckpoint(Bytes(*latest->turn_id()));
    if (!read) {
      return QueueError(connection, request.request_id(), read.error());
    }
    const auto blob_size = read->has_value() ? (*read)->blob.size() : 0U;
    flatbuffers::FlatBufferBuilder builder(blob_size + 512U);
    flatbuffers::Offset<protocol::CheckpointResult> result;
    if (read->has_value()) {
      const auto blob = builder.CreateVector(
          reinterpret_cast<const std::uint8_t*>((*read)->blob.data()),
          (*read)->blob.size());
      result = protocol::CreateCheckpointResult(builder, true, (*read)->lsn,
                                                blob);
    } else {
      result = protocol::CreateCheckpointResult(builder, false, 0, 0);
    }
    const auto bytes = FinishResponse(
        builder, request.request_id(), protocol::ResponseStatus::ok,
        protocol::ResponsePayload::CheckpointResult, result.Union());
    return QueuePayload(connection, bytes);
  }
  if (request.payload_type() == protocol::RequestPayload::Attest) {
    const auto* attest = request.payload_as_Attest();
    const auto used =
        attest->used() == nullptr
            ? std::span<const std::uint64_t>{}
            : std::span<const std::uint64_t>(attest->used()->data(),
                                             attest->used()->size());
    const auto ignored =
        attest->ignored() == nullptr
            ? std::span<const std::uint64_t>{}
            : std::span<const std::uint64_t>(attest->ignored()->data(),
                                             attest->ignored()->size());
    auto written = actor->Attest(connection.connection_id,
                                 attest->client_seq(), used, ignored);
    if (!written) {
      return QueueError(connection, request.request_id(), written.error());
    }
    RequestArena arena;
    auto& builder = arena.builder();
    const auto ack = protocol::CreateAttestAck(
        builder, written->first_lsn, written->last_lsn, written->count);
    const auto bytes = FinishResponse(builder, request.request_id(),
                                      protocol::ResponseStatus::ok,
                                      protocol::ResponsePayload::AttestAck,
                                      ack.Union());
    auto queued = QueuePayload(connection, bytes);
    if (!queued) {
      return std::unexpected(queued.error());
    }
    return Broadcast(connection.actor, written->frames);
  }
  return QueueError(connection, request.request_id(),
                    Error{ErrorCode::kOperationUnavailable, 0});
}

std::expected<void, Error> Server::HandleAdminRequest(
    Connection& connection, const protocol::Request& request) {
  RequestArena arena;
  auto& builder = arena.builder();
  if (request.payload_type() == protocol::RequestPayload::Health) {
    const bool ready = std::all_of(
        actors_.begin(), actors_.end(),
        [](const auto& item) { return item.second->ready(); });
    const auto health = protocol::CreateHealthResult(
        builder, ready, static_cast<std::uint32_t>(actors_.size()),
        static_cast<std::uint32_t>(connections_.size()));
    const auto bytes = FinishResponse(
        builder, request.request_id(), protocol::ResponseStatus::ok,
        protocol::ResponsePayload::HealthResult, health.Union());
    return QueuePayload(connection, bytes);
  }
  if (request.payload_type() == protocol::RequestPayload::Stats) {
    const auto actor_id = request.payload_as_Stats()->actor();
    auto* actor = FindActor(actor_id);
    if (actor == nullptr) {
      return QueueError(connection, request.request_id(),
                        Error{ErrorCode::kCapabilityDenied, 0});
    }
    auto stats = actor->Stats();
    if (!stats) {
      return QueueError(connection, request.request_id(), stats.error());
    }
    std::vector<flatbuffers::Offset<protocol::ProjectionStat>> projections;
    projections.reserve(stats->projections.size());
    for (const auto& projection : stats->projections) {
      projections.push_back(protocol::CreateProjectionStat(
          builder, builder.CreateString(projection.name),
          projection.applied_lsn));
    }
    const auto result = protocol::CreateStatsResult(
        builder, actor_id, stats->log_events, stats->log_bytes,
        builder.CreateVector(projections));
    const auto bytes = FinishResponse(
        builder, request.request_id(), protocol::ResponseStatus::ok,
        protocol::ResponsePayload::StatsResult, result.Union());
    return QueuePayload(connection, bytes);
  }
  if (request.payload_type() == protocol::RequestPayload::LatencyHistograms) {
    std::vector<flatbuffers::Offset<protocol::LatencyBucket>> buckets;
    buckets.reserve(latency_counts_.size() * kLatencyBoundsNs.size());
    for (std::size_t operation = 1; operation < latency_counts_.size();
         ++operation) {
      const auto type = static_cast<protocol::RequestPayload>(operation);
      const auto name = protocol::EnumNameRequestPayload(type);
      for (std::size_t bucket = 0; bucket < kLatencyBoundsNs.size(); ++bucket) {
        buckets.push_back(protocol::CreateLatencyBucket(
            builder, builder.CreateString(name), kLatencyBoundsNs[bucket],
            latency_counts_[operation][bucket]));
      }
    }
    const auto result =
        protocol::CreateLatencyResult(builder, builder.CreateVector(buckets));
    const auto bytes = FinishResponse(
        builder, request.request_id(), protocol::ResponseStatus::ok,
        protocol::ResponsePayload::LatencyResult, result.Union());
    return QueuePayload(connection, bytes);
  }
  if (request.payload_type() == protocol::RequestPayload::VerifyStatus) {
    const auto actor_id = request.payload_as_VerifyStatus()->actor();
    auto* actor = FindActor(actor_id);
    if (actor == nullptr) {
      return QueueError(connection, request.request_id(),
                        Error{ErrorCode::kCapabilityDenied, 0});
    }
    const auto status = actor->VerificationStatus();
    const auto root = builder.CreateVector(
        reinterpret_cast<const std::uint8_t*>(status.root.data()),
        status.root.size());
    const auto result = protocol::CreateVerifyResult(
        builder, actor_id, root, status.leaf_count,
        status.last_checkpoint_lsn, status.verified);
    const auto bytes = FinishResponse(
        builder, request.request_id(), protocol::ResponseStatus::ok,
        protocol::ResponsePayload::VerifyResult, result.Union());
    return QueuePayload(connection, bytes);
  }
  if (request.payload_type() == protocol::RequestPayload::RebuildProjection) {
    const auto* rebuild = request.payload_as_RebuildProjection();
    auto* actor = FindActor(rebuild->actor());
    if (actor == nullptr) {
      return QueueError(connection, request.request_id(),
                        Error{ErrorCode::kCapabilityDenied, 0});
    }
    auto applied = actor->RebuildProjection(rebuild->name()->string_view());
    if (!applied) {
      return QueueError(connection, request.request_id(), applied.error());
    }
    const auto result = protocol::CreateRebuildResult(
        builder, rebuild->actor(),
        builder.CreateString(rebuild->name()->string_view()), *applied);
    const auto bytes = FinishResponse(
        builder, request.request_id(), protocol::ResponseStatus::ok,
        protocol::ResponsePayload::RebuildResult, result.Union());
    return QueuePayload(connection, bytes);
  }
  if (request.payload_type() == protocol::RequestPayload::CryptoDelete) {
    const auto actor_id = request.payload_as_CryptoDelete()->actor();
    auto* actor = FindActor(actor_id);
    if (actor == nullptr) {
      return QueueError(connection, request.request_id(),
                        Error{ErrorCode::kCapabilityDenied, 0});
    }
    auto receipt = actor->CryptoDelete();
    if (!receipt) {
      return QueueError(connection, request.request_id(), receipt.error());
    }
    const auto receipt_vector = builder.CreateVector(
        reinterpret_cast<const std::uint8_t*>(receipt->data()), receipt->size());
    const auto result =
        protocol::CreateDeleteResult(builder, actor_id, receipt_vector);
    const auto bytes = FinishResponse(
        builder, request.request_id(), protocol::ResponseStatus::ok,
        protocol::ResponsePayload::DeleteResult, result.Union());
    return QueuePayload(connection, bytes);
  }
  return QueueError(connection, request.request_id(),
                    Error{ErrorCode::kOperationUnavailable, 0});
}

std::expected<void, Error> Server::QueueEvent(
    Connection& connection, std::uint64_t subscription_id,
    const log::Frame& frame) {
  flatbuffers::FlatBufferBuilder builder(
      std::min<std::size_t>(frame.sealed_payload.size() + 256U,
                            kMaximumProtocolPayloadBytes));
  const auto conversation = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(frame.header.conversation.bytes.data()),
      frame.header.conversation.bytes.size());
  const auto payload = builder.CreateVector(
      reinterpret_cast<const std::uint8_t*>(frame.sealed_payload.data()),
      frame.sealed_payload.size());
  const auto event = protocol::CreateEvent(
      builder, subscription_id, frame.header.lsn,
      static_cast<std::uint8_t>(frame.header.kind),
      frame.header.wall_timestamp_ns, frame.header.actor, conversation, payload);
  const auto bytes =
      FinishWire(builder, protocol::WirePayload::Event, event.Union());
  return QueuePayload(connection, bytes);
}

std::expected<void, Error> Server::Broadcast(
    std::uint16_t actor_id, std::span<const log::Frame> frames) {
  for (auto& connection : connections_) {
    if (!connection.handshaken || connection.admin ||
        connection.actor != actor_id) {
      continue;
    }
    bool saturated = false;
    for (const auto& subscription : connection.subscriptions) {
      for (const auto& frame : frames) {
        if (!subscription.conversation ||
            *subscription.conversation == frame.header.conversation) {
          auto queued = QueueEvent(connection, subscription.id, frame);
          if (!queued) {
            static_cast<void>(::shutdown(connection.descriptor, SHUT_RDWR));
            saturated = true;
            break;
          }
        }
      }
      if (saturated) {
        break;
      }
    }
  }
  return {};
}

std::expected<bool, Error> Server::ReadConnection(Connection& connection) {
  std::array<std::byte, static_cast<std::size_t>(64) * 1024U> buffer{};
  while (true) {
    const auto received = ::recv(connection.descriptor, buffer.data(),
                                 buffer.size(), 0);
    if (received == 0) {
      return false;
    }
    if (received < 0) {
      if (errno == EAGAIN || errno == EWOULDBLOCK) {
        return true;
      }
      if (errno == EINTR) {
        continue;
      }
      return std::unexpected(Error{ErrorCode::kReadFailed, errno});
    }
    auto frames = connection.parser.Push(
        std::span(buffer).first(static_cast<std::size_t>(received)));
    if (!frames) {
      return std::unexpected(frames.error());
    }
    for (const auto& frame : *frames) {
      auto handled = HandlePayload(connection, frame);
      if (!handled) {
        return std::unexpected(handled.error());
      }
    }
  }
}

std::expected<bool, Error> Server::WriteConnection(Connection& connection) {
  while (!connection.output.empty()) {
    auto& frame = connection.output.front();
    const auto remaining = std::span(frame.bytes).subspan(frame.offset);
    const auto sent = ::send(connection.descriptor, remaining.data(),
                             remaining.size(), MSG_NOSIGNAL);
    if (sent < 0) {
      if (errno == EAGAIN || errno == EWOULDBLOCK) {
        return true;
      }
      if (errno == EINTR) {
        continue;
      }
      return std::unexpected(Error{ErrorCode::kWriteFailed, errno});
    }
    if (sent == 0) {
      return false;
    }
    frame.offset += static_cast<std::size_t>(sent);
    if (frame.offset == frame.bytes.size()) {
      connection.output_bytes -= frame.bytes.size();
      connection.output.pop_front();
    }
  }
  return true;
}

void Server::CloseConnection(std::size_t index) {
  ::close(connections_[index].descriptor);
  connections_.erase(connections_.begin() +
                     static_cast<std::ptrdiff_t>(index));
}

std::expected<void, Error> Server::Run() {
  while (true) {
    std::vector<pollfd> descriptors;
    descriptors.reserve(connections_.size() + 2U);
    descriptors.push_back(
        pollfd{.fd = signal_descriptor_, .events = POLLIN, .revents = 0});
    descriptors.push_back(
        pollfd{.fd = listen_descriptor_,
               .events = static_cast<short>(
                   connections_.size() < config_.maximum_connections ? POLLIN
                                                                      : 0),
               .revents = 0});
    for (const auto& connection : connections_) {
      descriptors.push_back(pollfd{
          .fd = connection.descriptor,
          .events = static_cast<short>(
              POLLIN | (connection.output.empty() ? 0 : POLLOUT)),
          .revents = 0});
    }
    const int status = ::poll(descriptors.data(), descriptors.size(), -1);
    if (status < 0) {
      if (errno == EINTR) {
        continue;
      }
      return std::unexpected(Error{ErrorCode::kReadFailed, errno});
    }
    if ((descriptors[0].revents & POLLIN) != 0) {
      signalfd_siginfo signal{};
      static_cast<void>(
          ::read(signal_descriptor_, &signal, sizeof(signal)));
      return {};
    }
    for (std::size_t index = connections_.size(); index > 0; --index) {
      const auto connection_index = index - 1U;
      const auto revents = descriptors[connection_index + 2U].revents;
      bool keep = (revents & (POLLERR | POLLHUP | POLLNVAL)) == 0;
      if (keep && (revents & POLLIN) != 0) {
        auto read = ReadConnection(connections_[connection_index]);
        keep = read && *read;
      }
      if (keep && (revents & POLLOUT) != 0) {
        auto written = WriteConnection(connections_[connection_index]);
        keep = written && *written;
      }
      if (!keep) {
        CloseConnection(connection_index);
      }
    }
    if ((descriptors[1].revents & POLLIN) != 0) {
      auto accepted = AcceptConnections();
      if (!accepted) {
        return std::unexpected(accepted.error());
      }
    }
  }
}

}  // namespace neocortex::serve
