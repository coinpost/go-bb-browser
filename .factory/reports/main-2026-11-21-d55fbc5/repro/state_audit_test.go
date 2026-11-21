package state

// main@d55fbc5 审计复现（只读 overlay）。断言正确行为；FAIL 即缺陷证据。

import (
	"testing"

	"github.com/chromedp/cdproto/target"
)

// AGENTS.md：短 id 由 CDP target id 确定性派生、跨重启稳定。后缀碰撞时实际取决于登记顺序。
func TestAudit_ShortIDsMustBeStableAcrossRestartOnSuffixCollision(t *testing.T) {
	a := target.ID("FFFF000000000000000000000000ABCD") // 运行期先创建
	b := target.ID("0000111111111111111111111111ABCD") // 运行期后创建，但字典序更小

	run1 := NewTabRegistry()
	a1 := run1.RegisterPageTarget(a) // 事件 / tab_new 顺序
	b1 := run1.RegisterPageTarget(b)

	run2 := NewTabRegistry() // 重启后：SyncPageTargets 按 target id 排序登记
	run2.SyncPageTargets([]*target.Info{{TargetID: a, Type: "page"}, {TargetID: b, Type: "page"}})
	a2, _ := run2.ShortForTarget(a)
	b2, _ := run2.ShortForTarget(b)

	t.Logf("run1: A=%q B=%q; after restart: A=%q B=%q", a1, b1, a2, b2)
	if a1 != a2 || b1 != b2 {
		t.Errorf("BUG: short ids changed across restart (A %q→%q, B %q→%q)", a1, a2, b1, b2)
	}
	if tid, _ := run2.Lookup(a1); tid != a {
		t.Errorf("BUG: short id %q (tab A before restart, e.g. in rpc.jsonl / held by clients) now resolves to %s", a1, tid)
	}
}
