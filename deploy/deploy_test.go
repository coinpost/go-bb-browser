// Package deploy_test holds static text checks of the container deployment
// files. They do NOT build or run an image: real Docker ownership, volume
// initialisation and compose startup are not exercised here.
package deploy_test

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

// State directory of the official compose example.
const stateDir = "/var/lib/bb-daemon"

// instructions returns a repo file's trimmed, non-blank, non-comment lines.
func instructions(t *testing.T, rel string) []string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join("..", filepath.FromSlash(rel)))
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, l := range strings.Split(string(b), "\n") {
		if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
			out = append(out, l)
		}
	}
	return out
}

// Static check: the image pre-creates the compose state dir owned by the
// nonroot runtime user (uid/gid 65532), so a fresh named volume mounted there
// can inherit writable ownership. The base image has no shell, so the empty
// directory comes from a scratch stage rather than RUN.
func TestStaticDockerfileProvisionsNonrootStateDir(t *testing.T) {
	df := instructions(t, "Dockerfile")
	final := 0
	for i, l := range df {
		if strings.HasPrefix(l, "FROM ") {
			final = i
		}
	}
	helper, image := df[:final], df[final:]

	if want := []string{"FROM scratch AS state-dir", "WORKDIR " + stateDir}; !slices.Equal(helper, want) {
		t.Errorf("helper stage = %q, want exactly %q (empty dir, no RUN, no image pull)", helper, want)
	}
	if want := "COPY --from=state-dir --chown=65532:65532 " + stateDir + " " + stateDir; !slices.Contains(image, want) {
		t.Errorf("final stage lacks %q: a fresh named volume at %s would be root-owned and unwritable for uid 65532", want, stateDir)
	}
	if !slices.Contains(image, "USER nonroot:nonroot") {
		t.Error("final stage must run as USER nonroot:nonroot")
	}

	// GoReleaser builds from an isolated context holding only the binaries.
	var contextSources []string
	for _, l := range df {
		f := strings.Fields(l)
		switch f[0] {
		case "RUN":
			t.Errorf("%q: neither scratch nor distroless can execute RUN", l)
		case "COPY", "ADD":
			if !strings.Contains(l, "--from=") {
				contextSources = append(contextSources, f[1:len(f)-1]...)
			}
		}
	}
	if want := []string{"bb-daemon", "bb-browser"}; !slices.Equal(contextSources, want) {
		t.Errorf("build-context COPY sources = %q, want only %q", contextSources, want)
	}
}

// Static check: the compose example points the daemon and its named volume at
// the directory the image provisions.
func TestStaticComposeUsesProvisionedStateDir(t *testing.T) {
	compose := instructions(t, "deploy/docker-compose.example.yml")
	for _, want := range []string{
		`BB_BROWSER_STATE_DIR: "` + stateDir + `"`,
		"- bb-state:" + stateDir,
	} {
		if !slices.Contains(compose, want) {
			t.Errorf("compose example lacks line %q", want)
		}
	}
}

// Static check: both GoReleaser image entries build this Dockerfile with just
// the two binaries in their context (no extra_files).
func TestStaticGoReleaserDockerContext(t *testing.T) {
	var dockers []string
	in := false
	for _, l := range instructions(t, ".goreleaser.yaml") {
		switch {
		case l == "dockers:":
			in = true
		case l == "docker_manifests:":
			in = false
		case in:
			dockers = append(dockers, l)
		}
	}
	count := func(want string) (n int) {
		for _, l := range dockers {
			if l == want {
				n++
			}
		}
		return n
	}
	for _, want := range []string{"dockerfile: Dockerfile", "ids: [bb-browser, bb-daemon]"} {
		if got := count(want); got != 2 {
			t.Errorf("dockers entries with %q = %d, want 2 (amd64 + arm64)", want, got)
		}
	}
	if slices.ContainsFunc(dockers, func(l string) bool { return strings.HasPrefix(l, "extra_files") }) {
		t.Error("dockers extra_files changes the build context; update the Dockerfile COPY check with it")
	}
}
