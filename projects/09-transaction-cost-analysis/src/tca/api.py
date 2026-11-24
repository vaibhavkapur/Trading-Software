"""HTTP API and dashboard host.

There is no authentication. The server binds to loopback by default and is meant for a
single analyst's machine; put it behind an authenticating proxy before exposing it.
"""

from __future__ import annotations

from importlib import resources
from typing import Annotated, Any, Literal

from fastapi import Depends, FastAPI, HTTPException, Query, Request
from fastapi.concurrency import run_in_threadpool
from fastapi.responses import HTMLResponse, Response

from tca import __version__
from tca.importer import ImportFormatError
from tca.render import order_row, order_view, overlay_view
from tca.report import benchmarks_csv, fills_csv, methodology_view, orders_csv, summary_csv
from tca.service import Filters, TcaService
from tca.store import ImportResult
from tca.timeutil import parse_ts

MAX_IMPORT_BYTES = 64 * 1024 * 1024
MAX_REJECTS_RETURNED = 100

Benchmark = Literal["arrival_mid", "decision_mid", "interval_vwap"]
GroupBy = Literal["all", "broker", "symbol", "strategy", "side", "venue", "time_bucket"]
ImportDataset = Literal["orders", "child-orders", "fills", "quotes", "bars"]


def _timestamp(name: str, value: str | None) -> int | None:
    if not value:
        return None
    try:
        return parse_ts(value)
    except ValueError as exc:
        raise HTTPException(422, f"{name}: {exc}") from exc


def _filters(
    symbol: str | None = None,
    broker: str | None = None,
    strategy: str | None = None,
    side: Literal["buy", "sell", ""] | None = None,
    venue: str | None = None,
    start: Annotated[
        str | None, Query(description="arrival_ts >= start (ISO-8601 with offset)")
    ] = None,
    end: Annotated[str | None, Query(description="arrival_ts < end (ISO-8601 with offset)")] = None,
) -> Filters:
    return Filters(
        symbol=symbol or None,
        broker=broker or None,
        strategy=strategy or None,
        side=side or None,
        venue=venue or None,
        start_ts=_timestamp("start", start),
        end_ts=_timestamp("end", end),
    )


FiltersDep = Annotated[Filters, Depends(_filters)]


def _import_view(result: ImportResult) -> dict[str, Any]:
    return {
        "dataset": result.dataset,
        "source": result.source,
        "rows": result.rows,
        "accepted": result.accepted,
        "duplicates": result.duplicates,
        "rejected": result.rejected,
        "rejects": [
            {"row": reject.row, "code": reject.code, "detail": reject.detail}
            for reject in result.rejects[:MAX_REJECTS_RETURNED]
        ],
        "rejects_truncated": result.rejected > MAX_REJECTS_RETURNED,
    }


def _csv(content: str, filename: str) -> Response:
    return Response(
        content,
        media_type="text/csv; charset=utf-8",
        headers={"Content-Disposition": f'attachment; filename="{filename}"'},
    )


