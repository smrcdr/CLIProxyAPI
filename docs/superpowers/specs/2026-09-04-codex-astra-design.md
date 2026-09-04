# Codex Astra model integration

## Goal

Expose the Codex model `gpt-6-astra` through SmartCLIProxy for every available Codex OAuth account, including the existing production accounts. The integration must survive the background model-catalog refresh and must not alter account credentials.

## Current context

- Production is the `prod` profile.
- Codex OAuth files already exist for multiple accounts and are managed by SmartCLIProxy.
- The embedded catalog uses the `codex-pro` section for Pro-tier model metadata.
- SmartCLIProxy refreshes its model catalog from the shared `router-for-me/models` repository at startup and every three hours.
- The shared remote catalog currently does not contain `gpt-6-astra`.
- The user has verified that Astra is visible and responds in Codex on all accounts.

## Design

Add one global `gpt-6-astra` definition to the Codex Pro catalog. Do not add per-account aliases or modify OAuth files. The model remains available to all Codex credentials that are eligible at runtime.

The definition uses the official model ID and metadata:

- `id`: `gpt-6-astra`
- `object`: `model`
- `owned_by`: `openai`
- `type`: `openai`
- text and image input; text output
- context length `1,050,000`
- maximum completion tokens `128,000`
- tools supported
- reasoning levels `low`, `medium`, `high`, `xhigh`, `max`

Because the shared catalog refresh would otherwise replace the embedded catalog and remove a local-only model, preserve the Astra entry when applying a refreshed Codex catalog. The merge is limited to this intentional local model and does not disable or bypass refreshes for other providers or models.

## Routing and failure behavior

- Requests use the existing Codex OAuth transport and the selected account's existing token.
- No API key or new credential is introduced.
- If a Codex account is not entitled to Astra, normal provider failure/cooldown behavior remains in effect; the model is not silently redirected to another model.
- The model list exposes the official ID without a new alias.

## Verification

- Unit coverage verifies the Astra definition is present in the Codex Pro catalog with the required metadata and reasoning levels.
- Unit coverage verifies a remote catalog refresh retains the local Astra entry.
- Local smoke verification checks the built server's model list.
- Production verification, after explicit confirmation immediately before mutation, checks the deployed `/v1/models` result and sends a minimal request using `gpt-6-astra` through the existing Codex account pool.

## Scope exclusions

- No changes to Codex OAuth credentials or account files.
- No changes to OpenCode Zen configuration.
- No new provider, API key, billing configuration, or public listener.
- No changes to `prod-pro`.
