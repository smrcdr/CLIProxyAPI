import importlib.util
import json
import tempfile
import unittest
from pathlib import Path


MODULE_PATH = Path(__file__).with_name("prepare-prod-runtime.py")
SPEC = importlib.util.spec_from_file_location("prepare_prod_runtime", MODULE_PATH)
assert SPEC is not None and SPEC.loader is not None
prepare = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(prepare)


class PrepareRuntimeTest(unittest.TestCase):
    def test_build_config_uses_openai_chat_for_aliases(self) -> None:
        model_map = {
            "gpt-5.5": "qwen3.7-plus",
            "opus-4.8": "qwen3.7-plus",
        }
        swaper_config = {
            "modelMap": model_map,
            "modelInstructions": {
                alias: {"enabled": True, "mode": "prepend", "prompt": alias}
                for alias in model_map
            },
            "stripReasoning": {"chat": True, "responses": True, "anthropic": True},
        }

        config = prepare.build_config(
            swaper_config,
            ["upstream-one", "upstream-two"],
            ["router-one"],
            "management-key",
        )

        self.assertNotIn("claude-api-key", config)
        providers = config["openai-compatibility"]
        self.assertEqual(len(providers), 1)
        self.assertEqual(providers[0]["base-url"], "https://opencode.ai/zen/go/v1")
        self.assertEqual(
            [entry["api-key"] for entry in providers[0]["api-key-entries"]],
            ["upstream-one", "upstream-two"],
        )
        aliases = {model["alias"]: model["name"] for model in providers[0]["models"]}
        self.assertEqual(aliases["gpt-5.5"], "qwen3.7-plus")
        self.assertEqual(aliases["opus-4.8"], "qwen3.7-plus")
        self.assertEqual(
            config["payload"]["override"][0],
            {
                "models": [{"name": "qwen3.7-plus", "protocol": "openai"}],
                "params": {"reasoning_effort": "none"},
            },
        )

    def test_existing_runtime_secrets_support_yaml_and_json(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.yaml"
            path.write_text(
                """remote-management:
  secret-key: 'management-yaml'
api-keys:
- router-one
- "router-two"
debug: false
""",
                encoding="utf-8",
            )
            self.assertEqual(
                prepare.existing_runtime_secrets(path),
                (["router-one", "router-two"], "management-yaml"),
            )

            path.write_text(
                json.dumps(
                    {
                        "remote-management": {"secret-key": "management-json"},
                        "api-keys": ["router-three"],
                    }
                ),
                encoding="utf-8",
            )
            self.assertEqual(
                prepare.existing_runtime_secrets(path),
                (["router-three"], "management-json"),
            )

    def test_management_key_file_takes_precedence_over_hashed_config(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "config.yaml"
            path.write_text(
                json.dumps(
                    {
                        "remote-management": {"secret-key": "$2b$hashed-value"},
                        "api-keys": ["router-one"],
                    }
                ),
                encoding="utf-8",
            )
            (path.parent / "management.key").write_text(
                "management-plaintext\n",
                encoding="utf-8",
            )

            self.assertEqual(
                prepare.existing_runtime_secrets(path),
                (["router-one"], "management-plaintext"),
            )

    def test_atomic_write_secret_uses_owner_only_permissions(self) -> None:
        with tempfile.TemporaryDirectory() as directory:
            path = Path(directory) / "management.key"
            prepare.atomic_write_secret(path, "management-plaintext")

            self.assertEqual(path.read_text(encoding="utf-8"), "management-plaintext\n")
            self.assertEqual(path.stat().st_mode & 0o777, 0o600)

    def test_existing_openai_source_migrates_claude_runtime(self) -> None:
        existing_config = {
            "model-instructions": {
                "gpt-5.5": {
                    "enabled": True,
                    "mode": "prepend",
                    "prompt": "GPT instruction",
                },
                "opus-4.8": {
                    "enabled": True,
                    "mode": "prepend",
                    "prompt": "Claude instruction",
                },
            },
            "claude-api-key": [
                {
                    "api-key": "upstream-one",
                    "models": [
                        {
                            "name": "qwen3.7-plus",
                            "alias": "gpt-5.5",
                            "force-mapping": True,
                        },
                        {
                            "name": "qwen3.7-plus",
                            "alias": "opus-4.8",
                            "force-mapping": True,
                        },
                        {
                            "name": "qwen3.7-plus",
                            "alias": "qwen3.7-plus",
                            "force-mapping": True,
                        },
                    ],
                },
                {"api-key": "upstream-two", "models": []},
            ],
        }

        model_map, instructions, upstream_keys = prepare.existing_openai_source(
            existing_config
        )

        self.assertEqual(
            model_map,
            {"gpt-5.5": "qwen3.7-plus", "opus-4.8": "qwen3.7-plus"},
        )
        self.assertEqual(instructions, existing_config["model-instructions"])
        self.assertEqual(upstream_keys, ["upstream-one", "upstream-two"])

        migrated = prepare.build_config(
            {
                "modelMap": model_map,
                "modelInstructions": instructions,
                "stripReasoning": {
                    "chat": True,
                    "responses": True,
                    "anthropic": True,
                },
            },
            upstream_keys,
            ["router-key"],
            "management-key",
        )
        self.assertNotIn("claude-api-key", migrated)
        aliases = {
            model["alias"]: model["name"]
            for model in migrated["openai-compatibility"][0]["models"]
        }
        self.assertEqual(aliases["gpt-5.5"], "qwen3.7-plus")
        self.assertEqual(aliases["opus-4.8"], "qwen3.7-plus")
        self.assertEqual(migrated["model-instructions"], instructions)


if __name__ == "__main__":
    unittest.main()
