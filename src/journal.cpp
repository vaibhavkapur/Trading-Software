#include "journal.hpp"
#include <memory>

namespace sim {
Rows Journal::sql(const char *query, const Strings &args) const {
  sqlite3_stmt *raw = nullptr;
  if (sqlite3_prepare_v2(db, query, -1, &raw, nullptr) != SQLITE_OK)
    throw std::runtime_error(sqlite3_errmsg(db));
  std::unique_ptr<sqlite3_stmt, decltype(&sqlite3_finalize)> stmt(
      raw, sqlite3_finalize);
  int column = 1;
  for (const auto &arg : args) {
    if (sqlite3_bind_text(raw, column++, arg.data(),
                          static_cast<int>(arg.size()),
                          SQLITE_TRANSIENT) != SQLITE_OK)
      throw std::runtime_error(sqlite3_errmsg(db));
  }
  Rows rows;
  int rc;
  while ((rc = sqlite3_step(raw)) == SQLITE_ROW) {
    Strings row;
    for (int i = 0; i < sqlite3_column_count(raw); ++i) {
      auto *p = sqlite3_column_text(raw, i);
      row.emplace_back(p ? reinterpret_cast<const char *>(p) : "");
    }
    rows.push_back(std::move(row));
  }
  if (rc != SQLITE_DONE)
    throw std::runtime_error(sqlite3_errmsg(db));
  return rows;
}

Journal::Journal(const std::filesystem::path &file) {
  if (sqlite3_open(file.c_str(), &db) != SQLITE_OK) {
    std::string reason = sqlite3_errmsg(db);
    sqlite3_close(db);
    throw std::runtime_error(reason);
  }
  try {
    sql("PRAGMA synchronous=FULL");
    sql("PRAGMA foreign_keys=ON");
    Transaction tx(*this);
    auto v = sql("PRAGMA user_version")[0][0];
    if (v != "0" && v != "1")
      throw std::runtime_error("unsupported journal schema");
    sql("CREATE TABLE IF NOT EXISTS requests(id TEXT PRIMARY KEY, signature "
        "TEXT NOT NULL, wire TEXT NOT NULL,"
        " done INTEGER NOT NULL DEFAULT 0, created_utc TEXT NOT NULL)");
    sql("CREATE TABLE IF NOT EXISTS events(event_id TEXT PRIMARY KEY, "
        "request_id TEXT NOT NULL REFERENCES requests(id),"
        " order_id TEXT NOT NULL, kind TEXT NOT NULL, status TEXT NOT NULL, "
        "last_qty INTEGER NOT NULL,"
        " last_cents INTEGER NOT NULL, event_utc TEXT NOT NULL, available_utc "
        "TEXT NOT NULL,"
        " wire TEXT NOT NULL, published INTEGER NOT NULL DEFAULT 0)");
    sql("CREATE INDEX IF NOT EXISTS request_events ON events(request_id)");
    sql("CREATE INDEX IF NOT EXISTS order_events ON events(order_id)");
    sql("CREATE INDEX IF NOT EXISTS delivery ON events(published)");
    sql("PRAGMA user_version=1");
    tx.commit();
  } catch (...) {
    sqlite3_close(db);
    throw;
  }
}
Journal::~Journal() { sqlite3_close(db); }
Journal::Transaction::Transaction(Journal &j) : journal(j) {
  journal.sql("BEGIN IMMEDIATE");
}
void Journal::Transaction::commit() {
  journal.sql("COMMIT");
  committed = true;
}
Journal::Transaction::~Transaction() {
  if (!committed)
    sqlite3_exec(journal.db, "ROLLBACK", nullptr, nullptr, nullptr);
}
bool Journal::stage(const Command &c, const Msg &m) {
  auto old = sql("SELECT signature,done FROM requests WHERE id=?", {c.id});
  if (!old.empty()) {
    if (old[0][0] != c.signature())
      throw FIX::IncorrectTagValue(11, "conflicting ClOrdID");
    return old[0][1] == "0";
  }
  sql("INSERT INTO requests(id,signature,wire,created_utc) VALUES(?,?,?,?)",
      {c.id, c.signature(), body(m), utc()});
  return true;
}
void Journal::insert(const Report &r, const Msg &m) {
  sql("INSERT INTO "
      "events(event_id,request_id,order_id,kind,status,last_qty,last_cents,"
      "event_utc,available_utc,wire)"
      " VALUES(?,?,?,?,?,?,?,?,?,?)",
      {r.event, r.id, r.order, r.kind, r.status, std::to_string(r.last),
       std::to_string(r.last_price), r.time, utc(), body(m)});
}
Report Journal::decision(const Command &c) const {
  Report r;
  r.event = c.id + "/ack";
  r.id = c.id;
  r.orig = c.orig;
  r.side = c.side;
  r.time = utc();
  r.order = "O/" + c.id;
  r.qty = c.qty;
  r.limit = c.price;
  r.status = "0";
  r.kind = "0";
  r.leaves = c.qty;
  if (c.op == "D")
    return r;
  auto chain = sql("SELECT order_id FROM events WHERE request_id=? AND kind IN "
                   "('0','5','4','F') LIMIT 1",
                   {c.orig});
  r.order = chain.empty() ? "NONE" : chain[0][0];
  r.status = "8";
  r.reason = "unknown original order";
  if (!chain.empty()) {
    auto row = sql("SELECT wire FROM events WHERE order_id=? AND kind IN "
                   "('0','5','4','F') ORDER BY rowid DESC LIMIT 1",
                   {r.order});
    auto last = Report::decode(Msg(row[0][0], false));
    r.status = last.status;
    if (last.id == c.orig && last.status == "0" && last.side == c.side &&
        (c.op != "F" || c.qty == last.qty)) {
      if (c.op == "F") {
        r.kind = "4";
        r.status = "4";
        r.limit = last.limit;
        r.leaves = 0;
      } else
        r.kind = "5";
      r.reason.clear();
      return r;
    }
    r.reason = "order is closed, stale or mismatched";
  }
  r.kind = c.op == "F" ? "cancel_reject" : "replace_reject";
  r.event = "R/" + c.id;
  return r;
}
void Journal::execute(const Command &c, const Msg &m) {
  Transaction tx(*this);
  if (!stage(c, m)) {
    // A repeated application request republishes its original result.
    sql("UPDATE events SET published=0 WHERE request_id=?", {c.id});
    tx.commit();
    return;
  }
  auto r = decision(c);
  insert(r, r.encode());
  if (!r.rejected() && r.kind != "4" &&
      (c.side == "1" ? c.price >= quote_cents : c.price <= quote_cents)) {
    r.event = c.id + "/trade";
    r.kind = "F";
    r.status = "2";
    r.cum = r.last = c.qty;
    r.leaves = 0;
    r.last_price = quote_cents;
    insert(r, r.encode());
  }
  sql("UPDATE requests SET done=1 WHERE id=?", {c.id});
  tx.commit();
}
bool Journal::receive(const Msg &m) {
  auto r = Report::decode(m);
  Transaction tx(*this);
  auto prev = sql("SELECT wire FROM events WHERE event_id=?", {r.event});
  if (!prev.empty()) {
    if (prev[0][0] != body(m))
      throw std::runtime_error("conflicting execution identity");
    tx.commit();
    return false;
  }
  auto req = sql("SELECT wire FROM requests WHERE id=?", {r.id});
  if (req.empty())
    throw std::runtime_error("execution for unjournaled request");
  auto c = Command::decode(Msg(req[0][0], false));
  if (r.orig != c.orig)
    throw std::runtime_error("original request mismatch");
  if (r.rejected()) {
    if (r.kind != (c.op == "G" ? "replace_reject" : "cancel_reject") ||
        c.op == "D")
      throw std::runtime_error("cancel reject request mismatch");
  } else {
    if (r.qty != c.qty || r.side != c.side ||
        (c.op != "F" && r.limit != c.price))
      throw std::runtime_error("execution terms mismatch");
    auto previous = sql("SELECT request_id,kind,status FROM events WHERE "
                        "order_id=? AND kind IN ('0','5','4','F')"
                        " ORDER BY rowid DESC LIMIT 1",
                        {r.order});
    if (r.kind == "0") {
      if (c.op != "D" || !previous.empty())
        throw std::runtime_error("unexpected new acknowledgment");
    } else {
      bool fill = r.kind == "F";
      bool correct_op =
          fill ? c.op != "F" : (r.kind == "4" ? c.op == "F" : c.op == "G");
      if (!correct_op || previous.empty() ||
          previous[0][0] != (fill ? r.id : r.orig) || previous[0][2] != "0")
        throw std::runtime_error("invalid order state transition");
      if (fill &&
          (c.side == "1" ? r.last_price > c.price : r.last_price < c.price))
        throw std::runtime_error("trade outside order limit");
    }
  }
  insert(r, m);
  sql("UPDATE requests SET done=1 WHERE id=?", {r.id});
  tx.commit();
  return true;
}
Rows Journal::pending() const {
  return sql("SELECT wire FROM requests WHERE done=0 ORDER BY rowid");
}
Rows Journal::outbox() const {
  return sql(
      "SELECT event_id,wire FROM events WHERE published=0 ORDER BY rowid");
}
void Journal::published(const std::string &event) {
  sql("UPDATE events SET published=1 WHERE event_id=?", {event});
}
Rows Journal::snapshot() const {
  return sql("SELECT event_id,wire FROM events ORDER BY event_id");
}
Rows Journal::results(const std::string &id) const {
  return sql(
      "SELECT wire,available_utc FROM events WHERE request_id=? ORDER BY rowid",
      {id});
}
Rows Journal::totals() const {
  return sql(
      "SELECT count(*),COALESCE(sum(kind='F'),0),COALESCE(sum(last_qty),0),"
      "(SELECT count(*) FROM requests WHERE done=0) FROM events");
}
} // namespace sim
