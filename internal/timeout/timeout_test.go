package timeout

import (
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestOperationSecondsMatchesOperation(t *testing.T) {
	if time.Duration(OperationSeconds)*time.Second != Operation {
		t.Fatalf("OperationSeconds = %d, Operation = %v", OperationSeconds, Operation)
	}
}

var (
	budgetLine    = regexp.MustCompile(`超时|预算|上限`)
	budgetSeconds = regexp.MustCompile(`(\d+)\s*(?:s\b|秒)`)
)

// Every "N s / N 秒" on a line about 超时 / 预算 / 上限 in the adapter and CLI
// docs must be the daemon's real per-RPC budget. Each file must state it at
// least `min` times, so the number cannot silently disappear either.
func TestDocsStateOperationBudget(t *testing.T) {
	root := filepath.Join("..", "..")
	docs := map[string]int{
		"skills/bb-browser-adapter/SKILL.md":               2,
		"skills/bb-browser-adapter/references/patterns.md": 2,
	}
	for rel, min := range docs {
		t.Run(rel, func(t *testing.T) {
			raw, err := os.ReadFile(filepath.Join(root, filepath.FromSlash(rel)))
			if err != nil {
				t.Fatal(err)
			}
			stated := 0
			for i, line := range strings.Split(string(raw), "\n") {
				if !budgetLine.MatchString(line) {
					continue
				}
				for _, m := range budgetSeconds.FindAllStringSubmatch(line, -1) {
					n, err := strconv.Atoi(m[1])
					if err != nil {
						t.Fatal(err)
					}
					if n != OperationSeconds {
						t.Errorf("%s:%d states %ds, daemon budget is %ds: %s", rel, i+1, n, OperationSeconds, strings.TrimSpace(line))
						continue
					}
					stated++
				}
			}
			if stated < min {
				t.Errorf("%s states the %ds budget %d times, want at least %d", rel, OperationSeconds, stated, min)
			}
		})
	}
}

// The README timeout paragraph is anchored by a marker so it is checked
// without constraining the unrelated durations elsewhere in the file.
func TestReadmeStatesOperationBudget(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "README.md"))
	if err != nil {
		t.Fatal(err)
	}
	const marker = "**超时预算**"
	var para string
	for _, line := range strings.Split(string(raw), "\n") {
		if strings.Contains(line, marker) {
			para = line
			break
		}
	}
	if para == "" {
		t.Fatalf("README.md has no %s paragraph", marker)
	}
	matches := budgetSeconds.FindAllStringSubmatch(para, -1)
	if len(matches) == 0 {
		t.Fatalf("README %s paragraph states no budget: %s", marker, para)
	}
	for _, m := range matches {
		if n, _ := strconv.Atoi(m[1]); n != OperationSeconds {
			t.Errorf("README %s paragraph states %ds, daemon budget is %ds", marker, n, OperationSeconds)
		}
	}
}
