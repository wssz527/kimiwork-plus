//go:build windows

package platform

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// The Windows store encrypts each value with DPAPI (CryptProtectData) under the
// current user, and keeps the ciphertext in one file per key inside a private
// directory. DPAPI is the documented per-user protector: the blob cannot be
// decrypted by another account or on another machine, which is the property a
// credential store needs.
//
// This store never decrypts a value the old Electron build produced with
// safeStorage; that format belongs to that build.

var (
	crypt32Lib         = syscall.NewLazyDLL("crypt32.dll")
	procCryptProtect   = crypt32Lib.NewProc("CryptProtectData")
	procCryptUnprotect = crypt32Lib.NewProc("CryptUnprotectData")
	procLocalFree      = kernel32.NewProc("LocalFree")
)

// cryptProtectUIForbidden is CRYPTPROTECT_UI_FORBIDDEN. It is set on both calls
// so a protection operation that would need a dialog fails immediately instead.
// A background daemon cannot show one, and a status call that blocks inside an
// invisible prompt is worse than an error the panel can explain.
const cryptProtectUIForbidden = 0x1

type dataBlob struct {
	size uint32
	data *byte
}

type dpapi struct{ dir string }

// dpapiDirectory is where the ciphertext lives. It is a per-user application
// directory, created with owner-only access.
func dpapiDirectory() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, StateDirName, "secrets"), nil
}

func dpapiDirectoryForService(service string) (string, error) {
	if strings.HasPrefix(service, "dev.kimi-work-plus.preview") {
		base, err := os.UserConfigDir()
		if err != nil {
			return "", err
		}
		return filepath.Join(base, LegacyPreviewStateDirName, "secrets"), nil
	}
	return dpapiDirectory()
}

func newDPAPI(service string) (Credentials, error) {
	dir, err := dpapiDirectoryForService(service)
	if err != nil {
		return nil, err
	}
	dir = filepath.Join(dir, service)
	if _, err := EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	return &dpapi{dir: dir}, nil
}

// path maps a key to a file name. The key is hex-encoded so no key can escape
// the directory or collide with a reserved name.
func (d *dpapi) path(key string) string {
	return filepath.Join(d.dir, hex.EncodeToString([]byte(key))+".bin")
}

func (d *dpapi) Get(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	ciphertext, err := os.ReadFile(d.path(key))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return "", ErrCredentialMissing
		}
		return "", errors.New("credential_store_failed")
	}
	if len(ciphertext) == 0 || len(ciphertext) > maxSecretBytes*2 {
		return "", errors.New("credential_store_failed")
	}
	plain, err := unprotect(ciphertext)
	if err != nil {
		return "", err
	}
	return plain, nil
}

func (d *dpapi) Set(key, value string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := validateValue(value); err != nil {
		return err
	}
	ciphertext, err := protect([]byte(value))
	if err != nil {
		return err
	}
	// Write to a fresh file and rename, so a crash cannot leave a half-written
	// secret that then fails to decrypt.
	temporary := d.path(key) + "." + randomSuffix() + ".tmp"
	if err := os.WriteFile(temporary, ciphertext, 0o600); err != nil {
		return errors.New("credential_store_failed")
	}
	if err := os.Rename(temporary, d.path(key)); err != nil {
		os.Remove(temporary)
		return errors.New("credential_store_failed")
	}
	return nil
}

func (d *dpapi) Delete(key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := os.Remove(d.path(key)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("credential_store_failed")
	}
	return nil
}

func randomSuffix() string {
	buffer := make([]byte, 8)
	if _, err := rand.Read(buffer); err != nil {
		return "tmp"
	}
	return hex.EncodeToString(buffer)
}

// protect wraps CryptProtectData. The description string is empty so the blob
// carries no user-visible label, and CRYPTPROTECT_UI_FORBIDDEN keeps the call
// from ever waiting on a dialog.
func protect(plain []byte) ([]byte, error) {
	var in, out dataBlob
	if len(plain) > 0 {
		in = dataBlob{size: uint32(len(plain)), data: &plain[0]}
	}
	status, _, _ := procCryptProtect.Call(uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)))
	if status == 0 {
		return nil, errors.New("credential_protect_failed")
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	if out.data == nil || out.size == 0 {
		return nil, errors.New("credential_protect_failed")
	}
	return append([]byte(nil), unsafe.Slice(out.data, int(out.size))...), nil
}

// unprotect wraps CryptUnprotectData. A failure is reported as an access error
// rather than an empty value, so a corrupt blob never reads as "no secret".
//
// CRYPTPROTECT_UI_FORBIDDEN matters most here: without it, a blob whose
// protection is not available to this user could make CryptUnprotectData put up a
// dialog and block. With it, the call returns an error immediately, which is what
// lets a background status request answer instead of hanging.
func unprotect(ciphertext []byte) (string, error) {
	if len(ciphertext) == 0 {
		return "", ErrCredentialAccessDenied
	}
	var in, out dataBlob
	in = dataBlob{size: uint32(len(ciphertext)), data: &ciphertext[0]}
	status, _, _ := procCryptUnprotect.Call(uintptr(unsafe.Pointer(&in)), 0, 0, 0, 0, cryptProtectUIForbidden,
		uintptr(unsafe.Pointer(&out)))
	if status == 0 {
		return "", ErrCredentialAccessDenied
	}
	defer procLocalFree.Call(uintptr(unsafe.Pointer(out.data)))
	if out.data == nil || out.size == 0 {
		return "", ErrCredentialAccessDenied
	}
	return string(unsafe.Slice(out.data, int(out.size))), nil
}

var _ = strings.TrimSpace
