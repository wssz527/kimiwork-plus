package bridge

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
)

type AccountSummary struct {
	ID               string `json:"id"`
	Name             string `json:"name"`
	AvatarURL        string `json:"avatarURL,omitempty"`
	CapturedAt       int64  `json:"capturedAt"`
	LastUsedAt       int64  `json:"lastUsedAt,omitempty"`
	NeedsLogin       bool   `json:"needsLogin"`
	AccessExpiresAt  string `json:"accessExpiresAt,omitempty"`
	RefreshExpiresAt string `json:"refreshExpiresAt,omitempty"`
	CookieExpiresAt  string `json:"cookieExpiresAt,omitempty"`
	TokenHealth      string `json:"tokenHealth,omitempty"`
}

type accountRecord struct {
	AccountSummary
	CredentialRef string `json:"credentialRef"`
}

type accountIndex struct {
	Version            int             `json:"version"`
	CurrentID          string          `json:"currentId,omitempty"`
	Accounts           []accountRecord `json:"accounts"`
	RetiredCredentials []string        `json:"retiredCredentials,omitempty"`
	PendingAccountID   string          `json:"pendingAccountId,omitempty"`
	RestoreID          string          `json:"restoreId,omitempty"`
	Adding             bool            `json:"adding,omitempty"`
}

type accountCapture struct {
	UserID    string
	Name      string
	AvatarURL string
	Session   json.RawMessage
}

type accountVault struct {
	mu   sync.Mutex
	path string
	keys KeyStore
	now  func() time.Time
}

func newAccountVault(dir string, keys KeyStore) *accountVault {
	return &accountVault{path: filepath.Join(dir, "accounts.json"), keys: keys, now: time.Now}
}

func accountID(userID string) string {
	digest := sha256.Sum256([]byte(userID))
	return "account-" + hex.EncodeToString(digest[:12])
}

func (v *accountVault) read() (accountIndex, error) {
	raw, err := core.ReadPrivateFile(v.path, 256<<10)
	if errors.Is(err, os.ErrNotExist) {
		return accountIndex{Version: 1, Accounts: []accountRecord{}}, nil
	}
	if err != nil {
		return accountIndex{}, errors.New("account_store_unavailable")
	}
	var index accountIndex
	if json.Unmarshal(raw, &index) != nil || index.Version != 1 || len(index.Accounts) > 32 {
		return accountIndex{}, errors.New("account_store_invalid")
	}
	seen := map[string]bool{}
	activeRefs := map[string]bool{}
	for _, row := range index.Accounts {
		if len(row.ID) != 32 || !strings.HasPrefix(row.ID, "account-") || !validAccountCredentialRef(row.CredentialRef) || seen[row.ID] {
			return accountIndex{}, errors.New("account_store_invalid")
		}
		seen[row.ID] = true
		activeRefs[row.CredentialRef] = true
	}
	for _, ref := range index.RetiredCredentials {
		if !validAccountCredentialRef(ref) || activeRefs[ref] {
			return accountIndex{}, errors.New("account_store_invalid")
		}
	}
	if index.CurrentID != "" && !seen[index.CurrentID] || index.RestoreID != "" && !seen[index.RestoreID] || index.Adding && index.RestoreID == "" || index.PendingAccountID != "" && !seen[index.PendingAccountID] {
		return accountIndex{}, errors.New("account_store_invalid")
	}
	return index, nil
}

func validAccountCredentialRef(ref string) bool {
	const prefix = "account-session-"
	if !strings.HasPrefix(ref, prefix) || len(ref) != len(prefix)+64 {
		return false
	}
	_, err := hex.DecodeString(strings.TrimPrefix(ref, prefix))
	return err == nil
}

func (v *accountVault) cleanup(index *accountIndex) error {
	if len(index.RetiredCredentials) == 0 {
		return nil
	}
	remaining := make([]string, 0, len(index.RetiredCredentials))
	for _, ref := range index.RetiredCredentials {
		// Removal is not a user gesture: it may not open a system dialog and may
		// not wait behind one. A store that reports a pending interaction keeps
		// the reference here and the next cleanup retries it.
		if v.keys == nil || deleteCredentialBackground(v.keys, ref) != nil {
			remaining = append(remaining, ref)
		}
	}
	index.RetiredCredentials = remaining
	if err := core.AtomicJSON(v.path, index); err != nil {
		return errors.New("account_store_write_failed")
	}
	if len(remaining) > 0 {
		return errors.New("account_credential_cleanup_failed")
	}
	return nil
}

