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
func (k *CachedKeys) Set(name, value string) error {
	k.mu.Lock()
	defer k.mu.Unlock()
	if e := k.store.Set(name, value); e != nil {
		return e
	}
	delete(k.values, name)
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
