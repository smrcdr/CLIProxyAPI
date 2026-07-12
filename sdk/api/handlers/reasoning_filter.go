package handlers

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/interfaces"
	"golang.org/x/net/context"
)

// stripReasoningResponse applies the last-mile reasoning policy after all translators and plugins.
func stripReasoningResponse(protocol string, body []byte) ([]byte, error) {
	if len(bytes.TrimSpace(body)) == 0 {
		return nil, fmt.Errorf("reasoning filter: empty response")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.UseNumber()
	var value any
	if err := decoder.Decode(&value); err != nil {
		return nil, fmt.Errorf("reasoning filter: invalid JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, fmt.Errorf("reasoning filter: invalid JSON: %w", err)
	}
	cleaned := sanitizeReasoningValue(protocol, value)
	out, err := json.Marshal(cleaned)
	if err != nil {
		return nil, fmt.Errorf("reasoning filter: encode response: %w", err)
	}
	return out, nil
}

func ensureJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err == nil {
		return fmt.Errorf("multiple JSON values")
	} else if err != io.EOF {
		return err
	}
	return nil
}

func sanitizeReasoningValue(protocol string, value any) any {
	switch node := value.(type) {
	case map[string]any:
		for _, key := range []string{"reasoning", "reasoning_content", "reasoning_details"} {
			delete(node, key)
		}
		for key, child := range node {
			if text, ok := child.(string); ok && isVisibleTextField(protocol, key, node) {
				node[key] = stripThinkText(text)
				continue
			}
			node[key] = sanitizeReasoningValue(protocol, child)
		}
		return node
	case []any:
		out := make([]any, 0, len(node))
		for _, child := range node {
			if object, ok := child.(map[string]any); ok && isReasoningObject(protocol, object) {
				continue
			}
			out = append(out, sanitizeReasoningValue(protocol, child))
		}
		return out
	default:
		return value
	}
}

func removeStructuredReasoning(protocol string, value any) any {
	switch node := value.(type) {
	case map[string]any:
		for _, key := range []string{"reasoning", "reasoning_content", "reasoning_details"} {
			delete(node, key)
		}
		for key, child := range node {
			node[key] = removeStructuredReasoning(protocol, child)
		}
		return node
	case []any:
		out := make([]any, 0, len(node))
		for _, child := range node {
			if object, ok := child.(map[string]any); ok && isReasoningObject(protocol, object) {
				continue
			}
			out = append(out, removeStructuredReasoning(protocol, child))
		}
		return out
	default:
		return value
	}
}

func isVisibleTextField(protocol, key string, parent map[string]any) bool {
	if key == "content" {
		_, isString := parent[key].(string)
		return isString
	}
	if key == "text" {
		typeName, _ := parent["type"].(string)
		return typeName == "text" || typeName == "output_text" || typeName == "input_text" || typeName == "text_delta"
	}
	return key == "delta" && protocol == "openai-response" && strings.Contains(stringValue(parent["type"]), "output_text")
}

func isReasoningObject(protocol string, object map[string]any) bool {
	typeName := strings.ToLower(stringValue(object["type"]))
	switch protocol {
	case "claude":
		return typeName == "thinking" || typeName == "redacted_thinking"
	case "openai-response":
		return typeName == "reasoning" || strings.Contains(typeName, "reasoning_")
	default:
		return false
	}
}

func stringValue(value any) string {
	text, _ := value.(string)
	return text
}

func stripThinkText(text string) string {
	stripper := newThinkStripper()
	out := stripper.Write(text)
	out += stripper.Flush()
	return out
}

type thinkStripper struct {
	buffer string
	inside bool
}

func newThinkStripper() *thinkStripper { return &thinkStripper{} }

func (s *thinkStripper) Write(text string) string {
	s.buffer += text
	var out strings.Builder
	for len(s.buffer) > 0 {
		if s.buffer[0] != '<' {
			if !s.inside {
				out.WriteByte(s.buffer[0])
			}
			s.buffer = s.buffer[1:]
			continue
		}

		matched, opening, partial := matchThinkTag(s.buffer)
		if partial {
			break
		}
		if matched > 0 {
			s.inside = opening
			s.buffer = s.buffer[matched:]
			continue
		}
		if !s.inside {
			out.WriteByte('<')
		}
		s.buffer = s.buffer[1:]
	}
	return out.String()
}

func (s *thinkStripper) Flush() string {
	if s.inside {
		s.buffer = ""
		return ""
	}
	out := s.buffer
	s.buffer = ""
	return out
}

func matchThinkTag(value string) (length int, opening bool, partial bool) {
	lower := strings.ToLower(value)
	tags := []struct {
		text    string
		opening bool
	}{
		{"<think>", true}, {"<thinking>", true}, {"</think>", false}, {"</thinking>", false},
	}
	for _, tag := range tags {
		if strings.HasPrefix(lower, tag.text) {
			return len(tag.text), tag.opening, false
		}
		if strings.HasPrefix(tag.text, lower) {
			return 0, false, true
		}
	}
	return 0, false, false
}

// reasoningStreamFilter frames arbitrary SSE byte chunks and sanitizes complete events.
type reasoningStreamFilter struct {
	protocol        string
	buffer          []byte
	sequence        int64
	messageIndices  map[int]int
	droppedMessages map[int]bool
	textStrippers   map[string]*thinkStripper
}

func newReasoningStreamFilter(protocol string) *reasoningStreamFilter {
	return &reasoningStreamFilter{
		protocol:        protocol,
		messageIndices:  make(map[int]int),
		droppedMessages: make(map[int]bool),
		textStrippers:   make(map[string]*thinkStripper),
	}
}

func (f *reasoningStreamFilter) Write(chunk []byte) ([][]byte, error) {
	trimmed := bytes.TrimSpace(chunk)
	if f.protocol != "openai-response" && len(f.buffer) == 0 && len(trimmed) > 0 && trimmed[0] == '{' {
		cleaned, keep, err := f.filterJSONChunk(trimmed)
		if err != nil || !keep {
			return nil, err
		}
		return [][]byte{cleaned}, nil
	}
	if reasoningSSENeedsLineBreak(f.buffer, chunk) {
		f.buffer = append(f.buffer, '\n')
	}
	f.buffer = append(f.buffer, chunk...)
	var output [][]byte
	for {
		end, separator := findSSEFrameEnd(f.buffer)
		if end < 0 {
			break
		}
		frame := bytes.Clone(f.buffer[:end])
		f.buffer = f.buffer[end+separator:]
		cleaned, keep, err := f.filterFrame(frame)
		if err != nil {
			return nil, err
		}
		if keep {
			output = append(output, append(cleaned, '\n', '\n'))
		}
	}
	if len(bytes.TrimSpace(f.buffer)) > 0 && reasoningSSECanEmitWithoutDelimiter(f.buffer) {
		cleaned, keep, err := f.filterFrame(bytes.Clone(f.buffer))
		if err != nil {
			return nil, err
		}
		f.buffer = f.buffer[:0]
		if keep {
			output = append(output, append(cleaned, '\n', '\n'))
		}
	}
	return output, nil
}

func (f *reasoningStreamFilter) filterJSONChunk(raw []byte) ([]byte, bool, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var event map[string]any
	if err := decoder.Decode(&event); err != nil {
		return nil, false, fmt.Errorf("reasoning filter: invalid stream JSON: %w", err)
	}
	if err := ensureJSONEOF(decoder); err != nil {
		return nil, false, fmt.Errorf("reasoning filter: invalid stream JSON: %w", err)
	}
	keep := true
	if f.protocol == "claude" {
		keep = f.filterMessagesEvent(event)
	} else {
		f.filterChatEvent(event)
	}
	if !keep {
		return nil, false, nil
	}
	cleaned, err := json.Marshal(event)
	if err != nil {
		return nil, false, fmt.Errorf("reasoning filter: encode stream JSON: %w", err)
	}
	return cleaned, true, nil
}

func (f *reasoningStreamFilter) Flush() ([][]byte, error) {
	if len(bytes.TrimSpace(f.buffer)) != 0 {
		return nil, fmt.Errorf("reasoning filter: incomplete SSE frame")
	}
	f.buffer = nil
	return nil, nil
}

func findSSEFrameEnd(data []byte) (int, int) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	if lf < 0 {
		if crlf < 0 {
			return -1, 0
		}
		return crlf, 4
	}
	if crlf >= 0 && crlf < lf {
		return crlf, 4
	}
	return lf, 2
}

