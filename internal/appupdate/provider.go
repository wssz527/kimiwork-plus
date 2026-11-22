package appupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"time"

	"github.com/wailsapp/wails/v3/pkg/updater"
	"github.com/wailsapp/wails/v3/pkg/updater/providers/endpoint"
)

const (
	metaUpdateSource             = "kwp.update.source"
	metaUpdateReleaseURL         = "kwp.update.release_url"
	metaUpdateDownloadCandidates = "kwp.update.download_candidates"

	defaultCheckSourceTimeout            = 10 * time.Second
	defaultDownloadStallTimeout          = 35 * time.Second
	defaultDownloadResponseHeaderTimeout = 20 * time.Second
	manifestBodyMaxBytes                 = 8 << 20
	manifestSchemaVersionMax             = 1
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

type manifestHeader struct {
	SchemaVersion int    `json:"schemaVersion"`
	Version       string `json:"version"`
}

type mirrorProviderConfig struct {
	Repo              string
	MirrorBase        string
	GitHubWebBase     string
	GitHubAPIBase     string
	UseCustomAPI      bool
	ExplicitGitHubWeb bool
	Channel           string
	HTTPClient        *http.Client
	CheckTimeout      time.Duration
	StallTimeout      time.Duration
	OnSourceActive    func(source string)
}

// mirrorFallbackProvider coordinates CNB mirror-first checks and downloads with
// GitHub fallback inside a single updater.Provider so Wails pkg/updater never
// short-circuits on a mirror miss or locks downloads to a failed mirror while
// keeping fail-closed Ed25519 + SHA-256 verification intact.
type mirrorFallbackProvider struct {
	repo              string
	mirrorBase        string
	githubWebBase     string
	githubAPIBase     string
	useCustomAPI      bool
	explicitGitHubWeb bool
	channel           string
	httpClient        *http.Client
	checkTimeout      time.Duration
	stallTimeout      time.Duration
	onSourceActive    func(source string)
}

func newMirrorFallbackProvider(cfg mirrorProviderConfig) (*mirrorFallbackProvider, error) {
	repo := strings.TrimSpace(cfg.Repo)
	if !strings.Contains(repo, "/") {
		return nil, fmt.Errorf("invalid github repository %q", repo)
	}
	apiBase := strings.TrimRight(strings.TrimSpace(cfg.GitHubAPIBase), "/")
	if apiBase == "" {
		apiBase = strings.TrimRight(DefaultGitHubAPIBase, "/")
	}
	webBase := strings.TrimRight(strings.TrimSpace(cfg.GitHubWebBase), "/")
	if webBase == "" {
		webBase = strings.TrimRight(DefaultGitHubWebBase, "/")
	}
	mirrorBase := strings.TrimRight(strings.TrimSpace(cfg.MirrorBase), "/")
	client := cfg.HTTPClient
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	checkTimeout := cfg.CheckTimeout
	if checkTimeout <= 0 {
		checkTimeout = defaultCheckSourceTimeout
	}
	stallTimeout := cfg.StallTimeout
	if stallTimeout <= 0 {
		stallTimeout = defaultDownloadStallTimeout
	}
	return &mirrorFallbackProvider{
		repo:              repo,
		mirrorBase:        mirrorBase,
		githubWebBase:     webBase,
		githubAPIBase:     apiBase,
		useCustomAPI:      cfg.UseCustomAPI,
		explicitGitHubWeb: cfg.ExplicitGitHubWeb,
		channel:           normalizeChannel(cfg.Channel),
		httpClient:        client,
		checkTimeout:      checkTimeout,
		stallTimeout:      stallTimeout,
		onSourceActive:    cfg.OnSourceActive,
	}, nil
}

func (p *mirrorFallbackProvider) Name() string { return "mirror-fallback" }

func (p *mirrorFallbackProvider) recordActiveSource(source string) {
	if source != "" && p.onSourceActive != nil {
		p.onSourceActive(source)
	}
}

func (p *mirrorFallbackProvider) Check(ctx context.Context, req updater.CheckRequest) (*updater.Release, error) {
	if !p.useCustomAPI && p.channel == "stable" {
		return p.checkStableDirect(ctx, req)
	}
	return p.checkViaGitHubAPI(ctx, req)
}

func (p *mirrorFallbackProvider) checkStableDirect(ctx context.Context, req updater.CheckRequest) (*updater.Release, error) {
	type checkSource struct {
		name        string
		manifestURL string
	}
	var sources []checkSource
	if p.mirrorBase != "" {
		sources = append(sources, checkSource{
			name:        "cnb",
			manifestURL: p.mirrorBase + "/" + p.repo + "/-/releases/latest/download/update-manifest.json",
		})
	}
	if p.githubWebBase != "" {
		sources = append(sources, checkSource{
			name:        "github",
			manifestURL: p.githubWebBase + "/" + p.repo + "/releases/latest/download/update-manifest.json",
		})
	}
	if len(sources) == 0 {
		return nil, errors.New("mirror-fallback: no update check source configured")
	}

	var errs []string
	for _, src := range sources {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		rel, err := p.fetchDirectManifest(ctx, src.name, src.manifestURL, req)
		if err != nil {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			errs = append(errs, fmt.Sprintf("%s: %v", src.name, err))
			continue
		}
		p.recordActiveSource(src.name)
		if rel == nil {
			return nil, nil
		}
		p.populateReleaseMetadata(rel, src.name)
		return rel, nil
	}
	return nil, fmt.Errorf("update check failed across all sources (%s)", strings.Join(errs, "; "))
}

func (p *mirrorFallbackProvider) checkViaGitHubAPI(ctx context.Context, req updater.CheckRequest) (*updater.Release, error) {
	manifestURL, err := p.resolveGitHubAPIManifestURL(ctx)
	if err != nil {
		return nil, err
	}
	if manifestURL == "" {
		p.recordActiveSource("github")
		return nil, nil
	}
	rel, err := p.fetchDirectManifest(ctx, "github", manifestURL, req)
	if err != nil {
		return nil, err
	}
	p.recordActiveSource("github")
	if rel == nil {
		return nil, nil
	}
	p.populateReleaseMetadata(rel, "github")
	return rel, nil
}

func (p *mirrorFallbackProvider) fetchDirectManifest(ctx context.Context, sourceName, manifestURL string, req updater.CheckRequest) (*updater.Release, error) {
	srcCtx, cancel := withTimeoutIfPositive(ctx, p.checkTimeout)
	defer cancel()

	httpReq, err := http.NewRequestWithContext(srcCtx, http.MethodGet, manifestURL, nil)
	if err != nil {
		return nil, err
	}
	httpReq.Header.Set("Accept", "application/json")
	httpReq.Header.Set("User-Agent", "KimiWorkPlus-Updater")

	checkClient := wrapStripAuthOnRedirect(p.httpClient)
	resp, err := checkClient.Do(httpReq)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, manifestBodyMaxBytes+1))
	if err != nil {
		return nil, err
	}
	if len(body) > manifestBodyMaxBytes {
		return nil, errors.New("manifest exceeds size limit")
	}

	var hdr manifestHeader
	if err := json.Unmarshal(body, &hdr); err != nil {
		return nil, fmt.Errorf("invalid manifest JSON: %w", err)
	}
	if hdr.SchemaVersion > manifestSchemaVersionMax {
		return nil, fmt.Errorf("unsupported manifest schemaVersion %d", hdr.SchemaVersion)
	}
	if strings.TrimSpace(hdr.Version) == "" {
		return nil, errors.New("manifest missing version")
	}

	// Delegate semver comparison, artifact selection, relative URL resolution,
	// and verification parsing to Wails's endpoint provider without issuing a
	// second network request.
	memClient := &http.Client{
		Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"application/json"}},
				Body:       io.NopCloser(bytes.NewReader(body)),
				Request:    r,
			}, nil
		}),
	}
	ep, err := endpoint.New(endpoint.Config{
		URL:        manifestURL,
		Channel:    p.channel,
		HTTPClient: memClient,
	})
	if err != nil {
		return nil, err
	}
	rel, err := ep.Check(ctx, req)
	if err != nil {
		return nil, err
	}
	if rel != nil {
		if rel.Metadata == nil {
			rel.Metadata = map[string]any{}
		}
		rel.Metadata["github.manifest.url"] = manifestURL
		rel.Metadata[metaUpdateSource] = sourceName
	}
	return rel, nil
}

