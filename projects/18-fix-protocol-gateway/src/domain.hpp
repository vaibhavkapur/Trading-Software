#pragma once
#include <quickfix/fix44/Message.h>
#include <string>
#include <vector>

namespace sim {
using Msg = FIX::Message;
using Strings = std::vector<std::string>;
constexpr int quote_cents = 10000;
std::string utc();
std::string type(const Msg &);
std::string body(const Msg &);
int integer(const std::string &, int tag);
std::string money(int cents);
int cents(const std::string &, int tag);

struct Command {
  std::string op, id, orig, side;
  int qty{}, price{};
  void validate() const;
  std::string signature() const;
  Msg encode() const;
  static Command decode(const Msg &);
  static Command parse(const Strings &);
};

struct Report {
  std::string event, id, orig, order, kind, status, side, time, reason;
  int qty{}, limit{}, cum{}, leaves{}, last{}, last_price{};
  bool rejected() const;
  Msg encode() const;
  static Report decode(const Msg &);
};
} // namespace sim
