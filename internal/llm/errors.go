package llm

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
)

// CodeBadUpstreamStatus 是 OneAPI 在上游拒绝请求时报告的错误码；
// 即使是诸如不支持的参数这类客户端错误，它也会以 HTTP 503 的形式返回。
const CodeBadUpstreamStatus = "bad_response_status_code"

// ErrStreamTruncated 表示流在没有 finish reason 或 [DONE] 的情况下结束。
var ErrStreamTruncated = errors.New("model stream ended unexpectedly")

// APIError 是来自模型网关的错误响应。
type APIError struct {
	StatusCode int
	Type       string
	Code       string
	Message    string
	RequestID  string
}

func (e *APIError) Error() string {
	var b strings.Builder
	fmt.Fprintf(&b, "model API error: HTTP %d", e.StatusCode)
	if e.Code != "" {
		fmt.Fprintf(&b, " (%s)", e.Code)
	}
	if e.Message != "" {
		b.WriteString(": ")
		b.WriteString(e.Message)
	}
	return b.String()
}

// Retryable 报告再次发送同一个请求是否可能成功。
func (e *APIError) Retryable() bool {
	switch e.StatusCode {
	case http.StatusTooManyRequests, http.StatusRequestTimeout,
		http.StatusInternalServerError, http.StatusBadGateway,
		http.StatusServiceUnavailable, http.StatusGatewayTimeout:
		return true
	}
	return false
}

// MaxRetries 将该错误的重试次数限制在客户端配置的上限之内。
// 被包装的上游拒绝通常仍会失败，因此只重试一次。
func (e *APIError) MaxRetries(configured int) int {
	if e.Code == CodeBadUpstreamStatus {
		return min(configured, 1)
	}
	return configured
}

var requestIDPattern = regexp.MustCompile(`request id: ([A-Za-z0-9-]+)`)

// ExtractRequestID 从错误消息中找出嵌入的 OneAPI request id。
func ExtractRequestID(message string) string {
	if m := requestIDPattern.FindStringSubmatch(message); m != nil {
		return m[1]
	}
	return ""
}

// IsContextOverflow 报告 err 是否表示 prompt 超出了上下文窗口。
// 尚未观察到这种情况下的 OneAPI 错误形态，因此这里
// 匹配 OpenAI 常见的措辞。
func IsContextOverflow(err error) bool {
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		return false
	}
	if apiErr.Code == "context_length_exceeded" {
		return true
	}
	msg := strings.ToLower(apiErr.Message)
	return strings.Contains(msg, "context length") || strings.Contains(msg, "maximum context")
}
