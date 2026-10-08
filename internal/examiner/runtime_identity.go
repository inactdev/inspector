package examiner

import runtimeidentity "github.com/inactdev/inspector"

func RuntimeSourceFingerprint() string {
	return runtimeidentity.Fingerprint()
}
