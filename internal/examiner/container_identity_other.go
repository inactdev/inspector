//go:build !unix

package examiner

func containerHostUser() string {
	return "0:0"
}
