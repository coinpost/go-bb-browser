package main

import (
	"errors"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

// runArgs is the parsed command line of `bb-browser run`.
type runArgs struct {
	script     string            // first positional token
	positional []string          // adapter positional arguments, in order
	named      map[string]string // adapter --name value / --name=value (last one wins)
	globals    []runGlobal       // CLI flags to apply, in order of appearance
}

// runGlobal is one occurrence of a CLI flag (--url, --tab, --json, --api-token, --help).
type runGlobal struct {
	name, value string
}

// runFlagKind reports whether name is a CLI flag of the run command. noOptDefVal is the
// value the flag takes when written without "=value" (non-empty only for bool flags).
type runFlagKind func(name string) (known bool, noOptDefVal string)

// parseRunArgs splits the raw tokens after `run` (Cobra flag parsing is disabled for that
// command) into CLI flags and adapter arguments without dropping or reordering anything.
//
// Rules:
//   - Before the first bare "--", a --name that is a CLI flag is a CLI flag (reserved names
//     win). "-h" is --help. After the "--" every --name belongs to the adapter.
//   - Any other --name value / --name=value is an adapter named argument. The value is passed
//     verbatim (may be empty, may contain "="); the split form needs a next token that does
//     not start with "--", otherwise: flag --name needs a value.
//   - Everything else (including single-dash tokens such as -5) is positional; the first
//     positional token is the script path.
func parseRunArgs(tokens []string, kind runFlagKind) (runArgs, error) {
	out := runArgs{named: map[string]string{}}
	haveScript := false
	separated := false
	for i := 0; i < len(tokens); i++ {
		tok := tokens[i]
		if !separated {
			if tok == "--" {
				separated = true
				continue
			}
			if tok == "-h" {
				out.globals = append(out.globals, runGlobal{"help", "true"})
				continue
			}
		}
		if !strings.HasPrefix(tok, "--") {
			if haveScript {
				out.positional = append(out.positional, tok)
			} else {
				out.script, haveScript = tok, true
			}
			continue
		}
		name, value, hasValue := strings.Cut(tok[2:], "=")
		if name == "" {
			return out, fmt.Errorf("flag needs a name: %s", tok)
		}
		if !separated {
			if known, noOptDefVal := kind(name); known {
				switch {
				case hasValue:
				case noOptDefVal != "":
					value = noOptDefVal
				case i+1 < len(tokens):
					// Same as pflag: a value-taking flag consumes the next token whatever it is.
					i++
					value = tokens[i]
				default:
					return out, fmt.Errorf("flag needs an argument: --%s", name)
				}
				out.globals = append(out.globals, runGlobal{name, value})
				continue
			}
		}
		if !hasValue {
			if i+1 >= len(tokens) || strings.HasPrefix(tokens[i+1], "--") {
				return out, fmt.Errorf("flag --%s needs a value", name)
			}
			i++
			value = tokens[i]
		}
		out.named[name] = value
	}
	return out, nil
}

// parseRunCommand parses tokens for the run command and applies the CLI flags found among
// them through pflag (type checking, Changed tracking). help reports that --help/-h was given.
func parseRunCommand(cmd *cobra.Command, tokens []string) (parsed runArgs, help bool, err error) {
	flags := cmd.Flags()
	flags.AddFlagSet(cmd.InheritedFlags())
	parsed, err = parseRunArgs(tokens, func(name string) (bool, string) {
		f := flags.Lookup(name)
		if f == nil {
			return false, ""
		}
		return true, f.NoOptDefVal
	})
	if err != nil {
		return parsed, false, err
	}
	for _, g := range parsed.globals {
		if err := flags.Set(g.name, g.value); err != nil {
			return parsed, false, err
		}
	}
	if help, _ = flags.GetBool("help"); help {
		return parsed, true, nil
	}
	if strings.TrimSpace(parsed.script) == "" {
		return parsed, false, errors.New("missing script path: usage: bb-browser run SCRIPT.js [args...]")
	}
	return parsed, false, nil
}
