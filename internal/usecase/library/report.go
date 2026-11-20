package library

import (
	"context"
	_ "embed"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"math/rand/v2"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/miguelangel-nubla/immich-optimizer/internal/domain/entity"
)

// What became of an asset in a run.
const (
	statusWouldConvert = "would_convert"
	statusReplaced     = "replaced"
	statusKept         = "kept"
	statusSkipped      = "skipped"
	statusFailed       = "failed"
	statusFinalized    = "finalized"
	statusRolledBack   = "rolled_back"
	statusSecondCopy   = "second_copy"
	statusWouldTrash   = "would_trash"
)

// Item is one asset's row in the report.
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
	NewID   string  `json:"new_id,omitempty"`
	NewFile string  `json:"new_file,omitempty"`
	NewSize int64   `json:"new_size,omitempty"`
	Task    string  `json:"task,omitempty"`
	Sample  *Sample `json:"sample,omitempty"`
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

// Sample is the result of converting one candidate during a dry run. Nothing
// of it reaches Immich.
type Sample struct {
	// Result is "smaller", "not_smaller" or "failed".
	Result  string `json:"result"`
	NewSize int64  `json:"new_size,omitempty"`
	NewName string `json:"new_name,omitempty"`
	Task    string `json:"task,omitempty"`
	Error   string `json:"error,omitempty"`
	// Before and After are small JPEG previews as data URIs.
	Before string `json:"before,omitempty"`
	After  string `json:"after,omitempty"`
}

// Estimate extrapolates the sample to all candidates.
type Estimate struct {
	Sampled    int `json:"sampled"`
	Smaller    int `json:"smaller"`
	NotSmaller int `json:"not_smaller"`
	Failed     int `json:"failed"`
	// SampleBefore and SampleAfter are the bytes of the sampled files that
	// converted, before and after (a file that did not get smaller counts
	// with its original size, because the original would be kept).
	SampleBefore int64 `json:"sample_before"`
	SampleAfter  int64 `json:"sample_after"`
	// CandidateBytes is the size of everything that would be converted, and
	// EstimatedAfter what it is expected to shrink to.
	CandidateBytes int64 `json:"candidate_bytes"`
	EstimatedAfter int64 `json:"estimated_after"`
}

// sample converts a random selection of the candidates of a dry run.
func (s *Service) sample(ctx context.Context, items []*Item) *Estimate {
	var candidates []*Item
	for _, it := range items {
		if it.Status == statusWouldConvert {
			candidates = append(candidates, it)
		}
	}
	if len(candidates) == 0 {
		return nil
	}
	picked := append([]*Item(nil), candidates...)
	rand.Shuffle(len(picked), func(i, j int) { picked[i], picked[j] = picked[j], picked[i] })
	if len(picked) > s.cfg.Sample {
		picked = picked[:s.cfg.Sample]
	}
	s.infof("Library (dry run): converting a sample of %d of %d candidate(s) to estimate the saving; nothing is uploaded", len(picked), len(candidates))

	est := &Estimate{}
	type ratio struct{ before, after int64 }
	byExt := map[string]*ratio{}
	for _, it := range picked {
		if ctx.Err() != nil {
			break
		}
		sm := s.sampleOne(ctx, it)
		it.Sample = sm
		est.Sampled++
		after := it.Size
		switch sm.Result {
		case "smaller":
			est.Smaller++
			after = sm.NewSize
		case "not_smaller":
			est.NotSmaller++
		default:
			est.Failed++
			continue // a failed asset stays as it is and says nothing about size
		}
		est.SampleBefore += it.Size
		est.SampleAfter += after
		r := byExt[it.Ext]
		if r == nil {
			r = &ratio{}
			byExt[it.Ext] = r
		}
		r.before += it.Size
		r.after += after
		// Recorded like every other operation, clearly marked as a dry run.
		_ = s.record(Entry{Event: "sampled", Asset: it.ID, File: it.File, Outcome: sm.Result,
			OriginalBytes: it.Size, ConvertedBytes: sm.NewSize, Task: sm.Task})
	}

	// Each candidate is scaled by what its own file type did in the sample,
	// or by the overall result when that type was not sampled.
	overall := 1.0
	if est.SampleBefore > 0 {
		overall = float64(est.SampleAfter) / float64(est.SampleBefore)
	}
	for _, it := range candidates {
		est.CandidateBytes += it.Size
		f := overall
		if r := byExt[it.Ext]; r != nil && r.before > 0 {
			f = float64(r.after) / float64(r.before)
		}
		est.EstimatedAfter += int64(float64(it.Size) * f)
	}
	s.infof("Library (dry run): sample of %d: %d smaller, %d not smaller, %d failed; estimated %d -> %d bytes for all %d candidate(s)",
		est.Sampled, est.Smaller, est.NotSmaller, est.Failed, est.CandidateBytes, est.EstimatedAfter, len(candidates))
	return est
}

