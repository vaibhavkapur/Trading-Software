package domain

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
)

// BrokerCapabilities declares the versioned capability contract of a broker adapter.
type BrokerCapabilities struct {
	BrokerID              string          `json:"broker_id"`
	DisplayName           string          `json:"display_name"`
	AllowedModes          []ExecutionMode `json:"allowed_modes"`
	SupportedAssetClasses []AssetClass    `json:"supported_asset_classes"`
	SupportedOrderTypes   []OrderType     `json:"supported_order_types"`
	SupportedTimeInForce  []TimeInForce   `json:"supported_time_in_force"`
	SupportsFractionalQty bool            `json:"supports_fractional_qty"`
	SupportsReplace       bool            `json:"supports_replace"`
	SupportsWebhooks      bool            `json:"supports_webhooks"`
	SupportsEventReplay   bool            `json:"supports_event_replay"`
	MaxNotionalUSD        float64         `json:"max_notional_usd"`
}

// CapabilityManifest is the versioned capability registry loaded from config.
type CapabilityManifest struct {
	SchemaVersion string               `json:"schema_version"`
	UpdatedAtUTC  string               `json:"updated_at_utc"`
	Brokers       []BrokerCapabilities `json:"brokers"`
}

// CapabilityMatrix validates orders against broker capabilities prior to dispatch.
type CapabilityMatrix struct {
	SchemaVersion string
	byBroker      map[string]BrokerCapabilities
}

// NewCapabilityMatrix builds an indexed capability matrix.
func NewCapabilityMatrix(m CapabilityManifest) *CapabilityMatrix {
	idx := make(map[string]BrokerCapabilities, len(m.Brokers))
	for _, b := range m.Brokers {
		idx[b.BrokerID] = b
	}
	return &CapabilityMatrix{
		SchemaVersion: m.SchemaVersion,
		byBroker:      idx,
	}
}

// LoadCapabilityMatrix reads and parses config/capabilities.json.
func LoadCapabilityMatrix(path string) (*CapabilityMatrix, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read capabilities manifest: %w", err)
	}
	var m CapabilityManifest
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, fmt.Errorf("parse capabilities manifest: %w", err)
	}
	return NewCapabilityMatrix(m), nil
}

// Get retrieves capabilities for a broker.
func (cm *CapabilityMatrix) Get(brokerID string) (BrokerCapabilities, bool) {
	c, ok := cm.byBroker[brokerID]
	return c, ok
}

// All returns all registered broker capability entries.
func (cm *CapabilityMatrix) All() []BrokerCapabilities {
	out := make([]BrokerCapabilities, 0, len(cm.byBroker))
	for _, c := range cm.byBroker {
		out = append(out, c)
	}
	return out
}

// ValidateOrder checks whether a canonical order is supported by the target broker.
// Unsupported order types, TIFs, asset classes, modes, or fractional quantities are rejected before dispatch.
func (cm *CapabilityMatrix) ValidateOrder(req OrderRequest, inst InstrumentSpec) error {
	caps, ok := cm.byBroker[req.BrokerID]
	if !ok {
		return NewError(ErrCodeBrokerNotFound, fmt.Sprintf("no capabilities registered for broker %q", req.BrokerID), false).
			WithContext(req.BrokerID, "")
	}
	if !containsMode(caps.AllowedModes, req.Mode) {
		return NewError(ErrCodeUnsupportedCapability, fmt.Sprintf("broker %q does not allow execution mode %q", req.BrokerID, req.Mode), false).
			WithContext(req.BrokerID, "")
	}
	if !containsAssetClass(caps.SupportedAssetClasses, inst.AssetClass) {
		return NewError(ErrCodeUnsupportedCapability, fmt.Sprintf("broker %q does not support asset class %q", req.BrokerID, inst.AssetClass), false).
			WithContext(req.BrokerID, "")
	}
	if !containsOrderType(caps.SupportedOrderTypes, req.OrderType) {
		return NewError(ErrCodeUnsupportedCapability, fmt.Sprintf("broker %q does not support order type %q", req.BrokerID, req.OrderType), false).
			WithContext(req.BrokerID, "")
	}
	if !containsTIF(caps.SupportedTimeInForce, req.TimeInForce) {
		return NewError(ErrCodeUnsupportedCapability, fmt.Sprintf("broker %q does not support time_in_force %q", req.BrokerID, req.TimeInForce), false).
			WithContext(req.BrokerID, "")
	}
	if !caps.SupportsFractionalQty && math.Abs(req.Quantity-math.Round(req.Quantity)) > 1e-9 {
		return NewError(ErrCodeUnsupportedCapability, fmt.Sprintf("broker %q does not support fractional quantity %.6f", req.BrokerID, req.Quantity), false).
			WithContext(req.BrokerID, "")
	}
	if caps.MaxNotionalUSD > 0 && req.EstimatedNotional(0) > caps.MaxNotionalUSD {
		return NewError(ErrCodeUnsupportedCapability, fmt.Sprintf("order notional %.2f exceeds broker %q max_notional_usd %.2f", req.EstimatedNotional(0), req.BrokerID, caps.MaxNotionalUSD), false).
			WithContext(req.BrokerID, "")
	}
	return nil
}

func containsMode(list []ExecutionMode, target ExecutionMode) bool {
	for _, x := range list {
		if x == target {
			return true
		}
	}
	return false
}

func containsAssetClass(list []AssetClass, target AssetClass) bool {
	for _, x := range list {
		if x == target {
			return true
		}
	}
	return false
}

func containsOrderType(list []OrderType, target OrderType) bool {
	for _, x := range list {
		if x == target {
			return true
		}
	}
	return false
}

func containsTIF(list []TimeInForce, target TimeInForce) bool {
	for _, x := range list {
		if x == target {
			return true
		}
	}
	return false
}
