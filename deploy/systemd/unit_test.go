// Package systemd holds static regression checks for the shipped user unit.
package systemd

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

const unitFile = "bb-daemon.service"

// parseUnit returns section -> key -> values for the effective (non-comment)
// settings of a systemd unit. It covers only the syntax this unit uses: section
// headers, key=value assignments, and "#" / ";" comment lines. As in systemd, an
// empty assignment resets the values collected so far for that key.
func parseUnit(data string) (map[string]map[string][]string, error) {
	sections := map[string]map[string][]string{}
	section := ""
	for i, raw := range strings.Split(data, "\n") {
		line := strings.TrimSpace(raw)
		if line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";") {
			continue
		}
		if strings.HasPrefix(line, "[") {
			if !strings.HasSuffix(line, "]") {
				return nil, fmt.Errorf("line %d: malformed section header %q", i+1, line)
			}
			section = line[1 : len(line)-1]
			if sections[section] == nil {
				sections[section] = map[string][]string{}
			}
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			return nil, fmt.Errorf("line %d: expected key=value, got %q", i+1, line)
		}
		if section == "" {
			return nil, fmt.Errorf("line %d: assignment %q outside any section", i+1, line)
		}
		key, value = strings.TrimSpace(key), strings.TrimSpace(value)
		if value == "" {
			delete(sections[section], key)
			continue
		}
		sections[section][key] = append(sections[section][key], value)
	}
	return sections, nil
}

// checkUnit verifies what `systemctl --user enable` and the README rely on.
func checkUnit(data string) error {
	sections, err := parseUnit(data)
	if err != nil {
		return err
	}

	install, ok := sections["Install"]
	if !ok {
		return fmt.Errorf("missing [Install] section: `systemctl --user enable` cannot create a wants link")
	}
	// A user unit must hook into the user manager's default.target;
	// multi-user.target does not exist there.
	var wantedBy []string
	for _, v := range install["WantedBy"] {
		wantedBy = append(wantedBy, strings.Fields(v)...)
	}
	if len(wantedBy) != 1 || wantedBy[0] != "default.target" {
		return fmt.Errorf("[Install] WantedBy= must be exactly default.target, got %q", wantedBy)
	}

	if len(sections["Unit"]["Description"]) == 0 {
		return fmt.Errorf("[Unit] must set a non-empty Description=")
	}
	if len(sections["Service"]["ExecStart"]) == 0 {
		return fmt.Errorf("[Service] must keep ExecStart=")
	}
	return nil
}

func TestUnitIsEnableable(t *testing.T) {
	data, err := os.ReadFile(unitFile)
	if err != nil {
		t.Fatalf("read unit: %v", err)
	}
	if err := checkUnit(string(data)); err != nil {
		t.Fatalf("%s: %v", unitFile, err)
	}
}

// TestCheckUnitRejects keeps the check honest: empty files, comments and
// settings in the wrong section must not pass.
func TestCheckUnitRejects(t *testing.T) {
	const (
		unit    = "[Unit]\nDescription=bb-daemon\n"
		service = "[Service]\nExecStart=/bin/true\n"
	)
	if err := checkUnit(unit + service + "[Install]\nWantedBy=default.target\n"); err != nil {
		t.Fatalf("valid unit rejected: %v", err)
	}

	cases := map[string]string{
		"empty file":              "",
		"comments only":           "# [Install]\n# WantedBy=default.target\n",
		"no install section":      unit + service,
		"empty install section":   unit + service + "[Install]\n",
		"commented WantedBy":      unit + service + "[Install]\n# WantedBy=default.target\n;WantedBy=default.target\n",
		"WantedBy in service":     unit + service + "WantedBy=default.target\n[Install]\n",
		"WantedBy in unit":        unit + "WantedBy=default.target\n" + service + "[Install]\n",
		"wrong target":            unit + service + "[Install]\nWantedBy=multi-user.target\n",
		"extra target":            unit + service + "[Install]\nWantedBy=default.target multi-user.target\n",
		"extra target, own line":  unit + service + "[Install]\nWantedBy=default.target\nWantedBy=multi-user.target\n",
		"reset by empty value":    unit + service + "[Install]\nWantedBy=default.target\nWantedBy=\n",
		"similar key":             unit + service + "[Install]\nRequiredBy=default.target\n",
		"similar section":         unit + service + "[Installer]\nWantedBy=default.target\n",
		"missing description":     service + "[Install]\nWantedBy=default.target\n",
		"empty description":       "[Unit]\nDescription=\n" + service + "[Install]\nWantedBy=default.target\n",
		"description in service":  service + "Description=bb-daemon\n[Install]\nWantedBy=default.target\n",
		"missing ExecStart":       unit + "[Service]\nType=simple\n[Install]\nWantedBy=default.target\n",
		"malformed section":       unit + service + "[Install\nWantedBy=default.target\n",
		"assignment before start": "WantedBy=default.target\n" + unit + service,
	}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if err := checkUnit(data); err == nil {
				t.Fatalf("checkUnit accepted invalid unit:\n%s", data)
			}
		})
	}
}
