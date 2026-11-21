package bridge

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/platform"
)

const migrationStateFile = "migration-preview.json"

type PreviewMigrationState struct {
	Version               int               `json:"version"`
	Status                string            `json:"status"`
	SourceDir             string            `json:"sourceDir,omitempty"`
	SourceNamespace       string            `json:"sourceNamespace,omitempty"`
	TargetNamespace       string            `json:"targetNamespace,omitempty"`
	StateFilesCopied      bool              `json:"stateFilesCopied"`
	CopiedFiles           []string          `json:"copiedFiles,omitempty"`
	CredentialsCompleted  bool              `json:"credentialsCompleted"`
	MigratedCredentials   []string          `json:"migratedCredentials,omitempty"`
	PendingCredentials    []string          `json:"pendingCredentials,omitempty"`
	FailedCredentials     map[string]string `json:"failedCredentials,omitempty"`
	LegacyServiceMigrated bool              `json:"legacyServiceMigrated"`
	LegacyDirPreserved    bool              `json:"legacyDirPreserved"`
	StartedAt             string            `json:"startedAt,omitempty"`
	CompletedAt           string            `json:"completedAt,omitempty"`
}

func ReadMigrationState(dir string) (PreviewMigrationState, bool, error) {
	path := filepath.Join(dir, migrationStateFile)
	raw, err := core.ReadPrivateFile(path, 512<<10)
	if errors.Is(err, os.ErrNotExist) {
		return PreviewMigrationState{}, false, nil
	}
	if err != nil {
		return PreviewMigrationState{}, false, err
	}
	var state PreviewMigrationState
	if err := json.Unmarshal(raw, &state); err != nil {
		return PreviewMigrationState{}, false, err
	}
	if state.FailedCredentials == nil {
		state.FailedCredentials = map[string]string{}
	}
	return state, true, nil
}

func saveMigrationState(dir string, state PreviewMigrationState) error {
	state.Version = 1
	return core.AtomicJSON(filepath.Join(dir, migrationStateFile), state)
}

func isTransientMigrationEntry(rel string) bool {
	parts := strings.Split(filepath.ToSlash(rel), "/")
	if len(parts) == 0 {
		return true
	}
	top := parts[0]
	switch top {
	case "daemon.lock", "daemon.log", "service-definition.json",
		"service-operations", "service", "secrets", migrationStateFile, ".DS_Store":
		return true
	}
	base := parts[len(parts)-1]
	if base == ".DS_Store" || strings.HasSuffix(base, ".tmp") {
		return true
	}
	return false
}

func hasPersistentState(dir string) bool {
	for _, name := range []string{"gateway.json", "accounts.json", "gateway-token.json", "quota-settings.json", "web-tools-settings.json", "shared-instructions.md"} {
		if info, err := os.Lstat(filepath.Join(dir, name)); err == nil && info.Mode().IsRegular() {
			return true
		}
	}
	if info, err := os.Lstat(filepath.Join(dir, "usage", "ledger.jsonl")); err == nil && info.Mode().IsRegular() {
		return true
	}
	return false
}

