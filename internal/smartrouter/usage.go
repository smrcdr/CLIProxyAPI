package smartrouter

import (
	"bytes"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

// CanonicalUsage is the protocol-neutral usage reported by the successful
// route. Nil fields mean the upstream did not report that value.
type CanonicalUsage struct {
	InputTokens         *int64 `json:"input_tokens,omitempty"`
	OutputTokens        *int64 `json:"output_tokens,omitempty"`
	CachedTokens        *int64 `json:"cached_tokens,omitempty"`
	CacheReadTokens     *int64 `json:"cache_read_tokens,omitempty"`
	CacheCreationTokens *int64 `json:"cache_creation_tokens,omitempty"`
	ReasoningTokens     *int64 `json:"reasoning_tokens,omitempty"`
	TotalTokens         *int64 `json:"total_tokens,omitempty"`
	Estimated           bool   `json:"estimated"`
}

func canonicalUsageFromJSON(protocol config.RouterProtocol, payload []byte) (*CanonicalUsage, bool) {
	if !gjson.ValidBytes(payload) {
		return nil, false
	}
	root := gjson.ParseBytes(payload)
	var usageRoot gjson.Result
	switch protocol {
	case config.RouterProtocolOpenAIChatCompletions:
		usageRoot = root.Get("usage")
	case config.RouterProtocolOpenAIResponses:
		usageRoot = firstExisting(root.Get("usage"), root.Get("response.usage"))
	case config.RouterProtocolAnthropicMessages:
		usageRoot = firstExisting(root.Get("usage"), root.Get("message.usage"))
	default:
		return nil, false
	}
	if !usageRoot.Exists() || !usageRoot.IsObject() {
		return nil, false
	}

	usage := &CanonicalUsage{}
	switch protocol {
	case config.RouterProtocolOpenAIChatCompletions:
		usage.InputTokens = tokenValue(usageRoot.Get("prompt_tokens"))
		usage.OutputTokens = tokenValue(usageRoot.Get("completion_tokens"))
		usage.TotalTokens = tokenValue(usageRoot.Get("total_tokens"))
		usage.CachedTokens = tokenValue(usageRoot.Get("prompt_tokens_details.cached_tokens"))
		usage.ReasoningTokens = tokenValue(usageRoot.Get("completion_tokens_details.reasoning_tokens"))
	case config.RouterProtocolOpenAIResponses:
		usage.InputTokens = tokenValue(usageRoot.Get("input_tokens"))
		usage.OutputTokens = tokenValue(usageRoot.Get("output_tokens"))
		usage.TotalTokens = tokenValue(usageRoot.Get("total_tokens"))
		usage.CachedTokens = tokenValue(usageRoot.Get("input_tokens_details.cached_tokens"))
		usage.ReasoningTokens = tokenValue(usageRoot.Get("output_tokens_details.reasoning_tokens"))
	case config.RouterProtocolAnthropicMessages:
		usage.InputTokens = tokenValue(usageRoot.Get("input_tokens"))
		usage.OutputTokens = tokenValue(usageRoot.Get("output_tokens"))
		usage.CacheReadTokens = tokenValue(usageRoot.Get("cache_read_input_tokens"))
		usage.CacheCreationTokens = tokenValue(usageRoot.Get("cache_creation_input_tokens"))
	}
	if usage.TotalTokens == nil && usage.InputTokens != nil && usage.OutputTokens != nil {
		total := *usage.InputTokens + *usage.OutputTokens
		usage.TotalTokens = &total
	}
	if !usage.present() {
		return nil, false
	}
	return usage, true
}

func firstExisting(values ...gjson.Result) gjson.Result {
	for _, value := range values {
		if value.Exists() {
			return value
		}
	}
	return gjson.Result{}
}

func tokenValue(value gjson.Result) *int64 {
	if !value.Exists() || value.Type != gjson.Number {
		return nil
	}
	number := value.Int()
	if number < 0 {
		return nil
	}
	return &number
}

func (u *CanonicalUsage) present() bool {
	return u != nil && (u.InputTokens != nil ||
		u.OutputTokens != nil ||
		u.CachedTokens != nil ||
		u.CacheReadTokens != nil ||
		u.CacheCreationTokens != nil ||
		u.ReasoningTokens != nil ||
		u.TotalTokens != nil)
}

func (u *CanonicalUsage) merge(next *CanonicalUsage) {
	if u == nil || next == nil {
		return
	}
	if next.InputTokens != nil {
		u.InputTokens = cloneToken(next.InputTokens)
	}
	if next.OutputTokens != nil {
		u.OutputTokens = cloneToken(next.OutputTokens)
	}
	if next.CachedTokens != nil {
		u.CachedTokens = cloneToken(next.CachedTokens)
	}
	if next.CacheReadTokens != nil {
		u.CacheReadTokens = cloneToken(next.CacheReadTokens)
	}
	if next.CacheCreationTokens != nil {
		u.CacheCreationTokens = cloneToken(next.CacheCreationTokens)
	}
	if next.ReasoningTokens != nil {
		u.ReasoningTokens = cloneToken(next.ReasoningTokens)
	}
	if next.TotalTokens != nil {
		u.TotalTokens = cloneToken(next.TotalTokens)
	}
	u.Estimated = u.Estimated || next.Estimated
}

func cloneToken(source *int64) *int64 {
	if source == nil {
		return nil
	}
	value := *source
	return &value
}

type streamUsageAccumulator struct {
	protocol      config.RouterProtocol
	pending       []byte
	usage         CanonicalUsage
	found         bool
	explicitTotal bool
}

func newStreamUsageAccumulator(protocol config.RouterProtocol) *streamUsageAccumulator {
	return &streamUsageAccumulator{protocol: protocol}
}

func (a *streamUsageAccumulator) Observe(chunk []byte) {
	if a == nil || len(chunk) == 0 {
		return
	}
	a.pending = append(a.pending, chunk...)
	for {
		index := bytes.IndexByte(a.pending, '\n')
		if index < 0 {
			break
		}
		a.observeLine(a.pending[:index])
		a.pending = a.pending[index+1:]
	}
	a.observeCompleteTail()
}

func (a *streamUsageAccumulator) Result() (*CanonicalUsage, bool) {
	if a == nil {
		return nil, false
	}
	if len(bytes.TrimSpace(a.pending)) > 0 {
		a.observeLine(a.pending)
		a.pending = nil
	}
	if !a.found {
		return nil, false
	}
	if !a.explicitTotal && a.usage.InputTokens != nil && a.usage.OutputTokens != nil {
		total := *a.usage.InputTokens + *a.usage.OutputTokens
		a.usage.TotalTokens = &total
	}
	result := a.usage
	return &result, true
}

func (a *streamUsageAccumulator) observeLine(line []byte) {
	line = bytes.TrimSpace(line)
	if bytes.HasPrefix(line, []byte("data:")) {
		line = bytes.TrimSpace(line[len("data:"):])
	}
	a.observePayload(line)
}

func (a *streamUsageAccumulator) observeCompleteTail() {
	tail := bytes.TrimSpace(a.pending)
	if bytes.HasPrefix(tail, []byte("data:")) {
		tail = bytes.TrimSpace(tail[len("data:"):])
	}
	if !gjson.ValidBytes(tail) {
		return
	}
	a.observePayload(tail)
	a.pending = nil
}

func (a *streamUsageAccumulator) observePayload(payload []byte) {
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return
	}
	usage, ok := canonicalUsageFromJSON(a.protocol, payload)
	if !ok {
		return
	}
	a.usage.merge(usage)
	a.explicitTotal = a.explicitTotal || canonicalUsageHasExplicitTotal(a.protocol, payload)
	a.found = true
}

func canonicalUsageHasExplicitTotal(protocol config.RouterProtocol, payload []byte) bool {
	if !gjson.ValidBytes(payload) {
		return false
	}
	root := gjson.ParseBytes(payload)
	switch protocol {
	case config.RouterProtocolOpenAIChatCompletions:
		return root.Get("usage.total_tokens").Exists()
	case config.RouterProtocolOpenAIResponses:
		return root.Get("usage.total_tokens").Exists() || root.Get("response.usage.total_tokens").Exists()
	default:
		return false
	}
}
