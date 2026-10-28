package builtin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/Czyyy-Joey/joey-agent/internal/protocol"
	"github.com/Czyyy-Joey/joey-agent/internal/sandbox"
	"github.com/Czyyy-Joey/joey-agent/internal/tools"
)

// editEnv 创建 base/{ws,outside,secrets}，返回 workspace-write 策略下的 Env
// （base/secrets 在 deny_read 中）。sandbox 只在本机有 Seatbelt 时创建。
func editEnv(t *testing.T, approval sandbox.ApprovalMode) (Env, string) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	for _, d := range []string{"ws/.git", "ws/src", "outside", "secrets", "tmp"} {
		must(t, os.MkdirAll(filepath.Join(base, d), 0o755))
	}
	must(t, os.WriteFile(filepath.Join(base, "ws/src/main.go"), []byte("package main\n\nfunc main() {\n\tport := 8080\n\tserve(port)\n}\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(base, "ws/notes.txt"), []byte("TODO: one\nfine\n"), 0o644))
	must(t, os.WriteFile(filepath.Join(base, "secrets/key"), []byte("TODO: secret"), 0o600))
	must(t, os.Symlink(filepath.Join(base, "outside"), filepath.Join(base, "ws/out-link")))
	must(t, os.Symlink(filepath.Join(base, "secrets"), filepath.Join(base, "ws/sec-link")))
	// t.TempDir 在 $TMPDIR 下，而 $TMPDIR 对命令可写；换掉才能测越权写。
	t.Setenv("TMPDIR", filepath.Join(base, "tmp"))
	p, err := sandbox.NewPolicy(sandbox.WorkspaceWrite, filepath.Join(base, "ws"), sandbox.PolicyOptions{
		DenyRead: []string{filepath.Join(base, "secrets")},
	})
	must(t, err)
	env := Env{WS: WorkspaceFromGuard(sandbox.NewGuard(p)), Approval: approval}
	if sandbox.Available() {
		env.Sandbox, err = sandbox.New(p)
		must(t, err)
	}
	return env, base
}

// recordingApprover 按顺序返回 decisions，并记录收到的请求。
type recordingApprover struct {
	decisions []protocol.ReviewDecision
	requests  []tools.ApprovalRequest
}

func (a *recordingApprover) Approve(_ context.Context, _ tools.Invocation, req tools.ApprovalRequest) protocol.ReviewDecision {
	a.requests = append(a.requests, req)
	if len(a.decisions) == 0 {
		return protocol.DecisionDenied
	}
	d := a.decisions[0]
	a.decisions = a.decisions[1:]
	return d
}

func invoke(t *testing.T, tool tools.Tool, args string, ap tools.Approver) (tools.Result, error) {
	t.Helper()
	if err := tools.Validate(tool.Spec().Parameters, json.RawMessage(args)); err != nil {
		return tools.Result{}, err
	}
	return tool.Handle(context.Background(), tools.Invocation{CallID: "c", Tool: tool.Spec().Name, Args: json.RawMessage(args), Approver: ap})
}

func readFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		return "<" + err.Error() + ">"
	}
	return string(b)
}

