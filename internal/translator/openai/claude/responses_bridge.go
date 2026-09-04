package claude

import (
	"bytes"
	"context"

	sdktranslator "github.com/router-for-me/CLIProxyAPI/v7/sdk/translator"
)

// ConvertClaudeRequestToOpenAIResponses composes the registered Claude ->
// Chat and Chat -> Responses translators.
func ConvertClaudeRequestToOpenAIResponses(modelName string, rawJSON []byte, stream bool) []byte {
	chatRequest := sdktranslator.TranslateRequest(
		sdktranslator.FormatClaude,
		sdktranslator.FormatOpenAI,
		modelName,
		rawJSON,
		stream,
	)
	return sdktranslator.TranslateRequest(
		sdktranslator.FormatOpenAI,
		sdktranslator.FormatOpenAIResponse,
		modelName,
		chatRequest,
		stream,
	)
}

type responsesToClaudeStreamState struct {
	responsesToChat any
	chatToClaude    any
}

func responsesToClaudeState(param *any) *responsesToClaudeStreamState {
	if param == nil {
		return &responsesToClaudeStreamState{}
	}
	if state, ok := (*param).(*responsesToClaudeStreamState); ok && state != nil {
		return state
	}
	state := &responsesToClaudeStreamState{}
	*param = state
	return state
}

// ConvertOpenAIResponsesResponseToClaude composes the registered Responses ->
// Chat and Chat -> Claude streaming response translators.
func ConvertOpenAIResponsesResponseToClaude(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON,
	requestRawJSON,
	rawJSON []byte,
	param *any,
) [][]byte {
	state := responsesToClaudeState(param)
	chatRequest := sdktranslator.TranslateRequest(
		sdktranslator.FormatClaude,
		sdktranslator.FormatOpenAI,
		modelName,
		originalRequestRawJSON,
		true,
	)
	chatChunks := sdktranslator.TranslateStream(
		ctx,
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatOpenAI,
		modelName,
		chatRequest,
		requestRawJSON,
		rawJSON,
		&state.responsesToChat,
	)
	outputs := make([][]byte, 0, len(chatChunks))
	for _, chunk := range chatChunks {
		chunk = ensureOpenAIStreamFrame(chunk)
		outputs = append(outputs, sdktranslator.TranslateStream(
			ctx,
			sdktranslator.FormatOpenAI,
			sdktranslator.FormatClaude,
			modelName,
			originalRequestRawJSON,
			chatRequest,
			chunk,
			&state.chatToClaude,
		)...)
	}
	return outputs
}

func ensureOpenAIStreamFrame(chunk []byte) []byte {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 || bytes.HasPrefix(trimmed, []byte("data:")) {
		return chunk
	}
	framed := make([]byte, 0, len(trimmed)+8)
	framed = append(framed, "data: "...)
	framed = append(framed, trimmed...)
	framed = append(framed, '\n', '\n')
	return framed
}

// ConvertOpenAIResponsesResponseToClaudeNonStream composes the registered
// Responses -> Chat and Chat -> Claude non-stream response translators.
func ConvertOpenAIResponsesResponseToClaudeNonStream(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON,
	requestRawJSON,
	rawJSON []byte,
	_ *any,
) []byte {
	chatRequest := sdktranslator.TranslateRequest(
		sdktranslator.FormatClaude,
		sdktranslator.FormatOpenAI,
		modelName,
		originalRequestRawJSON,
		false,
	)
	chatResponse := sdktranslator.TranslateNonStream(
		ctx,
		sdktranslator.FormatOpenAIResponse,
		sdktranslator.FormatOpenAI,
		modelName,
		chatRequest,
		requestRawJSON,
		rawJSON,
		nil,
	)
	return sdktranslator.TranslateNonStream(
		ctx,
		sdktranslator.FormatOpenAI,
		sdktranslator.FormatClaude,
		modelName,
		originalRequestRawJSON,
		chatRequest,
		chatResponse,
		nil,
	)
}
