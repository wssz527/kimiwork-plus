package kimi

import (
	"regexp"
	"sort"
	"strings"
)

// The installed bundle is obfuscated and every release renames its symbols, so
// nothing here may depend on a name. The string table, the decoder and the
// catalog parser are located by shape instead:
//
//	table    the symbol used by the bootstrap IIFE `})(NAME, <number>)` whose
//	         function body declares a large string array and returns it
//	decoder  `function f(a, b) { a = a - <number>; ... }`
//	parser   the function whose decoded body mentions `skippedModels` and
//	         `family` and contains no `await`
//
// Everything is best effort. A bundle this cannot read is `unverified`, which
// is not a failure: the gateway only emits the documented api.json shape and
// never patches Kimi.

const (
	maxFunctionBytes = 6000
	maxDeclaration   = 200000
	maxExpression    = 8000
	maxClosure       = 16
	maxAliasRounds   = 8
	maxRetries       = 60
)

var (
	reBootstrap  = regexp.MustCompile(`\}\)\((_0x[0-9a-f]+),\s*(\d+)\)`)
	reFuncDecl   = regexp.MustCompile(`function (_0x[0-9a-f]+)\(`)
	reTableConst = regexp.MustCompile(`const _0x[0-9a-f]+ = \[`)
	reReturnsTbl = regexp.MustCompile(`return _0x[0-9a-f]+;`)
	reAliasPair  = regexp.MustCompile(`\b(_0x[0-9a-f]+)\s*=\s*(_0x[0-9a-f]+)\s*[,;]`)
	reCallNumber = regexp.MustCompile(`\b(_0x[0-9a-f]+)\((\d+)\)`)
	reLiteral    = regexp.MustCompile(`^(?:\{|\[|"|'|-?\d|!0|!1|void 0|null|true|false)`)
	reDeclName   = regexp.MustCompile(`^(_0x[0-9a-f]+) = ([\s\S]+)$`)
	reIdent      = regexp.MustCompile(`\b(_0x[0-9a-f]+)\b`)
)

// sourceIndex maps a function name to the byte offset of its declaration.
type sourceIndex map[string]int

func indexFunctions(text string) sourceIndex {
	index := make(sourceIndex)
	for _, match := range reFuncDecl.FindAllStringSubmatchIndex(text, -1) {
		if match[0] > 0 && text[match[0]-1] != '\n' {
			continue
		}
		index[text[match[2]:match[3]]] = match[0]
	}
	return index
}

// functionAt returns the source of a named function, ending at the first `\n}`.
// Ordinary bundle functions are short and this boundary is what the original
// probe uses; a shape it cannot read yields no source rather than a wider slice.
func functionAt(index sourceIndex, text, name string) (string, bool) {
	at, ok := index[name]
	if !ok {
		return "", false
	}
	end := strings.Index(text[at:], "\n}")
	if end < 0 {
		return "", false
	}
	return text[at : at+end+2], true
}

func functionBodyOf(text, name string) string {
	at := strings.Index(text, "function "+name+"(")
	if at < 0 {
		return ""
	}
	end := strings.Index(text[at:], "\n}")
	if end < 0 {
		return ""
	}
	return text[at : at+end+2]
}

func skipString(text string, at int) int {
	quote := text[at]
	for at++; at < len(text); at++ {
		if text[at] == '\\' {
			at++
			continue
		}
		if text[at] == quote {
			return at
		}
	}
	return len(text)
}

func arrayEnd(text string, open int) int {
	depth := 0
	for at := open; at < len(text); at++ {
		switch character := text[at]; character {
		case '"', '\'', '`':
			at = skipString(text, at)
		case '[':
			depth++
		case ']':
			depth--
			if depth == 0 {
				return at
			}
		}
	}
	return -1
}

func tailEnd(text string, from int) int {
	depth := 1
	for at := from; at < len(text); at++ {
		switch character := text[at]; character {
		case '"', '\'', '`':
			at = skipString(text, at)
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return at
			}
		}
	}
	return -1
}

