package platform

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
)

const localMasterKeySize = 32

var errLocalMasterKey = errors.New("credential_master_key_invalid")

type localSecretStore struct {
	dir     string
	service string
}

func newLocalSecretStore(service string) (Credentials, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return nil, err
	}
	dir := filepath.Join(base, StateDirName, "secrets", service)
	if _, err := EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	return &localSecretStore{dir: dir, service: service}, nil
}

func (s *localSecretStore) path(key string) string {
	name := hex.EncodeToString([]byte(key))
	if len(name)+len(".bin.")+32+len(".tmp") > 255 {
		digest := sha256.Sum256([]byte(key))
		name = hex.EncodeToString(digest[:])
	}
	return filepath.Join(s.dir, name+".bin")
}

func (s *localSecretStore) Get(key string) (string, error) {
	if err := validateKey(key); err != nil {
		return "", err
	}
	path := s.path(key)
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrCredentialMissing
	}
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() > maxSecretBytes+28 {
		return "", ErrCredentialAccessDenied
	}
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", ErrCredentialMissing
	}
	if err != nil {
		return "", err
	}
	plain, err := decryptLocalSecret(data, s.service)
	if err != nil {
		return "", err
	}
	return string(plain), nil
}

func (s *localSecretStore) Set(key, value string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	if err := validateValue(value); err != nil {
		return err
	}
	data, err := encryptLocalSecret([]byte(value), s.service)
	if err != nil {
		return err
	}
	return writeLocalSecret(s.path(key), data)
}

func (s *localSecretStore) Delete(key string) error {
	if err := validateKey(key); err != nil {
		return err
	}
	err := os.Remove(s.path(key))
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	return err
}

func encryptLocalSecret(plain []byte, service string) ([]byte, error) {
	key, err := localMasterKey()
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return gcm.Seal(nonce, nonce, plain, []byte(service)), nil
}

func decryptLocalSecret(data []byte, service string) ([]byte, error) {
	path, err := localMasterKeyPath()
	if err != nil {
		return nil, err
	}
	key, err := readLocalMasterKey(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, errLocalMasterKey
	}
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(data) < gcm.NonceSize()+gcm.Overhead() {
		return nil, ErrCredentialAccessDenied
	}
	plain, err := gcm.Open(nil, data[:gcm.NonceSize()], data[gcm.NonceSize():], []byte(service))
	if err != nil {
		return nil, ErrCredentialAccessDenied
	}
	return plain, nil
}

func localMasterKeyPath() (string, error) {
	base, err := os.UserConfigDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, StateDirName, "secrets", "master.key"), nil
}

func localMasterKey() ([]byte, error) {
	path, err := localMasterKeyPath()
	if err != nil {
		return nil, err
	}
	dir := filepath.Dir(path)
	if _, err := EnsurePrivateDir(dir); err != nil {
		return nil, err
	}
	key, err := readLocalMasterKey(path)
	if err == nil {
		return key, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	key = make([]byte, localMasterKeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, err
	}
	suffix, err := randomLocalSuffix()
	if err != nil {
		return nil, err
	}
	temporary := path + "." + suffix + ".tmp"
	if err := writeLocalNewFile(temporary, key); err != nil {
		return nil, err
	}
	defer os.Remove(temporary)
	if err := os.Link(temporary, path); err != nil {
		existing, readErr := readLocalMasterKey(path)
		if readErr == nil {
			return existing, nil
		}
		return nil, err
	}
	return key, nil
}

func readLocalMasterKey(path string) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, errLocalMasterKey
	}
	if info.Mode().Perm() != 0o600 {
		if err := os.Chmod(path, 0o600); err != nil {
			return nil, err
		}
	}
	key, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	if len(key) != localMasterKeySize {
		return nil, errLocalMasterKey
	}
	return key, nil
}

func writeLocalSecret(path string, data []byte) error {
	suffix, err := randomLocalSuffix()
	if err != nil {
		return err
	}
	temporary := path + "." + suffix + ".tmp"
	if err := writeLocalNewFile(temporary, data); err != nil {
		return err
	}
	defer os.Remove(temporary)
	return os.Rename(temporary, path)
}

func writeLocalNewFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.Write(data); err != nil {
		file.Close()
		os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

func randomLocalSuffix() (string, error) {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		return "", err
	}
	return hex.EncodeToString(buffer), nil
}
