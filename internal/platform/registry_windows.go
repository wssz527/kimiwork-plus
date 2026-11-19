//go:build windows

package platform

import (
	"context"
	"errors"
	"strings"
	"syscall"
	"unsafe"
)

var (
	advapi32Registry    = syscall.NewLazyDLL("advapi32.dll")
	procRegCreateKeyExW = advapi32Registry.NewProc("RegCreateKeyExW")
	procRegSetValueExW  = advapi32Registry.NewProc("RegSetValueExW")
	procRegDeleteValueW = advapi32Registry.NewProc("RegDeleteValueW")
)

func runPlatformRegistry(ctx context.Context, name string, args []string) (bool, string, error) {
	if !strings.EqualFold(name, registryProgram) {
		return false, "", nil
	}
	command, ok := parseWindowsRegistryCommand(args)
	if !ok {
		// A form this does not implement falls back to reg.exe with its console
		// suppressed, so adding a registry step later cannot silently fail here.
		return false, "", nil
	}
	if err := ctx.Err(); err != nil {
		return true, "", err
	}
	out, err := runWindowsRegistryCommand(command)
	return true, out, err
}

// executeWindowsRegistry implements exactly the argument forms service.go plans:
// add, delete and query under HKCU with /v, /t REG_SZ, /d and /f. Anything else
// is refused rather than guessed at.
func executeWindowsRegistry(args []string) (string, error) {
	command, ok := parseWindowsRegistryCommand(args)
	if !ok {
		return "", errors.New("registry_arguments_invalid")
	}
	return runWindowsRegistryCommand(command)
}

func runWindowsRegistryCommand(command registryCommand) (string, error) {
	switch command.verb {
	case "query":
		return queryHKCUString(command.subkey, command.valueName)
	case "add":
		return "", setHKCUString(command.subkey, command.valueName, command.data)
	case "delete":
		return "", deleteHKCUValue(command.subkey, command.valueName)
	}
	return "", errors.New("registry_arguments_invalid")
}

func queryHKCUString(subkey, valueName string) (string, error) {
	subkeyPtr, err := syscall.UTF16PtrFromString(subkey)
	if err != nil {
		return "", err
	}
	valuePtr, err := syscall.UTF16PtrFromString(valueName)
	if err != nil {
		return "", err
	}
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, subkeyPtr, 0, syscall.KEY_QUERY_VALUE, &key); err != nil {
		return "", err
	}
	defer syscall.RegCloseKey(key)

	var valType uint32
	var bytesNeeded uint32
	err = syscall.RegQueryValueEx(key, valuePtr, nil, &valType, nil, &bytesNeeded)
	if err != nil && !errors.Is(err, syscall.ERROR_MORE_DATA) {
		return "", err
	}
	typeName := registryTypeName(valType)
	if typeName == "" {
		return "", errors.New("registry_type_mismatch")
	}
	// The type is reported as stored. reg.exe would print REG_EXPAND_SZ, and
	// parseRegistryRunValue only accepts REG_SZ, so a hand-written expandable
	// value stays "not enabled" here exactly as it did through reg.exe.
	prefix := "\r\nHKEY_CURRENT_USER\\" + subkey + "\r\n    " + valueName + "    " + typeName + "    "
	if bytesNeeded == 0 {
		return prefix + "\r\n", nil
	}
	words := make([]uint16, (bytesNeeded+1)/2+1)
	size := uint32(len(words) * 2)
	if err := syscall.RegQueryValueEx(key, valuePtr, nil, &valType, (*byte)(unsafe.Pointer(&words[0])), &size); err != nil {
		return "", err
	}
	return prefix + syscall.UTF16ToString(words[:size/2]) + "\r\n", nil
}

func setHKCUString(subkey, valueName, data string) error {
	subkeyPtr, err := syscall.UTF16PtrFromString(subkey)
	if err != nil {
		return err
	}
	valuePtr, err := syscall.UTF16PtrFromString(valueName)
	if err != nil {
		return err
	}
	encoded, err := syscall.UTF16FromString(data)
	if err != nil {
		return err
	}
	var key syscall.Handle
	var disposition uint32
	ret, _, _ := procRegCreateKeyExW.Call(
		uintptr(syscall.HKEY_CURRENT_USER),
		uintptr(unsafe.Pointer(subkeyPtr)),
		0,
		0,
		0,
		uintptr(syscall.KEY_SET_VALUE),
		0,
		uintptr(unsafe.Pointer(&key)),
		uintptr(unsafe.Pointer(&disposition)),
	)
	if ret != 0 {
		return syscall.Errno(ret)
	}
	defer syscall.RegCloseKey(key)

	byteLen := uint32(len(encoded) * 2)
	ret, _, _ = procRegSetValueExW.Call(
		uintptr(key),
		uintptr(unsafe.Pointer(valuePtr)),
		0,
		uintptr(syscall.REG_SZ),
		uintptr(unsafe.Pointer(&encoded[0])),
		uintptr(byteLen),
	)
	if ret != 0 {
		return syscall.Errno(ret)
	}
	return nil
}

func deleteHKCUValue(subkey, valueName string) error {
	subkeyPtr, err := syscall.UTF16PtrFromString(subkey)
	if err != nil {
		return err
	}
	valuePtr, err := syscall.UTF16PtrFromString(valueName)
	if err != nil {
		return err
	}
	var key syscall.Handle
	if err := syscall.RegOpenKeyEx(syscall.HKEY_CURRENT_USER, subkeyPtr, 0, syscall.KEY_SET_VALUE, &key); err != nil {
		return err
	}
	defer syscall.RegCloseKey(key)

	ret, _, _ := procRegDeleteValueW.Call(uintptr(key), uintptr(unsafe.Pointer(valuePtr)))
	if ret != 0 {
		return syscall.Errno(ret)
	}
	return nil
}
