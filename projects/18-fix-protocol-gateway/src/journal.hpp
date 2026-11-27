#pragma once
#include "domain.hpp"
#include <filesystem>
#include <sqlite3.h>

namespace sim {
using Rows = std::vector<Strings>;

// Each counterparty owns an independent database and session store.
class Journal {
public:
  explicit Journal(const std::filesystem::path &);
  ~Journal();
  Journal(const Journal &) = delete;
  Journal &operator=(const Journal &) = delete;
  bool stage(const Command &, const Msg &);
  void execute(const Command &, const Msg &);
  bool receive(const Msg &);
  Rows pending() const;
  Rows outbox() const;
  void published(const std::string &);
  Rows snapshot() const;
  Rows totals() const;
  Rows results(const std::string &request_id) const;

private:
  sqlite3 *db{};
  Rows sql(const char *, const Strings &args = {}) const;
  void insert(const Report &, const Msg &);
  Report decision(const Command &) const;
  class Transaction {
    Journal &journal;
    bool committed{};

  public:
    explicit Transaction(Journal &);
    void commit();
    ~Transaction();
  };
};
} // namespace sim
