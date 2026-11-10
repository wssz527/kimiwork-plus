package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/kimi"
	"kimiworkplus.local/migration/internal/webtools"
)

type webService = webtools.Service

type webKey struct {
	keys        KeyStore
	ref         string
	previousRef string
}

func (k *webKey) Get() (string, error) {
	if k.keys == nil || k.ref == "" {
		return "", nil
	}
	return k.keys.Get(k.ref)
}

func (k *webKey) Set(value string) error {
	if k.keys == nil {
		return errors.New("credentials_unavailable")
	}
	ref := "tavily-" + RandomKey()
	if err := k.keys.Set(ref, value); err != nil {
		return err
	}
	actual, err := k.keys.Get(ref)
	if err != nil || actual != value {
		_ = k.keys.Delete(ref)
		return errors.New("credentials_readback_failed")
	}
	k.previousRef = k.ref
	k.ref = ref
	return nil
}

func (k *webKey) rollback() {
	if k.previousRef == "" && k.ref == "" {
		return
	}
	if k.previousRef != k.ref {
		_ = k.keys.Delete(k.ref)
	}
	k.ref = k.previousRef
	k.previousRef = ""
}

type webCredentialSettings struct {
	path       string
	credential *webKey
}

func (s webCredentialSettings) Read() ([]byte, error) {
	return core.ReadPrivateFile(s.path, 4096)
}
func (s webCredentialSettings) Write(body []byte) error {
	var document webtools.Settings
	if err := json.Unmarshal(body, &document); err != nil {
		return errors.New("invalid_web_settings")
	}
	value := map[string]any{"schemaVersion": document.SchemaVersion, "provider": document.Provider}
	ref := currentWebCredentialRef(s.path)
	if s.credential != nil && s.credential.ref != "" {
		ref = s.credential.ref
	}
	if ref != "" {
		value["credentialRef"] = ref
	}
	return core.AtomicJSON(s.path, value)
}

func currentWebCredentialRef(path string) string {
	body, err := core.ReadPrivateFile(path, 4096)
	if err != nil {
		return ""
	}
	var value struct {
		Ref string `json:"credentialRef"`
	}
	if json.Unmarshal(body, &value) != nil || len(value.Ref) != len("tavily-")+64 || value.Ref[:7] != "tavily-" {
		return ""
	}
	for _, char := range value.Ref[7:] {
		switch {
		case char >= 'a' && char <= 'f', char >= '0' && char <= '9':
		default:
			return ""
		}
	}
	return value.Ref
}

func (n *Native) webServices(block map[string]any) (kimi.ServicesInput, error) {
	var input kimi.ServicesInput
	if err := decodeStrict(block, &input); err != nil {
		return input, errors.New("invalid_services")
	}
	return input, nil
}

func (n *Native) saveWebServices(block map[string]any) error {
	input, err := n.webServices(block)
	if err != nil {
		return err
	}
	status := n.policies.services.Update(input)
	if status.State == kimi.NativeStateConflict {
		return errors.New(status.Reason)
	}
	return nil
}

func (n *Native) initEnhancements() error {
	home := filepath.Dir(filepath.Dir(filepath.Dir(n.policies.subagents.HomeDir())))
	store, err := kimi.NewMCPFileStore(home)
	if err != nil {
		return err
	}
	n.mcp, err = kimi.NewMCPManager(store)
	if err != nil {
		return err
	}
	settings := filepath.Join(n.service.StateDir, "web-tools-settings.json")
	n.webKeys = &webKey{keys: n.service.Keys, ref: currentWebCredentialRef(settings)}
	n.web = webtools.NewService(webtools.ServiceOptions{Settings: webCredentialSettings{path: settings, credential: n.webKeys}, Credentials: n.webKeys, AuthFile: filepath.Join(n.service.StateDir, "web-tools-local-auth.json")})
	if state := n.web.State(nil); state.Provider == webtools.ProviderTavily && state.KeySet {
		block, err := n.web.SaveProvider(webtools.ProviderTavily, "")
		if err != nil {
			return err
		}
		services, err := n.webServices(block)
		if err != nil {
			return err
		}
		n.policies.services.Update(services)
	}
	return nil
}

func (n *Native) toolsState(ctx context.Context) core.Document {
	result := n.mcp.List(ctx)
	servers := result.Servers
	if servers == nil {
		servers = []kimi.MCPServerView{}
	}
	return core.Document{"available": result.OK, "reason": result.Reason, "servers": servers, "probe": core.Document{"available": false, "reason": "native_mcp_test_requires_kernel_runtime"}}
}

func (n *Native) toolsAction(ctx context.Context, in core.Document) (any, error) {
	var result kimi.MCPStatus
	switch in["action"] {
	case "add":
		var input kimi.MCPServer
		if core.Object(in["server"]) == nil || decodeStrict(in["server"], &input) != nil {
			return nil, errors.New("invalid_tool_server")
		}
		result = n.mcp.Add(ctx, input)
	case "update":
		var patch kimi.MCPServer
		if core.Object(in["patch"]) == nil || decodeStrict(in["patch"], &patch) != nil {
			return nil, errors.New("invalid_tool_server")
		}
		result = n.mcp.Update(ctx, core.String(in["name"]), patch)
	case "remove":
		result = n.mcp.Remove(ctx, core.String(in["name"]))
	case "test":
		return nil, errors.New("native_mcp_test_requires_kernel_runtime")
	default:
		return nil, errors.New("invalid_tool_action")
	}
	if !result.OK {
		return nil, errors.New(result.Reason)
	}
	return core.Document{"tools": n.toolsState(ctx), "result": result}, nil
}

func (n *Native) webAction(in core.Document) (any, error) {
	provider := core.String(in["provider"])
	key := core.String(in["apiKey"])
	if in["action"] == "test" {
		return n.web.Probe(provider, key), nil
	}
	if in["action"] != "save" {
		return nil, errors.New("invalid_services")
	}
	n.service.mutation.Lock()
	defer n.service.mutation.Unlock()
	n.policies.mu.Lock()
	defer n.policies.mu.Unlock()
	committed := false
	defer func() {
		if !committed {
			n.webKeys.rollback()
		}
	}()
	block, err := n.web.SaveProvider(provider, key)
	if err != nil {
		return nil, err
	}
	if block != nil {
		if err := n.saveWebServices(block); err != nil {
			return nil, err
		}
	}
	if err = n.web.Commit(provider); err != nil {
		return nil, err
	}
	n.webKeys.previousRef = ""
	committed = true
	return core.Document{"state": n.web.State(n.policies.servicesView())}, nil
}
