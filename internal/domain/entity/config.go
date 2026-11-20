package entity

import (
	"encoding/json"
	"time"
)

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
	// Downloads are tasks the proxy runs on an original when it is
	// downloaded, to hand it out in another format.
	Downloads []Task `mapstructure:"downloads"`
	// MotionPhotos is the default for tasks that do not set their own
	// "motion_photos". Empty means MotionPhotosKeep.
	MotionPhotos string `mapstructure:"motion_photos"`
	// LibraryFilter is the "library_filter" block as JSON: a filter of
	// Immich's own asset search, which library mode passes on unchanged.
	// Nil when the tasks file has none.
	LibraryFilter json.RawMessage `mapstructure:"-"`
}

type AppConfig struct {
	ShowVersion      bool
	HealthCheck      bool
	Mode             string
	BindAddr         string
	ProxyAppSync     bool
	ImmichURL        string
	ImmichAPIKey     string
	ImmichAPIKeyFile string
	WatchDir         string
	UndoneDir        string
	UndoneUnmatched  bool
	AuditLogFile     string
	// Library mode.
	LibraryDryRun         bool
	LibraryDryRunSample   int
	LibraryReportFile     string
	ImmichPublicURL       string
	LibraryConcurrency    int
	LibraryReadyTimeout   time.Duration
	LibraryInterval       time.Duration
	ConfigFile            string
	MaxConcurrentRequests int
	ImmichTimeout         time.Duration
	TasksTimeout          time.Duration
	ShutdownTimeout       time.Duration
	InotifyBufferSize     int
	LogLevel              string
	Tasks                 *Config
}
