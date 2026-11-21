package main

import (
	"fmt"
	"os"

	"github.com/spf13/viper"
)

// envSetting maps a viper key to its IO_* variable.
type envSetting struct {
	Key string
	Env string
}

type lookupFunc func(string) (string, bool)

// bindEnv binds each setting to its IO_* variable.
func bindEnv(v *viper.Viper) error {
	for _, s := range envSettings {
		if err := v.BindEnv(s.Key, s.Env); err != nil {
			return fmt.Errorf("bind env for %s: %w", s.Key, err)
		}
	}
	return nil
}

// envSource returns the name of the env var that supplied key ("" if none).
// Empty values count as unset, matching viper.
func envSource(key string, lookup lookupFunc) string {
	for _, s := range envSettings {
		if s.Key != key {
			continue
		}
		if v, ok := lookup(s.Env); ok && v != "" {
			return s.Env
		}
	}
	return ""
}

// allEnvNames lists every variable name.
func allEnvNames() []string {
	names := make([]string, 0, len(envSettings))
	for _, s := range envSettings {
		names = append(names, s.Env)
	}
	return names
}

var osLookup lookupFunc = os.LookupEnv
