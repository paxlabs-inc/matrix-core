#pragma once

#include <array>
#include <cstddef>
#include <cstdint>
#include <deque>
#include <expected>
#include <map>
#include <memory>
#include <optional>
#include <span>
#include <vector>

#include "core/error.h"
#include "serve/actor_engine.h"
#include "serve/config.h"
#include "serve/protocol.h"

namespace neocortex::serve {

class Server final {
 public:
  static std::expected<std::unique_ptr<Server>, Error> Open(ServerConfig config);

  ~Server();
  Server(const Server&) = delete;
  Server& operator=(const Server&) = delete;

  [[nodiscard]] std::expected<void, Error> Run();

 private:
  struct QueuedFrame final {
    std::vector<std::byte> bytes;
    std::size_t offset;
  };

  struct Subscription final {
    std::uint64_t id;
    std::optional<log::ConversationId> conversation;
  };

  struct Connection final {
    int descriptor;
    ProtocolFrameParser parser;
    std::deque<QueuedFrame> output;
    std::size_t output_bytes;
    bool handshaken;
    bool admin;
    std::uint16_t actor;
    std::array<std::byte, 16> connection_id;
    std::vector<Subscription> subscriptions;
    std::uint64_t next_subscription_id;
  };

  explicit Server(ServerConfig config);

  [[nodiscard]] std::expected<void, Error> Initialize();
  [[nodiscard]] std::expected<void, Error> AcceptConnections();
  [[nodiscard]] std::expected<bool, Error> ReadConnection(Connection& connection);
  [[nodiscard]] std::expected<bool, Error> WriteConnection(Connection& connection);
  [[nodiscard]] std::expected<void, Error> HandlePayload(
      Connection& connection, std::span<const std::byte> payload);
  [[nodiscard]] std::expected<void, Error> HandleHello(
      Connection& connection, const protocol::Hello& hello);
  [[nodiscard]] std::expected<void, Error> HandleRequest(
      Connection& connection, const protocol::Request& request);
  [[nodiscard]] std::expected<void, Error> HandleActorRequest(
      Connection& connection, const protocol::Request& request);
  [[nodiscard]] std::expected<void, Error> HandleAdminRequest(
      Connection& connection, const protocol::Request& request);
  [[nodiscard]] std::expected<void, Error> QueuePayload(
      Connection& connection, std::span<const std::byte> payload);
  [[nodiscard]] std::expected<void, Error> QueueError(
      Connection& connection, std::uint64_t request_id, Error error);
  [[nodiscard]] std::expected<void, Error> QueueEvent(
      Connection& connection, std::uint64_t subscription_id,
      const log::Frame& frame);
  [[nodiscard]] std::expected<void, Error> Broadcast(
      std::uint16_t actor, std::span<const log::Frame> frames);
  [[nodiscard]] ActorEngine* FindActor(std::uint16_t actor);
  void CloseConnection(std::size_t index);
  void RecordLatency(protocol::RequestPayload operation,
                     std::uint64_t elapsed_ns);

  ServerConfig config_;
  int listen_descriptor_ = -1;
  int signal_descriptor_ = -1;
  std::map<std::uint16_t, std::unique_ptr<ActorEngine>> actors_;
  std::vector<Connection> connections_;
  std::array<std::array<std::uint64_t, 8>, 16> latency_counts_{};
};

}  // namespace neocortex::serve