func (p *mirrorFallbackProvider) populateReleaseMetadata(rel *updater.Release, sourceName string) {
	if rel.Metadata == nil {
		rel.Metadata = map[string]any{}
	}
	rel.Metadata[metaUpdateSource] = sourceName
	if releaseURL := p.releaseTagURL(sourceName, rel.Version); releaseURL != "" {
		rel.Metadata[metaUpdateReleaseURL] = releaseURL
	}
	rel.Metadata[metaUpdateDownloadCandidates] = p.buildDownloadCandidates(rel)
}

func (p *mirrorFallbackProvider) releaseTagURL(sourceName, version string) string {
	ver := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if ver == "" {
		return ""
	}
	tag := "v" + ver
	if sourceName == "cnb" && p.mirrorBase != "" {
		return p.mirrorBase + "/" + p.repo + "/-/releases/tag/" + tag
	}
	if p.githubWebBase != "" {
		return p.githubWebBase + "/" + p.repo + "/releases/tag/" + tag
	}
	return ""
}

func (p *mirrorFallbackProvider) buildDownloadCandidates(rel *updater.Release) []string {
	var candidates []string
	seen := map[string]bool{}
	add := func(u string) {
		u = strings.TrimSpace(u)
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		candidates = append(candidates, u)
	}

	ver := strings.TrimPrefix(strings.TrimSpace(rel.Version), "v")
	filename := strings.TrimSpace(rel.Artifact.Filename)
	if filename != "" {
		filename = filepath.Base(filename)
	}
	if ver != "" && filename != "" && filename != "." && filename != "/" {
		tag := "v" + ver
		escaped := url.PathEscape(filename)
		if p.mirrorBase != "" {
			add(p.mirrorBase + "/" + p.repo + "/-/releases/download/" + tag + "/" + escaped)
		}
		if p.githubWebBase != "" && (!p.useCustomAPI || p.explicitGitHubWeb) {
			add(p.githubWebBase + "/" + p.repo + "/releases/download/" + tag + "/" + escaped)
		}
	}
	if rawURL, _ := rel.Metadata["endpoint.artifact.url"].(string); rawURL != "" {
		add(rawURL)
	}
	return candidates
}

