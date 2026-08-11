package llm

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ErrSchemaRejected is returned when the endpoint explicitly rejects the
// response_format schema, usually because it does not support the strict flag.
var ErrSchemaRejected = errors.New("endpoint rejected the strict JSON schema")

// Client talks to an OpenAI-compatible chat completions endpoint over HTTP.
type Client struct {
	url    string
	apiKey string
	model  string
	temp   float64
	topP   float64
	http   *http.Client

	// maxRetries is how many additional attempts are made after the first
	// when the endpoint returns a transient error (429/5xx) or the request
	// fails at the network level. baseDelay seeds the exponential backoff.
	maxRetries int
	baseDelay  time.Duration
	// jsonMode asks the endpoint to constrain output to a strict JSON schema.
	jsonMode bool
	// jsonFormat selects the response_format variant (json_schema or json_object).
	jsonFormat string
	// reasoningEffort is the reasoning effort level forwarded to the endpoint.
	// Empty means omit from the request so the endpoint default is used.
	reasoningEffort string
	// debug receives raw request/response traffic. It is opened once with
	// no-follow semantics so a later path replacement cannot redirect logs.
	debug   *os.File
	debugMu sync.Mutex
	// rng supplies jitter for retry backoff. Seeded per client so Go 1.19
	// (which does not auto-seed math/rand) still varies delays. Guarded by
	// rngMu because Stream may be called from more than one place.
	rng   *rand.Rand
	rngMu sync.Mutex
}

// StreamOpts controls per-request behaviour without mutating the Client.
type StreamOpts struct {
	// DisableResponseFormat omits response_format even when the client is in
	// JSON mode. Compaction uses this so summaries are plain text.
	DisableResponseFormat bool
	// DisableJSONModeParse treats streamed content as ordinary prose rather
	// than a JSON tool-call serialization.
	DisableJSONModeParse bool
}

// NewClient constructs a client. timeout bounds each HTTP attempt, including
// its streamed body. jsonMode asks the endpoint to constrain output to a tool
// call schema; jsonFormat selects json_schema (the default) or json_object.
func NewClient(chatURL, apiKey, model string, temperature float64, timeout time.Duration, jsonMode bool, jsonFormat string) *Client {
	return NewClientWithReasoning(chatURL, apiKey, model, temperature, 0, "", timeout, jsonMode, jsonFormat)
}

// NewClientWithReasoning constructs a client with explicit top-p and reasoning
// effort. The supplied reasoningEffort value is forwarded only when non-empty.
func NewClientWithReasoning(chatURL, apiKey, model string, temperature, topP float64, reasoningEffort string, timeout time.Duration, jsonMode bool, jsonFormat string) *Client {
	return newClientWithSource(chatURL, apiKey, model, temperature, topP, reasoningEffort, timeout, jsonMode, jsonFormat, rand.NewSource(time.Now().UnixNano()))
}

// newClientWithSource is the deterministic construction seam used by tests.
// The source remains private because callers do not otherwise need to control
// retry jitter.
func newClientWithSource(chatURL, apiKey, model string, temperature, topP float64, reasoningEffort string, timeout time.Duration, jsonMode bool, jsonFormat string, source rand.Source) *Client {
	if jsonFormat == "" {
		jsonFormat = "json_schema"
	}
	return &Client{
		url:             chatURL,
		apiKey:          apiKey,
		model:           model,
		temp:            temperature,
		topP:            topP,
		reasoningEffort: reasoningEffort,
		jsonMode:        jsonMode,
		jsonFormat:      jsonFormat,
		http:            &http.Client{Timeout: timeout},
		maxRetries:      5,
		baseDelay:       time.Second,
		rng:             rand.New(source),
	}
}

