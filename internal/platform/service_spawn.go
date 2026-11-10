package platform

import (
	"os"
	"os/exec"
	"path/filepath"
)

// spawnHelper starts the lifecycle helper detached from this process.
//
// Detaching matters for one reason: the helper's job is to stop or reload the
// daemon that started it. A child in the same process group or job object would
// be torn down with its parent — exactly the failure this design removes. The
// platform files below set the right flags; this file holds the part both
// platforms share.
//
// The helper's output goes to a per-operation log, never to the shared daemon log
// and never to the terminal. The process is not waited on: its result is the
// journal entry, not an exit status anyone can still read after a reboot.
func spawnHelper(start HelperStart) (int, error) {
	if start.Binary == "" || !filepath.IsAbs(start.Binary) {
		return 0, os.ErrInvalid
	}
	command := exec.Command(start.Binary, append([]string{start.Arg}, start.Args...)...)
	if len(start.Env) > 0 {
		command.Env = start.Env
	}
	command.Stdin = nil
	if start.LogPath != "" {
		log, err := openHelperLog(start.LogPath)
		if err != nil {
			return 0, err
		}
		defer log.Close()
		command.Stdout = log
		command.Stderr = log
	}
	detach(command)
	if err := command.Start(); err != nil {
		return 0, err
	}
	pid := command.Process.Pid
	if err := command.Process.Release(); err != nil {
		return pid, err
	}
	return pid, nil
}

// openHelperLog opens the operation log. The file must be private and regular:
// the helper's output can quote launchctl or schtasks output, which is not a
// secret but is also not something another user should read.
func openHelperLog(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return nil, os.ErrPermission
		}
		if !isWindows() && info.Mode().Perm()&0o077 != 0 {
			return nil, os.ErrPermission
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	log, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, err
	}
	opened, err := log.Stat()
	if err != nil {
		log.Close()
		return nil, err
	}
	current, err := os.Lstat(path)
	if err != nil || !os.SameFile(opened, current) || !current.Mode().IsRegular() {
		log.Close()
		return nil, os.ErrPermission
	}
	return log, nil
}