def create_app(service: TcaService | None = None) -> FastAPI:
    service = service or TcaService()
    app = FastAPI(
        title="Transaction Cost Analysis",
        version=__version__,
        description="Execution-quality measures over imported order, fill, quote and bar data.",
    )
    app.state.service = service

    @app.get("/", response_class=HTMLResponse, include_in_schema=False)
    def dashboard() -> str:
        return resources.files("tca").joinpath("web/index.html").read_text(encoding="utf-8")

    @app.get("/healthz")
    def healthz() -> dict[str, Any]:
        return {"status": "ok", "version": __version__, "store_revision": service.store.revision()}

    @app.get("/metrics", include_in_schema=False)
    def metrics() -> Response:
        return Response(service.metrics.render(), media_type="text/plain; version=0.0.4")

    @app.post("/v1/imports/{dataset}")
    async def import_dataset(
        dataset: ImportDataset, request: Request, source: str = ""
    ) -> dict[str, Any]:
        """Load rows from a CSV body, or a JSON body (`{"rows": [...]}`) when the content
        type is `application/json`. Valid rows are stored and invalid ones reported, so a
        200 response can still carry rejects. Re-sending rows is safe."""
        declared = request.headers.get("content-length", "0")
        if declared.isdigit() and int(declared) > MAX_IMPORT_BYTES:
            raise HTTPException(413, f"import body exceeds {MAX_IMPORT_BYTES} bytes")
        body = await request.body()
        if len(body) > MAX_IMPORT_BYTES:
            raise HTTPException(413, f"import body exceeds {MAX_IMPORT_BYTES} bytes")
        name = dataset.replace("-", "_")
        is_json = "json" in request.headers.get("content-type", "").lower()
        try:
            if is_json:
                result = await run_in_threadpool(service.import_json, name, body, source)
            else:
                text = body.decode("utf-8-sig")
                result = await run_in_threadpool(service.import_csv, name, text, source)
        except (ImportFormatError, UnicodeDecodeError) as exc:
            raise HTTPException(400, str(exc)) from exc
        return _import_view(result)

    @app.get("/v1/datasets")
    def datasets() -> dict[str, Any]:
        """Content versions, row counts, filter values and recent imports."""
        return service.datasets()

    @app.get("/v1/methodology")
    def methodology() -> dict[str, Any]:
        return methodology_view(service.methodology)

    @app.get("/v1/tca/orders")
    def list_orders(filters: FiltersDep) -> dict[str, Any]:
        analysis, results = service.select(filters)
        return {
            "count": len(results),
            "orders": [order_row(result, analysis) for result in results],
            "methodology_version": analysis.methodology.id,
            "dataset_version": analysis.dataset.version,
        }

    def _order(order_id: str):
        analysis = service.analysis()
        result = analysis.by_id.get(order_id)
        if result is None:
            raise HTTPException(404, f"unknown order {order_id!r}")
        return analysis, result

    @app.get("/v1/tca/orders/{order_id}")
    def get_order(order_id: str, benchmark: Benchmark = "arrival_mid") -> dict[str, Any]:
        analysis, result = _order(order_id)
        return order_view(result, analysis, benchmark)

    @app.get("/v1/tca/orders/{order_id}/overlay")
    def get_overlay(order_id: str) -> dict[str, Any]:
        """Quote path, bars, fills and benchmark levels around one order."""
        analysis, result = _order(order_id)
        return overlay_view(result, analysis)

    @app.get("/v1/tca/summary")
    def summary(
        filters: FiltersDep,
        group_by: GroupBy = "all",
        benchmark: Benchmark = "arrival_mid",
        include_untrusted: bool = False,
        bucket_minutes: Annotated[int, Query(ge=1, le=1440)] = 30,
    ) -> dict[str, Any]:
        return service.summary(
            group_by,
            benchmark,
            filters,
            include_untrusted=include_untrusted,
            bucket_minutes=bucket_minutes,
        )

    @app.get("/v1/tca/summary.csv")
    def summary_export(
        filters: FiltersDep,
        group_by: GroupBy = "all",
        benchmark: Benchmark = "arrival_mid",
        include_untrusted: bool = False,
        bucket_minutes: Annotated[int, Query(ge=1, le=1440)] = 30,
    ) -> Response:
        _, results = service.select(filters)
        content = summary_csv(
            results,
            group_by,
            benchmark,
            include_untrusted=include_untrusted,
            bucket_minutes=bucket_minutes,
        )
        return _csv(content, f"tca_summary_by_{group_by}.csv")

    @app.get("/v1/tca/export.csv")
    def export(
        filters: FiltersDep, view: Literal["orders", "fills", "benchmarks"] = "orders"
    ) -> Response:
        """One row per order (default), per fill, or per order benchmark. Every order and
        benchmark row carries the methodology version; order rows also carry the dataset
        version."""
        analysis, results = service.select(filters)
        if view == "fills":
            content = fills_csv(results)
        elif view == "benchmarks":
            content = benchmarks_csv(results, analysis)
        else:
            content = orders_csv(results, analysis)
        return _csv(content, f"tca_{view}.csv")

    return app
