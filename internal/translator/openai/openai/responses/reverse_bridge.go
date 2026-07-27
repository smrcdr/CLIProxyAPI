package responses

import (
	"context"

	interactionschat "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/interactions/chat-completions"
	interactionsresponses "github.com/router-for-me/CLIProxyAPI/v7/internal/translator/openai/interactions/responses"
)

// ConvertOpenAIChatCompletionsRequestToOpenAIResponses composes the existing
// Chat -> Interactions and Interactions -> Responses translators.
func ConvertOpenAIChatCompletionsRequestToOpenAIResponses(modelName string, rawJSON []byte, stream bool) []byte {
	interactionsRequest := interactionschat.ConvertOpenAIRequestToInteractions(modelName, rawJSON, stream)
	return interactionsresponses.ConvertInteractionsRequestToOpenAIResponses(modelName, interactionsRequest, stream)
}

type responsesToChatStreamState struct {
	responsesToInteractions any
	interactionsToChat      any
}

func responsesToChatState(param *any) *responsesToChatStreamState {
	if param == nil {
		return &responsesToChatStreamState{}
	}
	if state, ok := (*param).(*responsesToChatStreamState); ok && state != nil {
		return state
	}
	state := &responsesToChatStreamState{}
	*param = state
	return state
}

// ConvertOpenAIResponsesResponseToOpenAIChatCompletions composes the existing
// Responses -> Interactions and Interactions -> Chat stream translators while
// keeping independent incremental state for both stages.
func ConvertOpenAIResponsesResponseToOpenAIChatCompletions(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON,
	requestRawJSON,
	rawJSON []byte,
	param *any,
) [][]byte {
	state := responsesToChatState(param)
	interactionsRequest := interactionschat.ConvertOpenAIRequestToInteractions(modelName, originalRequestRawJSON, true)
	interactionChunks := interactionsresponses.ConvertOpenAIResponsesResponseToInteractions(
		ctx,
		modelName,
		interactionsRequest,
		requestRawJSON,
		rawJSON,
		&state.responsesToInteractions,
	)
	outputs := make([][]byte, 0, len(interactionChunks))
	for _, chunk := range interactionChunks {
		outputs = append(outputs, interactionschat.ConvertInteractionsResponseToOpenAI(
			ctx,
			modelName,
			originalRequestRawJSON,
			interactionsRequest,
			chunk,
			&state.interactionsToChat,
		)...)
	}
	return outputs
}

// ConvertOpenAIResponsesResponseToOpenAIChatCompletionsNonStream composes the
// corresponding existing non-stream response translators.
func ConvertOpenAIResponsesResponseToOpenAIChatCompletionsNonStream(
	ctx context.Context,
	modelName string,
	originalRequestRawJSON,
	requestRawJSON,
	rawJSON []byte,
	_ *any,
) []byte {
	interactionsRequest := interactionschat.ConvertOpenAIRequestToInteractions(modelName, originalRequestRawJSON, false)
	interactionsResponse := interactionsresponses.ConvertOpenAIResponsesResponseToInteractionsNonStream(
		ctx,
		modelName,
		interactionsRequest,
		requestRawJSON,
		rawJSON,
		nil,
	)
	return interactionschat.ConvertInteractionsResponseToOpenAINonStream(
		ctx,
		modelName,
		originalRequestRawJSON,
		interactionsRequest,
		interactionsResponse,
		nil,
	)
}
