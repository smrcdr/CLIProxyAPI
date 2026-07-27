package smartrouter

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
)

func TestCanonicalUsageFromJSON(t *testing.T) {
	tests := []struct {
		name     string
		protocol config.RouterProtocol
		body     string
		want     CanonicalUsage
	}{
		{
			name:     "chat",
			protocol: config.RouterProtocolOpenAIChatCompletions,
			body: `{"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,` +
				`"prompt_tokens_details":{"cached_tokens":3},` +
				`"completion_tokens_details":{"reasoning_tokens":2}}}`,
			want: CanonicalUsage{
				InputTokens:     usageToken(10),
				OutputTokens:    usageToken(4),
				CachedTokens:    usageToken(3),
				ReasoningTokens: usageToken(2),
				TotalTokens:     usageToken(14),
			},
		},
		{
			name:     "responses completed event",
			protocol: config.RouterProtocolOpenAIResponses,
			body: `{"type":"response.completed","response":{"usage":{"input_tokens":21,"output_tokens":8,` +
				`"total_tokens":29,"input_tokens_details":{"cached_tokens":5},` +
				`"output_tokens_details":{"reasoning_tokens":6}}}}`,
			want: CanonicalUsage{
				InputTokens:     usageToken(21),
				OutputTokens:    usageToken(8),
				CachedTokens:    usageToken(5),
				ReasoningTokens: usageToken(6),
				TotalTokens:     usageToken(29),
			},
		},
		{
			name:     "anthropic derives total",
			protocol: config.RouterProtocolAnthropicMessages,
			body: `{"usage":{"input_tokens":30,"output_tokens":9,` +
				`"cache_read_input_tokens":7,"cache_creation_input_tokens":2}}`,
			want: CanonicalUsage{
				InputTokens:         usageToken(30),
				OutputTokens:        usageToken(9),
				CacheReadTokens:     usageToken(7),
				CacheCreationTokens: usageToken(2),
				TotalTokens:         usageToken(39),
			},
		},
		{
			name:     "explicit zeros remain known",
			protocol: config.RouterProtocolOpenAIResponses,
			body:     `{"usage":{"input_tokens":0,"output_tokens":0,"total_tokens":0}}`,
			want: CanonicalUsage{
				InputTokens:  usageToken(0),
				OutputTokens: usageToken(0),
				TotalTokens:  usageToken(0),
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, ok := canonicalUsageFromJSON(test.protocol, []byte(test.body))
			if !ok || got == nil {
				t.Fatal("canonicalUsageFromJSON() reported missing usage")
			}
			assertCanonicalUsage(t, got, &test.want)
			if got.Estimated {
				t.Fatal("Estimated = true")
			}
		})
	}
}

func TestCanonicalUsageMissingDoesNotBecomeZero(t *testing.T) {
	usage, ok := canonicalUsageFromJSON(config.RouterProtocolOpenAIResponses, []byte(`{"usage":{}}`))
	if ok || usage != nil {
		t.Fatalf("usage = %#v, ok = %t", usage, ok)
	}
}

func TestStreamUsageAccumulatorMergesSplitAnthropicUsageWithoutSumming(t *testing.T) {
	accumulator := newStreamUsageAccumulator(config.RouterProtocolAnthropicMessages)
	accumulator.Observe([]byte("event: message_start\n"))
	accumulator.Observe([]byte(`data: {"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":0,`))
	accumulator.Observe([]byte(`"cache_read_input_tokens":4}}}` + "\n\n"))
	accumulator.Observe([]byte(`data: {"type":"message_delta","usage":{"output_tokens":7}}` + "\n\n"))

	got, ok := accumulator.Result()
	if !ok || got == nil {
		t.Fatal("Result() reported missing usage")
	}
	want := &CanonicalUsage{
		InputTokens:     usageToken(12),
		OutputTokens:    usageToken(7),
		CacheReadTokens: usageToken(4),
		TotalTokens:     usageToken(19),
	}
	assertCanonicalUsage(t, got, want)
}

func TestStreamUsageAccumulatorAcceptsConsecutiveBareJSONChunks(t *testing.T) {
	accumulator := newStreamUsageAccumulator(config.RouterProtocolAnthropicMessages)
	accumulator.Observe([]byte(`{"type":"message_start","message":{"usage":{"input_tokens":12,"output_tokens":0}}}`))
	accumulator.Observe([]byte(`{"type":"message_delta","usage":{"output_tokens":7}}`))

	got, ok := accumulator.Result()
	if !ok || got == nil {
		t.Fatal("Result() reported missing usage")
	}
	want := &CanonicalUsage{
		InputTokens:  usageToken(12),
		OutputTokens: usageToken(7),
		TotalTokens:  usageToken(19),
	}
	assertCanonicalUsage(t, got, want)
}

func usageToken(value int64) *int64 {
	return &value
}

func assertCanonicalUsage(t *testing.T, got, want *CanonicalUsage) {
	t.Helper()
	assertUsageToken(t, "input", got.InputTokens, want.InputTokens)
	assertUsageToken(t, "output", got.OutputTokens, want.OutputTokens)
	assertUsageToken(t, "cached", got.CachedTokens, want.CachedTokens)
	assertUsageToken(t, "cache read", got.CacheReadTokens, want.CacheReadTokens)
	assertUsageToken(t, "cache creation", got.CacheCreationTokens, want.CacheCreationTokens)
	assertUsageToken(t, "reasoning", got.ReasoningTokens, want.ReasoningTokens)
	assertUsageToken(t, "total", got.TotalTokens, want.TotalTokens)
}

func assertUsageToken(t *testing.T, name string, got, want *int64) {
	t.Helper()
	if got == nil || want == nil {
		if got != nil || want != nil {
			t.Fatalf("%s tokens = %v, want %v", name, got, want)
		}
		return
	}
	if *got != *want {
		t.Fatalf("%s tokens = %d, want %d", name, *got, *want)
	}
}
