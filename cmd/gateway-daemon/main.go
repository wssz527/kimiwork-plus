package main

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
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
	dir := flag.String("state-dir", defaultDir, "isolated preview state directory")
	port := flag.String("port", "0", "loopback port (0 selects a free port)")
	flag.Parse()
	absolute, e := filepath.Abs(*dir)
	if e != nil {
		return e
	}
	*dir = absolute
	if e = core.PrivateDirectory(*dir); e != nil {
		return e
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
