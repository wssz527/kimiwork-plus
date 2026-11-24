package ledger

import (
	"encoding/json"
	"regexp"
	"sort"
	"time"
	_ "time/tzdata"
)

var (
	identifierPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:@/+%-]{0,127}$`)
	sourcePattern     = regexp.MustCompile(`^[a-z][a-z0-9_]{0,31}$`)
	errorCodePattern  = regexp.MustCompile(`^[a-z][a-z0-9_]{0,63}$`)
	digestPattern     = regexp.MustCompile(`^id_[a-f0-9]{12}$`)
)

// --- usage normalisation ---------------------------------------------------

type usageCounts struct {
	input, output, total, cacheRead, cacheWrite *int64
}

func countOf(value any) *int64 {
	switch item := value.(type) {
	case float64:
		if item < 0 || item != float64(int64(item)) || item > 1<<53-1 {
			return nil
		}
		return int64Ptr(int64(item))
	case int64:
		if item < 0 {
			return nil
		}
		return int64Ptr(item)
	case int:
		if item < 0 {
			return nil
		}
		return int64Ptr(int64(item))
	case json.Number:
		parsed, err := item.Int64()
		if err != nil || parsed < 0 {
			return nil
		}
		return int64Ptr(parsed)
	}
	return nil
}

func sumKnown(values ...*int64) *int64 {
	var total int64
	found := false
	for _, value := range values {
		if value != nil {
			total += *value
			found = true
		}
	}
	if !found {
		return nil
	}
	return int64Ptr(total)
}

// cacheCreation reads Anthropic's cache creation, which is either the flat
// counter or a nested per-lifetime breakdown. Only known parts are summed: a
// missing breakdown is unknown, not zero.
func cacheCreation(value map[string]any) *int64 {
	if flat := countOf(value["cache_creation_input_tokens"]); flat != nil {
		return flat
	}
	nested, ok := value["cache_creation"].(map[string]any)
	if !ok {
		return nil
	}
	return sumKnown(countOf(nested["ephemeral_5m_input_tokens"]), countOf(nested["ephemeral_1h_input_tokens"]))
}

func anthropicUsage(value map[string]any) usageCounts {
	// Anthropic splits the complete input three ways: ordinary input, cache read
	// and cache creation. The sum is what the request actually fed the model, so
	// dropping the cache parts would understate every cached conversation.
	cacheWrite := cacheCreation(value)
	input := sumKnown(countOf(value["input_tokens"]), countOf(value["cache_read_input_tokens"]), cacheWrite)
	output := countOf(value["output_tokens"])
	var total *int64
	if input != nil && output != nil {
		total = int64Ptr(*input + *output)
	}
	return usageCounts{input: input, output: output, total: total,
		cacheRead: countOf(value["cache_read_input_tokens"]), cacheWrite: cacheWrite}
}

func openAIUsage(value map[string]any) usageCounts {
	input := countOf(value["prompt_tokens"])
	if input == nil {
		input = countOf(value["input_tokens"])
	}
	output := countOf(value["completion_tokens"])
	if output == nil {
		output = countOf(value["output_tokens"])
	}
	// A cached prompt is a subset of the prompt, not an extra charge on top of
	// it, so it is reported separately and never added to the total again.
	var cached *int64
	if details, ok := value["prompt_tokens_details"].(map[string]any); ok {
		cached = countOf(details["cached_tokens"])
	}
	if cached == nil {
		if details, ok := value["input_tokens_details"].(map[string]any); ok {
			cached = countOf(details["cached_tokens"])
		}
	}
	total := countOf(value["total_tokens"])
	if total == nil && input != nil && output != nil {
		total = int64Ptr(*input + *output)
	}
	return usageCounts{input: input, output: output, total: total, cacheRead: cached}
}

func scannerUsage(value map[string]any) usageCounts {
	input := countOf(value["inputTokens"])
	output := countOf(value["outputTokens"])
	total := countOf(value["totalTokens"])
	if total == nil && input != nil && output != nil {
		total = int64Ptr(*input + *output)
	}
	return usageCounts{input: input, output: output, total: total,
		cacheRead: countOf(value["cachedInputTokens"]), cacheWrite: countOf(value["cacheWriteTokens"])}
}

func isScannerUsage(value map[string]any) bool {
	for _, field := range []string{"inputTokens", "outputTokens", "totalTokens", "cachedInputTokens"} {
		if _, ok := value[field]; ok {
			return true
		}
	}
	return false
}

// normalizeUsage covers the three shapes this project meets: the gateway
// scanner's counts, an OpenAI envelope and an Anthropic `usage` object.
func normalizeUsage(value map[string]any, protocol string) usageCounts {
	if value == nil {
		return usageCounts{}
	}
	if isScannerUsage(value) {
		return scannerUsage(value)
	}
	if protocol == "anthropic" {
		return anthropicUsage(value)
	}
	return openAIUsage(value)
}

// --- local calendar arithmetic ---------------------------------------------

// ResolveTimeZone validates a zone name. An empty name means the machine's own
// zone, which is what the panel shows when the user never chose one.
func ResolveTimeZone(value string) (*time.Location, error) {
	if value == "" {
		return time.Local, nil
	}
	if len(value) > 64 {
		return nil, ErrInvalidTimeZone
	}
	location, err := time.LoadLocation(value)
	if err != nil {
		return nil, ErrInvalidTimeZone
	}
	return location, nil
}

// BoundaryMillis turns a boundary value into an instant. A date without a time
// names a local calendar day, and the end of a range includes that whole day; a
// timestamp is taken as the instant it names.
func BoundaryMillis(value any, zone *time.Location, edge string) (int64, bool) {
	switch item := value.(type) {
	case int64:
		return item, true
	case int:
		return int64(item), true
	case float64:
		if item != float64(int64(item)) {
			return 0, false
		}
		return int64(item), true
	case json.Number:
		parsed, err := item.Int64()
		return parsed, err == nil
	case string:
		if match := datePattern.FindStringSubmatch(item); match != nil {
			year, month, day := atoi(match[1]), atoi(match[2]), atoi(match[3])
			if edge == "end" {
				day++
			}
			return zonedTimeToUTC(year, month, day, 0, zone), true
		}
		parsed, err := time.Parse(time.RFC3339Nano, item)
		if err != nil {
			return 0, false
		}
		return parsed.UnixMilli(), true
	}
	return 0, false
}

var datePattern = regexp.MustCompile(`^(\d{4})-(\d{2})-(\d{2})$`)

func atoi(value string) int {
	result := 0
	for _, r := range value {
		if r < '0' || r > '9' {
			return 0
		}
		result = result*10 + int(r-'0')
	}
	return result
}

// zonedTimeToUTC returns the instant at which the zone shows
// `year-month-day hour:00`. Day and month numbers may overflow, which is how
// "the next local day" is expressed.
func zonedTimeToUTC(year, month, day, hour int, zone *time.Location) int64 {
	return time.Date(year, time.Month(month), day, hour, 0, 0, 0, zone).UnixMilli()
}

func localParts(ms int64, zone *time.Location) time.Time { return time.UnixMilli(ms).In(zone) }

func pad(value, width int) string {
	digits := []byte{}
	if value == 0 {
		digits = []byte{'0'}
	}
	for value > 0 {
		digits = append([]byte{byte('0' + value%10)}, digits...)
		value /= 10
	}
	for len(digits) < width {
		digits = append([]byte{'0'}, digits...)
	}
	return string(digits)
}

// localLabel is the wall-clock label a bucket shows, e.g. 2026-11-09T00:00.
func localLabel(ms int64, zone *time.Location) string {
	parts := localParts(ms, zone)
	return pad(parts.Year(), 4) + "-" + pad(int(parts.Month()), 2) + "-" + pad(parts.Day(), 2) +
		"T" + pad(parts.Hour(), 2) + ":" + pad(parts.Minute(), 2)
}

// nextBoundary steps to the next bucket. An hour is an hour everywhere, so
// stepping in absolute time keeps a spring-forward day 23 buckets long and a
// fall-back day 25, each labelled with the local hour it really is.
func nextBoundary(ms int64, unit string, zone *time.Location) int64 {
	if unit == "hour" {
		return ms + 3600000
	}
	parts := localParts(ms, zone)
	switch unit {
	case "day":
		return zonedTimeToUTC(parts.Year(), int(parts.Month()), parts.Day()+1, 0, zone)
	case "week":
		return zonedTimeToUTC(parts.Year(), int(parts.Month()), parts.Day()+7, 0, zone)
	default:
		return zonedTimeToUTC(parts.Year(), int(parts.Month())+1, 1, 0, zone)
	}
}

type timeRange struct {
	preset string
	unit   string
	zone   *time.Location
	fromMS int64
	toMS   int64
}

func rangeOf(preset string, from, to any, zone *time.Location, now int64) (timeRange, error) {
	if preset == "custom" {
		fromMS, ok := BoundaryMillis(from, zone, "start")
		if !ok {
			return timeRange{}, ErrInvalidRange
		}
		toMS, ok := BoundaryMillis(to, zone, "end")
		if !ok || toMS <= fromMS {
			return timeRange{}, ErrInvalidRange
		}
		span := toMS - fromMS
		// The unit follows the length of the range: an hour-by-hour chart over a
		// month is unreadable, a daily chart over a day is empty.
		unit := "month"
		switch {
		case span <= 48*3600000:
			unit = "hour"
		case span <= 62*dayMillis:
			unit = "day"
		case span <= 400*dayMillis:
			unit = "week"
		}
		return timeRange{preset: "custom", unit: unit, zone: zone, fromMS: fromMS, toMS: toMS}, nil
	}
	parts := localParts(now, zone)
	dayStart := zonedTimeToUTC(parts.Year(), int(parts.Month()), parts.Day(), 0, zone)
	dayEnd := zonedTimeToUTC(parts.Year(), int(parts.Month()), parts.Day()+1, 0, zone)
	if preset == "today" || preset == "" {
		return timeRange{preset: "today", unit: "hour", zone: zone, fromMS: dayStart, toMS: dayEnd}, nil
	}
	match := daysPattern.FindStringSubmatch(preset)
	if match == nil {
		return timeRange{}, ErrInvalidRangePreset
	}
	days := atoi(match[1])
	if days < 1 || days > 400 {
		return timeRange{}, ErrInvalidRangePreset
	}
	fromMS := zonedTimeToUTC(parts.Year(), int(parts.Month()), parts.Day()-(days-1), 0, zone)
	return timeRange{preset: preset, unit: "day", zone: zone, fromMS: fromMS, toMS: dayEnd}, nil
}

var daysPattern = regexp.MustCompile(`^(\d{1,3})d$`)

func bucketStarts(r timeRange) ([]int64, error) {
	starts := []int64{}
	cursor := r.fromMS
	for cursor < r.toMS {
		starts = append(starts, cursor)
		if len(starts) > maxBuckets {
			return nil, ErrRangeTooLarge
		}
		cursor = nextBoundary(cursor, r.unit, r.zone)
	}
	return starts, nil
}

// --- query -----------------------------------------------------------------

// Filter names one dimension a query may narrow. An unknown field is refused
// even when the range happens to be empty.
var FilterFields = map[string]bool{"connectionId": true, "providerId": true, "modelId": true,
	"conversationId": true, "account": true}

// Query is one read request.
type Query struct {
	Preset   string            `json:"preset"`
	From     any               `json:"from"`
	To       any               `json:"to"`
	TimeZone string            `json:"timeZone"`
	Filters  map[string]string `json:"filters"`
	Now      *int64            `json:"now"`
	// Limit caps the detail rows. Zero means the panel's default (2000), and
	// Unlimited removes the cap entirely, which is what an export needs. The
	// legacy API expressed "no cap" as a null limit, so a negative value is
	// refused rather than silently meaning the same thing: a caller that got
	// the sign wrong would otherwise export everything by accident.
	Limit     int  `json:"limit"`
	Unlimited bool `json:"unlimited"`
}

// Bucket is one calendar slice of the range.
type Bucket struct {
	Start             string   `json:"start"`
	End               string   `json:"end"`
	LocalStart        string   `json:"localStart"`
	UTCOffsetMinutes  int      `json:"utcOffsetMinutes"`
	Requests          int      `json:"requests"`
	RequestsWithUsage int      `json:"requestsWithUsage"`
	Errors            int      `json:"errors"`
	InputTokens       *int64   `json:"inputTokens"`
	OutputTokens      *int64   `json:"outputTokens"`
	TotalTokens       *int64   `json:"totalTokens"`
	CacheReadTokens   *int64   `json:"cacheReadTokens"`
	CacheWriteTokens  *int64   `json:"cacheWriteTokens"`
	EstimatedCost     *float64 `json:"estimatedCost"`
}

// Summary aggregates the filtered set.
type Summary struct {
	Requests          int            `json:"requests"`
	RequestsWithUsage int            `json:"requestsWithUsage"`
	Errors            int            `json:"errors"`
	ByStatus          map[string]int `json:"byStatus"`
	InputTokens       *int64         `json:"inputTokens"`
	OutputTokens      *int64         `json:"outputTokens"`
	TotalTokens       *int64         `json:"totalTokens"`
	CacheReadTokens   *int64         `json:"cacheReadTokens"`
	CacheWriteTokens  *int64         `json:"cacheWriteTokens"`
	EstimatedCost     *float64       `json:"estimatedCost"`
	PriceSources      []string       `json:"priceSources"`
	AverageLatencyMS  *int64         `json:"averageLatencyMs"`
	DataSources       map[string]int `json:"dataSources"`
}

// Option is one filter candidate.
type Option struct {
	Value string `json:"value"`
	Count int    `json:"count"`
}

// ModelOption is a model candidate that also names its Kimi-facing aliases.
type ModelOption struct {
	Value   string   `json:"value"`
	Count   int      `json:"count"`
	Aliases []string `json:"aliases"`
}

// Filters lists what the current range contains, before the filter is applied,
// so a filter can always be widened again.
type Filters struct {
	Connections   []Option      `json:"connections"`
	Providers     []Option      `json:"providers"`
	Models        []ModelOption `json:"models"`
	Conversations []Option      `json:"conversations"`
	Accounts      []Option      `json:"accounts"`
}

// Coverage says what this history can and cannot account for.
type Coverage struct {
	Scope           string         `json:"scope"`
	Sources         []string       `json:"sources"`
	RecordingSince  *string        `json:"recordingSince"`
	LastRecordAt    *string        `json:"lastRecordAt"`
	RecordsInLedger int            `json:"recordsInLedger"`
	RecordsInRange  int            `json:"recordsInRange"`
	Pricing         string         `json:"pricing"`
	PriceSources    []string       `json:"priceSources"`
	Retention       map[string]int `json:"retention"`
	State           string         `json:"state"`
	SkippedLines    int            `json:"skippedLines"`
}

// RangeInfo is the resolved range.
type RangeInfo struct {
	Preset      string `json:"preset"`
	Unit        string `json:"unit"`
	TimeZone    string `json:"timeZone"`
	From        string `json:"from"`
	To          string `json:"to"`
	BucketCount int    `json:"bucketCount"`
}

// QueryResult is the whole panel payload for one read.
type QueryResult struct {
	SchemaVersion    int              `json:"schemaVersion"`
	Range            RangeInfo        `json:"range"`
	Summary          Summary          `json:"summary"`
	Buckets          []Bucket         `json:"buckets"`
	Records          []map[string]any `json:"records"`
	RecordsTotal     int              `json:"recordsTotal"`
	RecordsTruncated bool             `json:"recordsTruncated"`
	Filters          Filters          `json:"filters"`
	Coverage         Coverage         `json:"coverage"`
}

func isoOf(ms int64) string { return time.UnixMilli(ms).UTC().Format("2006-01-02T15:04:05.000Z") }

// Query reads one range. One filtered set feeds the summary, the buckets and
// the detail rows, so a filter or a date change can never leave the three
// showing different data.
func (l *Ledger) Query(q Query) (QueryResult, error) {
	zone, err := ResolveTimeZone(q.TimeZone)
	if err != nil {
		return QueryResult{}, err
	}
	now := l.now()
	if q.Now != nil {
		now = *q.Now
	}
	limit := q.Limit
	if limit == 0 {
		limit = 2000
	}
	if limit < 1 {
		return QueryResult{}, ErrInvalidLimit
	}
	unlimited := q.Unlimited
	accepted, err := filterOf(q.Filters)
	if err != nil {
		return QueryResult{}, err
	}
	preset := q.Preset
	if preset == "" {
		preset = "today"
	}
	rng, err := rangeOf(preset, q.From, q.To, zone, now)
	if err != nil {
		return QueryResult{}, err
	}
	starts, err := bucketStarts(rng)
	if err != nil {
		return QueryResult{}, err
	}
	l.mu.Lock()
	all := make([]*Record, len(l.records))
	for i, record := range l.records {
		all[i] = record.clone()
	}
	state := l.stateLocked()
	skipped := l.skippedLines
	maxRecords := l.maxRecords
	maxAgeDays := l.maxAgeDays
	l.mu.Unlock()

	inRange := make([]*Record, 0, len(all))
	for _, record := range all {
		ms := record.atMillis()
		if ms >= rng.fromMS && ms < rng.toMS {
			inRange = append(inRange, record)
		}
	}
	filters := Filters{
		Connections:   tally(inRange, func(r *Record) string { return deref(r.ConnectionID) }),
		Providers:     tally(inRange, func(r *Record) string { return deref(r.ProviderID) }),
		Models:        modelOptions(inRange),
		Conversations: tally(inRange, func(r *Record) string { return deref(r.ConversationID) }),
		Accounts:      tally(inRange, func(r *Record) string { return deref(r.Account) }),
	}
	filtered := make([]*Record, 0, len(inRange))
	for _, record := range inRange {
		if matches(record, accepted) {
			filtered = append(filtered, record)
		}
	}
	buckets := make([]Bucket, 0, len(starts))
	for i, start := range starts {
		end := rng.toMS
		if i+1 < len(starts) {
			end = starts[i+1]
		}
		buckets = append(buckets, emptyBucket(start, end, zone))
	}
	cursor := 0
	for _, record := range filtered {
		ms := record.atMillis()
		for cursor+1 < len(starts) && ms >= starts[cursor+1] {
			cursor++
		}
		addToBucket(&buckets[cursor], record)
	}
	newest := make([]*Record, 0, len(filtered))
	for i := len(filtered) - 1; i >= 0; i-- {
		newest = append(newest, filtered[i])
	}
	truncated := false
	if !unlimited && len(newest) > limit {
		newest = newest[:limit]
		truncated = true
	}
	rows := make([]map[string]any, 0, len(newest))
	for _, record := range newest {
		rows = append(rows, record.Public())
	}
	return QueryResult{SchemaVersion: SchemaVersion,
		Range: RangeInfo{Preset: rng.preset, Unit: rng.unit, TimeZone: zone.String(),
			From: isoOf(rng.fromMS), To: isoOf(rng.toMS), BucketCount: len(buckets)},
		Summary: summarize(filtered), Buckets: buckets, Records: rows,
		RecordsTotal: len(filtered), RecordsTruncated: truncated, Filters: filters,
		Coverage: coverage(all, len(inRange), state, skipped, maxRecords, maxAgeDays)}, nil
}

func deref(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func filterOf(filters map[string]string) (map[string]string, error) {
	accepted := map[string]string{}
	for field, value := range filters {
		if value == "" {
			continue
		}
		if !FilterFields[field] {
			return nil, ErrInvalidFilter
		}
		if len(value) > maxIdentifier {
			return nil, ErrInvalidFilter
		}
		accepted[field] = value
	}
	return accepted, nil
}

func matches(record *Record, filters map[string]string) bool {
	for field, value := range filters {
		if field == "modelId" {
			if deref(record.ModelID) != value && deref(record.ModelAlias) != value {
				return false
			}
			continue
		}
		if deref(record.fieldPtr(field)) != value {
			return false
		}
	}
	return true
}

func (r *Record) fieldPtr(name string) *string {
	switch name {
	case "connectionId":
		return r.ConnectionID
	case "providerId":
		return r.ProviderID
	case "conversationId":
		return r.ConversationID
	case "account":
		return r.Account
	}
	return nil
}

func emptyBucket(start, end int64, zone *time.Location) Bucket {
	_, seconds := time.UnixMilli(start).In(zone).Zone()
	offset := int64(seconds) / 60
	return Bucket{Start: isoOf(start), End: isoOf(end), LocalStart: localLabel(start, zone),
		UTCOffsetMinutes: int(offset)}
}

func addToBucket(bucket *Bucket, record *Record) {
	bucket.Requests++
	if record.DataSource == "upstream_usage" {
		bucket.RequestsWithUsage++
	}
	if record.Status == "failed" || record.Status == "cancelled" {
		bucket.Errors++
	}
	bucket.InputTokens = addCount(bucket.InputTokens, record.InputTokens)
	bucket.OutputTokens = addCount(bucket.OutputTokens, record.OutputTokens)
	bucket.TotalTokens = addCount(bucket.TotalTokens, record.TotalTokens)
	bucket.CacheReadTokens = addCount(bucket.CacheReadTokens, record.CacheReadTokens)
	bucket.CacheWriteTokens = addCount(bucket.CacheWriteTokens, record.CacheWriteTokens)
	if record.EstimatedCost != nil {
		value := 0.0
		if bucket.EstimatedCost != nil {
			value = *bucket.EstimatedCost
		}
		value += *record.EstimatedCost
		bucket.EstimatedCost = &value
	}
}

func addCount(into, value *int64) *int64 {
	if value == nil {
		return into
	}
	total := *value
	if into != nil {
		total += *into
	}
	return int64Ptr(total)
}

func summarize(records []*Record) Summary {
	summary := Summary{Requests: len(records), ByStatus: map[string]int{},
		PriceSources: []string{}, DataSources: map[string]int{"upstream_usage": 0, "request_only": 0}}
	for _, status := range statuses {
		summary.ByStatus[status] = 0
	}
	var latency int64
	var latencySamples int64
	priceSources := map[string]bool{}
	var cost float64
	costed := 0
	for _, record := range records {
		if _, ok := summary.ByStatus[record.Status]; ok {
			summary.ByStatus[record.Status]++
		}
		if record.DataSource == "upstream_usage" {
			summary.RequestsWithUsage++
		}
		summary.DataSources[record.DataSource]++
		if record.Status == "failed" || record.Status == "cancelled" {
			summary.Errors++
		}
		summary.InputTokens = addCount(summary.InputTokens, record.InputTokens)
		summary.OutputTokens = addCount(summary.OutputTokens, record.OutputTokens)
		summary.TotalTokens = addCount(summary.TotalTokens, record.TotalTokens)
		summary.CacheReadTokens = addCount(summary.CacheReadTokens, record.CacheReadTokens)
		summary.CacheWriteTokens = addCount(summary.CacheWriteTokens, record.CacheWriteTokens)
		if record.LatencyMS != nil {
			latency += *record.LatencyMS
			latencySamples++
		}
		if record.EstimatedCost != nil {
			cost += *record.EstimatedCost
			costed++
			if record.PriceSource != nil {
				priceSources[*record.PriceSource] = true
			}
		}
	}
	if latencySamples > 0 {
		value := latency / latencySamples
		summary.AverageLatencyMS = &value
	}
	for source := range priceSources {
		summary.PriceSources = append(summary.PriceSources, source)
	}
	sort.Strings(summary.PriceSources)
	// A sum of estimates from different price tables is not an estimate of
	// anything, so a mixed history reports no total rather than a misleading one.
	if costed > 0 && len(priceSources) <= 1 {
		rounded := float64(int64(cost*1e6+0.5)) / 1e6
		summary.EstimatedCost = &rounded
	}
	return summary
}

func tally(records []*Record, pick func(*Record) string) []Option {
	counts := map[string]int{}
	for _, record := range records {
		value := pick(record)
		if value == "" {
			continue
		}
		counts[value]++
	}
	out := make([]Option, 0, len(counts))
	for value, count := range counts {
		out = append(out, Option{Value: value, Count: count})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	return out
}

// modelOptions keys the list by the model the upstream really served and keeps
// the Kimi-facing alias, so either identifier can be used to filter.
func modelOptions(records []*Record) []ModelOption {
	type entry struct {
		count   int
		aliases map[string]bool
	}
	entries := map[string]*entry{}
	order := []string{}
	for _, record := range records {
		value := deref(record.ModelID)
		if value == "" {
			value = deref(record.ModelAlias)
		}
		if value == "" {
			continue
		}
		current, ok := entries[value]
		if !ok {
			current = &entry{aliases: map[string]bool{}}
			entries[value] = current
			order = append(order, value)
		}
		current.count++
		if alias := deref(record.ModelAlias); alias != "" && alias != value {
			current.aliases[alias] = true
		}
	}
	out := make([]ModelOption, 0, len(order))
	for _, value := range order {
		current := entries[value]
		aliases := []string{}
		for alias := range current.aliases {
			aliases = append(aliases, alias)
		}
		sort.Strings(aliases)
		out = append(out, ModelOption{Value: value, Count: current.count, Aliases: aliases})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Value < out[j].Value
	})
	return out
}

func coverage(all []*Record, recordsInRange int, state string, skipped int, maxRecords int, maxAgeDays int) Coverage {
	cov := Coverage{Scope: "gateway_observed", Sources: []string{}, PriceSources: []string{},
		RecordsInLedger: len(all), RecordsInRange: recordsInRange,
		Retention: map[string]int{"maxRecords": maxRecords, "maxAgeDays": maxAgeDays},
		State:     state, SkippedLines: skipped}
	sources := map[string]bool{}
	priced := map[string]bool{}
	for _, record := range all {
		sources[record.Source] = true
		if record.PriceSource != nil {
			priced[*record.PriceSource] = true
		}
	}
	for source := range sources {
		cov.Sources = append(cov.Sources, source)
	}
	sort.Strings(cov.Sources)
	if len(all) > 0 {
		first := all[0].At
		last := all[len(all)-1].At
		cov.RecordingSince = &first
		cov.LastRecordAt = &last
	}
	for source := range priced {
		cov.PriceSources = append(cov.PriceSources, source)
	}
	sort.Strings(cov.PriceSources)
	// Cost is an estimate against a named price table or nothing at all; a
	// history with no reliable price says so instead of implying a zero bill.
	switch {
	case len(priced) == 1:
		cov.Pricing = "estimated"
	case len(priced) > 1:
		cov.Pricing = "mixed_price_sources"
	default:
		cov.Pricing = "not_provided"
	}
	return cov
}

// ExportJSON is the same redacted record set the query returns: no key, no
// prompt and no response body ever entered it.
func (l *Ledger) ExportJSON(q Query) ([]byte, error) {
	if q.Preset == "" {
		q.Preset = "90d"
	}
	q.Unlimited = true
	result, err := l.Query(q)
	if err != nil {
		return nil, err
	}
	exported := map[string]any{"schemaVersion": SchemaVersion,
		"exportedAt": isoOf(l.now()), "range": result.Range, "summary": result.Summary,
		"coverage": result.Coverage, "records": result.Records}
	return json.Marshal(exported)
}

func int64Ptr(value int64) *int64 { return &value }
