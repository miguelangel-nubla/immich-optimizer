package entity

import "context"

// TaskInfo is what the caller of a conversion knows about the file, for the
// task command's environment. The command itself only ever sees a copy under
// a temporary name.
type TaskInfo struct {
	// OriginalName is the file's name as it was uploaded, or as it is
	// stored in Immich.
	OriginalName string
	// Source is where the file comes from: proxy, watcher, library or
	// download.
	Source string
}

type taskInfoKey struct{}

// WithTaskInfo attaches the info to a context handed to a MediaProcessor.
func WithTaskInfo(ctx context.Context, info TaskInfo) context.Context {
	return context.WithValue(ctx, taskInfoKey{}, info)
}

// TaskInfoFrom returns the info attached with WithTaskInfo.
func TaskInfoFrom(ctx context.Context) TaskInfo {
	info, _ := ctx.Value(taskInfoKey{}).(TaskInfo)
	return info
}

// SourceDownload is the Source of a file converted on its way out.
const SourceDownload = "download"
