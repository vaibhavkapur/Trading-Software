package broker

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"oms/src/domain"
	"oms/src/fixed"
)

// SignatureHeader carries the webhook signature: "sha256=<hex HMAC-SHA256 of
// the raw request body>", keyed with the shared webhook secret.
const SignatureHeader = "X-Broker-Signature"

// Sign computes the signature header value for a webhook body.
func Sign(secret string, body []byte) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write(body)
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

// VerifySignature checks a webhook signature in constant time. It fails
// closed: an empty secret never verifies.
func VerifySignature(secret, header string, body []byte) bool {
	if secret == "" {
		return false
	}
	return hmac.Equal([]byte(header), []byte(Sign(secret, body)))
}

// Webhook event types sent by the broker.
const (
	EventAcknowledged   = "order.acknowledged"
	EventRejected       = "order.rejected"
	EventFill           = "order.fill"
	EventFillCorrected  = "order.fill_corrected"
	EventFillBusted     = "order.fill_busted"
	EventCancelled      = "order.cancelled"
	EventCancelRejected = "order.cancel_rejected"
	EventReplaced       = "order.replaced"
	EventReplaceReject  = "order.replace_rejected"
	EventExpired        = "order.expired"
)

var eventKinds = map[string]domain.ReportKind{
	EventAcknowledged:   domain.ReportAck,
	EventRejected:       domain.ReportReject,
	EventFill:           domain.ReportFill,
	EventFillCorrected:  domain.ReportCorrect,
	EventFillBusted:     domain.ReportBust,
	EventCancelled:      domain.ReportCancelled,
	EventCancelRejected: domain.ReportCancelRejected,
	EventReplaced:       domain.ReportReplaced,
	EventReplaceReject:  domain.ReportReplaceReject,
	EventExpired:        domain.ReportExpired,
}

// WebhookEvent is the broker's wire format for a pushed report.
type WebhookEvent struct {
	// EventID is unique per report and reused on redelivery.
	EventID       string `json:"event_id"`
	Type          string `json:"type"`
	ClientOrderID string `json:"client_order_id"`
	BrokerOrderID string `json:"broker_order_id,omitempty"`

	ExecID    string        `json:"exec_id,omitempty"`
	RefExecID string        `json:"ref_exec_id,omitempty"`
	Qty       int64         `json:"qty,omitempty"`
	Price     *fixed.Micros `json:"price,omitempty"`
	Fee       *fixed.Micros `json:"fee,omitempty"`

	OrderQty   int64         `json:"order_qty,omitempty"`
	LimitPrice *fixed.Micros `json:"limit_price,omitempty"`

	Reason string `json:"reason,omitempty"`
	// EventTime is the broker's clock, RFC 3339.
	EventTime time.Time `json:"event_time"`
}

// ErrMalformed is wrapped by every NormalizeWebhook error.
var ErrMalformed = errors.New("malformed broker event")

// NormalizeWebhook turns a webhook body into a normalized report. It checks
// only that the fields a report of its type needs are present; whether the
// report makes sense for the order is the aggregate's decision.
func NormalizeWebhook(body []byte) (domain.BrokerReport, error) {
	var ev WebhookEvent
	if err := json.Unmarshal(body, &ev); err != nil {
		return domain.BrokerReport{}, fmt.Errorf("%w: %v", ErrMalformed, err)
	}
	kind, ok := eventKinds[ev.Type]
	var missing []string
	need := func(present bool, field string) {
		if !present {
			missing = append(missing, field)
		}
	}
	need(ev.EventID != "", "event_id")
	need(ok, "a known type")
	need(ev.ClientOrderID != "", "client_order_id")
	need(!ev.EventTime.IsZero(), "event_time")
	switch kind {
	case domain.ReportFill, domain.ReportCorrect:
		need(ev.ExecID != "", "exec_id")
		need(ev.Qty > 0, "qty")
		need(ev.Price != nil, "price")
		need(ev.Fee != nil, "fee")
		need(kind == domain.ReportFill || ev.RefExecID != "", "ref_exec_id")
	case domain.ReportBust:
		need(ev.ExecID != "", "exec_id")
		need(ev.RefExecID != "", "ref_exec_id")
	case domain.ReportReplaced:
		need(ev.OrderQty > 0, "order_qty")
	}
	if len(missing) > 0 {
		return domain.BrokerReport{}, fmt.Errorf("%w: missing %s", ErrMalformed, strings.Join(missing, ", "))
	}

	r := domain.BrokerReport{
		EventID: ev.EventID, Kind: kind, OrderID: ev.ClientOrderID, BrokerOrderID: ev.BrokerOrderID,
		ExecID: ev.ExecID, RefExecID: ev.RefExecID, Qty: ev.Qty,
		OrderQty: ev.OrderQty, LimitPrice: ev.LimitPrice, Reason: ev.Reason,
		EventTime: ev.EventTime.UTC(), Raw: append(json.RawMessage(nil), body...),
	}
	if ev.Price != nil {
		r.Price = *ev.Price
	}
	if ev.Fee != nil {
		r.Fee = *ev.Fee
	}
	return r, nil
}
