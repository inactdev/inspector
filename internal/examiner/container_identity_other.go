//go:build !unix && !examiner_agent

package examiner

func containerHostUser() string {
	return "0:0"
}
