package main

import (
	"errors"
	"flag"
	"fmt"
	"strconv"
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
	// LegacyEnv and LegacyFlags are older names still accepted, with a
	// warning at startup. For a duration, the old names took whole seconds.
	LegacyEnv   []string
	LegacyFlags []string
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
		Help:      "What to run: `proxy`, `watcher`, `library`, or several separated by commas",
		LegacyEnv: []string{"IUO_MODE"},
		target:    func(ac *entity.AppConfig) any { return &ac.Mode }},
	{Name: "log_level", Group: groupGeneral, Default: "info",
		Help:      "How much to log: `debug`, `info`, `warn` or `error`",
		LegacyEnv: []string{"IUO_LOG_LEVEL"},
		target:    func(ac *entity.AppConfig) any { return &ac.LogLevel }},
	{Name: "shutdown_timeout", Group: groupGeneral, Default: 8 * time.Second,
		Help:   "On shutdown, how long running jobs get to finish before they are cancelled",
		target: func(ac *entity.AppConfig) any { return &ac.ShutdownTimeout }},

	{Name: "immich_url", Group: groupImmich, Default: "",
		Help:      "Address of the Immich server, for example `http://immich-server:2283`. Required",
		LegacyEnv: []string{"IUO_IMMICH_URL", "IUO_UPSTREAM"}, LegacyFlags: []string{"upstream"},
		target: func(ac *entity.AppConfig) any { return &ac.ImmichURL }},
	{Name: "immich_api_key", Group: groupImmich, Default: "",
		Help:      "Immich API key. Required in watcher and library mode; not used by proxy mode",
		LegacyEnv: []string{"IUO_IMMICH_API_KEY"},
		target:    func(ac *entity.AppConfig) any { return &ac.ImmichAPIKey }},
	{Name: "immich_api_key_file", Group: groupImmich, Default: "",
		Help:      "File that contains the API key, for example a Docker secret. Wins over `immich_api_key`",
		LegacyEnv: []string{"IUO_IMMICH_API_KEY_FILE"},
		target:    func(ac *entity.AppConfig) any { return &ac.ImmichAPIKeyFile }},
	{Name: "immich_public_url", Group: groupImmich, Default: "", DefaultDoc: "same as `IO_IMMICH_URL`",
		Help:   "Address of Immich as you open it in a browser, for example `https://photos.example.com`. Used for the links in the library report",
		target: func(ac *entity.AppConfig) any { return &ac.ImmichPublicURL }},
	{Name: "immich_timeout", Group: groupImmich, Default: 2 * time.Minute,
		Help:      "How long to wait when connecting to Immich, for an upload that stops making progress, and for Immich's answer after an upload. It does not limit how long an upload may take in total",
		LegacyEnv: []string{"IUO_HTTP_TIMEOUT_SECONDS"}, LegacyFlags: []string{"http_timeout_seconds"},
		target: func(ac *entity.AppConfig) any { return &ac.ImmichTimeout }},

	{Name: "tasks_file", Group: groupTasks, Default: "", DefaultDoc: "`./tasks.yaml`, else `" + defaultTasksFile + "`",
		Help:      "The tasks file: which command converts which file type",
		LegacyEnv: []string{"IUO_TASKS_FILE"},
		target:    func(ac *entity.AppConfig) any { return &ac.ConfigFile }},
	{Name: "tasks_concurrency", Group: groupTasks, Default: 10,
		Help:      "How many files are converted at the same time, counted across all modes",
		LegacyEnv: []string{"IUO_MAX_CONCURRENT_REQUESTS"}, LegacyFlags: []string{"max_concurrent_requests"},
		target: func(ac *entity.AppConfig) any { return &ac.MaxConcurrentRequests }},
	{Name: "tasks_timeout", Group: groupTasks, Default: time.Duration(0),
		Help:   "Longest time one file may take to convert; `0` means no limit",
		target: func(ac *entity.AppConfig) any { return &ac.TasksTimeout }},

	{Name: "undone_dir", Group: groupUndone, Default: "/undone",
		Help:      "Folder where files that could not be converted are kept, in every mode",
		LegacyEnv: []string{"IUO_WATCHER_UNDONE_DIR", "IUO_UNDONE_DIR"},
		target:    func(ac *entity.AppConfig) any { return &ac.UndoneDir }},
	{Name: "undone_unmatched", Group: groupUndone, Default: true,
		Help:   "Also keep files whose type no task handles (they are refused either way)",
		target: func(ac *entity.AppConfig) any { return &ac.UndoneUnmatched }},
	{Name: "audit_log_file", Group: groupUndone, Default: "", DefaultDoc: "`audit.log` in the undone folder",
		Help:   "Persistent record of every asset operation",
		target: func(ac *entity.AppConfig) any { return &ac.AuditLogFile }},

	{Name: "proxy_bind_addr", Group: groupProxy, Default: ":2283",
		Help:      "Address and port the proxy listens on",
		LegacyEnv: []string{"IUO_PROXY_BIND_ADDR", "IUO_BIND_ADDR", "IUO_LISTEN"}, LegacyFlags: []string{"bind_addr", "listen"},
		target: func(ac *entity.AppConfig) any { return &ac.BindAddr }},
	{Name: "watcher_dir", Group: groupWatcher, Default: "/watch",
		Help:      "Folder to watch for new files",
		LegacyEnv: []string{"IUO_WATCHER_WATCH_DIR", "IUO_WATCH_DIR"}, LegacyFlags: []string{"watch_dir"},
		target: func(ac *entity.AppConfig) any { return &ac.WatchDir }},
	{Name: "watcher_inotify_buffer_size", Group: groupWatcher, Default: 8192,
		Help:      "Size in bytes of the buffer for file system events",
		LegacyEnv: []string{"IUO_WATCHER_INOTIFY_BUFFER_SIZE", "IUO_INOTIFY_BUFFER_SIZE"}, LegacyFlags: []string{"inotify_buffer_size"},
		target: func(ac *entity.AppConfig) any { return &ac.InotifyBufferSize }},

	{Name: "library_dry_run", Group: groupLibrary, Default: true,
		Help:   "Only report what would be converted and change nothing",
		target: func(ac *entity.AppConfig) any { return &ac.LibraryDryRun }},
	{Name: "library_interval", Group: groupLibrary, Default: time.Duration(0),
		Help:   "Run again this long after each run ends, such as `24h`; `0` runs once and exits",
		target: func(ac *entity.AppConfig) any { return &ac.LibraryInterval }},
	{Name: "library_dry_run_sample", Group: groupLibrary, Default: 20,
		Help:   "In a dry run, how many candidates are downloaded and converted (nothing is uploaded) to estimate the saving and show before/after previews; `0` turns it off",
		target: func(ac *entity.AppConfig) any { return &ac.LibraryDryRunSample }},
	{Name: "library_report_file", Group: groupLibrary, Default: "", DefaultDoc: "`library-report.html` in the undone folder",
		Help:   "Where the HTML report of each run is written",
		target: func(ac *entity.AppConfig) any { return &ac.LibraryReportFile }},
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
		out = append(out, envSetting{Key: s.Name, Env: s.Env(), Legacy: s.LegacyEnv})
	}
	return out
}()

