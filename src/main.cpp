#include "session.hpp"
#include <chrono>
#include <fstream>
#include <iostream>
#include <sstream>
#include <sys/stat.h>

namespace {
std::string json(const std::string &s) {
  std::string out = "\"";
  for (unsigned char c : s) {
    if (c == '"' || c == '\\')
      out += '\\';
    if (c >= 32)
      out += static_cast<char>(c);
  }
  return out + '"';
}
void status(sim::Gateway &gateway, const std::string &event) {
  auto h = gateway.health();
  auto row = gateway.oms().totals().at(0);
  std::cout << "{\"schema\":1,\"event\":" << json(event)
            << ",\"utc\":" << json(sim::utc())
            << ",\"logged_on\":" << (h.logged_on ? "true" : "false")
            << ",\"oms_next_send\":" << h.oms_send
            << ",\"oms_next_receive\":" << h.oms_recv
            << ",\"venue_next_send\":" << h.venue_send
            << ",\"venue_next_receive\":" << h.venue_recv
            << ",\"reports\":" << row[0] << ",\"fills\":" << row[1]
            << ",\"filled_shares\":" << row[2] << ",\"pending\":" << row[3]
            << ",\"duplicates\":" << h.duplicates
            << ",\"invalid_frames\":" << h.invalid_frames
            << ",\"protocol_rejects\":" << h.rejects
            << ",\"resend_requests\":" << h.resends
            << ",\"gap_fills\":" << h.gap_fills
            << ",\"endpoint_logons\":" << h.logons
            << ",\"endpoint_logouts\":" << h.logouts << "}\n";
}
void command(sim::Gateway &g, const sim::Strings &words) {
  if (words.empty())
    return;
  if (words.size() == 1 && words[0] == "reconnect") {
    g.disconnect();
    g.connect();
    status(g, "reconnected");
    return;
  }
  if (words.size() == 1 && words[0] == "status") {
    status(g, "status");
    return;
  }
  auto c = sim::Command::parse(words);
  const auto start = std::chrono::steady_clock::now();
  g.submit(c);
  auto us = std::chrono::duration_cast<std::chrono::microseconds>(
                std::chrono::steady_clock::now() - start)
                .count();
  for (const auto &item : g.oms().results(c.id)) {
    auto r = sim::Report::decode(sim::Msg(item[0], false));
    std::cout << "{\"schema\":1,\"event\":\"order_result\",\"cl_ord_id\":"
              << json(r.id) << ",\"event_id\":" << json(r.event)
              << ",\"order_id\":" << json(r.order)
              << ",\"kind\":" << json(r.kind)
              << ",\"ord_status\":" << json(r.status)
              << ",\"last_shares\":" << r.last
              << ",\"last_price_usd_cents\":" << r.last_price
              << ",\"event_utc\":" << json(r.time)
              << ",\"available_utc\":" << json(item[1])
              << ",\"reason\":" << json(r.reason) << "}\n";
  }
  std::cout << "{\"schema\":1,\"event\":\"command_processed\",\"cl_ord_id\":"
            << json(c.id) << ",\"elapsed_us\":" << us << "}\n";
}
} // namespace
int main(int argc, char **argv) {
  umask(0077);
  if (argc < 3) {
    std::cerr << "usage: fix-gateway STATE (run COMMAND_FILE | status | new "
                 "... | replace ... | cancel ...)\n";
    return 2;
  }
  try {
    sim::Gateway g(argv[1]);
    g.connect();
    if (std::string(argv[2]) == "run") {
      if (argc != 4)
        throw std::invalid_argument("run requires one batch file");
      std::ifstream in(argv[3]);
      if (!in)
        throw std::runtime_error("cannot read command file");
      std::string line;
      while (std::getline(in, line)) {
        if (line.empty() || line[0] == '#')
          continue;
        std::istringstream stream(line);
        sim::Strings words;
        for (std::string word; stream >> word;)
          words.push_back(word);
        g.tick();
        command(g, words);
      }
      if (in.bad())
        throw std::runtime_error("batch read failed");
    } else
      command(g, sim::Strings(argv + 2, argv + argc));
    g.reconcile();
    status(g, "reconciled");
    g.logout();
    status(g, "logout");
  } catch (const std::exception &e) {
    std::cerr << "{\"schema\":1,\"event\":\"halted\",\"reason\":"
              << json(e.what()) << "}\n";
    return 1;
  }
}
