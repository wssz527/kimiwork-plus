package bridge

import (
	"context"
	"testing"

	"github.com/wssz527/kimiwork-plus/internal/core"
)

func TestNativeLaunchRejectsUnrecognizedProfileRequests(t *testing.T) {
	n := &Native{}
	for _, input := range []core.Document{
		{"action": "launch-kimi", "profileMode": "unknown"},
		{"action": "launch-kimi", "profileMode": true},
		{"action": "connect-kimi", "profileMode": "existing-authorized"},
	} {
		if _, err := n.Action(context.Background(), input); err == nil || err.Error() != "invalid_profile_mode" {
			t.Fatalf("profile request was not rejected before side effects: %+v %v", input, err)
		}
	}
}
