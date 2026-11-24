package reconcile

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/trading-software-projects/broker-reliability-reconciliation/src/domain"
	"github.com/trading-software-projects/broker-reliability-reconciliation/src/identity"
)

// SnapshotInput bundles raw internal and broker records fetched for an account as of a cutoff.
type SnapshotInput struct {
	AccountID    string
	Cutoff       time.Time
	Policy       domain.ReconPolicy
	LedgerOrders []domain.LedgerOrder
	BrokerOrders []domain.BrokerOrder
	LedgerFills  []domain.LedgerFill
	BrokerFills  []domain.BrokerFill
	InternalPos  []domain.PositionSnapshot
	BrokerPos    []domain.PositionSnapshot
	InternalCash []domain.CashSnapshot
	BrokerCash   []domain.CashSnapshot
}

// LateFillObservation captures arrival delay (receive_time - event_time) for metrics.
type LateFillObservation struct {
	BrokerExecID   string
	AccountID      string
	ArrivalSeconds float64
}

// Result contains the deterministic output of a reconciliation pass.
type Result struct {
	InputDigest      string
	Issues           []domain.ReconIssue
	Summary          domain.RunSummary
	LateObservations []LateFillObservation
}

// Engine executes normalization, cutoff filtering, comparison, and diff classification.
type Engine struct {
	mapper *identity.Mapper
	nowFn  func() time.Time
}

// NewEngine creates a reconciliation engine backed by the given identity mapper.
func NewEngine(mapper *identity.Mapper) *Engine {
	if mapper == nil {
		mapper = identity.NewMapper(nil)
	}
	return &Engine{
		mapper: mapper,
		nowFn:  func() time.Time { return time.Now().UTC() },
	}
}

// WithClock overrides the engine wall clock (useful for deterministic replay tests).
func (e *Engine) WithClock(fn func() time.Time) *Engine {
	if fn != nil {
		e.nowFn = fn
	}
	return e
}