func jsonArgs(kv ...string) string {
	m := map[string]string{}
	for i := 0; i+1 < len(kv); i += 2 {
		m[kv[i]] = kv[i+1]
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func TestWriteFile(t *testing.T) {
	env, base := editEnv(t, sandbox.ApproveOnRequest)
	wf := &WriteFile{Env: env}
	ws := base + "/ws"
	for _, tt := range []struct {
		name, path, content, wantErr string
	}{
		{"new file in new dirs", "pkg/a/b.go", "package a\n", ""},
		{"overwrite", "notes.txt", "replaced\n", ""},
		{"outside via dotdot", "../outside/x", "x", "outside the allowed directories"},
		{"absolute outside", base + "/outside/x", "x", "outside the allowed directories"},
		{"via symlink", "out-link/x", "x", "resolves outside"},
		{".git", ".git/hooks/pre-commit", "x", "outside the allowed directories"},
		{"deny_read via symlink", "sec-link/key", "x", "denied by the sandbox"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			res, err := invoke(t, wf, jsonArgs("path", tt.path, "content", tt.content), nil)
			if tt.wantErr != "" {
				if err == nil || !tools.IsModelError(err) || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if got := readFile(t, filepath.Join(ws, tt.path)); got != tt.content {
				t.Errorf("file = %q (result %q)", got, res.Content)
			}
		})
	}
	for _, f := range []string{base + "/outside/x", ws + "/.git/hooks/pre-commit"} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was written", f)
		}
	}
	if readFile(t, base+"/secrets/key") != "TODO: secret" {
		t.Error("deny_read file changed")
	}
	if res, _ := invoke(t, wf, jsonArgs("path", "notes.txt", "content", "replaced\n"), nil); !strings.Contains(res.Content, "unchanged") || res.Diff != "" {
		t.Errorf("identical write = %q, diff %q", res.Content, res.Diff)
	}
	res, err := invoke(t, wf, jsonArgs("path", "notes.txt", "content", "replaced\nmore\n"), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "--- a/notes.txt\n+++ b/notes.txt\n@@ -1 +1,2 @@\n replaced\n+more\n"; res.Diff != want {
		t.Errorf("overwrite diff = %q, want %q", res.Diff, want)
	}
	res, err = invoke(t, wf, jsonArgs("path", "fresh.txt", "content", "hi\n"), nil)
	if err != nil || res.Diff != "--- /dev/null\n+++ b/fresh.txt\n@@ -0,0 +1 @@\n+hi\n" {
		t.Errorf("new file diff = %q, err %v", res.Diff, err)
	}
	if _, err := invoke(t, wf, `{"path":"x"}`, nil); err == nil || !strings.Contains(err.Error(), `missing required field "content"`) {
		t.Errorf("missing content = %v", err)
	}
}

func TestWriteFileReadOnlyPolicy(t *testing.T) {
	ws := t.TempDir()
	w, err := NewWorkspace(ws)
	must(t, err)
	_, err = invoke(t, &WriteFile{Env: Env{WS: w}}, jsonArgs("path", "a.txt", "content", "x"), nil)
	if err == nil || !strings.Contains(err.Error(), "read-only") {
		t.Errorf("err = %v", err)
	}
}

func TestEditApprovalUntrusted(t *testing.T) {
	env, base := editEnv(t, sandbox.ApproveUntrusted)
	ap := &recordingApprover{decisions: []protocol.ReviewDecision{protocol.DecisionDenied, protocol.DecisionApproved}}
	wf := &WriteFile{Env: env}
	args := jsonArgs("path", "new.txt", "content", "hello\n")
	if _, err := invoke(t, wf, args, ap); err == nil || !strings.Contains(err.Error(), "denied") {
		t.Fatalf("denied write = %v", err)
	}
	if _, err := os.Stat(base + "/ws/new.txt"); err == nil {
		t.Fatal("denied write happened")
	}
	if _, err := invoke(t, wf, args, ap); err != nil {
		t.Fatal(err)
	}
	if readFile(t, base+"/ws/new.txt") != "hello\n" || len(ap.requests) != 2 {
		t.Errorf("approved write: file=%q requests=%d", readFile(t, base+"/ws/new.txt"), len(ap.requests))
	}
	d := ap.requests[0].Data
	if d.Kind != protocol.ApprovalPatch || d.Paths[0] != "new.txt" || !strings.Contains(d.Patch, "+hello") || ap.requests[0].Keys[0] != "patch:"+base+"/ws/new.txt" {
		t.Errorf("request = %+v", ap.requests[0])
	}
	// on-request 下修改文件不需要审批。
	env2, _ := editEnv(t, sandbox.ApproveOnRequest)
	ap2 := &recordingApprover{}
	if _, err := invoke(t, &WriteFile{Env: env2}, args, ap2); err != nil || len(ap2.requests) != 0 {
		t.Errorf("on-request write: %v, %d requests", err, len(ap2.requests))
	}
}

const mainPatch = `*** Begin Patch
*** Update File: src/main.go
@@ func main() {
-	port := 8080
+	port := 9090
 	serve(port)
*** Add File: src/util.go
+package main
*** Delete File: notes.txt
*** End Patch`

func TestApplyPatch(t *testing.T) {
	env, base := editEnv(t, sandbox.ApproveOnRequest)
	ap := &ApplyPatch{Env: env}
	ws := base + "/ws"
	res, err := invoke(t, ap, jsonArgs("patch", mainPatch), nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := "M src/main.go\nA src/util.go\nD notes.txt\n"; !strings.Contains(res.Content, want) {
		t.Errorf("summary = %q", res.Content)
	}
	if got := readFile(t, ws+"/src/main.go"); !strings.Contains(got, "port := 9090") || strings.Contains(got, "8080") {
		t.Errorf("main.go = %q", got)
	}
	if readFile(t, ws+"/src/util.go") != "package main\n" {
		t.Error("util.go not added")
	}
	if _, err := os.Stat(ws + "/notes.txt"); err == nil {
		t.Error("notes.txt not deleted")
	}
	for _, want := range []string{
		"--- a/src/main.go\n+++ b/src/main.go\n@@ -1,6 +1,6 @@\n",
		"-\tport := 8080\n+\tport := 9090\n",
		"--- /dev/null\n+++ b/src/util.go\n@@ -0,0 +1 @@\n+package main\n",
		"--- a/notes.txt\n+++ /dev/null\n",
	} {
		if !strings.Contains(res.Diff, want) {
			t.Errorf("diff is missing %q:\n%s", want, res.Diff)
		}
	}
	if strings.Contains(res.Content, "---") {
		t.Errorf("the diff leaked into the model output: %q", res.Content)
	}

	// 改名。
	move := "*** Begin Patch\n*** Update File: src/util.go\n*** Move to: lib/util.go\n@@\n-package main\n+package lib\n*** End Patch"
	res, err = invoke(t, ap, jsonArgs("patch", move), nil)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(res.Diff, "--- a/src/util.go\n+++ b/lib/util.go\n@@ -1 +1 @@\n-package main\n+package lib\n") {
		t.Errorf("move diff = %q", res.Diff)
	}
	if readFile(t, ws+"/lib/util.go") != "package lib\n" {
		t.Error("move failed")
	}
	if _, err := os.Stat(ws + "/src/util.go"); err == nil {
		t.Error("source of the move still exists")
	}
}

func TestApplyPatchRejectsWithoutPartialWrites(t *testing.T) {
	env, base := editEnv(t, sandbox.ApproveOnRequest)
	ap := &ApplyPatch{Env: env}
	ws := base + "/ws"
	original := readFile(t, ws+"/src/main.go")
	for name, tt := range map[string]struct{ patch, want string }{
		"outside":          {"*** Begin Patch\n*** Update File: src/main.go\n@@\n-\tport := 8080\n+\tport := 1\n*** Add File: ../outside/evil\n+x\n*** End Patch", "outside the allowed directories"},
		"via symlink":      {"*** Begin Patch\n*** Add File: out-link/evil\n+x\n*** End Patch", "resolves outside"},
		".git":             {"*** Begin Patch\n*** Add File: .git/hooks/post-checkout\n+x\n*** End Patch", "outside the allowed directories"},
		"move into .git":   {"*** Begin Patch\n*** Update File: src/main.go\n*** Move to: .git/main.go\n@@\n-\tport := 8080\n+\tport := 1\n*** End Patch", "outside the allowed directories"},
		"context mismatch": {"*** Begin Patch\n*** Update File: src/main.go\n@@\n-\tport := 7070\n+\tport := 1\n*** End Patch", "cannot find these lines"},
		"add existing":     {"*** Begin Patch\n*** Add File: src/main.go\n+x\n*** End Patch", "already exists"},
		"delete missing":   {"*** Begin Patch\n*** Delete File: nope.txt\n*** End Patch", "cannot delete"},
		"same file twice":  {"*** Begin Patch\n*** Delete File: src/main.go\n*** Delete File: src/main.go\n*** End Patch", "more than once"},
		"bad syntax":       {"*** Begin Patch\n*** Frobnicate: x\n*** End Patch", "invalid patch at line 2"},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := invoke(t, ap, jsonArgs("patch", tt.patch), nil)
			if err == nil || !tools.IsModelError(err) || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want %q", err, tt.want)
			}
			if readFile(t, ws+"/src/main.go") != original {
				t.Error("main.go changed although the patch was rejected")
			}
		})
	}
	for _, f := range []string{base + "/outside/evil", ws + "/.git/hooks/post-checkout", ws + "/.git/main.go"} {
		if _, err := os.Stat(f); err == nil {
			t.Errorf("%s was written", f)
		}
	}
}

