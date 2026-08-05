#!/usr/bin/env python3
"""Enable the SmartAPI management console in an existing runtime config."""

from __future__ import annotations

import argparse
import datetime
import json
import os
from pathlib import Path
import re
import secrets
import shutil

import yaml


def replace_management_settings(config_path: Path, management_key: str) -> None:
    lines = config_path.read_text(encoding="utf-8").splitlines(keepends=True)
    found_enabled = False
    remote_start: int | None = None
    remote_end = len(lines)

    for index, line in enumerate(lines):
        if re.match(r"^smart-management-enabled\s*:", line):
            lines[index] = "smart-management-enabled: true\n"
            found_enabled = True
        if re.match(r"^remote-management\s*:", line):
            remote_start = index
            continue
        if (
            remote_start is not None
            and index > remote_start
            and line.strip()
            and not line.startswith((" ", "\t", "#"))
        ):
            remote_end = index
            break

    if not found_enabled or remote_start is None:
        raise ValueError("required management config fields are missing")

    found_allow_remote = False
    found_secret_key = False
    for index in range(remote_start + 1, remote_end):
        indent = lines[index][: len(lines[index]) - len(lines[index].lstrip())]
        if re.match(r"^\s+allow-remote\s*:", lines[index]):
            lines[index] = f"{indent}allow-remote: true\n"
            found_allow_remote = True
        elif re.match(r"^\s+secret-key\s*:", lines[index]):
            lines[index] = f"{indent}secret-key: {json.dumps(management_key)}\n"
            found_secret_key = True

    if not found_allow_remote or not found_secret_key:
        raise ValueError("required remote-management fields are missing")

    updated = "".join(lines)
    parsed = yaml.safe_load(updated)
    if parsed.get("smart-management-enabled") is not True:
        raise ValueError("smart-management-enabled validation failed")
    remote_management = parsed.get("remote-management", {})
    if remote_management.get("allow-remote") is not True:
        raise ValueError("allow-remote validation failed")
    if remote_management.get("secret-key") != management_key:
        raise ValueError("secret-key validation failed")

    temporary_path = config_path.with_name(f".{config_path.name}.smart-management.tmp")
    descriptor = os.open(
        temporary_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600
    )
    with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
        handle.write(updated)
    os.replace(temporary_path, config_path)
    os.chmod(config_path, 0o600)


def main() -> None:
    parser = argparse.ArgumentParser()
    parser.add_argument("runtime_dir", type=Path)
    arguments = parser.parse_args()

    runtime_dir = arguments.runtime_dir.resolve()
    config_path = runtime_dir / "config.yaml"
    key_path = runtime_dir / "management.key"
    if not config_path.is_file():
        raise SystemExit(f"runtime config is missing: {config_path}")

    timestamp = datetime.datetime.now(datetime.timezone.utc).strftime(
        "%Y%m%dT%H%M%SZ"
    )
    backup_path = runtime_dir / f"config.yaml.backup-smart-management-{timestamp}"
    shutil.copy2(config_path, backup_path)
    os.chmod(backup_path, 0o600)

    if key_path.exists():
        management_key = key_path.read_text(encoding="utf-8").strip()
        if not management_key or management_key.startswith(("$2a$", "$2b$", "$2y$")):
            raise SystemExit("management.key is not a usable plaintext key")
    else:
        management_key = secrets.token_urlsafe(48)
        descriptor = os.open(
            key_path, os.O_WRONLY | os.O_CREAT | os.O_EXCL, 0o600
        )
        with os.fdopen(descriptor, "w", encoding="utf-8") as handle:
            handle.write(management_key + "\n")
    os.chmod(key_path, 0o600)

    replace_management_settings(config_path, management_key)
    print(f"backup={backup_path.name}")
    print("smart-management-enabled=true")
    print("allow-remote=true (management key required)")
    print("management-key-file=present mode=0600")


if __name__ == "__main__":
    main()
