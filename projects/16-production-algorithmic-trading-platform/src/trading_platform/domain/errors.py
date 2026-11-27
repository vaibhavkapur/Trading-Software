"""Structured domain and operational error hierarchy."""

from __future__ import annotations

from typing import Any


class PlatformError(Exception):
    """Base structured error for the trading platform."""

    code: str = "PLATFORM_ERROR"
    recoverable: bool = False

    def __init__(
        self,
        message: str,
        *,
        code: str | None = None,
        details: dict[str, Any] | None = None,
        recoverable: bool | None = None,
    ) -> None:
        super().__init__(message)
        self.message = message
        if code is not None:
            self.code = code
        if recoverable is not None:
            self.recoverable = recoverable
        self.details: dict[str, Any] = details or {}

    def to_dict(self) -> dict[str, Any]:
        return {
            "error_code": self.code,
            "message": self.message,
            "recoverable": self.recoverable,
            "details": self.details,
        }


class ValidationError(PlatformError):
    code = "VALIDATION_ERROR"
    recoverable = False


class DataEntitlementError(PlatformError):
    code = "DATA_ENTITLEMENT_ERROR"
    recoverable = False


class StaleFeedError(PlatformError):
    code = "STALE_FEED"
    recoverable = True


class ClockSkewError(PlatformError):
    code = "CLOCK_SKEW_DETECTED"
    recoverable = True


class RiskBreachError(PlatformError):
    code = "RISK_LIMIT_BREACH"
    recoverable = False


class KillSwitchActiveError(PlatformError):
    code = "KILL_SWITCH_ACTIVE"
    recoverable = False


class LiveModeBlockedError(PlatformError):
    code = "LIVE_MODE_BLOCKED"
    recoverable = False


class DuplicateOrderConflictError(PlatformError):
    code = "DUPLICATE_ORDER_CONFLICT"
    recoverable = False


class StateTransitionError(PlatformError):
    code = "INVALID_STATE_TRANSITION"
    recoverable = False


class BrokerTimeoutError(PlatformError):
    code = "BROKER_TIMEOUT_UNKNOWN_OUTCOME"
    recoverable = True


class BrokerDisconnectedError(PlatformError):
    code = "BROKER_DISCONNECTED"
    recoverable = True


class ReconciliationBreakError(PlatformError):
    code = "RECONCILIATION_BREAK"
    recoverable = True


class AuthorizationError(PlatformError):
    code = "AUTHORIZATION_DENIED"
    recoverable = False
