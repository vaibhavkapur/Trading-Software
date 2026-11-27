package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
)

// InstrumentSpec defines a canonical tradable instrument and its per-broker symbol mappings.
type InstrumentSpec struct {
	CanonicalID   string            `json:"canonical_id"`
	AssetClass    AssetClass        `json:"asset_class"`
	Currency      string            `json:"currency"`
	TickSize      string            `json:"tick_size"`
	LotSize       string            `json:"lot_size"`
	MinQty        string            `json:"min_qty"`
	BrokerSymbols map[string]string `json:"broker_symbols"`
}

// InstrumentManifest is the versioned catalog loaded from config/instruments.json.
type InstrumentManifest struct {
	SchemaVersion string           `json:"schema_version"`
	UpdatedAtUTC  string           `json:"updated_at_utc"`
	Instruments   []InstrumentSpec `json:"instruments"`
}

// InstrumentMapper resolves canonical instrument IDs to broker-native symbols and back.
type InstrumentMapper struct {
	SchemaVersion string
	byCanonical   map[string]InstrumentSpec
	byBrokerSym   map[string]map[string]string // brokerID -> brokerSymbol -> canonicalID
}

// NewInstrumentMapper constructs an indexed bidirectional instrument mapper.
func NewInstrumentMapper(m InstrumentManifest) *InstrumentMapper {
	byCanon := make(map[string]InstrumentSpec, len(m.Instruments))
	byBroker := make(map[string]map[string]string)
	for _, inst := range m.Instruments {
		byCanon[inst.CanonicalID] = inst
		for brokerID, sym := range inst.BrokerSymbols {
			if byBroker[brokerID] == nil {
				byBroker[brokerID] = make(map[string]string)
			}
			byBroker[brokerID][sym] = inst.CanonicalID
		}
	}
	return &InstrumentMapper{
		SchemaVersion: m.SchemaVersion,
		byCanonical:   byCanon,
		byBrokerSym:   byBroker,
	}
}

// LoadInstrumentMapper loads config/instruments.json.
func LoadInstrumentMapper(path string) (*InstrumentMapper, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read instrument manifest: %w", err)
	}
	var m InstrumentManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse instrument manifest: %w", err)
	}
	return NewInstrumentMapper(m), nil
}

// ResolveToBroker validates currency, minimum quantity, lot/tick increments, and resolves the broker-native symbol.
func (im *InstrumentMapper) ResolveToBroker(brokerID string, req OrderRequest) (InstrumentSpec, string, error) {
	inst, ok := im.byCanonical[req.InstrumentID]
	if !ok {
		return InstrumentSpec{}, "", NewError(
			ErrCodeSymbolMismatch,
			fmt.Sprintf("unknown canonical instrument %q", req.InstrumentID),
			false,
		).WithContext(brokerID, "")
	}
	sym, ok := inst.BrokerSymbols[brokerID]
	if !ok || strings.TrimSpace(sym) == "" {
		return InstrumentSpec{}, "", NewError(
			ErrCodeSymbolMismatch,
			fmt.Sprintf("instrument %q is not mapped for broker %q", req.InstrumentID, brokerID),
			false,
		).WithContext(brokerID, "")
	}
	if !strings.EqualFold(inst.Currency, req.Currency) {
		return InstrumentSpec{}, "", NewError(
			ErrCodeSymbolMismatch,
			fmt.Sprintf("currency mismatch for %q: expected %s, got %s", req.InstrumentID, inst.Currency, req.Currency),
			false,
		).WithContext(brokerID, "")
	}
	if minQty, err := strconv.ParseFloat(inst.MinQty, 64); err == nil && minQty > 0 {
		if req.Quantity+1e-9 < minQty {
			return InstrumentSpec{}, "", NewError(
				ErrCodeValidation,
				fmt.Sprintf("quantity %.6f below min_qty %.6f for %q", req.Quantity, minQty, req.InstrumentID),
				false,
			).WithContext(brokerID, "")
		}
	}
	if lotSize, err := strconv.ParseFloat(inst.LotSize, 64); err == nil && lotSize > 0 {
		rem := math.Mod(req.Quantity, lotSize)
		if rem > 1e-6 && math.Abs(rem-lotSize) > 1e-6 {
			return InstrumentSpec{}, "", NewError(
				ErrCodeValidation,
				fmt.Sprintf("quantity %.6f is not a multiple of lot_size %.6f for %q", req.Quantity, lotSize, req.InstrumentID),
				false,
			).WithContext(brokerID, "")
		}
	}
	return inst, sym, nil
}

// ResolveFromBroker maps a broker-native symbol back to a canonical instrument ID.
func (im *InstrumentMapper) ResolveFromBroker(brokerID, brokerSymbol string) (string, bool) {
	m, ok := im.byBrokerSym[brokerID]
	if !ok {
		return "", false
	}
	cid, ok := m[brokerSymbol]
	return cid, ok
}

// All returns all canonical instrument specs.
func (im *InstrumentMapper) All() []InstrumentSpec {
	out := make([]InstrumentSpec, 0, len(im.byCanonical))
	for _, s := range im.byCanonical {
		out = append(out, s)
	}
	return out
}
