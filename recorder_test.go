package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func testFFmpeg(t *testing.T) string {
	for _, p := range []string{filepath.Join("third_party", "ffmpeg", "ffmpeg.exe"), "ffmpeg"} {
		if abs, err := exec.LookPath(p); err == nil {
			abs, _ = filepath.Abs(abs)
			return abs
		}
	}
	t.Skip("ffmpeg not available")
	return ""
}

func TestVideoCodecRe(t *testing.T) {
	out := "Input #0, rtsp, from 'rtsp://x':\n  Stream #0:0: Video: hevc (Main), yuv420p(tv), 2880x1620, 15 fps\n  Stream #0:1: Audio: aac (LC), 16000 Hz\n"
	if m := videoCodecRe.FindStringSubmatch(out); m == nil || m[1] != "hevc" {
		t.Fatalf("got %v", m)
	}
}

// Stored-as-is video must end up playable on Apple devices: H.265 needs the
// hvc1 tag in recordings and fMP4 live segments, H.264 keeps TS segments.
func TestCopyOutputTags(t *testing.T) {
	ff := testFFmpeg(t)
	for _, tc := range []struct{ codec, encoder, wantTag, liveFile string }{
		{"hevc", "libx265", "hvc1", "init.mp4"},
		{"h264", "libx264", "avc1", "seg00000.ts"},
	} {
		t.Run(tc.codec, func(t *testing.T) {
			dir := t.TempDir()
			src := filepath.Join(dir, "src.ts")
			if out, err := exec.Command(ff, "-v", "error", "-f", "lavfi", "-i", "testsrc2=size=320x240:rate=15",
				"-f", "lavfi", "-i", "sine", "-t", "5", "-c:v", tc.encoder, "-preset", "ultrafast", "-g", "15",
				"-c:a", "aac", "-f", "mpegts", src).CombinedOutput(); err != nil {
				t.Skipf("cannot create %s source: %v %s", tc.codec, err, out)
			}
			s := spec{Cam: Camera{ID: "c", Type: "rtsp", URL: "rtsp://unused/", Audio: true}, StorageDir: dir, SegmentSec: 300, FFmpeg: ff}
			args := buildArgs(s, tc.codec)
			outArgs := args[slices.Index(args, "-map"):] // everything after the camera input
			for _, d := range []string{recSubdir, liveSubdir} {
				os.MkdirAll(filepath.Join(dir, d, "c"), 0o755)
			}
			cmd := exec.Command(ff, append([]string{"-v", "error", "-i", src}, outArgs...)...)
			cmd.Dir = dir
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("ffmpeg failed: %v\n%s", err, out)
			}
			recs, _ := filepath.Glob(filepath.Join(dir, recSubdir, "c", "*.mp4"))
			if len(recs) == 0 {
				t.Fatal("no recording written")
			}
			info, _ := exec.Command(ff, "-hide_banner", "-i", recs[0]).CombinedOutput()
			if !strings.Contains(string(info), "("+tc.wantTag+" /") {
				t.Errorf("recording tag: want %s in:\n%s", tc.wantTag, info)
			}
			if _, err := os.Stat(filepath.Join(dir, liveSubdir, "c", tc.liveFile)); err != nil {
				t.Errorf("live output %s missing", tc.liveFile)
			}
			if _, err := os.Stat(filepath.Join(dir, liveSubdir, "c", "index.m3u8")); err != nil {
				t.Error("live playlist missing")
			}
		})
	}
}
