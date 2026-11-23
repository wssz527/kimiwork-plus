package appupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wssz527/kimiwork-plus/internal/core"
)

const (
	settingsFileName   = "update-settings.json"
	historyFileName    = "update-history.json"
	rollbackDirName    = "rollback"
	rollbackPrevName   = "previous"
	rollbackMetaFile   = "metadata.json"
	defaultCheckPeriod = 24 * time.Hour
	startupCheckDelay  = 5 * time.Second
)

type Options struct {
	StateDir             string
	CurrentVersion       string
	BuildID              string
	ExecutablePath       string
	TargetPath           string
	FeedURL              string
	MirrorBase           string
	GitHubRepo           string
	GitHubAPIBase        string
	GitHubWebBase        string
	PublicKey            []byte
	HTTPClient           *http.Client
	IdleCheck            func() (idle bool, reason string)
	ValidateStaged       func(stagedPath string) error
	CheckPeriod          time.Duration
	StartupDelay         time.Duration
	CheckSourceTimeout   time.Duration
	DownloadStallTimeout time.Duration
	DisableAutoRun       bool
	LaunchMode           string // "" (desktop app) or "daemon" (headless daemon-only verification)
}

type Settings struct {
	Version       int    `json:"version"`
	AutoCheck     bool   `json:"autoCheck"`
	Channel       string `json:"channel"`
	LastCheckedAt string `json:"lastCheckedAt,omitempty"`
}

type AvailableRelease struct {
	Version     string `json:"version"`
	Channel     string `json:"channel"`
	Name        string `json:"name,omitempty"`
	Notes       string `json:"notes,omitempty"`
	PublishedAt string `json:"publishedAt,omitempty"`
	Filename    string `json:"filename,omitempty"`
	Size        int64  `json:"size,omitempty"`
	Source      string `json:"source,omitempty"`
	ReleaseURL  string `json:"releaseUrl,omitempty"`
}

type DownloadProgress struct {
	Written int64   `json:"written"`
	Total   int64   `json:"total"`
	Percent int     `json:"percent"`
	Rate    float64 `json:"rate"`
}

type UpdateError struct {
	Stage    string `json:"stage"`
	Code     string `json:"code"`
	Message  string `json:"message"`
	NextStep string `json:"nextStep"`
	Detail   string `json:"detail,omitempty"`
}

type RollbackMetadata struct {
	Version    string `json:"version"`
	BuildID    string `json:"buildId"`
	TargetPath string `json:"targetPath"`
	BackupPath string `json:"backupPath"`
	SavedAt    string `json:"savedAt"`
	Platform   string `json:"platform"`
}

type RollbackRecord struct {
	Automatic   bool   `json:"automatic"`
	FromVersion string `json:"fromVersion,omitempty"`
	FromBuildID string `json:"fromBuildId,omitempty"`
	ToVersion   string `json:"toVersion"`
	ToBuildID   string `json:"toBuildId,omitempty"`
	Reason      string `json:"reason,omitempty"`
	At          string `json:"at"`
}

type History struct {
	Version      int             `json:"version"`
	LastRollback *RollbackRecord `json:"lastRollback,omitempty"`
	LastApplied  *RollbackRecord `json:"lastApplied,omitempty"`
}

type Manager struct {
	mu                sync.Mutex
	opts              Options
	pubKey            []byte
	settings          Settings
	useCustomAPI      bool
	explicitGitHubWeb bool

	state      string
	available  *AvailableRelease
	progress   *DownloadProgress
	lastErr    *UpdateError
	idleReason string
	lastSource string

	wailsUpdater *updater.Updater
	wailsHost    *managerHost
	opCancel     context.CancelFunc
	operationID  uint64
	stateEpoch   uint64
	stopCh       chan struct{}
	stopOnce     sync.Once
}

func isMirrorDisabledValue(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "off", "none", "disabled":
		return true
	default:
		return false
	}
}

