package utils

import (
	"path/filepath"
	"strings"
)

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
