package identity

import (
	"strings"
	"sync"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
)

// Mapper normalizes symbols, currencies, timestamps (UTC), and maintains
// cross-system links between client_order_id, broker_order_id, and broker_exec_id.
type Mapper struct {
	mu             sync.RWMutex
	symbolAliases  map[string]string
	clientToBroker map[string]string
	brokerToClient map[string]string
	execToClient   map[string]string
	execToBroker   map[string]string
}

// NewMapper constructs an identity mapper with optional symbol alias overrides.
func NewMapper(aliases map[string]string) *Mapper {
	m := &Mapper{
		symbolAliases:  make(map[string]string),
		clientToBroker: make(map[string]string),
		brokerToClient: make(map[string]string),
		execToClient:   make(map[string]string),
		execToBroker:   make(map[string]string),
	}
	for k, v := range aliases {
		m.symbolAliases[strings.ToUpper(strings.TrimSpace(k))] = strings.ToUpper(strings.TrimSpace(v))
	}
	return m
}

// NormalizeSymbol canonicalizes broker/internal symbol formatting:
// trims whitespace, uppercases, strips common exchange suffixes (.US, :US, /US),
// maps dash-class tickers (BRK-B -> BRK.B), and applies configured aliases.
func (m *Mapper) NormalizeSymbol(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return ""
	}
	m.mu.RLock()
	if alias, ok := m.symbolAliases[s]; ok {
		m.mu.RUnlock()
		return alias
	}
	m.mu.RUnlock()

	for _, suffix := range []string{".US", ":US", "/US", ".NASDAQ", ".NYSE"} {
		if strings.HasSuffix(s, suffix) {
			s = strings.TrimSuffix(s, suffix)
			break
		}
	}
	// Normalize dash share-class notation (e.g. BRK-B -> BRK.B)
	if idx := strings.LastIndex(s, "-"); idx > 0 && idx == len(s)-2 {
		s = s[:idx] + "." + s[idx+1:]
	}

	m.mu.RLock()
	if alias, ok := m.symbolAliases[s]; ok {
		m.mu.RUnlock()
		return alias
	}
	m.mu.RUnlock()
	return s
}

// NormalizeCurrency canonicalizes ISO 4217 currency codes.
func (m *Mapper) NormalizeCurrency(raw string) string {
	s := strings.ToUpper(strings.TrimSpace(raw))
	if s == "" {
		return "USD"
	}
	return s
}

// NormalizeSide canonicalizes BUY/SELL side strings.
func (m *Mapper) NormalizeSide(raw domain.Side) domain.Side {
	switch strings.ToUpper(strings.TrimSpace(string(raw))) {
	case "B", "BUY", "LONG":
		return domain.SideBuy
	case "S", "SELL", "SHORT":
		return domain.SideSell
	default:
		return domain.Side(strings.ToUpper(strings.TrimSpace(string(raw))))
	}
}

// NormalizeUTC forces timestamps into UTC. If receiveTime is zero, falls back to eventTime.
func NormalizeUTC(eventTime, receiveTime time.Time) (time.Time, time.Time) {
	ev := eventTime.UTC()
	rc := receiveTime.UTC()
	if rc.IsZero() {
		rc = ev
	}
	return ev, rc
}

// RegisterOrderLink records a verified link between client_order_id and broker_order_id.
func (m *Mapper) RegisterOrderLink(clientOrderID, brokerOrderID string) {
	cID := strings.TrimSpace(clientOrderID)
	bID := strings.TrimSpace(brokerOrderID)
	if cID == "" || bID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	m.clientToBroker[cID] = bID
	m.brokerToClient[bID] = cID
}