func NewManager(opts Options) (*Manager, error) {
	if opts.StateDir == "" {
		return nil, errors.New("update_state_dir_required")
	}
	pub := opts.PublicKey
	if len(pub) == 0 {
		var err error
		pub, err = PinnedPublicKey()
		if err != nil {
			return nil, err
		}
	}
	opts.GitHubRepo = strings.TrimSpace(opts.GitHubRepo)
	if opts.GitHubRepo == "" {
		opts.GitHubRepo = DefaultGitHubRepo
	}

	rawAPIBase := strings.TrimRight(strings.TrimSpace(opts.GitHubAPIBase), "/")
	defaultAPIBase := strings.TrimRight(strings.TrimSpace(DefaultGitHubAPIBase), "/")
	useCustomAPI := rawAPIBase != "" && rawAPIBase != defaultAPIBase
	if rawAPIBase == "" {
		opts.GitHubAPIBase = defaultAPIBase
	} else {
		opts.GitHubAPIBase = rawAPIBase
	}

	rawWebBase := strings.TrimRight(strings.TrimSpace(opts.GitHubWebBase), "/")
	explicitGitHubWeb := rawWebBase != ""
	if rawWebBase == "" {
		opts.GitHubWebBase = strings.TrimRight(strings.TrimSpace(DefaultGitHubWebBase), "/")
	} else {
		opts.GitHubWebBase = rawWebBase
	}

	mirrorRaw := strings.TrimSpace(opts.MirrorBase)
	mirrorExplicit := mirrorRaw != ""
	if !mirrorExplicit {
		if envMirror, ok := os.LookupEnv("KIMI_WORK_PLUS_UPDATE_MIRROR_BASE"); ok && strings.TrimSpace(envMirror) != "" {
			mirrorRaw = strings.TrimSpace(envMirror)
			mirrorExplicit = true
		}
	}
	if mirrorExplicit {
		if isMirrorDisabledValue(mirrorRaw) {
			opts.MirrorBase = ""
		} else {
			opts.MirrorBase = strings.TrimRight(mirrorRaw, "/")
		}
	} else if !useCustomAPI && opts.GitHubRepo == DefaultGitHubRepo && !isMirrorDisabledValue(DefaultMirrorBase) {
		opts.MirrorBase = strings.TrimRight(strings.TrimSpace(DefaultMirrorBase), "/")
	} else {
		opts.MirrorBase = ""
	}

	if opts.FeedURL == "" {
		opts.FeedURL = strings.TrimSpace(os.Getenv("KIMI_WORK_PLUS_UPDATE_FEED_URL"))
	}
	if opts.LaunchMode == "" {
		opts.LaunchMode = strings.TrimSpace(os.Getenv("KIMI_WORK_PLUS_UPDATE_LAUNCH_MODE"))
	}
	if opts.CheckPeriod <= 0 {
		opts.CheckPeriod = defaultCheckPeriod
	}
	if opts.StartupDelay <= 0 {
		opts.StartupDelay = startupCheckDelay
	}
	if opts.CheckSourceTimeout <= 0 {
		opts.CheckSourceTimeout = defaultCheckSourceTimeout
	}
	if opts.DownloadStallTimeout <= 0 {
		opts.DownloadStallTimeout = defaultDownloadStallTimeout
	}
	if opts.ExecutablePath == "" {
		if exe, err := os.Executable(); err == nil {
			opts.ExecutablePath = exe
		}
	}
	if opts.TargetPath == "" {
		opts.TargetPath = resolveInstallTarget(opts.ExecutablePath)
	}

	m := &Manager{
		opts:              opts,
		pubKey:            pub,
		useCustomAPI:      useCustomAPI,
		explicitGitHubWeb: explicitGitHubWeb,
		state:             "idle",
		stopCh:            make(chan struct{}),
	}
	m.settings = m.loadSettings()
	if !opts.DisableAutoRun {
		go m.backgroundLoop()
	}
	return m, nil
}

func (m *Manager) Close() {
	m.stopOnce.Do(func() {
		close(m.stopCh)
	})
	m.Cancel()
}

func (m *Manager) backgroundLoop() {
	select {
	case <-time.After(m.opts.StartupDelay):
		if m.autoCheckEnabled() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			_, _ = m.CheckNow(ctx)
			cancel()
		}
	case <-m.stopCh:
		return
	}

	ticker := time.NewTicker(m.opts.CheckPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			if m.autoCheckEnabled() {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				_, _ = m.CheckNow(ctx)
				cancel()
			}
		case <-m.stopCh:
			return
		}
	}
}

func (m *Manager) autoCheckEnabled() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.settings.AutoCheck
}

func (m *Manager) loadSettings() Settings {
	def := Settings{Version: 1, AutoCheck: true, Channel: "stable"}
	raw, err := core.ReadPrivateFile(filepath.Join(m.opts.StateDir, settingsFileName), 64<<10)
	if err != nil {
		return def
	}
	var s Settings
	if json.Unmarshal(raw, &s) != nil {
		return def
	}
	s.Version = 1
	s.Channel = normalizeChannel(s.Channel)
	return s
}

func (m *Manager) saveSettingsLocked() error {
	m.settings.Version = 1
	m.settings.Channel = normalizeChannel(m.settings.Channel)
	return core.AtomicJSON(filepath.Join(m.opts.StateDir, settingsFileName), m.settings)
}

func loadHistory(stateDir string) History {
	raw, err := core.ReadPrivateFile(filepath.Join(stateDir, historyFileName), 64<<10)
	if err != nil {
		return History{Version: 1}
	}
	var h History
	if json.Unmarshal(raw, &h) != nil {
		return History{Version: 1}
	}
	h.Version = 1
	return h
}

func saveHistory(stateDir string, h History) error {
	h.Version = 1
	return core.AtomicJSON(filepath.Join(stateDir, historyFileName), h)
}

func (m *Manager) UpdateSettings(autoCheck *bool, channel string) (core.Document, error) {
	m.mu.Lock()
	changedChannel := false
	if autoCheck != nil {
		m.settings.AutoCheck = *autoCheck
	}
	if strings.TrimSpace(channel) != "" {
		next := normalizeChannel(channel)
		if next != m.settings.Channel {
			m.settings.Channel = next
			changedChannel = true
		}
	}
	if err := m.saveSettingsLocked(); err != nil {
		m.mu.Unlock()
		return nil, err
	}
	if changedChannel {
		m.stateEpoch++
		if m.state == "checking" || m.state == "available" || m.state == "up_to_date" || m.state == "error" {
			m.state = "idle"
			m.available = nil
			m.lastErr = nil
			m.lastSource = ""
			m.wailsUpdater = nil
			m.wailsHost = nil
		}
	}
	m.mu.Unlock()
	return m.State(), nil
}

