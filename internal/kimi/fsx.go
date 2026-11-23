package kimi

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

func equalPath(a, b string) bool {
	return equalPathForOS(runtime.GOOS, a, b)
}

func equalPathForOS(goos, a, b string) bool {
	if a == "" || b == "" {
		return a == b
	}
	cleanA := filepath.Clean(a)
	cleanB := filepath.Clean(b)
	if goos == "windows" {
		cleanA = strings.ReplaceAll(cleanA, `\`, "/")
		cleanB = strings.ReplaceAll(cleanB, `\`, "/")
		return strings.EqualFold(cleanA, cleanB)
	}
	return cleanA == cleanB
}

func pathWithinDir(candidate, dir string) bool {
	return pathWithinDirForOS(runtime.GOOS, candidate, dir)
}

func pathWithinDirForOS(goos, candidate, dir string) bool {
	if candidate == "" || dir == "" {
		return false
	}
	cleanCandidate := filepath.Clean(candidate)
	cleanDir := filepath.Clean(dir)
	if equalPathForOS(goos, cleanCandidate, cleanDir) {
		return true
	}
	if goos == "windows" {
		normCandidate := strings.ReplaceAll(cleanCandidate, `\`, "/")
		normDir := strings.ReplaceAll(cleanDir, `\`, "/")
		if !strings.HasSuffix(normDir, "/") {
			normDir += "/"
		}
		if len(normCandidate) < len(normDir) {
			return false
		}
		return strings.EqualFold(normCandidate[:len(normDir)], normDir)
	}
	sep := string(filepath.Separator)
	if !strings.HasSuffix(cleanDir, sep) {
		cleanDir += sep
	}
	return strings.HasPrefix(cleanCandidate, cleanDir)
}

// writePrivateFile replaces a file atomically with owner-only permissions. The
// caller has already decided the path is safe; this only guarantees a reader
// never sees a half-written document.
func writePrivateFile(path string, content []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	temporary := file.Name()
	cleanup := true
	defer func() {
		if cleanup {
			_ = os.Remove(temporary)
		}
	}()
	if err := file.Chmod(mode); err != nil {
		_ = file.Close()
		return err
	}
	if _, err := file.Write(content); err != nil {
		_ = file.Close()
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporary, path); err != nil {
		return err
	}
	cleanup = false
	return nil
}

// removeFile deletes a file if it exists, and reports whether it did.
func removeFile(path string) bool {
	if err := os.Remove(path); err != nil {
		return false
	}
	return true
}
