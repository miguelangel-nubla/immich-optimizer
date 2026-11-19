package port

type FileSystem interface {
	RemoveFile(filePath string) error
}