func MigrateStateDir(targetDir, legacyDir string) (PreviewMigrationState, error) {
	if targetDir == "" {
		return PreviewMigrationState{Version: 1, Status: "not_needed"}, nil
	}
	existing, found, err := ReadMigrationState(targetDir)
	if err != nil {
		return PreviewMigrationState{}, err
	}
	if found && existing.StateFilesCopied {
		return existing, nil
	}
	if legacyDir == "" {
		return PreviewMigrationState{Version: 1, Status: "not_needed"}, nil
	}
	absTarget, err := filepath.Abs(targetDir)
	if err != nil {
		return PreviewMigrationState{}, err
	}
	absLegacy, err := filepath.Abs(legacyDir)
	if err != nil {
		return PreviewMigrationState{}, err
	}
	if absTarget == absLegacy {
		return PreviewMigrationState{Version: 1, Status: "not_needed"}, nil
	}
	// If target already has independent state and no in-progress migration marker, never overwrite.
	if !found && hasPersistentState(absTarget) {
		return PreviewMigrationState{Version: 1, Status: "not_needed"}, nil
	}
	if !hasPersistentState(absLegacy) {
		return PreviewMigrationState{Version: 1, Status: "not_needed"}, nil
	}

	if err := core.PrivateDirectory(absTarget); err != nil {
		return PreviewMigrationState{}, err
	}
	// Stop any running legacy preview daemon so files are quiescent and two daemons do not coexist.
	if _, epErr := ReadEndpoint(absLegacy); epErr == nil {
		_ = StopDaemon(absLegacy)
	}

	state := existing
	if !found {
		state = PreviewMigrationState{
			Version:            1,
			Status:             "in_progress",
			SourceDir:          absLegacy,
			SourceNamespace:    LegacyPreviewCredentialNamespace(absLegacy),
			TargetNamespace:    CredentialNamespace(absTarget),
			LegacyDirPreserved: true,
			StartedAt:          time.Now().UTC().Format(time.RFC3339),
			FailedCredentials:  map[string]string{},
		}
		if err := saveMigrationState(absTarget, state); err != nil {
			return PreviewMigrationState{}, err
		}
	}

	copiedSet := map[string]bool{}
	for _, rel := range state.CopiedFiles {
		copiedSet[rel] = true
	}

	var files []string
	walkErr := filepath.WalkDir(absLegacy, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if path == absLegacy {
			return nil
		}
		rel, err := filepath.Rel(absLegacy, path)
		if err != nil {
			return err
		}
		slashRel := filepath.ToSlash(rel)
		if isTransientMigrationEntry(slashRel) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if entry.IsDir() {
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		files = append(files, slashRel)
		return nil
	})
	if walkErr != nil {
		return state, walkErr
	}
	sort.Strings(files)

	for _, rel := range files {
		if copiedSet[rel] {
			continue
		}
		src := filepath.Join(absLegacy, filepath.FromSlash(rel))
		dst := filepath.Join(absTarget, filepath.FromSlash(rel))
		if err := copyPrivateFileAtomic(src, dst); err != nil {
			return state, err
		}
		state.CopiedFiles = append(state.CopiedFiles, rel)
		copiedSet[rel] = true
		if err := saveMigrationState(absTarget, state); err != nil {
			return state, err
		}
	}

	state.StateFilesCopied = true
	state.LegacyDirPreserved = true
	if err := saveMigrationState(absTarget, state); err != nil {
		return state, err
	}
	return state, nil
}

func copyPrivateFileAtomic(src, dst string) error {
	if err := core.PrivateDirectory(filepath.Dir(dst)); err != nil {
		return err
	}
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	tmp := dst + ".migrate.tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return err
	}
	if runtime.GOOS != "windows" {
		_ = os.Chmod(tmp, 0o600)
	}
	if err := os.Rename(tmp, dst); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return nil
}

func CollectCredentialRefs(dir string) []string {
	refs := map[string]bool{}
	collectFromConfig := func(path string) {
		raw, err := core.ReadPrivateFile(path, 4<<20)
		if err != nil {
			return
		}
		var doc core.Document
		if json.Unmarshal(raw, &doc) != nil {
			return
		}
		for _, v := range core.Rows(doc["connections"]) {
			c := core.Object(v)
			if ref := credentialReference(c); ref != "" {
				refs[ref] = true
			}
			if env := core.String(c["apiKeyEnv"]); env != "" {
				refs[env] = true
			}
		}
		if tr := core.Object(doc["translation"]); tr != nil {
			if ref := core.String(tr["apiKeyRef"]); ref != "" {
				refs[ref] = true
			}
			if env := core.String(tr["apiKeyEnv"]); env != "" {
				refs[env] = true
			}
		}
	}

	collectFromConfig(filepath.Join(dir, "gateway.json"))
	if raw, err := core.ReadPrivateFile(filepath.Join(dir, "gateway.json.journal"), 4<<20); err == nil {
		var pending struct {
			After core.Document `json:"after"`
		}
		if json.Unmarshal(raw, &pending) == nil && pending.After != nil {
			collectCredentialRefs(pending.After, refs)
		}
	}
	if raw, err := core.ReadPrivateFile(filepath.Join(dir, "accounts.json"), 256<<10); err == nil {
		var index accountIndex
		if json.Unmarshal(raw, &index) == nil {
			for _, row := range index.Accounts {
				if row.CredentialRef != "" {
					refs[row.CredentialRef] = true
				}
			}
		}
	}
	if ref := currentWebCredentialRef(filepath.Join(dir, "web-tools-settings.json")); ref != "" {
		refs[ref] = true
	}
	refs["kimi-code:manual"] = true

	list := make([]string, 0, len(refs))
	for ref := range refs {
		if strings.TrimSpace(ref) != "" {
			list = append(list, ref)
		}
	}
	sort.Strings(list)
	return list
}

