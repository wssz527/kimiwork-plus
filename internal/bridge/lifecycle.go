package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/platform"
)

var openServiceManager = platform.OpenManager

func (s *Service) serviceManager() (platform.Manager, error) {
	executable, err := os.Executable()
	if err != nil {
		return nil, err
	}
	binary := filepath.Join(filepath.Dir(executable), "gateway-daemon")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	return openServiceManager(platform.ManagerOptions{Binary: binary, StateDir: s.StateDir, BuildID: BuildID})
}

func (s *Service) serviceStatus() core.Document {
	state := core.Document{"mode": "preview", "state": "running", "buildId": BuildID,
		"windowIndependent": true, "loginStartup": "manual"}
	if s.StateDir == "" {
		return state
	}
	manager, err := s.serviceManager()
	if err != nil {
		state["registrationState"] = "unavailable"
		state["registrationReason"] = core.ErrorCode(err)
		return state
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	registered, err := manager.Status(ctx)
	state["registered"] = registered.Registered
	state["loginStartup"] = "manual"
	if registered.Enabled {
		state["loginStartup"] = "enabled"
	}
	state["serviceKind"] = registered.Kind
	state["registrationState"] = "ready"
	if registered.Stale {
		state["registrationState"] = "stale"
	}
	if err != nil {
		state["registrationState"] = "unavailable"
		state["registrationReason"] = core.ErrorCode(err)
	}
	return state
}

func (s *Service) lifecycleAction(ctx context.Context, in core.Document) (any, error) {
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
