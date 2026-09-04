# Task 3 Implementation Report

## Implementation
- Replaced the all-or-nothing loader with parallel `Promise.all` loading using per-request settled wrappers for state, upstreams, model groups, route states, metrics, policy, and all three schemas. Responses are normalized by their endpoint envelopes, revisions are collected from successful responses, and failed resources remain visible without replacing prior data with fabricated success values.
- Added overview metrics for requests, success rate, attempts, failovers, active routes, open circuits, average latency, and average TTFT. Request-row denominators are filtered by `latency_count` and `ttft_count`; zero denominators render an em dash. Model-group and route-health summaries use DOM text nodes.
- Added an upstream table with host-only URL display, capability badges, health and route usage summaries, enabled state, and action controls.
- Added schema-backed protocol, auth, and endpoint choices plus supported upstream fields for base URL, headers, health checks, trusted pool, affinity forwarding, and capabilities. Edit forms leave write-only secrets blank and omit `auth.secret` unless a replacement is entered.
- Added revision-aware upstream create/edit, probe, enable/disable, and delete operations. Mutations use the existing API client and conflict gate; enable/disable/delete require confirmation; probe output only displays the probe metadata and never response body data. Validation errors leave the editor open.

## Tests
- `go test ./internal/api -run 'TestRouterManagementPage' -count=1` — PASS.
- `go test ./internal/api/handlers/management -run 'TestRouterManagement' -count=1` — PASS.
- Embedded JavaScript parsed successfully with Node `new Function` syntax check.
- Browser smoke check loaded the embedded page, rendered a hostile upstream name as text (no script nodes), displayed a host-only URL, and verified an edit form omits the blank write-only secret.

## Self-review
- Only `internal/api/assets/router-management.html` was changed in production code. Existing authentication, sessionStorage, polling, stale semantics, and 409 conflict behavior remain in the shared client architecture.
- Server-controlled values are assigned through `textContent` or created text nodes; the only table `innerHTML` is a constant header template.
- Mutation payloads use fields supported by `routerUpstreamInput`; immutable IDs are sent only on create and write-only secrets are never rendered from DTO data.

## Concerns
- The embedded asset has no JavaScript unit-test harness; full browser/API CRUD interaction remains best verified by the integration/runtime verification pass.
- Schema responses currently describe the upstream capability object at a high level, so nested health/capability controls follow the exact fields exposed by the existing DTO/input contract rather than inventing additional schema fields.

## Review Fixes
- Added a zero-safe `average` helper that aggregates metric totals by their count fields, formats millisecond averages, and returns an em dash for zero observations.
- Unwrapped the upstream probe response from `data.probe` and restricted rendering to the allowlisted probe DTO fields.
- Made `auth.header` explicit in every upstream payload: the entered header is retained only for `api-key-header`; all other auth types send an empty header so PATCH clears stale values.
- Made health-check payloads explicit. Empty mode normalizes to `none`, and `none` sends empty path/interval plus zero thresholds, clearing prior HTTP health-check settings; HTTP values remain supported.
- Routed upstream save, test, enable/disable, and delete errors through `handleApiError`, including the existing 401 logout/session removal and 409 stale/conflict handling. Validation errors remain local to the editor.

## Review Fix Verification
- `go test ./internal/api -run 'TestRouterManagementPage' -count=1` — PASS (`ok github.com/router-for-me/CLIProxyAPI/v7/internal/api 0.008s`).
- `go test ./internal/api/handlers/management -run 'TestRouterManagement' -count=1` — PASS (`ok github.com/router-for-me/CLIProxyAPI/v7/internal/api/handlers/management 0.009s`).
- `node -e 'const fs=require("fs");const html=fs.readFileSync("internal/api/assets/router-management.html","utf8");const script=html.match(/<script>([\s\S]*)<\/script>/)[1];new Function(script);console.log("embedded JavaScript parsed successfully")'` — PASS (`embedded JavaScript parsed successfully`).

## Review Self-review
- Production scope remains limited to the embedded router-management asset; no backend routing or DTO/input contracts were changed.
- Secret values remain write-only in the form and are sent only when newly entered; the management key remains sessionStorage-backed and 401 handling removes it.
- Existing mutation gating and revision/conflict behavior remains centralized in `api` and `handleApiError`; only action error dispatch was consolidated.
- No browser smoke check was rerun because the requested verification scope was limited to the two targeted Go tests and embedded-JS syntax check.
