package smartrouter

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v7/internal/config"
	"github.com/tidwall/gjson"
)

var (
	ErrStreamModeMismatch         = errors.New("router stream mode does not match the request")
	ErrStreamSinkNotConfigured    = errors.New("router stream sink is not configured")
	ErrStreamProtocolNotSupported = errors.New("router stream protocol is not supported")
)

// maxUncommittedBytes bounds the prelude buffered before the downstream is
// committed. Complete non-semantic events are pruned continuously, so only a
// single pathological event can approach this limit; exceeding it is treated
// as a protocol failure rather than unbounded growth.
const maxUncommittedBytes = 1 << 20

// StreamState is the downstream commitment state of one streaming attempt.
type StreamState string

const (
	StreamStateNotStarted      StreamState = "not_started"
	StreamStateHeadersReceived StreamState = "headers_received"
	StreamStateUncommitted     StreamState = "uncommitted"
	StreamStateCommitted       StreamState = "committed"
	StreamStateCompleted       StreamState = "completed"
	StreamStateFailedPartial   StreamState = "failed_partial"
)

// StreamAttemptExecutor opens exactly one upstream stream for one selected
// route. Implementations own protocol translation, credentials, and transport.
// A nil error means response headers were received, regardless of status code.
type StreamAttemptExecutor interface {
	ExecuteStreamAttempt(context.Context, AttemptRequest) (*StreamAttempt, error)
}

// StreamAttempt exposes the initial upstream status and headers separately
// from the chunk channel so the coordinator can fail over before it commits
// anything downstream.
type StreamAttempt struct {
	StatusCode int
	Headers    http.Header
	// Chunks is closed by the executor when the upstream stream ends. It may
	// be nil for a non-2xx attempt.
	Chunks <-chan StreamChunk
	// Close releases upstream resources. It must be safe to call once, must
	// not block, and must unblock any pending send on Chunks. The coordinator
	// also cancels the attempt context, so an executor that only honors
	// context cancellation may leave Close nil.
	Close func()
}

// StreamChunk carries one downstream-protocol chunk or the terminal error of
// the upstream stream.
type StreamChunk struct {
	Data []byte
	Err  error
}

// StreamCommit describes the attempt that committed the downstream response.
type StreamCommit struct {
	StatusCode       int
	Headers          http.Header
	RouteID          string
	UpstreamID       string
	SnapshotRevision uint64
}

// StreamSink receives the committed downstream stream. Commit is called at
// most once per request, always before the first Send. Fail is called only
// after a successful Commit, when the upstream failed mid-stream. A request
// that never commits reports its failure through the ExecuteStream error and
// never touches the sink.
type StreamSink interface {
	Commit(StreamCommit) error
	// Send forwards one downstream payload. The slice is owned by the caller
	// and is only valid for the duration of the call.
	Send([]byte) error
	Fail(*ExecutionError) error
}

type streamEventKind int

// Ordering matters: a prelude is classified by its strongest complete line,
// and a malformed line outranks a semantic one so that a partially invalid
// event still fails over while the downstream is uncommitted.
const (
	streamEventIgnorable streamEventKind = iota
	streamEventSemantic
	streamEventMalformed
)

// streamClassifier decides whether an SSE line commits the downstream
// response. It reads the downstream (entry) protocol, because commitment is
// defined by what the client has already observed.
type streamClassifier struct {
	protocol config.RouterProtocol
}

func streamClassifierFor(protocol config.RouterProtocol) (streamClassifier, error) {
	switch protocol {
	case config.RouterProtocolOpenAIChatCompletions,
		config.RouterProtocolOpenAIResponses,
		config.RouterProtocolAnthropicMessages:
		return streamClassifier{protocol: protocol}, nil
	}
	return streamClassifier{}, fmt.Errorf("%w: %q", ErrStreamProtocolNotSupported, protocol)
}

func (s streamClassifier) classifyLine(line []byte) streamEventKind {
	line = bytes.TrimSpace(line)
	if len(line) == 0 {
		return streamEventIgnorable
	}
	switch {
	case line[0] == ':':
		// SSE comment, including the common keepalive form.
		return streamEventIgnorable
	case bytes.HasPrefix(line, []byte("event:")),
		bytes.HasPrefix(line, []byte("id:")),
		bytes.HasPrefix(line, []byte("retry:")):
		return streamEventIgnorable
	case bytes.HasPrefix(line, []byte("data:")):
		return s.classifyPayload(bytes.TrimSpace(line[len("data:"):]))
	case line[0] == '{' || line[0] == '[':
		// Some executors emit bare JSON payloads without SSE framing.
		return s.classifyPayload(line)
	default:
		return streamEventMalformed
	}
}