func TestApplyPatchApprovalKeys(t *testing.T) {
	env, base := editEnv(t, sandbox.ApproveUntrusted)
	ap := &recordingApprover{decisions: []protocol.ReviewDecision{protocol.DecisionApproved}}
	if _, err := invoke(t, &ApplyPatch{Env: env}, jsonArgs("patch", mainPatch), ap); err != nil {
		t.Fatal(err)
	}
	req := ap.requests[0]
	if strings.Join(req.Data.Paths, ",") != "src/main.go,src/util.go,notes.txt" || !strings.HasPrefix(req.Data.Patch, "--- a/src/main.go\n") || !strings.Contains(req.Data.Patch, "+\tport := 9090\n") || len(req.Keys) != 3 || req.Keys[0] != "patch:"+base+"/ws/src/main.go" {
		t.Errorf("request = %+v", req)
	}
}

func TestSearchFiles(t *testing.T) {
	env, base := editEnv(t, sandbox.ApproveOnRequest)
	must(t, os.WriteFile(base+"/ws/src/b.go", []byte("// todo: lower\n// TODO: upper\n"), 0o644))
	must(t, os.WriteFile(base+"/ws/.git/config", []byte("TODO in git"), 0o644))
	must(t, os.WriteFile(base+"/ws/bin.dat", []byte("TODO\x00binary"), 0o644))
	must(t, os.WriteFile(base+"/outside/o.txt", []byte("TODO outside"), 0o644))
	for _, rg := range []string{"-", ""} {
		name := "go"
		if rg == "" {
			name = "ripgrep"
			if (&SearchFiles{}).rgPath() == "" {
				continue
			}
		}
		t.Run(name, func(t *testing.T) {
			sf := &SearchFiles{WS: env.WS, Rg: rg}
			res, err := invoke(t, sf, `{"pattern":"TODO"}`, nil)
			if err != nil {
				t.Fatal(err)
			}
			for _, want := range []string{"notes.txt:1:TODO: one", "src/b.go:2:// TODO: upper"} {
				if !strings.Contains(res.Content, want) {
					t.Errorf("missing %q in\n%s", want, res.Content)
				}
			}
			for _, bad := range []string{"secret", ".git", "bin.dat", "outside", "lower"} {
				if strings.Contains(res.Content, bad) {
					t.Errorf("result contains %q:\n%s", bad, res.Content)
				}
			}
			if res, _ := invoke(t, sf, `{"pattern":"todo","case_insensitive":true,"glob":"*.go"}`, nil); !strings.Contains(res.Content, "src/b.go:1:") || strings.Contains(res.Content, "notes.txt") {
				t.Errorf("glob + case = %s", res.Content)
			}
			if res, _ := invoke(t, sf, `{"pattern":"TODO","max_results":1}`, nil); !strings.Contains(res.Content, "truncated at 1") {
				t.Errorf("limit = %s", res.Content)
			}
			if res, _ := invoke(t, sf, `{"pattern":"nothing-matches-this"}`, nil); !strings.HasPrefix(res.Content, "No matches") {
				t.Errorf("no match = %s", res.Content)
			}
		})
	}
	sf := &SearchFiles{WS: env.WS, Rg: "-"}
	for args, want := range map[string]string{
		`{"pattern":"("}`:                     "invalid regular expression",
		`{"pattern":"x","path":"../outside"}`: "outside the allowed directories",
		`{"pattern":"x","path":"sec-link"}`:   "denied by the sandbox",
		`{"pattern":"x","glob":"[" }`:         "invalid glob",
		`{"pattern":"x","max_results":0}`:     "must be at least 1",
		`{"pattern":"x","path":"out-link"}`:   "resolves outside",
	} {
		if _, err := invoke(t, sf, args, nil); err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("%s: err = %v, want %q", args, err, want)
		}
	}
}
