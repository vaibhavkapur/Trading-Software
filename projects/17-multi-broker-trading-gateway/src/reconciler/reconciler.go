package reconciler

import (
	"context"
	"fmt"
	"math"
	"sync/atomic"

	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/broker"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/domain"
	"github.com/vaibhavkapur/Trading-Software/projects/17-multi-broker-trading-gateway/src/gateway"
)

// Reconciler independently audits local gateway state against broker ground truth,
// resolves unknown submit results & cancel timeouts, and trips the fail-closed gate on unresolved drift.
type Reconciler struct {
	gw     *gateway.Gateway
	runSeq atomic.Int64
}

// New constructs an independent Reconciler bound to a Gateway.
func New(gw *gateway.Gateway) *Reconciler {
	return &Reconciler{gw: gw}
}

// ReconcileAll audits every registered broker adapter and persists a ReconciliationReport.
func (r *Reconciler) ReconcileAll(ctx context.Context) (domain.ReconciliationReport, error) {
	return r.ReconcileBrokers(ctx, r.gw.Registry().ListIDs())
}

// ReconcileBroker audits a single broker adapter.
func (r *Reconciler) ReconcileBroker(ctx context.Context, brokerID string) (domain.ReconciliationReport, error) {
	return r.ReconcileBrokers(ctx, []string{brokerID})
}

