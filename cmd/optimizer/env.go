package main

import (
	"fmt"
	"os"
	"strings"

	"github.com/spf13/viper"
)

// envSetting maps a viper key to its canonical IO_* variable and the legacy
// names still accepted for one transition period (in priority order).
type envSetting struct {
	Key    string
	Env    string
	Legacy []string
}

// deprecation records one use of a deprecated env var or flag.
type deprecation struct {
	Old, New string
	// Ignored is true when the replacement (or a higher-priority legacy name)
	// was also set, so this one had no effect.
	Ignored bool
	// Winner names the setting that took effect when Ignored.
	Winner string
}

type lookupFunc func(string) (string, bool)

// nonEmpty adapts a lookup so that empty values count as unset, matching viper.
func nonEmpty(lookup lookupFunc) lookupFunc {
	return func(k string) (string, bool) {
		v, ok := lookup(k)
		return v, ok && v != ""
	}
}

// bindEnv binds Env then Legacy; viper takes the first non-empty one, so IO_* wins.
func bindEnv(v *viper.Viper) error {
	for _, s := range envSettings {
		names := append([]string{s.Key, s.Env}, s.Legacy...)
		if err := v.BindEnv(names...); err != nil {
			return fmt.Errorf("bind env for %s: %w", s.Key, err)
		}
	}
	return nil
}

// legacyEnvUsage reports every non-empty legacy variable.
func legacyEnvUsage(lookup lookupFunc) []deprecation {
	lookup = nonEmpty(lookup)
	var out []deprecation
	for _, s := range envSettings {
		winner := ""
		if _, ok := lookup(s.Env); ok {
			winner = s.Env
		}
		for _, old := range s.Legacy {
			if _, ok := lookup(old); !ok {
				continue
			}
			d := deprecation{Old: old, New: s.Env}
			if winner != "" {
				d.Ignored = true
				d.Winner = winner
			} else {
				winner = old
			}
			out = append(out, d)
		}
	}
	return out
}

// envSource returns the name of the env var that supplied key ("" if none).
func envSource(key string, lookup lookupFunc) string {
	lookup = nonEmpty(lookup)
	for _, s := range envSettings {
		if s.Key != key {
			continue
		}
		for _, name := range append([]string{s.Env}, s.Legacy...) {
			if _, ok := lookup(name); ok {
				return name
			}
		}
	}
	return ""
}

// allEnvNames lists every canonical and legacy variable name.
func allEnvNames() []string {
	var names []string
	for _, s := range envSettings {
		names = append(names, s.Env)
		names = append(names, s.Legacy...)
	}
	return names
}

// formatDeprecations renders a single-line startup warning.
func formatDeprecations(d []deprecation) string {
	if len(d) == 0 {
		return ""
	}
	parts := make([]string, 0, len(d))
	for _, x := range d {
		p := x.Old + " -> " + x.New
		if x.Ignored {
			p += fmt.Sprintf(" (ignored, %s is set)", x.Winner)
		}
		parts = append(parts, p)
	}
	return "Deprecated configuration in use (IUO_* variables and old flag names will be removed in a future release): " +
		strings.Join(parts, "; ")
}

var osLookup lookupFunc = os.LookupEnv