func reasoningSSECanEmitWithoutDelimiter(chunk []byte) bool {
	trimmed := bytes.TrimSpace(chunk)
	if len(trimmed) == 0 {
		return false
	}
	hasEvent := false
	hasData := false
	for _, line := range bytes.Split(trimmed, []byte("\n")) {
		line = bytes.TrimSpace(bytes.TrimRight(line, "\r"))
		switch {
		case bytes.HasPrefix(line, []byte("event:")):
			hasEvent = true
		case bytes.HasPrefix(line, []byte("data:")):
			hasData = true
			data := bytes.TrimSpace(line[len("data:"):])
			if len(data) > 0 && !bytes.Equal(data, []byte("[DONE]")) && !json.Valid(data) {
				return false
			}
		}
	}
	return hasData && (!hasEvent || hasData)
}

func reasoningSSENeedsLineBreak(pending, chunk []byte) bool {
	if len(pending) == 0 || len(chunk) == 0 || bytes.HasSuffix(pending, []byte("\n")) || bytes.HasSuffix(pending, []byte("\r")) || chunk[0] == '\n' || chunk[0] == '\r' {
		return false
	}
	trimmed := bytes.TrimLeft(chunk, " \t")
	for _, prefix := range [][]byte{[]byte("data:"), []byte("event:"), []byte("id:"), []byte("retry:"), []byte(":")} {
		if bytes.HasPrefix(trimmed, prefix) {
			return true
		}
	}
	return false
}

