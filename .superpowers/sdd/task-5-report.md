# Task 5 report

Implemented the route-state, metrics, and network-policy views in `internal/api/assets/router-management.html`.

- Route diagnostics now expose the complete route-state DTO fields, safe status badges, optional cooldown/probe/reset details, and Probe / Reset Circuit actions.
- Route probe and circuit reset use the management POST endpoints, show only redacted probe/reset DTO fields, confirm resets, and reload state and metrics through the existing revision/stale gates.
- Metrics now render totals, request aggregates, and route aggregates with derived average/max latency and TTFT values, current-snapshot timestamps, stale labeling, and model/endpoint/route/upstream/result client-side filters.
- Network policy now renders controls from the network-policy schema, submits a revision-aware `PATCH /network-policy` with the existing mutation gate, confirms broadened access with a restriction summary, and reloads policy/revision after success.

Focused verification:

- `go test ./internal/api -run 'TestRouterManagementPage' -count=1` — PASS
- `go test ./internal/api/handlers/management -run 'TestRouterManagement' -count=1` — PASS
- Embedded script extraction followed by `node --check` — PASS

No backend, routing, or deployment files were changed.
