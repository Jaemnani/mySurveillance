package main

import (
	"context"
	"log"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

var recFileRe = regexp.MustCompile(`^\d{8}-\d{6}\.mp4$`)

type Recording struct {
	Camera string    `json:"camera"`
	File   string    `json:"file"`
	Start  time.Time `json:"start"`
	End    time.Time `json:"end"`
	Size   int64     `json:"size"`
	Active bool      `json:"active"` // still being written
	URL    string    `json:"url"`
}

type StorageStatus struct {
	UsedBytes       int64            `json:"usedBytes"`
	MaxBytes        int64            `json:"maxBytes"` // 0 = unlimited; estimated when limiting by days
	LimitBy         string           `json:"limitBy"`
	MaxDays         float64          `json:"maxDays"`
	BytesPerDay     int64            `json:"bytesPerDay"` // measured for the enabled cameras, 0 = not known yet
	CamerasEnabled  int              `json:"camerasEnabled"`
	CamerasMeasured int              `json:"camerasMeasured"` // enabled cameras included in BytesPerDay
	PerCamera       map[string]int64 `json:"perCamera"`       // camera id -> bytes per day
	DiskFreeBytes   uint64           `json:"diskFreeBytes"`
	ReserveBytes    uint64           `json:"reserveBytes"` // always kept free on the disk
	DiskTotalBytes  uint64           `json:"diskTotalBytes"`
	Files           int              `json:"files"`
	Oldest          *time.Time       `json:"oldest,omitempty"`
}

type Storage struct {
	mu   sync.Mutex
	last StorageStatus
}

// listRecordings returns one camera's files in chronological order.
func listRecordings(storageDir, cam string) []Recording {
	ents, err := os.ReadDir(filepath.Join(storageDir, recSubdir, cam))
	if err != nil {
		return nil
	}
	var out []Recording
	for _, e := range ents {
		if e.IsDir() || !recFileRe.MatchString(e.Name()) {
			continue
		}
		fi, err := e.Info()
		if err != nil {
			continue
		}
		start, err := time.ParseInLocation(recTimeFmt, strings.TrimSuffix(e.Name(), ".mp4"), time.Local)
		if err != nil {
			continue
		}
		out = append(out, Recording{
			Camera: cam, File: e.Name(), Start: start, End: fi.ModTime(), Size: fi.Size(),
			Active: time.Since(fi.ModTime()) < 20*time.Second,
			URL:    "/rec/" + cam + "/" + e.Name(),
		})
	}
	if n := len(out); n > 0 { // the newest file may still be open for writing
		if fi, err := statOpen(filepath.Join(storageDir, recSubdir, cam, out[n-1].File)); err == nil {
			out[n-1].End, out[n-1].Size = fi.ModTime(), fi.Size()
			out[n-1].Active = time.Since(fi.ModTime()) < 20*time.Second
		}
	}
	return out
}

func listAll(storageDir string) []Recording {
	dirs, _ := os.ReadDir(filepath.Join(storageDir, recSubdir))
	var all []Recording
	for _, d := range dirs {
		if d.IsDir() {
			all = append(all, listRecordings(storageDir, d.Name())...)
		}
	}
	return all
}

// dailyBytes measures how much the enabled cameras write per day, from what
// they recorded over the last 24 hours. perCam holds only the cameras that
// have enough footage to be measured.
func dailyBytes(all []Recording, cams []Camera, now time.Time) (perDay int64, perCam map[string]int64) {
	type acc struct {
		bytes int64
		dur   time.Duration
	}
	by := map[string]*acc{}
	for _, r := range all {
		d := r.End.Sub(r.Start)
		if now.Sub(r.Start) > 24*time.Hour || d <= 0 || d > 2*time.Hour {
			continue
		}
		a := by[r.Camera]
		if a == nil {
			a = &acc{}
			by[r.Camera] = a
		}
		a.bytes += r.Size
		a.dur += d
	}
	perCam = map[string]int64{}
	for _, c := range cams {
		if a := by[c.ID]; c.Enabled && a != nil && a.dur >= time.Minute {
			perCam[c.ID] = int64(float64(a.bytes) / a.dur.Seconds() * 86400)
			perDay += perCam[c.ID]
		}
	}
	return perDay, perCam
}

// reserveBytes is the disk space recordings must always leave free: enough for
// the system and for what the cameras write between two cleanup passes.
func reserveBytes(diskTotal uint64) uint64 {
	return max(5<<30, diskTotal/50)
}

// enforce deletes the oldest recordings until the archive fits the configured
// limit (age or size) and the disk keeps its minimum free space.
func (s *Storage) enforce(cfg Config) {
	all := listAll(cfg.StorageDir)
	sort.Slice(all, func(i, j int) bool { return all[i].Start.Before(all[j].Start) })
	var used int64
	for _, r := range all {
		used += r.Size
	}
	now := time.Now()
	perDay, perCam := dailyBytes(all, cfg.Cameras, now)
	byDays := cfg.LimitBy != "size"
	var maxBytes int64
	var maxAge time.Duration
	if byDays {
		maxAge = time.Duration(cfg.MaxDays * 24 * float64(time.Hour))
		maxBytes = int64(cfg.MaxDays * float64(perDay)) // estimate, not enforced
	} else {
		maxBytes = int64(cfg.MaxStorageGB * (1 << 30))
	}
	free, total, _ := diskFree(cfg.StorageDir)
	minFree := reserveBytes(total)

	deleted := 0
	for len(all) > 0 {
		r := all[0]
		over := !byDays && maxBytes > 0 && used > maxBytes
		old := byDays && maxAge > 0 && now.Sub(r.End) > maxAge
		low := total > 0 && free < minFree
		if !over && !old && !low {
			break
		}
		if now.Sub(r.End) < 2*time.Minute {
			break // never delete what is being written right now
		}
		all = all[1:]
		if err := os.Remove(filepath.Join(cfg.StorageDir, recSubdir, r.Camera, r.File)); err != nil {
			log.Printf("오래된 파일 삭제 실패 %s/%s: %v", r.Camera, r.File, err)
			continue
		}
		used -= r.Size
		free += uint64(r.Size)
		deleted++
	}
	if deleted > 0 {
		log.Printf("저장 용량 확보: 오래된 녹화 %d개 삭제", deleted)
	}

	st := StorageStatus{UsedBytes: used, MaxBytes: maxBytes, DiskFreeBytes: free, DiskTotalBytes: total, ReserveBytes: minFree, Files: len(all),
		LimitBy: "size", MaxDays: cfg.MaxDays, BytesPerDay: perDay, CamerasMeasured: len(perCam), PerCamera: perCam}
	if byDays {
		st.LimitBy = "days"
	}
	for _, c := range cfg.Cameras {
		if c.Enabled {
			st.CamerasEnabled++
		}
	}
	if len(all) > 0 {
		t := all[0].Start
		st.Oldest = &t
	}
	s.mu.Lock()
	s.last = st
	s.mu.Unlock()
}

func (s *Storage) Status() StorageStatus {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.last
}

func (s *Storage) loop(ctx context.Context, getCfg func() Config) {
	t := time.NewTicker(20 * time.Second)
	defer t.Stop()
	for {
		s.enforce(getCfg())
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
