//go:build !windows

package main

import (
	"flag"
	"fmt"
	"os"
)

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
