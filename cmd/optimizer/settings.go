package main

import (
	"errors"
	"flag"
	"fmt"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
	"github.com/spf13/viper"
)

// setting describes one configuration value. This table is the single source
// of truth: the flags, the environment variables, -help and the settings
// reference in docs/configuration.md are all generated from it.
//
// Naming rule: a setting is called <group>_<what>. Its flag is -<name> and its
// environment variable is IO_<NAME>. Settings that apply to everything have
// no group prefix.
type setting struct {
	Name    string
	Group   string
	Default any // string, int, bool or time.Duration
	Help    string
	// DefaultDoc replaces the default shown in the docs when it is computed.
	DefaultDoc string
	// target returns where the value is stored.
	target func(ac *entity.AppConfig) any
}

// Env returns the setting's environment variable.
func (s setting) Env() string { return "IO_" + strings.ToUpper(s.Name) }

const (
	groupGeneral = "General"
	groupImmich  = "Immich"
	groupTasks   = "Tasks"
	groupUndone  = "Undone folder and audit log"
	groupProxy   = "Proxy mode"
	groupWatcher = "Watcher mode"
	groupLibrary = "Library mode"
)

// settingGroups is the order groups appear in -help and in the docs.
var settingGroups = []string{groupGeneral, groupImmich, groupTasks, groupUndone, groupProxy, groupWatcher, groupLibrary}

var settings = []setting{
	{Name: "mode", Group: groupGeneral, Default: "", DefaultDoc: "none: it has to be set",
		Help:   "What to run: `proxy`, `watcher`, `library`, or several separated by commas",
		target: func(ac *entity.AppConfig) any { return &ac.Mode }},
	{Name: "log_level", Group: groupGeneral, Default: "info",
		Help:   "How much to log: `debug`, `info`, `warn` or `error`",
		target: func(ac *entity.AppConfig) any { return &ac.LogLevel }},
	{Name: "shutdown_timeout", Group: groupGeneral, Default: 8 * time.Second,
		Help:   "On shutdown, how long running jobs get to finish before they are cancelled",
		target: func(ac *entity.AppConfig) any { return &ac.ShutdownTimeout }},

	{Name: "immich_url", Group: groupImmich, Default: "",
		Help:   "Address of the Immich server, for example `http://immich-server:2283`. Required",
		target: func(ac *entity.AppConfig) any { return &ac.ImmichURL }},
	{Name: "immich_api_key", Group: groupImmich, Default: "",
		Help:   "Immich API key. Required in watcher and library mode; not used by proxy mode",
		target: func(ac *entity.AppConfig) any { return &ac.ImmichAPIKey }},
	{Name: "immich_api_key_file", Group: groupImmich, Default: "",
		Help:   "File that contains the API key, for example a Docker secret. Wins over `immich_api_key`",
		target: func(ac *entity.AppConfig) any { return &ac.ImmichAPIKeyFile }},
	{Name: "immich_public_url", Group: groupImmich, Default: "", DefaultDoc: "same as `IO_IMMICH_URL`",
		Help:   "Address of Immich as you open it in a browser, for example `https://photos.example.com`. Used for the links in the library report",
		target: func(ac *entity.AppConfig) any { return &ac.ImmichPublicURL }},
	{Name: "immich_timeout", Group: groupImmich, Default: 2 * time.Minute,
		Help:   "How long to wait when connecting to Immich, for an upload that stops making progress, and for Immich's answer after an upload. It does not limit how long an upload may take in total",
		target: func(ac *entity.AppConfig) any { return &ac.ImmichTimeout }},

	{Name: "tasks_file", Group: groupTasks, Default: "", DefaultDoc: "`./tasks.yaml`, else `" + defaultTasksFile + "`",
		Help:   "The tasks file: which command converts which file type",
		target: func(ac *entity.AppConfig) any { return &ac.ConfigFile }},
	{Name: "tasks_concurrency", Group: groupTasks, Default: 10,
		Help:   "How many files are converted at the same time, counted across all modes",
		target: func(ac *entity.AppConfig) any { return &ac.MaxConcurrentRequests }},
	{Name: "tasks_timeout", Group: groupTasks, Default: time.Duration(0),
		Help:   "Longest time one file may take to convert; `0` means no limit",
		target: func(ac *entity.AppConfig) any { return &ac.TasksTimeout }},

	{Name: "undone_dir", Group: groupUndone, Default: "/undone",
		Help:   "Folder where files that could not be converted are kept, in every mode",
		target: func(ac *entity.AppConfig) any { return &ac.UndoneDir }},
	{Name: "undone_unmatched", Group: groupUndone, Default: true,
		Help:   "Also keep files whose type no task handles (they are refused either way)",
		target: func(ac *entity.AppConfig) any { return &ac.UndoneUnmatched }},
	{Name: "audit_log_file", Group: groupUndone, Default: "", DefaultDoc: "`audit.log` in the undone folder",
		Help:   "Persistent record of every asset operation",
		target: func(ac *entity.AppConfig) any { return &ac.AuditLogFile }},

	{Name: "proxy_bind_addr", Group: groupProxy, Default: ":2283",
		Help:   "Address and port the proxy listens on",
		target: func(ac *entity.AppConfig) any { return &ac.BindAddr }},
	{Name: "watcher_dir", Group: groupWatcher, Default: "/watch",
		Help:   "Folder to watch for new files",
		target: func(ac *entity.AppConfig) any { return &ac.WatchDir }},
	{Name: "watcher_inotify_buffer_size", Group: groupWatcher, Default: 8192,
		Help:   "Size in bytes of the buffer for file system events",
		target: func(ac *entity.AppConfig) any { return &ac.InotifyBufferSize }},

	{Name: "library_concurrency", Group: groupLibrary, Default: 1,
		Help:   "How many assets are replaced at the same time. Each one makes Immich generate thumbnails and analyse a new file",
		target: func(ac *entity.AppConfig) any { return &ac.LibraryConcurrency }},
	{Name: "library_ready_timeout", Group: groupLibrary, Default: 10 * time.Minute,
		Help:   "How long to wait for Immich to finish reading a converted asset",
		target: func(ac *entity.AppConfig) any { return &ac.LibraryReadyTimeout }},
}

