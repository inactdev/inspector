//go:build !unix

package container

// HostUser returns "0:0": there is no uid/gid to read on a non-Unix
// host, so New runs the check container as the image's own default
// user instead of guessing one.
func HostUser() string {
	return "0:0"
}
