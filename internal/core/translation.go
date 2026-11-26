package core

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf16"
)

type TranslationConfig struct {
	Enabled           bool   `json:"enabled"`
	Provider          string `json:"provider"`
	TargetLanguage    string `json:"targetLanguage"`
	View              string `json:"view"`
	CustomEndpoint    string `json:"customEndpoint"`
	CustomModel       string `json:"customModel"`
	APIKeyEnv         string `json:"apiKeyEnv"`
	APIKeyRef         string `json:"apiKeyRef,omitempty"`
	APIKeyEnvDisabled bool   `json:"-"`
}

var languages = map[string][3]string{"zh": {"zh-Hans", "zh-CN", "Simplified Chinese"}, "en": {"en", "en", "English"}, "ja": {"ja", "ja", "Japanese"}, "ko": {"ko", "ko", "Korean"}, "es": {"es", "es", "Spanish"}, "fr": {"fr", "fr", "French"}, "de": {"de", "de", "German"}, "ru": {"ru", "ru", "Russian"}}

const defaultTranslationAPIKeyEnv = "KIMI_WORK_TRANSLATION_API_KEY"

var translationRefPattern = regexp.MustCompile(`^translation-[a-f0-9]{64}$`)

func TranslationSettings(v any) (TranslationConfig, error) {
	c := TranslationConfig{Provider: "bing", TargetLanguage: "zh", View: "translated", APIKeyEnv: defaultTranslationAPIKeyEnv}
	if v != nil {
		obj := Object(v)
		if obj == nil {
			return c, errors.New("invalid_translation_config")
		}
		for k := range obj {
			if !strings.Contains(" enabled provider targetLanguage view customEndpoint customModel apiKeyEnv apiKeyRef ", " "+k+" ") {
				return c, errors.New("invalid_translation_config")
			}
		}
		b, _ := json.Marshal(v)
		if json.Unmarshal(b, &c) != nil {
			return c, errors.New("invalid_translation_config")
		}
	}
	if c.APIKeyEnv == "" {
		c.APIKeyEnv = defaultTranslationAPIKeyEnv
	}
	if c.APIKeyRef == "" && translationRefPattern.MatchString(c.APIKeyEnv) {
		c.APIKeyRef = c.APIKeyEnv
		c.APIKeyEnv = defaultTranslationAPIKeyEnv
	}
	if c.APIKeyRef != "" && !translationRefPattern.MatchString(c.APIKeyRef) {
		return c, errors.New("invalid_translation_config")
	}
	if c.Provider != "bing" && c.Provider != "google" && c.Provider != "custom" {
		return c, errors.New("invalid_translation_config")
	}
	if _, ok := languages[c.TargetLanguage]; !ok {
		return c, errors.New("invalid_translation_config")
	}
	if c.View != "translated" && c.View != "original" {
		return c, errors.New("invalid_translation_config")
	}
	if !envPattern.MatchString(c.APIKeyEnv) || reservedEnv(c.APIKeyEnv) || len(c.CustomModel) > 2048 || len(c.CustomEndpoint) > 2048 {
		return c, errors.New("invalid_translation_config")
	}
	for _, r := range c.CustomModel {
		if r < 32 || r == 127 {
			return c, errors.New("invalid_translation_config")
		}
	}
	c.CustomModel = strings.TrimSpace(c.CustomModel)
	c.CustomEndpoint = strings.TrimSpace(c.CustomEndpoint)
	if c.CustomEndpoint != "" {
		var e error
		c.CustomEndpoint, e = BaseURL(c.CustomEndpoint)
		if e != nil {
			return c, errors.New("invalid_translation_endpoint")
		}
	}
	if c.Provider == "custom" && c.CustomEndpoint == "" {
		return c, errors.New("translation_endpoint_required")
	}
	return c, nil
}
func reservedEnv(name string) bool {
	for _, s := range strings.Fields("PATH HOME TMPDIR TMP TEMP APPDATA LOCALAPPDATA PROGRAMFILES USERPROFILE SHELL PWD OLDPWD LANG LC_ALL TERM KIMI_API_KEY KIMI_SHARE_DIR") {
		if name == s {
			return true
		}
	}
	for _, p := range []string{"DAIMON_", "ELECTRON_", "NODE_", "DYLD_", "LD_", "KIMI_DAIMON_", "KIMI_CODE_"} {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

type translationPiece struct {
	keep string
	unit int
}
type translationPlan struct {
	pieces []translationPiece
	units  []string
}

var protectedPatterns = []*regexp.Regexp{regexp.MustCompile("(?s)```.*?(?:```|$)"), regexp.MustCompile("(?s)~~~.*?(?:~~~|$)"), regexp.MustCompile("``[^\\n]*?``"), regexp.MustCompile("`[^`\\n]*`"), regexp.MustCompile("https?://[^\\s<>\"'`]+")}
var spanTokens = regexp.MustCompile("[ \\t]+|\\r\\n|\\r|\\n|[^ \\t\\r\\n]+")

func utfLen(s string) int { return len(utf16.Encode([]rune(s))) }
func makeTranslationPlan(text string, protected []string) translationPlan {
	ranges := [][2]int{}
	for _, p := range protectedPatterns {
		for _, m := range p.FindAllStringIndex(text, -1) {
			end := m[1]
			if strings.HasPrefix(text[m[0]:], "http") {
				for end > m[0] {
					value := text[m[0]:end]
					runes := []rune(value)
					last := runes[len(runes)-1]
					if last == ')' {
						if strings.Count(value, ")") <= strings.Count(value, "(") {
							break
						}
					} else if !strings.ContainsRune(".,;:!?，。；：！？、”’\"']} ", last) {
						break
					}
					end -= len(string(last))
				}
			}
			if end > m[0] {
				ranges = append(ranges, [2]int{m[0], end})
			}
		}
	}
	candidates := []string{}
	for _, s := range protected {
		if s == "" || utfLen(s) > 120000 {
			continue
		}
		candidates = append(candidates, s)
		if v := strings.TrimSpace(s); v != "" && v != s {
			candidates = append(candidates, v)
		}
		if len(candidates) >= 512 {
			break
		}
	}
	for _, s := range candidates {
		for from := 0; from < len(text); {
			i := strings.Index(text[from:], s)
			if i < 0 {
				break
			}
			start := from + i
			ranges = append(ranges, [2]int{start, start + len(s)})
			from = start + len(s)
		}
	}
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i][0] != ranges[j][0] {
			return ranges[i][0] < ranges[j][0]
		}
		return ranges[i][1] < ranges[j][1]
	})
	merged := [][2]int{}
	for _, r := range ranges {
		if len(merged) > 0 && r[0] <= merged[len(merged)-1][1] {
			merged[len(merged)-1][1] = max(merged[len(merged)-1][1], r[1])
		} else {
			merged = append(merged, r)
		}
	}
	plan := translationPlan{}
	unit, gap := "", ""
	keep := func(s string) {
		if s != "" {
			plan.pieces = append(plan.pieces, translationPiece{keep: s, unit: -1})
		}
	}
	flush := func() {
		if unit != "" {
			plan.pieces = append(plan.pieces, translationPiece{unit: len(plan.units)})
			plan.units = append(plan.units, unit)
			unit = ""
		}
	}
	release := func() { keep(gap); gap = "" }
	appendSpan := func(span string) {
		for _, token := range spanTokens.FindAllString(span, -1) {
			if strings.Trim(token, " \t") == "" {
				if unit != "" && gap == "" && token == " " {
					gap = " "
				} else {
					flush()
					release()
					keep(token)
				}
				continue
			}
			hasWord := false
			for _, r := range token {
				if unicode.IsLetter(r) || unicode.IsNumber(r) {
					hasWord = true
					break
				}
			}
			if !hasWord {
				flush()
				release()
				keep(token)
				continue
			}
			if unit != "" && utfLen(unit+gap+token) <= 1200 {
				unit += gap + token
				gap = ""
				continue
			}
			flush()
			release()
			rest := []rune(token)
			for utfLen(string(rest)) > 1200 {
				cut, size, last := 0, 0, 0
				for i, r := range rest {
					n := 1
					if r > 0xffff {
						n = 2
					}
					if size+n > 1200 {
						break
					}
					size += n
					cut = i + 1
					if strings.ContainsRune("。.！!？?；;，,：:、）)】]", r) {
						last = cut
					}
				}
				if last >= cut/2 {
					cut = last
				}
				unit = string(rest[:cut])
				flush()
				rest = rest[cut:]
			}
			unit = string(rest)
		}
	}
	cursor := 0
	for _, r := range merged {
		appendSpan(text[cursor:r[0]])
		flush()
		release()
		keep(text[r[0]:r[1]])
		cursor = r[1]
	}
	appendSpan(text[cursor:])
	flush()
	release()
	return plan
}

