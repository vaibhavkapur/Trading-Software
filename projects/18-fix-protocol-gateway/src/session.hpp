#pragma once
#include "journal.hpp"
#include <functional>
#include <memory>

namespace sim {
struct Health {
  bool logged_on{};
  uint64_t oms_send{}, oms_recv{}, venue_send{}, venue_recv{};
  unsigned duplicates{}, invalid_frames{}, rejects{}, resends{}, gap_fills{},
      logons{}, logouts{};
};
class Gateway {
public:
  explicit Gateway(const std::filesystem::path &);
  ~Gateway();
  void connect();
  void disconnect();
  void logout();
  void tick();
  void submit(const Command &);
  void reconcile();
  Health health() const;
  const Journal &oms() const;
  const Journal &venue() const;

  // Harness seam at the actual outbound FIX wire, after session persistence.
  using WireHook =
      std::function<std::string(bool from_oms, const std::string &)>;
  void observe_wire(WireHook);
  void send_fixture(bool from_oms, Msg);
  void drain();

private:
  struct Impl;
  std::unique_ptr<Impl> p;
};
} // namespace sim