func (m *Manager) setActiveSource(source string) {
	if source == "" {
		return
	}
	m.mu.Lock()
	m.lastSource = source
	m.mu.Unlock()
}

func availableReleaseFromUpdater(rel *updater.Release) *AvailableRelease {
	pubAt := ""
	if !rel.PublishedAt.IsZero() {
		pubAt = rel.PublishedAt.UTC().Format(time.RFC3339)
	}
	source, _ := rel.Metadata[metaUpdateSource].(string)
	releaseURL, _ := rel.Metadata[metaUpdateReleaseURL].(string)
	return &AvailableRelease{
		Version:     rel.Version,
		Channel:     normalizeChannel(rel.Channel),
		Name:        rel.Name,
		Notes:       rel.Notes,
		PublishedAt: pubAt,
		Filename:    rel.Artifact.Filename,
		Size:        rel.Artifact.Size,
		Source:      source,
		ReleaseURL:  releaseURL,
	}
}

func (m *Manager) newWailsUpdater(channel string) (*updater.Updater, *managerHost, error) {
	client := m.opts.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}
	var provider updater.Provider
	var err error
	if m.opts.FeedURL != "" {
		provider, err = newFeedEndpointProvider(m.opts.FeedURL, channel, client, m.opts.DownloadStallTimeout, m.setActiveSource)
	} else {
		provider, err = newMirrorFallbackProvider(mirrorProviderConfig{
			Repo:              m.opts.GitHubRepo,
			MirrorBase:        m.opts.MirrorBase,
			GitHubWebBase:     m.opts.GitHubWebBase,
			GitHubAPIBase:     m.opts.GitHubAPIBase,
			UseCustomAPI:      m.useCustomAPI,
			ExplicitGitHubWeb: m.explicitGitHubWeb,
			Channel:           channel,
			HTTPClient:        client,
			CheckTimeout:      m.opts.CheckSourceTimeout,
			StallTimeout:      m.opts.DownloadStallTimeout,
			OnSourceActive:    m.setActiveSource,
		})
	}
	if err != nil {
		return nil, nil, err
	}

	host := &managerHost{mgr: m}
	u := updater.New(host)
	err = u.Init(updater.Config{
		CurrentVersion: m.opts.CurrentVersion,
		Providers:      []updater.Provider{provider},
		PublicKey:      m.pubKey,
		Channel:        channel,
		Platform:       runtime.GOOS,
		Arch:           runtime.GOARCH,
		Window:         updater.WindowNone,
	})
	if err != nil {
		return nil, nil, err
	}
	return u, host, nil
}

func (m *Manager) CheckNow(ctx context.Context) (core.Document, error) {
	m.mu.Lock()
	if m.state == "checking" || m.state == "downloading" || m.state == "verifying" || m.state == "waiting_idle" || m.state == "applying" {
		st := m.stateLocked()
		m.mu.Unlock()
		return st, nil
	}
	channel := m.settings.Channel
	m.stateEpoch++
	epoch := m.stateEpoch
	m.state = "checking"
	m.lastErr = nil
	m.mu.Unlock()

	u, host, err := m.newWailsUpdater(channel)
	if err != nil {
		m.mu.Lock()
		if m.state == "checking" && m.stateEpoch == epoch {
			m.state = "error"
			m.lastErr = classifyUpdateError("check", err)
		}
		st := m.stateLocked()
		m.mu.Unlock()
		return st, err
	}

	rel, err := u.Check(ctx)
	now := time.Now().UTC().Format(time.RFC3339)

	m.mu.Lock()
	if m.state != "checking" || m.stateEpoch != epoch {
		st := m.stateLocked()
		m.mu.Unlock()
		return st, nil
	}
	m.settings.LastCheckedAt = now
	_ = m.saveSettingsLocked()
	if err != nil {
		m.state = "error"
		m.lastErr = classifyUpdateError("check", err)
		st := m.stateLocked()
		m.mu.Unlock()
		return st, err
	}
	m.wailsUpdater = u
	m.wailsHost = host
	if rel == nil {
		m.state = "up_to_date"
		m.available = nil
		m.lastErr = nil
	} else {
		m.state = "available"
		avail := availableReleaseFromUpdater(rel)
		if avail.Source != "" {
			m.lastSource = avail.Source
		}
		m.available = avail
		m.lastErr = nil
	}
	st := m.stateLocked()
	m.mu.Unlock()
	return st, nil
}

