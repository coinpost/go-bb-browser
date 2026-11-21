// Package deploy_test holds static contract checks for the container
// deployment assets (Dockerfile, .goreleaser.yaml, compose example).
//
// They cannot prove what a real Docker engine does, but they pin the two
// properties the image relies on:
//
//   - the state directory used by the official compose example exists in the
//     image and is owned by the nonroot runtime user, so a fresh named volume
//     mounted there inherits writable ownership;
//   - the Dockerfile only needs files that exist in GoReleaser's isolated
//     Docker build context (the built binaries plus declared extra_files) and
//     never needs a shell or a pulled build stage.
package deploy_test

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

const (
	// distroless "nonroot" user/group.
	nonrootID = "65532"
)

type instruction struct {
	cmd   string            // upper-cased, e.g. COPY
	flags map[string]string // --from=x → {"from": "x"}
	args  []string
	raw   string
}

type stage struct {
	image string
	name  string
	insts []instruction
}

func readRepoFile(t *testing.T, rel string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatalf("read %s: %v", rel, err)
	}
	return string(b)
}

func parseDockerfile(t *testing.T, src string) []stage {
	t.Helper()
	var logical []string
	var cur strings.Builder
	for _, line := range strings.Split(src, "\n") {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "#") {
			continue
		}
		if strings.HasSuffix(trim, "\\") {
			cur.WriteString(strings.TrimSuffix(trim, "\\"))
			cur.WriteString(" ")
			continue
		}
		cur.WriteString(trim)
		logical = append(logical, cur.String())
		cur.Reset()
	}
	if cur.Len() > 0 {
		t.Fatalf("Dockerfile ends inside a line continuation: %q", cur.String())
	}

	var stages []stage
	for _, l := range logical {
		fields := strings.Fields(l)
		in := instruction{cmd: strings.ToUpper(fields[0]), flags: map[string]string{}, raw: l}
		rest := fields[1:]
		for len(rest) > 0 && strings.HasPrefix(rest[0], "--") {
			k, v, _ := strings.Cut(strings.TrimPrefix(rest[0], "--"), "=")
			in.flags[k] = v
			rest = rest[1:]
		}
		in.args = rest
		if in.cmd == "FROM" {
			if len(in.args) == 0 {
				t.Fatalf("FROM without image: %q", l)
			}
			st := stage{image: in.args[0]}
			if len(in.args) == 3 && strings.EqualFold(in.args[1], "AS") {
				st.name = in.args[2]
			}
			stages = append(stages, st)
			continue
		}
		if len(stages) == 0 {
			if in.cmd == "ARG" {
				continue
			}
			t.Fatalf("instruction before first FROM: %q", l)
		}
		last := &stages[len(stages)-1]
		last.insts = append(last.insts, in)
	}
	if len(stages) == 0 {
		t.Fatal("Dockerfile has no FROM")
	}
	return stages
}

// composeStateDir returns the BB_BROWSER_STATE_DIR value and the container
// path of the named volume mounted in the compose example.
func composeStateDir(t *testing.T, compose string) (envDir, volumeName, mountDir string) {
	t.Helper()
	envRe := regexp.MustCompile(`(?m)^\s*BB_BROWSER_STATE_DIR:\s*"?([^"\s]+)"?\s*$`)
	m := envRe.FindStringSubmatch(compose)
	if m == nil {
		t.Fatal("compose example does not set BB_BROWSER_STATE_DIR")
	}
	envDir = m[1]
	volRe := regexp.MustCompile(`(?m)^\s*-\s*"?([A-Za-z0-9][A-Za-z0-9_.-]*):(/[^":\s]*)"?\s*$`)
	for _, vm := range volRe.FindAllStringSubmatch(compose, -1) {
		if path.Clean(vm[2]) == path.Clean(envDir) {
			return envDir, vm[1], vm[2]
		}
	}
	t.Fatalf("compose example mounts no named volume at BB_BROWSER_STATE_DIR %q", envDir)
	return
}

func isNonrootOwner(v string) bool {
	switch v {
	case nonrootID, nonrootID + ":" + nonrootID, "nonroot", "nonroot:nonroot":
		return true
	}
	return false
}

