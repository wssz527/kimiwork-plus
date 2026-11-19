package appupdate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/endpoint"
)

type githubRelease struct {
	TagName    string        `json:"tag_name"`
	Name       string        `json:"name"`
	Body       string        `json:"body"`
	Draft      bool          `json:"draft"`
	Prerelease bool          `json:"prerelease"`
	Assets     []githubAsset `json:"assets"`
}

type githubAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
	Size               int64  `json:"size"`
}

// githubManifestProvider locates update-manifest.json on GitHub Releases for
// either the stable or preview channel, then delegates manifest parsing and
// artifact download to Wails's standard endpoint provider so Ed25519
// signatures are verified fail-closed by pkg/updater.
type githubManifestProvider struct {
	repo       string
	apiBase    string
	channel    string
	httpClient *http.Client
}

func newGitHubManifestProvider(repo, apiBase, channel string, client *http.Client) (*githubManifestProvider, error) {
	repo = strings.TrimSpace(repo)
	if !strings.Contains(repo, "/") {
		return nil, fmt.Errorf("invalid github repository %q", repo)
	}
	base := strings.TrimRight(strings.TrimSpace(apiBase), "/")
	if base == "" {
		base = DefaultGitHubAPIBase
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	ch := normalizeChannel(channel)
	return &githubManifestProvider{
		repo:       repo,
		apiBase:    base,
		channel:    ch,
		httpClient: client,
	}, nil
}

func (p *githubManifestProvider) Name() string { return "github-releases" }

func (p *githubManifestProvider) Check(ctx context.Context, req updater.CheckRequest) (*updater.Release, error) {
	manifestURL, err := p.resolveManifestAssetURL(ctx)
	if err != nil {
		return nil, err
	}
	if manifestURL == "" {
		return nil, nil
	}
	ep, err := endpoint.New(endpoint.Config{
		URL:        manifestURL,
		Channel:    p.channel,
		HTTPClient: p.httpClient,
	})
	if err != nil {
		return nil, err
	}
	rel, err := ep.Check(ctx, req)
	if err != nil || rel == nil {
		return rel, err
	}
	if rel.Metadata == nil {
		rel.Metadata = map[string]any{}
	}
	rel.Metadata["github.manifest.url"] = manifestURL
	return rel, nil
}

func (p *githubManifestProvider) Download(ctx context.Context, r *updater.Release, dst io.Writer, onProgress func(written, total int64)) error {
	manifestURL, _ := r.Metadata["github.manifest.url"].(string)
	if manifestURL == "" {
		return errors.New("github-releases: missing manifest url")
	}
	ep, err := endpoint.New(endpoint.Config{
		URL:        manifestURL,
		Channel:    p.channel,
		HTTPClient: p.httpClient,
	})
	if err != nil {
		return err
	}
	return ep.Download(ctx, r, dst, onProgress)
}

func (p *githubManifestProvider) resolveManifestAssetURL(ctx context.Context) (string, error) {
	releasesURL := p.apiBase + "/repos/" + p.repo + "/releases/latest"
	if p.channel == "preview" {
		releasesURL = p.apiBase + "/repos/" + p.repo + "/releases?per_page=10"
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("User-Agent", "KimiWorkPlus-Updater")

	resp, err := p.httpClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("github-releases: fetch releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	rateLimited := resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""))
	if rateLimited && p.channel == "stable" && p.apiBase == DefaultGitHubAPIBase {
		return "https://github.com/" + p.repo + "/releases/latest/download/update-manifest.json", nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", fmt.Errorf("github-releases: HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return "", err
	}

	var list []githubRelease
	if p.channel == "preview" {
		if err := json.Unmarshal(body, &list); err != nil {
			return "", fmt.Errorf("github-releases: decode release list: %w", err)
		}
	} else {
		var single githubRelease
		if err := json.Unmarshal(body, &single); err != nil {
			return "", fmt.Errorf("github-releases: decode latest release: %w", err)
		}
		list = []githubRelease{single}
	}

	preferred := []string{"update-manifest-" + p.channel + ".json", "update-manifest.json"}
	for _, rel := range list {
		if rel.Draft {
			continue
		}
		if p.channel == "stable" && rel.Prerelease {
			continue
		}
		for _, wantName := range preferred {
			for _, asset := range rel.Assets {
				if strings.EqualFold(asset.Name, wantName) && asset.BrowserDownloadURL != "" {
					return asset.BrowserDownloadURL, nil
				}
			}
		}
	}
	return "", nil
}

func normalizeChannel(ch string) string {
	switch strings.ToLower(strings.TrimSpace(ch)) {
	case "preview", "beta", "prerelease":
		return "preview"
	default:
		return "stable"
	}
}