// Reconcile performs deterministic order, fill, position, and cash reconciliation.
// Rerunning with identical inputs, cutoff, and policy produces identical InputDigest,
// issue IDs, classifications, exposures, and ordering.
func (e *Engine) Reconcile(runID string, in SnapshotInput) (*Result, error) {
	if strings.TrimSpace(in.AccountID) == "" {
		return nil, fmt.Errorf("account_id is required")
	}
	if in.Cutoff.IsZero() {
		return nil, fmt.Errorf("cutoff timestamp is required")
	}
	if err := in.Policy.Validate(); err != nil {
		return nil, fmt.Errorf("invalid policy: %w", err)
	}

	cutoff := in.Cutoff.UTC()
	capturedAt := e.nowFn().UTC()

	// 1. Normalize and filter by cutoff on EventTime (UTC).
	lOrders := make([]domain.LedgerOrder, 0, len(in.LedgerOrders))
	for _, o := range in.LedgerOrders {
		norm := e.mapper.NormalizeLedgerOrder(o)
		if !norm.EventTime.After(cutoff) {
			lOrders = append(lOrders, norm)
		}
	}
	bOrders := make([]domain.BrokerOrder, 0, len(in.BrokerOrders))
	for _, o := range in.BrokerOrders {
		norm := e.mapper.NormalizeBrokerOrder(o)
		if !norm.EventTime.After(cutoff) {
			bOrders = append(bOrders, norm)
		}
	}

	lFills := make([]domain.LedgerFill, 0, len(in.LedgerFills))
	for _, f := range in.LedgerFills {
		norm := e.mapper.NormalizeLedgerFill(f)
		if err := norm.Validate(); err != nil {
			return nil, err
		}
		if !norm.EventTime.After(cutoff) {
			lFills = append(lFills, norm)
		}
	}
	bFills := make([]domain.BrokerFill, 0, len(in.BrokerFills))
	for _, f := range in.BrokerFills {
		norm := e.mapper.NormalizeBrokerFill(f)
		if err := norm.Validate(); err != nil {
			return nil, err
		}
		if !norm.EventTime.After(cutoff) {
			bFills = append(bFills, norm)
		}
	}

	iPos := make([]domain.PositionSnapshot, 0, len(in.InternalPos))
	for _, p := range in.InternalPos {
		norm := e.mapper.NormalizePosition(p)
		if !norm.AsOf.After(cutoff) {
			iPos = append(iPos, norm)
		}
	}
	bPos := make([]domain.PositionSnapshot, 0, len(in.BrokerPos))
	for _, p := range in.BrokerPos {
		norm := e.mapper.NormalizePosition(p)
		if !norm.AsOf.After(cutoff) {
			bPos = append(bPos, norm)
		}
	}

	iCash := make([]domain.CashSnapshot, 0, len(in.InternalCash))
	for _, c := range in.InternalCash {
		norm := e.mapper.NormalizeCash(c)
		if !norm.AsOf.After(cutoff) {
			iCash = append(iCash, norm)
		}
	}
	bCash := make([]domain.CashSnapshot, 0, len(in.BrokerCash))
	for _, c := range in.BrokerCash {
		norm := e.mapper.NormalizeCash(c)
		if !norm.AsOf.After(cutoff) {
			bCash = append(bCash, norm)
		}
	}

	// Deterministic sorting before hashing & comparison.
	sort.Slice(lOrders, func(i, j int) bool { return lOrders[i].ClientOrderID < lOrders[j].ClientOrderID })
	sort.Slice(bOrders, func(i, j int) bool { return bOrders[i].BrokerOrderID < bOrders[j].BrokerOrderID })
	sort.Slice(lFills, func(i, j int) bool {
		if lFills[i].BrokerExecID != lFills[j].BrokerExecID {
			return lFills[i].BrokerExecID < lFills[j].BrokerExecID
		}
		return lFills[i].ID < lFills[j].ID
	})
	sort.Slice(bFills, func(i, j int) bool {
		if bFills[i].BrokerExecID != bFills[j].BrokerExecID {
			return bFills[i].BrokerExecID < bFills[j].BrokerExecID
		}
		return bFills[i].ReceiveTime.Before(bFills[j].ReceiveTime)
	})
	sort.Slice(iPos, func(i, j int) bool { return iPos[i].Symbol < iPos[j].Symbol })
	sort.Slice(bPos, func(i, j int) bool { return bPos[i].Symbol < bPos[j].Symbol })
	sort.Slice(iCash, func(i, j int) bool { return iCash[i].Currency < iCash[j].Currency })
	sort.Slice(bCash, func(i, j int) bool { return bCash[i].Currency < bCash[j].Currency })

	digest := computeInputDigest(in.AccountID, cutoff, in.Policy, lOrders, bOrders, lFills, bFills, iPos, bPos, iCash, bCash)

	var issues []domain.ReconIssue
	var lateObs []LateFillObservation

	// Helper to build deterministic ReconIssue
	makeIssue := func(
		kind domain.IssueKind,
		severity domain.IssueSeverity,
		refKey string,
		symbol string,
		currency string,
		exposure domain.Decimal,
		posDrift domain.Decimal,
		refs []domain.IssueReference,
		ev domain.IssueEvidence,
	) domain.ReconIssue {
		ev.PolicyVersion = in.Policy.Version
		ev.Cutoff = cutoff
		ev.CapturedAt = capturedAt
		id := deriveIssueID(in.AccountID, cutoff, in.Policy.Version, kind, refKey)
		return domain.ReconIssue{
			ID:               id,
			RunID:            runID,
			AccountID:        in.AccountID,
			Kind:             kind,
			Severity:         severity,
			Symbol:           symbol,
			Currency:         currency,
			MonetaryExposure: exposure.Abs(),
			PositionDrift:    posDrift.Abs(),
			References:       refs,
			State:            domain.IssueStateOpen,
			Evidence:         ev,
			History: []domain.IssueHistoryEntry{
				{
					Timestamp: capturedAt,
					Actor:     "reconcile-core",
					Action:    "created",
					ToState:   domain.IssueStateOpen,
					Note:      ev.Summary,
				},
			},
			CreatedAt: capturedAt,
			UpdatedAt: capturedAt,
		}
	}

	// 2. Reconcile Fills (duplicates, late arrival, clock skew, missing, mismatches)
	ledgerFillByExec := make(map[string]domain.LedgerFill)
	ledgerFillCounts := make(map[string]int)
	for _, lf := range lFills {
		// Clock skew check
		if lf.EventTime.Sub(lf.ReceiveTime) > time.Duration(in.Policy.ClockSkewToleranceSec)*time.Second {
			evTime := lf.EventTime
			rcTime := lf.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindClockSkewAnomaly,
				domain.SeverityMedium,
				"internal-fill:"+lf.ID,
				lf.Symbol,
				lf.Currency,
				domain.Zero(),
				domain.Zero(),
				[]domain.IssueReference{{System: "internal", Entity: "fill", ID: lf.ID}},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Internal fill %s event_time (%s) ahead of receive_time (%s)", lf.ID, evTime.Format(time.RFC3339), rcTime.Format(time.RFC3339)),
					InternalData: map[string]string{
						"ledger_fill_id": lf.ID,
						"broker_exec_id": lf.BrokerExecID,
						"event_time":     evTime.Format(time.RFC3339),
						"receive_time":   rcTime.Format(time.RFC3339),
					},
					Deltas: map[string]string{
						"skew_seconds": fmt.Sprintf("%.2f", evTime.Sub(rcTime).Seconds()),
					},
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
		}

		ledgerFillCounts[lf.BrokerExecID]++
		if ledgerFillCounts[lf.BrokerExecID] == 1 {
			ledgerFillByExec[lf.BrokerExecID] = lf
		} else if ledgerFillCounts[lf.BrokerExecID] == 2 {
			notional := lf.Qty.Mul(lf.Price)
			evTime := lf.EventTime
			rcTime := lf.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindDuplicateLedgerFill,
				domain.SeverityHigh,
				"dup-ledger:"+lf.BrokerExecID,
				lf.Symbol,
				lf.Currency,
				notional,
				lf.Qty,
				[]domain.IssueReference{{System: "internal", Entity: "fill", ID: lf.BrokerExecID}},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Duplicate internal ledger fill for broker_exec_id %s", lf.BrokerExecID),
					InternalData: map[string]string{
						"broker_exec_id": lf.BrokerExecID,
						"symbol":         lf.Symbol,
						"qty":            lf.Qty.String(),
						"price":          lf.Price.String(),
					},
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
		}
	}

	brokerFillByExec := make(map[string]domain.BrokerFill)
	brokerFillCounts := make(map[string]int)
	for _, bf := range bFills {
		// Clock skew check
		if bf.EventTime.Sub(bf.ReceiveTime) > time.Duration(in.Policy.ClockSkewToleranceSec)*time.Second {
			evTime := bf.EventTime
			rcTime := bf.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindClockSkewAnomaly,
				domain.SeverityMedium,
				"broker-fill:"+bf.BrokerExecID,
				bf.Symbol,
				bf.Currency,
				domain.Zero(),
				domain.Zero(),
				[]domain.IssueReference{{System: "broker", Entity: "fill", ID: bf.BrokerExecID}},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Broker fill %s event_time (%s) ahead of receive_time (%s)", bf.BrokerExecID, evTime.Format(time.RFC3339), rcTime.Format(time.RFC3339)),
					BrokerData: map[string]string{
						"broker_exec_id": bf.BrokerExecID,
						"event_time":     evTime.Format(time.RFC3339),
						"receive_time":   rcTime.Format(time.RFC3339),
					},
					Deltas: map[string]string{
						"skew_seconds": fmt.Sprintf("%.2f", evTime.Sub(rcTime).Seconds()),
					},
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
		}

		// Late fill detection (EventTime <= cutoff, but ReceiveTime > cutoff OR arrival delay > threshold)
		arrivalDelay := bf.ReceiveTime.Sub(bf.EventTime)
		if arrivalDelay > 0 {
			lateObs = append(lateObs, LateFillObservation{
				BrokerExecID:   bf.BrokerExecID,
				AccountID:      in.AccountID,
				ArrivalSeconds: arrivalDelay.Seconds(),
			})
		}

		brokerFillCounts[bf.BrokerExecID]++
		if brokerFillCounts[bf.BrokerExecID] == 1 {
			brokerFillByExec[bf.BrokerExecID] = bf
			if bf.ReceiveTime.After(cutoff) || arrivalDelay > time.Duration(in.Policy.LateFillThresholdSec)*time.Second {
				evTime := bf.EventTime
				rcTime := bf.ReceiveTime
				notional := bf.Qty.Mul(bf.Price)
				issues = append(issues, makeIssue(
					domain.IssueKindLateFill,
					domain.SeverityMedium,
					"late:"+bf.BrokerExecID,
					bf.Symbol,
					bf.Currency,
					notional,
					domain.Zero(),
					[]domain.IssueReference{{System: "broker", Entity: "fill", ID: bf.BrokerExecID}},
					domain.IssueEvidence{
						Summary: fmt.Sprintf("Late broker fill %s arrived %.0fs after venue event (cutoff=%s, receive=%s)",
							bf.BrokerExecID, arrivalDelay.Seconds(), cutoff.Format(time.RFC3339), rcTime.Format(time.RFC3339)),
						BrokerData: map[string]string{
							"broker_exec_id":  bf.BrokerExecID,
							"broker_order_id": bf.BrokerOrderID,
							"symbol":          bf.Symbol,
							"qty":             bf.Qty.String(),
							"price":           bf.Price.String(),
							"event_time":      evTime.Format(time.RFC3339),
							"receive_time":    rcTime.Format(time.RFC3339),
						},
						Deltas: map[string]string{
							"arrival_delay_sec": fmt.Sprintf("%.2f", arrivalDelay.Seconds()),
							"threshold_sec":     fmt.Sprintf("%d", in.Policy.LateFillThresholdSec),
							"arrived_post_cut":  fmt.Sprintf("%t", bf.ReceiveTime.After(cutoff)),
						},
						EventTime:   &evTime,
						ReceiveTime: &rcTime,
					},
				))
			}
		} else if brokerFillCounts[bf.BrokerExecID] == 2 {
			notional := bf.Qty.Mul(bf.Price)
			evTime := bf.EventTime
			rcTime := bf.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindDuplicateBrokerFill,
				domain.SeverityHigh,
				"dup-broker:"+bf.BrokerExecID,
				bf.Symbol,
				bf.Currency,
				notional,
				bf.Qty,
				[]domain.IssueReference{{System: "broker", Entity: "fill", ID: bf.BrokerExecID}},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Duplicate broker fill delivery detected for broker_exec_id %s (%s %s @ %s)",
						bf.BrokerExecID, bf.Qty.String(), bf.Symbol, bf.Price.String()),
					BrokerData: map[string]string{
						"broker_exec_id":  bf.BrokerExecID,
						"broker_order_id": bf.BrokerOrderID,
						"symbol":          bf.Symbol,
						"qty":             bf.Qty.String(),
						"price":           bf.Price.String(),
						"fee":             bf.Fee.String(),
					},
					Deltas: map[string]string{
						"duplicate_count": fmt.Sprintf("%d", brokerFillCounts[bf.BrokerExecID]),
					},
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
		}
	}

	// Collect all unique broker_exec_ids in deterministic order
	execIDs := unionSortedKeys(ledgerFillByExec, brokerFillByExec)
	for _, execID := range execIDs {
		lf, hasLedger := ledgerFillByExec[execID]
		bf, hasBroker := brokerFillByExec[execID]

		if hasLedger && !hasBroker {
			exposure := lf.Qty.Mul(lf.Price).Add(lf.Fee.Abs())
			evTime := lf.EventTime
			rcTime := lf.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindMissingBrokerFill,
				domain.SeverityHigh,
				"missing-broker-fill:"+execID,
				lf.Symbol,
				lf.Currency,
				exposure,
				lf.Qty,
				[]domain.IssueReference{
					{System: "internal", Entity: "fill", ID: lf.ID},
					{System: "internal", Entity: "order", ID: lf.ClientOrderID},
				},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Internal fill %s (exec %s) missing from broker records", lf.ID, execID),
					InternalData: map[string]string{
						"ledger_fill_id":  lf.ID,
						"client_order_id": lf.ClientOrderID,
						"broker_exec_id":  lf.BrokerExecID,
						"symbol":          lf.Symbol,
						"side":            string(lf.Side),
						"qty":             lf.Qty.String(),
						"price":           lf.Price.String(),
						"fee":             lf.Fee.String(),
					},
					Deltas: map[string]string{
						"missing_side": "broker",
						"notional":     lf.Qty.Mul(lf.Price).String(),
					},
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
			continue
		}

		if hasBroker && !hasLedger {
			exposure := bf.Qty.Mul(bf.Price).Add(bf.Fee.Abs())
			evTime := bf.EventTime
			rcTime := bf.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindMissingLedgerFill,
				domain.SeverityCritical,
				"missing-ledger-fill:"+execID,
				bf.Symbol,
				bf.Currency,
				exposure,
				bf.Qty,
				[]domain.IssueReference{
					{System: "broker", Entity: "fill", ID: bf.BrokerExecID},
					{System: "broker", Entity: "order", ID: bf.BrokerOrderID},
				},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Broker fill %s (%s %s @ %s) missing from internal ledger",
						bf.BrokerExecID, bf.Qty.String(), bf.Symbol, bf.Price.String()),
					BrokerData: map[string]string{
						"broker_exec_id":  bf.BrokerExecID,
						"broker_order_id": bf.BrokerOrderID,
						"client_order_id": bf.ClientOrderID,
						"symbol":          bf.Symbol,
						"side":            string(bf.Side),
						"qty":             bf.Qty.String(),
						"price":           bf.Price.String(),
						"fee":             bf.Fee.String(),
					},
					Deltas: map[string]string{
						"missing_side": "internal",
						"notional":     bf.Qty.Mul(bf.Price).String(),
					},
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
			continue
		}

		// Both sides exist: compare normalized attributes against policy tolerances.
		qtyDelta := bf.Qty.Sub(lf.Qty)
		priceDelta := bf.Price.Sub(lf.Price)
		feeDelta := bf.Fee.Sub(lf.Fee)

		deltas := map[string]string{}
		if bf.Symbol != lf.Symbol {
			deltas["symbol"] = lf.Symbol + " != " + bf.Symbol
		}
		if bf.Side != lf.Side {
			deltas["side"] = string(lf.Side) + " != " + string(bf.Side)
		}
		if bf.Currency != lf.Currency {
			deltas["currency"] = lf.Currency + " != " + bf.Currency
		}
		if qtyDelta.Abs().Cmp(in.Policy.QtyTolerance) > 0 {
			deltas["qty_delta"] = qtyDelta.String()
		}
		if priceDelta.Abs().Cmp(in.Policy.PriceTolerance) > 0 {
			deltas["price_delta"] = priceDelta.String()
		}
		if feeDelta.Abs().Cmp(in.Policy.FeeTolerance) > 0 {
			deltas["fee_delta"] = feeDelta.String()
		}

		if len(deltas) > 0 {
			notionalDelta := bf.Qty.Mul(bf.Price).Sub(lf.Qty.Mul(lf.Price)).Abs()
			exposure := notionalDelta.Add(feeDelta.Abs())
			sev := domain.SeverityMedium
			if qtyDelta.Sign() != 0 || bf.Symbol != lf.Symbol || bf.Side != lf.Side || exposure.Cmp(domain.DecimalFromInt(100)) > 0 {
				sev = domain.SeverityHigh
			}
			evTime := bf.EventTime
			rcTime := bf.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindFillMismatch,
				sev,
				"fill-mismatch:"+execID,
				bf.Symbol,
				bf.Currency,
				exposure,
				qtyDelta.Abs(),
				[]domain.IssueReference{
					{System: "internal", Entity: "fill", ID: lf.ID},
					{System: "broker", Entity: "fill", ID: bf.BrokerExecID},
				},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Fill attribute mismatch on exec %s (%s): %s", execID, bf.Symbol, formatDeltaSummary(deltas)),
					InternalData: map[string]string{
						"ledger_fill_id": lf.ID,
						"symbol":         lf.Symbol,
						"side":           string(lf.Side),
						"qty":            lf.Qty.String(),
						"price":          lf.Price.String(),
						"fee":            lf.Fee.String(),
						"currency":       lf.Currency,
					},
					BrokerData: map[string]string{
						"broker_exec_id": bf.BrokerExecID,
						"symbol":         bf.Symbol,
						"side":           string(bf.Side),
						"qty":            bf.Qty.String(),
						"price":          bf.Price.String(),
						"fee":            bf.Fee.String(),
						"currency":       bf.Currency,
					},
					Deltas:      deltas,
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
		}
	}

	// 3. Reconcile Orders via identity mapper (client_order_id <-> broker_order_id)
	brokerOrderByClient := make(map[string]domain.BrokerOrder)
	brokerOrderByID := make(map[string]domain.BrokerOrder)
	matchedBrokerOrders := make(map[string]bool)
	for _, bo := range bOrders {
		brokerOrderByID[bo.BrokerOrderID] = bo
		if bo.ClientOrderID != "" {
			brokerOrderByClient[bo.ClientOrderID] = bo
		}
	}

	for _, lo := range lOrders {
		var bo domain.BrokerOrder
		var ok bool
		if lo.ClientOrderID != "" {
			bo, ok = brokerOrderByClient[lo.ClientOrderID]
		}
		if !ok && lo.BrokerOrderID != "" {
			bo, ok = brokerOrderByID[lo.BrokerOrderID]
		}
		if !ok {
			if mappedBrokerID, found := e.mapper.BrokerOrderForClient(lo.ClientOrderID); found {
				bo, ok = brokerOrderByID[mappedBrokerID]
			}
		}

		if !ok {
			evTime := lo.EventTime
			rcTime := lo.ReceiveTime
			exposure := lo.Qty.Mul(lo.LimitPrice)
			issues = append(issues, makeIssue(
				domain.IssueKindMissingBrokerOrder,
				domain.SeverityHigh,
				"missing-broker-order:"+lo.ClientOrderID,
				lo.Symbol,
				lo.Currency,
				exposure,
				domain.Zero(),
				[]domain.IssueReference{{System: "internal", Entity: "order", ID: lo.ClientOrderID}},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Internal order %s (%s %s %s) not found at broker", lo.ClientOrderID, lo.Side, lo.Qty.String(), lo.Symbol),
					InternalData: map[string]string{
						"ledger_order_id": lo.ID,
						"client_order_id": lo.ClientOrderID,
						"symbol":          lo.Symbol,
						"side":            string(lo.Side),
						"qty":             lo.Qty.String(),
						"filled_qty":      lo.FilledQty.String(),
						"status":          string(lo.Status),
					},
					Deltas:      map[string]string{"missing_side": "broker"},
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
			continue
		}

		matchedBrokerOrders[bo.BrokerOrderID] = true
		deltas := map[string]string{}
		if lo.Status != bo.Status {
			deltas["status"] = string(lo.Status) + " != " + string(bo.Status)
		}
		if lo.Symbol != bo.Symbol {
			deltas["symbol"] = lo.Symbol + " != " + bo.Symbol
		}
		if lo.Side != bo.Side {
			deltas["side"] = string(lo.Side) + " != " + string(bo.Side)
		}
		if bo.Qty.Sub(lo.Qty).Abs().Cmp(in.Policy.QtyTolerance) > 0 {
			deltas["qty_delta"] = bo.Qty.Sub(lo.Qty).String()
		}
		if bo.FilledQty.Sub(lo.FilledQty).Abs().Cmp(in.Policy.QtyTolerance) > 0 {
			deltas["filled_qty_delta"] = bo.FilledQty.Sub(lo.FilledQty).String()
		}

		if len(deltas) > 0 {
			filledDrift := bo.FilledQty.Sub(lo.FilledQty).Abs()
			exposure := filledDrift.Mul(bo.LimitPrice)
			evTime := bo.EventTime
			rcTime := bo.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindOrderMismatch,
				domain.SeverityMedium,
				"order-mismatch:"+lo.ClientOrderID,
				bo.Symbol,
				bo.Currency,
				exposure,
				filledDrift,
				[]domain.IssueReference{
					{System: "internal", Entity: "order", ID: lo.ClientOrderID},
					{System: "broker", Entity: "order", ID: bo.BrokerOrderID},
				},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Order discrepancy between %s and %s: %s", lo.ClientOrderID, bo.BrokerOrderID, formatDeltaSummary(deltas)),
					InternalData: map[string]string{
						"client_order_id": lo.ClientOrderID,
						"status":          string(lo.Status),
						"qty":             lo.Qty.String(),
						"filled_qty":      lo.FilledQty.String(),
					},
					BrokerData: map[string]string{
						"broker_order_id": bo.BrokerOrderID,
						"status":          string(bo.Status),
						"qty":             bo.Qty.String(),
						"filled_qty":      bo.FilledQty.String(),
					},
					Deltas:      deltas,
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
		}
	}

	for _, bo := range bOrders {
		if matchedBrokerOrders[bo.BrokerOrderID] {
			continue
		}
		evTime := bo.EventTime
		rcTime := bo.ReceiveTime
		exposure := bo.Qty.Mul(bo.LimitPrice)
		issues = append(issues, makeIssue(
			domain.IssueKindMissingLedgerOrder,
			domain.SeverityCritical,
			"missing-ledger-order:"+bo.BrokerOrderID,
			bo.Symbol,
			bo.Currency,
			exposure,
			bo.FilledQty,
			[]domain.IssueReference{{System: "broker", Entity: "order", ID: bo.BrokerOrderID}},
			domain.IssueEvidence{
				Summary: fmt.Sprintf("Broker order %s (%s %s %s) missing from internal OMS", bo.BrokerOrderID, bo.Side, bo.Qty.String(), bo.Symbol),
				BrokerData: map[string]string{
					"broker_order_id": bo.BrokerOrderID,
					"client_order_id": bo.ClientOrderID,
					"symbol":          bo.Symbol,
					"side":            string(bo.Side),
					"qty":             bo.Qty.String(),
					"filled_qty":      bo.FilledQty.String(),
					"status":          string(bo.Status),
				},
				Deltas:      map[string]string{"missing_side": "internal"},
				EventTime:   &evTime,
				ReceiveTime: &rcTime,
			},
		))
	}

	// 4. Reconcile Positions (+ staleness checks)
	latestInternalPos := latestPositionsBySymbol(iPos)
	latestBrokerPos := latestPositionsBySymbol(bPos)
	posSymbols := unionSortedKeys(latestInternalPos, latestBrokerPos)

	staleThreshold := time.Duration(in.Policy.StaleSnapshotThresholdSec) * time.Second
	for _, sym := range posSymbols {
		ip, hasI := latestInternalPos[sym]
		bp, hasB := latestBrokerPos[sym]

		if hasI && cutoff.Sub(ip.AsOf) > staleThreshold {
			evTime := ip.AsOf
			rcTime := ip.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindStaleSnapshot,
				domain.SeverityHigh,
				"stale-pos-internal:"+sym,
				sym,
				ip.Currency,
				domain.Zero(),
				domain.Zero(),
				[]domain.IssueReference{{System: "internal", Entity: "position", ID: sym}},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Stale internal position snapshot for %s (age %.0fs > %ds)", sym, cutoff.Sub(ip.AsOf).Seconds(), in.Policy.StaleSnapshotThresholdSec),
					InternalData: map[string]string{
						"symbol": sym,
						"asof":   ip.AsOf.Format(time.RFC3339),
						"qty":    ip.Qty.String(),
					},
					Deltas: map[string]string{
						"staleness_sec": fmt.Sprintf("%.0f", cutoff.Sub(ip.AsOf).Seconds()),
					},
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
		}
		if hasB && cutoff.Sub(bp.AsOf) > staleThreshold {
			evTime := bp.AsOf
			rcTime := bp.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindStaleSnapshot,
				domain.SeverityHigh,
				"stale-pos-broker:"+sym,
				sym,
				bp.Currency,
				domain.Zero(),
				domain.Zero(),
				[]domain.IssueReference{{System: "broker", Entity: "position", ID: sym}},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Stale broker position snapshot for %s (age %.0fs > %ds)", sym, cutoff.Sub(bp.AsOf).Seconds(), in.Policy.StaleSnapshotThresholdSec),
					BrokerData: map[string]string{
						"symbol": sym,
						"asof":   bp.AsOf.Format(time.RFC3339),
						"qty":    bp.Qty.String(),
					},
					Deltas: map[string]string{
						"staleness_sec": fmt.Sprintf("%.0f", cutoff.Sub(bp.AsOf).Seconds()),
					},
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
		}

		iQty := domain.Zero()
		bQty := domain.Zero()
		refPrice := domain.Zero()
		ccy := "USD"
		if hasI {
			iQty = ip.Qty
			refPrice = ip.AvgPrice
			ccy = ip.Currency
		}
		if hasB {
			bQty = bp.Qty
			if !bp.AvgPrice.IsZero() {
				refPrice = bp.AvgPrice
			}
			ccy = bp.Currency
		}

		drift := bQty.Sub(iQty)
		if drift.Abs().Cmp(in.Policy.QtyTolerance) > 0 {
			exposure := drift.Abs().Mul(refPrice)
			issues = append(issues, makeIssue(
				domain.IssueKindPositionBreak,
				domain.SeverityCritical,
				"pos-break:"+sym,
				sym,
				ccy,
				exposure,
				drift.Abs(),
				[]domain.IssueReference{
					{System: "internal", Entity: "position", ID: sym},
					{System: "broker", Entity: "position", ID: sym},
				},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Position break on %s: internal=%s vs broker=%s (drift=%s)", sym, iQty.String(), bQty.String(), drift.String()),
					InternalData: map[string]string{
						"symbol":    sym,
						"qty":       iQty.String(),
						"avg_price": ip.AvgPrice.String(),
					},
					BrokerData: map[string]string{
						"symbol":    sym,
						"qty":       bQty.String(),
						"avg_price": bp.AvgPrice.String(),
					},
					Deltas: map[string]string{
						"qty_drift":          drift.String(),
						"estimated_exposure": exposure.String(),
					},
				},
			))
		}
	}

	// 5. Reconcile Cash (Settled vs Pending strictly separated — explicit non-goal guard!)
	latestInternalCash := latestCashByCurrency(iCash)
	latestBrokerCash := latestCashByCurrency(bCash)
	currencies := unionSortedKeys(latestInternalCash, latestBrokerCash)

	for _, ccy := range currencies {
		ic, hasI := latestInternalCash[ccy]
		bc, hasB := latestBrokerCash[ccy]

		if hasI && cutoff.Sub(ic.AsOf) > staleThreshold {
			evTime := ic.AsOf
			rcTime := ic.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindStaleSnapshot,
				domain.SeverityHigh,
				"stale-cash-internal:"+ccy,
				"",
				ccy,
				domain.Zero(),
				domain.Zero(),
				[]domain.IssueReference{{System: "internal", Entity: "cash", ID: ccy}},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Stale internal cash snapshot for %s (age %.0fs > %ds)", ccy, cutoff.Sub(ic.AsOf).Seconds(), in.Policy.StaleSnapshotThresholdSec),
					InternalData: map[string]string{
						"currency":     ccy,
						"asof":         ic.AsOf.Format(time.RFC3339),
						"settled_cash": ic.SettledCash.String(),
					},
					Deltas: map[string]string{
						"staleness_sec": fmt.Sprintf("%.0f", cutoff.Sub(ic.AsOf).Seconds()),
					},
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
		}
		if hasB && cutoff.Sub(bc.AsOf) > staleThreshold {
			evTime := bc.AsOf
			rcTime := bc.ReceiveTime
			issues = append(issues, makeIssue(
				domain.IssueKindStaleSnapshot,
				domain.SeverityHigh,
				"stale-cash-broker:"+ccy,
				"",
				ccy,
				domain.Zero(),
				domain.Zero(),
				[]domain.IssueReference{{System: "broker", Entity: "cash", ID: ccy}},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Stale broker cash snapshot for %s (age %.0fs > %ds)", ccy, cutoff.Sub(bc.AsOf).Seconds(), in.Policy.StaleSnapshotThresholdSec),
					BrokerData: map[string]string{
						"currency":     ccy,
						"asof":         bc.AsOf.Format(time.RFC3339),
						"settled_cash": bc.SettledCash.String(),
					},
					Deltas: map[string]string{
						"staleness_sec": fmt.Sprintf("%.0f", cutoff.Sub(bc.AsOf).Seconds()),
					},
					EventTime:   &evTime,
					ReceiveTime: &rcTime,
				},
			))
		}

		iSettled, iPending := domain.Zero(), domain.Zero()
		bSettled, bPending := domain.Zero(), domain.Zero()
		if hasI {
			iSettled = ic.SettledCash
			iPending = ic.PendingCash
		}
		if hasB {
			bSettled = bc.SettledCash
			bPending = bc.PendingCash
		}

		settledDelta := bSettled.Sub(iSettled)
		if settledDelta.Abs().Cmp(in.Policy.CashTolerance) > 0 {
			issues = append(issues, makeIssue(
				domain.IssueKindSettledCashBreak,
				domain.SeverityCritical,
				"settled-cash:"+ccy,
				"",
				ccy,
				settledDelta.Abs(),
				domain.Zero(),
				[]domain.IssueReference{
					{System: "internal", Entity: "cash", ID: ccy + ":settled"},
					{System: "broker", Entity: "cash", ID: ccy + ":settled"},
				},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Settled cash break in %s: internal=%s vs broker=%s (delta=%s; pending excluded)",
						ccy, iSettled.String(), bSettled.String(), settledDelta.String()),
					InternalData: map[string]string{
						"currency":     ccy,
						"settled_cash": iSettled.String(),
						"pending_cash": iPending.String(),
					},
					BrokerData: map[string]string{
						"currency":     ccy,
						"settled_cash": bSettled.String(),
						"pending_cash": bPending.String(),
					},
					Deltas: map[string]string{
						"settled_cash_delta": settledDelta.String(),
						"note":               "Pending/settling balances are never treated as final settled cash",
					},
				},
			))
		}

		pendingDelta := bPending.Sub(iPending)
		if pendingDelta.Abs().Cmp(in.Policy.PendingCashTolerance) > 0 {
			issues = append(issues, makeIssue(
				domain.IssueKindPendingCashBreak,
				domain.SeverityMedium,
				"pending-cash:"+ccy,
				"",
				ccy,
				pendingDelta.Abs(),
				domain.Zero(),
				[]domain.IssueReference{
					{System: "internal", Entity: "cash", ID: ccy + ":pending"},
					{System: "broker", Entity: "cash", ID: ccy + ":pending"},
				},
				domain.IssueEvidence{
					Summary: fmt.Sprintf("Pending/settling cash divergence in %s: internal=%s vs broker=%s (delta=%s)",
						ccy, iPending.String(), bPending.String(), pendingDelta.String()),
					InternalData: map[string]string{
						"currency":     ccy,
						"pending_cash": iPending.String(),
						"settled_cash": iSettled.String(),
					},
					BrokerData: map[string]string{
						"currency":     ccy,
						"pending_cash": bPending.String(),
						"settled_cash": bSettled.String(),
					},
					Deltas: map[string]string{
						"pending_cash_delta": pendingDelta.String(),
						"note":               "Tracked separately from settled cash until settlement completes",
					},
				},
			))
		}
	}

	// Deterministic issue ordering: severity desc, kind asc, id asc.
	sort.Slice(issues, func(i, j int) bool {
		si, sj := severityRank(issues[i].Severity), severityRank(issues[j].Severity)
		if si != sj {
			return si < sj
		}
		if issues[i].Kind != issues[j].Kind {
			return issues[i].Kind < issues[j].Kind
		}
		return issues[i].ID < issues[j].ID
	})

	// Build summary
	summary := domain.RunSummary{
		LedgerOrdersCount:   len(lOrders),
		BrokerOrdersCount:   len(bOrders),
		LedgerFillsCount:    len(lFills),
		BrokerFillsCount:    len(bFills),
		PositionsChecked:    len(posSymbols),
		CashBalancesChecked: len(currencies),
		TotalIssues:         len(issues),
		IssuesByKind:        make(map[string]int),
		IssuesBySeverity:    make(map[string]int),
		MonetaryExposure:    domain.Zero(),
		TotalPositionDrift:  domain.Zero(),
	}
	for _, iss := range issues {
		summary.IssuesByKind[string(iss.Kind)]++
		summary.IssuesBySeverity[string(iss.Severity)]++
		summary.MonetaryExposure = summary.MonetaryExposure.Add(iss.MonetaryExposure)
		summary.TotalPositionDrift = summary.TotalPositionDrift.Add(iss.PositionDrift)
	}

	return &Result{
		InputDigest:      digest,
		Issues:           issues,
		Summary:          summary,
		LateObservations: lateObs,
	}, nil
}