func (p *mirrorFallbackProvider) sourceForURL(u string) string {
	if p.mirrorBase != "" && strings.HasPrefix(u, p.mirrorBase+"/") {
		return "cnb"
	}
	if p.githubWebBase != "" && strings.HasPrefix(u, p.githubWebBase+"/") {
		return "github"
	}
	if strings.Contains(strings.ToLower(u), "cnb.cool") {
		return "cnb"
	}
	return "github"
}

func (p *mirrorFallbackProvider) Download(ctx context.Context, r *updater.Release, dst io.Writer, onProgress func(written, total int64)) error {
	if r == nil || r.Metadata == nil {
		return errors.New("mirror-fallback: release missing metadata")
	}
	candidates := extractDownloadCandidates(r.Metadata)
	if len(candidates) == 0 {
		return errors.New("mirror-fallback: missing artifact download url")
	}

	dlClient, clonedTransport := cloneDownloadClient(p.httpClient)
	if clonedTransport != nil {
		defer clonedTransport.CloseIdleConnections()
	}

	if len(candidates) == 1 {
		if err := downloadURLToWriter(ctx, dlClient, candidates[0], r.Artifact.Size, false, p.stallTimeout, dst, onProgress); err != nil {
			return err
		}
		p.recordActiveSource(p.sourceForURL(candidates[0]))
		return nil
	}

	var errs []string
	for _, candidateURL := range candidates {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		tmpPath, err := downloadCandidateToTemp(ctx, dlClient, candidateURL, r.Artifact.Size, p.stallTimeout, onProgress)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			errs = append(errs, err.Error())
			continue
		}
		copyErr := copyTempFileToWriter(tmpPath, dst)
		_ = os.Remove(tmpPath)
		if copyErr != nil {
			return copyErr
		}
		p.recordActiveSource(p.sourceForURL(candidateURL))
		return nil
	}
	return fmt.Errorf("download failed across all candidates (%s)", strings.Join(errs, "; "))
}

