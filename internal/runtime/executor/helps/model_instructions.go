package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

// ApplyModelInstruction adds a configured system instruction to an Anthropic payload.
// The client-visible model is preferred so aliases remain independently configurable.
func ApplyModelInstruction(cfg *config.Config, requestedModel, upstreamModel string, payload []byte) []byte {
	if cfg == nil || len(cfg.ModelInstructions) == 0 {
		return payload
	}
	instruction, ok := cfg.ModelInstructions[strings.TrimSpace(requestedModel)]
	if !ok {
		instruction, ok = cfg.ModelInstructions[strings.TrimSpace(upstreamModel)]
	}
	if !ok || !instruction.Enabled || strings.TrimSpace(instruction.Prompt) == "" {
		return payload
	}

	block := []byte(`{"type":"text","text":""}`)
	block, _ = sjson.SetBytes(block, "text", instruction.Prompt)
	mode := strings.ToLower(strings.TrimSpace(instruction.Mode))
	if mode == "" {
		mode = "prepend"
	}
	if mode != "prepend" && mode != "append" && mode != "override" {
		return payload
	}

	system := gjson.GetBytes(payload, "system")
	parts := make([][]byte, 0, 4)
	if mode != "override" && system.Exists() {
		if system.IsArray() {
			system.ForEach(func(_, item gjson.Result) bool {
				parts = append(parts, []byte(item.Raw))
				return true
			})
		} else if system.Type == gjson.String && system.String() != "" {
			existing := []byte(`{"type":"text","text":""}`)
			existing, _ = sjson.SetBytes(existing, "text", system.String())
			parts = append(parts, existing)
		}
	}
	if mode == "prepend" || mode == "override" {
		parts = append([][]byte{block}, parts...)
	} else {
		parts = append(parts, block)
	}

	result := []byte("[")
	for i, part := range parts {
		if i > 0 {
			result = append(result, ',')
		}
		result = append(result, part...)
	}
	result = append(result, ']')
	updated, err := sjson.SetRawBytes(payload, "system", result)
	if err != nil {
		return payload
	}
	return updated
}
