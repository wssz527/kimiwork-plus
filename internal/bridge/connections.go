package bridge

import (
	"errors"
	"kimiworkplus.local/migration/internal/core"
)

func credentialFor(c map[string]any, keys KeyStore) (string, error) {
	if keys == nil {
		return "", nil
	}
	ref := core.String(c["apiKeyEnv"])
	if v, ok := c["apiKeyRef"]; ok {
		ref = core.String(v)
	}
	if ref == "" {
		return "", nil
	}
	return keys.Get(ref)
}
func (s *Service) stageCredentials(d core.Document, changes map[string]*string) (err error) {
	if len(changes) > 0 && s.Keys == nil {
		return errors.New("credentials_unavailable")
	}
	created := []string{}
	defer func() {
		if err == nil {
			return
		}
		for _, ref := range created {
			if e := s.Keys.Delete(ref); e != nil {
				err = errors.New("credentials_cleanup_failed")
			}
		}
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
		if e := s.Keys.Set(ref, *value); e != nil {
			return errors.New("credentials_unavailable")
		}
		created = append(created, ref)
		actual, e := s.Keys.Get(ref)
		if e != nil || actual != *value {
			return errors.New("credentials_readback_failed")
		}
		c["apiKeyRef"] = ref
	}
	return nil
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
			row["credentialError"] = "credentials_unavailable"
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
