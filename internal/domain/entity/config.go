package entity

type Task struct {
	Name       string   `mapstructure:"name"`
	Extensions []string `mapstructure:"extensions"`
	Command    string   `mapstructure:"command"`
}

type Config struct {
	Tasks []Task `mapstructure:"tasks"`
}

type AppConfig struct {
	ShowVersion            bool
	Mode                   string
	BindAddr               string
	FilterPath             string
	FilterFormKey          string
	ImmichURL              string
	ImmichAPIKey           string
	WatchDir               string
	UndoneDir              string
	UndoneUnmatched        bool
	ConfigFile             string
	MaxConcurrentRequests  int
	HTTPTimeoutSeconds     int
	TaskTimeoutSeconds     int
	ShutdownTimeoutSeconds int
	InotifyBufferSize      int
	LogLevel               string
	Tasks                  *Config
}
