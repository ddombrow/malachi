//go:build !linux

package sandbox

import "errors"

func runHelper(string) error {
	return errors.New("the sandbox helper is only used on Linux")
}
