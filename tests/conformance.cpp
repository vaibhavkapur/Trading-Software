#include "session.hpp"
#include <cstdlib>
#include <fstream>
#include <iostream>
#include <sys/wait.h>
#include <unistd.h>

using namespace sim;
namespace {
struct Temp {
  std::filesystem::path path;
  Temp() {
    auto pattern =
        (std::filesystem::temp_directory_path() / "fix-scenario-XXXXXX")
            .string();
    if (!mkdtemp(pattern.data()))
      throw std::runtime_error("mkdtemp failed");
    path = pattern;
  }
  ~Temp() {
    std::error_code ec;
    std::filesystem::remove_all(path, ec);
  }
};
void require(bool ok, const std::string &why) {
  if (!ok)
    throw std::runtime_error(why);
}
template <class Exception, class Fn> void throws(Fn fn) {
  try {
    fn();
  } catch (const Exception &) {
    return;
  }
  throw std::runtime_error("expected failure");
}
Command buy(std::string id, int price = 10000) {
  return {"D", id, "", "1", 7, price};
}
FIX44::Message message(const char *t) {
  return FIX44::Message(FIX::MsgType(t));
}
void totals(Gateway &g, const Strings &expected) {
  require(g.oms().totals().at(0) == expected,
          "incorrect OMS totals (reports, fills, shares, pending)");
}
struct Capture {
  std::vector<Msg> frames;
  explicit Capture(Gateway &g) {
    g.observe_wire([this](bool, const std::string &s) {
      frames.emplace_back(s, true);
      return s;
    });
  }
  bool has(const std::string &t, int tag, const std::string &value) const {
    for (const auto &m : frames)
      if (type(m) == t && m.isSetField(tag) && m.getField(tag) == value)
        return true;
    return false;
  }
};
void domain() {
  for (const Command &c : {buy("n"), Command{"F", "c", "n", "1", 7, 0},
                           Command{"G", "g", "n", "1", 8, 1}})
    require(Command::decode(c.encode()).signature() == c.signature(),
            "command round trip");
  for (const auto &s : {"-1", "1.001", "1e2", "", "9999999999"})
    throws<FIX::IncorrectTagValue>([&] { cents(s, 44); });
  for (auto c : {buy("bad id"), buy("x", 0), buy("x", 100000001),
                 Command{"F", "x", "x", "1", 2, 0}})
    throws<FIX::IncorrectTagValue>([&] { c.encode(); });
  auto m = buy("x").encode();
  m.setField(59, "0");
  throws<FIX::IncorrectTagValue>([&] { Command::decode(m); });
  m = buy("x").encode();
  m.setField(111, "2");
  throws<FIX::TagNotDefinedForMessage>([&] { Command::decode(m); });
}
void lifecycle() {
  Temp dir;
  Gateway g(dir.path);
  g.connect();
  g.submit(buy("a", 9900));
  g.submit({"G", "b", "a", "1", 9, 9950});
  g.submit({"F", "c", "b", "1", 9, 0});
  g.submit({"F", "stale", "a", "1", 7, 0});
  g.submit({"G", "missing", "unknown", "2", 2, 11000});
  g.submit(buy("x", 9900));
  g.submit({"G", "cross", "x", "1", 11, 10001});
  g.submit(buy("x", 9900));
  throws<FIX::IncorrectTagValue>([&] { g.submit(buy("x", 9901)); });
  g.reconcile();
  totals(g, {"8", "1", "11", "0"});
  std::map<std::string, Report> reports;
  for (auto &row : g.oms().snapshot())
    reports.emplace(row[0], Report::decode(Msg(row[1], false)));
  require(reports.at("c/ack").order == "O/a" &&
              reports.at("c/ack").status == "4",
          "cancel chain");
  require(reports.at("R/stale").status == "4", "stale cancel status");
  require(reports.at("R/missing").reason == "unknown original order",
          "unknown order rejection");
  require(reports.at("cross/trade").last_price == 10000, "price improvement");
  g.logout();
  throws<std::runtime_error>([&] { g.submit(buy("closed")); });
}
void session_admin() {
  Temp dir;
  Gateway g(dir.path);
  Capture capture(g);
  g.connect();
  auto m = message("1");
  m.setField(112, "ping-1");
  g.send_fixture(true, m);
  g.drain();
  require(capture.has("0", 112, "ping-1"), "missing echo heartbeat");
  g.logout();
  require(!g.health().logged_on && g.health().logouts == 2,
          "unacknowledged logout");
  int logout_frames = 0;
  for (const auto &f : capture.frames)
    logout_frames += type(f) == "5";
  require(logout_frames == 2, "missing logout frame");
}
void validation() {
  Temp dir;
  Gateway g(dir.path);
  Capture capture(g);
  g.connect();
  auto m = buy("invalid").encode();
  m.removeField(54);
  g.send_fixture(true, m);
  g.drain();
  require(capture.has("3", 371, "54") && capture.has("3", 373, "1"),
          "missing required-field reject");
  auto unsupported = message("H");
  unsupported.setField(11, "unknown");
  unsupported.setField(55, "SYNTH");
  unsupported.setField(54, "1");
  g.send_fixture(true, unsupported);
  g.drain();
  require(capture.has("j", 380, "3") && capture.has("j", 372, "H"),
          "missing unsupported-message reject");
  totals(g, {"0", "0", "0", "0"});
  g.reconcile();
}
void checksum() {
  Temp dir;
  Gateway g(dir.path);
  g.connect();
  bool damaged = false;
  g.observe_wire([&](bool from_oms, const std::string &wire) {
    auto result = wire;
    if (from_oms && !damaged && type(Msg(wire, true)) == "D") {
      damaged = true;
      auto digit = result.rfind("10=") + 3;
      result[digit] = result[digit] == '0' ? '1' : '0';
    }
    return result;
  });
  auto expected = g.health().venue_recv;
  g.submit(buy("checksum"));
  require(damaged && g.health().invalid_frames == 1 &&
              g.health().venue_recv == expected,
          "checksum consumed sequence");
  require(g.venue().snapshot().empty(), "corrupt order executed");
  totals(g, {"0", "0", "0", "1"});
  g.send_fixture(true, message("0"));
  g.drain();
  g.reconcile();
  totals(g, {"2", "1", "7", "0"});
}
void resend() {
  Temp dir;
  Gateway g(dir.path);
  g.connect();
  int drops = 0;
  bool replay = false, gapfill = false;
  g.observe_wire([&](bool from_oms, const std::string &wire) {
    Msg m(wire, true);
    if (from_oms && drops < 2 && (type(m) == "D" || type(m) == "0")) {
      ++drops;
      return std::string{};
    }
    if (from_oms && type(m) == "D" && m.getHeader().isSetField(43)) {
      replay =
          m.getHeader().getField(43) == "Y" && m.getHeader().isSetField(122);
    }
    if (from_oms && type(m) == "4")
      gapfill = m.getField(123) == "Y" && m.isSetField(36);
    return wire;
  });
  g.send_fixture(true, message("0"));
  g.submit(buy("replay"));
  totals(g, {"0", "0", "0", "1"});
  g.send_fixture(true, message("0"));
  g.drain();
  g.reconcile();
  require(replay && gapfill && g.health().resends > 0,
          "missing replay or administrative gap-fill");
  totals(g, {"2", "1", "7", "0"});
}
void duplicate() {
  Temp dir;
  Gateway g(dir.path);
  g.connect();
  g.submit(buy("fill"));
  auto rows = g.oms().snapshot();
  Msg fill(rows.back()[1], false);
  require(fill.getField(150) == "F", "fixture is not a trade");
  g.send_fixture(false, fill);
  g.drain();
  g.reconcile();
  require(g.health().duplicates == 1, "duplicate not identified");
  totals(g, {"2", "1", "7", "0"});
  fill.setField(60, "20200101-00:00:00.000");
  g.send_fixture(false, fill);
  throws<std::runtime_error>([&] { g.drain(); });
  totals(g, {"2", "1", "7", "0"});
}
void restart() {
  Temp dir;
  uint64_t seq = 0;
  {
    Gateway g(dir.path);
    g.connect();
    g.observe_wire([](bool from_oms, const std::string &wire) {
      Msg m(wire, true);
      if (!from_oms && type(m) == "8" && m.getField(150) == "F")
        return std::string{};
      return wire;
    });
    g.submit(buy("lost"));
    totals(g, {"1", "0", "0", "0"});
    require(g.venue().snapshot().size() == 2, "venue did not fill");
    seq = g.health().venue_send;
    g.disconnect();
  }
  // Restart after an earlier UTC trading date. Only the engine's creation
  // timestamp changes. The message bodies and sequence numbers are untouched.
  for (const auto &file :
       std::filesystem::directory_iterator(dir.path / "sequence"))
    if (file.path().extension() == ".session")
      std::ofstream(file.path()) << "20200101-00:00:00";
  Gateway g(dir.path);
  g.connect();
  require(g.health().venue_send > seq && g.health().resends > 0,
          "missing reconnect replay");
  totals(g, {"2", "1", "7", "0"});
  g.reconcile();
  g.logout();
}
std::string executable;
void interrupt_delivery(const std::filesystem::path &state) {
  Gateway g(state);
  g.connect();
  g.observe_wire([](bool client, const std::string &wire) {
    Msg m(wire, true);
    if (!client && type(m) == "8" && m.getField(150) == "F")
      _exit(77);
    return wire;
  });
  g.submit(buy("crash"));
}
void crash_recovery() {
  Temp dir;
  auto pid = fork();
  if (pid < 0)
    throw std::runtime_error("fork failed");
  if (pid == 0) {
    execl(executable.c_str(), executable.c_str(), "--interrupt-delivery",
          dir.path.c_str(), nullptr);
    _exit(1);
  }
  int status = 0;
  require(waitpid(pid, &status, 0) == pid && WIFEXITED(status) &&
              WEXITSTATUS(status) == 77,
          "crash point not reached");
  Gateway g(dir.path);
  g.connect();
  g.reconcile();
  totals(g, {"2", "1", "7", "0"});
  require(g.health().resends > 0 && g.health().duplicates > 0,
          "wire replay and outbox recovery did not overlap");
}
void interrupted_handoffs() {
  Temp dir;
  auto first = buy("staged"), second = buy("committed");
  auto a = first.encode(), b = second.encode();
  {
    Journal oms(dir.path / "oms.sqlite"), venue(dir.path / "venue.sqlite");
    oms.stage(second, b);
    venue.execute(second, b); // venue committed before sending its outbox
    oms.stage(first, a);      // OMS committed before handing off to FIX
  }
  Gateway g(dir.path);
  g.connect();
  g.reconcile();
  totals(g, {"4", "2", "14", "0"});
  throws<std::system_error>([&] { Gateway other(dir.path); });
}
} // namespace
int main(int argc, char **argv) {
  executable = std::filesystem::absolute(argv[0]).string();
  if (argc == 3 && std::string(argv[1]) == "--interrupt-delivery") {
    interrupt_delivery(argv[2]);
    return 1;
  }
  using Test = std::pair<const char *, void (*)()>;
  const Test tests[] = {
      {"domain invariants and dialect", domain},
      {"order lifecycle and unknown order", lifecycle},
      {"logon, test request, heartbeat and logout", session_admin},
      {"missing field and unsupported message", validation},
      {"invalid checksum and recovery", checksum},
      {"sequence gap, resend and gap-fill", resend},
      {"duplicate and conflicting execution report", duplicate},
      {"lost fill and cross-date restart", restart},
      {"process death during fill delivery", crash_recovery},
      {"uncertain handoffs and exclusive state ownership",
       interrupted_handoffs}};
  int failures = 0;
  for (auto [name, test] : tests) {
    try {
      test();
      std::cout << "PASS " << name << std::endl;
    } catch (const std::exception &e) {
      ++failures;
      std::cerr << "FAIL " << name << " - " << e.what() << std::endl;
    }
  }
  return failures ? 1 : 0;
}
