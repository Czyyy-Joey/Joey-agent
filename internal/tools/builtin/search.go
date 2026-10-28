package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

const (
	searchDefaultMax = 100
	searchHardMax    = 500
	searchMaxFile    = 2 << 20
	searchMaxLine    = 300
)

// SearchFiles 在工作区中按正则表达式搜索文件内容。装了 ripgrep 时用它
// （速度快，并且遵守 .gitignore）；否则用 Go 自己遍历。两种方式都不跟随
// 符号链接，都跳过 deny_read 中的路径和二进制文件。
type SearchFiles struct {
	WS *Workspace
	// Rg 是 ripgrep 的路径；为空时在 PATH 中查找，"-" 表示不用 ripgrep。
	Rg string
}

type searchArgs struct {
	Pattern         string `json:"pattern"`
	Path            string `json:"path"`
	Glob            string `json:"glob"`
	CaseInsensitive bool   `json:"case_insensitive"`
	MaxResults      int    `json:"max_results"`
}

func (t *SearchFiles) Spec() tools.Spec {
	return tools.Spec{
		Name: "search_files",
		Description: "Search file contents in the workspace with a regular expression (RE2 / ripgrep syntax). " +
			"Returns matching lines as path:line:text. Skips .git, ignored files, binary files and symlinks.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "pattern":          {"type": "string", "description": "Regular expression, e.g. \"TODO|FIXME\" or \"func \\\\w+Handler\""},
    "path":             {"type": "string", "description": "Directory or file to search, relative to the workspace root (default: the root)"},
    "glob":             {"type": "string", "description": "Only search files whose name matches this glob, e.g. \"*.go\""},
    "case_insensitive": {"type": "boolean"},
    "max_results":      {"type": "integer", "minimum": 1, "description": "Maximum matching lines to return (default 100, max 500)"}
  },
  "required": ["pattern"],
  "additionalProperties": false
}`),
		ParallelSafe: true,
	}
}

type match struct {
	path string // 相对工作区
	line int
	text string
}

func (t *SearchFiles) Handle(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	var args searchArgs
	if err := tools.DecodeArgs(inv.Args, &args); err != nil {
		return tools.Result{}, err
	}
	if args.Pattern == "" {
		return tools.Result{}, tools.Errorf("pattern is required")
	}
	expr := args.Pattern
	if args.CaseInsensitive {
		expr = "(?i)" + expr
	}
	re, err := regexp.Compile(expr)
	if err != nil {
		return tools.Result{}, tools.Errorf("invalid regular expression: %v", err)
	}
	if args.Glob != "" {
		if _, err := filepath.Match(args.Glob, "x"); err != nil {
			return tools.Result{}, tools.Errorf("invalid glob %q: %v", args.Glob, err)
		}
	}
	limit := args.MaxResults
	if limit <= 0 {
		limit = searchDefaultMax
	}
	limit = min(limit, searchHardMax)
	root, err := t.WS.Resolve(args.Path)
	if err != nil {
		return tools.Result{}, err
	}

	var matches []match
	more := false
	engine := "go"
	if rg := t.rgPath(); rg != "" {
		engine = "ripgrep"
		matches, more, err = t.searchRg(ctx, rg, root, args, limit)
	} else {
		matches, more, err = t.searchGo(ctx, root, re, args.Glob, limit)
	}
	if err != nil {
		if ctx.Err() != nil {
			return tools.Result{}, ctx.Err()
		}
		return tools.Result{}, tools.Errorf("search failed (%s): %v", engine, err)
	}
	if len(matches) == 0 {
		return tools.Result{Content: fmt.Sprintf("No matches for %q.", args.Pattern)}, nil
	}
	var b strings.Builder
	for _, m := range matches {
		text := m.text
		if r := []rune(text); len(r) > searchMaxLine {
			text = string(r[:searchMaxLine]) + "…"
		}
		fmt.Fprintf(&b, "%s:%d:%s\n", m.path, m.line, text)
	}
	if more {
		fmt.Fprintf(&b, "[results truncated at %d matches; narrow the pattern, path or glob]\n", limit)
	}
	return tools.Result{Content: b.String()}, nil
}

func (t *SearchFiles) rgPath() string {
	switch t.Rg {
	case "-":
		return ""
	case "":
		p, err := exec.LookPath("rg")
		if err != nil {
			return ""
		}
		return p
	}
	return t.Rg
}

func (t *SearchFiles) searchRg(ctx context.Context, rg, root string, args searchArgs, limit int) ([]match, bool, error) {
	argv := []string{"--line-number", "--no-heading", "--with-filename", "--color", "never", "--no-config", "--max-filesize", "2M"}
	if args.CaseInsensitive {
		argv = append(argv, "--ignore-case")
	}
	if args.Glob != "" {
		argv = append(argv, "--glob", args.Glob)
	}
	argv = append(argv, "--regexp", args.Pattern, "--", root)
	cmd := exec.CommandContext(ctx, rg, argv...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, false, err
	}
	if err := cmd.Start(); err != nil {
		return nil, false, err
	}
	var matches []match
	more := false
	sc := bufio.NewScanner(out)
	sc.Buffer(make([]byte, 0, 64<<10), 4<<20)
	for sc.Scan() {
		path, rest, ok := strings.Cut(sc.Text(), ":")
		if !ok {
			continue
		}
		num, text, ok := strings.Cut(rest, ":")
		if !ok {
			continue
		}
		var line int
		if _, err := fmt.Sscan(num, &line); err != nil || t.WS.Denied(path) {
			continue
		}
		if len(matches) == limit {
			more = true
			break
		}
		matches = append(matches, match{path: t.WS.Rel(path), line: line, text: text})
	}
	if more {
		_ = cmd.Process.Kill()
	}
	err = cmd.Wait()
	var ee *exec.ExitError
	// 退出码 1 表示没有匹配；被我们提前杀掉时也不算错误。
	if err != nil && !more && !(errors.As(err, &ee) && ee.ExitCode() == 1) {
		return nil, false, fmt.Errorf("%v: %s", err, strings.TrimSpace(stderr.String()))
	}
	return matches, more, nil
}

// skipDirs 是 Go 遍历时跳过的目录（ripgrep 靠 .gitignore 做到同样的事）。
var skipDirs = map[string]bool{".git": true, "node_modules": true, ".joey": true}

func (t *SearchFiles) searchGo(ctx context.Context, root string, re *regexp.Regexp, glob string, limit int) ([]match, bool, error) {
	var matches []match
	more := false
	errStop := errors.New("stop")
	err := filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return nil // 读不了的目录跳过
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if t.WS.Denied(p) {
			if d.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if p != root && skipDirs[d.Name()] {
				return filepath.SkipDir
			}
			return nil
		}
		if !d.Type().IsRegular() {
			return nil
		}
		if glob != "" {
			target := d.Name()
			if strings.Contains(glob, "/") {
				target = filepath.ToSlash(t.WS.Rel(p))
			}
			if ok, _ := filepath.Match(glob, target); !ok {
				return nil
			}
		}
		if info, err := d.Info(); err != nil || info.Size() > searchMaxFile {
			return nil
		}
		data, err := os.ReadFile(p)
		if err != nil || bytes.IndexByte(data[:min(len(data), binarySniffBytes)], 0) >= 0 {
			return nil
		}
		for i, line := range strings.Split(string(data), "\n") {
			if !re.MatchString(line) {
				continue
			}
			if len(matches) == limit {
				more = true
				return errStop
			}
			matches = append(matches, match{path: t.WS.Rel(p), line: i + 1, text: strings.TrimRight(line, "\r")})
		}
		return nil
	})
	if err != nil && err != errStop {
		return nil, false, err
	}
	return matches, more, nil
}
