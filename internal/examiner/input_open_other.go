//go:build !aix && !darwin && !dragonfly && !freebsd && !linux && !netbsd && !openbsd && !solaris

package examiner

import (
	"fmt"
	"os"
)

func openRegularInput(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("is not a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	info, err = file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, fmt.Errorf("is not a regular file")
	}
	return file, nil
}