func inspectAccountSessionLifetime(raw json.RawMessage, now time.Time) kimi.AccountTokenLifetime {
	var session kimi.AccountSession
	if json.Unmarshal(raw, &session) != nil {
		return kimi.AccountTokenLifetime{TokenHealth: kimi.TokenHealthUnknown}
	}
	return kimi.InspectCredentialLifetime(session, now)
}

func applyAccountTokenLifetime(summary *AccountSummary, lifetime kimi.AccountTokenLifetime) {
	summary.AccessExpiresAt = lifetime.AccessExpiresAt
	summary.RefreshExpiresAt = lifetime.RefreshExpiresAt
	summary.CookieExpiresAt = lifetime.CookieExpiresAt
	summary.TokenHealth = lifetime.TokenHealth
}

func refreshAccountTokenHealth(summary AccountSummary, now time.Time) AccountSummary {
	summary.TokenHealth = kimi.ComputeTokenHealth(kimi.AccountTokenLifetime{
		AccessExpiresAt:  summary.AccessExpiresAt,
		RefreshExpiresAt: summary.RefreshExpiresAt,
		CookieExpiresAt:  summary.CookieExpiresAt,
	}, now)
	return summary
}

func (v *accountVault) summaries() ([]AccountSummary, string, error) {
	rows, current, _, err := v.state()
	return rows, current, err
}

func (v *accountVault) state() ([]AccountSummary, string, string, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	index, err := v.read()
	if err != nil {
		return nil, "", "", err
	}
	now := v.now()
	rows := make([]AccountSummary, 0, len(index.Accounts))
	for _, row := range index.Accounts {
		rows = append(rows, refreshAccountTokenHealth(row.AccountSummary, now))
	}
	return rows, index.CurrentID, index.PendingAccountID, nil
}

func (v *accountVault) capture(snapshot accountCapture) (AccountSummary, error) {
	return v.stage(snapshot, false)
}

// captureBackground stores a session observed without a user gesture. Its
// credential write must never open a system authorization dialog and must never
// queue behind one that is already open.
func (v *accountVault) captureBackground(snapshot accountCapture) (AccountSummary, error) {
	return v.stage(snapshot, true)
}

func (v *accountVault) stage(snapshot accountCapture, background bool) (AccountSummary, error) {
	if strings.TrimSpace(snapshot.UserID) == "" || len(snapshot.UserID) > 1024 || len(snapshot.Name) > 256 || strings.ContainsAny(snapshot.Name, "\x00\r\n") || len(snapshot.Session) == 0 || len(snapshot.Session) > 256<<10 || !json.Valid(snapshot.Session) {
		return AccountSummary{}, errors.New("account_snapshot_invalid")
	}
	v.mu.Lock()
	defer v.mu.Unlock()
	if v.keys == nil {
		return AccountSummary{}, errors.New("credentials_unavailable")
	}
	index, err := v.read()
	if err != nil {
		return AccountSummary{}, err
	}
	id := accountID(snapshot.UserID)
	position := -1
	oldRef := ""
	for i, row := range index.Accounts {
		if row.ID == id {
			position, oldRef = i, row.CredentialRef
		}
	}
	if position < 0 && len(index.Accounts) == 32 {
		return AccountSummary{}, errors.New("account_limit_reached")
	}
	name := strings.TrimSpace(snapshot.Name)
	if name == "" {
		name = "Kimi 账号 " + id[len(id)-6:]
	}
	now := v.now()
	summary := AccountSummary{ID: id, Name: name, AvatarURL: snapshot.AvatarURL, CapturedAt: now.UnixMilli()}
	applyAccountTokenLifetime(&summary, inspectAccountSessionLifetime(snapshot.Session, now))
	row := accountRecord{AccountSummary: summary, CredentialRef: "account-session-" + RandomKey()}
	if position >= 0 {
		row.LastUsedAt = index.Accounts[position].LastUsedAt
	}
	write, remove := stageCredential, v.keys.Delete
	if background {
		write, remove = stageCredentialBackground, func(ref string) error { return deleteCredentialBackground(v.keys, ref) }
	}
	// The index keeps its previous reference until the new one is written and
	// read back, so a refused or locked store leaves a usable account behind.
	if err := write(v.keys, row.CredentialRef, string(snapshot.Session)); err != nil {
		return AccountSummary{}, err
	}
	if position >= 0 {
		index.Accounts[position] = row
	} else {
		index.Accounts = append(index.Accounts, row)
	}
	index.CurrentID = id
	// A session captured from the signed-in page supersedes any offline switch
	// still waiting for a page sync.
	index.PendingAccountID = ""
	if oldRef != "" {
		index.RetiredCredentials = append(index.RetiredCredentials, oldRef)
	}
	if err := core.AtomicJSON(v.path, index); err != nil {
		_ = remove(row.CredentialRef)
		return AccountSummary{}, errors.New("account_store_write_failed")
	}
	_ = v.cleanup(&index)
	return row.AccountSummary, nil
}

