package kimi

import (
	"bufio"
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// fakeCDPFlag re-enters this test binary as a stand-in Kimi child. It exists so
// the launch path can be exercised with real descriptors: the fake reads
// commands from fd3 and writes responses to fd4, exactly as Chromium does with
// -remote-debugging-pipe.
const fakeCDPFlag = "--fake-cdp-child"

// TestMain lets the test binary act as the contract worker or as a stand-in Kimi
// child. RunJob invokes os.Executable, so the child is this same binary.
func TestMain(m *testing.M) {
	// The flags are scanned rather than read from position 1: the managed child
	// receives -remote-debugging-pipe before anything else, exactly as the real
	// launch does.
	for _, argument := range os.Args[1:] {
		switch argument {
		case ContractWorkerFlag:
			os.Exit(RunContractWorker(os.Stdin, os.Stdout))
		case fakeCDPFlag:
			os.Exit(runFakeCDPChild())
		}
	}
	os.Exit(m.Run())
}

// runFakeCDPChild answers the CDP calls the host makes over the pipe.
//
// It speaks both transports the launcher can use, because a test host is not
// necessarily the host the transport was written for: descriptors 3 and 4 on
// Unix, and the inherited handles named in --remote-debugging-io-pipes on
// Windows. Reading the same switch Chromium reads means a Windows run exercises
// the real handover rather than a convenience path the product does not use.
func runFakeCDPChild() int {
	commandHandle, responseHandle := uintptr(3), uintptr(4)
	for _, argument := range os.Args[1:] {
		const prefix = "--remote-debugging-io-pipes="
		if strings.HasPrefix(argument, prefix) {
			read, write, ok := parseIOPipes(strings.TrimPrefix(argument, prefix))
			if !ok {
				return 2
			}
			commandHandle, responseHandle = read, write
		}
	}
	commands := os.NewFile(commandHandle, "commands")
	responses := os.NewFile(responseHandle, "responses")
	if commands == nil || responses == nil {
		return 2
	}
	reader := bufio.NewReaderSize(commands, 64<<10)
	write := func(payload any) {
		encoded, _ := json.Marshal(payload)
		_, _ = responses.Write(append(encoded, 0))
	}
	contextEmitted := map[string]bool{}
	for {
		frame, err := reader.ReadString(0)
		if len(frame) > 0 && frame[len(frame)-1] == 0 {
			frame = frame[:len(frame)-1]
		}
		if frame != "" {
			var request struct {
				ID        int            `json:"id"`
				Method    string         `json:"method"`
				Params    map[string]any `json:"params"`
				SessionID string         `json:"sessionId"`
			}
			if json.Unmarshal([]byte(frame), &request) == nil {
				result := map[string]any{}
				switch request.Method {
				case "Target.getTargets":
					result["targetInfos"] = []any{
						map[string]any{"targetId": "work-1", "type": "page", "url": "app://localhost/kimi-agent.html"},
						map[string]any{"targetId": "settings", "type": "page", "url": "app://localhost/work-settings.html"},
						map[string]any{"targetId": "remote", "type": "page", "url": "https://www.kimi.com/chat/abc"},
					}
				case "Target.attachToTarget":
					// One session per target, as a real browser does. A single
					// shared session would make two pages indistinguishable.
					targetID, _ := request.Params["targetId"].(string)
					result["sessionId"] = "session-" + targetID
				case "Page.getFrameTree":
					result["frameTree"] = map[string]any{"frame": map[string]any{"id": "main-frame"}}
				case "Runtime.enable":
					// Each session reports its own default world exactly once, as a
					// real browser does. A single shared context id would let one
					// page's binding be answered in another page's world.
					if !contextEmitted[request.SessionID] {
						contextEmitted[request.SessionID] = true
						write(map[string]any{"method": "Runtime.executionContextCreated",
							"sessionId": request.SessionID,
							"params": map[string]any{"context": map[string]any{"id": contextIDFor(request.SessionID),
								"auxData": map[string]any{"isDefault": true, "frameId": "main-frame"}}}})
					}
				case "Page.addScriptToEvaluateOnNewDocument":
					result["identifier"] = "script-1"
				case "Runtime.evaluate":
					expression, _ := request.Params["expression"].(string)
					result["result"] = map[string]any{"type": "object", "value": fakeEvaluate(expression, request.SessionID)}
				}
				sessionID := request.SessionID
				if sessionID == "" {
					targetID, _ := request.Params["targetId"].(string)
					sessionID = "session-" + targetID
				}
				write(map[string]any{"id": request.ID, "result": result, "sessionId": sessionID})
			}
		}
		if err != nil {
			return 0
		}
	}
}

// fakeEvaluate answers the page expressions the CDP actions use. It models the
// preload API surface, not a DOM: the actions call these names and nothing else.
func fakeEvaluate(expression, sessionID string) any {
	switch {
	case expression == importProbeExpression:
		// Only the settings overlay carries the import API.
		return true
	case strings.Contains(expression, "importByokModels"):
		// The settings overlay reports a successful import with a restart owed.
		return map[string]any{"ok": true, "models": float64(3), "requiresRestart": true}
	case strings.Contains(expression, "revealByok"):
		return map[string]any{"available": true}
	case strings.Contains(expression, "closeSettingsPage") || strings.Contains(expression, "closeKimiWorkSettingPage"):
		return map[string]any{"asked": []any{"overlay"}, "failed": []any{}}
	case strings.Contains(expression, "openKimiWorkTab"):
		return map[string]any{"asked": true, "failed": false}
	case strings.Contains(expression, "openSettings"):
		return true
	case strings.Contains(expression, "status()"):
		// The status poll carries no config, so the surface is derived from the
		// session that asked: a real renderer reports the surface it was
		// injected for, and each target has its own session.
		return map[string]any{"version": 1, "state": "active", "surface": surfaceOfSession(sessionID),
			"top": true, "structure": "matched", "blocks": 2, "seenBlocks": 2}
	case strings.Contains(expression, "visibilityState"):
		return map[string]any{"visible": true, "painted": true, "text": 10, "open": false}
	case strings.Contains(expression, "__kwgTranslationGeneration"):
		return map[string]any{"version": 1, "state": "active", "surface": surfaceOf(expression),
			"top": true, "structure": "matched", "blocks": 2, "seenBlocks": 2}
	}
	return map[string]any{"version": 1, "state": "active", "surface": surfaceOf(expression),
		"top": true, "structure": "matched", "blocks": 2, "seenBlocks": 2}
}

// surfaceOf reads the surface the host declared in the injected config, which is
// what a real renderer echoes back in its receipt.
func surfaceOf(expression string) string {
	at := strings.Index(expression, `"surface":"`)
	if at < 0 {
		return "work"
	}
	rest := expression[at+len(`"surface":"`):]
	if end := strings.IndexByte(rest, '"'); end > 0 {
		return rest[:end]
	}
	return "work"
}

// surfaceOfSession maps the fake's session ids back to the surface each page
// belongs to.
func surfaceOfSession(sessionID string) string {
	switch sessionID {
	case "session-work-1":
		return "work"
	case "session-remote":
		return "chat"
	}
	return "work"
}

// contextIDFor gives each fake session a distinct default world.
func contextIDFor(sessionID string) int {
	if sessionID == "session-remote" {
		return 22
	}
	return 11
}