func (s streamClassifier) classifyPayload(payload []byte) streamEventKind {
	if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
		return streamEventIgnorable
	}
	if !gjson.ValidBytes(payload) {
		return streamEventMalformed
	}
	root := gjson.ParseBytes(payload)
	if root.Get("error").Exists() {
		return streamEventMalformed
	}
	switch s.protocol {
	case config.RouterProtocolOpenAIChatCompletions:
		// A usage-only chunk still carries "choices", so any well formed
		// chunk object commits the downstream response.
		if root.Get("choices").Exists() || strings.TrimSpace(root.Get("object").String()) != "" {
			return streamEventSemantic
		}
		return streamEventMalformed
	case config.RouterProtocolOpenAIResponses:
		if strings.TrimSpace(root.Get("type").String()) == "" {
			return streamEventMalformed
		}
		return streamEventSemantic
	case config.RouterProtocolAnthropicMessages:
		switch strings.TrimSpace(root.Get("type").String()) {
		case "":
			return streamEventMalformed
		case "ping":
			return streamEventIgnorable
		default:
			return streamEventSemantic
		}
	}
	return streamEventMalformed
}

// classifyCompleteTail recognizes payloads that are complete without a final
// newline. Invalid JSON may still be an incomplete chunk, so it remains
// pending until more bytes arrive or the stream closes.
func (s streamClassifier) classifyCompleteTail(tail []byte) (streamEventKind, bool) {
	tail = bytes.TrimSpace(tail)
	if len(tail) == 0 {
		return streamEventIgnorable, false
	}

	payload := tail
	if bytes.HasPrefix(tail, []byte("data:")) {
		payload = bytes.TrimSpace(tail[len("data:"):])
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			return streamEventIgnorable, true
		}
	} else if tail[0] != '{' && tail[0] != '[' {
		return streamEventIgnorable, false
	}

	if !gjson.ValidBytes(payload) {
		return streamEventIgnorable, false
	}
	return s.classifyPayload(payload), true
}

// streamPrelude accumulates the bytes emitted before the downstream response
// is committed. It recognizes complete newline-delimited lines and complete
// JSON at a chunk boundary, while an incomplete JSON tail remains pending. It
// prunes complete non-semantic events so keepalives cannot grow the buffer.
type streamPrelude struct {
	buf []byte
	// scanned is the offset past the last complete line.
	scanned int
	// boundary is the offset past the last completed SSE event, which is the
	// only point at which the prelude may be safely truncated.
	boundary int
}

// append adds one chunk and returns the strongest kind observed among newly
// completed lines or a demonstrably complete non-newline-terminated tail.
func (p *streamPrelude) append(classifier streamClassifier, chunk []byte) streamEventKind {
	p.buf = append(p.buf, chunk...)
	kind := streamEventIgnorable
	for {
		index := bytes.IndexByte(p.buf[p.scanned:], '\n')
		if index < 0 {
			break
		}
		line := p.buf[p.scanned : p.scanned+index]
		p.scanned += index + 1
		if len(bytes.TrimSpace(line)) == 0 {
			// A blank line terminates the current SSE event.
			p.boundary = p.scanned
			continue
		}
		if lineKind := classifier.classifyLine(line); lineKind > kind {
			kind = lineKind
		}
	}
	if tailKind, complete := classifier.classifyCompleteTail(p.buf[p.scanned:]); complete && tailKind > kind {
		kind = tailKind
	}
	return kind
}

// finalize classifies the last unterminated line when the upstream closes.
// At that point an invalid JSON tail is malformed rather than merely pending.
func (p *streamPrelude) finalize(classifier streamClassifier) streamEventKind {
	tail := p.buf[p.scanned:]
	if len(bytes.TrimSpace(tail)) == 0 {
		return streamEventIgnorable
	}
	return classifier.classifyLine(tail)
}

// pruneCompletedEvents drops every complete event already buffered. It must
// only be called when no semantic line has been seen in the prelude, so the
// dropped bytes can never be owed to the downstream client.
func (p *streamPrelude) pruneCompletedEvents() {
	if p.boundary <= 0 {
		return
	}
	remaining := copy(p.buf, p.buf[p.boundary:])
	p.buf = p.buf[:remaining]
	p.scanned -= p.boundary
	p.boundary = 0
}

func (p *streamPrelude) size() int {
	return len(p.buf)
}

func (p *streamPrelude) bytes() []byte {
	return p.buf
}

// streamChunkFailure derives a safe failure category from a terminal chunk
// error without retaining the error message.
func streamChunkFailure(err error) (FailureCategory, int) {
	var carrier interface{ StatusCode() int }
	if errors.As(err, &carrier) {
		status := carrier.StatusCode()
		if status <= 0 {
			return FailureTransient, 0
		}
		if category := ClassifyHTTPFailure(status); category != FailureNone {
			return category, status
		}
		if status >= 200 && status <= 299 {
			return FailureProtocol, status
		}
		return FailureClient, status
	}
	return FailureTransient, 0
}
