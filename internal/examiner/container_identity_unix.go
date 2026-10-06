//go:build unix && !examiner_agent

package examiner

import (
	"fmt"
	"os"
)

func containerHostUser() string {
	return fmt.Sprintf("%d:%d", os.Getuid(), os.Getgid())
}
