//go:build !darwin && !windows

package appupdate

import (
	"errors"
	"os/exec"
	"time"
)

func configureDetachedHelper(*exec.Cmd)                                         {}
func waitForProcessExit(int, time.Duration) bool                                { return true }
func stopTargetDesktop(string, func(string, ...any))                            {}
func forceStopTargetDaemon(string, func(string, ...any))                        {}
func validateStagedArtifact(string) error                                       { return errors.New("unsupported_platform") }
func validateInstalledArtifact(string) error                                    { return errors.New("unsupported_platform") }
func swapInstallation(string, string, string, func(string, ...any)) error       { return errors.New("unsupported_platform") }
func restoreInstallation(string, string, func(string, ...any)) error            { return errors.New("unsupported_platform") }
func launchInstallation(string, string, string, func(string, ...any)) error     { return errors.New("unsupported_platform") }
func installedDaemonBinary(string) string                                       { return "" }
func postInstallPlatformMetadata(ApplyPlan, daemonHealth)                       {}
