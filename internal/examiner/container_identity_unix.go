//go:build unix

package examiner

import (
	"fmt"
	"os"
)

func containerHostUser() string {
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}