func (v *accountVault) session(id string) (json.RawMessage, error) {
	return v.readSession(id, false)
}

func (v *accountVault) sessionBackground(id string) (json.RawMessage, error) {
	return v.readSession(id, true)
}

func (v *accountVault) readSession(id string, background bool) (json.RawMessage, error) {
	v.mu.Lock()
	index, err := v.read()
	v.mu.Unlock()
	if err != nil {
		return nil, err
	}
	for _, row := range index.Accounts {
		if row.ID != id {
			continue
		}
		if v.keys == nil {
			return nil, errors.New("credentials_unavailable")
		}
		var value string
		if background {
			value, err = v.keys.Get(row.CredentialRef)
		} else {
			value, err = getVisibleCredential(v.keys, row.CredentialRef)
		}
		if err != nil {
			return nil, errors.New("account_credentials_unavailable")
		}
		if len(value) == 0 || len(value) > 256<<10 || !json.Valid([]byte(value)) {
			return nil, errors.New("account_credentials_unavailable")
		}
		return json.RawMessage(value), nil
	}
	return nil, errors.New("account_not_found")
}

func (v *accountVault) pending() (string, bool, error) {
	v.mu.Lock()
	defer v.mu.Unlock()
	index, err := v.read()
	return index.RestoreID, index.Adding, err
}

func (v *accountVault) setPending(id string, adding bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	index, err := v.read()
	if err != nil {
		return err
	}
	if id != "" {
		found := false
		for _, row := range index.Accounts {
			found = found || row.ID == id
		}
		if !found {
			return errors.New("account_not_found")
		}
	}
	index.RestoreID, index.Adding = id, adding
	if err := core.AtomicJSON(v.path, index); err != nil {
		return errors.New("account_store_write_failed")
	}
	return nil
}

func (v *accountVault) updateTokenLifetime(id string, lifetime kimi.AccountTokenLifetime) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	index, err := v.read()
	if err != nil {
		return err
	}
	for i := range index.Accounts {
		if index.Accounts[i].ID != id {
			continue
		}
		row := &index.Accounts[i].AccountSummary
		if row.AccessExpiresAt == lifetime.AccessExpiresAt &&
			row.RefreshExpiresAt == lifetime.RefreshExpiresAt &&
			row.CookieExpiresAt == lifetime.CookieExpiresAt &&
			row.TokenHealth == lifetime.TokenHealth {
			return nil
		}
		applyAccountTokenLifetime(row, lifetime)
		if err := core.AtomicJSON(v.path, index); err != nil {
			return errors.New("account_store_write_failed")
		}
		return nil
	}
	return errors.New("account_not_found")
}

func (v *accountVault) mark(id string, needsLogin bool) error {
	return v.markActivation(id, needsLogin, false)
}

func (v *accountVault) markActivation(id string, needsLogin, offline bool) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	index, err := v.read()
	if err != nil {
		return err
	}
	for i := range index.Accounts {
		if index.Accounts[i].ID == id {
			index.Accounts[i].NeedsLogin = needsLogin
			if needsLogin {
				// Rejected credentials end the switch: a lingering pending id
				// would re-run the page sync every few seconds.
				if index.PendingAccountID == id {
					index.PendingAccountID = ""
				}
			} else {
				index.CurrentID = id
				index.PendingAccountID = ""
				if offline {
					index.PendingAccountID = id
				}
				index.Accounts[i].LastUsedAt = v.now().UnixMilli()
			}
			if err := core.AtomicJSON(v.path, index); err != nil {
				return errors.New("account_store_write_failed")
			}
			return nil
		}
	}
	return errors.New("account_not_found")
}

func (v *accountVault) remove(id string) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	index, err := v.read()
	if err != nil {
		return err
	}
	if err := v.cleanup(&index); err != nil {
		return err
	}
	if id == index.CurrentID || id == index.RestoreID || id == index.PendingAccountID {
		return errors.New("account_current_in_use")
	}
	for i, row := range index.Accounts {
		if row.ID == id {
			index.Accounts = append(index.Accounts[:i], index.Accounts[i+1:]...)
			index.RetiredCredentials = append(index.RetiredCredentials, row.CredentialRef)
			if err := core.AtomicJSON(v.path, index); err != nil {
				return errors.New("account_store_write_failed")
			}
			return v.cleanup(&index)
		}
	}
	return nil
}
