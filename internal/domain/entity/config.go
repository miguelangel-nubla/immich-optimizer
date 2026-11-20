package entity

type Task struct {
	Name       string   `mapstructure:"name"`
	Extensions []string `mapstructure:"extensions"`
	Command    string   `mapstructure:"command"`
}

// Motion photo policies for the tasks file ("motion_photos").
const (
	// MotionPhotosDropVideo keeps the photo and drops the embedded video
	// before any task runs. This is the default.
	MotionPhotosDropVideo = "drop_video"
	// MotionPhotosKeep hands motion photos to the tasks unchanged.
	MotionPhotosKeep = "keep"
)

type Config struct {
	Tasks []Task `mapstructure:"tasks"`
	// MotionPhotos is MotionPhotosDropVideo (default) or MotionPhotosKeep.
	MotionPhotos string `mapstructure:"motion_photos"`
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
