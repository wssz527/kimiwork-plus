package platform

import (
	"errors"
	"sync"
)

type migratingCredentialStore struct {
	mu      sync.Mutex
	current Credentials
	legacy  Credentials
}

func (m *migratingCredentialStore) Get(key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, err := m.current.Get(key)
	if !errors.Is(err, ErrCredentialMissing) {
		return value, err
	}
	value, err = m.legacy.Get(key)
	return m.migrate(key, value, err)
}

func (m *migratingCredentialStore) GetVisible(key string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	value, err := m.current.Get(key)
	if !errors.Is(err, ErrCredentialMissing) {
		return value, err
	}
	legacy, ok := m.legacy.(interface {
		GetVisible(string) (string, error)
	})
	if ok {
		value, err = legacy.GetVisible(key)
	} else {
		value, err = m.legacy.Get(key)
	}
	return m.migrate(key, value, err)
}

func (m *migratingCredentialStore) migrate(key, value string, err error) (string, error) {
	if err != nil {
		return "", err
	}
	if err := m.saveCurrent(key, value); err != nil {
		return "", err
	}
	return value, nil
}

func (m *migratingCredentialStore) Set(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveCurrent(key, value)
}

func (m *migratingCredentialStore) SetVisible(key, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.saveCurrent(key, value)
}

func (m *migratingCredentialStore) SessionVisible() bool { return true }

func (m *migratingCredentialStore) saveCurrent(key, value string) error {
	if err := m.current.Set(key, value); err != nil {
		return err
	}
	actual, err := m.current.Get(key)
	if err != nil {
		_ = m.current.Delete(key)
		return err
	}
	if actual != value {
		_ = m.current.Delete(key)
		return ErrCredentialReadbackMismatch
	}
	// A legacy ACL failure must not make a verified local credential unusable.
	_ = m.legacy.Delete(key)
	return nil
}

func (m *migratingCredentialStore) Delete(key string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := m.current.Delete(key); err != nil {
		return err
	}
	_ = m.legacy.Delete(key)
	return nil
}
