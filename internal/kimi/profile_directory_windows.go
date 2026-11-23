//go:build windows

package kimi

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

const windowsProfileShareAll = syscall.FILE_SHARE_READ | syscall.FILE_SHARE_WRITE | syscall.FILE_SHARE_DELETE

func validateProfileDirectoryPlatform(path string, info os.FileInfo) error {
	attrs, ok := info.Sys().(*syscall.Win32FileAttributeData)
	if !ok || attrs.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 || attrs.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return errors.New("existing_login_profile_invalid")
	}
	return validateWindowsDirectoryJunction(path)
}

func validateWindowsDirectoryJunction(path string) error {
	pathPtr, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return errors.New("existing_login_profile_invalid")
	}
	raw, err := readWindowsDirectoryReparseData(pathPtr)
	if err != nil {
		return errors.New("existing_login_profile_invalid")
	}
	target, ok := parseWindowsJunctionTarget(raw)
	if !ok {
		return errors.New("existing_login_profile_invalid")
	}

	targetInfo, err := os.Lstat(filepath.Clean(target))
	if err != nil || !targetInfo.IsDir() || targetInfo.Mode()&os.ModeSymlink != 0 {
		return errors.New("existing_login_profile_invalid")
	}

	// Open through the junction with GENERIC_READ so ACL-denied targets are rejected.
	resolvedHandle, err := syscall.CreateFile(
		pathPtr,
		syscall.GENERIC_READ,
		windowsProfileShareAll,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		return errors.New("existing_login_profile_invalid")
	}
	defer syscall.CloseHandle(resolvedHandle)

	var resolvedInfo syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(resolvedHandle, &resolvedInfo); err != nil {
		return errors.New("existing_login_profile_invalid")
	}
	if resolvedInfo.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 {
		return errors.New("existing_login_profile_invalid")
	}
	return nil
}

func readWindowsDirectoryReparseData(pathPtr *uint16) ([]byte, error) {
	handle, err := syscall.CreateFile(
		pathPtr,
		0,
		windowsProfileShareAll,
		nil,
		syscall.OPEN_EXISTING,
		syscall.FILE_FLAG_BACKUP_SEMANTICS|syscall.FILE_FLAG_OPEN_REPARSE_POINT,
		0,
	)
	if err != nil {
		return nil, err
	}
	defer syscall.CloseHandle(handle)

	var info syscall.ByHandleFileInformation
	if err := syscall.GetFileInformationByHandle(handle, &info); err != nil {
		return nil, err
	}
	if info.FileAttributes&syscall.FILE_ATTRIBUTE_DIRECTORY == 0 ||
		info.FileAttributes&syscall.FILE_ATTRIBUTE_REPARSE_POINT == 0 {
		return nil, errors.New("not_directory_reparse_point")
	}

	buf := make([]byte, syscall.MAXIMUM_REPARSE_DATA_BUFFER_SIZE)
	var returned uint32
	if err := syscall.DeviceIoControl(
		handle,
		syscall.FSCTL_GET_REPARSE_POINT,
		nil,
		0,
		&buf[0],
		uint32(len(buf)),
		&returned,
		nil,
	); err != nil {
		return nil, err
	}
	return buf[:returned], nil
}
