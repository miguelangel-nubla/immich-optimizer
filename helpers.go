package main

import (
	"fmt"
	"slices"
	"strings"
)

func humanReadableSize(size int64) string {
	const (
		_  = iota // ignore first value by assigning to blank identifier
		KB = 1 << (10 * iota)
		MB
		GB
		TB
	)

	switch {
	case size >= TB:
		return fmt.Sprintf("%.2f TB", float64(size)/TB)
	case size >= GB:
		return fmt.Sprintf("%.2f GB", float64(size)/GB)
	case size >= MB:
		return fmt.Sprintf("%.2f MB", float64(size)/MB)
	case size >= KB:
		return fmt.Sprintf("%.2f KB", float64(size)/KB)
	default:
		return fmt.Sprintf("%d bytes", size)
	}
}

func normalizeExtension(extension string) string {
	return strings.TrimPrefix(strings.ToLower(extension), ".")
}

func shouldProcessExtension(extension string, tasks []Task) bool {
	checkExt := normalizeExtension(extension)
	for _, task := range tasks {
		if slices.Contains(task.Extensions, checkExt) {
			return true
		}
	}
	return false
}

func trimSuffixCaseInsensitive(str, suffix string) string {
	if strings.HasSuffix(strings.ToLower(str), strings.ToLower(suffix)) {
		return str[:len(str)-len(suffix)]
	}
	return str
}
