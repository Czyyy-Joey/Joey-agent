package patch

import (
	"reflect"
	"strings"
	"testing"
)

func TestParse(t *testing.T) {
	text := `*** Begin Patch
*** Add File: path/add.py
+abc
+def
*** Delete File: path/delete.py
*** Update File: path/update.py
*** Move to: path/update2.py
@@ def f():
-    pass
+    return 123
*** End Patch`
	hunks, err := Parse(text)
	if err != nil {
		t.Fatal(err)
	}
	want := []Hunk{
		{Op: Add, Path: "path/add.py", Contents: "abc\ndef\n"},
		{Op: Delete, Path: "path/delete.py"},
		{Op: Update, Path: "path/update.py", MoveTo: "path/update2.py", Chunks: []Chunk{
			{Context: "def f():", Old: []string{"    pass"}, New: []string{"    return 123"}},
		}},
	}
	if !reflect.DeepEqual(hunks, want) {
		t.Errorf("hunks = %+v", hunks)
	}

	// 第一块可以省略 @@；上下文行同时进入 Old 和 New；End of File 标记。
	hunks, err = Parse("*** Begin Patch\n*** Update File: f.py\n import foo\n+bar\n@@\n+tail\n*** End of File\n*** End Patch")
	if err != nil {
		t.Fatal(err)
	}
	if got := hunks[0].Chunks; len(got) != 2 ||
		!reflect.DeepEqual(got[0], Chunk{Old: []string{"import foo"}, New: []string{"import foo", "bar"}}) ||
		!reflect.DeepEqual(got[1], Chunk{New: []string{"tail"}, EOF: true}) {
		t.Errorf("chunks = %+v", got)
	}

	// heredoc 包裹和 CRLF 都能接受。
	if _, err := Parse("<<'EOF'\r\n*** Begin Patch\r\n*** Delete File: x\r\n*** End Patch\r\nEOF"); err != nil {
		t.Errorf("heredoc: %v", err)
	}
}

func TestParseErrors(t *testing.T) {
	for text, want := range map[string]string{
		"bad":                                   "first line of the patch must be '*** Begin Patch'",
		"*** Begin Patch\nbad":                  "last line of the patch must be '*** End Patch'",
		"*** Begin Patch\n*** End Patch":        "no file operations",
		"*** Begin Patch\nhello\n*** End Patch": `line 2: expected '*** Add File: '`,
		"*** Begin Patch\n*** Update File: a\n*** End Patch":             `update for "a" has no changes`,
		"*** Begin Patch\n*** Add File: a\nno plus\n*** End Patch":       `line 3: every line of an added file must start with '+'`,
		"*** Begin Patch\n*** Update File: a\n@@\n*x\n*** End Patch":     `line 4: change lines must start with`,
		"*** Begin Patch\n*** Add File: \n+x\n*** End Patch":             "missing file path",
		"*** Begin Patch\n*** Update File: a\n@@\n@@\n+x\n*** End Patch": "empty change block",
	} {
		_, err := Parse(text)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Parse(%q) = %v, want %q", text, err, want)
		}
	}
}

func TestApply(t *testing.T) {
	src := "package main\n\nfunc main() {\n\tport := 8080\n\tserve(port)\n}\n"
	tests := []struct {
		name   string
		in     string
		chunks []Chunk
		want   string
		err    string
	}{
		{"replace", src, []Chunk{{Old: []string{"\tport := 8080"}, New: []string{"\tport := 9090"}}},
			strings.Replace(src, "8080", "9090", 1), ""},
		{"with context line", src, []Chunk{{Context: "func main() {", Old: []string{"\tport := 8080", "\tserve(port)"}, New: []string{"\tport := 1", "\tserve(port)"}}},
			strings.Replace(src, "8080", "1", 1), ""},
		{"insert after context", src, []Chunk{{Context: "func main() {", New: []string{"\tlog()"}}},
			strings.Replace(src, "{\n", "{\n\tlog()\n", 1), ""},
		{"append at end", "a\n", []Chunk{{New: []string{"b"}}}, "a\nb\n", ""},
		{"delete lines", "a\nb\nc\n", []Chunk{{Old: []string{"a", "b", "c"}, New: []string{"a", "c"}}}, "a\nc\n", ""},
		{"trailing whitespace tolerated", "x := 1   \n", []Chunk{{Old: []string{"x := 1"}, New: []string{"x := 2"}}}, "x := 2\n", ""},
		{"indentation tolerated", "    x := 1\n", []Chunk{{Old: []string{"x := 1"}, New: []string{"    x := 2"}}}, "    x := 2\n", ""},
		{"unicode punctuation", "s := “hi” — ok\n", []Chunk{{Old: []string{`s := "hi" - ok`}, New: []string{"s := 1"}}}, "s := 1\n", ""},
		{"extra blank line at end of chunk", "a\nb\n", []Chunk{{Old: []string{"b", ""}, New: []string{"c", ""}}}, "a\nc\n", ""},
		{"eof prefers the last match", "x\ny\nx\n", []Chunk{{Old: []string{"x"}, New: []string{"z"}, EOF: true}}, "x\ny\nz\n", ""},
		{"two chunks in order", "a\nb\nc\nd\n", []Chunk{{Old: []string{"a"}, New: []string{"A"}}, {Old: []string{"c"}, New: []string{"C"}}}, "A\nb\nC\nd\n", ""},
		{"crlf preserved", "a\r\nb\r\n", []Chunk{{Old: []string{"b"}, New: []string{"B"}}}, "a\r\nB\r\n", ""},
		{"no trailing newline kept", "a\nb", []Chunk{{Old: []string{"b"}, New: []string{"B"}}}, "a\nB", ""},
		{"missing lines", src, []Chunk{{Old: []string{"\tport := 7070"}, New: []string{"x"}}}, "", "cannot find these lines"},
		{"missing context", src, []Chunk{{Context: "func other() {", Old: []string{"x"}}}, "", `cannot find the context line "func other() {"`},
		{"chunks out of order", "a\nb\n", []Chunk{{Old: []string{"b"}, New: []string{"B"}}, {Old: []string{"a"}, New: []string{"A"}}}, "", "cannot find these lines"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Apply(tt.in, tt.chunks)
			if tt.err != "" {
				if err == nil || !strings.Contains(err.Error(), tt.err) {
					t.Fatalf("err = %v, want %q", err, tt.err)
				}
				return
			}
			if err != nil || got != tt.want {
				t.Fatalf("Apply = %q, %v; want %q", got, err, tt.want)
			}
		})
	}
}

func TestParseAndApplyRoundTrip(t *testing.T) {
	hunks, err := Parse(`*** Begin Patch
*** Update File: main.go
@@ func main() {
-	port := 8080
+	port := 9090
 	serve(port)
*** End Patch`)
	if err != nil {
		t.Fatal(err)
	}
	got, err := Apply("package main\n\nfunc main() {\n\tport := 8080\n\tserve(port)\n}\n", hunks[0].Chunks)
	if err != nil || !strings.Contains(got, "port := 9090\n\tserve(port)\n}") {
		t.Errorf("got %q, %v", got, err)
	}
}
