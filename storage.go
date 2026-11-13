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
	UsedBytes      int64      `json:"usedBytes"`
	MaxBytes       int64      `json:"maxBytes"` // 0 = unlimited
	DiskFreeBytes  uint64     `json:"diskFreeBytes"`
	DiskTotalBytes uint64     `json:"diskTotalBytes"`
	Files          int        `json:"files"`
	Oldest         *time.Time `json:"oldest,omitempty"`
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

// enforce deletes the oldest recordings until the archive fits the configured
// quota and the disk keeps its minimum free space.
func (s *Storage) enforce(cfg Config) {
	all := listAll(cfg.StorageDir)
	sort.Slice(all, func(i, j int) bool { return all[i].Start.Before(all[j].Start) })
	var used int64
	for _, r := range all {
		used += r.Size
	}
	maxBytes := int64(cfg.MaxStorageGB * (1 << 30))
	minFree := uint64(cfg.MinFreeGB * (1 << 30))
	free, total, _ := diskFree(cfg.StorageDir)

	deleted := 0
	for len(all) > 0 {
		over := maxBytes > 0 && used > maxBytes
		low := total > 0 && free < minFree
		if !over && !low {
			break
		}
		r := all[0]
		if time.Since(r.End) < 2*time.Minute {
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

	st := StorageStatus{UsedBytes: used, MaxBytes: maxBytes, DiskFreeBytes: free, DiskTotalBytes: total, Files: len(all)}
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
