#include "domain.hpp"
#include <algorithm>
#include <charconv>
#include <stdexcept>

namespace sim {
std::string utc() {
  return FIX::UtcTimeStampConvertor::convert(FIX::UtcTimeStamp::now(), 3);
}
std::string type(const Msg &m) { return m.getHeader().getField(35); }

std::string body(const Msg &m) {
  FIX44::Message clean{FIX::MsgType(type(m))};
  static_cast<FIX::FieldMap &>(clean) = static_cast<const FIX::FieldMap &>(m);
  return clean.toString();
}
int integer(const std::string &s, int tag) {
  if (s.empty() || s.find_first_not_of("0123456789") != s.npos)
    throw FIX::IncorrectTagValue(tag);
  int n = 0;
  auto result = std::from_chars(s.data(), s.data() + s.size(), n);
  if (result.ec != std::errc{} || result.ptr != s.data() + s.size() || n < 0 ||
      n > 100000000)
    throw FIX::IncorrectTagValue(tag);
  return n;
}
std::string money(int n) {
  return std::to_string(n / 100) + "." + (n % 100 < 10 ? "0" : "") +
         std::to_string(n % 100);
}
int cents(const std::string &s, int tag) {
  auto pos = s.find('.');
  std::string whole = s.substr(0, pos);
  std::string frac = pos == s.npos ? "" : s.substr(pos + 1);
  if (whole.empty() || frac.size() > 2 || (pos != s.npos && frac.empty()))
    throw FIX::IncorrectTagValue(tag);
  frac.resize(2, '0');
  return integer(whole + frac, tag);
}
static void check_id(const std::string &s, int tag) {
  if (s.empty() || s.size() > 40 ||
      s.find_first_not_of(
          "ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz0123456789_-") !=
          s.npos)
    throw FIX::IncorrectTagValue(tag);
}
void Command::validate() const {
  if (op != "D" && op != "F" && op != "G")
    throw FIX::UnsupportedMessageType();
  check_id(id, 11);
  if (op == "D" && !orig.empty())
    throw FIX::IncorrectTagValue(41);
  if (op == "F" && price != 0)
    throw FIX::IncorrectTagValue(44);
  if (op != "D") {
    check_id(orig, 41);
    if (orig == id)
      throw FIX::IncorrectTagValue(41);
  }
  if (side != "1" && side != "2")
    throw FIX::IncorrectTagValue(54);
  if (qty < 1 || qty > 1000000)
    throw FIX::IncorrectTagValue(38);
  if (op != "F" && (price < 1 || price > 100000000))
    throw FIX::IncorrectTagValue(44);
}
std::string Command::signature() const {
  return op + " " + id + " " + orig + " " + side + " " + std::to_string(qty) +
         " " + std::to_string(price);
}
Msg Command::encode() const {
  validate();
  FIX44::Message m{FIX::MsgType(op)};
  m.setField(11, id);
  m.setField(55, "SYNTH");
  m.setField(54, side);
  m.setField(60, utc());
  m.setField(38, std::to_string(qty));
  if (op != "D")
    m.setField(41, orig);
  if (op != "F") {
    m.setField(40, "2");
    m.setField(44, money(price));
    m.setField(15, "USD");
    m.setField(59, "1");
  }
  return m;
}
Command Command::decode(const Msg &m) {
  Command c;
  c.op = type(m);
  if (c.op != "D" && c.op != "F" && c.op != "G")
    throw FIX::UnsupportedMessageType();
  const std::vector<int> allowed =
      c.op == "F"
          ? std::vector<int>{11, 41, 55, 54, 60, 38}
          : (c.op == "D"
                 ? std::vector<int>{11, 55, 54, 60, 38, 40, 44, 15, 59}
                 : std::vector<int>{11, 41, 55, 54, 60, 38, 40, 44, 15, 59});
  for (const auto &field : m) {
    if (std::find(allowed.begin(), allowed.end(), field.getTag()) ==
        allowed.end())
      throw FIX::TagNotDefinedForMessage(field.getTag());
  }
  c.id = m.getField(11);
  c.side = m.getField(54);
  if (m.getField(55) != "SYNTH")
    throw FIX::IncorrectTagValue(55);
  if (c.op != "D")
    c.orig = m.getField(41);
  c.qty = integer(m.getField(38), 38);
  if (c.op != "F") {
    if (m.getField(40) != "2")
      throw FIX::IncorrectTagValue(40);
    if (m.isSetField(15) && m.getField(15) != "USD")
      throw FIX::IncorrectTagValue(15);
    if (m.getField(59) != "1")
      throw FIX::IncorrectTagValue(59);
    c.price = cents(m.getField(44), 44);
  }
  c.validate();
  return c;
}
Command Command::parse(const Strings &a) {
  Command c;
  if (a.empty())
    throw std::invalid_argument("missing command");
  if (a[0] == "new" && a.size() == 6)
    c.op = "D";
  else if (a[0] == "replace" && a.size() == 7)
    c.op = "G";
  else if (a[0] == "cancel" && a.size() == 6)
    c.op = "F";
  else
    throw std::invalid_argument("invalid command or argument count");
  size_t i = 1;
  c.id = a[i++];
  if (c.op != "D")
    c.orig = a[i++];
  if (a[i++] != "SYNTH")
    throw FIX::IncorrectTagValue(55);
  const auto &side = a[i++];
  c.side = side == "buy" ? "1" : side == "sell" ? "2" : "";
  c.qty = integer(a[i++], 38);
  if (c.op != "F")
    c.price = integer(a[i], 44);
  c.validate();
  return c;
}
bool Report::rejected() const {
  return kind == "cancel_reject" || kind == "replace_reject";
}
Msg Report::encode() const {
  FIX44::Message m{FIX::MsgType(rejected() ? "9" : "8")};
  m.setField(11, id);
  m.setField(37, order);
  m.setField(39, status);
  m.setField(60, time);
  if (!orig.empty())
    m.setField(41, orig);
  if (rejected()) {
    m.setField(434, kind == "cancel_reject" ? "1" : "2");
    m.setField(102, order == "NONE" ? "1" : "0");
    m.setField(58, reason);
    return m;
  }
  m.setField(17, event);
  m.setField(150, kind);
  m.setField(54, side);
  m.setField(55, "SYNTH");
  m.setField(15, "USD");
  m.setField(38, std::to_string(qty));
  m.setField(44, money(limit));
  m.setField(14, std::to_string(cum));
  m.setField(151, std::to_string(leaves));
  m.setField(6, money(last_price));
  if (kind == "F") {
    m.setField(32, std::to_string(last));
    m.setField(31, money(last_price));
  }
  return m;
}
Report Report::decode(const Msg &m) {
  Report r;
  auto t = type(m);
  if (t != "8" && t != "9")
    throw FIX::UnsupportedMessageType();
  r.id = m.getField(11);
  r.order = m.getField(37);
  r.status = m.getField(39);
  r.time = m.getField(60);
  if (m.isSetField(41))
    r.orig = m.getField(41);
  if (t == "9") {
    auto response = m.getField(434);
    if (response != "1" && response != "2")
      throw FIX::IncorrectTagValue(434);
    r.kind = response == "1" ? "cancel_reject" : "replace_reject";
    r.event = "R/" + r.id;
    r.reason = m.getField(58);
    return r;
  }
  r.kind = m.getField(150);
  r.event = m.getField(17);
  r.side = m.getField(54);
  if (m.getField(55) != "SYNTH")
    throw FIX::IncorrectTagValue(55);
  if (m.getField(15) != "USD")
    throw FIX::IncorrectTagValue(15);
  r.qty = integer(m.getField(38), 38);
  r.limit = cents(m.getField(44), 44);
  r.cum = integer(m.getField(14), 14);
  r.leaves = integer(m.getField(151), 151);
  if (r.kind == "F") {
    r.last = integer(m.getField(32), 32);
    r.last_price = cents(m.getField(31), 31);
    if (r.status != "2" || r.last != r.qty || r.cum != r.qty || r.leaves != 0 ||
        r.last_price <= 0)
      throw FIX::IncorrectTagValue(32);
  } else {
    if (r.kind != "0" && r.kind != "5" && r.kind != "4")
      throw FIX::IncorrectTagValue(150);
    if (r.status != (r.kind == "4" ? "4" : "0") || r.cum != 0 ||
        r.leaves != (r.kind == "4" ? 0 : r.qty))
      throw FIX::IncorrectTagValue(39);
  }
  if (cents(m.getField(6), 6) != r.last_price)
    throw FIX::IncorrectTagValue(6);
  return r;
}
} // namespace sim
