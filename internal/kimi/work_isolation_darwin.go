package kimi

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
)

func existingProfileIdentity(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", errors.New("existing_login_profile_invalid")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm()&0o022 != 0 {
		return "", errors.New("existing_login_profile_invalid")
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return "", errors.New("existing_login_profile_invalid")
	}
	return fmt.Sprintf("%d:%d:%d", stat.Dev, stat.Ino, stat.Uid), nil
}

func validateExistingProfileDirectory(path string) error {
	_, err := existingProfileIdentity(path)
	return err
}
