package bridge

import (
	"context"
	"testing"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/platform"
)

type statusOnlyManager struct {
	platform.Manager
	state platform.ManagerState
}

func (m statusOnlyManager) Status(context.Context) (platform.ManagerState, error) {
	return m.state, nil
}

func TestStateReadsServiceRegistration(t *testing.T) {
	previous := openServiceManager
	openServiceManager = func(platform.ManagerOptions) (platform.Manager, error) {
		return statusOnlyManager{state: platform.ManagerState{Registered: true, Enabled: true, Kind: "launchd"}}, nil
	}
	t.Cleanup(func() { openServiceManager = previous })
	store, err := core.Open(t.TempDir() + "/state")
	if err != nil {
		t.Fatal(err)
	}
	service := Service{Store: store, StateDir: t.TempDir()}
	state, err := service.State()
	if err != nil {
		t.Fatal(err)
	}
	actual := core.Object(state["service"])
	if actual["registered"] != true || actual["loginStartup"] != "enabled" || actual["serviceKind"] != "launchd" {
		t.Fatalf("stale service state: %+v", actual)
	}
}
