// Package patch 解析并应用 Codex 的 apply_patch 格式（codex-rs/apply-patch）：
//
//	*** Begin Patch
//	*** Add File: path/new.go
//	+package x
//	*** Update File: path/main.go
//	*** Move to: path/renamed.go
//	@@ func main() {
//	-	port := 8080
//	+	port := 9090
//	*** Delete File: path/old.go
//	*** End Patch
//
// Update 的每个块（以 @@ 开头）由上下文行（空格开头）、删除行（-）、新增行
// （+）组成；"@@ 某行" 先定位到这一行之后再匹配。匹配从严到宽分四级
// （seek_sequence.rs）：完全相同、忽略行尾空白、忽略首尾空白、把常见的
// Unicode 标点归一成 ASCII。
package patch

import (
	"errors"
	"fmt"
	"strings"
)

const (
	beginMarker  = "*** Begin Patch"
	endMarker    = "*** End Patch"
	addMarker    = "*** Add File: "
	deleteMarker = "*** Delete File: "
	updateMarker = "*** Update File: "
	moveMarker   = "*** Move to: "
	eofMarker    = "*** End of File"
)

// Op 是对一个文件的操作。
type Op int

const (
	Add Op = iota
	Delete
	Update
)

// Hunk 是补丁中对一个文件的修改。
type Hunk struct {
	Op   Op
	Path string
	// MoveTo 只用于 Update：修改后改名到这里。
	MoveTo string
	// Contents 只用于 Add：新文件的完整内容。
	Contents string
	// Chunks 只用于 Update。
	Chunks []Chunk
}

// Chunk 是 Update 中的一处修改。
type Chunk struct {
	// Context 是 "@@ xxx" 中的 xxx：先找到这一行，再在它之后匹配 Old。
	Context string
	Old     []string
	New     []string
	// EOF 表示这一块必须匹配到文件末尾（*** End of File）。
	EOF bool
}

// Error 是带行号的解析错误。
type Error struct {
	Line int
	Msg  string
}

func (e *Error) Error() string {
	if e.Line == 0 {
		return "invalid patch: " + e.Msg
	}
	return fmt.Sprintf("invalid patch at line %d: %s", e.Line, e.Msg)
}

// Parse 解析补丁文本。和 Codex 的宽松模式一样，接受被 heredoc（<<'EOF' … EOF）
// 包住的补丁，因为模型常常照着 shell 用法写。
func Parse(text string) ([]Hunk, error) {
	lines := strings.Split(strings.ReplaceAll(strings.TrimSpace(text), "\r\n", "\n"), "\n")
	if n := len(lines); n >= 4 && strings.HasPrefix(lines[0], "<<") && strings.HasSuffix(strings.TrimSpace(lines[n-1]), "EOF") {
		lines = lines[1 : n-1]
	}
	if len(lines) == 0 || strings.TrimSpace(lines[0]) != beginMarker {
		return nil, &Error{Msg: "the first line of the patch must be '" + beginMarker + "'"}
	}
	if len(lines) < 2 || strings.TrimSpace(lines[len(lines)-1]) != endMarker {
		return nil, &Error{Msg: "the last line of the patch must be '" + endMarker + "'"}
	}
	p := parser{lines: lines[:len(lines)-1], i: 1}
	var hunks []Hunk
	for p.i < len(p.lines) {
		h, err := p.hunk()
		if err != nil {
			return nil, err
		}
		hunks = append(hunks, h)
	}
	if len(hunks) == 0 {
		return nil, &Error{Msg: "the patch contains no file operations"}
	}
	return hunks, nil
}

type parser struct {
	lines []string
	i     int
}

func (p *parser) line() string { return p.lines[p.i] }

// lineNo 是当前行在补丁中的行号（从 1 开始）。
func (p *parser) lineNo() int { return p.i + 1 }

func (p *parser) atHeader() bool {
	l := strings.TrimSpace(p.line()) + " "
	return strings.HasPrefix(l, addMarker) || strings.HasPrefix(l, deleteMarker) || strings.HasPrefix(l, updateMarker)
}