func (p *mirrorFallbackProvider) resolveGitHubAPIManifestURL(ctx context.Context) (string, error) {
	srcCtx, cancel := withTimeoutIfPositive(ctx, p.checkTimeout)
	defer cancel()

	releasesURL := p.githubAPIBase + "/repos/" + p.repo + "/releases/latest"
	if p.channel == "preview" {
		releasesURL = p.githubAPIBase + "/repos/" + p.repo + "/releases?per_page=10"
	}
	httpReq, err := http.NewRequestWithContext(srcCtx, http.MethodGet, releasesURL, nil)
	if err != nil {
		return "", err
	}
	httpReq.Header.Set("Accept", "application/vnd.github+json")
	httpReq.Header.Set("User-Agent", "KimiWorkPlus-Updater")

	checkClient := wrapStripAuthOnRedirect(p.httpClient)
	resp, err := checkClient.Do(httpReq)
	if err != nil {
		return "", fmt.Errorf("github-releases: fetch releases: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode == http.StatusNotFound {
		return "", nil
	}
	rateLimited := resp.StatusCode == http.StatusTooManyRequests ||
		(resp.StatusCode == http.StatusForbidden && (resp.Header.Get("X-RateLimit-Remaining") == "0" || resp.Header.Get("Retry-After") != ""))
	if rateLimited && p.channel == "stable" && !p.useCustomAPI {
		return p.githubWebBase + "/" + p.repo + "/releases/latest/download/update-manifest.json", nil
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

// feedEndpointProvider wraps a custom FeedURL manifest provider so Check uses
// the caller's HTTPClient while Download disables total client timeout and
// enforces stall timeout via stallTimeoutReader.
type feedEndpointProvider struct {
	feedURL        string
	channel        string
	httpClient     *http.Client
	stallTimeout   time.Duration
	onSourceActive func(source string)
}

func newFeedEndpointProvider(feedURL, channel string, client *http.Client, stallTimeout time.Duration, onSourceActive func(string)) (*feedEndpointProvider, error) {
	feedURL = strings.TrimSpace(feedURL)
	if feedURL == "" {
		return nil, errors.New("endpoint: URL is required")
	}
	if client == nil {
		client = &http.Client{Timeout: 30 * time.Second}
	}
	if stallTimeout <= 0 {
		stallTimeout = defaultDownloadStallTimeout
	}
	return &feedEndpointProvider{
		feedURL:        feedURL,
		channel:        normalizeChannel(channel),
		httpClient:     client,
		stallTimeout:   stallTimeout,
		onSourceActive: onSourceActive,
	}, nil
}

func (p *feedEndpointProvider) Name() string { return "endpoint" }

func (p *feedEndpointProvider) Check(ctx context.Context, req updater.CheckRequest) (*updater.Release, error) {
	ep, err := endpoint.New(endpoint.Config{
		URL:        p.feedURL,
		Channel:    p.channel,
		HTTPClient: wrapStripAuthOnRedirect(p.httpClient),
	})
	if err != nil {
		return nil, err
	}
	rel, err := ep.Check(ctx, req)
	if err != nil {
		return nil, err
	}
	if p.onSourceActive != nil {
		p.onSourceActive("endpoint")
	}
	if rel != nil {
		if rel.Metadata == nil {
			rel.Metadata = map[string]any{}
		}
		rel.Metadata[metaUpdateSource] = "endpoint"
	}
	return rel, nil
}

func (p *feedEndpointProvider) Download(ctx context.Context, r *updater.Release, dst io.Writer, onProgress func(written, total int64)) error {
	if r == nil || r.Metadata == nil {
		return errors.New("endpoint: release missing metadata")
	}
	urlStr, _ := r.Metadata["endpoint.artifact.url"].(string)
	if strings.TrimSpace(urlStr) == "" {
		return errors.New("endpoint: release metadata missing artifact URL")
	}
	dlClient, clonedTransport := cloneDownloadClient(p.httpClient)
	if clonedTransport != nil {
		defer clonedTransport.CloseIdleConnections()
	}
	if err := downloadURLToWriter(ctx, dlClient, urlStr, r.Artifact.Size, false, p.stallTimeout, dst, onProgress); err != nil {
		return err
	}
	if p.onSourceActive != nil {
		p.onSourceActive("endpoint")
	}
	return nil
}

func extractDownloadCandidates(meta map[string]any) []string {
	if raw, ok := meta[metaUpdateDownloadCandidates]; ok {
		switch v := raw.(type) {
		case []string:
			if len(v) > 0 {
				return v
			}
		case []any:
			var out []string
			for _, item := range v {
				if s, ok := item.(string); ok && strings.TrimSpace(s) != "" {
					out = append(out, s)
				}
			}
			if len(out) > 0 {
				return out
			}
		}
	}
	if u, _ := meta["endpoint.artifact.url"].(string); strings.TrimSpace(u) != "" {
		return []string{u}
	}
	return nil
}

func downloadCandidateToTemp(ctx context.Context, client *http.Client, urlStr string, expectedSize int64, stallTimeout time.Duration, onProgress func(written, total int64)) (string, error) {
	tmpFile, err := os.CreateTemp("", "kwp-dl-*")
	if err != nil {
		return "", err
	}
	tmpPath := tmpFile.Name()
	dlErr := downloadURLToWriter(ctx, client, urlStr, expectedSize, true, stallTimeout, tmpFile, onProgress)
	closeErr := tmpFile.Close()
	if dlErr != nil {
		_ = os.Remove(tmpPath)
		return "", dlErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpPath)
		return "", closeErr
	}
	return tmpPath, nil
}

func copyTempFileToWriter(path string, dst io.Writer) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	_, err = io.Copy(dst, f)
	return err
}

func downloadURLToWriter(ctx context.Context, client *http.Client, urlStr string, expectedSize int64, strictSize bool, stallTimeout time.Duration, dst io.Writer, onProgress func(written, total int64)) error {
	reqCtx, cancelReq := context.WithCancel(ctx)
	defer cancelReq()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, urlStr, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/octet-stream")
	req.Header.Set("User-Agent", "KimiWorkPlus-Updater")

	resp, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		return fmt.Errorf("download %s: %w", safeURLForLog(urlStr), err)
	}
	defer resp.Body.Close()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("download %s: HTTP %d", safeURLForLog(urlStr), resp.StatusCode)
	}

	total := expectedSize
	if total <= 0 && resp.ContentLength > 0 {
		total = resp.ContentLength
	}

	reader := &stallTimeoutReader{
		parentCtx: ctx,
		cancelReq: cancelReq,
		body:      resp.Body,
		timeout:   stallTimeout,
	}

	buf := make([]byte, 64*1024)
	var written int64
	for {
		n, rerr := reader.Read(buf)
		if n > 0 {
			if strictSize && expectedSize > 0 && written+int64(n) > expectedSize {
				return fmt.Errorf("download %s: oversized payload (exceeds %d bytes)", safeURLForLog(urlStr), expectedSize)
			}
			if _, werr := dst.Write(buf[:n]); werr != nil {
				return werr
			}
			written += int64(n)
			if onProgress != nil {
				onProgress(written, total)
			}
		}
		if rerr == io.EOF {
			break
		}
		if rerr != nil {
			return rerr
		}
	}
	if strictSize && expectedSize > 0 && written != expectedSize {
		if written > expectedSize {
			return fmt.Errorf("download %s: oversized payload (%d of %d bytes)", safeURLForLog(urlStr), written, expectedSize)
		}
		return fmt.Errorf("download %s: truncated payload (%d of %d bytes)", safeURLForLog(urlStr), written, expectedSize)
	}
	return nil
}

