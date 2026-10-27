package builtin

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

const listDirMaxEntries = 500

// ListDirectory 列出一个目录的条目（非递归）。
type ListDirectory struct {
	WS *Workspace
}

type listDirArgs struct {
	Path string `json:"path"`
}

func (t *ListDirectory) Spec() tools.Spec {
	return tools.Spec{
		Name:        "list_directory",
		Description: "List files and subdirectories of a directory in the workspace. Directories end with '/'.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path": {"type": "string", "description": "Directory path relative to the workspace root; empty or '.' for the root"}
  },
  "additionalProperties": false
}`),
		ParallelSafe: true,
	}
}

func (t *ListDirectory) Handle(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	var args listDirArgs
	if err := tools.DecodeArgs(inv.Args, &args); err != nil {
		return tools.Result{}, err
	}
	dir, err := t.WS.Resolve(args.Path)
	if err != nil {
		return tools.Result{}, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return tools.Result{}, tools.Errorf("cannot list %q: %v", args.Path, err)
	}
	sort.Slice(entries, func(i, j int) bool {
		if entries[i].IsDir() != entries[j].IsDir() {
			return entries[i].IsDir()
		}
		return entries[i].Name() < entries[j].Name()
	})

	var out strings.Builder
	fmt.Fprintf(&out, "%s (%d entries)\n", displayDir(t.WS.Rel(dir)), len(entries))
	for i, e := range entries {
		if i == listDirMaxEntries {
			fmt.Fprintf(&out, "[%d more entries not shown]\n", len(entries)-i)
			break
		}
		if e.IsDir() {
			fmt.Fprintf(&out, "%s/\n", e.Name())
			continue
		}
		size := ""
		if info, err := e.Info(); err == nil && info.Mode().IsRegular() {
			size = fmt.Sprintf("  (%d bytes)", info.Size())
		} else if e.Type()&os.ModeSymlink != 0 {
			size = "  (symlink)"
		}
		fmt.Fprintf(&out, "%s%s\n", e.Name(), size)
	}
	return tools.Result{Content: out.String()}, nil
}

func displayDir(rel string) string {
	if rel == "." {
		return "./"
	}
	return rel + "/"
}