func (p *parser) hunk() (Hunk, error) {
	// 补一个空格，让 "*** Add File:"（路径为空）也能识别为文件头。
	header := strings.TrimSpace(p.line()) + " "
	start := p.lineNo()
	p.i++
	switch {
	case strings.HasPrefix(header, addMarker):
		h := Hunk{Op: Add, Path: strings.TrimSpace(strings.TrimPrefix(header, addMarker))}
		var b strings.Builder
		for p.i < len(p.lines) && !p.atHeader() {
			l := p.line()
			if !strings.HasPrefix(l, "+") {
				return h, &Error{Line: p.lineNo(), Msg: fmt.Sprintf("every line of an added file must start with '+', got %q", l)}
			}
			b.WriteString(l[1:] + "\n")
			p.i++
		}
		h.Contents = b.String()
		return h, checkPath(h.Path, start)
	case strings.HasPrefix(header, deleteMarker):
		h := Hunk{Op: Delete, Path: strings.TrimSpace(strings.TrimPrefix(header, deleteMarker))}
		return h, checkPath(h.Path, start)
	case strings.HasPrefix(header, updateMarker):
		h := Hunk{Op: Update, Path: strings.TrimSpace(strings.TrimPrefix(header, updateMarker))}
		if err := checkPath(h.Path, start); err != nil {
			return h, err
		}
		if p.i < len(p.lines) && strings.HasPrefix(strings.TrimSpace(p.line())+" ", moveMarker) {
			h.MoveTo = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(p.line())+" ", moveMarker))
			if err := checkPath(h.MoveTo, p.lineNo()); err != nil {
				return h, err
			}
			p.i++
		}
		for p.i < len(p.lines) && !p.atHeader() {
			c, err := p.chunk(len(h.Chunks) == 0)
			if err != nil {
				return h, err
			}
			h.Chunks = append(h.Chunks, c)
		}
		if len(h.Chunks) == 0 {
			return h, &Error{Line: start, Msg: fmt.Sprintf("update for %q has no changes", h.Path)}
		}
		return h, nil
	}
	return Hunk{}, &Error{Line: start, Msg: fmt.Sprintf("expected '%s', '%s' or '%s', got %q", addMarker, deleteMarker, updateMarker, strings.TrimSpace(header))}
}

// chunk 解析一个修改块。第一个块可以省略 "@@" 行。
func (p *parser) chunk(first bool) (Chunk, error) {
	var c Chunk
	start := p.lineNo()
	switch l := p.line(); {
	case l == "@@" || strings.TrimSpace(l) == "@@":
		p.i++
	case strings.HasPrefix(l, "@@ "):
		c.Context = l[3:]
		p.i++
	case !first:
		return c, &Error{Line: start, Msg: fmt.Sprintf("expected a '@@' line to start the next change, got %q", l)}
	}
	n := 0
	for p.i < len(p.lines) && !p.atHeader() {
		l := p.line()
		if strings.TrimSpace(l) == eofMarker {
			c.EOF = true
			p.i++
			break
		}
		if strings.HasPrefix(l, "@@") {
			if n == 0 {
				return c, &Error{Line: start, Msg: "empty change block"}
			}
			break
		}
		switch {
		case l == "":
			// 模型常把空的上下文行写成真正的空行，而不是一个空格。
			c.Old = append(c.Old, "")
			c.New = append(c.New, "")
		case l[0] == ' ':
			c.Old = append(c.Old, l[1:])
			c.New = append(c.New, l[1:])
		case l[0] == '-':
			c.Old = append(c.Old, l[1:])
		case l[0] == '+':
			c.New = append(c.New, l[1:])
		default:
			return c, &Error{Line: p.lineNo(), Msg: fmt.Sprintf("change lines must start with ' ', '+' or '-', got %q", l)}
		}
		n++
		p.i++
	}
	if n == 0 {
		return c, &Error{Line: start, Msg: "empty change block"}
	}
	return c, nil
}

func checkPath(path string, line int) error {
	if path == "" {
		return &Error{Line: line, Msg: "missing file path"}
	}
	return nil
}

