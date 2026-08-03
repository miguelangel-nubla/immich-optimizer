package port

type FileSystem interface {
	MoveToUndone(filePath string) error
	RemoveFile(filePath string) error
}
