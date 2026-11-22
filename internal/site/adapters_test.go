package site

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestReadAdapterFile_noMeta(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.js")
	if err := os.WriteFile(path, []byte("async function(args){}\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, meta, err := ReadAdapterFile(path)
	if err != nil || meta.Name != "" || meta.Domain != "" {
		t.Fatalf("meta=%+v err=%v", meta, err)
	}
}

func TestParseMeta(t *testing.T) {
	src := []byte(`/* @meta
{"name":"demo/x","description":"d","domain":"example.com","args":{"q":{"required":true}}}
*/
`)
	m, err := parseMeta(src)
	if err != nil || m.Name != "demo/x" || m.Domain != "example.com" {
		t.Fatalf("meta %+v err %v", m, err)
	}
}

func TestArgKeysFromAdapterSource_order(t *testing.T) {
	src := []byte(`/* @meta
{
 "name": "demo/search",
 "args": {
 "query": {"required": true},
 "count": {"required": false}
 }
}
*/
`)
	got := ArgKeysFromAdapterSource(src)
	if len(got) != 2 || got[0] != "query" || got[1] != "count" {
		t.Fatalf("keys %v", got)
	}
}

func TestArgKeysFromAdapterSource_missingArgs(t *testing.T) {
	src := []byte(`/* @meta {"name":"x","domain":"d.example"} */
`)
	if ArgKeysFromAdapterSource(src) != nil {
		t.Fatal("expected nil")
	}
}

func TestArgsObject_metaPositional(t *testing.T) {
	b, err := ArgsObject([]string{"bitcoin"}, nil, []string{"query", "count"})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), `"query":"bitcoin"`) || !strings.Contains(string(b), `"arg1":"bitcoin"`) {
		t.Fatalf("%s", b)
	}
}

func TestPrepareAdapterJS_anonymousAsync(t *testing.T) {
	src := []byte(`/* @meta
{"name":"demo/x","description":"d","domain":"example.com"}
*/
async function(args) {
  const x = "}";
  if (1) { return {ok: true}; }
  return {a: 1};
}
`)
	out := string(prepareAdapterJS(src))
	if !strings.Contains(out, "const __bb_run = async function(args)") || !strings.Contains(out, "return await __bb_run(__args)") {
		t.Fatalf("expected __bb_run wrapper, got:\n%s", out)
	}
}

func TestPrepareAdapterJS_namedAsyncUnchanged(t *testing.T) {
	src := []byte(`/* @meta {"name":"x"} */
async function main(args) {
  return 1;
}
`)
	out := string(prepareAdapterJS(src))
	if strings.Contains(out, "__bb_run") {
		t.Fatalf("named async should not be rewritten, got:\n%s", out)
	}
}

func TestRunScript_nodeSyntaxCheck(t *testing.T) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not in PATH")
	}
	src := []byte(`/* @meta {} */
async function(args) { return {n: 1}; }
`)
	js := RunScript(src, "{}")
	cmd := exec.Command("node", "--check")
	cmd.Stdin = bytes.NewReader([]byte(js))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("node --check: %v\n%s\nscript:\n%s", err, out, js)
	}
}

// nodeAdapterRun is what the Node harness reports after evaluating a RunScript
// output with a stubbed fetch (no real network).
type nodeAdapterRun struct {
	Result    json.RawMessage   `json:"result"`
	Undefined bool              `json:"undefined"`
	Error     string            `json:"error"`
	Calls     []json.RawMessage `json:"calls"`
}

const nodeAdapterHarness = `
const fs = require('fs');
const calls = [];
globalThis.fetch = async (...a) => {
  calls.push(a);
  return { ok: true, status: 200, json: async () => ({ ok: true }), text: async () => '{"ok":true}' };
};
(async () => {
  let out;
  try {
    const r = await (0, eval)(fs.readFileSync(process.argv[2], 'utf8'));
    out = { result: r === undefined ? null : r, undefined: r === undefined, calls };
  } catch (e) {
    out = { error: String(e), calls };
  }
  process.stdout.write(JSON.stringify(out));
})();
`

