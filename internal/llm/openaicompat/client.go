// Package openaicompat 为诸如 OneAPI 的 OpenAI 兼容
// Chat Completions 端点实现 llm.Provider。
package openaicompat

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"iter"
	"net/http"
	"strings"
	"time"

	"github.com/Czyyy-Joey/joey-agent/internal/llm"
)

// Config 用于配置 Client。
type Config struct {
	// BaseURL 包含 API 版本前缀，例如 "https://host/v1"。
	BaseURL    string
	APIKey     string
	Model      llm.ModelInfo
	HTTPClient *http.Client
	// MaxRetries 作用于响应任何部分被消费之前的失败。
	MaxRetries int
	// Backoff 返回第 n 次重试前的延迟（从 1 开始）。默认为指数退避。
	Backoff func(n int) time.Duration
}

// Client 从 OpenAI 兼容网关流式读取 chat completions。
type Client struct {
	endpoint   string
	apiKey     string
	model      llm.ModelInfo
	http       *http.Client
	maxRetries int
	backoff    func(int) time.Duration
}

var _ llm.Provider = (*Client)(nil)

// New 校验 cfg 并返回一个 Client。
func New(cfg Config) (*Client, error) {
	if cfg.BaseURL == "" {
		return nil, errors.New("openaicompat: base URL is required")
	}
	if cfg.Model.Name == "" {
		return nil, errors.New("openaicompat: model name is required")
	}
	c := &Client{
		endpoint:   strings.TrimRight(cfg.BaseURL, "/") + "/chat/completions",
		apiKey:     cfg.APIKey,
		model:      cfg.Model,
		http:       cfg.HTTPClient,
		maxRetries: cfg.MaxRetries,
		backoff:    cfg.Backoff,
	}
	if c.http == nil {
		c.http = &http.Client{}
	}
	if c.backoff == nil {
		c.backoff = func(n int) time.Duration { return time.Duration(1<<(n-1)) * 500 * time.Millisecond }
	}
	return c, nil
}

// Model 返回已配置的模型能力。
func (c *Client) Model() llm.ModelInfo { return c.model }

// Stream 实现 llm.Provider。
func (c *Client) Stream(ctx context.Context, req llm.Request) iter.Seq2[llm.StreamEvent, error] {
	return func(yield func(llm.StreamEvent, error) bool) {
		body, err := json.Marshal(buildBody(c.model, req))
		if err != nil {
			yield(llm.StreamEvent{}, fmt.Errorf("openaicompat: encode request: %w", err))
			return
		}
		resp, err := c.send(ctx, body)
		if err != nil {
			yield(llm.StreamEvent{}, err)
			return
		}
		defer resp.Body.Close()
		if c.model.Streaming {
			c.readStream(ctx, resp.Body, yield)
		} else {
			c.readCompletion(ctx, resp.Body, yield)
		}
	}
}

// send 发送 body，并重试在收到 200 响应之前发生的可重试失败。
func (c *Client) send(ctx context.Context, body []byte) (*http.Response, error) {
	for attempt := 0; ; attempt++ {
		resp, err := c.post(ctx, body)
		if err == nil {
			return resp, nil
		}
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		limit := c.maxRetries
		var apiErr *llm.APIError
		if errors.As(err, &apiErr) {
			if !apiErr.Retryable() {
				return nil, err
			}
			limit = apiErr.MaxRetries(c.maxRetries)
		}
		if attempt >= limit {
			return nil, err
		}
		t := time.NewTimer(c.backoff(attempt + 1))
		select {
		case <-ctx.Done():
			t.Stop()
			return nil, ctx.Err()
		case <-t.C:
		}
	}
}

func (c *Client) post(ctx context.Context, body []byte) (*http.Response, error) {
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.endpoint, bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Content-Type", "application/json")
	if c.model.Streaming {
		httpReq.Header.Set("Accept", "text/event-stream")
	}
	if c.apiKey != "" {
		httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	}
	resp, err := c.http.Do(httpReq)
	if err != nil {
		return nil, fmt.Errorf("openaicompat: request failed: %w", err)
	}
	if resp.StatusCode != http.StatusOK {
		defer resp.Body.Close()
		raw, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		return nil, decodeAPIError(resp.StatusCode, raw)
	}
	return resp, nil
}

type wireErrorBody struct {
	Error *struct {
		Message string `json:"message"`
		Type    string `json:"type"`
		Code    any    `json:"code"`
	} `json:"error"`
}

// decodeAPIError 根据错误响应体构建 APIError；当 raw 不包含错误对象
// 且 status 为 200 时，它返回 nil。
func decodeAPIError(status int, raw []byte) *llm.APIError {
	e := &llm.APIError{StatusCode: status}
	var body wireErrorBody
	if json.Unmarshal(raw, &body) == nil && body.Error != nil {
		e.Message = body.Error.Message
		e.Type = body.Error.Type
		if body.Error.Code != nil {
			e.Code = fmt.Sprint(body.Error.Code)
		}
	} else if status == http.StatusOK {
		return nil
	} else {
		e.Message = strings.TrimSpace(string(raw))
	}
	e.RequestID = llm.ExtractRequestID(e.Message)
	return e
}

func (c *Client) readStream(ctx context.Context, r io.Reader, yield func(llm.StreamEvent, error) bool) {
	a := newAssembler()
	done := false
	for data, err := range sseData(r) {
		if err != nil {
			yield(llm.StreamEvent{}, streamErr(ctx, err))
			return
		}
		if string(bytes.TrimSpace(data)) == "[DONE]" {
			done = true
			break
		}
		if apiErr := decodeAPIError(http.StatusOK, data); apiErr != nil {
			yield(llm.StreamEvent{}, apiErr)
			return
		}
		chunk, err := parseChunk(data)
		if err != nil {
			yield(llm.StreamEvent{}, fmt.Errorf("openaicompat: malformed chunk: %w", err))
			return
		}
		for _, ev := range a.feed(chunk) {
			if !yield(ev, nil) {
				return
			}
		}
	}
	if ctx.Err() != nil {
		yield(llm.StreamEvent{}, ctx.Err())
		return
	}
	if !done && !a.sawFinish() {
		yield(llm.StreamEvent{}, llm.ErrStreamTruncated)
		return
	}
	for _, ev := range a.finish() {
		if !yield(ev, nil) {
			return
		}
	}
}

func (c *Client) readCompletion(ctx context.Context, r io.Reader, yield func(llm.StreamEvent, error) bool) {
	raw, err := io.ReadAll(io.LimitReader(r, 32<<20))
	if err != nil {
		yield(llm.StreamEvent{}, streamErr(ctx, err))
		return
	}
	if apiErr := decodeAPIError(http.StatusOK, raw); apiErr != nil {
		yield(llm.StreamEvent{}, apiErr)
		return
	}
	chunk, err := parseChunk(raw)
	if err != nil {
		yield(llm.StreamEvent{}, fmt.Errorf("openaicompat: malformed response: %w", err))
		return
	}
	a := newAssembler()
	events := append(a.feed(chunk), a.finish()...)
	for _, ev := range events {
		if !yield(ev, nil) {
			return
		}
	}
}

func streamErr(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("openaicompat: reading response: %w", err)
}
