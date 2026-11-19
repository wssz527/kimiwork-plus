package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/appupdate"
	"github.com/wssz527/kimiwork-plus/internal/bridge"
	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
	"github.com/wssz527/kimiwork-plus/internal/platform"
)

func main() {
	if code, handled := appupdate.RunApplyHelperFromArgs(os.Args); handled {
		os.Exit(code)
	}
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
	dir := flag.String("state-dir", defaultDir, "application state directory")
	port := flag.String("port", "", "loopback port (empty preserves the saved address)")
	stopDaemon := flag.Bool("stop-daemon", false, "stop this daemon after draining its HTTP requests")
	flag.Parse()
	if *stopDaemon {
		if err := bridge.StopDaemon(*dir); err != nil {
			return err
		}
		fmt.Println("KimiWork + daemon stopped")
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

	var migState bridge.PreviewMigrationState
	if defaultAbs, err := filepath.Abs(defaultDir); err == nil && *dir == defaultAbs {
		if legacyDir, err := bridge.LegacyPreviewStateDir(); err == nil && legacyDir != "" {
			migState, _ = bridge.MigrateStateDir(*dir, legacyDir)
		}
	}
	if migState.SourceDir != "" && !migState.LegacyServiceMigrated {
		if executable, err := os.Executable(); err == nil {
			migCtx, migCancel := context.WithTimeout(context.Background(), 10*time.Second)
			_, _, migErr := platform.MigrateLegacyPreviewService(migCtx, platform.ManagerOptions{
				Binary:   executable,
				StateDir: *dir,
				BuildID:  bridge.BuildID,
			})
			migCancel()
			if migErr == nil {
				_ = bridge.MarkLegacyServiceMigrated(*dir)
			}
		}
	}

	store, e := core.Open(*dir)
	if e != nil {
		return e
	}
	credentials, e := platform.OpenCredentials(bridge.CredentialNamespace(*dir))
	if e != nil {
		return e
	}
	keys := bridge.CacheKeys(credentials)

	var migration *bridge.MigrationCoordinator
	credSourceDir := migState.SourceDir
	if credSourceDir == "" {
		if saved, found, err := bridge.ReadMigrationState(*dir); err == nil && found {
			credSourceDir = saved.SourceDir
			migState = saved
		} else if bridge.ShouldAttemptInPlaceCredentialMigration(*dir, keys) {
			credSourceDir = *dir
		}
	}
	if credSourceDir != "" {
		var oldKeys bridge.KeyStore
		if !migState.CredentialsCompleted {
			if oldCreds, oldErr := platform.OpenCredentials(bridge.LegacyPreviewCredentialNamespace(credSourceDir)); oldErr == nil {
				oldKeys = bridge.CacheKeys(oldCreds)
			}
		}
		migration = bridge.NewMigrationCoordinator(*dir, credSourceDir, oldKeys, keys)
		if oldKeys != nil {
			_, _ = migration.MigrateStartup()
		}
	}

	gateway, e := bridge.NewRuntime(*dir, store, keys)
	if e != nil {
		return e
	}
	var nativeRef atomic.Pointer[bridge.Native]
	exePath, _ := os.Executable()
	updates, e := appupdate.NewManager(appupdate.Options{
		StateDir:       *dir,
		CurrentVersion: bridge.Version,
		BuildID:        bridge.BuildID,
		ExecutablePath: exePath,
		IdleCheck: func() (bool, string) {
			if n := nativeRef.Load(); n != nil {
				return n.UpdateIdleCheck(gateway)
			}
			if gateway.ActiveRequests() > 0 {
				return false, "网关仍有正在传输的模型请求，等待完成后再更新。"
			}
			return true, ""
		},
	})
	if e != nil {
		return e
	}
	service := &bridge.Service{Store: store, StateDir: *dir, Keys: keys, Migration: migration, Updates: updates, Gateway: gateway, OnConfig: gateway.Apply, Extra: gateway.Action}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	service.SetStop(stop)
	if e = service.Start(*port, func() error {
		native, err := bridge.AttachNative(ctx, service, gateway)
		if err == nil && native != nil {
			nativeRef.Store(native)
		}
		return err
	}); e != nil {
		updates.Close()
		return e
	}
	fmt.Println("KimiWork + daemon ready")
	<-ctx.Done()
	deadline, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return service.Close(deadline)
}