func ShouldAttemptInPlaceCredentialMigration(targetDir string, newStore KeyStore) bool {
	if newStore == nil || !hasPersistentState(targetDir) {
		return false
	}
	if existing, found, err := ReadMigrationState(targetDir); err == nil && found && existing.CredentialsCompleted {
		return false
	}
	for _, ref := range CollectCredentialRefs(targetDir) {
		if ref == "kimi-code:manual" {
			continue
		}
		val, err := newStore.Get(ref)
		if errors.Is(err, platform.ErrCredentialMissing) || (err == nil && val == "") {
			return true
		}
	}
	return false
}

func MigrateCredentials(targetDir, sourceDir string, oldStore, newStore KeyStore, interactive bool) (PreviewMigrationState, error) {
	if oldStore == nil || newStore == nil {
		return PreviewMigrationState{}, errors.New("credentials_unavailable")
	}
	state, found, err := ReadMigrationState(targetDir)
	if err != nil {
		return PreviewMigrationState{}, err
	}
	if found && state.CredentialsCompleted {
		return state, nil
	}
	if !found {
		state = PreviewMigrationState{
			Version:            1,
			Status:             "in_progress",
			SourceDir:          sourceDir,
			LegacyDirPreserved: sourceDir != "" && sourceDir != targetDir,
			StartedAt:          time.Now().UTC().Format(time.RFC3339),
			FailedCredentials:  map[string]string{},
		}
	}
	if state.SourceDir == "" {
		state.SourceDir = sourceDir
	}
	state.SourceNamespace = LegacyPreviewCredentialNamespace(state.SourceDir)
	state.TargetNamespace = CredentialNamespace(targetDir)
	if state.FailedCredentials == nil {
		state.FailedCredentials = map[string]string{}
	}

	migratedSet := map[string]bool{}
	for _, ref := range state.MigratedCredentials {
		migratedSet[ref] = true
	}

	refs := CollectCredentialRefs(targetDir)
	for _, ref := range refs {
		if migratedSet[ref] {
			delete(state.FailedCredentials, ref)
			continue
		}
		if existingVal, existingErr := readBackCredential(newStore, ref); existingErr == nil && existingVal != "" {
			state.MigratedCredentials = append(state.MigratedCredentials, ref)
			migratedSet[ref] = true
			delete(state.FailedCredentials, ref)
			_ = saveMigrationState(targetDir, state)
			continue
		}

		var value string
		var readErr error
		if interactive {
			value, readErr = getVisibleCredential(oldStore, ref)
		} else {
			value, readErr = oldStore.Get(ref)
		}
		if errors.Is(readErr, platform.ErrCredentialMissing) || (readErr == nil && value == "") {
			delete(state.FailedCredentials, ref)
			continue
		}
		if readErr != nil {
			state.FailedCredentials[ref] = core.ErrorCode(readErr)
			_ = saveMigrationState(targetDir, state)
			continue
		}

		var writeErr error
		if interactive {
			writeErr = setVisibleCredential(newStore, ref, value)
		} else {
			writeErr = newStore.Set(ref, value)
		}
		if writeErr != nil {
			state.FailedCredentials[ref] = core.ErrorCode(writeErr)
			_ = saveMigrationState(targetDir, state)
			continue
		}

		verified, verifyErr := readBackCredential(newStore, ref)
		if verifyErr != nil || verified != value {
			state.FailedCredentials[ref] = "credential_migration_verify_failed"
			_ = saveMigrationState(targetDir, state)
			continue
		}

		state.MigratedCredentials = append(state.MigratedCredentials, ref)
		migratedSet[ref] = true
		delete(state.FailedCredentials, ref)
		if err := saveMigrationState(targetDir, state); err != nil {
			return state, err
		}
	}

	sort.Strings(state.MigratedCredentials)
	state.MigratedCredentials = slices.Compact(state.MigratedCredentials)
	pending := make([]string, 0, len(state.FailedCredentials))
	needsAuth := false
	for ref, code := range state.FailedCredentials {
		pending = append(pending, ref)
		switch code {
		case "credential_authorization_required", "credential_authorization_failed", "credential_session_unavailable", "credential_authorization_cancelled":
			needsAuth = true
		}
	}
	sort.Strings(pending)
	state.PendingCredentials = pending

	if len(state.FailedCredentials) == 0 {
		state.CredentialsCompleted = true
		state.Status = "completed"
		state.CompletedAt = time.Now().UTC().Format(time.RFC3339)
	} else if needsAuth {
		state.CredentialsCompleted = false
		state.Status = "needs_authorization"
	} else {
		state.CredentialsCompleted = false
		state.Status = "in_progress"
	}

	if found || state.StateFilesCopied || len(state.MigratedCredentials) > 0 || len(state.FailedCredentials) > 0 {
		if err := saveMigrationState(targetDir, state); err != nil {
			return state, err
		}
	}
	return state, nil
}