// A fresh named volume takes its ownership from the image directory it is
// mounted over. The image runs as uid 65532 on a shell-less base, so that
// directory must be pre-created with nonroot ownership at build time.
func TestImageProvisionsNonrootStateDirForComposeVolume(t *testing.T) {
	stages := parseDockerfile(t, readRepoFile(t, "Dockerfile"))
	envDir, volumeName, mountDir := composeStateDir(t, readRepoFile(t, "deploy/docker-compose.example.yml"))
	stateDir := path.Clean(envDir)
	if path.Clean(mountDir) != stateDir {
		t.Fatalf("volume %s mounted at %q, want BB_BROWSER_STATE_DIR %q", volumeName, mountDir, envDir)
	}

	final := stages[len(stages)-1]
	user := ""
	for _, in := range final.insts {
		if in.cmd == "USER" && len(in.args) == 1 {
			user = in.args[0]
		}
	}
	if !isNonrootOwner(user) {
		t.Fatalf("final image USER = %q, want nonroot (uid %s)", user, nonrootID)
	}

	var provision *instruction
	for i, in := range final.insts {
		if in.cmd != "COPY" || len(in.args) != 2 {
			continue
		}
		if path.Clean(in.args[1]) == stateDir {
			provision = &final.insts[i]
		}
	}
	if provision == nil {
		t.Fatalf("Dockerfile does not create the compose state dir %s in the final image; "+
			"a fresh named volume mounted there is root-owned and not writable by uid %s", stateDir, nonrootID)
	}
	// Numeric ids: no dependency on /etc/passwd lookups at build time.
	if got := provision.flags["chown"]; got != nonrootID+":"+nonrootID {
		t.Fatalf("%q: --chown=%q, want %s:%s", provision.raw, got, nonrootID, nonrootID)
	}
	from := provision.flags["from"]
	var src *stage
	for i := range stages[:len(stages)-1] {
		if stages[i].name != "" && stages[i].name == from {
			src = &stages[i]
		}
	}
	if src == nil {
		t.Fatalf("%q: state dir must be copied from an earlier named stage, got --from=%q", provision.raw, from)
	}
	srcDir := path.Clean(provision.args[0])
	created := false
	for _, in := range src.insts {
		switch in.cmd {
		case "WORKDIR":
			if len(in.args) == 1 && path.Clean(in.args[0]) == srcDir {
				created = true
			}
		case "COPY", "ADD":
			// Volume initialisation copies image content into the fresh
			// volume; the directory must stay empty.
			t.Fatalf("stage %q must only create the empty state dir, found %q", src.name, in.raw)
		}
	}
	if !created {
		t.Fatalf("stage %q does not create %s (expected WORKDIR %s)", src.name, srcDir, srcDir)
	}

	for _, in := range final.insts {
		if in.cmd == "VOLUME" {
			t.Fatalf("unexpected %q: anonymous volumes would hide missing mounts", in.raw)
		}
	}
}

type dockerEntry struct {
	id         string
	dockerfile string
	ids        []string
	extraFiles []string
}

