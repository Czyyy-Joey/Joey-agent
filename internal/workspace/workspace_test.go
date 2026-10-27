package workspace

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func mkdirs(t *testing.T, base string, dirs ...string) {
	t.Helper()
	for _, d := range dirs {
		if err := os.MkdirAll(filepath.Join(base, d), 0o755); err != nil {
			t.Fatal(err)
		}
	}
}

// setup 返回 base/{def, allowed/{a,b,.hidden}, outside}，以及一个指向 outside 的符号链接。
func setup(t *testing.T) (string, *Policy) {
	t.Helper()
	base, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	mkdirs(t, base, "def", "allowed/a", "allowed/B", "allowed/.hidden", "outside")
	if err := os.WriteFile(filepath.Join(base, "allowed", "file.txt"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(base, "outside"), filepath.Join(base, "allowed", "escape")); err != nil {
		t.Fatal(err)
	}
	p, err := NewPolicy(filepath.Join(base, "def"), []string{filepath.Join(base, "allowed")})
	if err != nil {
		t.Fatal(err)
	}
	return base, p
}

func TestResolve(t *testing.T) {
	base, p := setup(t)
	tests := []struct {
		dir     string
		want    string
		wantErr error
	}{
		{"", base + "/def", nil},
		{base + "/def", base + "/def", nil},
		{base + "/allowed/a", base + "/allowed/a", nil},
		{base + "/allowed/a/../B/", base + "/allowed/B", nil},
		{"allowed/a", "", ErrInvalid},
		{base + "/outside", "", ErrOutside},
		{base + "/allowed/escape", "", ErrOutside},
		{base + "/allowed/../outside", "", ErrOutside},
		{base + "/allowed/missing", "", ErrNotFound},
		{base + "/allowed/file.txt", "", ErrNotFound},
		{"/", "", ErrOutside},
	}
	for _, tt := range tests {
		got, err := p.Resolve(tt.dir)
		if tt.wantErr != nil {
			if !errors.Is(err, tt.wantErr) {
				t.Errorf("Resolve(%q) err = %v, want %v", tt.dir, err, tt.wantErr)
			}
			continue
		}
		if err != nil || got != tt.want {
			t.Errorf("Resolve(%q) = %q, %v; want %q", tt.dir, got, err, tt.want)
		}
	}
}

func TestNewPolicy(t *testing.T) {
	base, _ := setup(t)
	if _, err := NewPolicy(filepath.Join(base, "missing"), nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing default: %v", err)
	}
	if _, err := NewPolicy(base+"/def", []string{base + "/missing"}); !errors.Is(err, ErrNotFound) {
		t.Errorf("missing root: %v", err)
	}
	p, err := NewPolicy(base+"/allowed/a", []string{base + "/allowed", base + "/allowed", base + "/def"})
	if err != nil {
		t.Fatal(err)
	}
	roots := p.Roots()
	if len(roots) != 2 || roots[0] != base+"/allowed" || roots[1] != base+"/def" {
		t.Errorf("roots = %v", roots)
	}
	if p.Default() != base+"/allowed/a" {
		t.Errorf("default = %s", p.Default())
	}
}

func TestList(t *testing.T) {
	base, p := setup(t)

	l, err := p.List("", false)
	if err != nil {
		t.Fatal(err)
	}
	if l.Path != "" || len(l.Entries) != 2 || l.Default != base+"/def" {
		t.Errorf("roots listing = %+v", l)
	}

	l, err = p.List(base+"/allowed", false)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, e := range l.Entries {
		names = append(names, e.Name)
	}
	// escape 是指向目录的符号链接，会列出；选择它时由 Resolve 拒绝。
	if got := filepath.Join(names...); got != filepath.Join("a", "B", "escape") {
		t.Errorf("entries = %v", names)
	}
	if l.Parent != "" {
		t.Errorf("parent of a root should be empty, got %q", l.Parent)
	}

	l, _ = p.List(base+"/allowed", true)
	if len(l.Entries) != 4 {
		t.Errorf("with hidden = %+v", l.Entries)
	}

	l, _ = p.List(base+"/allowed/a", false)
	if l.Parent != base+"/allowed" {
		t.Errorf("parent = %q", l.Parent)
	}

	if _, err := p.List(base+"/outside", false); !errors.Is(err, ErrOutside) {
		t.Errorf("outside: %v", err)
	}
}

func TestExpandHome(t *testing.T) {
	home, _ := os.UserHomeDir()
	for in, want := range map[string]string{"~": home, "~/x/y": filepath.Join(home, "x/y"), "/a/~": "/a/~", "~user": "~user"} {
		if got := ExpandHome(in); got != want {
			t.Errorf("ExpandHome(%q) = %q, want %q", in, got, want)
		}
	}
}
