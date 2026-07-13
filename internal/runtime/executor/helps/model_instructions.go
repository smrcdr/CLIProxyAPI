package helps

import (
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func configuredModelInstruction(cfg *config.Config, requestedModel, upstreamModel string) (config.ModelInstruction, bool) {
	if cfg == nil || len(cfg.ModelInstructions) == 0 {
		return config.ModelInstruction{}, false
	}
	instruction, ok := cfg.ModelInstructions[strings.TrimSpace(requestedModel)]
	if !ok {
		instruction, ok = cfg.ModelInstructions[strings.TrimSpace(upstreamModel)]
	}
	if !ok || !instruction.Enabled || strings.TrimSpace(instruction.Prompt) == "" {
		return config.ModelInstruction{}, false
	}
	mode := strings.ToLower(strings.TrimSpace(instruction.Mode))
	if mode == "" {
		mode = "prepend"
	}
	if mode != "prepend" && mode != "append" && mode != "override" {
		return config.ModelInstruction{}, false
	}
	instruction.Mode = mode
	return instruction, true
}

// ApplyModelInstruction adds a configured system instruction to an Anthropic payload.
// The client-visible model is preferred so aliases remain independently configurable.
func ApplyModelInstruction(cfg *config.Config, requestedModel, upstreamModel string, payload []byte) []byte {
	instruction, ok := configuredModelInstruction(cfg, requestedModel, upstreamModel)
	if !ok {
		return payload
	}

	block := []byte(`{"type":"text","text":""}`)
	block, _ = sjson.SetBytes(block, "text", instruction.Prompt)

	system := gjson.GetBytes(payload, "system")
	parts := make([][]byte, 0, 4)
	if instruction.Mode != "override" && system.Exists() {
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
	if instruction.Mode == "prepend" || instruction.Mode == "override" {
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

// ApplyOpenAIModelInstruction adds a configured instruction to Chat Completions messages.
// This keeps alias-specific instructions active when an Anthropic or Responses request is
// translated to an OpenAI-compatible upstream.
func ApplyOpenAIModelInstruction(cfg *config.Config, requestedModel, upstreamModel string, payload []byte) []byte {
	instruction, ok := configuredModelInstruction(cfg, requestedModel, upstreamModel)
	if !ok {
		return payload
	}

	systemMessage := []byte(`{"role":"system","content":""}`)
	systemMessage, _ = sjson.SetBytes(systemMessage, "content", instruction.Prompt)
	messages := make([][]byte, 0, 8)
	gjson.GetBytes(payload, "messages").ForEach(func(_, message gjson.Result) bool {
		messages = append(messages, []byte(message.Raw))
		return true
	})

	parts := make([][]byte, 0, len(messages)+1)
	switch instruction.Mode {
	case "override":
		parts = append(parts, systemMessage)
		for _, message := range messages {
			if !strings.EqualFold(gjson.GetBytes(message, "role").String(), "system") {
				parts = append(parts, message)
			}
		}
	case "append":
		insertAt := 0
		for index, message := range messages {
			if strings.EqualFold(gjson.GetBytes(message, "role").String(), "system") {
				insertAt = index + 1
			}
		}
		parts = append(parts, messages[:insertAt]...)
		parts = append(parts, systemMessage)
		parts = append(parts, messages[insertAt:]...)
	default:
		parts = append(parts, systemMessage)
		parts = append(parts, messages...)
	}

	result := []byte("[")
	for index, part := range parts {
		if index > 0 {
			result = append(result, ',')
		}
		result = append(result, part...)
	}
	result = append(result, ']')
	updated, err := sjson.SetRawBytes(payload, "messages", result)
	if err != nil {
		return payload
	}
	return updated
}
