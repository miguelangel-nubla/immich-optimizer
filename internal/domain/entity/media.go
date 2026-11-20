package entity

type ProcessResult struct {
	ProcessedFilePath string
	ProcessedFilename string
	OriginalSize      int64
	ProcessedSize     int64
	Cleanup           func()
	// Task is the name of the task that produced the output.
	Task string
	// MotionPhotos is the task's motion photo setting when the input was a
	// motion photo that was split for it ("drop_video" or "drop_photo").
	MotionPhotos string
}
