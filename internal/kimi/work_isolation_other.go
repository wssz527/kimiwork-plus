//go:build !darwin

package kimi

func existingProfileIdentity(path string) (string, error) {
	return "", ErrProfileModeUnsupported
}
