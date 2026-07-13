#!/usr/bin/env python3
"""Create a secret runtime config while migrating from opencode-go-swaper.

The script reads production secrets from local files on the target host and
never prints their values. The output is JSON, which is also valid YAML and is
accepted by CLIProxyAPI's YAML configuration loader.
"""

from __future__ import annotations

import argparse
import json
import os
import secrets
import tempfile
from pathlib import Path
from typing import Any


NATIVE_MODELS = (
    "qwen3.7-plus",
    "deepseek-v4-flash",
    "deepseek-v4-pro",
    "mimo-v2.5",
    "mimo-v2.5-pro",
    "minimax-m3",
)


def parse_args() -> argparse.Namespace:
    parser = argparse.ArgumentParser()
    parser.add_argument("--swaper-dir", required=True, type=Path)
    parser.add_argument(
        "--router-keys-file",
        type=Path,
        help="JSON router key list. When omitted, reuse api-keys from runtime/config.yaml.",
    )
    parser.add_argument("--runtime-dir", required=True, type=Path)
    return parser.parse_args()


def read_json(path: Path) -> Any:
    with path.open("r", encoding="utf-8") as handle:
        return json.load(handle)


def require_secret_list(value: Any, label: str) -> list[str]:
    if not isinstance(value, list):
        raise ValueError(f"{label} must be a JSON list")
    result = [item.strip() for item in value if isinstance(item, str) and item.strip()]
    if len(result) != len(value):
        raise ValueError(f"{label} contains an empty or non-string entry")
    if len(set(result)) != len(result):
        raise ValueError(f"{label} contains duplicate entries")
    return result


def parse_yaml_scalar(value: str) -> str:
    value = value.strip()
    if value.startswith('"'):
        parsed = json.loads(value)
        return parsed if isinstance(parsed, str) else ""
    if value.startswith("'") and value.endswith("'"):
        return value[1:-1].replace("''", "'")
    return value.split(" #", 1)[0].strip()


def existing_runtime_secrets(config_path: Path) -> tuple[list[str], str | None]:
    if not config_path.exists():
        return [], None
    try:
        config = read_json(config_path)
    except (OSError, json.JSONDecodeError):
        config = None
    if isinstance(config, dict):
        router_keys = require_secret_list(config.get("api-keys", []), "existing router keys")
        remote_management = config.get("remote-management")
        key = remote_management.get("secret-key") if isinstance(remote_management, dict) else None
        return router_keys, key if isinstance(key, str) and key else None

    router_keys: list[str] = []
    management_key: str | None = None
    section = ""
    with config_path.open("r", encoding="utf-8") as handle:
        for raw_line in handle:
            stripped = raw_line.strip()
            if not stripped or stripped.startswith("#"):
                continue
            indent = len(raw_line) - len(raw_line.lstrip())
            if indent == 0 and not stripped.startswith("-"):
                section = stripped[:-1] if stripped.endswith(":") else ""
                continue
            if section == "api-keys" and stripped.startswith("-"):
                value = parse_yaml_scalar(stripped[1:])
                if value:
                    router_keys.append(value)
            elif section == "remote-management" and stripped.startswith("secret-key:"):
                value = parse_yaml_scalar(stripped.split(":", 1)[1])
                if value:
                    management_key = value
    return require_secret_list(router_keys, "existing router keys"), management_key


def build_models(model_map: dict[str, str]) -> list[dict[str, Any]]:
    models = [
        {"name": model, "alias": model, "force-mapping": True}
        for model in NATIVE_MODELS
    ]
    for alias, upstream in model_map.items():
        if not isinstance(alias, str) or not alias or not isinstance(upstream, str) or not upstream:
            raise ValueError("modelMap contains an invalid alias mapping")
        models.append({"name": upstream, "alias": alias, "force-mapping": True})
    return models


