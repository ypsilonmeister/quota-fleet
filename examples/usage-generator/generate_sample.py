#!/usr/bin/env python3
"""Minimal Schema v1 usage snapshot generator for smoke testing arbitrary pools.

WARNING:
  THIS SCRIPT IS FOR SMOKE-TESTING AND CONFIGURATION VERIFICATION ONLY.
  DO NOT USE IT TO FEED OPERATIONAL DATA TO `gate_mode: enforce`.
  Real `enforce` operation requires measured subscription usage from your own collector.
"""

from __future__ import annotations

import argparse
import datetime as dt
import json
import os
import sys
import tempfile


def parse_percent(raw: str, label: str) -> int:
    try:
        value = int(raw)
    except ValueError as exc:
        raise argparse.ArgumentTypeError(f"{label} must be an integer 0..100, got {raw!r}") from exc
    if value < 0 or value > 100:
        raise argparse.ArgumentTypeError(f"{label} must be between 0 and 100, got {value}")
    return value


def parse_short_arg(raw: str):
    low = raw.strip().lower()
    if low == "null":
        return ("null", None)
    if low == "none":
        return ("not_applicable", None)
    return ("percent", parse_percent(raw, "--short"))


def utc_now_rfc3339() -> str:
    return dt.datetime.now(dt.timezone.utc).replace(microsecond=0).strftime("%Y-%m-%dT%H:%M:%SZ")


def build_pool_entry(
    weekly: int,
    short_mode: str,
    short_val: int | None,
    status: str,
    error_code: str,
    measured_at: str,
    weekly_reset: str | None,
    short_reset: str | None,
    short_window: str,
) -> dict:
    entry: dict = {
        "status": status,
        "measured_at": measured_at,
        "weekly": {
            "remaining_percent": weekly,
            "reset_at": weekly_reset,
        },
    }
    if status == "error":
        entry["error"] = error_code or "sample_probe_error"

    if short_mode == "null":
        entry["short"] = None
    elif short_mode == "not_applicable":
        entry["short"] = {"applicable": False}
    else:
        entry["short"] = {
            "remaining_percent": short_val,
            "reset_at": short_reset,
            "window": short_window,
        }
    return entry


def parse_extra_pool(spec: str, measured_at: str, weekly_reset: str | None, short_reset: str | None, short_window: str) -> tuple[str, dict]:
    parts = spec.split(":")
    if len(parts) < 3 or len(parts) > 5:
        raise ValueError(f"--extra-pool must be NAME:WEEKLY:SHORT[:STATUS[:ERROR]], got {spec!r}")
    name = parts[0].strip()
    if not name:
        raise ValueError("--extra-pool name must not be empty")
    weekly = parse_percent(parts[1], f"extra-pool {name} weekly")
    short_mode, short_val = parse_short_arg(parts[2])
    status = parts[3].strip() if len(parts) >= 4 and parts[3].strip() else "ok"
    if status not in ("ok", "error"):
        raise ValueError(f"extra-pool {name} status must be ok or error, got {status!r}")
    err_code = parts[4].strip() if len(parts) == 5 else ""
    return name, build_pool_entry(
        weekly=weekly,
        short_mode=short_mode,
        short_val=short_val,
        status=status,
        error_code=err_code,
        measured_at=measured_at,
        weekly_reset=weekly_reset,
        short_reset=short_reset,
        short_window=short_window,
    )


def atomic_write_json(path: str, payload: dict) -> None:
    out_dir = os.path.dirname(os.path.abspath(path))
    os.makedirs(out_dir, exist_ok=True)
    content = (json.dumps(payload, indent=2) + "\n").encode("utf-8")
    fd, tmp_path = tempfile.mkstemp(prefix=".usage-sample.", suffix=".tmp", dir=out_dir)
    try:
        with os.fdopen(fd, "wb") as f:
            f.write(content)
            f.flush()
            os.fsync(f.fileno())
        os.chmod(tmp_path, 0o644)
        os.replace(tmp_path, path)
    except Exception:
        try:
            os.unlink(tmp_path)
        except OSError:
            pass
        raise


