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

CODEX_FINGERPRINT_ENV = "CODEX_ACCOUNT_FINGERPRINT_SECRET"


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


def read_runtime_config(path: Path) -> dict[str, Any] | None:
    if not path.exists():
        return None
    try:
        config = read_json(path)
    except (OSError, json.JSONDecodeError):
        try:
            import yaml  # type: ignore[import-not-found]
        except ImportError:
            return None
        with path.open("r", encoding="utf-8") as handle:
            config = yaml.safe_load(handle)
    return config if isinstance(config, dict) else None


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
    key_path = config_path.with_name("management.key")
    file_management_key = (
        key_path.read_text(encoding="utf-8").strip() if key_path.exists() else None
    )
    if file_management_key and file_management_key.startswith(
        ("$2a$", "$2b$", "$2y$")
    ):
        raise ValueError("runtime/management.key must contain the plaintext management key")
    if not config_path.exists():
        return [], file_management_key
    config = read_runtime_config(config_path)
    if isinstance(config, dict):
        router_keys = require_secret_list(config.get("api-keys", []), "existing router keys")
        remote_management = config.get("remote-management")
        key = remote_management.get("secret-key") if isinstance(remote_management, dict) else None
        config_management_key = key if isinstance(key, str) and key else None
        if config_management_key and config_management_key.startswith(
            ("$2a$", "$2b$", "$2y$")
        ):
            config_management_key = None
        return router_keys, file_management_key or config_management_key

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
    if management_key and management_key.startswith(("$2a$", "$2b$", "$2y$")):
        management_key = None
    return require_secret_list(router_keys, "existing router keys"), file_management_key or management_key


def existing_openai_source(
    config: dict[str, Any] | None,
) -> tuple[dict[str, str], dict[str, Any], list[str]]:
    if not config:
        return {}, {}, []

    model_map: dict[str, str] = {}
    upstream_keys: list[str] = []
    providers = config.get("openai-compatibility")
    if isinstance(providers, list):
        for provider in providers:
            if not isinstance(provider, dict):
                continue
            for entry in provider.get("api-key-entries") or []:
                if isinstance(entry, dict) and isinstance(entry.get("api-key"), str):
                    upstream_keys.append(entry["api-key"].strip())
            for model in provider.get("models") or []:
                if isinstance(model, dict):
                    name = model.get("name")
                    alias = model.get("alias")
                    if isinstance(name, str) and isinstance(alias, str) and name != alias:
                        model_map[alias] = name

    claude_entries = config.get("claude-api-key")
    if not upstream_keys and isinstance(claude_entries, list):
        for entry in claude_entries:
            if not isinstance(entry, dict):
                continue
            if isinstance(entry.get("api-key"), str):
                upstream_keys.append(entry["api-key"].strip())
            for model in entry.get("models") or []:
                if isinstance(model, dict):
                    name = model.get("name")
                    alias = model.get("alias")
                    if isinstance(name, str) and isinstance(alias, str) and name != alias:
                        model_map[alias] = name

    instructions = config.get("model-instructions")
    if not isinstance(instructions, dict):
        instructions = {}
    return (
        model_map,
        instructions,
        require_secret_list(list(dict.fromkeys(upstream_keys)), "existing upstream keys"),
    )


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
    if not isinstance(instructions, dict):
        instructions = {}
    instructions = {
        alias: instruction
        for alias, instruction in instructions.items()
        if alias in model_map
    }
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


def atomic_write_secret(path: Path, value: str) -> None:
    path.parent.mkdir(mode=0o700, parents=True, exist_ok=True)
    descriptor, temporary_name = tempfile.mkstemp(
        prefix=f".{path.name}.", dir=path.parent
    )
    temporary_path = Path(temporary_name)
    try:
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(value)
            handle.write("\n")
            handle.flush()
            os.fsync(handle.fileno())
        os.chmod(temporary_path, 0o600)
        os.replace(temporary_path, path)
    finally:
        temporary_path.unlink(missing_ok=True)


def prepare_service_env(path: Path) -> str:
    lines = path.read_text(encoding="utf-8").splitlines() if path.exists() else []
    secret = ""
    preserved: list[str] = []
    prefix = f"{CODEX_FINGERPRINT_ENV}="
    for line in lines:
        if line.startswith(prefix):
            if not secret:
                secret = parse_yaml_scalar(line.split("=", 1)[1])
            continue
        preserved.append(line)
    if len(secret) < 16:
        secret = secrets.token_urlsafe(32)
    preserved.append(f"{prefix}{secret}")
    atomic_write_secret(path, "\n".join(preserved))
    return secret


def main() -> None:
    args = parse_args()
    swaper_config = read_json(args.swaper_dir / "data" / "config.json")
    swaper_upstream_keys = require_secret_list(
        read_json(args.swaper_dir / "data" / "keys.json"), "upstream keys"
    )
    if not isinstance(swaper_config, dict):
        raise ValueError("swaper config must be a JSON object")

    args.runtime_dir.mkdir(mode=0o700, parents=True, exist_ok=True)
    (args.runtime_dir / "auths").mkdir(mode=0o700, exist_ok=True)
    (args.runtime_dir / "logs").mkdir(mode=0o700, exist_ok=True)
    config_path = args.runtime_dir / "config.yaml"
    existing_config = read_runtime_config(config_path)
    existing_router_keys, existing_management_key = existing_runtime_secrets(config_path)
    existing_model_map, existing_instructions, existing_upstream_keys = existing_openai_source(
        existing_config
    )
    if existing_model_map:
        swaper_config["modelMap"] = existing_model_map
    if existing_instructions:
        swaper_config["modelInstructions"] = existing_instructions
    upstream_keys = existing_upstream_keys or swaper_upstream_keys
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
    atomic_write_secret(args.runtime_dir / "management.key", management_key)
    prepare_service_env(args.runtime_dir / "service.env")
    atomic_write(config_path, config)
    print(
        f"Prepared {config_path} with {len(upstream_keys)} upstream credentials, "
        f"{len(router_keys)} router keys, and {len(config['model-instructions'])} instructions."
    )


if __name__ == "__main__":
    main()
