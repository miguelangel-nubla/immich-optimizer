package entity

import "time"

type FileEvent struct {
	Path      string
	Timestamp time.Time
}
