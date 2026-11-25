package bridge

import (
	"context"
	"errors"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

func (n *Native) readWorkConversations(ctx context.Context) (any, error) {
	return n.controller.WorkConversations(ctx), nil
}

func (n *Native) rebindWorkConversations(ctx context.Context, in core.Document) (any, error) {
	request, err := parseConversationRebind(in)
	if err != nil {
		return nil, err
	}
	const action = "rebind-conversations"
	id, err := n.beginOperation(ctx, action)
	if err != nil {
		if ctx.Err() != nil {
			err = errors.New("conversation_rebind_cancelled")
		}
		return nil, err
	}
	n.logOperationStarted(id, action)
	defer func() { n.finishOperation(id, "done", nil, err) }()

	if ctx.Err() != nil {
		err = errors.New("conversation_rebind_cancelled")
		return nil, err
	}
	stopCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	var result kimi.ConversationRebindResult
	result, err = n.controller.RebindWorkConversations(stopCtx, request)
	return result, err
}

func parseConversationRebind(in core.Document) (kimi.ConversationRebindRequest, error) {
	invalid := errors.New("invalid_conversation_rebind")
	rows, hasKeys := in["conversationKeys"].([]any)
	alias, hasAlias := in["targetModelAlias"].(string)
	migrate, hasMigration := in["migratePreviousAlias"].(bool)
	_, keysPresent := in["conversationKeys"]
	_, migrationPresent := in["migratePreviousAlias"]
	_, aliasPresent := in["targetModelAlias"]
	if (aliasPresent && (!hasAlias || alias == "")) || (!aliasPresent && !migrate) || (keysPresent && !hasKeys) || (migrationPresent && !hasMigration) || migrate == hasKeys {
		return kimi.ConversationRebindRequest{}, invalid
	}
	request := kimi.ConversationRebindRequest{TargetModelAlias: alias, MigratePreviousAlias: migrate, ConversationKeys: make([]string, 0, len(rows))}
	for _, row := range rows {
		key, valid := row.(string)
		if !valid {
			return kimi.ConversationRebindRequest{}, invalid
		}
		request.ConversationKeys = append(request.ConversationKeys, key)
	}
	if value, present := in["thinkingLevel"]; present {
		level, valid := value.(string)
		if !valid || (alias == "" && level != "") {
			return kimi.ConversationRebindRequest{}, invalid
		}
		request.ThinkingLevel = level
	}
	if value, present := in["restartKimi"]; present {
		restart, valid := value.(bool)
		if !valid {
			return kimi.ConversationRebindRequest{}, invalid
		}
		request.RestartKimi = restart
	}
	return request, nil
}
