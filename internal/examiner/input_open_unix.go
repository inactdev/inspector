//go:build aix || darwin || dragonfly || freebsd || linux || netbsd || openbsd || solaris

package examiner

import (
	"fmt"
	"os"
	"syscall"
)

func openRegularInput(path string) (*os.File, error) {
	fd, err := syscall.Open(path, syscall.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
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
