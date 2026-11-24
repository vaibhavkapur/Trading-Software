// Package risk is the pre-trade gate. It is independent of routing: the
// planner proposes, risk disposes.
package risk

import (
	"fmt"
	"slices"

	"sor/src/domain"
)

type Limits struct {
	Symbols                []string `json:"symbols"`
	MaxParentQty           int64    `json:"max_parent_qty"`
	MaxParentNotionalMills int64    `json:"max_parent_notional_mills"`
	MaxChildQty            int64    `json:"max_child_qty"`
	PriceCollarBps         int64    `json:"price_collar_bps"` // child limit vs current mid
	MaxDurationSeconds     int64    `json:"max_duration_seconds"`
	LotSize                int64    `json:"lot_size"`
}

// Rejection is a risk refusal with a stable machine-readable code.
type Rejection struct{ Code, Msg string }

func (r *Rejection) Error() string { return r.Code + ": " + r.Msg }

func reject(code, format string, a ...any) error {
	return &Rejection{Code: code, Msg: fmt.Sprintf(format, a...)}
}

type Gate struct {
	L    Limits
	kill bool
}

func New(l Limits) *Gate { return &Gate{L: l} }

func (g *Gate) SetKill(on bool) { g.kill = on }
func (g *Gate) Killed() bool    { return g.kill }

func (g *Gate) CheckParent(p domain.ParentOrder, arrivalMidMills int64) error {
	l := g.L
	switch {
	case g.kill:
		return reject("KILL_SWITCH", "kill switch engaged")
	case !slices.Contains(l.Symbols, p.Symbol):
		return reject("SYMBOL_NOT_ALLOWED", "symbol %q is not tradable", p.Symbol)
	case !p.Side.Valid():
		return reject("BAD_SIDE", "side must be BUY or SELL")
	case p.TargetQty <= 0 || p.TargetQty%max(l.LotSize, 1) != 0:
		return reject("BAD_QTY", "qty must be a positive multiple of lot size %d", l.LotSize)
	case p.TargetQty > l.MaxParentQty:
		return reject("MAX_PARENT_QTY", "qty %d exceeds limit %d", p.TargetQty, l.MaxParentQty)
	case p.DeadlineMs <= p.StartMs || p.IntervalMs <= 0:
		return reject("BAD_DURATION", "duration and interval must be positive")
	case p.DeadlineMs-p.StartMs > l.MaxDurationSeconds*1000:
		return reject("MAX_DURATION", "duration exceeds limit of %ds", l.MaxDurationSeconds)
	case p.TargetQty > l.MaxParentNotionalMills/max(arrivalMidMills, 1):
		return reject("MAX_NOTIONAL", "notional exceeds limit")
	}
	return nil
}

// CollarPx is the worst limit price a child may carry given the current mid.
func (g *Gate) CollarPx(side domain.Side, midMills int64) int64 {
	if side == domain.Sell {
		// round up: the lowest acceptable sell price
		return (midMills*(10000-g.L.PriceCollarBps) + 10000*domain.MillsPerTick - 1) / (10000 * domain.MillsPerTick)
	}
	return midMills * (10000 + g.L.PriceCollarBps) / (10000 * domain.MillsPerTick)
}

// CheckChild validates one child order. available is the parent quantity not
// yet filled or reserved by unresolved children.
func (g *Gate) CheckChild(side domain.Side, qty, limitPx, midMills, available int64) error {
	switch {
	case g.kill:
		return reject("KILL_SWITCH", "kill switch engaged")
	case qty <= 0 || qty%max(g.L.LotSize, 1) != 0:
		return reject("BAD_QTY", "child qty %d is not a positive lot multiple", qty)
	case qty > g.L.MaxChildQty:
		return reject("MAX_CHILD_QTY", "child qty %d exceeds limit %d", qty, g.L.MaxChildQty)
	case qty > available:
		return reject("OVERFILL_RISK", "child qty %d exceeds available parent qty %d", qty, available)
	case side.Sign()*(limitPx-g.CollarPx(side, midMills)) > 0:
		return reject("PRICE_COLLAR", "limit %d outside %dbps collar", limitPx, g.L.PriceCollarBps)
	}
	return nil
}