func MarkLegacyServiceMigrated(targetDir string) error {
	state, found, err := ReadMigrationState(targetDir)
	if err != nil || !found {
		return err
	}
	state.LegacyServiceMigrated = true
	return saveMigrationState(targetDir, state)
}

type MigrationCoordinator struct {
	mu        sync.Mutex
	targetDir string
	sourceDir string
	oldKeys   KeyStore
	newKeys   KeyStore
	last      PreviewMigrationState
	known     bool
}

func NewMigrationCoordinator(targetDir, sourceDir string, oldKeys, newKeys KeyStore) *MigrationCoordinator {
	state, found, _ := ReadMigrationState(targetDir)
	return &MigrationCoordinator{
		targetDir: targetDir,
		sourceDir: sourceDir,
		oldKeys:   oldKeys,
		newKeys:   newKeys,
		last:      state,
		known:     found,
	}
}

func (c *MigrationCoordinator) MigrateStartup() (PreviewMigrationState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	state, err := MigrateCredentials(c.targetDir, c.sourceDir, c.oldKeys, c.newKeys, false)
	if err == nil {
		c.last = state
		c.known = state.Status != "" && state.Status != "not_needed"
	}
	return state, err
}

func (c *MigrationCoordinator) ResumeInteractive() (PreviewMigrationState, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.oldKeys == nil && c.sourceDir != "" {
		oldCreds, err := platform.OpenCredentials(LegacyPreviewCredentialNamespace(c.sourceDir))
		if err != nil {
			return c.last, err
		}
		c.oldKeys = CacheKeys(oldCreds)
	}
	state, err := MigrateCredentials(c.targetDir, c.sourceDir, c.oldKeys, c.newKeys, true)
	if err == nil {
		c.last = state
		c.known = state.Status != "" && state.Status != "not_needed"
	}
	return state, err
}

func (c *MigrationCoordinator) Status() core.Document {
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.known {
		if state, found, err := ReadMigrationState(c.targetDir); err == nil && found {
			c.last = state
			c.known = true
		}
	}
	if !c.known {
		return core.Document{"status": "not_needed"}
	}
	return core.Document{
		"status":                c.last.Status,
		"stateFilesCopied":      c.last.StateFilesCopied,
		"credentialsCompleted":  c.last.CredentialsCompleted,
		"migratedCount":         len(c.last.MigratedCredentials),
		"pendingCount":          len(c.last.PendingCredentials),
		"legacyServiceMigrated": c.last.LegacyServiceMigrated,
		"legacyDirPreserved":    c.last.LegacyDirPreserved,
		"sourceDir":             c.last.SourceDir,
	}
}