// RegisterExecLink records execution-to-order linkage.
func (m *Mapper) RegisterExecLink(brokerExecID, clientOrderID, brokerOrderID string) {
	eID := strings.TrimSpace(brokerExecID)
	cID := strings.TrimSpace(clientOrderID)
	bID := strings.TrimSpace(brokerOrderID)
	if eID == "" {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if cID != "" {
		m.execToClient[eID] = cID
	}
	if bID != "" {
		m.execToBroker[eID] = bID
	}
	if cID != "" && bID != "" {
		m.clientToBroker[cID] = bID
		m.brokerToClient[bID] = cID
	}
}

// BrokerOrderForClient returns the mapped broker_order_id if known.
func (m *Mapper) BrokerOrderForClient(clientOrderID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.clientToBroker[strings.TrimSpace(clientOrderID)]
	return v, ok
}

// ClientOrderForBroker returns the mapped client_order_id if known.
func (m *Mapper) ClientOrderForBroker(brokerOrderID string) (string, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.brokerToClient[strings.TrimSpace(brokerOrderID)]
	return v, ok
}

// NormalizeLedgerOrder returns a normalized copy and indexes any ID links.
func (m *Mapper) NormalizeLedgerOrder(o domain.LedgerOrder) domain.LedgerOrder {
	out := o
	out.ID = strings.TrimSpace(o.ID)
	out.AccountID = strings.TrimSpace(o.AccountID)
	out.ClientOrderID = strings.TrimSpace(o.ClientOrderID)
	out.BrokerOrderID = strings.TrimSpace(o.BrokerOrderID)
	out.Symbol = m.NormalizeSymbol(o.Symbol)
	out.Currency = m.NormalizeCurrency(o.Currency)
	out.Side = m.NormalizeSide(o.Side)
	out.EventTime, out.ReceiveTime = NormalizeUTC(o.EventTime, o.ReceiveTime)
	m.RegisterOrderLink(out.ClientOrderID, out.BrokerOrderID)
	return out
}

// NormalizeBrokerOrder returns a normalized copy and indexes any ID links.
func (m *Mapper) NormalizeBrokerOrder(o domain.BrokerOrder) domain.BrokerOrder {
	out := o
	out.BrokerOrderID = strings.TrimSpace(o.BrokerOrderID)
	out.AccountID = strings.TrimSpace(o.AccountID)
	out.ClientOrderID = strings.TrimSpace(o.ClientOrderID)
	out.Symbol = m.NormalizeSymbol(o.Symbol)
	out.Currency = m.NormalizeCurrency(o.Currency)
	out.Side = m.NormalizeSide(o.Side)
	out.EventTime, out.ReceiveTime = NormalizeUTC(o.EventTime, o.ReceiveTime)
	m.RegisterOrderLink(out.ClientOrderID, out.BrokerOrderID)
	return out
}

// NormalizeLedgerFill returns a normalized copy and indexes execution links.
func (m *Mapper) NormalizeLedgerFill(f domain.LedgerFill) domain.LedgerFill {
	out := f
	out.ID = strings.TrimSpace(f.ID)
	out.AccountID = strings.TrimSpace(f.AccountID)
	out.ClientOrderID = strings.TrimSpace(f.ClientOrderID)
	out.BrokerOrderID = strings.TrimSpace(f.BrokerOrderID)
	out.BrokerExecID = strings.TrimSpace(f.BrokerExecID)
	out.Symbol = m.NormalizeSymbol(f.Symbol)
	out.Currency = m.NormalizeCurrency(f.Currency)
	out.Side = m.NormalizeSide(f.Side)
	out.EventTime, out.ReceiveTime = NormalizeUTC(f.EventTime, f.ReceiveTime)
	m.RegisterExecLink(out.BrokerExecID, out.ClientOrderID, out.BrokerOrderID)
	return out
}

// NormalizeBrokerFill returns a normalized copy and indexes execution links.
func (m *Mapper) NormalizeBrokerFill(f domain.BrokerFill) domain.BrokerFill {
	out := f
	out.BrokerExecID = strings.TrimSpace(f.BrokerExecID)
	out.BrokerOrderID = strings.TrimSpace(f.BrokerOrderID)
	out.ClientOrderID = strings.TrimSpace(f.ClientOrderID)
	out.AccountID = strings.TrimSpace(f.AccountID)
	out.Symbol = m.NormalizeSymbol(f.Symbol)
	out.Currency = m.NormalizeCurrency(f.Currency)
	out.Side = m.NormalizeSide(f.Side)
	out.EventTime, out.ReceiveTime = NormalizeUTC(f.EventTime, f.ReceiveTime)
	m.RegisterExecLink(out.BrokerExecID, out.ClientOrderID, out.BrokerOrderID)
	return out
}

// NormalizePosition returns a normalized copy of a PositionSnapshot.
func (m *Mapper) NormalizePosition(p domain.PositionSnapshot) domain.PositionSnapshot {
	out := p
	out.AccountID = strings.TrimSpace(p.AccountID)
	out.Symbol = m.NormalizeSymbol(p.Symbol)
	out.Currency = m.NormalizeCurrency(p.Currency)
	out.AsOf, out.ReceiveTime = NormalizeUTC(p.AsOf, p.ReceiveTime)
	return out
}

// NormalizeCash returns a normalized copy of a CashSnapshot.
func (m *Mapper) NormalizeCash(c domain.CashSnapshot) domain.CashSnapshot {
	out := c
	out.AccountID = strings.TrimSpace(c.AccountID)
	out.Currency = m.NormalizeCurrency(c.Currency)
	out.AsOf, out.ReceiveTime = NormalizeUTC(c.AsOf, c.ReceiveTime)
	return out
}
