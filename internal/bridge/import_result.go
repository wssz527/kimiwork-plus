package bridge

import (
	"errors"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func importResultError(result kimi.ImportResult) error {
	if result.Applied {
		return nil
	}
	if result.State == kimi.ImportStateWaitingLogin || result.State == kimi.ImportStateRequiresRestart || result.State == kimi.ImportStateAccepted {
		return nil
	}
	if result.Reason != "" {
		return errors.New(result.Reason)
	}
	return errors.New("native_import_not_verified")
}
