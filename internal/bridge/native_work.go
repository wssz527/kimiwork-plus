package bridge

import (
	"context"
	"time"

	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/kimi"
)

func (n *Native) readSwarm(ctx context.Context) (any, error) {
	deadline, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	snapshot, err := n.controller.SwarmMode(deadline)
	if err != nil && snapshot.Reason == "" {
		snapshot.State = kimi.SwarmStateUnknown
		snapshot.Reason = core.ErrorCode(err)
	}
	n.mu.Lock()
	n.swarm = snapshot
	n.mu.Unlock()
	return core.Document{"swarm": snapshot}, nil
}

func (n *Native) setSwarm(ctx context.Context, conversationKey string, enabled bool) (any, error) {
	snapshot, err := n.controller.SetSwarmMode(ctx, conversationKey, enabled)
	n.mu.Lock()
	n.swarm = snapshot
	n.mu.Unlock()
	return core.Document{"swarm": snapshot}, err
}