func (f *reasoningStreamFilter) filterFrame(frame []byte) ([]byte, bool, error) {
	lines := bytes.Split(bytes.ReplaceAll(frame, []byte("\r\n"), []byte("\n")), []byte("\n"))
	dataIndex := -1
	for i, line := range lines {
		if bytes.HasPrefix(line, []byte("data:")) {
			dataIndex = i
			break
		}
	}
	if dataIndex < 0 {
		return frame, true, nil
	}
	raw := bytes.TrimSpace(lines[dataIndex][5:])
	if bytes.Equal(raw, []byte("[DONE]")) {
		return bytes.Join(lines, []byte("\n")), true, nil
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	var event map[string]any
	if err := decoder.Decode(&event); err != nil {
		return nil, false, fmt.Errorf("reasoning filter: invalid SSE JSON: %w", err)
	}
	keep := true
	switch f.protocol {
	case "openai-response":
		keep = f.filterResponsesEvent(event)
	case "claude":
		keep = f.filterMessagesEvent(event)
	default:
		f.filterChatEvent(event)
	}
	if !keep {
		return nil, false, nil
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		return nil, false, fmt.Errorf("reasoning filter: encode SSE JSON: %w", err)
	}
	lines[dataIndex] = append([]byte("data: "), encoded...)
	return bytes.Join(lines, []byte("\n")), true, nil
}

func (f *reasoningStreamFilter) filterChatEvent(event map[string]any) {
	removeStructuredReasoning(f.protocol, event)
	choices, _ := event["choices"].([]any)
	for _, rawChoice := range choices {
		choice, _ := rawChoice.(map[string]any)
		if message, ok := choice["message"].(map[string]any); ok {
			choice["message"] = sanitizeReasoningValue(f.protocol, message)
		}
		delta, _ := choice["delta"].(map[string]any)
		content, ok := delta["content"].(string)
		if !ok {
			continue
		}
		key := "choice:" + numberKey(choice["index"])
		delta["content"] = f.stripper(key).Write(content)
	}
}

func (f *reasoningStreamFilter) filterResponsesEvent(event map[string]any) bool {
	typeName := strings.ToLower(stringValue(event["type"]))
	if strings.Contains(typeName, "reasoning") {
		return false
	}
	if item, ok := event["item"].(map[string]any); ok && isReasoningObject("openai-response", item) {
		return false
	}
	if part, ok := event["part"].(map[string]any); ok && isReasoningObject("openai-response", part) {
		return false
	}
	deltaText, hasDeltaText := event["delta"].(string)
	removeStructuredReasoning("openai-response", event)
	if hasDeltaText && strings.Contains(typeName, "output_text") {
		key := "response:" + numberKey(event["output_index"]) + ":" + numberKey(event["content_index"])
		event["delta"] = f.stripper(key).Write(deltaText)
	}
	if text, ok := event["text"].(string); ok && strings.Contains(typeName, "output_text") {
		event["text"] = stripThinkText(text)
	}
	if typeName == "response.completed" || typeName == "response.done" {
		sanitizeReasoningValue("openai-response", event)
	}
	event["sequence_number"] = f.sequence
	f.sequence++
	return true
}

func (f *reasoningStreamFilter) filterMessagesEvent(event map[string]any) bool {
	typeName := stringValue(event["type"])
	upstreamIndex, hasIndex := intValue(event["index"])
	switch typeName {
	case "content_block_start":
		block, _ := event["content_block"].(map[string]any)
		if isReasoningObject("claude", block) {
			if hasIndex {
				f.droppedMessages[upstreamIndex] = true
			}
			return false
		}
		if hasIndex {
			f.messageIndices[upstreamIndex] = len(f.messageIndices)
			event["index"] = f.messageIndices[upstreamIndex]
			if text, ok := block["text"].(string); ok {
				block["text"] = f.stripper("message:" + strconv.Itoa(f.messageIndices[upstreamIndex])).Write(text)
			}
		}
	case "content_block_delta":
		if hasIndex && f.droppedMessages[upstreamIndex] {
			return false
		}
		delta, _ := event["delta"].(map[string]any)
		deltaType := stringValue(delta["type"])
		if deltaType == "thinking_delta" || deltaType == "signature_delta" {
			return false
		}
		if hasIndex {
			mapped, ok := f.messageIndices[upstreamIndex]
			if !ok {
				return false
			}
			event["index"] = mapped
			if text, okText := delta["text"].(string); okText {
				delta["text"] = f.stripper("message:" + strconv.Itoa(mapped)).Write(text)
			}
		}
	case "content_block_stop":
		if hasIndex && f.droppedMessages[upstreamIndex] {
			return false
		}
		if hasIndex {
			mapped, ok := f.messageIndices[upstreamIndex]
			if !ok {
				return false
			}
			event["index"] = mapped
		}
	case "message_start":
		if message, ok := event["message"].(map[string]any); ok {
			event["message"] = sanitizeReasoningValue("claude", message)
		}
	}
	removeStructuredReasoning("claude", event)
	return true
}

func (f *reasoningStreamFilter) stripper(key string) *thinkStripper {
	stripper := f.textStrippers[key]
	if stripper == nil {
		stripper = newThinkStripper()
		f.textStrippers[key] = stripper
	}
	return stripper
}

func numberKey(value any) string {
	switch number := value.(type) {
	case json.Number:
		return number.String()
	case float64:
		return strconv.FormatFloat(number, 'f', -1, 64)
	default:
		return fmt.Sprint(value)
	}
}

func intValue(value any) (int, bool) {
	switch number := value.(type) {
	case json.Number:
		parsed, err := strconv.Atoi(number.String())
		return parsed, err == nil
	case float64:
		return int(number), true
	case int:
		return number, true
	default:
		return 0, false
	}
}

func (h *BaseAPIHandler) finalizeResponse(protocol string, body []byte, headers http.Header) ([]byte, http.Header, *interfaces.ErrorMessage) {
	if h == nil || h.Cfg == nil || !h.Cfg.StripReasoning {
		return body, headers, nil
	}
	cleaned, err := stripReasoningResponse(protocol, body)
	if err != nil {
		return nil, headers, &interfaces.ErrorMessage{StatusCode: 502, Error: err}
	}
	return cleaned, headers, nil
}

func (h *BaseAPIHandler) finalizeStream(ctx context.Context, protocol string, input <-chan []byte, inputErrors <-chan *interfaces.ErrorMessage) (<-chan []byte, <-chan *interfaces.ErrorMessage) {
	if h == nil || h.Cfg == nil || !h.Cfg.StripReasoning {
		return input, inputErrors
	}
	output := make(chan []byte)
	errorsOut := make(chan *interfaces.ErrorMessage, 1)
	filter := newReasoningStreamFilter(protocol)
	go func() {
		defer close(output)
		defer close(errorsOut)
		data := input
		errs := inputErrors
		for data != nil || errs != nil {
			select {
			case <-ctxDone(ctx):
				return
			case chunk, ok := <-data:
				if !ok {
					data = nil
					continue
				}
				frames, err := filter.Write(chunk)
				if err != nil {
					sendFilterError(ctx, errorsOut, err)
					return
				}
				for _, frame := range frames {
					if !sendFilterData(ctx, output, frame) {
						return
					}
				}
			case errMessage, ok := <-errs:
				if !ok {
					errs = nil
					continue
				}
				if errMessage != nil {
					select {
					case errorsOut <- errMessage:
					case <-ctxDone(ctx):
					}
					return
				}
			}
		}
		if _, err := filter.Flush(); err != nil {
			sendFilterError(ctx, errorsOut, err)
		}
	}()
	return output, errorsOut
}

func ctxDone(ctx context.Context) <-chan struct{} {
	if ctx == nil {
		return nil
	}
	return ctx.Done()
}

func sendFilterData(ctx context.Context, output chan<- []byte, data []byte) bool {
	select {
	case output <- data:
		return true
	case <-ctxDone(ctx):
		return false
	}
}

func sendFilterError(ctx context.Context, output chan<- *interfaces.ErrorMessage, err error) {
	message := &interfaces.ErrorMessage{StatusCode: 502, Error: err}
	select {
	case output <- message:
	case <-ctxDone(ctx):
	}
}
