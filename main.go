// mySurveillance: an always-on CCTV recorder for USB cameras, IP cameras and
// phone camera apps, with a quota-managed archive and an HTTP API for viewers.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"
)

var version = "0.1.0"

type App struct {
	dataDir string
	cfgPath string
	ffmpeg  string
	mgr     *Manager
	store   *Storage
	vod     vodCache
	limiter loginLimiter

	updMu sync.Mutex // serializes Update, including applying to recorders
	mu    sync.RWMutex
	cfg   Config
}

func (a *App) Config() Config {
	a.mu.RLock()
	defer a.mu.RUnlock()
	return a.cfg.clone()
}

// Update changes the config, persists it and re-applies it to the recorders.
//
// Whole updates run one at a time: applying to the recorders can take seconds
// (ffmpeg is stopped gracefully), and two overlapping updates must not apply
// their snapshots out of order. Readers of Config are not blocked meanwhile.
func (a *App) Update(fn func(*Config) error) error {
	a.updMu.Lock()
	defer a.updMu.Unlock()
	a.mu.Lock()
	next := a.cfg.clone()
	if err := fn(&next); err != nil {
		a.mu.Unlock()
		return err
	}
	if err := saveConfig(a.cfgPath, next); err != nil {
		a.mu.Unlock()
		return err
	}
	a.cfg = next
	a.mu.Unlock()
	a.mgr.Apply(next.clone(), a.ffmpeg)
	return nil
}

func (a *App) initialPasswordFile() string { return filepath.Join(a.dataDir, "initial-password.txt") }
func (a *App) removeInitialPassword()      { os.Remove(a.initialPasswordFile()) }

func setupLog(dataDir string) {
	dir := filepath.Join(dataDir, "logs")
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "mysurveillance.log")
	if fi, err := os.Stat(path); err == nil && fi.Size() > 10<<20 {
		os.Rename(path, path+".old")
	}
	f, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return
	}
	log.SetOutput(io.MultiWriter(os.Stderr, f))
}

func runApp(ctx context.Context, dataDir string) error {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return err
	}
	setupLog(dataDir)
	bindChildren()

	a := &App{dataDir: dataDir, cfgPath: filepath.Join(dataDir, "config.json"), mgr: newManager(), store: &Storage{}}
	cfg, fresh, err := loadConfig(a.cfgPath, dataDir)
	if err != nil {
		return err
	}
	if cfg.PasswordHash == "" {
		pw := randomHex(6)
		cfg.PasswordHash = hashPassword(pw)
		fresh = true
		msg := "mySurveillance 초기 비밀번호: " + pw + "\r\n웹 화면의 설정에서 비밀번호를 바꾸면 이 파일은 삭제됩니다.\r\n"
		if err := os.WriteFile(a.initialPasswordFile(), []byte(msg), 0o600); err != nil {
			return err
		}
		log.Printf("초기 비밀번호를 만들었습니다. 파일을 확인하세요: %s", a.initialPasswordFile())
	}
	if err := os.MkdirAll(cfg.StorageDir, 0o755); err != nil {
		return fmt.Errorf("저장 폴더를 만들 수 없습니다: %w", err)
	}
	if fresh {
		if err := saveConfig(a.cfgPath, cfg); err != nil {
			return err
		}
	}
	a.cfg = cfg

	// Binding the port doubles as the single-instance lock.
	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		return fmt.Errorf("%s 포트를 열 수 없습니다 (이미 실행 중일 수 있습니다): %w", cfg.Listen, err)
	}

	a.ffmpeg = findFFmpeg(cfg.FFmpegPath, dataDir)
	if a.ffmpeg == "" {
		log.Printf("경고: ffmpeg를 찾을 수 없어 녹화할 수 없습니다. 실행 파일 옆에 ffmpeg를 두세요.")
	} else {
		log.Printf("ffmpeg: %s", a.ffmpeg)
	}

	bg, stopBg := context.WithCancel(context.Background())
	defer stopBg()
	go a.store.loop(bg, a.Config)
	a.mgr.Apply(cfg.clone(), a.ffmpeg)

	srv := &http.Server{Handler: a.routes(), ReadHeaderTimeout: 15 * time.Second}
	errc := make(chan error, 1)
	go func() {
		if cfg.TLSCert != "" && cfg.TLSKey != "" {
			errc <- srv.ServeTLS(ln, cfg.TLSCert, cfg.TLSKey)
		} else {
			errc <- srv.Serve(ln)
		}
	}()
	port := cfg.Listen[strings.LastIndex(cfg.Listen, ":")+1:]
	log.Printf("mySurveillance %s 실행 중 — 이 컴퓨터에서 http://localhost:%s 로 접속하세요 (데이터: %s)", version, port, dataDir)

	select {
	case <-ctx.Done():
	case err = <-errc:
		log.Printf("웹 서버 오류: %v", err)
	}
	log.Printf("종료 중: 녹화 파일을 마무리합니다")
	a.mgr.StopAll()
	sctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	srv.Shutdown(sctx)
	if errors.Is(err, http.ErrServerClosed) {
		err = nil
	}
	return err
}

func main() {
	cmd, args := "run", os.Args[1:]
	if len(args) > 0 && !strings.HasPrefix(args[0], "-") {
		cmd, args = args[0], args[1:]
	}
	fs := flag.NewFlagSet(cmd, flag.ExitOnError)
	dataDir := fs.String("data", defaultDataDir(), "설정·로그·녹화가 저장되는 폴더")
	background := fs.Bool("background", false, "콘솔 창 숨기기")
	userMode := fs.Bool("user", false, "install: 관리자 권한 없이 현재 사용자 로그인 시 자동 시작")
	fs.Usage = func() {
		fmt.Fprintf(os.Stderr, `mySurveillance %s

사용법: mysurveillance [명령] [옵션]

  run        녹화 시작 (기본값)
  install    컴퓨터가 켜질 때 자동 시작하도록 등록 (관리자 권한, --user 는 권한 불필요)
  uninstall  자동 시작 해제
  devices    연결된 USB 카메라·마이크 목록
  version    버전

옵션:
`, version)
		fs.PrintDefaults()
	}
	fs.Parse(args)

	fail := func(err error) {
		if err != nil {
			fmt.Fprintln(os.Stderr, "오류:", err)
			os.Exit(1)
		}
	}
	switch cmd {
	case "run":
		var runErr error
		if ran, err := runAsService(func(ctx context.Context) {
			if runErr = runApp(ctx, *dataDir); runErr != nil {
				log.Printf("오류: %v", runErr)
			}
		}); ran {
			fail(errors.Join(err, runErr))
			return
		}
		if *background {
			hideConsole()
		}
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		fail(runApp(ctx, *dataDir))
	case "install":
		fail(installAutostart(*userMode, *dataDir))
		fmt.Println("자동 시작을 등록했습니다.")
	case "uninstall":
		fail(uninstallAutostart())
		fmt.Println("자동 시작을 해제했습니다.")
	case "devices":
		ff := findFFmpeg("", *dataDir)
		if ff == "" {
			fail(errors.New("ffmpeg를 찾을 수 없습니다"))
		}
		d := listDevices(ff)
		fmt.Println("카메라:")
		for _, v := range d.Video {
			fmt.Println("  " + v)
		}
		fmt.Println("마이크:")
		for _, v := range d.Audio {
			fmt.Println("  " + v)
		}
	case "version":
		fmt.Println(version)
	default:
		fs.Usage()
		os.Exit(2)
	}
}
