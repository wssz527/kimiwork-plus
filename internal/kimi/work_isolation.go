package kimi

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/platform"
)

var errWorkStorageConflict = errors.New("work_storage_restore_conflict")

type workStorageJournal struct {
	Version      int    `json:"version"`
	ProfileID    string `json:"profileId"`
	Before       []byte `json:"before,omitempty"`
	BeforeExists bool   `json:"beforeExists"`
	BeforeMode   uint32 `json:"beforeMode"`
	Active       []byte `json:"active"`
}

type workIsolation struct {
	mu      sync.Mutex
	profile string
	lock    *platform.Lock
	journal string
	closed  bool
}

func beginWorkIsolation(profile, isolatedProfile, share string) (*workIsolation, error) {
	identity, err := existingProfileIdentity(profile)
	if err != nil {
		return nil, err
	}
	realProfile, err := resolveAllowMissing(isolatedProfile)
	if err != nil {
		return nil, err
	}
	if _, err := ensureWithin(realProfile, share); err != nil {
		return nil, err
	}
	if !storagePathsDisjoint(profile, share) {
		return nil, errors.New("work_storage_target_conflict")
	}
	if _, err := platform.EnsurePrivateDir(share); err != nil {
		return nil, err
	}
	lock, err := platform.AcquireLock(filepath.Join(profile, ".kimi-work-plus-isolation"))
	if err != nil {
		return nil, errors.New("work_storage_isolation_busy")
	}
	lease := &workIsolation{profile: profile, lock: lock, journal: filepath.Join(profile, ".kimi-work-plus-isolation", "storage.json")}
	fail := func(err error) (*workIsolation, error) {
		_ = lock.Release()
		return nil, err
	}
	if err := lease.recover(); err != nil {
		return fail(err)
	}
	before, mode, exists, err := readStoragePointer(filepath.Join(profile, storageFileName))
	if err != nil {
		return fail(err)
	}
	if exists {
		var previous struct {
			Share string `json:"shareDir"`
		}
		_ = json.Unmarshal(before, &previous)
		if !storagePathsDisjoint(previous.Share, share) {
			return fail(errors.New("work_storage_target_conflict"))
		}
	}
	active, _ := json.Marshal(struct {
		Version int    `json:"version"`
		Share   string `json:"shareDir"`
	}{1, share})
	active = append(active, '\n')
	record := workStorageJournal{Version: 1, ProfileID: identity, Before: before, BeforeExists: exists, BeforeMode: uint32(mode), Active: active}
	if err := core.AtomicJSON(lease.journal, record); err != nil {
		return fail(errors.New("work_storage_journal_unavailable"))
	}
	current, _, present, err := readStoragePointer(filepath.Join(profile, storageFileName))
	if err != nil || present != exists || !bytes.Equal(current, before) {
		return fail(errWorkStorageConflict)
	}
	if err := atomicStoragePointer(filepath.Join(profile, storageFileName), active, 0o600); err != nil {
		return fail(errors.New("work_storage_pointer_unavailable"))
	}
	return lease, nil
}

func (w *workIsolation) Restore() error {
	if w == nil {
		return nil
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.closed {
		return nil
	}
	if _, err := os.Lstat(w.journal); err != nil {
		return errWorkStorageConflict
	}
	if err := w.recover(); err != nil {
		return err
	}
	w.closed = true
	return w.lock.Release()
}

func (w *workIsolation) recover() error {
	raw, err := core.ReadPrivateFile(w.journal, 256*1024)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return errors.New("work_storage_journal_unavailable")
	}
	var record workStorageJournal
	if json.Unmarshal(raw, &record) != nil || record.Version != 1 || record.ProfileID == "" || !validStoragePointer(record.Active) || (record.BeforeExists && !validStoragePointer(record.Before)) {
		return errors.New("work_storage_journal_invalid")
	}
	identity, err := existingProfileIdentity(w.profile)
	if err != nil || identity != record.ProfileID {
		return errWorkStorageConflict
	}
	pointer := filepath.Join(w.profile, storageFileName)
	current, _, exists, err := readStoragePointer(pointer)
	if err != nil {
		return errWorkStorageConflict
	}
	if exists == record.BeforeExists && bytes.Equal(current, record.Before) {
		return os.Remove(w.journal)
	}
	if !exists || !bytes.Equal(current, record.Active) {
		return errWorkStorageConflict
	}
	if record.BeforeExists {
		err = atomicStoragePointer(pointer, record.Before, os.FileMode(record.BeforeMode)&0o777)
	} else {
		err = os.Remove(pointer)
	}
	if err != nil {
		return errors.New("work_storage_restore_failed")
	}
	return os.Remove(w.journal)
}

func validStoragePointer(raw []byte) bool {
	var value map[string]json.RawMessage
	if json.Unmarshal(raw, &value) != nil || len(value) != 2 {
		return false
	}
	var version int
	var share string
	return json.Unmarshal(value["version"], &version) == nil && version == 1 && json.Unmarshal(value["shareDir"], &share) == nil && filepath.IsAbs(share)
}

func readStoragePointer(path string) ([]byte, os.FileMode, bool, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil, 0, false, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxStorageBytes {
		return nil, 0, false, errors.New("work_storage_pointer_invalid")
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, 0, false, err
	}
	defer file.Close()
	opened, err := file.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, 0, false, errWorkStorageConflict
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxStorageBytes+1))
	if err != nil || len(raw) > maxStorageBytes || !validStoragePointer(raw) {
		return nil, 0, false, errors.New("work_storage_pointer_invalid")
	}
	return raw, info.Mode().Perm(), true, nil
}

func atomicStoragePointer(path string, raw []byte, mode os.FileMode) error {
	file, err := os.CreateTemp(filepath.Dir(path), ".work-storage-*")
	if err != nil {
		return err
	}
	defer os.Remove(file.Name())
	if err = file.Chmod(mode); err == nil {
		_, err = file.Write(raw)
	}
	if err == nil {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(file.Name(), path)
}

func storagePathsDisjoint(first, second string) bool {
	a, errA := resolveAllowMissing(first)
	b, errB := resolveAllowMissing(second)
	if errA != nil || errB != nil {
		return false
	}
	return !pathWithinDir(b, a) && !pathWithinDir(a, b)
}
