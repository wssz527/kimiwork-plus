package bridge

import (
	"errors"
	"kimiworkplus.local/migration/internal/platform"
	"sync"
)

type CachedKeys struct {
	mu     sync.Mutex
	store  KeyStore
	values map[string]string
}

func CacheKeys(store KeyStore) *CachedKeys {
	return &CachedKeys{store: store, values: map[string]string{}}
}
func (k *CachedKeys) Get(name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if v, ok := k.values[name]; ok {
		return v, nil
	}
	v, e := k.store.Get(name)
	if errors.Is(e, platform.ErrCredentialMissing) {
		e = nil
		v = ""
	}
	if e == nil {
		k.values[name] = v
	}
	return v, e
}
func (k *CachedKeys) GetVisible(name string) (string, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if value, ok := k.values[name]; ok {
		return value, nil
	}
	value, err := getVisibleCredential(k.store, name)
	if err == nil {
		k.values[name] = value
	}
	return value, err
}

func getVisibleCredential(keys KeyStore, name string) (string, error) {
	if store, ok := keys.(interface{ GetVisible(string) (string, error) }); ok {
		return store.GetVisible(name)
	}
	return keys.Get(name)
}

func (k *CachedKeys) Set(name, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if e := k.store.Set(name, value); e != nil {
		return e
	}
	delete(k.values, name)
	return nil
}
func (k *CachedKeys) SetVisible(name, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if err := setVisibleCredential(k.store, name, value); err != nil {
		return err
	}
	delete(k.values, name)
	return nil
}
func (k *CachedKeys) SessionVisible() bool {
	k.mu.Lock()
	defer k.mu.Unlock()
	store, ok := k.store.(interface{ SessionVisible() bool })
	return !ok || store.SessionVisible()
}

type visibleCredentialStore interface {
	SetVisible(string, string) error
	SessionVisible() bool
}

func setVisibleCredential(keys KeyStore, name, value string) error {
	if store, ok := keys.(visibleCredentialStore); ok {
		if !store.SessionVisible() {
			return errors.New("credential_session_unavailable")
		}
		return store.SetVisible(name, value)
	}
	return keys.Set(name, value)
}

func stageCredential(keys KeyStore, name, value string) error {
	if err := setVisibleCredential(keys, name, value); err != nil {
		return err
	}
	actual, err := keys.Get(name)
	if err != nil {
		_ = keys.Delete(name)
		return err
	}
	if actual != value {
		_ = keys.Delete(name)
		return errors.New("credentials_readback_failed")
	}
	return nil
}

func (k *CachedKeys) Delete(name string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if e := k.store.Delete(name); e != nil {
		return e
	}
	k.values[name] = ""
	return nil
}