// Handlers receives streaming callbacks during a completion. All fields are
// optional; a nil func is simply not called.
type Handlers struct {
	// OnContent is called with each assistant text delta as it arrives.
	OnContent func(delta string)
	// OnToolName is called once per tool call the first time its name is
	// known, with the call's stream index and resolved tool name.
	OnToolName func(index int, name string)
	// OnToolArgs is called with each argument fragment for a tool call,
	// identified by its stream index, as the arguments stream in.
	OnToolArgs func(index int, fragment string)
	// OnRetry is called before a transient failure is retried, with the
	// upcoming (1-based) attempt number, the delay before it, and the error
	// that triggered the retry. Useful for keeping the UI informative.
	OnRetry func(attempt int, delay time.Duration, err error)
}

// Model returns the model name currently in use.
func (c *Client) Model() string { return c.model }

// JSONMode reports whether the client constrains output to a JSON schema.
func (c *Client) JSONMode() bool { return c.jsonMode }

// SetModel changes the model used for subsequent requests.
func (c *Client) SetModel(model string) { c.model = model }

// SetChatURL changes the chat completions endpoint for subsequent requests.
func (c *Client) SetChatURL(url string) { c.url = url }

// SetReasoningEffort changes the reasoning effort level for subsequent requests.
func (c *Client) SetReasoningEffort(level string) { c.reasoningEffort = level }

// SetAPIKey changes the API key used for subsequent requests.
func (c *Client) SetAPIKey(key string) { c.apiKey = key }

// SetDebug enables raw request/response dumping to path. The file is opened
// once with append and no-follow semantics and repaired to mode 0600.
func (c *Client) SetDebug(path string) error {
	c.debugMu.Lock()
	defer c.debugMu.Unlock()
	if c.debug != nil {
		_ = c.debug.Close()
		c.debug = nil
	}
	if path == "" {
		return nil
	}
	f, err := openDebugAppend(path)
	if err != nil {
		return err
	}
	c.debug = f
	return nil
}

// Stream sends the conversation and tool definitions, invoking h's callbacks
// as content and tool-call fragments arrive. Tool-call fragments are also
// accumulated internally and returned whole in the Result.
//
// Transient failures — a 429 (rate limit), a 5xx, or a network-level error
// before the stream begins — are retried with exponential backoff (honouring a
// Retry-After header when present) up to maxRetries times, so a rate limit no
// longer aborts the task. Handlers are invoked only once the chosen attempt
// starts streaming, so retries never double up output.
func (c *Client) Stream(ctx context.Context, messages []Message, tools []Tool, h Handlers) (*Result, error) {
	return c.StreamWithOptions(ctx, messages, tools, h, StreamOpts{})
}