// StartDownloadAndApply starts asynchronous download, Ed25519 signature
// verification, idle-wait, backup, and detached replacement.
func (m *Manager) StartDownloadAndApply() (core.Document, error) {
	m.mu.Lock()
	switch m.state {
	case "downloading", "verifying", "waiting_idle", "applying":
		st := m.stateLocked()
		m.mu.Unlock()
		return st, nil
	}
	if m.available == nil {
		m.mu.Unlock()
		return nil, errors.New("update_not_available")
	}
	u := m.wailsUpdater
	host := m.wailsHost
	channel := m.settings.Channel
	targetVersion := m.available.Version
	opCtx, cancel := context.WithCancel(context.Background())
	m.stateEpoch++
	m.operationID++
	operationID := m.operationID
	m.opCancel = cancel
	m.state = "downloading"
	m.lastErr = nil
	m.idleReason = ""
	m.progress = &DownloadProgress{Total: m.available.Size}
	st := m.stateLocked()
	m.mu.Unlock()

	go m.runDownloadAndApply(opCtx, u, host, channel, targetVersion, operationID)
	return st, nil
}

func (m *Manager) runDownloadAndApply(ctx context.Context, u *updater.Updater, host *managerHost, channel, targetVersion string, operationID uint64) {
	defer func() {
		m.mu.Lock()
		if m.operationID == operationID && m.opCancel != nil {
			if host != nil {
				host.operationID = 0
			}
			m.opCancel()
			m.opCancel = nil
		}
		m.mu.Unlock()
	}()

	// Re-run Check if updater was cleared or prior attempt failed so retry always works cleanly.
	if u == nil || u.State() != updater.StateAvailable {
		fresh, freshHost, err := m.newWailsUpdater(channel)
		if err != nil {
			m.failDownload(operationID, "download", err)
			return
		}
		rel, err := fresh.Check(ctx)
		if err != nil {
			m.failDownload(operationID, "check", err)
			return
		}
		if rel == nil {
			m.mu.Lock()
			if m.operationID != operationID {
				m.mu.Unlock()
				return
			}
			m.state = "up_to_date"
			m.available = nil
			m.progress = nil
			m.mu.Unlock()
			return
		}
		u = fresh
		host = freshHost
		targetVersion = rel.Version
		avail := availableReleaseFromUpdater(rel)
		m.mu.Lock()
		if m.operationID != operationID {
			m.mu.Unlock()
			return
		}
		m.wailsUpdater = u
		m.wailsHost = host
		m.available = avail
		if avail.Source != "" {
			m.lastSource = avail.Source
		}
		m.mu.Unlock()
	}

	m.mu.Lock()
	if m.operationID != operationID {
		m.mu.Unlock()
		return
	}
	if host != nil {
		host.operationID = operationID
	}
	m.mu.Unlock()

	if err := u.DownloadAndInstall(ctx); err != nil {
		if errors.Is(err, context.Canceled) {
			m.mu.Lock()
			if m.operationID != operationID {
				m.mu.Unlock()
				return
			}
			m.state = "available"
			m.progress = nil
			m.lastErr = nil
			m.mu.Unlock()
			return
		}
		stage := "download"
		lower := strings.ToLower(err.Error())
		if strings.Contains(lower, "signature") || strings.Contains(lower, "digest") || strings.Contains(lower, "public key") {
			stage = "verify"
		}
		// Reset updater instance so next click retries cleanly from scratch.
		m.failDownload(operationID, stage, err)
		return
	}

	stagedPath := u.DownloadedPath()
	m.mu.Lock()
	stale := m.operationID != operationID
	m.mu.Unlock()
	if stale {
		if stagedPath != "" {
			_ = os.RemoveAll(filepath.Dir(stagedPath))
		}
		return
	}
	if stagedPath == "" {
		m.failDownload(operationID, "install", errors.New("staged_payload_missing"))
		return
	}

	validate := validateStagedArtifact
	if m.opts.ValidateStaged != nil {
		validate = m.opts.ValidateStaged
	}
	if err := validate(stagedPath); err != nil {
		_ = os.RemoveAll(filepath.Dir(stagedPath))
		m.failDownload(operationID, "verify", err)
		return
	}

	m.mu.Lock()
	stale = m.operationID != operationID
	m.mu.Unlock()
	if stale {
		_ = os.RemoveAll(filepath.Dir(stagedPath))
		return
	}

	if err := m.waitUntilIdle(ctx, operationID); err != nil {
		_ = os.RemoveAll(filepath.Dir(stagedPath))
		m.mu.Lock()
		if m.operationID != operationID {
			m.mu.Unlock()
			return
		}
		m.wailsUpdater = nil
		m.wailsHost = nil
		if errors.Is(err, context.Canceled) {
			m.state = "available"
			m.progress = nil
			m.idleReason = ""
			m.lastErr = nil
		} else {
			m.state = "error"
			m.lastErr = classifyUpdateError("install", err)
		}
		m.mu.Unlock()
		return
	}

	m.mu.Lock()
	if m.operationID != operationID {
		m.mu.Unlock()
		_ = os.RemoveAll(filepath.Dir(stagedPath))
		return
	}
	m.state = "applying"
	m.idleReason = ""
	m.mu.Unlock()

	if err := m.prepareAndLaunchHelper(stagedPath, targetVersion, "update"); err != nil {
		_ = os.RemoveAll(filepath.Dir(stagedPath))
		m.failDownload(operationID, "install", err)
		return
	}
}