// stallTimeoutReader aborts a stalled download via request context cancellation
// instead of calling Response.Body.Close concurrently with Read.
type stallTimeoutReader struct {
	parentCtx context.Context
	cancelReq context.CancelFunc
	body      io.Reader
	timeout   time.Duration
	stalled   atomic.Bool
}

func (r *stallTimeoutReader) Read(p []byte) (int, error) {
	if r.timeout <= 0 {
		return r.body.Read(p)
	}
	var readDone atomic.Bool
	timer := time.AfterFunc(r.timeout, func() {
		if readDone.CompareAndSwap(false, true) {
			r.stalled.Store(true)
			r.cancelReq()
		}
	})
	n, err := r.body.Read(p)
	readDone.Store(true)
	timer.Stop()
	if err != nil && !errors.Is(err, io.EOF) && r.stalled.Load() {
		if r.parentCtx != nil && r.parentCtx.Err() != nil {
			return n, r.parentCtx.Err()
		}
		return n, fmt.Errorf("download stalled: no data received for %s", r.timeout)
	}
	return n, err
}

func cloneDownloadClient(base *http.Client) (*http.Client, *http.Transport) {
	var c http.Client
	if base != nil {
		c = *base
	}
	c.Timeout = 0
	var clonedTransport *http.Transport
	switch t := c.Transport.(type) {
	case nil:
		if def, ok := http.DefaultTransport.(*http.Transport); ok {
			tc := def.Clone()
			if tc.ResponseHeaderTimeout == 0 {
				tc.ResponseHeaderTimeout = defaultDownloadResponseHeaderTimeout
			}
			c.Transport = tc
			clonedTransport = tc
		}
	case *http.Transport:
		tc := t.Clone()
		if tc.ResponseHeaderTimeout == 0 {
			tc.ResponseHeaderTimeout = defaultDownloadResponseHeaderTimeout
		}
		c.Transport = tc
		clonedTransport = tc
	}
	return wrapStripAuthOnRedirect(&c), clonedTransport
}