// ReconcileBrokers audits the specified broker IDs and triggers fail-closed halt if unresolved drift exceeds threshold.
func (r *Reconciler) ReconcileBrokers(ctx context.Context, brokerIDs []string) (domain.ReconciliationReport, error) {
	started := r.gw.Clock().Now()
	runID := fmt.Sprintf("recon-%06d", r.runSeq.Add(1))
	report := domain.ReconciliationReport{
		RunID:          runID,
		StartedAtUTC:   started,
		BrokersChecked: append([]string(nil), brokerIDs...),
	}

	for _, bid := range brokerIDs {
		adapter, err := r.gw.Registry().Get(bid)
		if err != nil {
			report.Findings = append(report.Findings, domain.ReconciliationItem{
				BrokerID:    bid,
				Drift:       domain.DriftUnresolvedOutcome,
				Resolved:    false,
				ActionTaken: "broker adapter missing from registry",
				Severity:    "CRITICAL",
			})
			report.UnresolvedCount++
			continue
		}

		brokerOrders, err := adapter.ListOrders(ctx)
		if err != nil {
			report.Findings = append(report.Findings, domain.ReconciliationItem{
				BrokerID:    bid,
				Drift:       domain.DriftUnresolvedOutcome,
				Resolved:    false,
				ActionTaken: fmt.Sprintf("broker list_orders failed: %s", err.Error()),
				Severity:    "CRITICAL",
			})
			report.UnresolvedCount++
			continue
		}

		brokerByClient := make(map[string]broker.BrokerOrderSnapshot, len(brokerOrders))
		for _, bo := range brokerOrders {
			brokerByClient[bo.ClientOrderID] = bo
		}

		localOrders := r.gw.Store().ListOrders(bid)
		localByClient := make(map[string]domain.Order, len(localOrders))
		for _, lo := range localOrders {
			localByClient[lo.ClientOrderID] = lo
		}
		report.OrdersChecked += len(localOrders)

		// 1. Audit every local order against broker ground truth
		for _, local := range localOrders {
			bo, existsAtBroker := brokerByClient[local.ClientOrderID]

			// Case A: Unknown submit result -> reconcile rather than blindly retrying!
			if local.Status == domain.StatusSubmitUnknown {
				if existsAtBroker {
					local.BrokerOrderID = bo.BrokerOrderID
					fills, _ := adapter.ListFills(ctx, local.ClientOrderID)
					r.gw.ApplyReconciledFills(&local, fills)
					local.Status = bo.Status
					if math.Abs(local.FilledQuantity-bo.FilledQuantity) > 1e-6 {
						local.FilledQuantity = bo.FilledQuantity
						local.RemainingQty = math.Max(0, local.Quantity-bo.FilledQuantity)
						local.AvgFillPrice = bo.AvgFillPrice
					}
					local.Reconciled = true
					local.UpdatedAtUTC = r.gw.Clock().Now()
					_ = r.gw.Store().SaveOrder(local)
					r.gw.PublishEvent(domain.EventOrderUpdated, bid, local.OrderID, local)

					report.Findings = append(report.Findings, domain.ReconciliationItem{
						BrokerID:      bid,
						OrderID:       local.OrderID,
						ClientOrderID: local.ClientOrderID,
						BrokerOrderID: bo.BrokerOrderID,
						Drift:         domain.DriftUnknownSubmitResolved,
						LocalStatus:   domain.StatusSubmitUnknown,
						BrokerStatus:  bo.Status,
						LocalFilled:   local.FilledQuantity,
						BrokerFilled:  bo.FilledQuantity,
						Resolved:      true,
						ActionTaken:   fmt.Sprintf("reconciled unknown submit to broker status %s (no blind retry)", bo.Status),
						Severity:      "WARN",
					})
				} else {
					local.Status = domain.StatusRejected
					local.RejectReason = "reconciled: submit never reached broker"
					local.Reconciled = true
					local.UpdatedAtUTC = r.gw.Clock().Now()
					_ = r.gw.Store().SaveOrder(local)
					r.gw.PublishEvent(domain.EventOrderUpdated, bid, local.OrderID, local)

					report.Findings = append(report.Findings, domain.ReconciliationItem{
						BrokerID:      bid,
						OrderID:       local.OrderID,
						ClientOrderID: local.ClientOrderID,
						Drift:         domain.DriftUnknownSubmitResolved,
						LocalStatus:   domain.StatusSubmitUnknown,
						BrokerStatus:  domain.StatusRejected,
						Resolved:      true,
						ActionTaken:   "order absent at broker; marked REJECTED cleanly (safe to submit new client_order_id)",
						Severity:      "WARN",
					})
				}
				continue
			}

			// Case B: Cancel timeout (CANCEL_PENDING) -> check broker terminal state
			if local.Status == domain.StatusCancelPending {
				if !existsAtBroker {
					report.Findings = append(report.Findings, domain.ReconciliationItem{
						BrokerID:      bid,
						OrderID:       local.OrderID,
						ClientOrderID: local.ClientOrderID,
						Drift:         domain.DriftMissingBrokerOrder,
						LocalStatus:   local.Status,
						Resolved:      false,
						ActionTaken:   "order in CANCEL_PENDING missing from broker",
						Severity:      "CRITICAL",
					})
					report.UnresolvedCount++
					continue
				}
				if bo.Status.IsTerminal() {
					fills, _ := adapter.ListFills(ctx, local.ClientOrderID)
					r.gw.ApplyReconciledFills(&local, fills)
					local.Status = bo.Status
					local.Reconciled = true
					local.UpdatedAtUTC = r.gw.Clock().Now()
					_ = r.gw.Store().SaveOrder(local)
					r.gw.PublishEvent(domain.EventOrderUpdated, bid, local.OrderID, local)

					report.Findings = append(report.Findings, domain.ReconciliationItem{
						BrokerID:      bid,
						OrderID:       local.OrderID,
						ClientOrderID: local.ClientOrderID,
						BrokerOrderID: bo.BrokerOrderID,
						Drift:         domain.DriftCancelTimeoutResolved,
						LocalStatus:   domain.StatusCancelPending,
						BrokerStatus:  bo.Status,
						LocalFilled:   local.FilledQuantity,
						BrokerFilled:  bo.FilledQuantity,
						Resolved:      true,
						ActionTaken:   fmt.Sprintf("reconciled cancel timeout to broker terminal status %s", bo.Status),
						Severity:      "WARN",
					})
				} else {
					report.Findings = append(report.Findings, domain.ReconciliationItem{
						BrokerID:      bid,
						OrderID:       local.OrderID,
						ClientOrderID: local.ClientOrderID,
						BrokerOrderID: bo.BrokerOrderID,
						Drift:         domain.DriftUnresolvedOutcome,
						LocalStatus:   local.Status,
						BrokerStatus:  bo.Status,
						LocalFilled:   local.FilledQuantity,
						BrokerFilled:  bo.FilledQuantity,
						Resolved:      false,
						ActionTaken:   "broker still reports non-terminal cancel_pending; operator intervention required",
						Severity:      "CRITICAL",
					})
					report.UnresolvedCount++
				}
				continue
			}

			// Skip locally rejected pre-broker orders (e.g., auth/rate-limit) that legitimately never existed at broker
			if local.Status == domain.StatusRejected && local.BrokerOrderID == "" {
				continue
			}

			if !existsAtBroker {
				report.Findings = append(report.Findings, domain.ReconciliationItem{
					BrokerID:      bid,
					OrderID:       local.OrderID,
					ClientOrderID: local.ClientOrderID,
					BrokerOrderID: local.BrokerOrderID,
					Drift:         domain.DriftMissingBrokerOrder,
					LocalStatus:   local.Status,
					Resolved:      false,
					ActionTaken:   "acknowledged local order missing from broker snapshot",
					Severity:      "CRITICAL",
				})
				report.UnresolvedCount++
				continue
			}

			// Case C: Sync missing broker fills or status updates
			statusMismatch := local.Status != bo.Status
			qtyMismatch := math.Abs(local.FilledQuantity-bo.FilledQuantity) > 1e-6
			if statusMismatch || qtyMismatch {
				prevFilled := local.FilledQuantity
				prevStatus := local.Status
				fills, _ := adapter.ListFills(ctx, local.ClientOrderID)
				r.gw.ApplyReconciledFills(&local, fills)

				stillQtyDrift := math.Abs(local.FilledQuantity-bo.FilledQuantity) > 1e-6
				if stillQtyDrift {
					report.Findings = append(report.Findings, domain.ReconciliationItem{
						BrokerID:      bid,
						OrderID:       local.OrderID,
						ClientOrderID: local.ClientOrderID,
						BrokerOrderID: bo.BrokerOrderID,
						Drift:         domain.DriftFillQtyMismatch,
						LocalStatus:   prevStatus,
						BrokerStatus:  bo.Status,
						LocalFilled:   local.FilledQuantity,
						BrokerFilled:  bo.FilledQuantity,
						Resolved:      false,
						ActionTaken:   "fill quantity mismatch persists after broker fill sync",
						Severity:      "CRITICAL",
					})
					report.UnresolvedCount++
				} else {
					local.Status = bo.Status
					local.Reconciled = true
					local.UpdatedAtUTC = r.gw.Clock().Now()
					_ = r.gw.Store().SaveOrder(local)
					r.gw.PublishEvent(domain.EventOrderUpdated, bid, local.OrderID, local)
					report.Findings = append(report.Findings, domain.ReconciliationItem{
						BrokerID:      bid,
						OrderID:       local.OrderID,
						ClientOrderID: local.ClientOrderID,
						BrokerOrderID: bo.BrokerOrderID,
						Drift:         domain.DriftStatusMismatch,
						LocalStatus:   prevStatus,
						BrokerStatus:  bo.Status,
						LocalFilled:   prevFilled,
						BrokerFilled:  bo.FilledQuantity,
						Resolved:      true,
						ActionTaken:   "synced local order state and missing fills from broker snapshot",
						Severity:      "INFO",
					})
				}
			}
		}

		// 2. Detect phantom orders present at broker but completely unknown to gateway
		for cid, bo := range brokerByClient {
			if _, known := localByClient[cid]; !known {
				report.Findings = append(report.Findings, domain.ReconciliationItem{
					BrokerID:      bid,
					ClientOrderID: cid,
					BrokerOrderID: bo.BrokerOrderID,
					Drift:         domain.DriftPhantomBrokerOrder,
					BrokerStatus:  bo.Status,
					BrokerFilled:  bo.FilledQuantity,
					Resolved:      false,
					ActionTaken:   "unrecognized phantom order on broker; tripping fail-closed gate",
					Severity:      "CRITICAL",
				})
				report.UnresolvedCount++
			}
		}
	}

	report.CompletedAtUTC = r.gw.Clock().Now()
	r.gw.Metrics.ReconciliationRuns.Add(1)
	r.gw.Metrics.ReconciliationFindings.Add(int64(len(report.Findings)))

	if report.UnresolvedCount >= r.gw.MaxUnresolvedBeforeHalt() && r.gw.Policy().FailClosedOnReconcileDrift {
		report.TriggeredFailSafe = true
		r.gw.Halt(fmt.Sprintf("reconciliation %s detected %d unresolved drift item(s)", report.RunID, report.UnresolvedCount))
	}

	_ = r.gw.Store().SaveReconciliationReport(report)
	if r.gw.Logger() != nil {
		r.gw.Logger().Info("reconciler", "reconciliation completed", map[string]any{
			"run_id":             report.RunID,
			"orders_checked":     report.OrdersChecked,
			"findings":           len(report.Findings),
			"unresolved_count":   report.UnresolvedCount,
			"triggered_failsafe": report.TriggeredFailSafe,
		})
	}
	r.gw.PublishEvent(domain.EventReconciliationReport, "", "", report)
	return report, nil
}
