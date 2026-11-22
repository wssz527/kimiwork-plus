package bridge

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/platform"
)

var openServiceManager = platform.OpenManager
var openServiceController = platform.OpenController

func (s *Service) serviceOptions() (platform.ManagerOptions, error) {
	executable, err := os.Executable()
	if err != nil {
		return platform.ManagerOptions{}, err
	}
	binary := filepath.Join(filepath.Dir(executable), "gateway-daemon")
	if runtime.GOOS == "windows" {
		binary += ".exe"
	}
	options := platform.ManagerOptions{Binary: binary, StateDir: s.StateDir, BuildID: BuildID}
	if runtime.GOOS == "windows" && options.LogPath == "" && options.StateDir != "" {
		options.LogPath = filepath.Join(options.StateDir, "login-daemon.log")
	}
	return options, nil
}

func (s *Service) serviceManager() (platform.Manager, error) {
	options, err := s.serviceOptions()
	if err != nil {
		return nil, err
	}
	return openServiceManager(options)
}

func (s *Service) serviceController() (platform.Controller, error) {
	options, err := s.serviceOptions()
	if err != nil {
		return nil, err
	}
	return openServiceController(platform.ControllerOptions{ManagerOptions: options, HelperPath: options.Binary})
}

func (s *Service) serviceStatus() core.Document {
	state := core.Document{"mode": "release", "state": "running", "buildId": BuildID,
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
	if controller, controllerErr := s.serviceController(); controllerErr == nil {
		if operation, ok, activeErr := controller.Active(ctx); activeErr == nil && ok {
			state["operation"] = operation
		}
	}
	return state
}

func (s *Service) lifecycleAction(ctx context.Context, in core.Document) (any, error) {
	controller, err := s.serviceController()
	if err != nil {
		return nil, err
	}
	action := core.String(in["action"])
	if action == "status-service" {
		state, err := controller.Status(ctx)
		return core.Document{"service": state}, err
	}
	if action == "read-service-operation" {
		operation, err := controller.Read(ctx, core.String(in["operationId"]))
		if err != nil {
			return nil, err
		}
		result := core.Document{"operation": operation, "operationId": operation.ID}
		if operation.State != nil {
			result["service"] = *operation.State
		}
		return result, nil
	}
	spec, ok := platform.OperationForAction(action)
	if !ok {
		return nil, errors.New("invalid_service_action")
	}
	operation, err := controller.Begin(ctx, spec)
	if err != nil && operation.ID == "" {
		return nil, err
	}
	result := core.Document{"operation": operation, "operationId": operation.ID}
	if operation.State != nil {
		result["service"] = *operation.State
	}
	return result, nil
}
