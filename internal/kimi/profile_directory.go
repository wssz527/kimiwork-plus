package kimi

import (
	"errors"
	"os"
	"path/filepath"
)

func validateProfileDirectory(path string) error {
	if !filepath.IsAbs(path) {
		return errors.New("existing_login_profile_invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("existing_login_profile_invalid")
	}
	return nil
}