// StreamWithOptions is Stream with per-request options. Compaction uses it to
// disable response_format while leaving the client's global JSON mode alone.
func (c *Client) StreamWithOptions(ctx context.Context, messages []Message, tools []Tool, h Handlers, opts StreamOpts) (*Result, error) {
	body := request{
		Model:           c.model,
		Messages:        messages,
		Tools:           tools,
		Temperature:     c.temp,
		ReasoningEffort: c.reasoningEffort,
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
	}
	if c.topP > 0 {
		body.TopP = &c.topP
	}
	if c.jsonMode && !opts.DisableResponseFormat {
		body.ResponseFormat = toolCallResponseFormat(c.jsonFormat)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	c.debugWrite("REQUEST\n" + string(raw) + "\n\n")
	var attempts int
	for attempt := 0; ; attempt++ {
		attempts++
		res, transient, ra, err := c.attempt(ctx, raw, h, opts)
		if res == nil {
			res = &Result{}
		}
		res.Attempts = attempts
		if err == nil {
			return res, nil
		}
		if !transient || ctx.Err() != nil {
			return res, err
		}
		if attempt == c.maxRetries {
			return res, fmt.Errorf("giving up after %d attempts: %w", c.maxRetries+1, err)
		}
		delay := c.backoff(attempt+1, ra)
		if h.OnRetry != nil {
			h.OnRetry(attempt+1, delay, err)
		}
		if err := sleep(ctx, delay); err != nil {
			return res, err
		}
	}
}

// attempt performs a single request. transient reports whether a returned
// error is worth retrying; ra carries any Retry-After hint from the server.
func (c *Client) attempt(ctx context.Context, raw []byte, h Handlers, opts StreamOpts) (res *Result, transient bool, ra time.Duration, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(raw))
	if err != nil {
		return nil, false, 0, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "text/event-stream")
	if c.apiKey != "" {
		req.Header.Set("Authorization", "Bearer "+c.apiKey)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// A cancelled context is terminal; any other transport error is
		// worth retrying (connection reset, temporary DNS failure, etc.).
		return nil, ctx.Err() == nil, 0, err
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		ra := parseRetryAfter(resp.Header.Get("Retry-After"))
		status := resp.Status
		resp.Body.Close()
		err := fmt.Errorf("endpoint returned %s: %s", status, strings.TrimSpace(string(snippet)))
		if resp.StatusCode == http.StatusBadRequest && c.jsonMode && schemaError(string(snippet)) {
			err = fmt.Errorf("%w; try --json-format json_object or --no-json-mode (%v)", ErrSchemaRejected, err)
		}
		return nil, retriableStatus(resp.StatusCode), ra, err
	}

	// Success: stream the body. A mid-stream failure is not retried because
	// partial output has already been delivered to the handlers.
	res, err = c.consume(resp.Body, h, opts)
	resp.Body.Close()
	return res, false, 0, err
}

// schemaError reports whether an endpoint error message is about the JSON
// schema / response_format being rejected.
func schemaError(msg string) bool {
	m := strings.ToLower(msg)
	return strings.Contains(m, "strict") ||
		strings.Contains(m, "response_format") ||
		strings.Contains(m, "json_schema") ||
		strings.Contains(m, "json_object") ||
		strings.Contains(m, "schema")
}

// retriableStatus reports whether an HTTP status warrants a retry.
func retriableStatus(code int) bool {
	switch code {
	case http.StatusTooManyRequests,
		http.StatusInternalServerError,
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

// backoff returns the delay before the given attempt (1-based). A server
// Retry-After hint wins; otherwise it is exponential with full jitter, capped.
func (c *Client) backoff(attempt int, retryAfter time.Duration) time.Duration {
	const maxDelay = 30 * time.Second
	if retryAfter > 0 {
		if retryAfter > maxDelay {
			return maxDelay
		}
		return retryAfter
	}
	d := c.baseDelay * (1 << (attempt - 1))
	if d > maxDelay || d <= 0 {
		d = maxDelay
	}
	c.rngMu.Lock()
	n := c.rng.Int63n(int64(d/2) + 1)
	c.rngMu.Unlock()
	return d/2 + time.Duration(n)
}

// parseRetryAfter interprets a Retry-After header, which may be either a number
// of seconds or an HTTP date. It returns 0 when the value is absent or invalid.
func parseRetryAfter(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs < 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := time.Until(t); d > 0 {
			return d
		}
	}
	return 0
}

// sleep waits for d or until ctx is cancelled, whichever comes first.
// debugWrite appends text to the debug log when debugging is enabled.
func (c *Client) debugWrite(text string) {
	c.debugMu.Lock()
	defer c.debugMu.Unlock()
	if c.debug == nil {
		return
	}
	_, _ = c.debug.WriteString(text)
}

// teeReader returns a reader that copies everything read into the debug log.
func (c *Client) teeReader(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		c.debugMu.Lock()
		defer c.debugMu.Unlock()
		if c.debug == nil {
			io.Copy(pw, r)
			return
		}
		_, _ = c.debug.WriteString("RESPONSE\n")
		mw := io.MultiWriter(pw, c.debug)
		io.Copy(mw, r)
		_, _ = c.debug.WriteString("\n\n")
	}()
	return pr
}

