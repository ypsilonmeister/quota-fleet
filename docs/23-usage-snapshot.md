# 23 — Usage snapshot contract (Schema v1)

This document defines the normalized JSON contract (**Schema v1**, `schema_version: 1`) read by the Quota Gate (`quota-fleet serve`, `quota-fleet gate check`, `quota-fleet gate release`) and validated by `quota-fleet usage validate`.

`quota-fleet` stays focused on the file contract and gate evaluation; credential handling and provider probing live outside the core binary so credentials never leave the machine that holds the subscription login.

## 1. Sample Schema v1 JSON

```json
{
  "schema_version": 1,
  "generated_at": "2026-11-26T20:30:12Z",
  "collector": {
    "name": "quota-fleet-collector",
    "version": "1.0.0",
    "host": "collector-vm",
    "interval_seconds": 1800
  },
  "pools": {
    "claude": {
      "status": "ok",
      "measured_at": "2026-11-26T20:29:48Z",
      "weekly": {
        "remaining_percent": 100,
        "reset_at": "2026-12-03T11:00:00Z"
      },
      "short": {
        "remaining_percent": 100,
        "reset_at": null,
        "window": "5h"
      }
    },
    "codex": {
      "status": "error",
      "error": "probe_auth_expired",
      "measured_at": "2026-11-26T19:59:44Z",
      "weekly": {
        "remaining_percent": 62,
        "reset_at": "2026-12-01T05:54:42Z"
      },
      "short": {
        "remaining_percent": 63,
        "reset_at": "2026-11-26T22:54:25Z",
        "window": "5h"
      }
    },
    "agy-gemini": {
      "status": "ok",
      "measured_at": "2026-11-26T20:30:10Z",
      "weekly": {
        "remaining_percent": 94,
        "reset_at": "2026-12-03T09:29:03Z",
        "reset_at_approx": false
      },
      "short": {
        "remaining_percent": 79,
        "reset_at": "2026-11-26T23:29:03Z",
        "reset_at_approx": false,
        "window": "5h"
      }
    },
    "agy-3p": {
      "status": "ok",
      "measured_at": "2026-11-26T20:30:10Z",
      "weekly": {
        "remaining_percent": 0,
        "reset_at": "2026-12-02T08:24:03Z",
        "reset_at_approx": false
      },
      "short": null
    },
    "weekly-only-plan": {
      "status": "ok",
      "measured_at": "2026-11-26T20:30:11Z",
      "weekly": {
        "remaining_percent": 80,
        "reset_at": "2026-12-03T00:00:00Z"
      },
      "short": {
        "applicable": false
      }
    }
  }
}
```

## 2. Field specification and strict decoding

### Top-level fields

| Field | Type | Required | Rules |
|---|---|---|---|
| `schema_version` | integer (`1`) | **Required** | Must be the exact JSON integer `1` (`null`, `"1"`, `1.0`, or unknown numbers are rejected). **Whenever a JSON object has a top-level `schema_version` key, the parser never falls back to the Legacy parser.** |
| `generated_at` | string (RFC3339) | **Required** | Timestamp when snapshot generation **completed** (not collection start). Timezone offset or `Z` is required. |
| `collector` | object \| `null` | Optional | Diagnostic metadata (`name`, `version`, `host`, `interval_seconds > 0`). Not used for gate verdicts. |
| `pools` | object (`map<string, PoolEntry>`) | **Required** | Keys are arbitrary non-empty pool names matching `fleet.yaml` `pools:` keys (no leading/trailing whitespace). Empty `{}` is structurally valid (requested pools evaluate as `pool_not_found`). |

### `pools.<name>` entry fields

