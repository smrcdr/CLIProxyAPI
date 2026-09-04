package helps

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

func TestApplyModelInstructionModes(t *testing.T) {
	base := []byte(`{"system":[{"type":"text","text":"client"}],"messages":[{"role":"user","content":"hello"}]}`)
	tests := []struct {
		name string
		mode string
		want []string
	}{
		{name: "prepend", mode: "prepend", want: []string{"proxy", "client"}},
		{name: "append", mode: "append", want: []string{"client", "proxy"}},
		{name: "override", mode: "override", want: []string{"proxy"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{ModelInstructions: map[string]config.ModelInstruction{
				"alias": {Enabled: true, Mode: tt.mode, Prompt: "proxy"},
			}}
			got := ApplyModelInstruction(cfg, "alias", "upstream", base)
			for i, want := range tt.want {
				if value := gjson.GetBytes(got, "system."+string(rune('0'+i))+".text").String(); value != want {
					t.Fatalf("system[%d] = %q, want %q; payload=%s", i, value, want, got)
				}
			}
			if count := int(gjson.GetBytes(got, "system.#").Int()); count != len(tt.want) {
				t.Fatalf("system count = %d, want %d", count, len(tt.want))
			}
		})
	}
}

func TestApplyModelInstructionUsesUpstreamFallbackAndNoops(t *testing.T) {
	base := []byte(`{"messages":[]}`)
	cfg := &config.Config{ModelInstructions: map[string]config.ModelInstruction{
		"upstream": {Enabled: true, Prompt: "proxy"},
	}}
	if got := gjson.GetBytes(ApplyModelInstruction(cfg, "alias", "upstream", base), "system.0.text").String(); got != "proxy" {
		t.Fatalf("system text = %q", got)
	}
	cfg.ModelInstructions["upstream"] = config.ModelInstruction{Enabled: false, Prompt: "proxy"}
	if got := ApplyModelInstruction(cfg, "alias", "upstream", base); string(got) != string(base) {
		t.Fatalf("disabled instruction changed payload: %s", got)
	}
}

func TestApplyOpenAIModelInstructionModes(t *testing.T) {
	base := []byte(`{"messages":[{"role":"system","content":"client"},{"role":"user","content":"hello"}]}`)
	tests := []struct {
		name string
		mode string
		want []string
	}{
		{name: "prepend", mode: "prepend", want: []string{"proxy", "client", "hello"}},
		{name: "append", mode: "append", want: []string{"client", "proxy", "hello"}},
		{name: "override", mode: "override", want: []string{"proxy", "hello"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{ModelInstructions: map[string]config.ModelInstruction{
				"alias": {Enabled: true, Mode: tt.mode, Prompt: "proxy"},
			}}
			got := ApplyOpenAIModelInstruction(cfg, "alias", "upstream", base)
			for i, want := range tt.want {
				if value := gjson.GetBytes(got, "messages."+string(rune('0'+i))+".content").String(); value != want {
					t.Fatalf("messages[%d].content = %q, want %q; payload=%s", i, value, want, got)
				}
			}
			if count := int(gjson.GetBytes(got, "messages.#").Int()); count != len(tt.want) {
				t.Fatalf("message count = %d, want %d", count, len(tt.want))
			}
		})
	}
}

func TestApplyOpenAIModelInstructionUsesUpstreamFallbackAndNoops(t *testing.T) {
	base := []byte(`{"messages":[]}`)
	cfg := &config.Config{ModelInstructions: map[string]config.ModelInstruction{
		"upstream": {Enabled: true, Prompt: "proxy"},
	}}
	if got := gjson.GetBytes(ApplyOpenAIModelInstruction(cfg, "alias", "upstream", base), "messages.0.content").String(); got != "proxy" {
		t.Fatalf("system content = %q", got)
	}
	cfg.ModelInstructions["upstream"] = config.ModelInstruction{Enabled: false, Prompt: "proxy"}
	if got := ApplyOpenAIModelInstruction(cfg, "alias", "upstream", base); string(got) != string(base) {
		t.Fatalf("disabled instruction changed payload: %s", got)
	}
}