func sleep(ctx context.Context, d time.Duration) error {
	if d <= 0 {
		return ctx.Err()
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}

// consume reads the SSE stream line-by-line, merging content and tool-call
// deltas into a single Result. It is tolerant of blank lines, comments and
// non-JSON frames so no token is ever dropped on a well-formed stream.
func (c *Client) consume(r io.Reader, h Handlers, opts StreamOpts) (*Result, error) {
	c.debugMu.Lock()
	debugging := c.debug != nil
	c.debugMu.Unlock()
	if debugging {
		r = c.teeReader(r)
	}
	res := &Result{}
	// tool calls accumulated by their stream index.
	acc := map[int]*ToolCall{}
	var order []int
	parseJSON := c.jsonMode && !opts.DisableJSONModeParse

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)
	var totalSSE int
	var totalToolArgs int

	for scanner.Scan() {
		line := scanner.Text()
		totalSSE += len(line) + 1
		if totalSSE > maxStreamSSEBytes {
			return res, fmt.Errorf("SSE stream exceeds %d byte limit", maxStreamSSEBytes)
		}
		if !strings.HasPrefix(line, "data:") {
			continue // skip comments / event: lines / blanks
		}
		data := strings.TrimSpace(line[len("data:"):])
		if data == "" {
			continue
		}
		if data == "[DONE]" {
			break
		}

		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			continue // ignore malformed frames rather than aborting
		}
		if chunk.Usage != nil {
			res.Usage = *chunk.Usage
		}
		if len(chunk.Choices) == 0 {
			continue
		}
		choice := chunk.Choices[0]
		if choice.Delta.Content != "" {
			if len(res.Content)+len(choice.Delta.Content) > maxStreamContent {
				return res, fmt.Errorf("stream content exceeds %d byte limit", maxStreamContent)
			}
			res.Content += choice.Delta.Content
			// In JSON mode the content is a serialization of tool calls, not
			// prose meant for the user. Suppress live printing; it will be
			// parsed into tool calls at the end, or released as prose if parsing
			// fails.
			if h.OnContent != nil && !parseJSON {
				h.OnContent(choice.Delta.Content)
			}
		}
		// Some endpoints (e.g. llama.cpp) stream JSON-mode tool calls as
		// content deltas but never set a finish_reason on the same frame.
		// Reconstruct tool calls incrementally so the caller can dispatch them
		// as soon as the stream ends, rather than waiting for a finish_reason
		// that may never arrive.
		if parseJSON && res.Content != "" && len(res.ToolCalls) == 0 {
			if tcs, ok := parseJSONModeToolCalls(res.Content); ok {
				res.ToolCalls = tcs
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			cur, ok := acc[tc.Index]
			if !ok {
				if len(order) >= maxStreamToolCalls {
					return res, fmt.Errorf("stream exceeds %d tool call limit", maxStreamToolCalls)
				}
				cur = &ToolCall{Type: "function"}
				acc[tc.Index] = cur
				order = append(order, tc.Index)
			}
			if tc.ID != "" {
				cur.ID = tc.ID
			}
			if tc.Type != "" {
				cur.Type = tc.Type
			}
			if tc.Function.Name != "" {
				named := cur.Function.Name == ""
				cur.Function.Name += tc.Function.Name
				if named && h.OnToolName != nil {
					h.OnToolName(tc.Index, cur.Function.Name)
				}
			}
			if tc.Function.Arguments != "" {
				if len(cur.Function.Arguments)+len(tc.Function.Arguments) > maxStreamToolArgs {
					return res, fmt.Errorf("tool call arguments exceed %d byte limit", maxStreamToolArgs)
				}
				if len(tc.Function.Arguments) > maxStreamTotalToolArgs-totalToolArgs {
					return res, fmt.Errorf("aggregate tool call arguments exceed %d byte limit", maxStreamTotalToolArgs)
				}
				totalToolArgs += len(tc.Function.Arguments)
				cur.Function.Arguments += tc.Function.Arguments
				if h.OnToolArgs != nil {
					h.OnToolArgs(tc.Index, tc.Function.Arguments)
				}
			}
		}
		// In JSON mode some endpoints stream the constrained JSON array as
		// plain content deltas with no tool_calls array. Reconstruct the tool
		// calls from the accumulated content so downstream code always sees
		// native tool calls.
		if parseJSON && res.Content != "" && len(res.ToolCalls) == 0 && choice.FinishReason != "" {
			if tcs, ok := parseJSONModeToolCalls(res.Content); ok {
				res.ToolCalls = tcs
			}
		}
		if choice.FinishReason != "" {
			res.FinishReason = choice.FinishReason
		}
	}
	if err := scanner.Err(); err != nil {
		return res, err
	}

	for _, idx := range order {
		res.ToolCalls = append(res.ToolCalls, *acc[idx])
	}
	// If the endpoint streamed JSON-mode content as plain deltas, the
	// per-chunk reconstruction above may already have populated ToolCalls.
	// Make sure it is also reflected in the accumulated result.
	if parseJSON && res.Content != "" && len(res.ToolCalls) == 0 {
		if tcs, ok := parseJSONModeToolCalls(res.Content); ok {
			res.ToolCalls = tcs
		}
	}
	if err := validateToolArguments(res.ToolCalls); err != nil {
		return res, err
	}
	if parseJSON && len(res.ToolCalls) == 0 && isEmptyJSONModeArray(res.Content) {
		res.Content = ""
	}
	// If every parsed tool call is a "reply" pseudo-tool, convert its non-empty
	// messages to prose. An all-empty set becomes an empty turn for the agent's
	// bounded correction path.
	released := false
	if parseJSON && len(res.ToolCalls) > 0 {
		allReply := true
		for _, tc := range res.ToolCalls {
			if !IsReplyCall(tc) {
				allReply = false
				break
			}
		}
		if allReply {
			var b strings.Builder
			for _, tc := range res.ToolCalls {
				msg := ReplyMessage(tc)
				if msg == "" {
					continue
				}
				if b.Len() > 0 {
					b.WriteByte('\n')
				}
				b.WriteString(msg)
			}
			res.Content = b.String()
			res.ToolCalls = nil
			if h.OnContent != nil && res.Content != "" {
				h.OnContent(res.Content)
			}
			released = res.Content != ""
		}
	}
	// JSON mode content that could not be parsed as tool calls is ordinary
	// assistant prose; release it now so the caller can display it.
	if parseJSON && res.Content != "" && len(res.ToolCalls) == 0 && !released {
		if h.OnContent != nil {
			h.OnContent(res.Content)
		}
	} else if parseJSON && !released {
		// The content was a tool-call serialization; do not leave it in the
		// assistant message or the model will see its own JSON schema output.
		res.Content = ""
	}
	return res, nil
}

func validateToolArguments(calls []ToolCall) error {
	total := 0
	for _, tc := range calls {
		n := len(tc.Function.Arguments)
		if n > maxStreamToolArgs {
			return fmt.Errorf("tool call arguments exceed %d byte limit", maxStreamToolArgs)
		}
		if n > maxStreamTotalToolArgs-total {
			return fmt.Errorf("aggregate tool call arguments exceed %d byte limit", maxStreamTotalToolArgs)
		}
		total += n
	}
	return nil
}

// Bounds on what a single streamed completion may accumulate. Without them a
// runaway endpoint could exhaust memory before the request timeout fires.
const (
	maxStreamContent       = 4 << 20 // 4 MiB of assistant content
	maxStreamToolArgs      = 2 << 20 // 2 MiB of arguments per tool call
	maxStreamTotalToolArgs = 4 << 20 // 4 MiB across all tool calls
	maxStreamToolCalls     = 64
	maxStreamSSEBytes      = 16 << 20 // 16 MiB aggregate SSE payload
)