def build_config(
    swaper_config: dict[str, Any],
    upstream_keys: list[str],
    router_keys: list[str],
    management_key: str,
) -> dict[str, Any]:
    model_map = swaper_config.get("modelMap")
    instructions = swaper_config.get("modelInstructions")
    strip_reasoning = swaper_config.get("stripReasoning")
    if not isinstance(model_map, dict) or not model_map:
        raise ValueError("swaper config has no modelMap")
    if not isinstance(instructions, dict) or set(instructions) != set(model_map):
        raise ValueError("modelInstructions must cover every aliased model")
    if not isinstance(strip_reasoning, dict) or not all(
        strip_reasoning.get(protocol) is True for protocol in ("chat", "responses", "anthropic")
    ):
        raise ValueError("reasoning stripping is not enabled for every legacy protocol")

    models = build_models(model_map)
    openai_provider = {
        "name": "opencode-go",
        "base-url": "https://opencode.ai/zen/go/v1",
        "api-key-entries": [
            {"api-key": key, "proxy-url": ""} for key in upstream_keys
        ],
        "models": models,
    }

    return {
        "smart-management-enabled": True,
        "strip-reasoning": True,
        "host": "",
        "port": 8317,
        "remote-management": {
            "allow-remote": True,
            "secret-key": management_key,
            "disable-control-panel": True,
        },
        "auth-dir": "/root/.cli-proxy-api",
        "api-keys": router_keys,
        "debug": False,
        "logging-to-file": True,
        "logs-max-total-size-mb": 100,
        "usage-statistics-enabled": True,
        "request-retry": 0,
        "max-retry-credentials": 0,
        "max-retry-interval": 0,
        "retry-budget-ms": 300000,
        "credential-attempt-timeout-ms": 180000,
        "disable-cooling": False,
        "save-cooldown-status": True,
        "transient-error-cooldown-seconds": 3600,
        "disable-claude-cloak-mode": True,
        "disable-image-generation": "passthrough",
        "routing": {
            "strategy": "round-robin",
            "session-affinity": True,
            "session-affinity-ttl": "720h",
            "smartapi-affinity": True,
            "smartapi-affinity-ttl": "720h",
        },
        "ws-auth": True,
        "model-instructions": instructions,
        "payload": {
            "override": [
                {
                    "models": [{"name": "qwen3.7-plus", "protocol": "openai"}],
                    "params": {"reasoning_effort": "none"},
                }
            ]
        },
        "openai-compatibility": [openai_provider],
    }


def atomic_write(path: Path, value: dict[str, Any]) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(prefix=".config.", dir=path.parent)
    temporary_path = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            json.dump(value, handle, ensure_ascii=False, indent=2)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary_path, 0o600)
        os.replace(temporary_path, path)
    finally:
        temporary_path.unlink(missing_ok=True)


def main() -> None:
    args = parse_args()
    swaper_config = read_json(args.swaper_dir / "data" / "config.json")
    upstream_keys = require_secret_list(
        read_json(args.swaper_dir / "data" / "keys.json"), "upstream keys"
    )
    if not isinstance(swaper_config, dict):
        raise ValueError("swaper config must be a JSON object")

    args.runtime_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    (args.runtime_dir / "auths").mkdir(mode=0o700, exist_ok=True)
    (args.runtime_dir / "logs").mkdir(mode=0o700, exist_ok=True)
    config_path = args.runtime_dir / "config.yaml"
    existing_router_keys, existing_management_key = existing_runtime_secrets(config_path)
    router_keys = (
        require_secret_list(read_json(args.router_keys_file), "router keys")
        if args.router_keys_file
        else existing_router_keys
    )
    if not router_keys:
        raise ValueError(
            "router keys are required: pass --router-keys-file or provide runtime/config.yaml"
        )
    management_key = existing_management_key or secrets.token_urlsafe(48)
    config = build_config(swaper_config, upstream_keys, router_keys, management_key)
    atomic_write(config_path, config)
    print(
        f"Prepared {config_path} with {len(upstream_keys)} upstream credentials, "
        f"{len(router_keys)} router keys, and {len(config['model-instructions'])} instructions."
    )


if __name__ == "__main__":
    main()
