# Codex Astra Model Integration Implementation Plan

> **For agentic workers:** REQUIRED SUB-SKILL: Use superpowers:subagent-driven-development (recommended) or superpowers:executing-plans to implement this plan task-by-task. Steps use checkbox (`- [ ]`) syntax for tracking.

**Goal:** Expose `gpt-6-astra` through the Codex model registry for every eligible Codex OAuth account and preserve it across catalog refreshes.

**Architecture:** Add Astra as an intentional local Codex Pro override in the registry layer. The override is applied whenever a catalog is loaded, so the embedded catalog and every remote refresh contain the model without disabling refreshes for other models. Existing Codex OAuth credentials and routing remain unchanged.

**Tech Stack:** Go, embedded JSON model catalog, `go test`, Docker Compose production deployment.

## Global Constraints

- Use the official model ID `gpt-6-astra`.
- Do not modify Codex OAuth files or add credentials.
- Preserve existing remote model refresh behavior for all other providers and models.
- Do not change `prod-pro`.
- Do not mutate production until explicit confirmation immediately before the restart/deploy command.

---

### Task 1: Add Astra local catalog override

**Files:**
- Modify: `internal/registry/model_definitions.go`
- Modify: `internal/registry/model_updater.go`
- Test: `internal/registry/model_definitions_test.go`
- Test: `internal/registry/model_updater_test.go`

**Interfaces:**
- Produces `codexAstraModelInfo() *ModelInfo`, returning a fresh Astra metadata object.
- Produces `applyLocalModelOverrides(*staticModelsJSON)`, which inserts Astra into `codex-pro` only when absent and leaves an existing entry unchanged.
- `loadModelsFromBytes` calls the override before validation and storage.

- [ ] **Step 1: Write failing metadata tests**

Add tests that call `GetCodexProModels`, find `gpt-6-astra`, and assert `OwnedBy`, `Type`, `ContextLength`, `MaxCompletionTokens`, input/output modalities, tools, and exact reasoning levels `low`, `medium`, `high`, `xhigh`, `max`.

- [ ] **Step 2: Write failing refresh-retention test**

Add a test with a minimal valid `staticModelsJSON` encoded without Astra, load it through `loadModelsFromBytes`, and assert `LookupStaticModelInfo("gpt-6-astra")` is non-nil afterward. Restore the embedded catalog state with test cleanup.

- [ ] **Step 3: Implement the local model definition**

Define the model metadata once in `model_definitions.go` using the official values and return a new object so registry callers cannot mutate shared state.

- [ ] **Step 4: Implement refresh-safe insertion**

In `model_updater.go`, append the local Astra definition to `data.CodexPro` only if no matching ID exists. Call this helper from `loadModelsFromBytes` before `validateModelsCatalog` and before assigning the parsed catalog to `modelsCatalogStore`.

- [ ] **Step 5: Run focused registry tests**

Run:

```bash
go test ./internal/registry -run 'Test(Codex|LoadModels|LocalModel|Lookup)'
```

Expected: PASS, including Astra metadata and refresh-retention tests.

- [ ] **Step 6: Commit implementation**

```bash
git add internal/registry/model_definitions.go internal/registry/model_updater.go internal/registry/model_definitions_test.go internal/registry/model_updater_test.go
git commit -m "feat(registry): add Codex Astra model"
```

### Task 2: Verify model exposure locally

**Files:**
- Inspect only: `internal/api/server.go`, `cmd/server/main.go`, `Dockerfile`, `deploy/docker-compose.prod.yml`

**Interfaces:**
- Consumes the registry model definition from Task 1.
- Produces evidence that the server's OpenAI model list includes `gpt-6-astra` and that the production image can be rebuilt without configuration changes.

- [ ] **Step 1: Run all registry tests**

```bash
go test ./internal/registry
```

Expected: PASS.

- [ ] **Step 2: Build the production binary/image locally**

Use the repository's documented build command after checking `README.md`, `Dockerfile`, and the existing deployment files. Do not contact or mutate prod during this task.

- [ ] **Step 3: Run a local server smoke test**

Launch the built server with an isolated temporary auth/config directory, call its authenticated `/v1/models`, and assert the response contains `gpt-6-astra`. Do not send an inference request with a real account during local smoke testing.

- [ ] **Step 4: Commit verification-only fixes if needed**

If the smoke test exposes a routing issue, fix the smallest affected source and rerun the focused tests before continuing. Do not broaden scope to unrelated providers.

### Task 3: Prepare and verify production rollout

**Files:**
- Inspect only: remote `/home/smarer/coding/SmartCLIProxy`
- No server files are edited until approval.

**Interfaces:**
- Consumes the committed branch and existing `prod` profile.
- Produces a reviewed command sequence: fetch/pull, build/recreate `smart-cli-proxy-prod`, and verify `/v1/models` plus a minimal Astra request through the existing Codex pool.

- [ ] **Step 1: Confirm local branch and remote commit**

Verify the implementation commits are present and pushed to the configured GitHub SSH remote before any server mutation.

- [ ] **Step 2: Inspect the exact production compose invocation**

Read the remote deployment files and current container labels; prepare commands that update only `smart-cli-proxy-prod` and preserve mounted runtime/auth data.

- [ ] **Step 3: Present the mutation commands and request immediate confirmation**

Show the exact production mutation commands, including the container recreate/restart and verification commands. Do not run them without confirmation.

- [ ] **Step 4: Deploy after confirmation**

Pull the approved commit, rebuild/recreate only `smart-cli-proxy-prod`, and leave Caddy, DNS, auth files, and unrelated containers untouched.

- [ ] **Step 5: Verify production behavior**

Call the production proxy's authenticated `/v1/models` and confirm `gpt-6-astra` is listed. Send one minimal request selecting `gpt-6-astra`; record status and response shape without printing tokens.

- [ ] **Step 6: Commit or record no further changes**

The source commits remain the deployment record; do not create a second configuration-only commit for runtime secrets.
