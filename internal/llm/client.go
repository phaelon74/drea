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
	// debug, when non-empty, is the path to append raw request/response traffic
	// to for inspection.
	debug string
}

// NewClient constructs a client. timeout bounds a single request (including
// the full streamed body). jsonMode, when set, asks the endpoint to constrain
// output to a strict JSON schema describing a tool call. jsonFormat selects
// the response_format variant to send (json_schema or json_object).
func NewClient(chatURL, apiKey, model string, temperature float64, timeout time.Duration, jsonMode bool, jsonFormat string) *Client {
	return NewClientWithTopP(chatURL, apiKey, model, temperature, 0.95, timeout, jsonMode, jsonFormat)
}

// NewClientWithTopP constructs a client with explicit top-p nucleus sampling.
// The supplied topP value is always forwarded in requests.
func NewClientWithTopP(chatURL, apiKey, model string, temperature, topP float64, timeout time.Duration, jsonMode bool, jsonFormat string) *Client {
	return NewClientWithReasoning(chatURL, apiKey, model, temperature, topP, "", timeout, jsonMode, jsonFormat)
}

// NewClientWithReasoning constructs a client with explicit top-p and reasoning
// effort. The supplied reasoningEffort value is forwarded only when non-empty.
func NewClientWithReasoning(chatURL, apiKey, model string, temperature, topP float64, reasoningEffort string, timeout time.Duration, jsonMode bool, jsonFormat string) *Client {
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

// SetDebug enables raw request/response dumping to the given path.
func (c *Client) SetDebug(path string) { c.debug = path }

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
	body := request{
		Model:           c.model,
		Messages:        messages,
		Tools:           tools,
		Temperature:     c.temp,
		ReasoningEffort: c.reasoningEffort,
		Stream:          true,
		StreamOptions:   &streamOptions{IncludeUsage: true},
	}
	body.TopP = c.topP
	if c.jsonMode {
		body.ResponseFormat = toolCallResponseFormat(c.jsonFormat)
	}
	raw, err := json.Marshal(body)
	if err != nil {
		return nil, err
	}

	c.debugWrite("REQUEST\n" + string(raw) + "\n\n")
	for attempt := 0; ; attempt++ {
		res, transient, ra, err := c.attempt(ctx, raw, h)
		if err == nil {
			return res, nil
		}
		if !transient || ctx.Err() != nil {
			return nil, err
		}
		if attempt == c.maxRetries {
			return nil, fmt.Errorf("giving up after %d attempts: %w", c.maxRetries+1, err)
		}
		delay := c.backoff(attempt+1, ra)
		if h.OnRetry != nil {
			h.OnRetry(attempt+1, delay, err)
		}
		if err := sleep(ctx, delay); err != nil {
			return nil, err
		}
	}
}

// attempt performs a single request. transient reports whether a returned
// error is worth retrying; ra carries any Retry-After hint from the server.
func (c *Client) attempt(ctx context.Context, raw []byte, h Handlers) (res *Result, transient bool, ra time.Duration, err error) {
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
	res, err = c.consume(resp.Body, h)
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
	return d/2 + time.Duration(rand.Int63n(int64(d/2)+1))
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
	if c.debug == "" {
		return
	}
	f, err := os.OpenFile(c.debug, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return
	}
	defer f.Close()
	f.WriteString(text)
}

// teeReader returns a reader that copies everything read into the debug log.
func (c *Client) teeReader(r io.Reader) io.Reader {
	pr, pw := io.Pipe()
	go func() {
		defer pw.Close()
		f, err := os.OpenFile(c.debug, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
		if err != nil {
			io.Copy(pw, r)
			return
		}
		defer f.Close()
		f.WriteString("RESPONSE\n")
		mw := io.MultiWriter(pw, f)
		io.Copy(mw, r)
		f.WriteString("\n\n")
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
func (c *Client) consume(r io.Reader, h Handlers) (*Result, error) {
	if c.debug != "" {
		r = c.teeReader(r)
	}
	res := &Result{}
	// tool calls accumulated by their stream index.
	acc := map[int]*ToolCall{}
	var order []int

	scanner := bufio.NewScanner(r)
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
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
			res.Content += choice.Delta.Content
			// In JSON mode the content is a serialization of tool calls, not
			// prose meant for the user. Suppress live printing; it will be
			// parsed into tool calls at the end, or released as prose if parsing
			// fails.
			if h.OnContent != nil && !c.jsonMode {
				h.OnContent(choice.Delta.Content)
			}
		}
		// Some endpoints (e.g. llama.cpp) stream JSON-mode tool calls as
		// content deltas but never set a finish_reason on the same frame.
		// Reconstruct tool calls incrementally so the caller can dispatch them
		// as soon as the stream ends, rather than waiting for a finish_reason
		// that may never arrive.
		if c.jsonMode && res.Content != "" && len(res.ToolCalls) == 0 {
			if tcs, ok := parseJSONModeToolCalls(res.Content); ok {
				res.ToolCalls = tcs
			}
		}
		for _, tc := range choice.Delta.ToolCalls {
			cur, ok := acc[tc.Index]
			if !ok {
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
		if c.jsonMode && res.Content != "" && len(res.ToolCalls) == 0 && choice.FinishReason != "" {
			if tcs, ok := parseJSONModeToolCalls(res.Content); ok {
				res.ToolCalls = tcs
			}
		}
		if choice.FinishReason != "" {
			res.FinishReason = choice.FinishReason
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}

	for _, idx := range order {
		res.ToolCalls = append(res.ToolCalls, *acc[idx])
	}
	// If the endpoint streamed JSON-mode content as plain deltas, the
	// per-chunk reconstruction above may already have populated ToolCalls.
	// Make sure it is also reflected in the accumulated result.
	if c.jsonMode && res.Content != "" && len(res.ToolCalls) == 0 {
		if tcs, ok := parseJSONModeToolCalls(res.Content); ok {
			res.ToolCalls = tcs
		}
	}
	// If every parsed tool call is a non-empty "reply" pseudo-tool, treat the
	// whole turn as plain assistant prose so the agent stops and shows it to the
	// user. Empty replies are dropped so a confused model cannot stall the task
	// by emitting vacuous "[]" turns.
	released := false
	if c.jsonMode && len(res.ToolCalls) > 0 {
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
	if c.jsonMode && res.Content != "" && len(res.ToolCalls) == 0 && !released {
		if h.OnContent != nil {
			h.OnContent(res.Content)
		}
	} else if c.jsonMode && !released {
		// The content was a tool-call serialization; do not leave it in the
		// assistant message or the model will see its own JSON schema output.
		res.Content = ""
	}
	return res, nil
}
