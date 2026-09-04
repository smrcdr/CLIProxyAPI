package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveConfigPreserveCommentsPrunesDeletedModelInstructions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	original := []byte(`model-instructions:
  gpt-5.5:
    enabled: true
    mode: prepend
    prompt: alias
  qwen3.7-plus:
    enabled: true
    mode: append
    prompt: upstream
`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	cfg := &Config{ModelInstructions: map[string]ModelInstruction{
		"qwen3.7-plus": {Enabled: true, Mode: "append", Prompt: "upstream"},
	}}
	if err := SaveConfigPreserveComments(path, cfg); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	reloaded, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if _, ok := reloaded.ModelInstructions["gpt-5.5"]; ok {
		t.Fatal("deleted alias instruction was restored from the original YAML")
	}
	if got := reloaded.ModelInstructions["qwen3.7-plus"].Prompt; got != "upstream" {
		t.Fatalf("remaining instruction prompt = %q, want upstream", got)
	}
}

func TestSaveConfigPreserveCommentsPersistsEmptyModelInstructions(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "config.yaml")
	if err := os.WriteFile(path, []byte(`model-instructions:
  gpt-5.5:
    enabled: true
    mode: prepend
    prompt: alias
`), 0o600); err != nil {
		t.Fatalf("write config: %v", err)
	}

	if err := SaveConfigPreserveComments(path, &Config{ModelInstructions: map[string]ModelInstruction{}}); err != nil {
		t.Fatalf("SaveConfigPreserveComments() error = %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read config: %v", err)
	}
	reloaded, err := ParseConfigBytes(data)
	if err != nil {
		t.Fatalf("ParseConfigBytes() error = %v", err)
	}
	if len(reloaded.ModelInstructions) != 0 {
		t.Fatalf("model instructions after deleting all = %#v, want empty", reloaded.ModelInstructions)
	}
}
