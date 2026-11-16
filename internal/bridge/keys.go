package bridge

import (
	"errors"
	"kimiworkplus.local/migration/internal/platform"
	"sync"
)

// errCredentialAuthorizationRequired is what a background read reports while a
// user-visible keychain operation waits for the system dialog. The platform
// serialises every interaction behind one process-wide lock, so a background
// read issued during a dialog could only finish after the user answers it; the
// panel state endpoint must keep answering instead.
var errCredentialAuthorizationRequired = errors.New("credential_authorization_required")

type CachedKeys struct {
	mu   sync.Mutex
	gate sync.RWMutex
	// store is never called while mu is held: it can block on the system
	// dialog, and the cache lock must not be part of that wait.
	store   KeyStore
	values  map[string]string
	changes map[string]uint64
}

func CacheKeys(store KeyStore) *CachedKeys {
	return &CachedKeys{store: store, values: map[string]string{}, changes: map[string]uint64{}}
}
func (k *CachedKeys) Get(name string) (string, error) {
	if v, ok := k.cached(name); ok {
		return v, nil
	}
	// A visible interaction holds the gate for as long as its dialog is open.
	// Failing fast here is what keeps a background reader from queueing behind
	// the user, and it also keeps this process from starting a second,
	// invisible system interaction.
	if !k.gate.TryRLock() {
		return "", errCredentialAuthorizationRequired
	}
	defer k.gate.RUnlock()
	change := k.change(name)
	v, e := k.store.Get(name)
	if errors.Is(e, platform.ErrCredentialMissing) {
		e = nil
		v = ""
	}
	if e != nil {
		return "", e
	}
	k.keep(name, change, v)
	return v, nil
}
func (k *CachedKeys) GetVisible(name string) (string, error) {
	if value, ok := k.cached(name); ok {
		return value, nil
	}
	k.gate.Lock()
	defer k.gate.Unlock()
	// A visible read that was already waiting when the previous one answered
	// must reuse that answer: starting a second system dialog for the same
	// value would ask the user the same question again.
	if value, ok := k.cached(name); ok {
		return value, nil
	}
	change := k.change(name)
	value, err := getVisibleCredential(k.store, name)
	if err == nil {
		k.keep(name, change, value)
	}
	return value, err
}

func (k *CachedKeys) cached(name string) (string, bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	value, ok := k.values[name]
	return value, ok
}

func (k *CachedKeys) change(name string) uint64 {
	k.mu.Lock()
	defer k.mu.Unlock()
	return k.changes[name]
}

// keep stores a value read before the current generation, so a write or delete
// that landed while the read waited for the user wins over the older answer.
func (k *CachedKeys) keep(name string, change uint64, value string) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.changes[name] == change {
		k.values[name] = value
	}
}

// readBack reports the value the store holds now, bypassing the cache and the
// pending-interaction fast path. A write verifying its own readback must see
// what this process just wrote rather than fail because another dialog is open.
func (k *CachedKeys) readBack(name string) (string, error) {
	v, e := k.store.Get(name)
	if errors.Is(e, platform.ErrCredentialMissing) {
		return "", nil
	}
	return v, e
}

func getVisibleCredential(keys KeyStore, name string) (string, error) {
	if store, ok := keys.(interface{ GetVisible(string) (string, error) }); ok {
		return store.GetVisible(name)
	}
	return keys.Get(name)
}

func (k *CachedKeys) Set(name, value string) error {
	if e := k.store.Set(name, value); e != nil {
		return e
	}
	k.invalidate(name, false)
	return nil
}
func (k *CachedKeys) SetVisible(name, value string) error {
	k.gate.Lock()
	defer k.gate.Unlock()
	if err := setVisibleCredential(k.store, name, value); err != nil {
		return err
	}
	k.invalidate(name, false)
	return nil
}

// invalidate bumps the generation and drops the cached value once the store
// accepted the change, so an authorized read answered before this write cannot
// put its stale value back.
func (k *CachedKeys) invalidate(name string, tombstone bool) {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.changes[name]++
	if tombstone {
		k.values[name] = ""
		return
	}
	delete(k.values, name)
}

func (k *CachedKeys) SessionVisible() bool {
	k.mu.Lock()
	store, ok := k.store.(interface{ SessionVisible() bool })
	k.mu.Unlock()
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
	actual, err := readBackCredential(keys, name)
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

func readBackCredential(keys KeyStore, name string) (string, error) {
	if store, ok := keys.(interface{ readBack(string) (string, error) }); ok {
		return store.readBack(name)
	}
	return keys.Get(name)
}

func (k *CachedKeys) Delete(name string) error {
	if e := k.store.Delete(name); e != nil {
		return e
	}
	k.invalidate(name, true)
	return nil
}
