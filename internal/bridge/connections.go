package bridge

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"

	"github.com/wssz527/kimi-work-plus/internal/core"
)

func credentialReference(c map[string]any) string {
	if ref, ok := c["apiKeyRef"]; ok {
		return core.String(ref)
	}
	return core.String(c["apiKeyEnv"])
}

func credentialFor(c map[string]any, keys KeyStore) (string, error) {
	if keys == nil {
		return "", nil
	}
	ref := credentialReference(c)
	if ref == "" {
		return "", nil
	}
	return keys.Get(ref)
}

func (s *Service) authorizeConnectionCredentials(d core.Document) error {
	for _, value := range core.Rows(d["connections"]) {
		connection := core.Object(value)
		if _, err := credentialFor(connection, s.Keys); err != nil {
			if _, err := getVisibleCredential(s.Keys, credentialReference(connection)); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Service) stageCredentials(d core.Document, changes map[string]*string) (staged []string, err error) {
	if len(changes) > 0 && s.Keys == nil {
		return nil, errors.New("credentials_unavailable")
	}
	created := []string{}
	defer func() {
		if err != nil {
			for _, ref := range created {
				if e := s.Keys.Delete(ref); e != nil {
					err = errors.New("credentials_cleanup_failed")
				}
			}
			staged = nil
			return
		}
		staged = created
	}()
	for _, v := range core.Rows(d["connections"]) {
		c := core.Object(v)
		value, changed := changes[core.String(c["apiKeyEnv"])]
		if !changed {
			continue
		}
		c["apiKeyRef"] = ""
		if value == nil || *value == "" {
			continue
		}
		// Immutable credentials become active only when the config journal commits their reference.
		ref := "connection-" + RandomKey()
		created = append(created, ref)
		if e := stageCredential(s.Keys, ref, *value); e != nil {
			return nil, e
		}
		c["apiKeyRef"] = ref
	}
	return created, nil
}

// discardStagedCredentials removes connection credentials that a failed save
// staged once no durable configuration refers to them. A commit can fail after
// it wrote the recovery journal, and the next start applies that journal, so a
// reference in either durable document keeps the key alive.
func (s *Service) discardStagedCredentials(refs []string) {
	if len(refs) == 0 || s.Keys == nil {
		return
	}
	referenced, known := s.durableCredentialRefs()
	if !known {
		return
	}
	for _, ref := range refs {
		if !referenced[ref] {
			_ = s.Keys.Delete(ref)
		}
	}
}

func (s *Service) durableCredentialRefs() (map[string]bool, bool) {
	refs := map[string]bool{}
	current, _, err := s.Store.Read()
	if err != nil {
		return nil, false
	}
	collectCredentialRefs(current, refs)
	// Without the state directory the recovery journal cannot be inspected, and
	// a pending journal is the one document that can still activate these keys.
	if s.StateDir == "" {
		return nil, false
	}
	raw, err := core.ReadPrivateFile(filepath.Join(s.StateDir, "gateway.json.journal"), 4<<20)
	if errors.Is(err, os.ErrNotExist) {
		return refs, true
	}
	if err != nil {
		return nil, false
	}
	var pending struct {
		After core.Document `json:"after"`
	}
	if json.Unmarshal(raw, &pending) != nil || pending.After == nil {
		return nil, false
	}
	collectCredentialRefs(pending.After, refs)
	return refs, true
}

func collectCredentialRefs(d core.Document, refs map[string]bool) {
	for _, v := range core.Rows(d["connections"]) {
		if ref := core.String(core.Object(v)["apiKeyRef"]); ref != "" {
			refs[ref] = true
		}
	}
	if ref := core.String(core.Object(d["translation"])["apiKeyRef"]); ref != "" {
		refs[ref] = true
	}
}
func connectionRows(d core.Document, keys KeyStore) []any {
	rows := []any{}
	for _, v := range core.Rows(d["connections"]) {
		c := core.Object(v)
		row := core.Document{}
		for k, v := range c {
			if k != "apiKeyRef" {
				row[k] = v
			}
		}
		key, err := credentialFor(c, keys)
		if err != nil {
			row["credentialError"] = core.ErrorCode(err)
			row["credentialRecovery"] = "resave-required"
		}
		count := 0
		for _, m := range core.Rows(d["models"]) {
			if core.Object(m)["connectionId"] == c["id"] {
				count++
			}
		}
		row["modelCount"] = count
		row["legacy"] = false
		row["apiKeySet"] = key != ""
		row["environment"] = core.Document{"apiKeySet": key != ""}
		rows = append(rows, row)
	}
	return rows
}
