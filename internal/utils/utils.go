package utils

import (
	"fmt"
	"slices"
	"strings"
)

func HumanReadableSize(size int64) string {
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

func NormalizeExtension(extension string) string {
	return strings.TrimPrefix(strings.ToLower(extension), ".")
}

func TrimSuffixCaseInsensitive(str, suffix string) string {
	if strings.HasSuffix(strings.ToLower(str), strings.ToLower(suffix)) {
		return str[:len(str)-len(suffix)]
	}
	return str
}

// secretEnv lists the variables of the optimizer's own environment that the
// programs it runs are never given. They parse untrusted media, and the
// Immich API key (in library mode, one with every permission) is of no use
// to them.
var secretEnv = []string{"IO_IMMICH_API_KEY", "IO_IMMICH_API_KEY_FILE"}

// EnvWithoutSecrets is environ (os.Environ) without the optimizer's secrets,
// for a program the optimizer runs.
func EnvWithoutSecrets(environ []string) []string {
	out := make([]string, 0, len(environ))
	for _, kv := range environ {
		name, _, _ := strings.Cut(kv, "=")
		if !slices.Contains(secretEnv, name) {
			out = append(out, kv)
		}
	}
	return out
}
