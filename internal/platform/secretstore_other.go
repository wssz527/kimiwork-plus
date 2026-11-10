//go:build !darwin && !windows

package platform

// newKeychain and newDPAPI have no meaning on other platforms; OpenCredentials
// reports ErrUnsupported there instead of writing a secret somewhere weaker.
func newKeychain(string) (Credentials, error) { return nil, ErrUnsupported }

func newDPAPI(string) (Credentials, error) { return nil, ErrUnsupported }
