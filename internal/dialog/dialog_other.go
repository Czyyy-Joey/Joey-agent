//go:build !darwin

package dialog

import "context"

func pickFolder(context.Context, string, string) (string, error) { return "", ErrUnsupported }
