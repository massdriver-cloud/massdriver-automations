package costexplorer

import (
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const historyFile = "cost-explorer.json"

// history is one cost sample per instance per UTC day: the instance's last_day amount, read last on that day.
type history struct {
	Days []day `json:"days"`
	// Instances keeps the name and dimensions of every instance in Days, so a deleted instance still groups.
	Instances map[string]known `json:"instances"`
}

type day struct {
	Date  string             `json:"date"`
	Costs map[string]float64 `json:"costs"`
}

type known struct {
	Name       string            `json:"name"`
	Dimensions map[string]string `json:"dimensions"`
}

func (h *history) record(now time.Time, rows []Row, keepDays int) {
	today := day{Date: now.Format(time.DateOnly), Costs: map[string]float64{}}
	if h.Instances == nil {
		h.Instances = map[string]known{}
	}
	for _, r := range rows {
		if r.LastDay == nil {
			continue
		}
		today.Costs[r.ID] = *r.LastDay
		h.Instances[r.ID] = known{Name: r.Name, Dimensions: r.Dimensions}
	}
	if n := len(h.Days); n > 0 && h.Days[n-1].Date == today.Date {
		h.Days[n-1] = today
	} else {
		h.Days = append(h.Days, today)
	}
	sort.Slice(h.Days, func(i, j int) bool { return h.Days[i].Date < h.Days[j].Date })

	cutoff := now.AddDate(0, 0, -keepDays+1).Format(time.DateOnly)
	kept := h.Days[:0]
	for _, d := range h.Days {
		if d.Date >= cutoff {
			kept = append(kept, d)
		}
	}
	h.Days = kept

	used := map[string]bool{}
	for _, d := range h.Days {
		for id := range d.Costs {
			used[id] = true
		}
	}
	for id := range h.Instances {
		if !used[id] {
			delete(h.Instances, id)
		}
	}
}

func loadHistory(dir string) (history, error) {
	h := history{Instances: map[string]known{}}
	if dir == "" {
		return h, nil
	}
	b, err := os.ReadFile(filepath.Join(dir, historyFile))
	if errors.Is(err, fs.ErrNotExist) {
		return h, nil
	}
	if err != nil {
		return h, err
	}
	if err := json.Unmarshal(b, &h); err != nil {
		return h, err
	}
	if h.Instances == nil {
		h.Instances = map[string]known{}
	}
	return h, nil
}

func saveHistory(dir string, h history) error {
	if dir == "" {
		return nil
	}
	b, err := json.Marshal(h)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dir, 0o750); err != nil {
		return err
	}
	path := filepath.Join(dir, historyFile)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