| Field | Type | Required | Rules |
|---|---|---|---|
| `status` | `"ok"` \| `"error"` | Optional (default `"ok"`) | `null` or any other value is a schema violation. |
| `error` | string \| `null` | Optional | Short sanitized failure code from the known diagnostic allowlist (`probe_timeout`, `probe_auth_expired`, `probe_parse_failed`, `probe_failed`, `probe_rate_limited`, `probe_network_error`, `probe_http_error`, `probe_command_failed`, `probe_missing_pool`, `probe_invalid_output`, `probe_invalid_timestamp`, `collector_error`, `sample_probe_error`). **Must never contain raw CLI output, upstream response bodies, or credentials.** Receivers only pass through known diagnostic codes verbatim and replace any other non-empty value (including single-line credentials, arbitrary strings, or multiline text) with `unsanitized_error_redacted` while preserving `collector_error`. |
| `measured_at` | string (RFC3339) \| `null` | **Required key** | Timestamp when this pool was actually probed (preferably stamped from the collector host's single UTC clock on probe completion). Must be non-null RFC3339 when `status: "ok"`. When `status: "error"`, retain the last successful `measured_at` (`null` only on initial failure with no prior observation). Never bump a previous `measured_at` on re-copy or retry failure. |
| `weekly` | object \| `null` | **Required key** | Must be a JSON object when `status: "ok"`. May retain the prior object or be `null` on initial failure when `status: "error"`. |
| `weekly.remaining_percent` | integer (`0..100`) | **Required** (when `weekly != null`) | Strict JSON integer `0..100`. Omission, `null`, floats (`12.5`, `1.0`), strings, or out-of-range values are file-wide schema violations (`snapshot_error`). |
| `weekly.reset_at` | string (RFC3339) \| `null` | Optional | Weekly reset timestamp. Empty string `""` is invalid; use `null` or omit when unknown. |
| `weekly.reset_at_approx` | boolean | Optional (default `false`) | `true` marks an estimated reset timestamp (treated as unknown for candidate ordering). |
| `short` | object \| `null` | Optional | Canonical missing form for producers is `null` or omitted key. Any type other than object or `null` is a schema violation. |
| `short.applicable` | boolean | Optional (default `true`) | Must be boolean. When `false`, combining it with `remaining_percent`, `reset_at`, `reset_at_approx`, or `window` (even with `null` values) is a file-wide schema violation (`snapshot_error`). Cross-checked against `fleet.yaml` `pools.<name>.short_window`. |
| `short.remaining_percent` | integer (`0..100`) \| `null` / omitted | Normalized on receive | When present and non-null, must be a strict JSON integer `0..100` (out-of-range or wrong type is a file-wide schema violation). When `short` is an object with `applicable != false` and `remaining_percent` is omitted or `null`, the receiver normalizes it to missing (`short_window_missing` for that pool only, not a file-wide error). |
| `short.reset_at` | string (RFC3339) \| `null` | Optional | Short window reset timestamp. Empty string `""` is invalid. |
| `short.reset_at_approx` | boolean | Optional (default `false`) | `true` marks an estimated reset timestamp. |
| `short.window` | string | Optional (default `"5h"`) | Non-empty label (e.g. `"5h"`, `"1h"`, `"24h"`). |

### Strict decoding and fault isolation

- **Duplicate keys are rejected in Schema v1**: JSON objects at any nesting level (including `pools`) with duplicate keys fail decoding with `duplicate_key` (file-wide `snapshot_error`).
- **No implicit `0` on omission**: Missing or `null` numeric fields never coerce to `0`.
- **Blast radius**:
  - Syntax errors, duplicate keys, invalid `schema_version`, expected format mismatch, schema violations, and `generated_at > now + 5m` affect the **entire file** (`snapshot_error`, holding all measured pools in `v1 enforce`).
  - Schema-valid per-pool probe failures (`status: "error"`) and normalized missing short windows (`short: null`, omitted, `short: {}`, `short: {"remaining_percent": null}`) are **localized to that pool** and do not block healthy sibling pools or atomic deployment.

## 3. Missing short window & `fleet.yaml` authority

1. **Producer rule**: A collector may output `short.remaining_percent: 100` **only** when upstream returned an explicit valid numeric observation of `0` utilization / `100` remaining. Missing objects, missing keys, explicit `null`, or non-numeric upstream data must be emitted as `short: null` (or omit `short`).
2. **Authority in `fleet.yaml`**: Each pool in `fleet.yaml` supports `short_window: true | false` (default `true`). A snapshot cannot disable short-window enforcement on its own:

| `fleet.yaml` `short_window` | Normalized `short` | Result |
|---|---|---|
| `true` (default) | `remaining_percent` in `1..100` | Short window OK |
| `true` (default) | `remaining_percent == 0` | Exhausted (`blocked_by: 5h` or `short.window`) |
| `true` (default) | Missing (`null` / omitted / `{}` / `remaining_percent: null`) | If `weekly == 0`: **`exhausted` (`weekly`)** (CLI info only, `exit 0`). If `weekly > 0`: **`unknown` (`short_window_missing`)** (`v1 enforce` holds; `usage validate` normal `exit 2`). |
| `true` (default) | `{"applicable": false}` | Config mismatch: **`unknown` (`short_window_mismatch`)** (`v1 enforce` holds; `usage validate --config` `exit 1`). |
| `false` | `{"applicable": false}` or missing | Short check exempted (judged by `weekly` only). |
| `false` | `remaining_percent` in `0..100` | Short check exempted (`usage validate --config` emits advisory warning, `exit 0`). |

## 4. Timestamps, tolerances, and priority

- **`pools.<name>.measured_at`**: source of truth for per-pool freshness.
- **`generated_at`**: source of truth for snapshot liveness and generation ordering.
- **OS `mtime`**: diagnostic hint only; never used for gate freshness or root-cause attribution.

| Check | Tolerance | Behavior on violation |
|---|---|---|
| `generated_at` future skew | `<= now + 5m` | File-wide `snapshot_error` (`timestamp_future`). `usage validate` (normal & `--check-deploy`) exits `1`. |
| `measured_at` future skew | `<= now + 5m` | Localized to pool in Gate (`timestamp_future`, rank 4a). `usage validate` (normal & `--check-deploy`) exits `1`. |
| `measured_at` vs `generated_at` | `<= generated_at + 60s` | Localized to pool in Gate (`timestamp_inconsistent`, rank 4b). `usage validate` (normal & `--check-deploy`) exits `1`. |

### Single-pool evaluation priority (Schema v1, ranks 1–11)

1. **Rank 1 — File read/parse/format/`generated_at > now + 5m`**: `unknown` / `snapshot_error` (file-wide)
2. **Rank 2 — Snapshot stale (`now - generated_at > stale_after`)**: `unknown` / `stale` (`snapshot_stale`, file-wide)
3. **Rank 3 — Pool missing (`pool_not_found`) / deleted in held release (`pool_not_configured`)**: `unknown`
4. **Rank 4 — Per-pool timestamp anomaly (localized in Gate)**: **4a `timestamp_future`** takes precedence over **4b `timestamp_inconsistent`**
5. **Rank 5 — Per-pool probe error (`status == "error"`)**: `unknown` / `collector_error`
6. **Rank 6 — Per-pool stale (`now - measured_at > stale_after`)**: `unknown` / `stale` (`pool_stale`)
7. **Rank 7 — Short-window config mismatch**: `unknown` / `short_window_mismatch`
8. **Rank 8 — Weekly exhausted (`weekly.remaining_percent == 0`)**: `exhausted` (`weekly` or `weekly,<short.window>`)
9. **Rank 9 — Short window missing (`weekly > 0` and `short_window: true`)**: `unknown` / `short_window_missing`
10. **Rank 10 — Short window exhausted**: `exhausted` (`<short.window>`)
11. **Rank 11 — Healthy**: `healthy`

> **Gate primary reason vs CLI full scan**: Gate evaluation uses ranks 1–11 (with 4a > 4b) for each pool's primary reason and picks the lowest-ranked unknown across candidates (ties broken by candidate declaration order). `quota-fleet usage validate` always performs a **full scan** across file-level and all per-pool checks (never short-circuiting after `snapshot_stale`) and aggregates the exit code by severity (`exit 1` > `exit 2` > `exit 0`).

### Weekly reset ordering (Schema v1)

Among healthy measured candidates:
1. Candidates with a valid measured future reset (`reset_at != null && reset_at > now && !reset_at_approx`) always precede candidates whose reset is missing, in the past/present (`<= now`), or approximate (`reset_at_approx: true`).
2. Valid measured future resets are ordered earliest first.
3. Ties preserve candidate declaration order (`primary`, then `alt_pools`).

## 5. `quota.usage_format`, Gate order, and Legacy compatibility

Configure `quota.usage_format: legacy | v1` in `fleet.yaml` (default: `"legacy"`). There is no `auto` mode.

- **`usage_format: legacy` (default)**: Preserves all existing runtime behavior (empty `usage_file` allowed on startup with warning, legacy `resetBefore`, legacy hold rules, unmeasured alt bypass on snapshot error, `json.Unmarshal` legacy parsing when `schema_version` key is absent) while rejecting any JSON with a `schema_version` key (no fallback) and logging `legacy_compat_diff` diagnostics.
- **`usage_format: v1`**:
  - `config.Validate()` fails fast if `gate_mode` is `dry-run` or `enforce`, at least one measured pool exists, and `usage_file` is empty.
  - Registration follows strict 6-step evaluation order:
    - **Step 0**: No candidates → skip Gate (pass, log unmapped warning).
    - **Step 1**: Verify **all** candidates exist in `fleet.yaml` `pools:` **before** bypass or snapshot load. If any candidate is undefined: `enforce` immediately rejects (`HTTP 400 Bad Request` / Chain Driver `admission_rejected`), `dry-run` passes and records `unknown` / `pool_not_configured`.
    - **Step 2**: Primary pool is `unmeasured: true`, OR primary is empty and all `alt_pools` are defined `unmeasured: true` → bypass without reading usage file.
    - **Step 3**: Load snapshot and evaluate all candidates (ranks 1–11).
    - **Step 4**: Healthy measured candidate exists → select best by v1 weekly reset order.
    - **Step 5**: Detour to `unmeasured: true` alt **only** when there is at least one measured candidate and **every** measured candidate is definitively `exhausted` (`len(unknown) == 0`).
    - **Step 6**: Hold (fail-closed) in `enforce` / record in `dry-run` with rank-prioritized primary reason.
  - **Held jobs after config changes (periodic & manual release unified)**: Stored `(held.Pool, held.AltPools)` are preserved and evaluated against current `fleet.yaml` attributes. Empty candidates or all-deleted candidates cannot be released (`no_candidate_pools` / `pool_not_configured`). If stored primary `held.Pool` is changed to `unmeasured: true`, bypass release to `held.Pool` is permitted even if some `alt_pools` were removed (with a warning). On partial deletion of measured candidates, remaining healthy measured candidates can still release (with a warning), while deleted candidates stay `unknown` (`pool_not_configured`) and prevent unmeasured-alt detour.

## 6. CLI verification & side-by-side comparison

### `quota-fleet usage validate`

```bash
quota-fleet usage validate \
  [--file path/to/usage.json] \
  [--config fleet.yaml] \
  [--format legacy|v1] \
  [--stale-after 45m] \
  [--ignore-stale] \
  [--check-deploy] \
  [--reject-older-than path/to/existing.json] \
  [--now RFC3339] \
  [--json]
```

- **Normal diagnostic mode**:
  - `exit 0`: No hard errors and no operational degradations (weekly `0%` with missing short is informational only).
  - `exit 1`: Hard error (read error, invalid JSON, duplicate key, `format_mismatch`, schema violation, `generated_at > now + 5m`, any pool `measured_at > now + 5m` or `> generated_at + 60s`, `--config` missing measured pool or `short_window_mismatch`, `--reject-older-than` `older_generation` or `generation_conflict`). Always takes precedence over `exit 2`.
  - `exit 2`: Schema-valid operational degradation (`snapshot_stale`, `pool_stale`, `collector_error`, `short_window_missing` when `weekly > 0`). `--ignore-stale` excludes only `snapshot_stale` and `pool_stale` from triggering `exit 2`.
- **Deployment gate mode (`--check-deploy`)**:
  - `exit 0`: Deployable (zero hard errors). Schema-valid partial failures (`status: "error"`, missing short, stale) are allowed through with warnings (`degraded: true`) so healthy sibling pool updates are not blocked.
  - `exit 1`: Deployment rejected (any hard error, including `--reject-older-than` violations).
- **`--reject-older-than <existing>`**:
  - Compares `new.generated_at` vs `existing.generated_at` (UTC instants) when `<existing>` is a valid Schema v1 file with `existing.generated_at <= now + 5m`:
    - `new < existing` → `exit 1` (`older_generation`).
    - `new == existing` with identical SHA-256 → `exit 0` (`same_generation_identical`, no-op).
    - `new == existing` with different SHA-256 → `exit 1` (`generation_conflict`).
    - If `<existing>` is absent, corrupt, Legacy, or has a future-skewed `generated_at > now + 5m`, the comparison is skipped (with a warning for future clock skew so clock-recovery snapshots can replace it). Normal stale or per-pool errors on `<existing>` do **not** disable generation protection.

### `quota-fleet gate check` (side-by-side comparison & replay)

```bash
# Compare a shadow v1 snapshot against a past decision timestamp while serve runs on legacy:
quota-fleet gate check \
  --config fleet.yaml \
  --usage-file /var/lib/quota-fleet/usage-v1-shadow.json \
  --format v1 \
  --pool claude \
  --alt-pools codex,agy-argon \
  --now 2026-11-26T20:35:00Z

# Reproduce an empty-primary request with unmeasured alt only:
quota-fleet gate check --config fleet.yaml --usage-file usage-v1.json --format v1 --alt-pools agy-argon
```

- Format precedence: `--format` > `--config` `quota.usage_format` > `"legacy"`.
- Exits `1` whenever snapshot reading/parsing fails or `generated_at > now + 5m` (`snapshot_error`), even in `--json` mode.

## 7. Atomic deployment recipe & tuning `stale_after`

### Exclusive-lock atomic deployment (receiving host)

Never write directly to the destination file or use a fixed temp name. Use a unique temp file in the destination directory, an exclusive `flock`, `usage validate --check-deploy --reject-older-than`, SHA-256 no-op suppression, and `mv -f` (`rename(2)`). See [`examples/usage-generator/atomic_deploy.sh`](../examples/usage-generator/atomic_deploy.sh):

```bash
DEST="/var/lib/quota-fleet/usage-v1.json"
DEST_DIR="$(dirname "$DEST")"
LOCK="${DEST_DIR}/.usage-v1.json.lock"
TMP="$(mktemp "${DEST_DIR}/.usage-v1.json.XXXXXX")"
trap 'rm -f "$TMP"' EXIT

cat > "$TMP"
chmod 0644 "$TMP"

exec 9>"$LOCK"
flock -x 9
if [ -d "$DEST" ]; then
  echo "destination is a directory: $DEST" >&2
  exit 1
fi
quota-fleet usage validate --file "$TMP" --format v1 --check-deploy --reject-older-than "$DEST"

if [ -f "$DEST" ] && cmp -s "$TMP" "$DEST"; then
  exit 0
fi
mv -fT "$TMP" "$DEST"
trap - EXIT
```

### Tuning `stale_after` and migrating from Legacy to v1

1. **Measure end-to-end freshness** over 24–48 hours (48+ cycles) using a shadow v1 feed on the real sync path (or collector completion logs joined by SHA-256). Count only first arrivals where content SHA-256 changes and `generated_at` advances; verify NTP offset on both hosts and discard negative-latency intervals.
2. **Set `quota.stale_after`** to at least `max(T_arrive[i+1] - T_oldest_measured[i]) + 2 * clock_uncertainty + safety_margin` (e.g., 5–10m).
3. **Cut over cleanly**: keep the Legacy feed running in parallel for rollback, update all three settings together (`quota.usage_file`, `quota.usage_format: "v1"`, `quota.gate_mode: "enforce"`), run `quota-fleet config check` and `quota-fleet usage validate --config fleet.yaml` (both `exit 0`), and restart `quota-fleet serve`.
4. **Rollback**: revert all three settings (`usage_file`, `usage_format: "legacy"`, and `gate_mode`) together and restart `quota-fleet serve`.
