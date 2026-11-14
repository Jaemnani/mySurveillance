package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

const (
	recSubdir    = "rec"
	liveSubdir   = "live"
	recTimeFmt   = "20060102-150405"
	stallTimeout = 45 * time.Second
	// Recording files are cut at this length. Playback seeks inside a file by
	// byte range, so the length only trades the number of files against how
	// much one file holds.
	segmentMinutes = 10
)

// spec is everything that, when changed, requires restarting a camera's ffmpeg.
type spec struct {
	Cam        Camera
	StorageDir string
	SegmentSec int
	FFmpeg     string
}

type CamStatus struct {
	ID      string    `json:"id"`
	State   string    `json:"state"` // starting | recording | retrying
	Error   string    `json:"error,omitempty"`
	Since   time.Time `json:"since"`
	Restart int       `json:"restarts"`
}

type worker struct {
	spec   spec
	cancel context.CancelFunc
	done   chan struct{}

	mu sync.Mutex
	st CamStatus
}

type Manager struct {
	mu      sync.Mutex
	workers map[string]*worker
}

func newManager() *Manager { return &Manager{workers: map[string]*worker{}} }

// Apply starts, stops or restarts recorders so they match cfg.
func (m *Manager) Apply(cfg Config, ffmpeg string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	want := map[string]spec{}
	if ffmpeg != "" {
		for _, c := range cfg.Cameras {
			if c.Enabled {
				want[c.ID] = spec{c, cfg.StorageDir, segmentMinutes * 60, ffmpeg}
			}
		}
	}
	for id, w := range m.workers {
		if s, ok := want[id]; !ok || !reflect.DeepEqual(s, w.spec) {
			w.stop()
			delete(m.workers, id)
		}
	}
	for id, s := range want {
		if _, ok := m.workers[id]; !ok {
			ctx, cancel := context.WithCancel(context.Background())
			w := &worker{spec: s, cancel: cancel, done: make(chan struct{})}
			w.set("starting", "")
			m.workers[id] = w
			go w.run(ctx)
		}
	}
}

func (m *Manager) StopAll() {
	m.mu.Lock()
	defer m.mu.Unlock()
	var wg sync.WaitGroup
	for id, w := range m.workers {
		wg.Add(1)
		go func() { defer wg.Done(); w.stop() }()
		delete(m.workers, id)
	}
	wg.Wait()
}

func (m *Manager) Status() map[string]CamStatus {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := map[string]CamStatus{}
	for id, w := range m.workers {
		w.mu.Lock()
		out[id] = w.st
		w.mu.Unlock()
	}
	return out
}

func (w *worker) stop() {
	w.cancel()
	<-w.done
}

func (w *worker) set(state, errMsg string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.st.State != state {
		w.st.Since = time.Now()
	}
	w.st.ID, w.st.State, w.st.Error = w.spec.Cam.ID, state, errMsg
}

// run keeps ffmpeg alive forever: whenever it exits or stalls it is restarted.
func (w *worker) run(ctx context.Context) {
	defer close(w.done)
	name := w.spec.Cam.Name
	backoff := 2 * time.Second
	for ctx.Err() == nil {
		began := time.Now()
		err := w.runOnce(ctx)
		if ctx.Err() != nil {
			return
		}
		if time.Since(began) > time.Minute {
			backoff = 2 * time.Second
		}
		log.Printf("[%s] 녹화 중단, %v 뒤 재시도: %v", name, backoff, err)
		w.mu.Lock()
		w.st.Restart++
		w.mu.Unlock()
		w.set("retrying", err.Error())
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < 30*time.Second {
			backoff *= 2
		}
	}
}