// plainHelp is the help text without the Markdown code marks.
func (s setting) plainHelp() string { return strings.ReplaceAll(s.Help, "`", "") }

// registerSettings defines a flag for every setting, defaulting to its
// environment value, plus the hidden deprecated flag names. The returned
// function, called after parsing, applies deprecated flags that were used and
// reports them.
func registerSettings(fs *flag.FlagSet, v *viper.Viper, ac *entity.AppConfig) (applyLegacy func(visited map[string]bool) []deprecation, envErr error) {
	var envErrs []error
	defer func() { envErr = errors.Join(envErrs...) }()
	type legacy struct {
		old, new string
		copy     func()
	}
	var legacies []legacy
	for _, s := range settings {
		if _, isDuration := s.Default.(time.Duration); !isDuration {
			v.SetDefault(s.Name, s.Default)
		}
		help := s.plainHelp() + " [" + s.Env() + "]"
		switch p := s.target(ac).(type) {
		case *string:
			fs.StringVar(p, s.Name, v.GetString(s.Name), help)
			for _, old := range s.LegacyFlags {
				val := fs.String(old, "", "Deprecated: use -"+s.Name)
				legacies = append(legacies, legacy{old, s.Name, func() { *p = *val }})
			}
		case *int:
			fs.IntVar(p, s.Name, v.GetInt(s.Name), help)
			for _, old := range s.LegacyFlags {
				val := fs.Int(old, 0, "Deprecated: use -"+s.Name)
				legacies = append(legacies, legacy{old, s.Name, func() { *p = *val }})
			}
		case *bool:
			fs.BoolVar(p, s.Name, v.GetBool(s.Name), help)
			for _, old := range s.LegacyFlags {
				val := fs.Bool(old, false, "Deprecated: use -"+s.Name)
				legacies = append(legacies, legacy{old, s.Name, func() { *p = *val }})
			}
		case *time.Duration:
			// Durations use Go's format ("90s", "15m", "1h30m"; "0" is zero).
			// The old names took whole seconds.
			def := s.Default.(time.Duration)
			if raw := v.GetString(s.Name); raw != "" && envSource(s.Name, osLookup) != "" {
				var d time.Duration
				var err error
				if src := envSource(s.Name, osLookup); src != s.Env() {
					var secs int
					secs, err = strconv.Atoi(raw)
					d = time.Duration(secs) * time.Second
				} else {
					d, err = time.ParseDuration(raw)
				}
				if err != nil {
					envErrs = append(envErrs, fmt.Errorf("%s: %q is not a duration (use for example 90s, 15m or 1h30m)", envSource(s.Name, osLookup), raw))
				} else {
					def = d
				}
			}
			fs.DurationVar(p, s.Name, def, help)
			for _, old := range s.LegacyFlags {
				val := fs.Int(old, 0, "Deprecated: use -"+s.Name)
				legacies = append(legacies, legacy{old, s.Name, func() { *p = time.Duration(*val) * time.Second }})
			}
		default:
			panic(fmt.Sprintf("setting %s has unsupported type %T", s.Name, p))
		}
	}
	applyLegacy = func(visited map[string]bool) []deprecation {
		var deps []deprecation
		applied := map[string]bool{}
		for _, l := range legacies {
			if !visited[l.old] {
				continue
			}
			d := deprecation{Old: "-" + l.old, New: "-" + l.new}
			switch {
			case visited[l.new]:
				d.Ignored, d.Winner = true, "-"+l.new
			case applied[l.new]:
				d.Ignored, d.Winner = true, "an earlier deprecated flag"
			default:
				l.copy()
				applied[l.new] = true
			}
			deps = append(deps, d)
		}
		return deps
	}
	return applyLegacy, nil
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

// deprecatedNamesMarkdown renders the table of old names still accepted.
func deprecatedNamesMarkdown() string {
	var b strings.Builder
	b.WriteString("| Old name | Use instead |\n|---|---|\n")
	for _, s := range settings {
		var old []string
		for _, e := range s.LegacyEnv {
			old = append(old, "`"+e+"`")
		}
		for _, f := range s.LegacyFlags {
			old = append(old, "`-"+f+"`")
		}
		if len(old) > 0 {
			fmt.Fprintf(&b, "| %s | `%s`, `-%s` |\n", strings.Join(old, ", "), s.Env(), s.Name)
		}
	}
	return b.String()
}
