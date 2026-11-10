//go:build darwin && !cgo

package platform

// The macOS credential store is the Security framework reached through cgo, so a
// build with CGO_ENABLED=0 cannot provide it. It reports unsupported instead of
// falling back to a file: a plaintext secret on disk would be worse than a clear
// error, and this build's macOS targets are cgo builds anyway.
func newKeychain(string) (Credentials, error) { return nil, ErrUnsupported }

// newDPAPI is the Windows store; on macOS the Keychain is used instead.
func newDPAPI(string) (Credentials, error) { return nil, ErrUnsupported }
