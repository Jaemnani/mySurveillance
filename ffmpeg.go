package main

import (
	"io"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
)

// findFFmpeg locates the ffmpeg binary: explicit config path, next to the
// executable, the copy embedded in this program (extracted on first run), then PATH.
func findFFmpeg(cfgPath, dataDir string) string {
	name := "ffmpeg"
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	exists := func(p string) bool {
		fi, err := os.Stat(p)
		return err == nil && !fi.IsDir()
	}
	if cfgPath != "" && exists(cfgPath) {
		return cfgPath
	}
	if exe, err := os.Executable(); err == nil {
		if p := filepath.Join(filepath.Dir(exe), name); exists(p) {
			return p
		}
	}
	if p, err := extractEmbedded(filepath.Join(dataDir, "bin", name)); err != nil {
		log.Printf("내장 ffmpeg 추출 실패: %v", err)
	} else if p != "" {
		return p
	}
	if p, err := exec.LookPath(name); err == nil {
		return p
	}
	return ""
}

func extractEmbedded(dst string) (string, error) {
	src, size, err := openEmbeddedFFmpeg()
	if src == nil || err != nil {
		return "", err
	}
	defer src.Close()
	if fi, err := os.Stat(dst); err == nil && fi.Size() == size {
		return dst, nil
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return "", err
	}
	tmp := dst + ".tmp"
	f, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o755)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(f, src); err != nil {
		f.Close()
		return "", err
	}
	if err := f.Close(); err != nil {
		return "", err
	}
	return dst, os.Rename(tmp, dst)
}
