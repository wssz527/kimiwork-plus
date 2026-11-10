package kimi

import (
	"context"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"
)

// TestInstalledArchiveReadOnly probes the Kimi actually installed on this
// machine. It only reads app.asar: no process is started, nothing is written,
// and no network call is made. The assertion is deliberately loose on state —
// an unreadable future bundle must report unverified, not fail the suite — but
// it does require the probe to finish inside its budget.
func TestInstalledArchiveReadOnly(t *testing.T) {
	if os.Getenv("KIMI_SKIP_LIVE") == "1" {
		t.Skip("live probe disabled")
	}
	archive := firstExisting(ArchiveCandidates(runtime.GOOS, environMap()))
	if archive == "" {
		t.Skip("no Kimi installation on this host")
	}
	if _, err := os.Stat(archive); err != nil {
		t.Skip("archive not readable")
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result := Inspect(ctx, ProbeOptions{Archive: archive})
	t.Logf("archive=%s version=%s state=%s reason=%s models=%d accepted=%d functions=%d elapsed=%v",
		archive, result.Version, result.State, result.Reason, result.ModelCount,
		len(result.Accepted), result.Functions, time.Since(start))
	if result.State == StateUnavailable {
		t.Fatalf("installed archive reported unavailable: %s", result.Reason)
	}
	if result.Version == "" {
		t.Log("version unreadable; state still recorded")
	}
}

func TestInstalledLaunchContractReadOnly(t *testing.T) {
	if os.Getenv("KIMI_SKIP_LIVE") == "1" {
		t.Skip("live probe disabled")
	}
	archive := firstExisting(ArchiveCandidates(runtime.GOOS, environMap()))
	if archive == "" {
		t.Skip("no Kimi installation on this host")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	result := InspectLaunchContract(ctx, ProbeOptions{Archive: archive})
	t.Logf("launch state=%s reason=%s", result.State, result.Reason)
	switch result.State {
	case "supported", "unsupported", "unverified":
	default:
		t.Fatalf("unexpected launch state %q", result.State)
	}
}

func environMap() map[string]string {
	out := map[string]string{}
	for _, entry := range os.Environ() {
		name, value, found := strings.Cut(entry, "=")
		if !found {
			continue
		}
		out[name] = value
	}
	return out
}
