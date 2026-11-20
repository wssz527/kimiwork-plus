//go:build !windows

package main

import (
	"flag"
	"fmt"
	"os"
	"slices"

	"github.com/wssz527/kimiwork-plus/assets/icon"
)

func verifyPEIcon(path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	summary, err := icon.InspectPEIcons(raw)
	if err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	if !slices.Contains(summary.GroupIconIDs, icon.AppIconResourceID) || len(summary.IconIDs) == 0 {
		return fmt.Errorf("%s: missing embedded icon group %d (groups=%v icons=%v)", path, icon.AppIconResourceID, summary.GroupIconIDs, summary.IconIDs)
	}
	return nil
}

func main() {
	check := flag.String("check-payload", "", "verify embedded payload in a Windows setup executable")
	checkIcon := flag.String("check-icon", "", "verify embedded Windows PE icon resource in an executable")
	flag.Parse()
	if *check != "" {
		meta, files, err := inspectPayload(*check)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		if err := verifyPEIcon(*check); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("ok version=%s buildId=%s stubSize=%d files=%v\n", meta.Version, meta.BuildID, meta.StubSize, files)
		return
	}
	if *checkIcon != "" {
		if err := verifyPEIcon(*checkIcon); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("ok icon=%s\n", *checkIcon)
		return
	}
	fmt.Fprintln(os.Stderr, "wininstaller runs on Windows")
	os.Exit(1)
}
