package bridge

import (
	"context"

	"github.com/wssz527/kimi-work-plus/internal/core"
	"github.com/wssz527/kimi-work-plus/internal/kimi"
)

func (n *Native) syncWorkHome() (err error) {
	n.profileMu.Lock()
	defer n.profileMu.Unlock()
	defer func() {
		n.mu.Lock()
		n.profileReason = core.ErrorCode(err)
		n.mu.Unlock()
	}()
	home, err := n.controller.WorkHome()
	if err != nil {
		return err
	}
	changed, err := n.policies.setHome(home)
	if err != nil || !changed {
		return err
	}
	d, _, err := n.service.Store.Read()
	if err != nil {
		return err
	}
	if err := n.policies.Configure(d); err != nil {
		return err
	}
	n.policies.mu.Lock()
	n.policies.services.Update(n.policies.services.Intent())
	n.policies.mu.Unlock()

	n.mu.Lock()
	initialized := n.web != nil
	n.mu.Unlock()
	if !initialized {
		return nil
	}
	store, err := kimi.NewMCPFileStore(home)
	if err != nil {
		return err
	}
	manager, err := kimi.NewMCPManager(store)
	if err != nil {
		return err
	}
	n.mu.Lock()
	previous := n.mcp
	n.mcp = manager
	n.mu.Unlock()
	if previous != nil {
		_ = previous.Close(context.Background())
	}
	return nil
}
