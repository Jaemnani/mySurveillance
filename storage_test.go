package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func writeRec(t *testing.T, dir, cam string, start time.Time, dur time.Duration, size int) string {
	t.Helper()
	p := filepath.Join(dir, recSubdir, cam, start.Format(recTimeFmt)+".mp4")
	os.MkdirAll(filepath.Dir(p), 0o755)
	if err := os.WriteFile(p, make([]byte, size), 0o644); err != nil {
		t.Fatal(err)
	}
	os.Chtimes(p, start.Add(dur), start.Add(dur))
	return p
}

func TestDailyBytes(t *testing.T) {
	now := time.Now()
	recs := []Recording{
		{Camera: "a", Start: now.Add(-20 * time.Minute), End: now.Add(-10 * time.Minute), Size: 600},  // 1 B/s
		{Camera: "a", Start: now.Add(-10 * time.Minute), End: now, Size: 600},                         // 1 B/s
		{Camera: "b", Start: now.Add(-10 * time.Minute), End: now, Size: 1200},                        // 2 B/s
		{Camera: "b", Start: now.Add(-48 * time.Hour), End: now.Add(-47 * time.Hour), Size: 99999999}, // too old
		{Camera: "off", Start: now.Add(-10 * time.Minute), End: now, Size: 6000},                      // disabled
		{Camera: "new", Start: now.Add(-10 * time.Second), End: now, Size: 6000},                      // too short
	}
	cams := []Camera{{ID: "a", Enabled: true}, {ID: "b", Enabled: true}, {ID: "off"}, {ID: "new", Enabled: true}}
	perDay, perCam := dailyBytes(recs, cams, now)
	if perDay != 3*86400 || len(perCam) != 2 || perCam["b"] != 2*86400 {
		t.Fatalf("perDay=%d perCam=%v", perDay, perCam)
	}
}

func TestEnforceByDays(t *testing.T) {
	dir := t.TempDir()
	now := time.Now().Truncate(time.Second)
	old := writeRec(t, dir, "a", now.Add(-50*time.Hour), 5*time.Minute, 1000)
	keep := writeRec(t, dir, "a", now.Add(-10*time.Hour), 5*time.Minute, 1000)
	cfg := Config{StorageDir: dir, LimitBy: "days", MaxDays: 1, MaxStorageGB: 0.0000001, Cameras: []Camera{{ID: "a", Enabled: true}}}
	s := &Storage{}
	s.enforce(cfg)
	if _, err := os.Stat(old); err == nil {
		t.Error("recording older than MaxDays was kept")
	}
	if _, err := os.Stat(keep); err != nil {
		t.Error("recent recording was deleted (size limit must be ignored in days mode)")
	}
	st := s.Status()
	if st.LimitBy != "days" || st.BytesPerDay <= 0 || st.MaxBytes != st.BytesPerDay || st.CamerasMeasured != 1 {
		t.Errorf("status: %+v", st)
	}

	cfg.LimitBy, cfg.MaxStorageGB, cfg.MaxDays = "size", 0, 0.001
	s.enforce(cfg)
	if _, err := os.Stat(keep); err != nil {
		t.Error("age limit must be ignored in size mode")
	}
}