// tableBodyOf reads one shape: a `const NAME = [` array as the function's first
// statement. A future obfuscator that declares the table differently makes this
// return empty, and the probe then reports `unverified` instead of guessing.
func tableBodyOf(text, name string) string {
	at := strings.Index(text, "function "+name+"(")
	if at < 0 {
		return ""
	}
	head := text[at:min(at+4096, len(text))]
	for _, boundary := range []string{"\n}", "\nfunction "} {
		if cut := strings.Index(head, boundary); cut >= 0 {
			head = head[:cut]
		}
	}
	declaration := reTableConst.FindStringIndex(head)
	if declaration == nil {
		return ""
	}
	close := arrayEnd(text, at+declaration[1]-1)
	if close < 0 {
		return ""
	}
	end := tailEnd(text, close+1)
	if end < 0 {
		return ""
	}
	return text[at : end+1]
}

// tableFunction locates the bootstrap IIFE, the only `})(NAME, <number>)` in the
// bundle whose target declares and returns a string array.
func tableFunction(text string) (name string, footer string, at int, ok bool) {
	for _, match := range reBootstrap.FindAllStringSubmatchIndex(text, -1) {
		candidate := text[match[2]:match[3]]
		body := tableBodyOf(text, candidate)
		if body == "" || !reReturnsTbl.MatchString(body) || !reTableConst.MatchString(body) {
			continue
		}
		return candidate, text[match[0]:match[1]], match[0], true
	}
	return "", "", 0, false
}

// decoderFunction locates `function f(a, b) { a = a - <n>; ... }`. Go's regexp
// engine (RE2) has no backreferences, so the repeated parameter is checked by
// hand instead of with the original `\2` pattern.
func decoderFunction(text string) (name string, offset int, ok bool) {
	for _, match := range reFuncDecl.FindAllStringSubmatchIndex(text, -1) {
		open := strings.Index(text[match[1]:], ") {")
		if open < 0 || open > 120 {
			continue
		}
		head := text[match[0] : match[1]+open+3]
		parameters := regexp.MustCompile(`\((_0x[0-9a-f]+), (_0x[0-9a-f]+)\) \{`).FindStringSubmatch(head)
		if parameters == nil {
			continue
		}
		// Only the first parameter is required to be reassigned: that is what
		// the original `\2 = \2 - <n>` pattern asserted, and real bundles do
		// not always touch the second one.
		first := parameters[1]
		rest := text[match[1]+open+3:]
		body := rest[:min(len(rest), 200)]
		if !strings.HasPrefix(body, "\n  "+first+" = "+first+" - ") {
			continue
		}
		digits := regexp.MustCompile(`^` + regexp.QuoteMeta("\n  "+first+" = "+first+" - ") + `(\d+);`).FindStringSubmatch(body)
		if digits == nil {
			continue
		}
		value, err := parseSmallInt(digits[1])
		if err != nil {
			continue
		}
		return text[match[2]:match[3]], value, true
	}
	return "", 0, false
}

func parseSmallInt(text string) (int, error) {
	value := 0
	for _, character := range text {
		if character < '0' || character > '9' || value > 1<<30 {
			return 0, errNotANumber
		}
		value = value*10 + int(character-'0')
	}
	return value, nil
}

var errNotANumber = errorString("not_a_number")

type errorString string

func (e errorString) Error() string { return string(e) }

// aliases expands the set of names that resolve to the string decoder. Real
// builds chain thousands of assignments, so the expansion is a fixed point over
// one pass per round rather than a name list.
func aliases(text, decoderName string) map[string]bool {
	set := map[string]bool{decoderName: true, "_0x59c770": true}
	for round := 0; round < maxAliasRounds; round++ {
		grew := false
		for _, match := range reAliasPair.FindAllStringSubmatch(text, -1) {
			if set[match[2]] && !set[match[1]] {
				set[match[1]] = true
				grew = true
			}
		}
		if !grew {
			break
		}
	}
	return set
}

