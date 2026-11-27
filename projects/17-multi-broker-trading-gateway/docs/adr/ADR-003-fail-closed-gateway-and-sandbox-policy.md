# ADR-003: Fail-Closed Gateway Circuit and Paper/Sandbox Default Policy

- **Status:** Accepted
- **Date:** 2026-11-27
- **Context:** Execution gateways must never silently trade live capital or continue dispatching orders when local books disagree with broker ground truth.
- **Decision:**
  1. Default unspecified execution mode to `paper` and enforce `config/policy.json` (`deny_live_execution: true`, `permitted_modes: ["paper", "sandbox"]`) before any adapter dispatch.
  2. Wire the independent `Reconciler` to automatically invoke `Gateway.Halt(reason)` whenever unresolved drift (e.g., phantom broker orders, irreconcilable fill quantity divergence, or stuck `CANCEL_PENDING` orders) meets or exceeds `max_unresolved_drift_before_halt`.
  3. While halted, all new order submissions fail closed with `GATEWAY_HALTED` (HTTP `503`), while read/reconcile/cancel introspection remains accessible. Resuming requires an explicit operator action (`Gateway.Resume(operatorID, note)`) logged to audit output.
- **Consequences:** Prioritizes capital safety over availability during unexplained state divergence, ensuring an operator investigates before resuming trading.
