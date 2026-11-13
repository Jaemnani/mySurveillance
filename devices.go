package main

import (
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
)

type Devices struct {
	Video []string `json:"video"`
	Audio []string `json:"audio"`
}

var (
	dshowRe = regexp.MustCompile(`"([^"]+)"\s+\((video|audio)\)`)
	avfRe   = regexp.MustCompile(`\[(\d+)\]\s+(.+)$`)
	quoteRe = regexp.MustCompile(`\]\s+"([^"@][^"]*)"`)
)

// listDevices asks ffmpeg which capture devices are attached to this computer.
func listDevices(ffmpeg string) Devices {
	d := Devices{Video: []string{}, Audio: []string{}}
	run := func(args ...string) []string {
		cmd := exec.Command(ffmpeg, args...)
		setProcAttrs(cmd)
		out, _ := cmd.CombinedOutput() // ffmpeg exits non-zero when only listing
		return strings.Split(strings.ReplaceAll(string(out), "\r", ""), "\n")
	}
	switch runtime.GOOS {
	case "windows":
		section := ""
		for _, l := range run("-hide_banner", "-list_devices", "true", "-f", "dshow", "-i", "dummy") {
			if m := dshowRe.FindStringSubmatch(l); m != nil {
				if m[2] == "video" {
					d.Video = append(d.Video, m[1])
				} else {
					d.Audio = append(d.Audio, m[1])
				}
				continue
			}
			// older ffmpeg: section headers followed by bare quoted names
			switch {
			case strings.Contains(l, "DirectShow video devices"):
				section = "video"
			case strings.Contains(l, "DirectShow audio devices"):
				section = "audio"
			case section != "" && !strings.Contains(l, "Alternative name"):
				if m := quoteRe.FindStringSubmatch(l); m != nil {
					if section == "video" {
						d.Video = append(d.Video, m[1])
					} else {
						d.Audio = append(d.Audio, m[1])
					}
				}
			}
		}
	case "darwin":
		section := ""
		for _, l := range run("-hide_banner", "-f", "avfoundation", "-list_devices", "true", "-i", "") {
			switch {
			case strings.Contains(l, "AVFoundation video devices"):
				section = "video"
			case strings.Contains(l, "AVFoundation audio devices"):
				section = "audio"
			default:
				if m := avfRe.FindStringSubmatch(l); m != nil && section == "video" {
					d.Video = append(d.Video, m[1])
				} else if m != nil && section == "audio" {
					d.Audio = append(d.Audio, m[1])
				}
			}
		}
	default:
		if v, _ := filepath.Glob("/dev/video*"); v != nil {
			d.Video = v
		}
		d.Audio = []string{"default"}
	}
	return d
}
