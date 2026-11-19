//go:build !windows

package main

import (
	"flag"
	"fmt"
	"os"
)

func main() {
	check := flag.String("check-payload", "", "verify embedded payload in a Windows setup executable")
	flag.Parse()
	if *check != "" {
		meta, files, err := inspectPayload(*check)
		if err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("ok version=%s buildId=%s stubSize=%d files=%v\n", meta.Version, meta.BuildID, meta.StubSize, files)
		return
	}
	fmt.Fprintln(os.Stderr, "wininstaller runs on Windows")
	os.Exit(1)
}