func (w *worker) runOnce(ctx context.Context) error {
	s := w.spec
	recDir := filepath.Join(s.StorageDir, recSubdir, s.Cam.ID)
	liveDir := filepath.Join(s.StorageDir, liveSubdir, s.Cam.ID)
	if err := os.MkdirAll(recDir, 0o755); err != nil {
		return err
	}
	os.RemoveAll(liveDir)
	if err := os.MkdirAll(liveDir, 0o755); err != nil {
		return err
	}

	srcCodec := ""
	if s.Cam.Type == "rtsp" && !s.Cam.Transcode {
		srcCodec = probeVideoCodec(ctx, s)
		if ctx.Err() != nil {
			return ctx.Err()
		}
	}
	cmd := exec.Command(s.FFmpeg, buildArgs(s, srcCodec)...)
	cmd.Dir = s.StorageDir // output paths are relative, so no drive-letter escaping
	setProcAttrs(cmd)
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("ffmpeg 실행 실패: %w", err)
	}
	w.set("starting", "")
	log.Printf("[%s] 녹화 시작 (pid %d)", s.Cam.Name, cmd.Process.Pid)

	var tailMu sync.Mutex
	var tail []string
	exited := make(chan error, 1)
	go func() {
		sc := bufio.NewScanner(stderr)
		sc.Buffer(make([]byte, 64*1024), 1024*1024)
		for sc.Scan() {
			tailMu.Lock()
			tail = append(tail, sc.Text())
			if len(tail) > 8 {
				tail = tail[1:]
			}
			tailMu.Unlock()
		}
		io.Copy(io.Discard, stderr)
		exited <- cmd.Wait()
	}()
	lastLines := func() string {
		tailMu.Lock()
		defer tailMu.Unlock()
		// the last lines carry the actual failure; earlier ones are usually warnings
		s := redact(strings.Join(tail[max(0, len(tail)-2):], " | "))
		if r := []rune(s); len(r) > 300 {
			s = "…" + string(r[len(r)-300:])
		}
		return s
	}

	tick := time.NewTicker(10 * time.Second)
	defer tick.Stop()
	lastSig, lastChange := newestSig(recDir), time.Now()
	for {
		select {
		case err := <-exited:
			return fmt.Errorf("ffmpeg 종료(%v): %s", err, lastLines())
		case <-ctx.Done():
			io.WriteString(stdin, "q\n") // lets ffmpeg finalize the current file
			stdin.Close()
			select {
			case <-exited:
			case <-time.After(8 * time.Second):
				cmd.Process.Kill()
				<-exited
			}
			return ctx.Err()
		case <-tick.C:
			if sig := newestSig(recDir); sig != lastSig {
				lastSig, lastChange = sig, time.Now()
				w.set("recording", "")
			} else if time.Since(lastChange) > stallTimeout {
				cmd.Process.Kill()
				<-exited
				return errors.New("영상 데이터가 들어오지 않습니다: " + lastLines())
			}
		}
	}
}

// newestSig identifies the newest recording file and its size; if it stops
// changing, the camera has stalled.
func newestSig(dir string) string {
	ents, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	for i := len(ents) - 1; i >= 0; i-- {
		if strings.HasSuffix(ents[i].Name(), ".mp4") {
			if fi, err := statOpen(filepath.Join(dir, ents[i].Name())); err == nil {
				return ents[i].Name() + ":" + strconv.FormatInt(fi.Size(), 10)
			}
		}
	}
	return ""
}

// statOpen reads a file's size and time through an open handle. A directory
// listing is not enough for the file ffmpeg is writing: Windows only updates
// the listed size and time when the writer closes it.
func statOpen(path string) (os.FileInfo, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return f.Stat()
}

// redact hides credentials embedded in camera URLs before they reach logs/UI.
func redact(s string) string {
	return credRe.ReplaceAllString(s, "$1***@")
}

var credRe = regexp.MustCompile(`(\w+://)[^/\s@|'"]*@`)

// inputArgs returns the ffmpeg input options for a camera, whether its video
// must be encoded, and which input carries the audio.
func inputArgs(cam Camera) (a []string, encode bool, audioMap string) {
	encode = cam.Transcode
	audioMap = "0:a:0?"

	switch cam.Type {
	case "usb":
		encode = true
		switch runtime.GOOS {
		case "windows":
			a = append(a, "-f", "dshow", "-rtbufsize", "256M")
			a = append(a, captureOpts(cam)...)
			in := "video=" + cam.VideoDevice
			if cam.Audio && cam.AudioDevice != "" {
				in += ":audio=" + cam.AudioDevice
			}
			a = append(a, "-i", in)
		case "darwin":
			a = append(a, "-f", "avfoundation")
			a = append(a, captureOpts(cam)...)
			aud := "none"
			if cam.Audio && cam.AudioDevice != "" {
				aud = cam.AudioDevice
			}
			a = append(a, "-i", cam.VideoDevice+":"+aud)
		default:
			a = append(a, "-f", "v4l2")
			a = append(a, captureOpts(cam)...)
			a = append(a, "-i", cam.VideoDevice)
			if cam.Audio && cam.AudioDevice != "" {
				a = append(a, "-f", "pulse", "-i", cam.AudioDevice)
				audioMap = "1:a:0"
			}
		}
	case "rtsp":
		a = append(a, "-rtsp_transport", "tcp", "-timeout", "15000000", "-i", cam.URL)
	case "http":
		encode = true
		a = append(a, "-use_wallclock_as_timestamps", "1", "-rw_timeout", "15000000", "-i", cam.URL)
		if cam.Audio && cam.AudioURL != "" {
			a = append(a, "-rw_timeout", "15000000", "-i", cam.AudioURL)
			audioMap = "1:a:0"
		}
	}
	return a, encode, audioMap
}

