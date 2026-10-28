package builtin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/Czyyy-Joey/joey-agent/internal/patch"
	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/textdiff"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// writeFileMaxBytes 限制 write_file 一次写入的内容大小。
const writeFileMaxBytes = 1 << 20

// WriteFile 创建或整体覆盖一个文本文件。修改已有文件的一部分时应该用
// apply_patch，它只需要传改动的部分。
type WriteFile struct {
	Env Env
}

type writeFileArgs struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

func (t *WriteFile) Spec() tools.Spec {
	return tools.Spec{
		Name: "write_file",
		Description: "Create a file, or replace a whole file, in the workspace. Parent directories are created. " +
			"To change part of an existing file use apply_patch instead.",
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "path":    {"type": "string", "description": "File path, relative to the workspace root"},
    "content": {"type": "string", "description": "The complete new file content"}
  },
  "required": ["path", "content"],
  "additionalProperties": false
}`),
		Timeout: tools.NoTimeout,
	}
}

func (t *WriteFile) Handle(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	var args writeFileArgs
	if err := tools.DecodeArgs(inv.Args, &args); err != nil {
		return tools.Result{}, err
	}
	if args.Path == "" {
		return tools.Result{}, tools.Errorf("path is required")
	}
	if len(args.Content) > writeFileMaxBytes {
		return tools.Result{}, tools.Errorf("content is %d bytes; write_file accepts at most %d", len(args.Content), writeFileMaxBytes)
	}
	path, err := t.Env.WS.ResolveWrite(args.Path)
	if err != nil {
		return tools.Result{}, err
	}
	old, err := os.ReadFile(path)
	exists := err == nil
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return tools.Result{}, tools.Errorf("cannot read %q: %v", args.Path, err)
	}
	if exists && string(old) == args.Content {
		return tools.Result{Content: fmt.Sprintf("%s is unchanged (the content is identical).", args.Path)}, nil
	}
	rel := t.Env.WS.Rel(path)
	oldName := rel
	if !exists {
		oldName = "/dev/null"
	}
	diff := textdiff.Unified(oldName, rel, string(old), args.Content)
	if t.Env.editNeedsApproval() {
		data := protocol.ApprovalData{Kind: protocol.ApprovalPatch, Paths: []string{rel}, Patch: diff, Reason: "file changes need approval (approval=untrusted)"}
		if err := inv.RequireApproval(ctx, tools.ApprovalRequest{Data: data, Keys: []string{"patch:" + path}}); err != nil {
			return tools.Result{}, err
		}
	}
	if err := writeAtomic(path, []byte(args.Content)); err != nil {
		return tools.Result{}, tools.Errorf("cannot write %q: %v", args.Path, err)
	}
	verb := "Created"
	if exists {
		verb = "Overwrote"
	}
	return tools.Result{Content: fmt.Sprintf("%s %s (%d bytes, %d lines).", verb, rel, len(args.Content), strings.Count(args.Content, "\n")), Diff: diff}, nil
}

// ApplyPatch 用 Codex 的 apply_patch 格式新增、修改、删除、改名文件。
// 所有改动先在内存中算好、所有路径先通过守卫，然后才写盘：一处失败时
// 不会留下改了一半的文件（写盘本身失败的情况除外）。
type ApplyPatch struct {
	Env Env
}

type applyPatchArgs struct {
	Patch string `json:"patch"`
}

func (t *ApplyPatch) Spec() tools.Spec {
	return tools.Spec{
		Name: "apply_patch",
		Description: `Edit files with a patch. Format:
