"""Command-line interface (`patp`) for operating, inspecting, reconciling, and serving the platform."""

from __future__ import annotations

import argparse
import json
import sys
from pathlib import Path

from trading_platform.api.server import create_http_server
from trading_platform.domain.config import PlatformConfig
from trading_platform.domain.events import KillSwitchScope, Role
from trading_platform.platform import TradingPlatform

DEFAULT_CONFIG = Path("config/platform.paper.json")


def _load_platform(config_path: str | None) -> TradingPlatform:
    path = Path(config_path) if config_path else DEFAULT_CONFIG
    if path.exists():
        cfg = PlatformConfig.from_json_file(path)
    else:
        cfg = PlatformConfig()
    return TradingPlatform(cfg)


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        prog="patp",
        description="Production Algorithmic Trading Platform CLI (paper-first prototype)",
    )
    parser.add_argument("--config", default=str(DEFAULT_CONFIG), help="Path to platform JSON config")
    sub = parser.add_subparsers(dest="command", required=True)

    sub.add_parser("status", help="Print platform status, ledger, reservations, and audit health")
    sub.add_parser("check-live-gate", help="Evaluate fail-closed live rollout approval gate")
    sub.add_parser("reconcile", help="Run broker-to-ledger and OMS reconciliation")

    ks_engage = sub.add_parser("kill-engage", help="Engage kill switch")
    ks_engage.add_argument("--scope", default="GLOBAL", choices=["GLOBAL", "STRATEGY", "SYMBOL"])
    ks_engage.add_argument("--target", default="ALL")
    ks_engage.add_argument("--reason", required=True)
    ks_engage.add_argument("--actor", default="cli-operator")
    ks_engage.add_argument("--role", default="RISK_OFFICER")
    ks_engage.add_argument("--dual-release", action="store_true")

    ks_release = sub.add_parser("kill-release", help="Release kill switch")
    ks_release.add_argument("--scope", default="GLOBAL", choices=["GLOBAL", "STRATEGY", "SYMBOL"])
    ks_release.add_argument("--target", default="ALL")
    ks_release.add_argument("--reason", required=True)
    ks_release.add_argument("--actor", default="cli-operator")
    ks_release.add_argument("--role", default="RISK_OFFICER")
    ks_release.add_argument("--secondary-approver", default=None)
    ks_release.add_argument("--secondary-role", default=None)

    serve = sub.add_parser("serve", help="Run the Operator Console HTTP server + REST API")
    serve.add_argument("--host", default="127.0.0.1")
    serve.add_argument("--port", type=int, default=8080)

    args = parser.parse_args(argv)
    platform = _load_platform(args.config)

    try:
        if args.command == "status":
            print(json.dumps(platform.status_summary(), indent=2, default=str))
            return 0

        if args.command == "check-live-gate":
            gate = platform.check_live_gate()
            print(json.dumps(gate, indent=2))
            return 0 if gate["live_allowed"] else 2

        if args.command == "reconcile":
            report = platform.reconcile(actor="cli-operator")
            print(report.model_dump_json(indent=2))
            return 0 if report.matched else 1

        if args.command == "kill-engage":
            ks, canceled = platform.engage_kill_switch(
                scope=KillSwitchScope(args.scope),
                target=args.target,
                reason=args.reason,
                actor=args.actor,
                actor_role=Role(args.role),
                requires_dual_release=args.dual_release,
            )
            print(
                json.dumps(
                    {
                        "kill_switch": ks.model_dump(mode="json"),
                        "canceled_orders": [o.client_order_id for o in canceled],
                    },
                    indent=2,
                )
            )
            return 0

        if args.command == "kill-release":
            ks = platform.release_kill_switch(
                scope=KillSwitchScope(args.scope),
                target=args.target,
                reason=args.reason,
                actor=args.actor,
                actor_role=Role(args.role),
                secondary_approver=args.secondary_approver,
                secondary_role=Role(args.secondary_role) if args.secondary_role else None,
            )
            print(ks.model_dump_json(indent=2))
            return 0

        if args.command == "serve":
            server = create_http_server(platform, host=args.host, port=args.port)
            print(
                f"Operator Console listening on http://{args.host}:{args.port} "
                f"(mode={platform.config.execution_policy.execution_mode.value})"
            )
            try:
                server.serve_forever()
            except KeyboardInterrupt:
                server.shutdown()
            return 0

        return 1
    finally:
        platform.close()


if __name__ == "__main__":
    sys.exit(main())