func wrapStripAuthOnRedirect(c *http.Client) *http.Client {
	var clone http.Client
	if c != nil {
		clone = *c
	}
	prev := clone.CheckRedirect
	clone.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		for _, prevReq := range via {
			if prevReq == nil || prevReq.URL == nil {
				continue
			}
			crossHost := !strings.EqualFold(prevReq.URL.Host, req.URL.Host)
			downgrade := strings.EqualFold(prevReq.URL.Scheme, "https") && strings.EqualFold(req.URL.Scheme, "http")
			if crossHost || downgrade {
				req.Header.Del("Authorization")
				break
			}
		}
		if prev != nil {
			return prev(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}
	return &clone
}

func withTimeoutIfPositive(ctx context.Context, d time.Duration) (context.Context, context.CancelFunc) {
	if d <= 0 {
		return ctx, func() {}
	}
	return context.WithTimeout(ctx, d)
}

func safeURLForLog(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	u.RawQuery = ""
	u.Fragment = ""
	return u.String()
}

type roundTripFunc func(*http.Request) (*http.Response, error)

func (f roundTripFunc) RoundTrip(r *http.Request) (*http.Response, error) {
	return f(r)
}

func normalizeChannel(ch string) string {
	switch strings.ToLower(strings.TrimSpace(ch)) {
	case "preview", "beta", "prerelease":
		return "preview"
	default:
		return "stable"
	}
}
