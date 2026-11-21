package site

// main@d55fbc5 审计复现（只读 overlay）。断言正确行为；FAIL 即缺陷证据。

import (
	"os"
	"strings"
	"testing"
)

const auditAdapter = `/* @meta {"name":"demo","domain":"example.com","args":{"url":{"required":true}}} */
async function(args) {
  if (!/^https?:\/\//.test(args.url)) {
    return { error: 'bad url' };
  }
  const r = await fetch(args.url);
  return { ok: r.ok };
}
`

// 合法的匿名 async adapter 只要含 /…\/\//（或带引号、花括号的正则字面量），
// matchingJSBrace 就把它当成行注释 / 字符串，函数体被截断，生成的包装脚本语法错误。
func TestAudit_AdapterWithRegexLiteralMustWrapWholeFunction(t *testing.T) {
	out := RunScript([]byte(auditAdapter), `{"url":"https://example.com"}`)
	if p := os.Getenv("AUDIT_DUMP_WRAPPED"); p != "" {
		_ = os.WriteFile(p, []byte(out), 0o644)
	}
	end := strings.Index(out, "}; return await __bb_run(__args);")
	last := strings.Index(out, "return { ok: r.ok };")
	if end < 0 {
		t.Fatalf("BUG: anonymous async adapter was not rewritten at all:\n%s", out)
	}
	if last < 0 || last > end {
		t.Fatalf("BUG: adapter body truncated at offset %d, before its final statement (offset %d):\n%s", end, last, out)
	}
}
