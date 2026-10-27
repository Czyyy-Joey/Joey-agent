package dialog

import "testing"

func TestCleanPath(t *testing.T) {
	for in, want := range map[string]string{
		"/Users/me/code/joey/\n": "/Users/me/code/joey",
		"/Users/me/a b/\r\n":     "/Users/me/a b",
		"/\n":                    "/",
		"/x":                     "/x",
		"":                       "",
	} {
		if got := cleanPath(in); got != want {
			t.Errorf("cleanPath(%q) = %q, want %q", in, got, want)
		}
	}
}
