package bridge

import (
	"errors"
	"kimiworkplus.local/migration/internal/kimi"
)

func importResultError(result kimi.ImportResult) error {
	if result.OK {
		return nil
	}
	if result.Reason != "" {
		return errors.New(result.Reason)
	}
	return errors.New("native_import_not_verified")
}
