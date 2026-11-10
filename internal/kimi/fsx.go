package kimi

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
)

// The native writers all need the same few filesystem questions answered the
// same way: is this a symlink, is it a directory, is the error "not there".
// Centralising them keeps every module's refusal rules identical.

type pathInfo struct {
	isSymlink bool
	isDir     bool
	isFile    bool
	size      int64
	mode      os.FileMode
}

func lstatPath(path string) (pathInfo, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return pathInfo{}, err
	}
	return pathInfo{
		isSymlink: info.Mode()&os.ModeSymlink != 0,
		isDir:     info.IsDir(),
		isFile:    info.Mode().IsRegular(),
		size:      info.Size(),
		mode:      info.Mode().Perm(),
	}, nil
}

func isNotExist(err error) bool {
	return errors.Is(err, fs.ErrNotExist)
}

func isAbsPath(path string) bool { return filepath.IsAbs(path) }

func joinPath(parts ...string) string { return filepath.Join(parts...) }

// parentOf walks up `levels` directories from `path`.
func parentOf(path string, levels int) string {
	current := path
	for i := 0; i < levels; i++ {
		parent := filepath.Dir(current)
		if parent == current {
			return current
		}
		current = parent
	}
	return current
}

// writePrivateFile replaces a file atomically with owner-only permissions. The
// caller has already decided the path is safe; this only guarantees a reader
// never sees a half-written document.
func writePrivateFile(path string, content []byte, mode os.FileMode) error {
	temporary := path + ".tmp"
	if err := os.WriteFile(temporary, content, mode); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		_ = os.Remove(temporary)
		return err
	}
	return nil
}

// removeFile deletes a file if it exists, and reports whether it did.
func removeFile(path string) bool {
	if err := os.Remove(path); err != nil {
		return false
	}
	return true
}
