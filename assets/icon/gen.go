//go:build ignore

package main

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/wssz527/kimiwork-plus/assets/icon"
)

func main() {
	syso, err := icon.BuildAMD64ResourceSyso(icon.WindowsICO, icon.AppIconResourceID)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	targets := []string{
		filepath.Join("..", "..", "cmd", "desktop", "rsrc_windows_amd64.syso"),
		filepath.Join("..", "..", "cmd", "wininstaller", "rsrc_windows_amd64.syso"),
		filepath.Join("..", "..", "cmd", "gateway-daemon", "rsrc_windows_amd64.syso"),
	}
	for _, dst := range targets {
		if err := os.WriteFile(dst, syso, 0o644); err != nil {
			fmt.Fprintln(os.Stderr, err)
			os.Exit(1)
		}
		fmt.Printf("wrote %s (%d bytes)\n", dst, len(syso))
	}
}
