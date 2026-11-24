package bridge

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/wssz527/kimiwork-plus/internal/core"
	"github.com/wssz527/kimiwork-plus/internal/kimi"
	"github.com/wssz527/kimiwork-plus/internal/ledger"
	"github.com/wssz527/kimiwork-plus/internal/protocol"
)

const (
	workUsageSource       = "kimi_work_session"
	workUsageCursorSchema = 1
	workUsageCursorFile   = "work-wire-cursor.json"
	workUsageMaxLineBytes = 16 * 1024
	workUsageMaxFiles     = 2000
	workUsageMaxDepth     = 5
	workUsageDedupWindow  = int64(15000)
	workUsageRetentionMS  = int64(90) * 86400 * 1000
)

var (
	wireUUIDAliasPattern  = regexp.MustCompile(`^byok-[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}/(.+)$`)
	wireIdentifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/+-]{0,127}$`)
)

type workWireCursorState struct {
	SchemaVersion   int                         `json:"schemaVersion"`
	ClearedBeforeMS int64                       `json:"clearedBeforeMs,omitempty"`
	Files           map[string]workWireFileMark `json:"files"`
}

type workWireFileMark struct {
	Size      int64 `json:"size"`
	ModTimeNS int64 `json:"modTimeNs"`
	Offset    int64 `json:"offset"`
}

type wireUsageEvent struct {
	Type       string `json:"type"`
	AgentID    string `json:"agentId"`
	Model      string `json:"model"`
	UsageScope string `json:"usageScope"`
	Time       int64  `json:"time"`
	Usage      *struct {
		InputOther         *int64 `json:"inputOther"`
		Output             *int64 `json:"output"`
		InputCacheRead     *int64 `json:"inputCacheRead"`
		InputCacheCreation *int64 `json:"inputCacheCreation"`
	} `json:"usage"`
}

type workUsageCollector struct {
	mu              sync.Mutex
	stateDir        string
	kimiHome        string
	cursorPath      string
	ledger          *ledger.Ledger
	store           *core.Store
	budget          *budgetGate
	now             func() int64
	maxFiles        int
	snapshotGateway func(*ledger.Ledger) []gatewayDedupRecord
	cursor          workWireCursorState
	loaded          bool
}

func newWorkUsageCollector(stateDir, kimiHome string, usage *ledger.Ledger, store *core.Store, budget *budgetGate) *workUsageCollector {
	return &workUsageCollector{
		stateDir:   stateDir,
		kimiHome:   kimiHome,
		cursorPath: filepath.Join(stateDir, "usage", workUsageCursorFile),
		ledger:     usage,
		store:      store,
		budget:     budget,
		now:        func() int64 { return time.Now().UnixMilli() },
		cursor:     workWireCursorState{SchemaVersion: workUsageCursorSchema, Files: map[string]workWireFileMark{}},
	}
}

func (c *workUsageCollector) SetHome(kimiHome string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.kimiHome = kimiHome
	c.mu.Unlock()
}

func (c *workUsageCollector) MarkCleared(before any, timeZone string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.loadCursorLocked()
	cutoff := c.now()
	if before != nil {
		zone, err := ledger.ResolveTimeZone(timeZone)
		if err != nil {
			return
		}
		ms, ok := ledger.BoundaryMillis(before, zone, "start")
		if !ok {
			return
		}
		cutoff = ms
	}
	if cutoff > c.cursor.ClearedBeforeMS {
		c.cursor.ClearedBeforeMS = cutoff
		c.saveCursorLocked()
	}
}

func (c *workUsageCollector) loadCursorLocked() {
	if c.loaded {
		return
	}
	c.loaded = true
	if c.cursor.Files == nil {
		c.cursor.Files = map[string]workWireFileMark{}
	}
	raw, err := core.ReadPrivateFile(c.cursorPath, 2<<20)
	if err != nil {
		return
	}
	var parsed workWireCursorState
	if json.Unmarshal(raw, &parsed) != nil || parsed.SchemaVersion != workUsageCursorSchema {
		return
	}
	if parsed.Files == nil {
		parsed.Files = map[string]workWireFileMark{}
	}
	c.cursor = parsed
}

func (c *workUsageCollector) saveCursorLocked() {
	if c.cursorPath == "" {
		return
	}
	c.cursor.SchemaVersion = workUsageCursorSchema
	if c.cursor.Files == nil {
		c.cursor.Files = map[string]workWireFileMark{}
	}
	_ = core.AtomicJSON(c.cursorPath, c.cursor)
}

type workModelMeta struct {
	ModelID          string
	UpstreamModelID  string
	ConnectionID     string
	ProviderID       string
	Protocol         string
	UpstreamProtocol string
}

type workModelResolver struct {
	byAlias    map[string]workModelMeta
	byID       map[string]workModelMeta
	byUpstream map[string]workModelMeta
	ambiguous  map[string]bool
}

func buildWorkModelResolver(stateDir, kimiHome string, d core.Document) workModelResolver {
	res := workModelResolver{
		byAlias:    map[string]workModelMeta{},
		byID:       map[string]workModelMeta{},
		byUpstream: map[string]workModelMeta{},
		ambiguous:  map[string]bool{},
	}
	conns := map[string]map[string]any{}
	for _, row := range core.Rows(d["connections"]) {
		conn := core.Object(row)
		if id := core.String(conn["id"]); id != "" {
			conns[id] = conn
		}
	}
	for _, row := range core.Rows(d["models"]) {
		m := core.Object(row)
		id := core.String(m["id"])
		if id == "" {
			continue
		}
		connID := core.String(m["connectionId"])
		providerID := ""
		if conn := conns[connID]; conn != nil {
			providerID = core.String(conn["providerPresetId"])
		}
		upstream := core.String(m["upstreamModelId"])
		if upstream == "" {
			upstream = core.String(m["upstreamModel"])
		}
		meta := workModelMeta{
			ModelID:          id,
			UpstreamModelID:  upstream,
			ConnectionID:     connID,
			ProviderID:       providerID,
			Protocol:         core.String(m["protocol"]),
			UpstreamProtocol: core.String(m["upstreamProtocol"]),
		}
		res.byID[id] = meta
		res.byAlias["byok-kth-kimi/"+id] = meta
		res.byAlias["byok-kth-resp/"+id] = meta
		if upstream != "" {
			if existing, exists := res.byUpstream[upstream]; exists && existing.ModelID != id {
				res.ambiguous[upstream] = true
			} else {
				res.byUpstream[upstream] = meta
			}
		}
	}
	// Active intent aliases map logical model IDs to their live Kimi runtime aliases.
	for _, mode := range []string{core.IntegrationMode(d), ""} {
		aliases := kimi.ReadIntentAliases(stateDir, kimiHome, mode, "")
		for modelID, alias := range aliases {
			if meta, ok := res.byID[modelID]; ok && alias != "" {
				res.byAlias[alias] = meta
			}
		}
		if len(aliases) > 0 {
			break
		}
	}
	return res
}

func (r workModelResolver) resolve(raw string) (workModelMeta, string) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return workModelMeta{}, ""
	}
	if meta, ok := r.byAlias[trimmed]; ok {
		return meta, trimmed
	}
	for _, prefix := range []string{"byok-kth-kimi/", "byok-kth-resp/"} {
		if strings.HasPrefix(trimmed, prefix) {
			suffix := strings.TrimPrefix(trimmed, prefix)
			if meta, ok := r.byID[suffix]; ok {
				return meta, trimmed
			}
			return workModelMeta{ModelID: suffix}, trimmed
		}
	}
	if match := wireUUIDAliasPattern.FindStringSubmatch(trimmed); len(match) == 2 {
		suffix := match[1]
		if meta, ok := r.byID[suffix]; ok {
			return meta, trimmed
		}
		if !r.ambiguous[suffix] {
			if meta, ok := r.byUpstream[suffix]; ok {
				return meta, trimmed
			}
		}
		return workModelMeta{ModelID: suffix}, trimmed
	}
	if meta, ok := r.byID[trimmed]; ok {
		return meta, trimmed
	}
	if !strings.HasPrefix(trimmed, "byok-") {
		return workModelMeta{ModelID: trimmed, ProviderID: "kimi-official"}, trimmed
	}
	return workModelMeta{ModelID: trimmed}, trimmed
}

type gatewayDedupRecord struct {
	atMS       int64
	latencyMS  int64
	inFlight   bool
	modelID    string
	modelAlias string
	input      *int64
	output     *int64
	total      *int64
	used       bool
}

func snapshotGatewayRecords(l *ledger.Ledger) []gatewayDedupRecord {
	if l == nil {
		return nil
	}
	records := l.Records()
	out := make([]gatewayDedupRecord, 0, len(records))
	for _, rec := range records {
		if rec == nil || rec.Source == workUsageSource {
			continue
		}
		parsed, err := time.Parse("2006-01-02T15:04:05.000Z", rec.At)
		if err != nil {
			continue
		}
		var modelID, modelAlias string
		if rec.ModelID != nil {
			modelID = *rec.ModelID
		}
		if rec.ModelAlias != nil {
			modelAlias = *rec.ModelAlias
		}
		var latencyMS int64
		if rec.LatencyMS != nil && *rec.LatencyMS > 0 {
			latencyMS = *rec.LatencyMS
		}
		out = append(out, gatewayDedupRecord{
			atMS:       parsed.UnixMilli(),
			latencyMS:  latencyMS,
			inFlight:   rec.Status == "started",
			modelID:    modelID,
			modelAlias: modelAlias,
			input:      rec.InputTokens,
			output:     rec.OutputTokens,
			total:      rec.TotalTokens,
		})
	}
	return out
}

func stripWireAliasPrefix(raw string) string {
	for _, prefix := range []string{"byok-kth-kimi/", "byok-kth-resp/"} {
		if strings.HasPrefix(raw, prefix) {
			return strings.TrimPrefix(raw, prefix)
		}
	}
	if match := wireUUIDAliasPattern.FindStringSubmatch(raw); len(match) == 2 {
		return match[1]
	}
	return raw
}

func wireCandidateIDs(meta workModelMeta, rawModel string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, 4)
	add := func(v string) {
		v = strings.TrimSpace(v)
		if v == "" || seen[v] {
			return
		}
		seen[v] = true
		out = append(out, v)
	}
	add(meta.ModelID)
	add(meta.UpstreamModelID)
	add(rawModel)
	add(stripWireAliasPrefix(rawModel))
	return out
}

func gatewayMatchesModel(rec gatewayDedupRecord, candidates []string) bool {
	for _, cand := range candidates {
		if (rec.modelID != "" && rec.modelID == cand) || (rec.modelAlias != "" && rec.modelAlias == cand) {
			return true
		}
	}
	return false
}

func shouldDeferForInflightGateway(existing []gatewayDedupRecord, nowMS, atMS int64, candidates []string) bool {
	if nowMS-atMS >= 5000 || nowMS-atMS < -5000 {
		return false
	}
	for i := range existing {
		rec := &existing[i]
		if !rec.inFlight {
			continue
		}
		if nowMS-rec.atMS > 10*60*1000 || atMS < rec.atMS-5000 {
			continue
		}
		if gatewayMatchesModel(*rec, candidates) {
			return true
		}
	}
	return false
}

func matchGatewayDuplicate(existing []gatewayDedupRecord, atMS int64, candidates []string, input, output, total *int64) bool {
	if output == nil {
		return false
	}
	for i := range existing {
		rec := &existing[i]
		if rec.used || rec.inFlight {
			continue
		}
		finishMS := rec.atMS + rec.latencyMS
		if atMS < rec.atMS-5000 || atMS > finishMS+workUsageDedupWindow {
			continue
		}
		if !gatewayMatchesModel(*rec, candidates) {
			continue
		}
		if rec.output == nil || *rec.output != *output {
			continue
		}
		tokenMatch := (input != nil && rec.input != nil && *input == *rec.input) ||
			(total != nil && rec.total != nil && *total == *rec.total)
		if !tokenMatch {
			continue
		}
		rec.used = true
		return true
	}
	return false
}

func shortHomeTag(kimiHome string) string {
	return shortHomeTagForOS(runtime.GOOS, kimiHome)
}

func shortHomeTagForOS(goos, kimiHome string) string {
	cleaned := filepath.Clean(kimiHome)
	if goos == "windows" {
		cleaned = strings.ToLower(cleaned)
	}
	digest := sha256.Sum256([]byte(cleaned))
	return hex.EncodeToString(digest[:6])
}

type workUsageSyncContext struct {
	c          *workUsageCollector
	loaded     bool
	resolver   workModelResolver
	prices     *protocol.PriceTable
	existingGW []gatewayDedupRecord
}

func (s *workUsageSyncContext) ensure() (workModelResolver, *protocol.PriceTable, []gatewayDedupRecord) {
	if !s.loaded {
		s.loaded = true
		var doc core.Document
		if s.c.store != nil {
			if d, _, readErr := s.c.store.Read(); readErr == nil {
				doc = d
			}
		}
		s.resolver = buildWorkModelResolver(s.c.stateDir, s.c.kimiHome, doc)
		if s.c.budget != nil {
			s.prices = s.c.budget.Prices()
		}
		snapshot := s.c.snapshotGateway
		if snapshot == nil {
			snapshot = snapshotGatewayRecords
		}
		s.existingGW = snapshot(s.c.ledger)
	}
	return s.resolver, s.prices, s.existingGW
}

func (c *workUsageCollector) Sync() int {
	if c == nil || c.ledger == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kimiHome == "" {
		return 0
	}
	sessionsDir := filepath.Join(c.kimiHome, "runtime", "kimi-code", "home", "sessions")
	info, err := os.Lstat(sessionsDir)
	if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return 0
	}
	c.loadCursorLocked()

	homeTag := shortHomeTag(c.kimiHome)
	homePrefix := homeTag + ":"
	maxFiles := c.maxFiles
	if maxFiles <= 0 {
		maxFiles = workUsageMaxFiles
	}
	files, truncated := discoverWireFiles(sessionsDir, maxFiles, workUsageMaxDepth)
	present := make(map[string]bool, len(files))
	var changedFiles []wireFileEntry
	for _, f := range files {
		key := homePrefix + f.rel
		present[key] = true
		mark, ok := c.cursor.Files[key]
		if ok && mark.Size == f.size && mark.ModTimeNS == f.modTimeNS && mark.Offset == f.size {
			continue
		}
		changedFiles = append(changedFiles, f)
	}

	cursorDirty := false
	if !truncated {
		for key := range c.cursor.Files {
			if strings.HasPrefix(key, homePrefix) && !present[key] {
				delete(c.cursor.Files, key)
				cursorDirty = true
			}
		}
	}
	if len(changedFiles) == 0 {
		if cursorDirty {
			c.saveCursorLocked()
		}
		return 0
	}

	nowMS := c.now()
	minAtMS := nowMS - workUsageRetentionMS
	if c.cursor.ClearedBeforeMS > minAtMS {
		minAtMS = c.cursor.ClearedBeforeMS
	}
	syncCtx := &workUsageSyncContext{c: c}

	imported := 0
	for _, f := range changedFiles {
		key := homePrefix + f.rel
		startOffset := int64(0)
		if mark, ok := c.cursor.Files[key]; ok && mark.Offset <= f.size {
			startOffset = mark.Offset
		}
		newOffset, count := c.scanWireFile(homeTag, f, startOffset, nowMS, minAtMS, syncCtx)
		imported += count
		c.cursor.Files[key] = workWireFileMark{
			Size:      f.size,
			ModTimeNS: f.modTimeNS,
			Offset:    newOffset,
		}
		cursorDirty = true
	}
	if cursorDirty {
		c.saveCursorLocked()
	}
	return imported
}

type wireFileEntry struct {
	rel       string
	fullPath  string
	size      int64
	modTimeNS int64
}

func discoverWireFiles(root string, maxFiles, maxDepth int) ([]wireFileEntry, bool) {
	var out []wireFileEntry
	truncated := false
	var walk func(dir, relPrefix string, depth int)
	walk = func(dir, relPrefix string, depth int) {
		if depth > maxDepth {
			return
		}
		if len(out) >= maxFiles {
			truncated = true
			return
		}
		entries, err := os.ReadDir(dir)
		if err != nil {
			return
		}
		for _, entry := range entries {
			if len(out) >= maxFiles {
				truncated = true
				return
			}
			name := entry.Name()
			if name == "" || strings.HasPrefix(name, ".") {
				continue
			}
			full := filepath.Join(dir, name)
			info, err := os.Lstat(full)
			if err != nil || info.Mode()&os.ModeSymlink != 0 {
				continue
			}
			rel := name
			if relPrefix != "" {
				rel = relPrefix + "/" + name
			}
			if info.IsDir() {
				walk(full, rel, depth+1)
				continue
			}
			if name == "wire.jsonl" && info.Mode().IsRegular() {
				out = append(out, wireFileEntry{
					rel:       rel,
					fullPath:  full,
					size:      info.Size(),
					modTimeNS: info.ModTime().UnixNano(),
				})
			}
		}
	}
	walk(root, "", 0)
	return out, truncated
}

func (c *workUsageCollector) scanWireFile(
	homeTag string,
	f wireFileEntry,
	startOffset int64,
	nowMS int64,
	minAtMS int64,
	syncCtx *workUsageSyncContext,
) (int64, int) {
	file, err := os.Open(f.fullPath)
	if err != nil {
		return startOffset, 0
	}
	defer file.Close()

	if startOffset > 0 {
		if _, err := file.Seek(startOffset, io.SeekStart); err != nil {
			return startOffset, 0
		}
	}

	reader := bufio.NewReaderSize(file, 32*1024)
	offset := startOffset
	imported := 0
	convID := conversationIDFromWireRel(f.rel)

	for {
		lineStart := offset
		line, lineLen, complete, oversized, readErr := readBoundedLine(reader, workUsageMaxLineBytes)
		if !complete {
			break
		}
		if !oversized && bytes.Contains(line, []byte(`"usage.record"`)) {
			resolver, prices, existingGW := syncCtx.ensure()
			entry, ok, deferLine := buildWireLedgerEntry(homeTag, f.rel, lineStart, convID, line, nowMS, minAtMS, resolver, prices, existingGW)
			if deferLine {
				break
			}
			if ok {
				res := c.ledger.Record(entry)
				if res.OK && !res.Duplicate {
					imported++
				}
			}
		}
		offset += int64(lineLen)
		if readErr != nil {
			break
		}
	}
	return offset, imported
}

// readBoundedLine reads one newline-terminated line. If a line exceeds maxBytes,
// it drains bytes until '\n' without retaining the payload in memory, returning
// oversized=true and the exact byte count consumed so offsets stay accurate.
func readBoundedLine(r *bufio.Reader, maxBytes int) (line []byte, totalLen int, complete bool, oversized bool, err error) {
	for {
		chunk, sliceErr := r.ReadSlice('\n')
		totalLen += len(chunk)
		if !oversized {
			if len(line)+len(chunk) > maxBytes {
				oversized = true
				line = nil
			} else {
				line = append(line, chunk...)
			}
		}
		if sliceErr == nil {
			return line, totalLen, true, oversized, nil
		}
		if errors.Is(sliceErr, bufio.ErrBufferFull) {
			continue
		}
		// EOF before newline: incomplete trailing write, leave offset untouched.
		return nil, 0, false, false, sliceErr
	}
}

func conversationIDFromWireRel(rel string) *string {
	parts := strings.Split(rel, "/")
	if len(parts) < 4 {
		return nil
	}
	// Expected shape: <workspace>/<conversation>/agents/<agentId>/wire.jsonl
	conv := strings.TrimSpace(parts[1])
	if conv == "" || !wireIdentifierPattern.MatchString(conv) {
		return nil
	}
	return &conv
}

func nonNegativePtr(v *int64) *int64 {
	if v == nil || *v < 0 {
		return nil
	}
	cp := *v
	return &cp
}

func buildWireLedgerEntry(
	homeTag string,
	rel string,
	lineOffset int64,
	convID *string,
	line []byte,
	nowMS int64,
	minAtMS int64,
	resolver workModelResolver,
	prices *protocol.PriceTable,
	existingGW []gatewayDedupRecord,
) (ledger.Entry, bool, bool) {
	var ev wireUsageEvent
	if json.Unmarshal(line, &ev) != nil {
		return ledger.Entry{}, false, false
	}
	if ev.Type != "usage.record" || ev.Usage == nil || ev.Time <= 0 {
		return ledger.Entry{}, false, false
	}
	if ev.Time < minAtMS {
		return ledger.Entry{}, false, false
	}
	scope := strings.ToLower(strings.TrimSpace(ev.UsageScope))
	if scope != "" && scope != "turn" && scope != "step" {
		return ledger.Entry{}, false, false
	}

	inputOther := nonNegativePtr(ev.Usage.InputOther)
	cacheRead := nonNegativePtr(ev.Usage.InputCacheRead)
	cacheWrite := nonNegativePtr(ev.Usage.InputCacheCreation)
	outputTokens := nonNegativePtr(ev.Usage.Output)

	var inputTokens *int64
	if inputOther != nil || cacheRead != nil || cacheWrite != nil {
		var sum int64
		if inputOther != nil {
			sum += *inputOther
		}
		if cacheRead != nil {
			sum += *cacheRead
		}
		if cacheWrite != nil {
			sum += *cacheWrite
		}
		inputTokens = &sum
	}
	var totalTokens *int64
	if inputTokens != nil && outputTokens != nil {
		sum := *inputTokens + *outputTokens
		totalTokens = &sum
	}
	if inputTokens == nil && outputTokens == nil && cacheRead == nil && cacheWrite == nil {
		return ledger.Entry{}, false, false
	}

	meta, rawAlias := resolver.resolve(ev.Model)
	candidates := wireCandidateIDs(meta, rawAlias)
	if matchGatewayDuplicate(existingGW, ev.Time, candidates, inputTokens, outputTokens, totalTokens) {
		return ledger.Entry{}, false, false
	}
	if shouldDeferForInflightGateway(existingGW, nowMS, ev.Time, candidates) {
		return ledger.Entry{}, false, true
	}

	digest := sha256.Sum256([]byte(homeTag + ":" + rel + ":" + strconv.FormatInt(lineOffset, 10)))
	opID := "wire_" + hex.EncodeToString(digest[:12])

	entry := ledger.Entry{
		OperationID:      opID,
		Source:           workUsageSource,
		At:               ev.Time,
		Status:           "completed",
		ModelID:          optional(meta.ModelID),
		ModelAlias:       optional(rawAlias),
		ConnectionID:     optional(meta.ConnectionID),
		ProviderID:       optional(meta.ProviderID),
		Account:          optional(meta.ConnectionID),
		ConversationID:   convID,
		Protocol:         optional(meta.Protocol),
		UpstreamProtocol: optional(meta.UpstreamProtocol),
		InputTokens:      inputTokens,
		OutputTokens:     outputTokens,
		TotalTokens:      totalTokens,
		CacheReadTokens:  cacheRead,
		CacheWriteTokens: cacheWrite,
	}
	if prices != nil && meta.ModelID != "" {
		cost := prices.Cost(meta.ModelID, &protocol.Usage{
			InputTokens:       inputTokens,
			OutputTokens:      outputTokens,
			TotalTokens:       totalTokens,
			CachedInputTokens: cacheRead,
			CacheWriteTokens:  cacheWrite,
		})
		if cost != nil {
			entry.EstimatedCost = cost
			src := prices.Source()
			entry.PriceSource = &src
		}
	}
	return entry, true, false
}
