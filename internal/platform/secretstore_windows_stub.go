//go:build windows

package platform

// newKeychain is the macOS store; on Windows DPAPI is used instead.
func newKeychain(string) (Credentials, error) { return nil, ErrUnsupported }
