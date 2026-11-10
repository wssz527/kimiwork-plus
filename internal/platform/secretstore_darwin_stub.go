//go:build darwin

package platform

// newDPAPI is the Windows store; on macOS the Keychain is used instead.
func newDPAPI(string) (Credentials, error) { return nil, ErrUnsupported }