def main(argv: list[str] | None = None) -> int:
    parser = argparse.ArgumentParser(
        description=(
            "Generate a minimal Schema v1 usage snapshot JSON for smoke testing arbitrary pools. "
            "FOR SMOKE-TESTING / CONFIG VERIFICATION ONLY — DO NOT USE AS OPERATIONAL DATA FOR gate_mode: enforce."
        ),
        epilog=(
            "WARNING: This script emits synthetic data (collector.name=sample-smoke-generator) "
            "solely to verify Schema v1 wiring, `quota-fleet usage validate`, and `gate_mode: dry-run`. "
            "Never use it as operational input for `gate_mode: enforce`."
        ),
    )
    parser.add_argument("--pool", required=True, help="Primary pool name (e.g. demo-pool)")
    parser.add_argument("--weekly", type=lambda v: parse_percent(v, "--weekly"), default=80, help="Weekly remaining percent (0..100)")
    parser.add_argument("--short", default="60", help="Short window remaining percent (0..100), 'null' (missing), or 'none' (applicable: false)")
    parser.add_argument("--status", choices=("ok", "error"), default="ok", help="Pool entry status (ok or error)")
    parser.add_argument("--error", default="", help="Sanitized error code when --status=error (e.g. probe_timeout)")
    parser.add_argument("--weekly-reset", default=None, help="Optional RFC3339 weekly reset_at (default: now+7d)")
    parser.add_argument("--short-reset", default=None, help="Optional RFC3339 short reset_at (default: now+5h)")
    parser.add_argument("--short-window", default="5h", help="Short window label (default: 5h)")
    parser.add_argument(
        "--extra-pool",
        action="append",
        default=[],
        help="Optional additional pool entry as NAME:WEEKLY:SHORT[:STATUS[:ERROR]] (repeatable)",
    )
    parser.add_argument("--out", required=True, help="Output JSON file path (written atomically)")

    args = parser.parse_args(argv)
    pool_name = args.pool.strip()
    if not pool_name or pool_name != args.pool:
        parser.error("--pool must be non-empty without leading/trailing whitespace")

    now = dt.datetime.now(dt.timezone.utc).replace(microsecond=0)
    measured_at = now.strftime("%Y-%m-%dT%H:%M:%SZ")
    weekly_reset = args.weekly_reset or (now + dt.timedelta(days=7)).strftime("%Y-%m-%dT%H:%M:%SZ")
    short_reset = args.short_reset or (now + dt.timedelta(hours=5)).strftime("%Y-%m-%dT%H:%M:%SZ")

    short_mode, short_val = parse_short_arg(args.short)
    pools: dict[str, dict] = {
        pool_name: build_pool_entry(
            weekly=args.weekly,
            short_mode=short_mode,
            short_val=short_val,
            status=args.status,
            error_code=args.error,
            measured_at=measured_at,
            weekly_reset=weekly_reset,
            short_reset=short_reset,
            short_window=args.short_window,
        )
    }
    for spec in args.extra_pool:
        try:
            name, entry = parse_extra_pool(spec, measured_at, weekly_reset, short_reset, args.short_window)
        except (ValueError, argparse.ArgumentTypeError) as exc:
            parser.error(str(exc))
        if name in pools:
            parser.error(f"duplicate pool name {name!r}")
        pools[name] = entry

    generated_at = utc_now_rfc3339()
    payload = {
        "schema_version": 1,
        "generated_at": generated_at,
        "collector": {
            "name": "sample-smoke-generator",
            "version": "1.0.0",
        },
        "pools": pools,
    }
    atomic_write_json(args.out, payload)
    return 0


if __name__ == "__main__":
    sys.exit(main())
