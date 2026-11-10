package bridge

import (
	"context"
	"errors"
	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/kimi"
	"kimiworkplus.local/migration/internal/webtools"
	"path/filepath"
)

type webService = webtools.Service

type webKey struct{ keys KeyStore }

func (k webKey) Get() (string, error) {
	if k.keys == nil {
		return "", nil
	}
	return k.keys.Get("web-tools-tavily")
}
func (k webKey) Set(value string) error {
	if k.keys == nil {
		return errors.New("credentials_unavailable")
	}
	return k.keys.Set("web-tools-tavily", value)
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
	n.web = webtools.NewService(webtools.ServiceOptions{Settings: webtools.FileSettings{Path: filepath.Join(n.service.StateDir, "web-tools-settings.json")}, Credentials: webKey{n.service.Keys}, AuthFile: filepath.Join(n.service.StateDir, "web-tools-local-auth.json")})
	if state := n.web.State(nil); state.Provider == webtools.ProviderTavily && state.KeySet {
		block, err := n.web.SaveProvider(webtools.ProviderTavily, "")
		if err != nil {
			return err
		}
		var services kimi.ServicesInput
		if decodeStrict(block, &services) != nil {
			return errors.New("invalid_services")
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
	block, err := n.web.SaveProvider(provider, key)
	if err != nil {
		return nil, err
	}
	if block != nil {
		var input kimi.ServicesInput
		if decodeStrict(block, &input) != nil {
			return nil, errors.New("invalid_services")
		}
		status := n.policies.services.Update(input)
		if status.State == kimi.NativeStateConflict {
			return nil, errors.New(status.Reason)
		}
	}
	if err = n.web.Commit(provider); err != nil {
		return nil, err
	}
	return core.Document{"state": n.web.State(n.policies.servicesView())}, nil
}
