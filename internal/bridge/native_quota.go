package bridge

import (
	"context"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/quota"
)

type quotaSettings struct {
	BaseURL      string `json:"baseUrl"`
	ConnectionID string `json:"connectionId,omitempty"`
}

type nativeQuota struct {
	mu               sync.Mutex
	actionMu         sync.Mutex
	service          *Service
	code             *quota.CodeUsage
	magpie           *quota.Magpie
	settings         quotaSettings
	settingsReason   string
	magpieConfigured bool
	magpieKey        string
}

type visibleQuotaKeys struct{ KeyStore }

func (k visibleQuotaKeys) Set(name, value string) error {
	return setVisibleCredential(k.KeyStore, name, value)
}

func newNativeQuota(s *Service, workKey, localKey func() string) *nativeQuota {
	q := &nativeQuota{service: s, settings: quotaSettings{BaseURL: quota.MagpieDefaultBaseURL}}
	var keys quota.KeyStore
	if s.Keys != nil {
		keys = visibleQuotaKeys{s.Keys}
	}
	q.code = quota.NewCodeUsage(quota.CodeUsageOptions{Keys: keys, StateDir: filepath.Join(s.StateDir, "quota"), WorkKey: workKey, LocalKey: localKey})
	raw, err := core.ReadPrivateFile(q.settingsPath(), 8192)
	if err == nil {
		var settings quotaSettings
		if json.Unmarshal(raw, &settings) != nil {
			q.settingsReason = "quota_settings_invalid"
		} else if _, err := quota.NormalizeMagpieBase(settings.BaseURL); err != nil {
			q.settingsReason = "quota_settings_invalid"
		} else {
			q.settings = settings
			q.magpieConfigured = true
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		q.settingsReason = "quota_settings_unavailable"
	}
	q.magpie, _ = quota.NewMagpie(quota.MagpieOptions{BaseURL: q.settings.BaseURL})
	return q
}

func (q *nativeQuota) settingsPath() string {
	return filepath.Join(q.service.StateDir, "quota-settings.json")
}

func (q *nativeQuota) State() core.Document {
	q.mu.Lock()
	defer q.mu.Unlock()
	return core.Document{"codeUsage": q.code.State(), "magpie": q.magpieStateLocked()}
}

func (q *nativeQuota) magpieStateLocked() core.Document {
	raw, _ := json.Marshal(q.magpie.State())
	out := core.Document{}
	_ = json.Unmarshal(raw, &out)
	out["connectionId"] = q.settings.ConnectionID
	out["configured"] = q.magpieConfigured
	if q.settingsReason != "" {
		out["settingsReason"] = q.settingsReason
	}
	return out
}

func (q *nativeQuota) magpieOptions(settings quotaSettings) (quota.MagpieOptions, error) {
	options := quota.MagpieOptions{BaseURL: settings.BaseURL}
	if settings.ConnectionID == "" {
		return options, nil
	}
	d, _, err := q.service.Store.Read()
	if err != nil {
		return options, err
	}
	for _, raw := range core.Rows(d["connections"]) {
		row := core.Object(raw)
		if core.String(row["id"]) != settings.ConnectionID {
			continue
		}
		parsed, err := url.Parse(core.String(row["baseURL"]))
		if err != nil || (strings.TrimRight(parsed.Path, "/") != "" && strings.TrimRight(parsed.Path, "/") != "/v1") {
			return options, errors.New("invalid_magpie_connection")
		}
		options.BaseURL = parsed.Scheme + "://" + parsed.Host
		if _, err := quota.NormalizeMagpieBase(options.BaseURL); err != nil {
			return options, err
		}
		options.APIKey, err = credentialFor(row, q.service.Keys)
		return options, err
	}
	return options, errors.New("connection_unavailable")
}

func (q *nativeQuota) Action(ctx context.Context, path string, in core.Document) (any, error) {
	if path == "/api/quota/code" {
		return q.code.Action(ctx, core.String(in["action"]), in)
	}
	q.actionMu.Lock()
	defer q.actionMu.Unlock()
	q.mu.Lock()
	defer q.mu.Unlock()
	action := core.String(in["action"])
	if action == "address" {
		settings := quotaSettings{BaseURL: core.String(in["baseUrl"]), ConnectionID: core.String(in["connectionId"])}
		if settings.BaseURL == "" {
			settings.BaseURL = quota.MagpieDefaultBaseURL
		}
		options, err := q.magpieOptions(settings)
		if err != nil {
			return nil, err
		}
		reader, err := quota.NewMagpie(options)
		if err != nil {
			return nil, err
		}
		settings.BaseURL = strings.TrimSuffix(reader.Endpoint(), quota.MagpieQuotasPath)
		if err := core.AtomicJSON(q.settingsPath(), settings); err != nil {
			return nil, errors.New("quota_settings_write_failed")
		}
		q.settings, q.magpie, q.magpieKey, q.settingsReason = settings, reader, options.APIKey, ""
		q.magpieConfigured = true
		return core.Document{"state": q.magpieStateLocked()}, nil
	}
	if action == "refresh" {
		if !q.magpieConfigured {
			return core.Document{"state": q.magpieStateLocked()}, nil
		}
		options, err := q.magpieOptions(q.settings)
		if err != nil {
			q.settingsReason = core.ErrorCode(err)
			return nil, err
		}
		if options.APIKey != q.magpieKey || strings.TrimSuffix(q.magpie.Endpoint(), quota.MagpieQuotasPath) != options.BaseURL {
			reader, err := quota.NewMagpie(options)
			if err != nil {
				return nil, err
			}
			q.magpie, q.magpieKey = reader, options.APIKey
		}
		q.settingsReason = ""
	}
	reader := q.magpie
	q.mu.Unlock()
	result, err := reader.Action(ctx, action, in)
	q.mu.Lock()
	if err != nil {
		return nil, err
	}
	result["state"] = q.magpieStateLocked()
	return result, nil
}
