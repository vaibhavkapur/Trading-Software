#include "session.hpp"
#include <arpa/inet.h>
#include <fcntl.h>
#include <netinet/tcp.h>
#include <quickfix/Application.h>
#include <quickfix/FileLog.h>
#include <quickfix/FileStore.h>
#include <quickfix/Parser.h>
#include <quickfix/Responder.h>
#include <quickfix/Session.h>
#include <quickfix/SessionFactory.h>
#include <sys/file.h>
#include <sys/stat.h>
#include <system_error>
#include <unistd.h>

namespace sim {
namespace {
void checked(int result, const char *operation) {
  if (result < 0)
    throw std::system_error(errno, std::generic_category(), operation);
}
struct Fd {
  int value = -1;
  Fd() = default;
  explicit Fd(int n) : value(n) { checked(n, "open descriptor"); }
  ~Fd() { reset(); }
  Fd(const Fd &) = delete;
  void reset(int n = -1) {
    if (value >= 0)
      ::close(value);
    value = n;
  }
};
struct StateLock {
  Fd lock;
  explicit StateLock(const std::filesystem::path &root) {
    std::filesystem::create_directories(root);
    lock.reset(::open((root / "session.lock").c_str(), O_CREAT | O_RDWR, 0600));
    checked(lock.value, "open state lock");
    checked(flock(lock.value, LOCK_EX | LOCK_NB),
            "state directory already in use");
  }
};
struct Link : FIX::Responder {
  bool from_oms;
  Gateway::WireHook &hook;
  Fd socket;
  unsigned frames = 0;
  FIX::Parser parser;
  Link(bool side, Gateway::WireHook &h) : from_oms(side), hook(h) {}
  bool send(const std::string &original) override {
    auto bytes = hook ? hook(from_oms, original) : original;
    if (bytes.empty())
      return true;
    size_t offset = 0;
    while (offset < bytes.size()) {
      auto n =
          ::send(socket.value, bytes.data() + offset, bytes.size() - offset, 0);
      if (n < 0 && errno == EINTR)
        continue;
      checked(n, "loopback send");
      if (!n)
        throw std::runtime_error("closed sender");
      offset += n;
    }
    ++frames;
    return true;
  }
  std::string read() {
    std::string frame;
    while (!parser.readFixMessage(frame)) {
      char chunk[4096];
      auto n = recv(socket.value, chunk, sizeof(chunk), 0);
      if (n < 0 && errno == EINTR)
        continue;
      checked(n, "loopback receive");
      if (!n)
        throw std::runtime_error("unexpected end of stream");
      parser.addToStream(chunk, n);
    }
    return frame;
  }
  // A session can disconnect immediately after writing its logout reply.
  // The harness drains already-written bytes before closing both sockets.
  void disconnect() override {}
  void clear() {
    socket.reset();
    parser = FIX::Parser();
    frames = 0;
  }
};
void open_pair(Link &client, Link &server) {
  Fd listener(::socket(AF_INET, SOCK_STREAM, 0));
  sockaddr_in addr{};
  addr.sin_family = AF_INET;
  addr.sin_addr.s_addr = htonl(INADDR_LOOPBACK);
  checked(
      bind(listener.value, reinterpret_cast<sockaddr *>(&addr), sizeof(addr)),
      "bind loopback");
  checked(listen(listener.value, 1), "listen loopback");
  socklen_t size = sizeof(addr);
  checked(
      getsockname(listener.value, reinterpret_cast<sockaddr *>(&addr), &size),
      "listener address");
  client.socket.reset(::socket(AF_INET, SOCK_STREAM, 0));
  checked(client.socket.value, "client socket");
  checked(
      ::connect(client.socket.value, reinterpret_cast<sockaddr *>(&addr), size),
      "connect loopback");
  sockaddr_in origin{};
  server.socket.reset(
      accept(listener.value, reinterpret_cast<sockaddr *>(&origin), &size));
  checked(server.socket.value, "accept loopback");
  sockaddr_in local{};
  checked(getsockname(client.socket.value, reinterpret_cast<sockaddr *>(&local),
                      &size),
          "client address");
  if (origin.sin_port != local.sin_port ||
      origin.sin_addr.s_addr != local.sin_addr.s_addr)
    throw std::runtime_error("unexpected loopback peer");
  for (int fd : {client.socket.value, server.socket.value}) {
    int yes = 1;
    checked(setsockopt(fd, IPPROTO_TCP, TCP_NODELAY, &yes, sizeof(yes)),
            "TCP_NODELAY");
    timeval timeout{3, 0};
    checked(setsockopt(fd, SOL_SOCKET, SO_RCVTIMEO, &timeout, sizeof(timeout)),
            "receive timeout");
    checked(setsockopt(fd, SOL_SOCKET, SO_SNDTIMEO, &timeout, sizeof(timeout)),
            "send timeout");
  }
}

// ResetSeqNumFlag is processed by QuickFIX before the application's logon
// callback. Protect the store itself against a peer-initiated destructive
// reset.
class ContinuousStore final : public FIX::FileStore {
public:
  using FileStore::FileStore;
  void reset(const FIX::UtcTimeStamp &) override {
    throw FIX::IOException("sequence reset disabled");
  }
};
class StoreFactory final : public FIX::MessageStoreFactory {
  std::string path;

public:
  explicit StoreFactory(std::string p) : path(std::move(p)) {}
  FIX::MessageStore *create(const FIX::UtcTimeStamp &now,
                            const FIX::SessionID &id) override {
    return new ContinuousStore(now, path, id);
  }
  void destroy(FIX::MessageStore *s) override { delete s; }
};
struct Application : FIX::NullApplication {
  Journal &oms;
  Journal &venue;
  Health stats;
  Application(Journal &a, Journal &b) : oms(a), venue(b) {}
  void onLogon(const FIX::SessionID &) override { ++stats.logons; }
  void onLogout(const FIX::SessionID &) override { ++stats.logouts; }
  void fromAdmin(const Msg &m, const FIX::SessionID &) override {
    auto t = type(m);
    if (t == "2")
      ++stats.resends;
    if (t == "4" && m.isSetField(123) && m.getField(123) == "Y")
      ++stats.gap_fills;
    if (t == "3")
      ++stats.rejects;
    if (t == "A" && m.isSetField(141) && m.getField(141) == "Y")
      throw FIX::RejectLogon("sequence reset disabled");
  }
  void fromApp(const Msg &m, const FIX::SessionID &id) override {
    if (type(m) == "j") {
      ++stats.rejects;
      return;
    }
    if (id.getSenderCompID().getString() == "SIM")
      venue.execute(Command::decode(m), m);
    else if (!oms.receive(m))
      ++stats.duplicates;
  }
};
} // namespace

struct Gateway::Impl {
  StateLock lock;
  Journal oms, venue;
  Application app;
  WireHook hook;
  Link client{true, hook}, server{false, hook};
  StoreFactory stores;
  FIX::FileLogFactory logs;
  FIX::SessionFactory factory;
  std::unique_ptr<FIX::Session> initiator, acceptor;
  explicit Impl(const std::filesystem::path &root)
      : lock(root), oms(root / "oms.sqlite"), venue(root / "venue.sqlite"),
        app(oms, venue), stores((root / "sequence").string()),
        logs((root / "logs").string()), factory(app, stores, &logs) {
    FIX::SessionSettings config(std::string(GATEWAY_CONFIG_DIR) +
                                "/simulator-v1.cfg");
    for (const auto &id : config.getSessions()) {
      auto settings = config.get(id);
      settings.setString(FIX::DATA_DICTIONARY,
                         std::string(GATEWAY_CONFIG_DIR) + "/FIX44.xml");
      auto &endpoint =
          id.getSenderCompID().getString() == "OMS" ? initiator : acceptor;
      endpoint.reset(factory.create(id, settings));
    }
    if (!initiator || !acceptor)
      throw std::runtime_error("invalid simulator sessions");
  }
  void transfer(Link &from, Link &to, FIX::Session &target) {
    auto frame = to.read();
    --from.frames;
    try {
      target.next(frame, FIX::UtcTimeStamp::now());
    } catch (const FIX::InvalidMessage &) {
      ++app.stats.invalid_frames;
    }
  }
  void pump() {
    unsigned iterations = 0;
    while (client.frames || server.frames) {
      if (++iterations > 100000)
        throw std::runtime_error("session recovery did not converge");
      if (client.frames)
        transfer(client, server, *acceptor);
      if (server.frames)
        transfer(server, client, *initiator);
    }
  }
  void flush() {
    pump();
    while (acceptor->isLoggedOn() && initiator->isLoggedOn()) {
      auto pending = venue.outbox();
      if (pending.empty())
        return;
      for (const auto &row : pending) {
        Msg report(row[1], false);
        if (!acceptor->send(report))
          throw std::runtime_error("execution send failed");
        venue.published(row[0]);
        pump();
        if (!acceptor->isLoggedOn() || !initiator->isLoggedOn())
          return;
      }
    }
  }
};
Gateway::Gateway(const std::filesystem::path &root)
    : p(std::make_unique<Impl>(root)) {}
Gateway::~Gateway() = default;
void Gateway::connect() {
  disconnect();
  open_pair(p->client, p->server);
  p->acceptor->logon();
  p->initiator->logon();
  p->initiator->setResponder(&p->client);
  p->acceptor->setResponder(&p->server);
  tick();
  if (!health().logged_on)
    throw std::runtime_error("FIX logon failed");
  reconcile();
}
void Gateway::disconnect() {
  p->initiator->disconnect();
  p->acceptor->disconnect();
  p->client.clear();
  p->server.clear();
}
void Gateway::logout() {
  p->initiator->logout();
  tick();
  if (p->initiator->isLoggedOn() || p->acceptor->isLoggedOn())
    throw std::runtime_error("incomplete logout");
  disconnect();
}
void Gateway::tick() {
  if (p->client.socket.value < 0)
    return;
  auto now = FIX::UtcTimeStamp::now();
  p->initiator->next(now);
  p->acceptor->next(now);
  drain();
}
void Gateway::drain() { p->flush(); }
void Gateway::observe_wire(WireHook hook) { p->hook = std::move(hook); }
void Gateway::send_fixture(bool client, Msg m) {
  auto &session = client ? p->initiator : p->acceptor;
  if (!session->isLoggedOn() || !session->send(m))
    throw std::runtime_error("session unavailable");
}
void Gateway::submit(const Command &c) {
  if (!health().logged_on)
    throw std::runtime_error("order entry requires simulator logon");
  auto m = c.encode();
  if (p->oms.stage(c, m)) {
    send_fixture(true, m);
    drain();
  }
}
void Gateway::reconcile() {
  if (!health().logged_on)
    throw std::runtime_error("reconciliation requires logon");
  drain();
  for (const auto &row : p->oms.pending()) {
    send_fixture(true, Msg(row[0], false));
    drain();
  }
  auto h = health();
  if (h.oms_send != h.venue_recv || h.venue_send != h.oms_recv ||
      !p->oms.pending().empty() || p->oms.snapshot() != p->venue.snapshot())
    throw std::runtime_error("reconciliation mismatch");
}
Health Gateway::health() const {
  Health h = p->app.stats;
  h.logged_on = p->initiator->isLoggedOn() && p->acceptor->isLoggedOn();
  h.oms_send = p->initiator->getExpectedSenderNum();
  h.oms_recv = p->initiator->getExpectedTargetNum();
  h.venue_send = p->acceptor->getExpectedSenderNum();
  h.venue_recv = p->acceptor->getExpectedTargetNum();
  return h;
}
const Journal &Gateway::oms() const { return p->oms; }
const Journal &Gateway::venue() const { return p->venue; }
} // namespace sim