// envSettings is the environment view of the settings table.
var envSettings = func() []envSetting {
	out := make([]envSetting, 0, len(settings))
	for _, s := range settings {
		out = append(out, envSetting{Key: s.Name, Env: s.Env()})
	}
	return out
}()

// plainHelp is the help text without the Markdown code marks.
func (s setting) plainHelp() string { return strings.ReplaceAll(s.Help, "`", "") }

// registerSettings defines a flag for every setting, defaulting to its
// environment value.
func registerSettings(fs *flag.FlagSet, v *viper.Viper, ac *entity.AppConfig) error {
	var envErrs []error
	for _, s := range settings {
		if _, isDuration := s.Default.(time.Duration); !isDuration {
			v.SetDefault(s.Name, s.Default)
		}
		help := s.plainHelp() + " [" + s.Env() + "]"
		switch p := s.target(ac).(type) {
		case *string:
			fs.StringVar(p, s.Name, v.GetString(s.Name), help)
		case *int:
			fs.IntVar(p, s.Name, v.GetInt(s.Name), help)
		case *bool:
			fs.BoolVar(p, s.Name, v.GetBool(s.Name), help)
		case *time.Duration:
			// Durations use Go's format ("90s", "15m", "1h30m"; "0" is zero).
			def := s.Default.(time.Duration)
			if raw := v.GetString(s.Name); raw != "" && envSource(s.Name, osLookup) != "" {
				d, err := time.ParseDuration(raw)
				if err != nil {
					envErrs = append(envErrs, fmt.Errorf("%s: %q is not a duration (use for example 90s, 15m or 1h30m)", s.Env(), raw))
				} else {
					def = d
				}
			}
			fs.DurationVar(p, s.Name, def, help)
		default:
			panic(fmt.Sprintf("setting %s has unsupported type %T", s.Name, p))
		}
	}
	return errors.Join(envErrs...)
}

// shortDuration prints a duration without zero parts: "2m", not "2m0s".
func shortDuration(d time.Duration) string {
	if d == 0 {
		return "0"
	}
	s := d.String()
	s = strings.TrimSuffix(s, "0s")
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	if s == "" {
		return d.String()
	}
	return s
}

// settingsMarkdown renders the settings reference for docs/configuration.md.
func settingsMarkdown() string {
	var b strings.Builder
	for _, g := range settingGroups {
		fmt.Fprintf(&b, "### %s\n\n| Environment variable | Flag | Default | Description |\n|---|---|---|---|\n", g)
		for _, s := range settings {
			if s.Group != g {
				continue
			}
			def := s.DefaultDoc
			if def == "" {
				switch d := s.Default.(type) {
				case string:
					if d != "" {
						def = "`" + d + "`"
					}
				case time.Duration:
					def = "`" + shortDuration(d) + "`"
				default:
					def = fmt.Sprintf("`%v`", d)
				}
			}
			help := s.Help
			if _, ok := s.Default.(time.Duration); ok {
				help += ". A [duration](#durations)"
			}
			fmt.Fprintf(&b, "| `%s` | `-%s` | %s | %s |\n", s.Env(), s.Name, def, help)
		}
		b.WriteString("\n")
	}
	return b.String()
}
