package utils

import (
	"testing"
)

func TestHumanReadableSize(t *testing.T) {
	tests := []struct {
		size     int64
		expected string
	}{
		{500, "500 bytes"},
		{1024, "1.00 KB"},
		{1536, "1.50 KB"},
		{1048576, "1.00 MB"},
		{1073741824, "1.00 GB"},
		{1099511627776, "1.00 TB"},
	}

	for _, tt := range tests {
		result := HumanReadableSize(tt.size)
		if result != tt.expected {
			t.Errorf("HumanReadableSize(%d) = %q; expected %q", tt.size, result, tt.expected)
		}
	}
}

func TestNormalizeExtension(t *testing.T) {
	tests := []struct {
		ext      string
		expected string
	}{
		{".JPG", "jpg"},
		{"png", "png"},
		{".tar.gz", "tar.gz"},
		{"", ""},
	}

	for _, tt := range tests {
		result := NormalizeExtension(tt.ext)
		if result != tt.expected {
			t.Errorf("NormalizeExtension(%q) = %q; expected %q", tt.ext, result, tt.expected)
		}
	}
}

func TestTrimSuffixCaseInsensitive(t *testing.T) {
	tests := []struct {
		str      string
		suffix   string
		expected string
	}{
		{"image.JPG", ".jpg", "image"},
		{"image.jpg", ".JPG", "image"},
		{"IMAGE.PNG", ".png", "IMAGE"},
		{"image.gif", ".jpg", "image.gif"},
		{"nosuffix", ".txt", "nosuffix"},
	}

	for _, tt := range tests {
		result := TrimSuffixCaseInsensitive(tt.str, tt.suffix)
		if result != tt.expected {
			t.Errorf("TrimSuffixCaseInsensitive(%q, %q) = %q; expected %q", tt.str, tt.suffix, result, tt.expected)
		}
	}
}
