package history

import (
	"fmt"
	"unicode/utf8"
)

// TruncateMiddle 把 s 截到约 maxTokens 个 token：保留开头和结尾各一半，
// 中间换成 "…N tokens truncated…"。截断点不会落在多字节字符中间。没有
// 超出时原样返回 s 和 false。对应 Codex 的 truncate_middle_with_token_budget。
func TruncateMiddle(s string, maxTokens int) (string, bool) {
	maxBytes := maxTokens * BytesPerToken
	if maxTokens <= 0 || len(s) <= maxBytes {
		return s, false
	}
	left := maxBytes / 2
	right := maxBytes - left
	head := floorRune(s, left)
	tail := ceilRune(s, len(s)-right)
	removed := (len(s) - maxBytes + BytesPerToken - 1) / BytesPerToken
	return s[:head] + fmt.Sprintf("…%d tokens truncated…", removed) + s[tail:], true
}

// floorRune 返回不大于 i 的、位于字符边界上的下标。
func floorRune(s string, i int) int {
	for i > 0 && i < len(s) && !utf8.RuneStart(s[i]) {
		i--
	}
	return i
}

// ceilRune 返回不小于 i 的、位于字符边界上的下标。
func ceilRune(s string, i int) int {
	for i < len(s) && !utf8.RuneStart(s[i]) {
		i++
	}
	return i
}
