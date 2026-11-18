package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"
	"time"

	"kimiworkplus.local/migration/internal/bridge"
	"kimiworkplus.local/migration/internal/core"
	"kimiworkplus.local/migration/internal/kimi"
	"kimiworkplus.local/migration/internal/platform"
)

func main() {
	if code, handled := platform.RunHelperFromArgs(os.Args); handled {
		os.Exit(code)
	}
	// The launch and stop flags are handled before this binary's own flag set, so
	// the same executable can be started by the login item, by the panel and by the
	// uninstaller without any of them depending on a shell.
	if code, handled := platform.RunBackgroundFromArgs(os.Args); handled {
		os.Exit(code)
	}
	if len(os.Args) > 1 && os.Args[1] == kimi.ContractWorkerFlag {
		os.Exit(kimi.RunContractWorker(os.Stdin, os.Stdout))
	}
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, core.ErrorCode(err))
		os.Exit(1)
	}
}
func run() error {
	defaultDir, e := bridge.DefaultStateDir()
	if e != nil {
		return e
	}
	dir := flag.String("state-dir", defaultDir, "preview state directory")
	port := flag.String("port", "", "loopback port (empty preserves the preview address)")
	stopDaemon := flag.Bool("stop-daemon", false, "stop this preview daemon after draining its HTTP requests")
	flag.Parse()
	if *stopDaemon {
		if err := bridge.StopDaemon(*dir); err != nil {
			return err
		}
		fmt.Println("Kimi Work+ preview daemon stopped")
		return nil
	}
	absolute, e := filepath.Abs(*dir)
	if e != nil {
		return e
	}
	*dir = absolute
	if e = core.PrivateDirectory(*dir); e != nil {
		return e
	}
	if os.Getenv("KIMI_WORK_REPLACE_DAEMON") == "1" {
		deadline := time.Now().Add(10 * time.Second)
		for {
			locked, err := platform.LockHeldByAnother(*dir)
			if err != nil {
				return err
			}
			if !locked {
				break
			}
			if time.Now().After(deadline) {
				return errors.New("daemon_replace_lock_timeout")
			}
			time.Sleep(50 * time.Millisecond)
		}
	}
	lock, e := platform.AcquireLock(*dir)
	if e != nil {
		return e
	}
	defer lock.Release()
	store, e := core.Open(*dir)
	if e != nil {
		return e
	}
	identity := sha256.Sum256([]byte(*dir))
	credentials, e := platform.OpenCredentials("dev.kimi-work-plus.preview." + hex.EncodeToString(identity[:8]))
	if e != nil {
		return e
	}
	keys := bridge.CacheKeys(credentials)
	gateway, e := bridge.NewRuntime(*dir, store, keys)
	if e != nil {
		return e
	}
	service := &bridge.Service{Store: store, StateDir: *dir, Keys: keys, Gateway: gateway, OnConfig: gateway.Apply, Extra: gateway.Action}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	service.SetStop(stop)
	if e = service.Start(*port, func() error {
		_, err := bridge.AttachNative(ctx, service, gateway)
		return err
	}); e != nil {
		return e
	}
	fmt.Println("Kimi Work+ preview daemon ready")
	<-ctx.Done()
	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return service.Close(deadline)
}
