//go:build windows

package fleetagent_test

import "errors"

func mkfifo(string) error { return errors.New("no pipes in a directory here") }