type Translator struct {
	Client  *http.Client
	Timeout time.Duration
}

func (t Translator) timeout(custom bool) time.Duration {
	timeout := t.Timeout
	if timeout <= 0 {
		timeout = 20 * time.Second
	}
	timeout = min(max(timeout, time.Second), 120*time.Second)
	if custom {
		timeout = min(timeout*3, 120*time.Second)
	}
	return timeout
}

func (t Translator) Translate(ctx context.Context, text string, c TranslationConfig, key string, protected []string) (string, error) {
	if utfLen(text) > 120000 {
		return "", errors.New("translation_text_too_large")
	}
	if ctx.Err() != nil {
		return "", errors.New("translation_aborted")
	}
	plan := makeTranslationPlan(text, protected)
	if len(plan.units) == 0 {
		return text, nil
	}
	if len(plan.units) > 3000 {
		return "", errors.New("translation_text_too_large")
	}
	limits := map[string][3]int{"bing": {8, 2400, 3}, "google": {1, 1200, 4}, "custom": {10, 2800, 2}}[c.Provider]
	if limits[0] == 0 {
		return "", errors.New("translation_invalid_config")
	}
	batches := [][]int{}
	batch := []int{}
	size := 0
	for i, s := range plan.units {
		if len(batch) > 0 && (len(batch) >= limits[0] || size+utfLen(s) > limits[1]) {
			batches = append(batches, batch)
			batch = nil
			size = 0
		}
		batch = append(batch, i)
		size += utfLen(s)
	}
	if len(batch) > 0 {
		batches = append(batches, batch)
	}
	values := make([]string, len(plan.units))
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	jobs := make(chan []int)
	var wg sync.WaitGroup
	var once sync.Once
	var failure error
	for n := 0; n < min(limits[2], len(batches)); n++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range jobs {
				if ctx.Err() != nil {
					continue
				}
				texts := []string{}
				for _, i := range b {
					texts = append(texts, plan.units[i])
				}
				out, e := t.batch(ctx, texts, c, key)
				if e == nil && len(out) != len(b) {
					e = errors.New("translation_invalid_response")
				}
				if e == nil {
					for i, v := range out {
						if strings.TrimSpace(v) == "" {
							e = errors.New("translation_invalid_response")
							break
						}
						values[b[i]] = strings.TrimSpace(v)
					}
				}
				if e != nil {
					once.Do(func() { failure = e; cancel() })
				}
			}
		}()
	}
	for _, b := range batches {
		select {
		case jobs <- b:
		case <-ctx.Done():
		}
	}
	close(jobs)
	wg.Wait()
	if failure != nil {
		return "", failure
	}
	if ctx.Err() != nil {
		return "", errors.New("translation_aborted")
	}
	var out strings.Builder
	for _, p := range plan.pieces {
		if p.unit < 0 {
			out.WriteString(p.keep)
		} else {
			out.WriteString(values[p.unit])
		}
	}
	return out.String(), nil
}
func (t Translator) request(ctx context.Context, endpoint, contentType, key string, body []byte, custom bool) (any, error) {
	ctx, cancel := context.WithTimeout(ctx, t.timeout(custom))
	defer cancel()
	req, e := http.NewRequestWithContext(ctx, "POST", endpoint, bytes.NewReader(body))
	if e != nil {
		return nil, errors.New("translation_invalid_config")
	}
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Accept", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	client := http.Client{}
	if t.Client != nil {
		client = *t.Client
	}
	client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	res, e := client.Do(req)
	if e != nil {
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return nil, errors.New("translation_timeout")
		}
		if ctx.Err() != nil {
			return nil, errors.New("translation_aborted")
		}
		return nil, errors.New("translation_network_error")
	}
	defer res.Body.Close()
	if res.StatusCode < 200 || res.StatusCode >= 300 {
		return nil, errors.New("translation_http_error")
	}
	b, e := io.ReadAll(io.LimitReader(res.Body, 512*1024+1))
	if e != nil {
		return nil, errors.New("translation_network_error")
	}
	if len(b) > 512*1024 {
		return nil, errors.New("translation_response_too_large")
	}
	var out any
	if json.Unmarshal(b, &out) != nil {
		return nil, errors.New("translation_invalid_response")
	}
	return out, nil
}
func (t Translator) batch(ctx context.Context, texts []string, c TranslationConfig, key string) ([]string, error) {
	language := languages[c.TargetLanguage]
	switch c.Provider {
	case "bing":
		b, _ := json.Marshal(texts)
		v, e := t.request(ctx, "https://edge.microsoft.com/translate/translatetext?to="+url.QueryEscape(language[0])+"&isEnterpriseClient=false", "application/json", "", b, false)
		if e != nil {
			return nil, e
		}
		out := []string{}
		for _, item := range Rows(v) {
			translations := Rows(Object(item)["translations"])
			if len(translations) == 0 {
				return nil, errors.New("translation_invalid_response")
			}
			out = append(out, String(Object(translations[0])["text"]))
		}
		return out, nil
	case "google":
		v, e := t.request(ctx, "https://translate.googleapis.com/translate_a/single?client=gtx&sl=auto&tl="+url.QueryEscape(language[1])+"&dt=t", "application/x-www-form-urlencoded", "", []byte(url.Values{"q": {texts[0]}}.Encode()), false)
		if e != nil {
			return nil, e
		}
		rows := Rows(v)
		if len(rows) == 0 {
			return nil, errors.New("translation_invalid_response")
		}
		var out strings.Builder
		for _, row := range Rows(rows[0]) {
			parts := Rows(row)
			if len(parts) > 0 {
				out.WriteString(String(parts[0]))
			}
		}
		return []string{out.String()}, nil
	default:
		content, e := t.custom(ctx, texts, c, key, language[2])
		if e != nil {
			return nil, e
		}
		if len(texts) == 1 {
			return []string{strings.TrimSpace(content)}, nil
		}
		if out := alignTranslation(content, len(texts)); out != nil {
			return out, nil
		}
		out := []string{}
		for _, s := range texts {
			v, e := t.custom(ctx, []string{s}, c, key, language[2])
			if e != nil {
				return nil, e
			}
			out = append(out, v)
		}
		return out, nil
	}
}
func (t Translator) custom(ctx context.Context, texts []string, c TranslationConfig, key, language string) (string, error) {
	instruction := fmt.Sprintf("You are a translation engine. Translate the user text into %s. Preserve Markdown, code, URLs and line breaks exactly. Reply with only the translation.", language)
	content := texts[0]
	if len(texts) > 1 {
		instruction = fmt.Sprintf("You are a translation engine. The user message is a JSON array of %d strings. Translate every string into %s. Reply with only a JSON array of exactly %d translated strings in the same order; never merge, drop, split or reorder items. Preserve Markdown, code, URLs and line breaks exactly.", len(texts), language, len(texts))
		b, _ := json.Marshal(texts)
		content = string(b)
	}
	payload := Document{"messages": []any{Document{"role": "system", "content": instruction}, Document{"role": "user", "content": content}}, "temperature": 0, "stream": false}
	if c.CustomModel != "" {
		payload["model"] = c.CustomModel
	}
	b, _ := json.Marshal(payload)
	v, e := t.request(ctx, c.CustomEndpoint, "application/json", key, b, true)
	if e != nil {
		return "", e
	}
	choices := Rows(Object(v)["choices"])
	if len(choices) == 0 {
		return "", errors.New("translation_invalid_response")
	}
	out := String(Object(Object(choices[0])["message"])["content"])
	if strings.TrimSpace(out) == "" {
		return "", errors.New("translation_invalid_response")
	}
	return out, nil
}

var numberedLine = regexp.MustCompile(`^\d+[.)]\s*`)

func alignTranslation(content string, count int) []string {
	content = strings.TrimSpace(content)
	if strings.HasPrefix(content, "```") {
		lines := strings.Split(strings.ReplaceAll(content, "\r\n", "\n"), "\n")
		lines = lines[1:]
		if len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "```" {
			lines = lines[:len(lines)-1]
		}
		content = strings.TrimSpace(strings.Join(lines, "\n"))
	}
	var out []string
	if json.Unmarshal([]byte(content), &out) == nil && len(out) == count {
		return out
	}
	out = nil
	for _, s := range strings.Split(content, "\n") {
		if s = strings.TrimSpace(s); s != "" {
			out = append(out, numberedLine.ReplaceAllString(s, ""))
		}
	}
	if len(out) == count {
		return out
	}
	return nil
}
