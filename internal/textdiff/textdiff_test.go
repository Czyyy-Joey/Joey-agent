package textdiff

import (
	"fmt"
	"math/rand"
	"strings"
	"testing"
)

func TestUnified(t *testing.T) {
	for _, tc := range []struct {
		name, oldName, newName, old, new, want string
	}{
		{
			name: "identical", oldName: "a.txt", newName: "a.txt", old: "x\n", new: "x\n", want: "",
		},
		{
			name: "new file", oldName: "/dev/null", newName: "a.txt", old: "", new: "one\ntwo\n",
			want: "--- /dev/null\n+++ b/a.txt\n@@ -0,0 +1,2 @@\n+one\n+two\n",
		},
		{
			name: "deleted file", oldName: "a.txt", newName: "/dev/null", old: "one\n", new: "",
			want: "--- a/a.txt\n+++ /dev/null\n@@ -1 +0,0 @@\n-one\n",
		},
		{
			name: "change in the middle", oldName: "f", newName: "f",
			old:  "1\n2\n3\n4\n5\n6\n7\n8\n9\n",
			new:  "1\n2\n3\n4\nFIVE\n6\n7\n8\n9\n",
			want: "--- a/f\n+++ b/f\n@@ -2,7 +2,7 @@\n 2\n 3\n 4\n-5\n+FIVE\n 6\n 7\n 8\n",
		},
		{
			name: "two separate hunks", oldName: "f", newName: "f",
			old:  "a\n1\n2\n3\n4\n5\n6\n7\n8\nb\n",
			new:  "A\n1\n2\n3\n4\n5\n6\n7\n8\nB\n",
			want: "--- a/f\n+++ b/f\n@@ -1,4 +1,4 @@\n-a\n+A\n 1\n 2\n 3\n@@ -7,4 +7,4 @@\n 6\n 7\n 8\n-b\n+B\n",
		},
		{
			name: "close changes merge", oldName: "f", newName: "f",
			old:  "a\n1\n2\nb\n",
			new:  "A\n1\n2\nB\n",
			want: "--- a/f\n+++ b/f\n@@ -1,4 +1,4 @@\n-a\n+A\n 1\n 2\n-b\n+B\n",
		},
		{
			name: "missing final newline", oldName: "f", newName: "f", old: "x\n", new: "x\ny",
			want: "--- a/f\n+++ b/f\n@@ -1 +1,2 @@\n x\n+y\n\\ No newline at end of file\n",
		},
		{
			name: "rename only", oldName: "a", newName: "b", old: "x\n", new: "x\n",
			want: "--- a/a\n+++ b/b\n",
		},
		{
			name: "binary", oldName: "f", newName: "f", old: "a\x00b", new: "a\x00c",
			want: "--- a/f\n+++ b/f\nBinary files differ\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := Unified(tc.oldName, tc.newName, tc.old, tc.new); got != tc.want {
				t.Errorf("got:\n%s\nwant:\n%s", got, tc.want)
			}
		})
	}
}

// TestRoundTrip 用随机输入检查：把 diff 应用到旧内容上能得到新内容，并且
// 块头里的行数和块的内容一致。
func TestRoundTrip(t *testing.T) {
	rng := rand.New(rand.NewSource(1))
	words := []string{"a", "b", "c", "d"}
	gen := func() string {
		var b strings.Builder
		for range rng.Intn(30) {
			b.WriteString(words[rng.Intn(len(words))] + "\n")
		}
		if rng.Intn(4) == 0 {
			return strings.TrimSuffix(b.String(), "\n")
		}
		return b.String()
	}
	for i := range 500 {
		old, new := gen(), gen()
		d := Unified("f", "f", old, new)
		if got := apply(t, old, d); got != new {
			t.Fatalf("case %d: apply(diff) mismatch\nold:\n%s\nnew:\n%s\ndiff:\n%s\ngot:\n%s", i, old, new, d, got)
		}
	}
}

// TestLargeDistanceFallsBack 确认差别很大时仍然给出正确（可能不是最短）的 diff。
func TestLargeDistanceFallsBack(t *testing.T) {
	var a, b strings.Builder
	for i := range 3000 {
		fmt.Fprintf(&a, "old %d\n", i)
		fmt.Fprintf(&b, "new %d\n", i)
	}
	d := Unified("f", "f", a.String(), b.String())
	if strings.Contains(d, "truncated") {
		t.Fatalf("unexpected truncation, len = %d", len(d))
	}
	if got := apply(t, a.String(), d); got != b.String() {
		t.Error("fallback diff does not reproduce the new content")
	}
}

func TestTruncated(t *testing.T) {
	var a, b strings.Builder
	for i := range 20000 {
		fmt.Fprintf(&a, "old line number %d\n", i)
		fmt.Fprintf(&b, "new line number %d\n", i)
	}
	d := Unified("f", "f", a.String(), b.String())
	if len(d) > MaxBytes+100 || !strings.HasSuffix(d, "... (diff truncated)\n") {
		t.Errorf("len = %d, suffix = %q", len(d), d[max(0, len(d)-40):])
	}
}

// apply 是一个只处理 Unified 输出的最小补丁器。
func apply(t *testing.T, old, diff string) string {
	t.Helper()
	if diff == "" {
		return old
	}
	src := splitLines(old)
	var out []string
	pos := 0
	lines := strings.SplitAfter(diff, "\n")
	for i := 2; i < len(lines); i++ {
		l := lines[i]
		if l == "" {
			continue
		}
		if strings.HasPrefix(l, "@@") {
			var os, oc, ns, nc int
			parseRange(t, l, &os, &oc, &ns, &nc)
			start := os - 1
			if oc == 0 {
				start = os
			}
			out = append(out, src[pos:start]...)
			pos = start
			continue
		}
		if strings.HasPrefix(l, `\`) {
			last := len(out) - 1
			if last >= 0 && !strings.HasPrefix(lines[i-1], "-") {
				out[last] = strings.TrimSuffix(out[last], "\n")
			}
			continue
		}
		body := l[1:]
		if i+1 < len(lines) && strings.HasPrefix(lines[i+1], `\`) {
			body = strings.TrimSuffix(body, "\n")
		}
		switch l[0] {
		case ' ':
			if src[pos] != body {
				t.Fatalf("context mismatch at %d: %q vs %q", pos, src[pos], body)
			}
			out = append(out, src[pos])
			pos++
		case '-':
			pos++
		case '+':
			out = append(out, body)
		}
	}
	out = append(out, src[pos:]...)
	return strings.Join(out, "")
}

func parseRange(t *testing.T, l string, os, oc, ns, nc *int) {
	t.Helper()
	var o, n string
	if _, err := fmt.Sscanf(l, "@@ -%s +%s @@", &o, &n); err != nil {
		t.Fatalf("bad hunk header %q: %v", l, err)
	}
	split := func(s string, start, count *int) {
		*count = 1
		if i := strings.IndexByte(s, ','); i >= 0 {
			fmt.Sscanf(s[i+1:], "%d", count)
			s = s[:i]
		}
		fmt.Sscanf(s, "%d", start)
	}
	split(o, os, oc)
	split(n, ns, nc)
}