func (m *Manager) StartRollback() (core.Document, error) {
	m.mu.Lock()
	switch m.state {
	case "downloading", "verifying", "waiting_idle", "applying":
		st := m.stateLocked()
		m.mu.Unlock()
		return st, nil
	}
	meta, ok := m.readRollbackMetadata()
	if !ok {
		m.mu.Unlock()
		return nil, errors.New("rollback_unavailable")
	}
	opCtx, cancel := context.WithCancel(context.Background())
	m.stateEpoch++
	m.operationID++
	operationID := m.operationID
	m.opCancel = cancel
	m.state = "waiting_idle"
	m.lastErr = nil
	m.idleReason = ""
	st := m.stateLocked()
	m.mu.Unlock()

	go func() {
		defer func() {
			m.mu.Lock()
			if m.operationID == operationID && m.opCancel != nil {
				m.opCancel()
				m.opCancel = nil
			}
			m.mu.Unlock()
		}()
		if err := m.waitUntilIdle(opCtx, operationID); err != nil {
			m.mu.Lock()
			if m.operationID != operationID {
				m.mu.Unlock()
				return
			}
			if errors.Is(err, context.Canceled) {
				m.state = "idle"
				m.idleReason = ""
				m.lastErr = nil
			} else {
				m.state = "error"
				m.lastErr = classifyUpdateError("rollback", err)
			}
			m.mu.Unlock()
			return
		}
		m.mu.Lock()
		if m.operationID != operationID {
			m.mu.Unlock()
			return
		}
		m.state = "applying"
		m.idleReason = ""
		m.mu.Unlock()
		if err := m.launchRollbackHelper(meta); err != nil {
			m.setError(operationID, "rollback", err)
		}
	}()
	return st, nil
}

func (m *Manager) Cancel() core.Document {
	m.mu.Lock()
	if m.state == "applying" {
		st := m.stateLocked()
		m.mu.Unlock()
		return st
	}
	if m.opCancel != nil || m.state == "downloading" || m.state == "verifying" || m.state == "waiting_idle" {
		m.operationID++
	}
	if m.opCancel != nil {
		m.opCancel()
		m.opCancel = nil
		m.wailsUpdater = nil
		m.wailsHost = nil
	}
	if m.state == "downloading" || m.state == "verifying" || m.state == "waiting_idle" {
		if m.available != nil {
			m.state = "available"
		} else {
			m.state = "idle"
		}
		m.progress = nil
		m.idleReason = ""
	}
	st := m.stateLocked()
	m.mu.Unlock()
	return st
}