func aliasNames(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for name := range set {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}

// decodeSource replaces every alias call `_0xNNN(<number>)` with the decoder's
// answer. A single forward scan is used instead of one alternation over all
// alias names: on a real 3.2.x build the alternation is 15k branches and costs
// seconds per pass, while the scan is linear.
func decodeSource(source string, set map[string]bool, lookup func(int) (string, bool)) string {
	if !strings.Contains(source, "_0x") {
		return source
	}
	var out strings.Builder
	out.Grow(len(source))
	for at := 0; at < len(source); {
		relative := strings.Index(source[at:], "_0x")
		if relative < 0 {
			out.WriteString(source[at:])
			break
		}
		start := at + relative
		end := start + 3
		for end < len(source) && isLowerHex(source[end]) {
			end++
		}
		if end == start+3 || end >= len(source) || source[end] != '(' {
			out.WriteString(source[at : start+3])
			at = start + 3
			continue
		}
		digitsEnd := end + 1
		for digitsEnd < len(source) && source[digitsEnd] >= '0' && source[digitsEnd] <= '9' {
			digitsEnd++
		}
		if digitsEnd == end+1 || digitsEnd >= len(source) || source[digitsEnd] != ')' {
			out.WriteString(source[at : end+1])
			at = end + 1
			continue
		}
		name := source[start:end]
		value, err := parseSmallInt(source[end+1 : digitsEnd])
		if err != nil || !set[name] {
			out.WriteString(source[at : digitsEnd+1])
			at = digitsEnd + 1
			continue
		}
		decoded, ok := lookup(value)
		if !ok {
			out.WriteString(source[at : digitsEnd+1])
			at = digitsEnd + 1
			continue
		}
		out.WriteString(source[at:start])
		out.WriteString(quoteJS(decoded))
		at = digitsEnd + 1
	}
	return out.String()
}

func isLowerHex(character byte) bool {
	return character >= '0' && character <= '9' || character >= 'a' && character <= 'f'
}

// quoteJS renders a Go string as a double-quoted JavaScript literal. JSON
// escaping is a subset of what the original probe produced with JSON.stringify.
func quoteJS(value string) string {
	var out strings.Builder
	out.WriteByte('"')
	for _, character := range value {
		switch character {
		case '"':
			out.WriteString(`\"`)
		case '\\':
			out.WriteString(`\\`)
		case '\n':
			out.WriteString(`\n`)
		case '\r':
			out.WriteString(`\r`)
		case '\t':
			out.WriteString(`\t`)
		case '\u2028':
			out.WriteString(`\u2028`)
		case '\u2029':
			out.WriteString(`\u2029`)
		default:
			if character < 0x20 {
				out.WriteString(`\u00`)
				out.WriteByte("0123456789abcdef"[character>>4])
				out.WriteByte("0123456789abcdef"[character&0xf])
				continue
			}
			out.WriteRune(character)
		}
	}
	out.WriteByte('"')
	return out.String()
}

// findParser looks for the catalog parser among the functions that mention
// `skippedModels`, which survives obfuscation as plain text.
func findParser(text string, decode func(string) string, index sourceIndex) (name string, source string, ok bool) {
	seen := map[string]bool{}
	for at := 0; ; {
		found := strings.Index(text[at:], "skippedModels")
		if found < 0 {
			return "", "", false
		}
		position := at + found
		at = position + len("skippedModels")
		before := strings.LastIndex(text[:position], "\nfunction ")
		if before < 0 {
			continue
		}
		head := text[before+1 : min(before+60, len(text))]
		match := regexp.MustCompile(`^function (_0x[0-9a-f]+)\(`).FindStringSubmatch(head)
		if match == nil || seen[match[1]] {
			continue
		}
		seen[match[1]] = true
		raw, found2 := functionAt(index, text, match[1])
		if !found2 || len(raw) > maxFunctionBytes {
			continue
		}
		decoded := decode(raw)
		if strings.Contains(decoded, "family") && strings.Contains(decoded, "skippedModels") && !strings.Contains(decoded, "await ") {
			return match[1], decoded, true
		}
	}
}

// declaration is one `name = expression` pair inside a module-level const.
type declaration struct {
	name       string
	expression string
}

// splitDeclarations splits `const a = 1, b = {…}, c = [ … ];` at top-level
// commas, keeping nested commas and quoted text intact.
func splitDeclarations(body string) []declaration {
	var parts []string
	depth, start := 0, 0
	var quote byte
	for at := 0; at < len(body); at++ {
		character := body[at]
		if quote != 0 {
			if character == '\\' {
				at++
			} else if character == quote {
				quote = 0
			}
			continue
		}
		switch character {
		case '"', '\'', '`':
			quote = character
		case '{', '[', '(':
			depth++
		case '}', ']', ')':
			depth--
		case ',':
			if depth == 0 {
				parts = append(parts, body[start:at])
				start = at + 1
			}
		}
	}
	parts = append(parts, body[start:])
	var out []declaration
	for _, part := range parts {
		match := reDeclName.FindStringSubmatch(strings.TrimSpace(part))
		if match == nil {
			continue
		}
		out = append(out, declaration{name: match[1], expression: strings.TrimSpace(match[2])})
	}
	return out
}

// collectConstants resolves the module-level literals a missing symbol needs.
// Only literal shapes are taken: anything computed is left out so the probe
// cannot invent behaviour for something it could not read.
func collectConstants(text string, wanted []string, decode func(string) string) [][]declaration {
	type statement struct{ declarations []declaration }
	var statements []statement
	for at := 0; ; {
		found := strings.Index(text[at:], "\nconst ")
		if found < 0 {
			break
		}
		start := at + found + len("\nconst ")
		at = start
		end := strings.IndexByte(text[start:], ';')
		if end < 0 || end > maxDeclaration {
			continue
		}
		body := text[start : start+end]
		var declarations []declaration
		for _, item := range splitDeclarations(body) {
			expression := ""
			if len(item.expression) <= maxExpression {
				expression = decode(item.expression)
			}
			declarations = append(declarations, declaration{name: item.name, expression: expression})
		}
		statements = append(statements, statement{declarations: declarations})
	}

	needed := map[string]bool{}
	for _, name := range wanted {
		needed[name] = true
	}
	for changed := true; changed; {
		changed = false
		for _, entry := range statements {
			hit := false
			for _, item := range entry.declarations {
				if needed[item.name] {
					hit = true
					break
				}
			}
			if !hit {
				continue
			}
			for _, item := range entry.declarations {
				if !needed[item.name] {
					needed[item.name] = true
					changed = true
				}
				if item.expression == "" {
					continue
				}
				for _, match := range reIdent.FindAllStringSubmatchIndex(item.expression, -1) {
					if match[1] < len(item.expression) && item.expression[match[1]] == '(' {
						continue
					}
					name := item.expression[match[2]:match[3]]
					if !needed[name] {
						needed[name] = true
						changed = true
					}
				}
			}
		}
	}

	var out [][]declaration
	collected := map[string]bool{}
	for _, entry := range statements {
		var keep []declaration
		for _, item := range entry.declarations {
			if needed[item.name] && item.expression != "" && !collected[item.name] && reLiteral.MatchString(item.expression) {
				keep = append(keep, item)
			}
		}
		if len(keep) == 0 {
			continue
		}
		for _, item := range keep {
			collected[item.name] = true
		}
		out = append(out, keep)
	}
	return out
}

func renderConstants(groups [][]declaration) string {
	var lines []string
	for _, group := range groups {
		parts := make([]string, 0, len(group))
		for _, item := range group {
			parts = append(parts, item.name+" = "+item.expression)
		}
		lines = append(lines, "const "+strings.Join(parts, ", ")+";")
	}
	return strings.Join(lines, "\n")
}

// RuntimeMarkers reports which source-level markers the bundle still carries.
// They are observations only: obfuscation can hide a marker, so a missing one
// never proves a runtime feature is absent.
var runtimeMarkers = map[string]string{
	"byok-availability.json": "native_byok_flag",
	"byok-providers.json":    "native_profile_store",
	"models/api.json":        "native_catalog_route",
	"skippedModels":          "catalog_parser",
}

func RuntimeContracts(text string) map[string]bool {
	out := make(map[string]bool, len(runtimeMarkers))
	for marker, id := range runtimeMarkers {
		out[id] = strings.Contains(text, marker)
	}
	return out
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

var (
	regexpCallNames     = regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\(`)
	regexpMissingSymbol = regexp.MustCompile(`(\w+) is not defined`)
	regexpLaunchDecl    = regexp.MustCompile(`\bfunction\s+([A-Za-z_$][\w$]*)\s*\([^)]*\)\s*\{`)
	regexpLaunchReader  = regexp.MustCompile(`\b([A-Za-z_$][\w$]*)\(\s*["']DAIMON_BUNDLE_DIR["']\s*\)`)
)
