package utils

import (
	"path/filepath"
	"strings"
)

// UndoneNoteSuffix named the error notes of the pre-v2 undone layout. Such
// notes are still ignored if a user moves one into the watch directory.
const UndoneNoteSuffix = ".error.json"

// UndoneTempPrefix prefixes in-progress undone store temp files.
const UndoneTempPrefix = ".io-undone-"

func IsTempFile(filePath string) bool {
	ext := strings.ToLower(filepath.Ext(filePath))
	switch ext {
	case ".tmp", ".temp", ".part", ".crdownload", ".tacitpart", ".pending":
		return true
	}
	if strings.HasPrefix(ext, ".syncthing") {
		return true
	}
	return false
}

// IsUndoneArtifact reports whether path looks like a file written by the
// undone store itself (an in-progress temp entry or a legacy error note), so
// that it is never treated as media to ingest.
func IsUndoneArtifact(filePath string) bool {
	base := filepath.Base(filePath)
	if strings.HasSuffix(strings.ToLower(base), UndoneNoteSuffix) {
		return true
	}
	return strings.HasPrefix(base, UndoneTempPrefix)
}
