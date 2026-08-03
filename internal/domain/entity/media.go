package entity

type ProcessResult struct {
	ProcessedFilePath string
	ProcessedFilename string
	OriginalSize      int64
	ProcessedSize     int64
	Cleanup           func()
}