func severityRank(s domain.IssueSeverity) int {
	switch s {
	case domain.SeverityCritical:
		return 0
	case domain.SeverityHigh:
		return 1
	case domain.SeverityMedium:
		return 2
	case domain.SeverityLow:
		return 3
	default:
		return 4
	}
}

func deriveIssueID(accountID string, cutoff time.Time, policyVersion string, kind domain.IssueKind, refKey string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s|%s|%s|%s|%s", accountID, cutoff.UTC().Format(time.RFC3339Nano), policyVersion, kind, refKey)
	return "iss-" + hex.EncodeToString(h.Sum(nil))[:12]
}

// DeriveRunID computes a deterministic run identifier for replay equivalence.
func DeriveRunID(accountID string, cutoff time.Time, policyVersion string, inputDigest string) string {
	h := sha256.New()
	_, _ = fmt.Fprintf(h, "%s|%s|%s|%s", accountID, cutoff.UTC().Format(time.RFC3339Nano), policyVersion, inputDigest)
	return "run-" + hex.EncodeToString(h.Sum(nil))[:12]
}

func computeInputDigest(
	accountID string,
	cutoff time.Time,
	policy domain.ReconPolicy,
	lOrders []domain.LedgerOrder,
	bOrders []domain.BrokerOrder,
	lFills []domain.LedgerFill,
	bFills []domain.BrokerFill,
	iPos []domain.PositionSnapshot,
	bPos []domain.PositionSnapshot,
	iCash []domain.CashSnapshot,
	bCash []domain.CashSnapshot,
) string {
	payload := struct {
		AccountID string                    `json:"account_id"`
		Cutoff    string                    `json:"cutoff"`
		Policy    domain.ReconPolicy        `json:"policy"`
		LO        []domain.LedgerOrder      `json:"lo"`
		BO        []domain.BrokerOrder      `json:"bo"`
		LF        []domain.LedgerFill       `json:"lf"`
		BF        []domain.BrokerFill       `json:"bf"`
		IP        []domain.PositionSnapshot `json:"ip"`
		BP        []domain.PositionSnapshot `json:"bp"`
		IC        []domain.CashSnapshot     `json:"ic"`
		BC        []domain.CashSnapshot     `json:"bc"`
	}{
		AccountID: accountID,
		Cutoff:    cutoff.UTC().Format(time.RFC3339Nano),
		Policy:    policy,
		LO:        lOrders,
		BO:        bOrders,
		LF:        lFills,
		BF:        bFills,
		IP:        iPos,
		BP:        bPos,
		IC:        iCash,
		BC:        bCash,
	}
	raw, _ := json.Marshal(payload)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:])[:16]
}

func latestPositionsBySymbol(snaps []domain.PositionSnapshot) map[string]domain.PositionSnapshot {
	out := make(map[string]domain.PositionSnapshot)
	for _, p := range snaps {
		prev, ok := out[p.Symbol]
		if !ok || p.AsOf.After(prev.AsOf) {
			out[p.Symbol] = p
		}
	}
	return out
}

func latestCashByCurrency(snaps []domain.CashSnapshot) map[string]domain.CashSnapshot {
	out := make(map[string]domain.CashSnapshot)
	for _, c := range snaps {
		prev, ok := out[c.Currency]
		if !ok || c.AsOf.After(prev.AsOf) {
			out[c.Currency] = c
		}
	}
	return out
}

func unionSortedKeys[A any, B any](m1 map[string]A, m2 map[string]B) []string {
	seen := make(map[string]struct{}, len(m1)+len(m2))
	for k := range m1 {
		seen[k] = struct{}{}
	}
	for k := range m2 {
		seen[k] = struct{}{}
	}
	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func formatDeltaSummary(deltas map[string]string) string {
	keys := make([]string, 0, len(deltas))
	for k := range deltas {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, deltas[k]))
	}
	return strings.Join(parts, ", ")
}
