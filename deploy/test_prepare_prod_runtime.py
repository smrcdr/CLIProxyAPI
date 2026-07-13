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


if __name__ == "__main__":
    unittest.main()
