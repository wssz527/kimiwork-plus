package kimi

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

type importGateActions struct {
	fakeActions
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (a *importGateActions) ImportModels(ctx context.Context, request ImportRequest) (ImportResult, error) {
	a.fakeActions.mu.Lock()
	a.fakeActions.imports = append(a.fakeActions.imports, request)
	a.fakeActions.mu.Unlock()
	a.once.Do(func() { close(a.started) })
	select {
	case <-a.release:
		return a.fakeActions.imported, nil
	case <-ctx.Done():
		return ImportResult{}, ctx.Err()
	}
}

func TestExplicitImportWaitsForBackgroundRepair(t *testing.T) {
	profile, _ := nativeProfile(t, "user-1")
	controller, _ := controllerFor(t, profile, nil)
	actions := &importGateActions{fakeActions: fakeActions{connected: true,
		imported: ImportResult{OK: true, State: ImportStateAccepted}}, started: make(chan struct{}), release: make(chan struct{})}
	bridge, err := StartBridgeHost(profile, 1, actions)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = bridge.Close() })
	controller.mu.Lock()
	controller.bridge = bridge
	controller.mu.Unlock()

	go func() {
		_, _ = controller.importModelsAuthorized(context.Background(), "", "repair", "")
	}()
	select {
	case <-actions.started:
	case <-time.After(2 * time.Second):
		t.Fatal("background import did not start")
	}
	finished := make(chan error, 1)
	go func() {
		_, err := controller.ImportModelsAuthorized(context.Background())
		finished <- err
	}()
	select {
	case <-finished:
		t.Fatal("explicit import did not wait for the background repair")
	case <-time.After(importBusyPoll * 2):
	}
	close(actions.release)
	if err := <-finished; err != nil {
		t.Fatal(err)
	}
	if len(actions.fakeActions.imports) != 2 {
		t.Fatalf("imports = %d, want 2", len(actions.fakeActions.imports))
	}
}

func TestDirectImportBusyWaitIsBounded(t *testing.T) {
	controller := &Controller{importGranted: true}
	started := time.Now()
	result, err := controller.ImportDirectAuthorized(context.Background(), DirectImportRequest{})
	if !errors.Is(err, ErrImportBusy) || result.State != ImportStateFailed || result.Reason != ErrImportBusy.Error() {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if elapsed := time.Since(started); elapsed < importBusyWait || elapsed > importBusyWait+time.Second {
		t.Fatalf("busy wait = %s, want about %s", elapsed, importBusyWait)
	}
}

func TestExplicitImportBusyWaitIsBounded(t *testing.T) {
	profile, _ := nativeProfile(t, "user-1")
	controller, actions := controllerFor(t, profile, nil)
	controller.mu.Lock()
	controller.importGranted = true
	controller.mu.Unlock()
	started := time.Now()
	result, err := controller.ImportModelsAuthorized(context.Background())
	if !errors.Is(err, ErrImportBusy) || result.State != ImportStateFailed {
		t.Fatalf("result = %+v, err = %v", result, err)
	}
	if elapsed := time.Since(started); elapsed < importBusyWait || elapsed > importBusyWait+time.Second {
		t.Fatalf("busy wait = %s, want about %s", elapsed, importBusyWait)
	}
	if len(actions.imports) != 0 {
		t.Fatal("a busy controller reached the official import")
	}
}
