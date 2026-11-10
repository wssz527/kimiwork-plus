package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/platform"
)

func (s *Service) serviceManager() (platform.Manager, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	binary := filepath.Join(filepath.Dir(executable), "gateway-daemon")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	return platform.OpenManager(platform.ManagerOptions{Binary: binary, StateDir: s.StateDir, BuildID: BuildID})
}

func (s *Service) lifecycleAction(ctx context.Context, in core.Document) (any, error) {
	if core.String(in["action"]) == "restart-daemon" {
		return core.Document{"operation": core.Document{"action": "restart-daemon", "state": "stopping", "reason": "window_reopens_same_endpoint"}}, nil
	}
	manager, err := s.serviceManager()
	if err != nil {
		return nil, err
	}
	if core.String(in["action"]) == "status-service" {
		state, err := manager.Status(ctx)
		return core.Document{"service": state}, err
	}
	var state platform.ManagerState
	switch core.String(in["action"]) {
	case "register-service":
		state, err = manager.Register(ctx)
	case "enable-login-service":
		state, err = manager.Enable(ctx)
	case "disable-login-service":
		state, err = manager.Disable(ctx)
	case "start-service":
		state, err = manager.Start(ctx)
	case "stop-service":
		state, err = manager.Stop(ctx)
	case "uninstall-service":
		state, err = manager.Uninstall(ctx)
	default:
		return nil, errors.New("invalid_service_action")
	}
	return core.Document{"service": state}, err
}