func requireNode(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not in PATH")
	}
}

func nodeCheck(js string) ([]byte, error) {
	cmd := exec.Command("node", "--check")
	cmd.Stdin = strings.NewReader(js)
	return cmd.CombinedOutput()
}

// runWrappedInNode syntax-checks RunScript(src, argsJSON) and executes it in Node.
func runWrappedInNode(t *testing.T, src, argsJSON string) nodeAdapterRun {
	t.Helper()
	js := RunScript([]byte(src), argsJSON)
	if out, err := nodeCheck(js); err != nil {
		t.Fatalf("node --check: %v\n%s\nscript:\n%s", err, out, js)
	}
	dir := t.TempDir()
	harness := filepath.Join(dir, "harness.js")
	script := filepath.Join(dir, "wrapped.js")
	if err := os.WriteFile(harness, []byte(nodeAdapterHarness), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(script, []byte(js), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("node", harness, script).CombinedOutput()
	if err != nil {
		t.Fatalf("node run: %v\n%s\nscript:\n%s", err, out, js)
	}
	var res nodeAdapterRun
	if err := json.Unmarshal(out, &res); err != nil {
		t.Fatalf("harness output %q: %v\nscript:\n%s", out, err, js)
	}
	return res
}

func compactJSON(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatalf("compact %q: %v", raw, err)
	}
	return buf.String()
}

// Adapters must run as written: the wrapper may not cut the function short when
// its body holds regex literals or nested template literals (FT23).
func TestRunScript_adapterRunsWhole(t *testing.T) {
	requireNode(t)
	const args = `{"url":"https://api.example.test/v1","q":"a{b}\"c"}`
	const wantCall = `["https://api.example.test/v1",{"credentials":"include"}]`
	fetchTail := `
  const resp = await fetch(args.url, {credentials: 'include'});
  return await resp.json();
}`
	urlRegexAdapter := `async function(args) {
  if (!/^https?:\/\//.test(args.url)) {
    return {error: 'bad url'};
  }` + fetchTail
	cases := []struct {
		name     string
		src      string
		want     string
		wantCall string
	}{
		{
			name:     "url regex with multi-line if",
			src:      urlRegexAdapter,
			want:     `{"ok":true}`,
			wantCall: wantCall,
		},
		{
			name: "regex with quote and braces",
			src: `async function(args) {
  const cleaned = args.q.replace(/[{}"]/g, '');
  if (cleaned !== 'abc') {
    return {error: cleaned};
  }` + fetchTail,
			want:     `{"ok":true}`,
			wantCall: wantCall,
		},
		{
			name: "nested template literals",
			src: "async function(args) {\n" +
				"  const tag = `${[1, 2].map(n => `${n}}`).join('')}|${args.q.length}`;\n" +
				"  if (tag !== '1}2}|6') {\n" +
				"    return {error: tag};\n" +
				"  }" + fetchTail,
			want:     `{"ok":true}`,
			wantCall: wantCall,
		},
		{
			name: "meta, export default, trailing semicolon",
			src: `/* @meta
{"name":"demo/x","description":"d","domain":"example.test"}
*/
export default ` + urlRegexAdapter + ";\n",
			want:     `{"ok":true}`,
			wantCall: wantCall,
		},
		{
			name: "leading line comment, trailing semicolon and comment",
			src: `// fetches args.url
/* @meta {"name":"demo/x"} */
` + urlRegexAdapter + `; // {{ done
`,
			want:     `{"ok":true}`,
			wantCall: wantCall,
		},
		{
			name: "hoisted helper after the adapter",
			src: `async function(args) {
  if (!isHTTP(args.url)) {
    return {error: 'bad url'};
  }` + fetchTail + `
function isHTTP(u) { return /^https?:\/\//.test(u); }
`,
			want:     `{"ok":true}`,
			wantCall: wantCall,
		},
		{
			name: "script body with return",
			src: `/* @meta {"name":"demo/body"} */
if (!/^https?:\/\//.test(__args.url)) {
  return {error: 'bad url'};
}
const resp = await fetch(__args.url, {credentials: 'include'});
return await resp.json();
`,
			want:     `{"ok":true}`,
			wantCall: wantCall,
		},
		{
			name: "script body with nested anonymous async function",
			src: `const run = async function(args) {
  if (!/^https?:\/\//.test(args.url)) {
    return {error: 'bad url'};
  }` + fetchTail + `;
return await run(__args);
`,
			want:     `{"ok":true}`,
			wantCall: wantCall,
		},
		{
			name: "arrow IIFE script body",
			src: `return (async (args) => {
  if (!/^https?:\/\//.test(args.url)) {
    return {error: 'bad url'};
  }` + fetchTail + `)(__args);
`,
			want:     `{"ok":true}`,
			wantCall: wantCall,
		},
		{
			name: "adapter text inside string and comment of a script body",
			src: `// async function(args) { return {wrong: 1}; }
const s = "async function(args) { return {wrong: 2}; }";
/* export default async function(args) { */
return {ok: true, s};
`,
			want: `{"ok":true,"s":"async function(args) { return {wrong: 2}; }"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := runWrappedInNode(t, tc.src, args)
			if res.Error != "" || res.Undefined {
				t.Fatalf("error=%q undefined=%v\nscript:\n%s", res.Error, res.Undefined, RunScript([]byte(tc.src), args))
			}
			if got := compactJSON(t, res.Result); got != tc.want {
				t.Fatalf("result %s, want %s", got, tc.want)
			}
			wantCalls := 0
			if tc.wantCall != "" {
				wantCalls = 1
			}
			if len(res.Calls) != wantCalls {
				t.Fatalf("fetch calls %d, want %d: %s", len(res.Calls), wantCalls, res.Calls)
			}
			if wantCalls == 1 {
				if got := compactJSON(t, res.Calls[0]); got != tc.wantCall {
					t.Fatalf("fetch args %s, want %s", got, tc.wantCall)
				}
			}
		})
	}
}

// Forms the wrapper never invoked keep doing nothing rather than being rewritten.
func TestRunScript_uncalledFormsStayUndefined(t *testing.T) {
	requireNode(t)
	for name, src := range map[string]string{
		"empty":       "",
		"meta only":   "/* @meta {\"name\":\"x\"} */\n",
		"named async": "async function main(args) {\n  await fetch('https://api.example.test/');\n  return 1;\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			res := runWrappedInNode(t, src, "{}")
			if res.Error != "" || !res.Undefined || len(res.Calls) != 0 {
				t.Fatalf("got %+v", res)
			}
		})
	}
}

// Broken source must stay broken: never repaired into different, valid code.
func TestRunScript_invalidSourceStaysInvalid(t *testing.T) {
	requireNode(t)
	for name, src := range map[string]string{
		"unclosed body":           "async function(args) {\n  if (args.url) {\n    return {ok: true};\n}\n",
		"unclosed body, regex":    "async function(args) {\n  if (/^https?:\\/\\//.test(args.url)) {\n    return {ok: true};\n",
		"unterminated template":   "async function(args) {\n  return {ok: true};\n}\n`",
		"unterminated comment":    "async function(args) {\n  return {ok: true};\n}\n/* tail",
		"anonymous after prelude": "const base = 1;\nasync function(args) {\n  return {ok: true};\n}\n",
	} {
		t.Run(name, func(t *testing.T) {
			js := RunScript([]byte(src), "{}")
			if out, err := nodeCheck(js); err == nil {
				t.Fatalf("node --check accepted invalid source:\n%s\n%s", js, out)
			}
		})
	}
}

// A leading function expression that is consumed by trailing code is reported, not run.
func TestRunScript_nonFunctionAdapterErrors(t *testing.T) {
	requireNode(t)
	res := runWrappedInNode(t, "async function(args) {\n  await fetch(args.url);\n  return {ok: true};\n}\n.name\n", "{}")
	if !strings.Contains(res.Error, "adapter") || len(res.Calls) != 0 {
		t.Fatalf("got %+v", res)
	}
}
