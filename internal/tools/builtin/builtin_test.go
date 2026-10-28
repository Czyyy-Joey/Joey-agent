package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// setupWorkspace 创建 root/ws，内含示例文件以及一个同级的机密文件。
func setupWorkspace(t *testing.T) (*Workspace, string) {
	t.Helper()
	root := t.TempDir()
	ws := filepath.Join(root, "ws")
	must(t, os.MkdirAll(filepath.Join(ws, "docs"), 0o755))
	must(t, os.WriteFile(filepath.Join(ws, "README.md"), []byte("# Joey-agent\nline two\nline three\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(ws, "docs", "a.md"), []byte("a"), 0o644))
	must(t, os.WriteFile(filepath.Join(ws, "empty.txt"), nil, 0o644))
	must(t, os.WriteFile(filepath.Join(ws, "bin.dat"), []byte{0x7f, 'E', 'L', 'F', 0, 1, 2}, 0o644))
	secret := filepath.Join(root, "secret.txt")
	must(t, os.WriteFile(secret, []byte("TOP SECRET"), 0o600))
	must(t, os.Symlink(secret, filepath.Join(ws, "escape.txt")))
	must(t, os.Symlink(root, filepath.Join(ws, "parent")))
	w, err := NewWorkspace(ws)
	must(t, err)
	return w, secret
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

func call(t *testing.T, tool tools.Tool, args string) (string, error) {
	t.Helper()
	res, err := tool.Handle(context.Background(), tools.Invocation{CallID: "c", Args: json.RawMessage(args)})
	return res.Content, err
}

func TestReadFile(t *testing.T) {
	ws, secret := setupWorkspace(t)
	rf := &ReadFile{WS: ws}

	tests := []struct {
		name     string
		args     string
		want     string // 内容中的子串
		modelErr string // 如果预期出错，则为 *ModelError 的某个子串
	}{
		{name: "whole file", args: `{"path":"README.md"}`, want: "     1\t# Joey-agent\n     2\tline two\n     3\tline three\n"},
		{name: "offset and limit", args: `{"path":"README.md","offset":2,"limit":1}`, want: "     2\tline two\n[output truncated after line 2; call read_file again with offset=3"},
		{name: "empty file", args: `{"path":"empty.txt"}`, want: "empty.txt is empty."},
		{name: "offset past end", args: `{"path":"README.md","offset":10}`, modelErr: "past the end"},
		{name: "missing path arg", args: `{}`, modelErr: "path is required"},
		{name: "unknown field", args: `{"path":"README.md","lines":3}`, modelErr: "unknown field"},
		{name: "wrong type", args: `{"path":1}`, modelErr: "invalid arguments"},
		{name: "not found", args: `{"path":"nope.md"}`, modelErr: "does not exist"},
		{name: "directory", args: `{"path":"docs"}`, modelErr: "is a directory"},
		{name: "binary", args: `{"path":"bin.dat"}`, modelErr: "binary"},
		{name: "dotdot escape", args: `{"path":"../secret.txt"}`, modelErr: "outside the allowed directories"},
		{name: "absolute escape", args: `{"path":"` + secret + `"}`, modelErr: "outside the allowed directories"},
		{name: "symlink escape", args: `{"path":"escape.txt"}`, modelErr: "resolves outside"},
		{name: "symlinked dir escape", args: `{"path":"parent/secret.txt"}`, modelErr: "resolves outside"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := call(t, rf, tt.args)
			if tt.modelErr != "" {
				if !tools.IsModelError(err) || !strings.Contains(err.Error(), tt.modelErr) {
					t.Fatalf("err = %v, want model error containing %q", err, tt.modelErr)
				}
				if strings.Contains(got, "TOP SECRET") {
					t.Fatal("secret leaked")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(got, tt.want) {
				t.Errorf("content = %q, want substring %q", got, tt.want)
			}
		})
	}
}

func TestReadFileLargeOutputTruncated(t *testing.T) {
	ws, _ := setupWorkspace(t)
	line := strings.Repeat("x", 200) + "\n"
	must(t, os.WriteFile(filepath.Join(ws.Root(), "big.txt"), []byte(strings.Repeat(line, 1000)), 0o644))
	got, err := call(t, &ReadFile{WS: ws}, `{"path":"big.txt"}`)
	must(t, err)
	if len(got) > readFileMaxBytes+200 {
		t.Errorf("output %d bytes exceeds cap", len(got))
	}
	if !strings.Contains(got, "[output truncated after line") {
		t.Error("missing truncation notice")
	}
}

func TestListDirectory(t *testing.T) {
	ws, _ := setupWorkspace(t)
	ld := &ListDirectory{WS: ws}

	got, err := call(t, ld, `{}`)
	must(t, err)
	for _, want := range []string{"./ (", "docs/\n", "README.md  (", "escape.txt"} {
		if !strings.Contains(got, want) {
			t.Errorf("listing missing %q:\n%s", want, got)
		}
	}
	if strings.Index(got, "docs/") > strings.Index(got, "README.md") {
		t.Error("directories should be listed first")
	}

	got, err = call(t, ld, `{"path":"docs"}`)
	must(t, err)
	if !strings.HasPrefix(got, "docs/ (1 entries)") {
		t.Errorf("got %q", got)
	}

	for _, args := range []string{`{"path":".."}`, `{"path":"parent"}`, `{"path":"README.md"}`} {
		if _, err := call(t, ld, args); !tools.IsModelError(err) {
			t.Errorf("%s: err = %v, want model error", args, err)
		}
	}
}