*** Begin Patch
*** Add File: path/new.go
+line of the new file
*** Update File: path/existing.go
@@ func main() {
 context line (starts with a space)
-old line
+new line
*** Delete File: path/old.go
*** End Patch
Rules: paths are relative to the workspace root. In an update, each change starts with "@@" (optionally followed by a line that appears just before the change), then context lines (" "), removed lines ("-") and added lines ("+"). Include about 3 lines of context so the location is unique. "*** Move to: new/path" right after "*** Update File:" renames the file.`,
		Parameters: json.RawMessage(`{
  "type": "object",
  "properties": {
    "patch": {"type": "string", "description": "The whole patch, from *** Begin Patch to *** End Patch"}
  },
  "required": ["patch"],
  "additionalProperties": false
}`),
		Timeout: tools.NoTimeout,
	}
}

// fileChange 是一个已算好的文件改动。
type fileChange struct {
	op      patch.Op
	path    string // 绝对路径
	moveTo  string // Update 改名时的绝对路径
	old     []byte // Update、Delete 时的原内容
	content []byte
}

func (t *ApplyPatch) Handle(ctx context.Context, inv tools.Invocation) (tools.Result, error) {
	var args applyPatchArgs
	if err := tools.DecodeArgs(inv.Args, &args); err != nil {
		return tools.Result{}, err
	}
	hunks, err := patch.Parse(args.Patch)
	if err != nil {
		return tools.Result{}, tools.Errorf("%v", err)
	}
	changes, err := t.plan(hunks)
	if err != nil {
		return tools.Result{}, err
	}
	paths := make([]string, 0, len(changes))
	keys := make([]string, 0, len(changes))
	var diff strings.Builder
	for _, c := range changes {
		paths = append(paths, t.Env.WS.Rel(c.path))
		keys = append(keys, "patch:"+c.path)
		if c.moveTo != "" {
			paths = append(paths, t.Env.WS.Rel(c.moveTo))
			keys = append(keys, "patch:"+c.moveTo)
		}
		diff.WriteString(t.diff(c))
	}
	if t.Env.editNeedsApproval() {
		data := protocol.ApprovalData{Kind: protocol.ApprovalPatch, Paths: paths, Patch: diff.String(), Reason: "file changes need approval (approval=untrusted)"}
		if err := inv.RequireApproval(ctx, tools.ApprovalRequest{Data: data, Keys: keys}); err != nil {
			return tools.Result{}, err
		}
	}

	var summary strings.Builder
	summary.WriteString("Success. Updated the following files:\n")
	for _, c := range changes {
		rel := t.Env.WS.Rel(c.path)
		switch c.op {
		case patch.Add:
			if err := writeAtomic(c.path, c.content); err != nil {
				return tools.Result{}, tools.Errorf("cannot write %s: %v", rel, err)
			}
			fmt.Fprintf(&summary, "A %s\n", rel)
		case patch.Delete:
			if err := os.Remove(c.path); err != nil {
				return tools.Result{}, tools.Errorf("cannot delete %s: %v", rel, err)
			}
			fmt.Fprintf(&summary, "D %s\n", rel)
		case patch.Update:
			dst := c.path
			if c.moveTo != "" {
				dst = c.moveTo
			}
			if err := writeAtomic(dst, c.content); err != nil {
				return tools.Result{}, tools.Errorf("cannot write %s: %v", t.Env.WS.Rel(dst), err)
			}
			if c.moveTo != "" {
				if err := os.Remove(c.path); err != nil {
					return tools.Result{}, tools.Errorf("cannot remove %s after moving it: %v", rel, err)
				}
				fmt.Fprintf(&summary, "M %s -> %s\n", rel, t.Env.WS.Rel(c.moveTo))
			} else {
				fmt.Fprintf(&summary, "M %s\n", rel)
			}
		}
	}
	return tools.Result{Content: summary.String(), Diff: diff.String()}, nil
}

// diff 返回一个已算好的改动的 unified diff。
func (t *ApplyPatch) diff(c fileChange) string {
	rel := t.Env.WS.Rel(c.path)
	switch c.op {
	case patch.Add:
		return textdiff.Unified("/dev/null", rel, "", string(c.content))
	case patch.Delete:
		return textdiff.Unified(rel, "/dev/null", string(c.old), "")
	}
	dst := rel
	if c.moveTo != "" {
		dst = t.Env.WS.Rel(c.moveTo)
	}
	return textdiff.Unified(rel, dst, string(c.old), string(c.content))
}

// plan 校验路径并在内存中算出每个文件的新内容。
func (t *ApplyPatch) plan(hunks []patch.Hunk) ([]fileChange, error) {
	seen := map[string]bool{}
	claim := func(p, display string) error {
		if seen[p] {
			return tools.Errorf("the patch changes %s more than once; combine the changes into one section", display)
		}
		seen[p] = true
		return nil
	}
	var out []fileChange
	for _, h := range hunks {
		path, err := t.Env.WS.ResolveWrite(h.Path)
		if err != nil {
			return nil, err
		}
		if err := claim(path, h.Path); err != nil {
			return nil, err
		}
		info, statErr := os.Stat(path)
		switch h.Op {
		case patch.Add:
			if statErr == nil {
				return nil, tools.Errorf("cannot add %s: the file already exists; use *** Update File", h.Path)
			}
			out = append(out, fileChange{op: patch.Add, path: path, content: []byte(h.Contents)})
		case patch.Delete:
			if statErr != nil {
				return nil, tools.Errorf("cannot delete %s: %v", h.Path, statErr)
			}
			if !info.Mode().IsRegular() {
				return nil, tools.Errorf("cannot delete %s: only regular files can be deleted", h.Path)
			}
			old, err := os.ReadFile(path)
			if err != nil {
				return nil, tools.Errorf("cannot read %s: %v", h.Path, err)
			}
			out = append(out, fileChange{op: patch.Delete, path: path, old: old})
		case patch.Update:
			if statErr != nil {
				return nil, tools.Errorf("cannot update %s: %v", h.Path, statErr)
			}
			if !info.Mode().IsRegular() {
				return nil, tools.Errorf("cannot update %s: not a regular file", h.Path)
			}
			old, err := os.ReadFile(path)
			if err != nil {
				return nil, tools.Errorf("cannot read %s: %v", h.Path, err)
			}
			updated, err := patch.Apply(string(old), h.Chunks)
			if err != nil {
				return nil, tools.Errorf("cannot apply the change to %s: %v\nRead the file again and retry with context lines copied exactly.", h.Path, err)
			}
			c := fileChange{op: patch.Update, path: path, old: old, content: []byte(updated)}
			if h.MoveTo != "" {
				dst, err := t.Env.WS.ResolveWrite(h.MoveTo)
				if err != nil {
					return nil, err
				}
				if _, err := os.Lstat(dst); err == nil {
					return nil, tools.Errorf("cannot move %s to %s: the destination already exists", h.Path, h.MoveTo)
				}
				if err := claim(dst, h.MoveTo); err != nil {
					return nil, err
				}
				c.moveTo = dst
			}
			out = append(out, c)
		}
	}
	return out, nil
}

// writeAtomic 先写同目录下的临时文件再改名，读者不会看到写了一半的文件。
// 已有文件的权限位保留。
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	mode := os.FileMode(0o644)
	if info, err := os.Stat(path); err == nil {
		mode = info.Mode().Perm()
	}
	f, err := os.CreateTemp(dir, "."+filepath.Base(path)+".joey-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if _, err := f.Write(data); err != nil {
		f.Close()
		return err
	}
	if err := f.Chmod(mode); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
