//go:build !windows

package kimi

import "context"

func windowsIdentity(context.Context, int) ProcessIdentity {
	return ProcessIdentity{Kind: IdentityUnknown, Reason: "unsupported_platform"}
}