var videoCodecRe = regexp.MustCompile(`Stream #0:\d+[^
]*Video: (\w+)`)

// probeVideoCodec asks ffmpeg what the camera sends ("hevc", "h264", ...).
// It returns "" when the camera cannot be read; recording then proceeds with
// defaults and fails or succeeds on its own.
func probeVideoCodec(ctx context.Context, s spec) string {
	in, _, _ := inputArgs(s.Cam)
	pctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(pctx, s.FFmpeg, append([]string{"-hide_banner"}, in...)...)
	setProcAttrs(cmd)
	out, _ := cmd.CombinedOutput() // always "fails": no output file is given
	if m := videoCodecRe.FindSubmatch(out); m != nil {
		return string(m[1])
	}
	return ""
}

// buildArgs builds the full ffmpeg command. srcCodec is the camera's video
// codec when the video is stored as-is (see probeVideoCodec).
func buildArgs(s spec, srcCodec string) []string {
	cam := s.Cam
	in, encode, audioMap := inputArgs(cam)
	a := append([]string{"-hide_banner", "-loglevel", "warning", "-nostats"}, in...)

	a = append(a, "-map", "0:v:0")
	if cam.Audio {
		a = append(a, "-map", audioMap, "-c:a", "aac", "-b:a", "64k", "-ar", "44100")
	} else {
		a = append(a, "-an")
	}
	if encode {
		a = append(a, "-c:v", "libx264", "-preset", "veryfast", "-crf", "25", "-pix_fmt", "yuv420p",
			"-force_key_frames", "expr:gte(t,n_forced*2)")
		if cam.Type != "usb" && cam.Width > 0 && cam.Height > 0 {
			a = append(a, "-vf", fmt.Sprintf("scale=%d:%d", cam.Width, cam.Height))
		}
	} else {
		a = append(a, "-c:v", "copy")
	}
	hevc := !encode && (srcCodec == "hevc" || srcCodec == "h265")
	live := fmt.Sprintf("hls_segment_filename=%s/%s/seg%%05d.ts", liveSubdir, cam.ID)
	if hevc {
		// ffmpeg's default MP4 tag for H.265 is hev1, which iPhone, Safari and
		// QuickTime refuse to play; hvc1 is the same video with the tag they
		// accept. H.265 live streaming likewise needs fMP4 segments, not TS.
		a = append(a, "-tag:v", "hvc1")
		live = fmt.Sprintf("hls_segment_type=fmp4:hls_fmp4_init_filename=init.mp4:hls_segment_filename=%s/%s/seg%%05d.m4s", liveSubdir, cam.ID)
	}

	id := cam.ID
	// One encode feeds two outputs: crash-safe MP4 files for the archive and a
	// short rolling HLS playlist for live view. The live output may fail
	// without stopping the recording.
	tee := fmt.Sprintf("[f=segment:strftime=1:segment_time=%d:segment_atclocktime=1:reset_timestamps=1:"+
		"segment_format=mp4:segment_format_options=movflags=+frag_keyframe+empty_moov+default_base_moof]%s/%s/%%Y%%m%%d-%%H%%M%%S.mp4"+
		"|[f=hls:onfail=ignore:hls_time=2:hls_list_size=6:hls_flags=delete_segments+omit_endlist:"+
		"%s]%s/%s/index.m3u8",
		s.SegmentSec, recSubdir, id, live, liveSubdir, id)
	return append(a, "-flags", "+global_header", "-f", "tee", tee)
}

func captureOpts(cam Camera) []string {
	var a []string
	if cam.Width > 0 && cam.Height > 0 {
		a = append(a, "-video_size", fmt.Sprintf("%dx%d", cam.Width, cam.Height))
	}
	if cam.FPS > 0 {
		a = append(a, "-framerate", strconv.Itoa(cam.FPS))
	}
	return a
}
