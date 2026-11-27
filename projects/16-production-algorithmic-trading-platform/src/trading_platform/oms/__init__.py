"""Idempotent Order Management System (OMS) with deterministic state machine."""

from trading_platform.oms.order_manager import VALID_TRANSITIONS, OrderManager

__all__ = ["OrderManager", "VALID_TRANSITIONS"]
