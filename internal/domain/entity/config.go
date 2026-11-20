package entity

type Task struct {
	Name       string   `mapstructure:"name"`
	Extensions []string `mapstructure:"extensions"`
	Command    string   `mapstructure:"command"`
	// MotionPhotos says what the command receives when the file is a motion
	// photo. Empty in the tasks file means "use the top-level default";
	// after loading it always holds one of the MotionPhotos* values.
	MotionPhotos string `mapstructure:"motion_photos"`
}

// Values of "motion_photos" in the tasks file: what a task command receives
// when the input is a motion photo (a photo with an embedded video).
const (
	// MotionPhotosKeep passes the file as it is. This is the default.
	MotionPhotosKeep = "keep"
	// MotionPhotosDropVideo passes only the photo.
	MotionPhotosDropVideo = "drop_video"
	// MotionPhotosDropPhoto passes only the video.
	MotionPhotosDropPhoto = "drop_photo"
)

// ValidMotionPhotos reports whether v is one of the MotionPhotos* values.
func ValidMotionPhotos(v string) bool {
	return v == MotionPhotosKeep || v == MotionPhotosDropVideo || v == MotionPhotosDropPhoto
}

type Config struct {
	Tasks []Task `mapstructure:"tasks"`
	// MotionPhotos is the default for tasks that do not set their own
	// "motion_photos". Empty means MotionPhotosKeep.
	MotionPhotos string `mapstructure:"motion_photos"`
}

type AppConfig struct {
	ShowVersion      bool
	Mode             string
	BindAddr         string
	FilterPath       string
	FilterFormKey    string
	ImmichURL        string
	ImmichAPIKey     string
	ImmichAPIKeyFile string
	WatchDir         string
	UndoneDir        string
	UndoneUnmatched  bool
	AuditLogFile     string
	// Library mode.
	LibraryDryRun              bool
	LibraryLimit               int
	LibraryTakenBefore         string
	LibraryReadyTimeoutSeconds int
	ConfigFile                 string
	MaxConcurrentRequests      int
	HTTPTimeoutSeconds         int
	TaskTimeoutSeconds         int
	ShutdownTimeoutSeconds     int
	InotifyBufferSize          int
	LogLevel                   string
	Tasks                      *Config
}
