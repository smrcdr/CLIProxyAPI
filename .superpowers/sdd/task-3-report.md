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
