package library

import (
	"path/filepath"
	"strings"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// What became of an asset in a run.
const (
	statusReplaced   = "replaced"
	statusKept       = "kept"
	statusSkipped    = "skipped"
	statusFailed     = "failed"
	statusFinalized  = "finalized"
	statusRolledBack = "rolled_back"
	statusSecondCopy = "second_copy"
	statusMarkedDone = "marked_done"
)

// Item is what became of one asset in a pass.
type Item struct {
	ID     string `json:"id"`
	File   string `json:"file"`
	Type   string `json:"type"`
	Ext    string `json:"ext"`
	Date   string `json:"date,omitempty"`
	Size   int64  `json:"size"`
	Status string `json:"status"`
	// Reason says why it was skipped, kept or failed.
	Reason string `json:"reason,omitempty"`
	// NewID and NewSize describe the copy that replaced it.
	NewID   string `json:"new_id,omitempty"`
	NewFile string `json:"new_file,omitempty"`
	NewSize int64  `json:"new_size,omitempty"`
	Task    string `json:"task,omitempty"`

	// retry and review carry errSkip's flags to the tags at the end.
	retry, review bool
}

// fill sets what the listing knows about the asset, without overwriting what
// working on it found out.
func (it *Item) fill(a entity.LibraryAsset) {
	it.ID, it.File, it.Type = a.ID, a.OriginalFileName, a.Type
	it.Ext = strings.ToLower(strings.TrimPrefix(filepath.Ext(a.OriginalFileName), "."))
	if !a.FileCreatedAt.IsZero() {
		it.Date = a.FileCreatedAt.UTC().Format("2006-01-02")
	}
	if it.Size == 0 {
		it.Size = a.Size
	}
}
