package builtin

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

const (
	readFileDefaultLimit = 2000
	readFileMaxBytes     = 64 << 10
	binarySniffBytes     = 8 << 10
)

// ReadFile 返回文本文件的行，行号从 1 开始。
type ReadFile struct {
	WS *Workspace
}

type readFileArgs struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

func (t *ReadFile) Spec() tools.Spec {
	return tools.Spec{
		Name: "read_file",
		Description: "Read a UTF-8 text file in the workspace. Returns numbered lines. " +
			"Use offset and limit to page through large files.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":   {"type": "string", "description": "File path, relative to the workspace root"},
    "offset": {"type": "integer", "minimum": 1, "description": "1-based line to start from (default 1)"},
    "limit":  {"type": "integer", "minimum": 1, "description": "Maximum number of lines (default 2000)"}
  },
  "required": ["path"],
  "additionalProperties": false
}`),
		ParallelSafe: true,
	}
}

func (t *ReadFile) Handle(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	var args readFileArgs
	if err := tools.DecodeArgs(inv.Args, &args); err != nil {
		return tools.Result{}, err
	}
	if args.Path == "" {
		return tools.Result{}, tools.Errorf("path is required")
	}
	if args.Offset < 0 || args.Limit < 0 {
		return tools.Result{}, tools.Errorf("offset and limit must be positive")
	}
	offset := max(args.Offset, 1)
	limit := args.Limit
	if limit == 0 {
		limit = readFileDefaultLimit
	}

	path, err := t.WS.Resolve(args.Path)
	if err != nil {
		return tools.Result{}, err
	}
	f, err := os.Open(path)
	if err != nil {
		return tools.Result{}, tools.Errorf("cannot open %q: %v", args.Path, err)
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return tools.Result{}, tools.Errorf("cannot stat %q: %v", args.Path, err)
	}
	if info.IsDir() {
		return tools.Result{}, tools.Errorf("%q is a directory; use list_directory", args.Path)
	}

	br := bufio.NewReader(f)
	head, _ := br.Peek(binarySniffBytes)
	if bytes.IndexByte(head, 0) >= 0 {
		return tools.Result{}, tools.Errorf("%q looks like a binary file; refusing to read it", args.Path)
	}

	var out strings.Builder
	lineNo, shown := 0, 0
	truncated := false
	for {
		if err := ctx.Err(); err != nil {
			return tools.Result{}, err
		}
		line, err := br.ReadString('\n')
		if line == "" && err != nil {
			if err != io.EOF {
				return tools.Result{}, tools.Errorf("read %q: %v", args.Path, err)
			}
			break
		}
		lineNo++
		if lineNo < offset {
			continue
		}
		if shown >= limit {
			truncated = true
			break
		}
		entry := fmt.Sprintf("%6d\t%s\n", lineNo, strings.TrimRight(line, "\r\n"))
		if out.Len()+len(entry) > readFileMaxBytes {
			truncated = true
			break
		}
		out.WriteString(entry)
		shown++
	}

	switch {
	case lineNo == 0:
		return tools.Result{Content: fmt.Sprintf("%s is empty.", args.Path)}, nil
	case shown == 0:
		return tools.Result{}, tools.Errorf("offset %d is past the end of %q (%d lines)", offset, args.Path, lineNo)
	}
	if truncated {
		fmt.Fprintf(&out, "[output truncated after line %d; call read_file again with offset=%d to continue]\n", offset+shown-1, offset+shown)
	}
	return tools.Result{Content: out.String()}, nil
}
