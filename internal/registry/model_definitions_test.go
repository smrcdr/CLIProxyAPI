package registry

import "testing"

func TestGetCodexProModelsIncludesAstraMetadata(t *testing.T) {
	var astra *ModelInfo
	for _, model := range GetCodexProModels() {
		if model != nil && model.ID == "gpt-6-astra" {
			astra = model
			break
		}
	}
	if astra == nil {
		t.Fatal("GetCodexProModels() does not include gpt-6-astra")
	}
	if astra.OwnedBy != "openai" || astra.Type != "openai" {
		t.Fatalf("Astra ownership/type = %q/%q, want openai/openai", astra.OwnedBy, astra.Type)
	}
	if astra.ContextLength != 1050000 || astra.MaxCompletionTokens != 128000 {
		t.Fatalf("Astra limits = %d/%d, want 1050000/128000", astra.ContextLength, astra.MaxCompletionTokens)
	}
	if !equalStrings(astra.SupportedInputModalities, []string{"text", "image"}) {
		t.Fatalf("Astra input modalities = %#v, want text/image", astra.SupportedInputModalities)
	}
	if !equalStrings(astra.SupportedOutputModalities, []string{"text"}) {
		t.Fatalf("Astra output modalities = %#v, want text", astra.SupportedOutputModalities)
	}
	if !equalStrings(astra.SupportedParameters, []string{"tools"}) {
		t.Fatalf("Astra supported parameters = %#v, want tools", astra.SupportedParameters)
	}
	if astra.Thinking == nil || !equalStrings(astra.Thinking.Levels, []string{"low", "medium", "high", "xhigh", "max"}) {
		t.Fatalf("Astra reasoning levels = %#v, want low/medium/high/xhigh/max", astra.Thinking)
	}
}

func equalStrings(got, want []string) bool {
	if len(got) != len(want) {
		return false
	}
	for i := range got {
		if got[i] != want[i] {
			return false
		}
	}
	return true
}

func TestModelOverrideHeadersFromEmbeddedModels(t *testing.T) {
	const wantUA = "codex-tui/0.144.0 (Mac OS 26.5.1; arm64) iTerm.app/3.6.11 (codex-tui; 0.144.0)"
	got := ModelOverrideHeaders("gpt-5.6-luna")
	if got == nil {
		t.Fatal("ModelOverrideHeaders(gpt-5.6-luna) = nil, want headers")
	}
	if got["user-agent"] != wantUA {
		t.Fatalf("user-agent = %q, want %q", got["user-agent"], wantUA)
	}
	if got := ModelOverrideHeaders("gpt-5.4"); got != nil {
		t.Fatalf("ModelOverrideHeaders(gpt-5.4) = %#v, want nil", got)
	}
}

func TestWithXAIBuiltinsIncludesVideoPreviewModel(t *testing.T) {
	models := WithXAIBuiltins(nil)

	for _, model := range models {
		if model == nil {
			continue
		}
		if model.ID == xaiBuiltinVideo15PreviewModelID {
			return
		}
	}

	t.Fatalf("expected xAI builtin model %s", xaiBuiltinVideo15PreviewModelID)
}

func TestAntigravityWebSearchModelForRequiresRequestedModelCapability(t *testing.T) {
	registryRef := GetGlobalRegistry()
	registryRef.RegisterClient("test-antigravity-websearch-route", "antigravity", []*ModelInfo{
		{ID: "gemini-route-test"},
		{ID: "gemini-web-search-test", SupportsWebSearch: true},
	})
	registryRef.RegisterClient("test-gemini-websearch-route", "gemini", []*ModelInfo{
		{ID: "gemini-cross-provider-route"},
		{ID: "gemini-cross-provider-search", SupportsWebSearch: true},
	})
	t.Cleanup(func() {
		registryRef.UnregisterClient("test-antigravity-websearch-route")
		registryRef.UnregisterClient("test-gemini-websearch-route")
	})

	if got := AntigravityWebSearchModelFor("gemini-route-test"); got != "" {
		t.Fatalf("route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-route-test(high)"); got != "" {
		t.Fatalf("suffix route model without web search support should not get fallback model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-web-search-test"); got != "gemini-web-search-test" {
		t.Fatalf("AntigravityWebSearchModelFor capable model = %q, want itself", got)
	}
	if got := AntigravityWebSearchModelFor("gemini-cross-provider-route"); got != "" {
		t.Fatalf("cross-provider model should not get Antigravity web search model, got %q", got)
	}
	if got := AntigravityWebSearchModelFor("unknown-model"); got != "" {
		t.Fatalf("unknown model should not get Antigravity web search model, got %q", got)
	}
}
