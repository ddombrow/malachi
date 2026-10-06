//go:build !darwin && !linux

package sandbox

import (
	"errors"
	"os"
)

func fdPath(*os.File) (string, error) { return "", errors.ErrUnsupported }