func (s *Service) sampleOne(ctx context.Context, it *Item) *Sample {
	sm := &Sample{Result: "failed"}
	work, err := os.MkdirTemp("", "io-sample-*")
	if err != nil {
		sm.Error = err.Error()
		return sm
	}
	defer os.RemoveAll(work)

	ext := filepath.Ext(it.File)
	if strings.ContainsAny(ext, `/\`) || len(ext) > 32 {
		ext = ""
	}
	src := filepath.Join(work, "original"+ext)
	sum, err := s.api.DownloadOriginal(ctx, it.ID, src)
	if err != nil {
		sm.Error = "downloading the original: " + err.Error()
		return sm
	}
	sm.Before = s.preview(ctx, src)

	in := entity.ConvertInput{Path: src, Filename: it.File, Source: entity.SourceLibrary, SHA1: sum, SkipUndone: true}
	res, err := s.conv.Convert(ctx, in)
	if err != nil {
		var ce *entity.ConversionError
		sm.Error = err.Error()
		if errors.As(err, &ce) && ce.Failure.Err != nil {
			sm.Error = ce.Failure.Err.Error()
		}
		return sm
	}
	defer res.Release()
	if res.Marker != nil {
		sm.Task = res.Marker.Task
	}
	if res.Outcome != entity.OutcomeOptimized {
		sm.Result = "not_smaller"
		return sm
	}
	if err := s.conv.CheckOutput(ctx, in, res); err != nil {
		sm.Error = "the converted file does not look valid: " + err.Error()
		return sm
	}
	sm.Result, sm.NewSize, sm.NewName = "smaller", res.OutputSize, res.DeliverName
	sm.After = s.preview(ctx, res.DeliverPath)
	return sm
}

// preview returns a data URI of a small JPEG of the file, or "".
func (s *Service) preview(ctx context.Context, path string) string {
	if s.cfg.Preview == nil {
		return ""
	}
	jpeg, err := s.cfg.Preview(ctx, path)
	if err != nil || len(jpeg) == 0 {
		return ""
	}
	return "data:image/jpeg;base64," + base64.StdEncoding.EncodeToString(jpeg)
}

// reportData is what the report page renders.
type reportData struct {
	Generated string    `json:"generated"`
	Duration  string    `json:"duration"`
	Version   string    `json:"version"`
	DryRun    bool      `json:"dry_run"`
	User      string    `json:"user"`
	PublicURL string    `json:"public_url"`
	Summary   Summary   `json:"summary"`
	Estimate  *Estimate `json:"estimate,omitempty"`
	Items     []*Item   `json:"items"`
}

//go:embed report.html
var reportHTML string

var reportTemplate = template.Must(template.New("report").Parse(reportHTML))

// writeReport writes the HTML report of a run. A report that cannot be
// written is logged and nothing more: the audit log is the record that
// matters, the report is a way to read it.
func (s *Service) writeReport(user string, started time.Time, sum Summary, items []*Item, est *Estimate) {
	if s.cfg.ReportFile == "" {
		return
	}
	sort.SliceStable(items, func(i, j int) bool { return items[i].Date > items[j].Date })
	data := reportData{
		Generated: time.Now().UTC().Format(time.RFC3339),
		Duration:  time.Since(started).Round(time.Second).String(),
		Version:   s.cfg.Version,
		DryRun:    s.cfg.DryRun,
		User:      user,
		PublicURL: strings.TrimRight(s.cfg.PublicURL, "/"),
		Summary:   sum,
		Estimate:  est,
		Items:     items,
	}
	if data.Items == nil {
		data.Items = []*Item{}
	}
	err := func() error {
		// json.Marshal escapes <, > and &, so the data cannot break out of
		// its script element.
		blob, err := json.Marshal(data)
		if err != nil {
			return err
		}
		if err := os.MkdirAll(filepath.Dir(s.cfg.ReportFile), 0o750); err != nil {
			return err
		}
		tmp, err := os.CreateTemp(filepath.Dir(s.cfg.ReportFile), ".library-report-*.tmp")
		if err != nil {
			return err
		}
		defer os.Remove(tmp.Name())
		title := "Library run"
		if s.cfg.DryRun {
			title = "Library dry run"
		}
		err = reportTemplate.Execute(tmp, map[string]any{"Title": title, "Data": template.JS(blob)}) // #nosec G203 -- blob is JSON we marshalled ourselves
		if cerr := tmp.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			return err
		}
		if err := os.Chmod(tmp.Name(), 0o640); err != nil {
			return err
		}
		return os.Rename(tmp.Name(), s.cfg.ReportFile)
	}()
	if err != nil {
		if s.log != nil {
			s.log.Warnf("Library: could not write the report %s: %v", s.cfg.ReportFile, err)
		}
		return
	}
	s.infof("Library: report written to %s (open it in a browser)", s.cfg.ReportFile)
}
