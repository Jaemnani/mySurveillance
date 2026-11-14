package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"time"
)

// Camera describes one video source.
//
//	usb  : a capture device attached to this computer (webcam, capture card)
//	rtsp : an IP camera or a phone camera app that exposes an RTSP address
//	http : an HTTP/MJPEG stream (e.g. the "IP Webcam" phone app's /video address)
type Camera struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Type        string `json:"type"`
	Enabled     bool   `json:"enabled"`
	URL         string `json:"url,omitempty"`
	AudioURL    string `json:"audioUrl,omitempty"`
	VideoDevice string `json:"videoDevice,omitempty"`
	AudioDevice string `json:"audioDevice,omitempty"`
	Audio       bool   `json:"audio"`
	// Transcode forces re-encoding to H.264 for network cameras whose native
	// codec cannot be played by phones (e.g. H.265). USB/HTTP always encode.
	Transcode bool `json:"transcode,omitempty"`
	Width     int  `json:"width,omitempty"`
	Height    int  `json:"height,omitempty"`
	FPS       int  `json:"fps,omitempty"`
}

type Token struct {
	Hash    string    `json:"hash"`
	Created time.Time `json:"created"`
}

type Config struct {
	Listen         string   `json:"listen"`
	StorageDir     string   `json:"storageDir"`
	MaxStorageGB   float64  `json:"maxStorageGB"`
	MinFreeGB      float64  `json:"minFreeGB"`
	SegmentMinutes int      `json:"segmentMinutes"`
	Cameras        []Camera `json:"cameras"`
	FFmpegPath     string   `json:"ffmpegPath,omitempty"`
	TLSCert        string   `json:"tlsCert,omitempty"`
	TLSKey         string   `json:"tlsKey,omitempty"`
	PasswordHash   string   `json:"passwordHash"`
	Tokens         []Token  `json:"tokens,omitempty"`
}

func (c Config) clone() Config {
	c.Cameras = append([]Camera{}, c.Cameras...) // never nil: JSON must be [] not null
	c.Tokens = append([]Token(nil), c.Tokens...)
	return c
}

func defaultDataDir() string {
	switch runtime.GOOS {
	case "windows":
		if pd := os.Getenv("ProgramData"); pd != "" {
			return filepath.Join(pd, "mySurveillance")
		}
	case "darwin":
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, "Library", "Application Support", "mySurveillance")
		}
	}
	if dir, err := os.UserConfigDir(); err == nil {
		return filepath.Join(dir, "mySurveillance")
	}
	return "mySurveillance-data"
}

func defaultConfig(dataDir string) Config {
	return Config{
		Listen:         ":8787",
		StorageDir:     filepath.Join(dataDir, "recordings"),
		MaxStorageGB:   50,
		MinFreeGB:      2,
		SegmentMinutes: 5,
		Cameras:        []Camera{},
	}
}

func loadConfig(path, dataDir string) (Config, bool, error) {
	cfg := defaultConfig(dataDir)
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return cfg, true, nil
	}
	if err != nil {
		return cfg, false, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, false, fmt.Errorf("설정 파일(%s)을 읽을 수 없습니다: %w", path, err)
	}
	if cfg.Cameras == nil {
		cfg.Cameras = []Camera{}
	}
	return cfg, false, nil
}

func saveConfig(path string, cfg Config) error {
	b, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

var camIDRe = regexp.MustCompile(`^[a-z0-9_-]{1,32}$`)

// validateSettings checks and normalizes the user-editable part of the config.
func validateSettings(c *Config) error {
	c.StorageDir = strings.TrimSpace(c.StorageDir)
	if !filepath.IsAbs(c.StorageDir) {
		return errors.New("저장 폴더는 전체 경로로 입력해야 합니다 (예: D:\\CCTV)")
	}
	if err := os.MkdirAll(c.StorageDir, 0o755); err != nil {
		return fmt.Errorf("저장 폴더를 만들 수 없습니다: %v", err)
	}
	if c.MaxStorageGB < 0 || c.MinFreeGB < 0 {
		return errors.New("용량은 0 이상이어야 합니다")
	}
	if c.MaxStorageGB > 0 && c.MaxStorageGB < 0.2 {
		return errors.New("최대 저장 용량은 0.2GB 이상이어야 합니다 (0 = 제한 없음)")
	}
	if c.SegmentMinutes < 1 || c.SegmentMinutes > 60 {
		return errors.New("파일 길이는 1~60분 사이여야 합니다")
	}
	seen := map[string]bool{}
	for i := range c.Cameras {
		cam := &c.Cameras[i]
		cam.Name = strings.TrimSpace(cam.Name)
		if cam.Name == "" {
			return errors.New("카메라 이름을 입력하세요")
		}
		if cam.ID == "" {
			cam.ID = "cam-" + randomHex(4)
		}
		if !camIDRe.MatchString(cam.ID) || seen[cam.ID] {
			return fmt.Errorf("카메라 ID가 올바르지 않거나 중복됩니다: %q", cam.ID)
		}
		seen[cam.ID] = true
		if cam.Width < 0 || cam.Height < 0 || cam.FPS < 0 || cam.FPS > 120 {
			return fmt.Errorf("%s: 해상도/프레임 값이 올바르지 않습니다", cam.Name)
		}
		switch cam.Type {
		case "usb":
			if strings.TrimSpace(cam.VideoDevice) == "" {
				return fmt.Errorf("%s: USB 카메라 장치를 선택하세요", cam.Name)
			}
			cam.URL, cam.AudioURL = "", ""
		case "rtsp":
			if err := checkURL(cam.URL, "rtsp", "rtsps"); err != nil {
				return fmt.Errorf("%s: %v", cam.Name, err)
			}
			cam.VideoDevice, cam.AudioDevice, cam.AudioURL = "", "", ""
		case "http":
			if err := checkURL(cam.URL, "http", "https"); err != nil {
				return fmt.Errorf("%s: %v", cam.Name, err)
			}
			if cam.AudioURL != "" {
				if err := checkURL(cam.AudioURL, "http", "https"); err != nil {
					return fmt.Errorf("%s (소리 주소): %v", cam.Name, err)
				}
			}
			cam.VideoDevice, cam.AudioDevice = "", ""
		default:
			return fmt.Errorf("%s: 알 수 없는 카메라 종류 %q", cam.Name, cam.Type)
		}
	}
	return nil
}

func checkURL(raw string, schemes ...string) error {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Host == "" {
		return fmt.Errorf("주소가 올바르지 않습니다 (%s://... 형식)", schemes[0])
	}
	for _, s := range schemes {
		if strings.EqualFold(u.Scheme, s) {
			return nil
		}
	}
	return fmt.Errorf("주소는 %s:// 로 시작해야 합니다", schemes[0])
}