func (m *Manager) waitUntilIdle(ctx context.Context, operationID uint64) error {
	if m.opts.IdleCheck == nil {
		return nil
	}
	for {
		idle, reason := m.opts.IdleCheck()
		if idle {
			return nil
		}
		m.mu.Lock()
		if m.operationID != operationID {
			m.mu.Unlock()
			return context.Canceled
		}
		m.state = "waiting_idle"
		m.idleReason = reason
		m.mu.Unlock()

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-m.stopCh:
			return context.Canceled
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (m *Manager) prepareAndLaunchHelper(stagedPath, targetVersion, mode string) error {
	targetPath := m.opts.TargetPath
	if targetPath == "" {
		return errors.New("update_target_unresolved")
	}
	if _, err := os.Stat(targetPath); err != nil {
		return fmt.Errorf("update_target_missing: %w", err)
	}

	backupDir, err := m.backupCurrentInstallation(targetPath)
	if err != nil {
		return fmt.Errorf("update_backup_failed: %w", err)
	}

	plan := ApplyPlan{
		Mode:              mode,
		StateDir:          m.opts.StateDir,
		TargetPath:        targetPath,
		StagedPath:        stagedPath,
		StagingRoot:       filepath.Dir(stagedPath),
		BackupPath:        backupDir,
		PrevVersion:       m.opts.CurrentVersion,
		PrevBuildID:       m.opts.BuildID,
		TargetVersion:     targetVersion,
		DaemonPID:         os.Getpid(),
		LaunchMode:        m.opts.LaunchMode,
		StartupTimeoutSec: 25,
	}
	return m.spawnApplyHelper(plan)
}

func (m *Manager) launchRollbackHelper(meta RollbackMetadata) error {
	targetPath := m.opts.TargetPath
	if targetPath == "" {
		targetPath = meta.TargetPath
	}
	if targetPath == "" {
		return errors.New("update_target_unresolved")
	}
	// Copy rollback payload into a temporary staged directory so the swap can
	// keep the current version in a temporary recovery slot if rollback itself fails.
	stagingRoot, err := os.MkdirTemp(os.TempDir(), "kwp-rollback-stage-*")
	if err != nil {
		return err
	}
	stagedPath := filepath.Join(stagingRoot, filepath.Base(meta.BackupPath))
	if err := copyAny(meta.BackupPath, stagedPath); err != nil {
		_ = os.RemoveAll(stagingRoot)
		return err
	}
	safetyBackup := filepath.Join(stagingRoot, "pre-rollback-backup")
	if err := copyInstallationSnapshot(targetPath, safetyBackup); err != nil {
		_ = os.RemoveAll(stagingRoot)
		return err
	}

	plan := ApplyPlan{
		Mode:              "rollback",
		StateDir:          m.opts.StateDir,
		TargetPath:        targetPath,
		StagedPath:        stagedPath,
		StagingRoot:       stagingRoot,
		BackupPath:        safetyBackup,
		PrevVersion:       m.opts.CurrentVersion,
		PrevBuildID:       m.opts.BuildID,
		TargetVersion:     meta.Version,
		TargetBuildID:     meta.BuildID,
		DaemonPID:         os.Getpid(),
		LaunchMode:        m.opts.LaunchMode,
		StartupTimeoutSec: 25,
	}
	return m.spawnApplyHelper(plan)
}

func (m *Manager) spawnApplyHelper(plan ApplyPlan) error {
	planPath := filepath.Join(m.opts.StateDir, "update-plan.json")
	if err := core.AtomicJSON(planPath, plan); err != nil {
		return err
	}

	helperExe, err := copyHelperBinary(m.opts.ExecutablePath, m.opts.StateDir)
	if err != nil {
		return err
	}

	cmd := exec.Command(helperExe, ApplyHelperFlag, planPath)
	cmd.Dir = os.TempDir()
	configureDetachedHelper(cmd)
	if err := cmd.Start(); err != nil {
		_ = os.Remove(helperExe)
		return err
	}
	_ = cmd.Process.Release()
	return nil
}

func copyHelperBinary(srcExe, stateDir string) (string, error) {
	if srcExe == "" {
		var err error
		srcExe, err = os.Executable()
		if err != nil {
			return "", err
		}
	}
	helperDir := strings.TrimSpace(stateDir)
	if helperDir != "" {
		if err := core.PrivateDirectory(helperDir); err != nil {
			return "", err
		}
	} else {
		helperDir = os.TempDir()
	}
	sweepStaleHelpers(helperDir)
	if helperDir != os.TempDir() {
		sweepStaleHelpers(os.TempDir())
	}
	pattern := "kwp-update-helper-*"
	if runtime.GOOS == "windows" {
		pattern = "kwp-update-helper-*.exe"
	}
	in, err := os.Open(srcExe)
	if err != nil {
		return "", err
	}
	defer in.Close()

	out, err := os.CreateTemp(helperDir, pattern)
	if err != nil {
		return "", err
	}
	dst := out.Name()
	cleanup := func(cause error) (string, error) {
		_ = out.Close()
		_ = os.Remove(dst)
		return "", cause
	}
	if _, err := io.Copy(out, in); err != nil {
		return cleanup(err)
	}
	if err := out.Sync(); err != nil {
		return cleanup(err)
	}
	if err := out.Chmod(0o700); err != nil {
		return cleanup(err)
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(dst)
		return "", err
	}
	return dst, nil
}

func sweepStaleHelpers(dir string) {
	if dir == "" {
		return
	}
	matches, err := filepath.Glob(filepath.Join(dir, "kwp-update-helper-*"))
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-1 * time.Hour)
	for _, path := range matches {
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() && info.ModTime().Before(cutoff) {
			_ = os.Remove(path)
		}
	}
}

func (m *Manager) rollbackRoot() string {
	return filepath.Join(m.opts.StateDir, rollbackDirName)
}

func (m *Manager) backupCurrentInstallation(targetPath string) (string, error) {
	root := m.rollbackRoot()
	if err := core.PrivateDirectory(root); err != nil {
		return "", err
	}
	stage := filepath.Join(root, fmt.Sprintf("staging-%d", time.Now().UnixNano()))
	finalPrev := filepath.Join(root, rollbackPrevName)
	backupPayload := filepath.Join(stage, filepath.Base(targetPath))
	if err := copyInstallationSnapshot(targetPath, backupPayload); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	meta := RollbackMetadata{
		Version:    m.opts.CurrentVersion,
		BuildID:    m.opts.BuildID,
		TargetPath: targetPath,
		BackupPath: filepath.Join(finalPrev, filepath.Base(targetPath)),
		SavedAt:    time.Now().UTC().Format(time.RFC3339),
		Platform:   runtime.GOOS,
	}
	if err := core.AtomicJSON(filepath.Join(stage, rollbackMetaFile), meta); err != nil {
		_ = os.RemoveAll(stage)
		return "", err
	}
	oldAside := filepath.Join(root, fmt.Sprintf("old-%d", time.Now().UnixNano()))
	if _, err := os.Stat(finalPrev); err == nil {
		if err := os.Rename(finalPrev, oldAside); err != nil {
			_ = os.RemoveAll(stage)
			return "", err
		}
	}
	if err := os.Rename(stage, finalPrev); err != nil {
		if _, statErr := os.Stat(oldAside); statErr == nil {
			_ = os.Rename(oldAside, finalPrev)
		}
		_ = os.RemoveAll(stage)
		return "", err
	}
	_ = os.RemoveAll(oldAside)
	return meta.BackupPath, nil
}

func (m *Manager) readRollbackMetadata() (RollbackMetadata, bool) {
	path := filepath.Join(m.rollbackRoot(), rollbackPrevName, rollbackMetaFile)
	raw, err := core.ReadPrivateFile(path, 64<<10)
	if err != nil {
		return RollbackMetadata{}, false
	}
	var meta RollbackMetadata
	if json.Unmarshal(raw, &meta) != nil || meta.Version == "" || meta.BackupPath == "" {
		return RollbackMetadata{}, false
	}
	if _, err := os.Stat(meta.BackupPath); err != nil {
		return RollbackMetadata{}, false
	}
	return meta, true
}

func (m *Manager) setError(operationID uint64, stage string, err error) {
	m.mu.Lock()
	if m.operationID == operationID {
		m.state = "error"
		m.lastErr = classifyUpdateError(stage, err)
	}
	m.mu.Unlock()
}

func (m *Manager) failDownload(operationID uint64, stage string, err error) {
	m.mu.Lock()
	if m.operationID == operationID {
		m.wailsUpdater = nil
		m.wailsHost = nil
		m.state = "error"
		m.lastErr = classifyUpdateError(stage, err)
	}
	m.mu.Unlock()
}

func (m *Manager) State() core.Document {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.stateLocked()
}

func (m *Manager) stateLocked() core.Document {
	history := loadHistory(m.opts.StateDir)
	rbMeta, hasRB := m.readRollbackMetadata()
	rollbackDoc := core.Document{"available": false}
	if hasRB && (strings.TrimPrefix(rbMeta.Version, "v") != strings.TrimPrefix(m.opts.CurrentVersion, "v") || (rbMeta.BuildID != "" && rbMeta.BuildID != m.opts.BuildID)) {
		rollbackDoc = core.Document{
			"available": true,
			"version":   rbMeta.Version,
			"buildId":   rbMeta.BuildID,
			"savedAt":   rbMeta.SavedAt,
		}
	}
	preferredSource := "github"
	if m.opts.FeedURL != "" {
		preferredSource = "endpoint"
	} else if m.opts.MirrorBase != "" {
		preferredSource = "cnb"
	}
	source := "github:" + m.opts.GitHubRepo
	if m.opts.FeedURL != "" {
		source = "endpoint"
	} else if m.lastSource == "cnb" || (m.lastSource == "" && m.opts.MirrorBase != "") {
		source = "cnb:" + m.opts.GitHubRepo
	}
	releaseURL := ""
	if m.available != nil && m.available.ReleaseURL != "" {
		releaseURL = m.available.ReleaseURL
	} else if m.opts.FeedURL == "" {
		if (m.lastSource == "cnb" || (m.lastSource == "" && m.opts.MirrorBase != "")) && m.opts.MirrorBase != "" {
			releaseURL = m.opts.MirrorBase + "/" + m.opts.GitHubRepo + "/-/releases"
		} else if m.opts.GitHubWebBase != "" {
			releaseURL = m.opts.GitHubWebBase + "/" + m.opts.GitHubRepo + "/releases"
		}
	}
	doc := core.Document{
		"currentVersion":  m.opts.CurrentVersion,
		"buildId":         m.opts.BuildID,
		"channel":         m.settings.Channel,
		"autoCheck":       m.settings.AutoCheck,
		"lastCheckedAt":   m.settings.LastCheckedAt,
		"state":           m.state,
		"source":          source,
		"preferredSource": preferredSource,
		"rollback":        rollbackDoc,
	}
	if m.lastSource != "" {
		doc["activeSource"] = m.lastSource
	}
	if releaseURL != "" {
		doc["releaseUrl"] = releaseURL
	}
	if m.available != nil {
		doc["available"] = m.available
	}
	if m.progress != nil {
		doc["progress"] = m.progress
	}
	if m.idleReason != "" {
		doc["idleReason"] = m.idleReason
	}
	if m.lastErr != nil {
		doc["error"] = m.lastErr
	}
	if history.LastRollback != nil {
		doc["lastRollback"] = history.LastRollback
	}
	if history.LastApplied != nil {
		doc["lastApplied"] = history.LastApplied
	}
	return doc
}

func classifyUpdateError(stage string, err error) *UpdateError {
	detail := ""
	if err != nil {
		detail = err.Error()
	}
	lower := strings.ToLower(detail)
	switch {
	case strings.Contains(lower, "public key is configured") || strings.Contains(lower, "signature") || strings.Contains(lower, "codesign"):
		return &UpdateError{
			Stage:    "verify",
			Code:     "update_signature_invalid",
			Message:  "更新包签名校验未通过，已拒绝安装；当前版本和网关未受影响。",
			NextStep: "请确认更新源未被篡改，稍后重试检查更新或从官方 Release 页面核对校验和。",
			Detail:   detail,
		}
	case strings.Contains(lower, "digest mismatch"):
		return &UpdateError{
			Stage:    "verify",
			Code:     "update_digest_mismatch",
			Message:  "更新包完整性校验不一致，已丢弃下载文件；当前版本未受影响。",
			NextStep: "请重新点击「下载并更新」重试下载。",
			Detail:   detail,
		}
	case stage == "download" || strings.Contains(lower, "eof") || strings.Contains(lower, "timeout") || strings.Contains(lower, "stalled") || strings.Contains(lower, "connection"):
		return &UpdateError{
			Stage:    stage,
			Code:     "update_network_failed",
			Message:  "下载或连接更新源中断，已清理临时文件；当前网关仍在运行。",
			NextStep: "请检查网络连接后再次点击重试。",
			Detail:   detail,
		}
	default:
		return &UpdateError{
			Stage:    stage,
			Code:     "update_failed",
			Message:  "更新操作未能完成，当前安装与网关运行保持不变。",
			NextStep: "请查看下方诊断信息后重试，或稍后手动下载新版本安装包。",
			Detail:   detail,
		}
	}
}

// managerHost bridges Wails pkg/updater lifecycle events into Manager state.
type managerHost struct {
	mgr         *Manager
	operationID uint64
}

func (h *managerHost) lockOperation() bool {
	h.mgr.mu.Lock()
	if h.operationID == 0 || h.operationID != h.mgr.operationID {
		h.mgr.mu.Unlock()
		return false
	}
	return true
}

func (h *managerHost) Emit(name string, data ...any) bool {
	switch name {
	case updater.EventDownloadProgress:
		if len(data) > 0 {
			if p, ok := data[0].(updater.Progress); ok {
				written := p.Written
				if p.Total > 0 && written > p.Total {
					written = p.Total
				}
				pct := 0
				if p.Total > 0 {
					pct = int((written * 100) / p.Total)
					if pct > 100 {
						pct = 100
					}
				}
				if h.lockOperation() {
					h.mgr.state = "downloading"
					h.mgr.progress = &DownloadProgress{
						Written: written,
						Total:   p.Total,
						Percent: pct,
						Rate:    p.Rate,
					}
					h.mgr.mu.Unlock()
				}
			}
		}
	case updater.EventVerifying:
		if h.lockOperation() {
			h.mgr.state = "verifying"
			h.mgr.mu.Unlock()
		}
	case updater.EventInstalling:
		if h.lockOperation() {
			h.mgr.state = "verifying"
			h.mgr.mu.Unlock()
		}
	}
	return true
}

func (h *managerHost) OnEvent(string, func(any)) func()                      { return func() {} }
func (h *managerHost) OpenWindow(updater.WindowOptions) updater.WindowHandle { return nil }
func (h *managerHost) Quit()                                                 {}

func resolveInstallTarget(exePath string) string {
	if exePath == "" {
		return ""
	}
	abs, err := filepath.Abs(exePath)
	if err != nil {
		abs = exePath
	}
	if runtime.GOOS == "darwin" {
		// Inside .../Kimi Work+.app/Contents/MacOS/gateway-daemon -> return .../Kimi Work+.app
		dir := abs
		for {
			if strings.HasSuffix(dir, ".app") {
				return dir
			}
			parent := filepath.Dir(dir)
			if parent == dir {
				break
			}
			dir = parent
		}
	}
	// On Windows (or non-bundle dev builds), target is the directory containing both executables.
	return filepath.Dir(abs)
}

func copyInstallationSnapshot(src, dst string) error {
	if runtime.GOOS == "windows" {
		return copyWindowsInstallationWhitelist(src, dst)
	}
	return copyAny(src, dst)
}

func copyWindowsInstallationWhitelist(srcDir, dstDir string) error {
	info, err := os.Stat(srcDir)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("windows installation target is not a directory: %s", srcDir)
	}
	if err := os.MkdirAll(dstDir, 0o755); err != nil {
		return err
	}
	required := []string{"KimiWorkPlus.exe", "gateway-daemon.exe"}
	optional := []string{"LICENSE", "THIRD_PARTY_NOTICES.txt"}
	for _, name := range required {
		srcFile := filepath.Join(srcDir, name)
		fi, err := os.Lstat(srcFile)
		if err != nil {
			return fmt.Errorf("missing required installation file %s: %w", name, err)
		}
		if !fi.Mode().IsRegular() {
			return fmt.Errorf("installation file %s is not a regular file", name)
		}
		if err := copyFileExact(srcFile, filepath.Join(dstDir, name), fi.Mode().Perm()); err != nil {
			return err
		}
	}
	for _, name := range optional {
		srcFile := filepath.Join(srcDir, name)
		fi, err := os.Lstat(srcFile)
		if err != nil || !fi.Mode().IsRegular() {
			continue
		}
		if err := copyFileExact(srcFile, filepath.Join(dstDir, name), fi.Mode().Perm()); err != nil {
			return err
		}
	}
	return nil
}

func copyAny(src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		link, err := os.Readlink(src)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
			return err
		}
		return os.Symlink(link, dst)
	}
	if info.IsDir() {
		return copyDirTree(src, dst, info.Mode().Perm())
	}
	return copyFileExact(src, dst, info.Mode().Perm())
}

func copyDirTree(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(dst, mode); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if err := copyAny(filepath.Join(src, entry.Name()), filepath.Join(dst, entry.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFileExact(src, dst string, mode os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dst, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, mode)
	if err != nil {
		return err
	}
	_, copyErr := io.Copy(out, in)
	syncErr := out.Sync()
	closeErr := out.Close()
	if copyErr != nil || syncErr != nil || closeErr != nil {
		_ = os.Remove(dst)
		if copyErr != nil {
			return copyErr
		}
		if syncErr != nil {
			return syncErr
		}
		return closeErr
	}
	return nil
}
