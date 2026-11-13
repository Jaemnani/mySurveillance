//go:build windows && embedffmpeg

package main

import (
	"embed"
	"io"
)

// Built with -tags embedffmpeg: ffmpeg.exe travels inside the single executable.
//
//go:embed third_party/ffmpeg/ffmpeg.exe
var ffmpegFS embed.FS

func openEmbeddedFFmpeg() (io.ReadCloser, int64, error) {
	f, err := ffmpegFS.Open("third_party/ffmpeg/ffmpeg.exe")
	if err != nil {
		return nil, 0, err
	}
	fi, err := f.Stat()
	if err != nil {
		f.Close()
		return nil, 0, err
	}
	return f, fi.Size(), nil
}
