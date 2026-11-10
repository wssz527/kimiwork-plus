package core

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

type Document map[string]any

type Store struct {
	mu   sync.Mutex
	path string
}

type journal struct {
	Before string   `json:"before"`
	After  Document `json:"after"`
}

var ErrConflict = errors.New("configuration_conflict")

func Open(dir string) (*Store, error) {
	if err := PrivateDirectory(dir); err != nil {
		return nil, err
	}
	s := &Store{path: filepath.Join(dir, "gateway.json")}
	if err := s.recover(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(s.path); errors.Is(err, os.ErrNotExist) {
		if err := AtomicJSON(s.path, Document{"models": []any{}, "connections": []any{}, "subagents": Document{"mode": "inherit"}}); err != nil {
			return nil, err
		}
	}
	_, _, err := s.Read()
	return s, err
}

func Revision(d Document) string {
	b, _ := json.Marshal(d)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func (s *Store) read() (Document, string, error) {
	b, err := ReadPrivateFile(s.path, 4<<20)
	if err != nil {
		return nil, "", err
	}
	var d Document
	if json.Unmarshal(b, &d) != nil || d == nil {
		return nil, "", errors.New("configuration_unavailable")
	}
	return d, Revision(d), nil
}

func (s *Store) Read() (Document, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.read()
}

func (s *Store) Update(revision string, change func(Document) error) (Document, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	d, rev, err := s.read()
	if err != nil {
		return nil, "", err
	}
	if revision == "" || revision != rev {
		return nil, "", ErrConflict
	}
	if err := change(d); err != nil {
		return nil, "", err
	}
	if err := Validate(d); err != nil {
		return nil, "", err
	}
	if err := AtomicJSON(s.path+".journal", journal{Before: rev, After: d}); err != nil {
		return nil, "", err
	}
	_, latest, err := s.read()
	if err != nil || latest != rev {
		return nil, "", ErrConflict
	}
	if err := AtomicJSON(s.path, d); err != nil {
		return nil, "", err
	}
	actual, newRev, err := s.read()
	if err != nil || newRev != Revision(d) {
		return nil, "", errors.New("configuration_readback_failed")
	}
	if err := os.Remove(s.path + ".journal"); err != nil {
		return nil, "", err
	}
	return actual, newRev, nil
}

func (s *Store) recover() error {
	b, err := ReadPrivateFile(s.path+".journal", 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	var j journal
	if json.Unmarshal(b, &j) != nil || j.After == nil {
		return errors.New("configuration_recovery_invalid")
	}
	_, rev, err := s.read()
	if err != nil {
		return err
	}
	if rev != j.Before && rev != Revision(j.After) {
		return errors.New("configuration_recovery_conflict")
	}
	if rev == j.Before {
		if err := Validate(j.After); err != nil {
			return err
		}
		if err := AtomicJSON(s.path, j.After); err != nil {
			return err
		}
	}
	_, rev, err = s.read()
	if err != nil || rev != Revision(j.After) {
		return errors.New("configuration_readback_failed")
	}
	return os.Remove(s.path + ".journal")
}

func AtomicJSON(path string, value any) error {
	b, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	f, err := os.CreateTemp(filepath.Dir(path), ".transaction-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(b)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(name, path)
}