func inlineList(v string) []string {
	v = strings.TrimSpace(v)
	v = strings.TrimPrefix(v, "[")
	v = strings.TrimSuffix(v, "]")
	var out []string
	for _, p := range strings.Split(v, ",") {
		p = strings.Trim(strings.TrimSpace(p), `"'`)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// topLevelBlock returns the lines of a top-level YAML key's block.
func topLevelBlock(src, key string) []string {
	var out []string
	in := false
	for _, line := range strings.Split(src, "\n") {
		if strings.TrimSpace(line) == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		if !strings.HasPrefix(line, " ") && !strings.HasPrefix(line, "-") {
			in = strings.HasPrefix(line, key+":")
			continue
		}
		if in {
			out = append(out, line)
		}
	}
	return out
}

// parseGoReleaser extracts just what the Docker build context depends on:
// build id → binary name, and each dockers entry's ids / extra_files.
func parseGoReleaser(t *testing.T, src string) (binaries map[string]string, dockers []dockerEntry) {
	t.Helper()
	binaries = map[string]string{}
	id := ""
	for _, line := range topLevelBlock(src, "builds") {
		trim := strings.TrimSpace(line)
		if strings.HasPrefix(trim, "- ") {
			id = ""
			trim = strings.TrimSpace(strings.TrimPrefix(trim, "- "))
		}
		k, v, ok := strings.Cut(trim, ":")
		if !ok {
			continue
		}
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		switch k {
		case "id":
			id = v
			if _, seen := binaries[id]; !seen {
				binaries[id] = id
			}
		case "binary":
			if id == "" {
				t.Fatalf(".goreleaser.yaml: build binary %q declared before its id", v)
			}
			binaries[id] = v
		}
	}

	var cur *dockerEntry
	listKey := ""
	for _, line := range topLevelBlock(src, "dockers") {
		trim := strings.TrimSpace(line)
		indent := len(line) - len(strings.TrimLeft(line, " "))
		if strings.HasPrefix(trim, "- ") && indent <= 2 {
			dockers = append(dockers, dockerEntry{})
			cur = &dockers[len(dockers)-1]
			listKey = ""
			trim = strings.TrimSpace(strings.TrimPrefix(trim, "- "))
		}
		if cur == nil {
			continue
		}
		if strings.HasPrefix(trim, "- ") {
			if listKey == "extra_files" {
				cur.extraFiles = append(cur.extraFiles, inlineList(strings.TrimPrefix(trim, "- "))...)
			} else if listKey == "ids" {
				cur.ids = append(cur.ids, inlineList(strings.TrimPrefix(trim, "- "))...)
			}
			continue
		}
		k, v, ok := strings.Cut(trim, ":")
		if !ok {
			continue
		}
		v = strings.TrimSpace(v)
		listKey = ""
		switch k {
		case "id":
			cur.id = strings.Trim(v, `"'`)
		case "dockerfile":
			cur.dockerfile = strings.Trim(v, `"'`)
		case "ids", "extra_files":
			if v == "" {
				listKey = k
				continue
			}
			if k == "ids" {
				cur.ids = inlineList(v)
			} else {
				cur.extraFiles = inlineList(v)
			}
		}
	}
	return binaries, dockers
}

// GoReleaser builds each image from a temporary directory that contains only
// the Dockerfile, the selected binaries and declared extra_files — not the
// repository. Anything else the Dockerfile reads from the context breaks the
// release build, and neither scratch nor distroless can execute RUN.
func TestDockerfileBuildsInGoReleaserContext(t *testing.T) {
	stages := parseDockerfile(t, readRepoFile(t, "Dockerfile"))
	binaries, dockers := parseGoReleaser(t, readRepoFile(t, ".goreleaser.yaml"))
	if len(dockers) == 0 {
		t.Fatal(".goreleaser.yaml declares no dockers entries")
	}

	names := map[string]bool{}
	for i, st := range stages {
		last := i == len(stages)-1
		if !last && st.image != "scratch" {
			t.Errorf("stage %d (FROM %s): helper stages must be FROM scratch (no extra image pull)", i, st.image)
		}
		for _, in := range st.insts {
			switch in.cmd {
			case "RUN":
				t.Errorf("%q: RUN needs a shell/toolchain the scratch and distroless stages do not have", in.raw)
			case "COPY", "ADD":
				if from, ok := in.flags["from"]; ok && !names[from] {
					t.Errorf("%q: --from=%s is not an earlier stage of this Dockerfile", in.raw, from)
				}
			}
		}
		if st.name != "" {
			names[st.name] = true
		}
	}

	for _, d := range dockers {
		if d.dockerfile != "Dockerfile" {
			t.Errorf("dockers %s: dockerfile = %q, want Dockerfile", d.id, d.dockerfile)
		}
		context := map[string]bool{}
		for _, id := range d.ids {
			bin, ok := binaries[id]
			if !ok {
				t.Errorf("dockers %s: ids references unknown build %q", d.id, id)
				continue
			}
			context[bin] = true
		}
		inContext := func(src string) bool {
			src = path.Clean(src)
			if context[src] {
				return true
			}
			for _, extra := range d.extraFiles {
				extra = path.Clean(extra)
				if src == extra || strings.HasPrefix(src, extra+"/") {
					return true
				}
			}
			return false
		}
		for _, st := range stages {
			for _, in := range st.insts {
				if in.cmd != "COPY" && in.cmd != "ADD" {
					continue
				}
				if _, ok := in.flags["from"]; ok {
					continue
				}
				if len(in.args) < 2 {
					t.Errorf("%q: malformed", in.raw)
					continue
				}
				for _, src := range in.args[:len(in.args)-1] {
					if !inContext(src) {
						t.Errorf("dockers %s: %q reads %q, which is not in the GoReleaser build context (binaries %v, extra_files %v)",
							d.id, in.raw, src, d.ids, d.extraFiles)
					}
				}
			}
		}
		for _, want := range []string{"bb-daemon", "bb-browser"} {
			if !context[want] {
				t.Errorf("dockers %s: build context lacks binary %s", d.id, want)
			}
		}
	}
}
