package site

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"regexp"
	"strings"
)

var (
	reExportDefault  = regexp.MustCompile(`^export[ \t\r\n]+default[ \t\r\n]+`)
	reAnonAsyncStart = regexp.MustCompile(`^async[ \t]+function[ \t\r\n]*\(`)
)

// adapterRunSuffix invokes the adapter bound by prepareAdapterJS. It starts on a new
// line so a trailing // comment in the source cannot swallow it, and it refuses to
// continue when trailing code turned the binding into something other than a function.
const adapterRunSuffix = `
;if (typeof __bb_run !== "function") throw new TypeError("bb-browser: adapter source must be a single async function expression");
return await __bb_run(__args);`

// prepareAdapterJS fixes adapters written as `async function(args) { ... }`, which is a
// syntax error when nested inside RunScript's outer function (anonymous declaration).
// The source is kept verbatim: the function expression is bound to __bb_run by a prefix
// and invoked by a suffix, so nothing in the body is scanned or rewritten.
//
// Only the start of the source is inspected, where whitespace and comments can be
// skipped without a JS lexer: optional comments (such as @meta), an optional
// `export default`, then `async function (`. Anything else — a script body, a named
// function, an anonymous function further down — is passed through untouched and runs
// (or fails to parse) exactly as written.
func prepareAdapterJS(src []byte) []byte {
	s := string(src)
	head := skipLeadingJSTrivia(s)
	rest := s[head:]
	// ESM default export is invalid inside our wrapper; some adapters use it.
	if loc := reExportDefault.FindStringIndex(rest); loc != nil {
		rest = rest[loc[1]:]
	}
	if !reAnonAsyncStart.MatchString(rest) {
		return []byte(s[:head] + rest)
	}
	return []byte(s[:head] + "const __bb_run = " + rest + adapterRunSuffix)
}

// skipLeadingJSTrivia returns the offset of the first byte of s that is neither
// whitespace nor part of a comment. At the very start of a script `//` and `/*` can
// only open comments, so no further lexing is needed. An unterminated block comment
// is not skipped.
func skipLeadingJSTrivia(s string) int {
	i := 0
	for i < len(s) {
		switch {
		case s[i] == ' ' || s[i] == '\t' || s[i] == '\n' || s[i] == '\r':
			i++
		case strings.HasPrefix(s[i:], "//"):
			nl := strings.IndexByte(s[i:], '\n')
			if nl < 0 {
				return len(s)
			}
			i += nl + 1
		case strings.HasPrefix(s[i:], "/*"):
			end := strings.Index(s[i+2:], "*/")
			if end < 0 {
				return i
			}
			i += 2 + end + 2
		default:
			return i
		}
	}
	return i
}

// RunScript wraps adapter source as an async IIFE for bb-browser run.
func RunScript(adapterSrc []byte, argsJSON string) string {
	a := strings.TrimSpace(argsJSON)
	if a == "" || a == "null" {
		a = "{}"
	}
	body := prepareAdapterJS(adapterSrc)
	var buf bytes.Buffer
	buf.WriteString("(async function(__args){\n")
	buf.Write(body)
	buf.WriteString("\n})( ")
	buf.WriteString(a)
	buf.WriteString(" )")
	return buf.String()
}

// ArgsObject builds a JSON object from positional args: first mapped to @meta
// "args" keys in source order from @meta, then always arg1, arg2, …, plus named flags.
func ArgsObject(positional []string, named map[string]string, metaArgKeys []string) ([]byte, error) {
	m := make(map[string]any)
	for i, p := range positional {
		if i < len(metaArgKeys) {
			k := strings.TrimSpace(metaArgKeys[i])
			if k != "" {
				m[k] = p
			}
		}
		m[fmt.Sprintf("arg%d", i+1)] = p
	}
	for k, v := range named {
		k = strings.TrimPrefix(k, "--")
		if k != "" {
			m[k] = v
		}
	}
	return json.Marshal(m)
}

// WriteTempAdapter writes JS to a temp file for debugging (optional).
func WriteTempAdapter(js string) (path string, err error) {
	f, err := os.CreateTemp("", "bb-site-*.js")
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := f.WriteString(js); err != nil {
		os.Remove(f.Name())
		return "", err
	}
	return f.Name(), nil
}
