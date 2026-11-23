//go:build !windows

package kimi

import (
	"errors"
	"os"
)

func validateProfileDirectoryPlatform(string, os.FileInfo) error {
	return errors.New("existing_login_profile_invalid")
}