// Apply 把 Update 的块应用到文件内容上，返回新内容。
func Apply(content string, chunks []Chunk) (string, error) {
	crlf := strings.Contains(content, "\r\n")
	if crlf {
		content = strings.ReplaceAll(content, "\r\n", "\n")
	}
	lines := strings.Split(content, "\n")
	// 以换行结尾的文件最后会多出一个空串，它代表末尾的换行而不是一行内容。
	trailingNewline := len(lines) > 0 && lines[len(lines)-1] == ""
	if trailingNewline {
		lines = lines[:len(lines)-1]
	}

	type replacement struct {
		start, n int
		lines    []string
	}
	var reps []replacement
	at := 0
	for _, c := range chunks {
		if c.Context != "" {
			idx := seek(lines, []string{c.Context}, at, false)
			if idx < 0 {
				return "", fmt.Errorf("cannot find the context line %q", c.Context)
			}
			at = idx + 1
		}
		if len(c.Old) == 0 {
			// 纯新增：加在定位到的位置；没有 @@ 上下文时加在文件末尾。
			pos := len(lines)
			if c.Context != "" {
				pos = at
			}
			reps = append(reps, replacement{pos, 0, c.New})
			continue
		}
		old, repl := c.Old, c.New
		idx := seek(lines, old, at, c.EOF)
		if idx < 0 && old[len(old)-1] == "" {
			// 模型在块末尾多写的空行可能代表文件末尾的换行。
			old = old[:len(old)-1]
			if len(repl) > 0 && repl[len(repl)-1] == "" {
				repl = repl[:len(repl)-1]
			}
			idx = seek(lines, old, at, c.EOF)
		}
		if idx < 0 {
			return "", fmt.Errorf("cannot find these lines (they must match the file, including order):\n%s", strings.Join(c.Old, "\n"))
		}
		reps = append(reps, replacement{idx, len(old), repl})
		at = idx + len(old)
	}

	var out []string
	pos := 0
	for _, r := range reps {
		if r.start < pos {
			return "", errors.New("changes overlap or are out of order")
		}
		out = append(out, lines[pos:r.start]...)
		out = append(out, r.lines...)
		pos = r.start + r.n
	}
	out = append(out, lines[pos:]...)
	result := strings.Join(out, "\n")
	if trailingNewline || (len(out) > 0 && content == "") {
		result += "\n"
	}
	if crlf {
		result = strings.ReplaceAll(result, "\n", "\r\n")
	}
	return result, nil
}

// seek 在 lines[start:] 中查找 pattern，返回起始下标，找不到返回 -1。
// eof 为 true 时优先尝试与文件末尾对齐的位置。
func seek(lines, pattern []string, start int, eof bool) int {
	if len(pattern) == 0 {
		return start
	}
	if len(pattern) > len(lines) {
		return -1
	}
	from := start
	if eof {
		from = max(len(lines)-len(pattern), start)
	}
	for _, norm := range []func(string) string{
		func(s string) string { return s },
		func(s string) string { return strings.TrimRight(s, " \t") },
		strings.TrimSpace,
		normalizePunct,
	} {
		for _, begin := range []int{from, start} {
			for i := begin; i+len(pattern) <= len(lines); i++ {
				ok := true
				for j, p := range pattern {
					if norm(lines[i+j]) != norm(p) {
						ok = false
						break
					}
				}
				if ok {
					return i
				}
			}
			if begin == start {
				break
			}
		}
	}
	return -1
}

var punct = strings.NewReplacer(
	"‐", "-", "‑", "-", "‒", "-", "–", "-", "—", "-", "―", "-", "−", "-",
	"‘", "'", "’", "'", "‚", "'", "‛", "'",
	"“", `"`, "”", `"`, "„", `"`, "‟", `"`,
	" ", " ", " ", " ", " ", " ", " ", " ", " ", " ", " ", " ",
	" ", " ", " ", " ", " ", " ", " ", " ", " ", " ", " ", " ", "　", " ",
)

// normalizePunct 把常见的 Unicode 破折号、引号、空格换成 ASCII，再去掉首尾空白。
func normalizePunct(s string) string { return strings.TrimSpace(punct.Replace(s)) }
