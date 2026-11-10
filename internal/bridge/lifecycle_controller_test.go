package bridge

import (
	"context"
	"testing"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/platform"
)

type lifecycleControllerFake struct {
	platform.Controller
	spec      platform.OperationSpec
	operation platform.ServiceOperation
	readID    string
}

func (f *lifecycleControllerFake) Begin(_ context.Context, spec platform.OperationSpec) (platform.ServiceOperation, error) {
	f.spec = spec
	return f.operation, nil
}

func (f *lifecycleControllerFake) Read(_ context.Context, id string) (platform.ServiceOperation, error) {
	f.readID = id
	return f.operation, nil
}

func TestLifecycleReturnsOperationAndReadback(t *testing.T) {
	previous := openServiceController
	fake := &lifecycleControllerFake{}
	openServiceController = func(platform.ControllerOptions) (platform.Controller, error) { return fake, nil }
	t.Cleanup(func() { openServiceController = previous })
	service := &Service{StateDir: t.TempDir()}

	fake.operation = platform.ServiceOperation{ID: "0123456789abcdef0123456789abcdef", Action: platform.OperationLoginStart,
		Phase: platform.PhaseSucceeded, State: &platform.ManagerState{Registered: true, Enabled: true, Running: true}}
	result, err := service.lifecycleAction(context.Background(), core.Document{"action": "enable-login-service"})
	if err != nil {
		t.Fatal(err)
	}
	if fake.spec.Action != platform.OperationLoginStart || !fake.spec.LoginStart {
		t.Fatalf("wrong login-start request: %+v", fake.spec)
	}
	if result.(core.Document)["operation"].(platform.ServiceOperation).Phase != platform.PhaseSucceeded {
		t.Fatalf("missing terminal result: %+v", result)
	}

	fake.operation = platform.ServiceOperation{ID: "1123456789abcdef0123456789abcdef", Action: platform.OperationStopService, Phase: platform.PhaseRunning}
	result, err = service.lifecycleAction(context.Background(), core.Document{"action": "stop-service"})
	if err != nil {
		t.Fatal(err)
	}
	if result.(core.Document)["operationId"] != fake.operation.ID || fake.spec.Action != platform.OperationStopService {
		t.Fatalf("stop was not returned as a pending operation: %+v", result)
	}
	fake.operation.Phase = platform.PhaseSucceeded
	fake.operation.Drained = true
	fake.operation.Termination = platform.TerminationDrain
	result, err = service.lifecycleAction(context.Background(), core.Document{"action": "read-service-operation", "operationId": fake.operation.ID})
	if err != nil {
		t.Fatal(err)
	}
	operation := result.(core.Document)["operation"].(platform.ServiceOperation)
	if fake.readID != fake.operation.ID || !operation.Drained || operation.Termination != platform.TerminationDrain {
		t.Fatalf("wrong operation readback: %+v", operation)
	}
}

func TestLifecycleRetainsFailedOperation(t *testing.T) {
	previous := openServiceController
	fake := &lifecycleControllerFake{operation: platform.ServiceOperation{ID: "2123456789abcdef0123456789abcdef", Phase: platform.PhaseFailed, Reason: "daemon_drain_timeout"}}
	openServiceController = func(platform.ControllerOptions) (platform.Controller, error) { return fake, nil }
	t.Cleanup(func() { openServiceController = previous })
	result, err := (&Service{StateDir: t.TempDir()}).lifecycleAction(context.Background(), core.Document{"action": "stop-service"})
	if err != nil {
		t.Fatal(err)
	}
	if result.(core.Document)["operation"].(platform.ServiceOperation).Reason != "daemon_drain_timeout" {
		t.Fatalf("operation failure was lost: %+v", result)
	}
}
