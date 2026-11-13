package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/registry"
	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/mgr"
)

const (
	serviceName = "mySurveillance"
	runKeyPath  = `Software\Microsoft\Windows\CurrentVersion\Run`
)

type svcHandler struct{ run func(ctx context.Context) }

func (h svcHandler) Execute(_ []string, r <-chan svc.ChangeRequest, s chan<- svc.Status) (bool, uint32) {
	s <- svc.Status{State: svc.StartPending}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() { h.run(ctx); close(done) }()
	s <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
	for {
		select {
		case c := <-r:
			switch c.Cmd {
			case svc.Interrogate:
				s <- c.CurrentStatus
			case svc.Stop, svc.Shutdown:
				s <- svc.Status{State: svc.StopPending}
				cancel()
				<-done
				return false, 0
			}
		case <-done:
			// The app stopped on its own: report failure so Windows restarts it.
			return false, 1
		}
	}
}

// runAsService reports whether the process was started by the Windows service
// manager, and if so runs until the service is stopped.
func runAsService(run func(ctx context.Context)) (bool, error) {
	is, err := svc.IsWindowsService()
	if err != nil || !is {
		return false, err
	}
	return true, svc.Run(serviceName, svcHandler{run})
}

// installAutostart registers the program to start with the computer.
//
//	service mode (default, needs administrator): starts at boot before anyone
//	logs in, and Windows restarts it automatically if it ever stops.
//	user mode: starts when the current user logs in; no administrator needed.
func installAutostart(userMode bool, dataDir string) error {
	exe, err := os.Executable()
	if err != nil {
		return err
	}
	if userMode {
		k, _, err := registry.CreateKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE)
		if err != nil {
			return err
		}
		defer k.Close()
		return k.SetStringValue(serviceName, fmt.Sprintf(`"%s" run --background --data "%s"`, exe, dataDir))
	}
	m, err := mgr.Connect()
	if err != nil {
		return fmt.Errorf("서비스 관리자에 연결할 수 없습니다 (관리자 권한으로 실행하세요): %w", err)
	}
	defer m.Disconnect()
	if old, err := m.OpenService(serviceName); err == nil {
		old.Close()
		return errors.New("이미 설치되어 있습니다. 먼저 uninstall 을 실행하세요")
	}
	s, err := m.CreateService(serviceName, exe, mgr.Config{
		DisplayName: "mySurveillance CCTV Recorder",
		Description: "카메라 영상을 상시 녹화합니다.",
		StartType:   mgr.StartAutomatic,
	}, "run", "--data", dataDir)
	if err != nil {
		return err
	}
	defer s.Close()
	restart := mgr.RecoveryAction{Type: mgr.ServiceRestart, Delay: 5 * time.Second}
	if err := s.SetRecoveryActions([]mgr.RecoveryAction{restart, restart, restart}, 3600); err != nil {
		return err
	}
	return s.Start()
}

func uninstallAutostart() error {
	removedUser := false
	if k, err := registry.OpenKey(registry.CURRENT_USER, runKeyPath, registry.SET_VALUE); err == nil {
		removedUser = k.DeleteValue(serviceName) == nil
		k.Close()
	}
	m, err := mgr.Connect()
	if err != nil {
		if removedUser {
			return nil // it was a per-user install; no administrator rights needed
		}
		return fmt.Errorf("서비스 관리자에 연결할 수 없습니다 (서비스로 설치했다면 관리자 권한 필요): %w", err)
	}
	defer m.Disconnect()
	s, err := m.OpenService(serviceName)
	if err != nil {
		return nil // not installed as a service
	}
	defer s.Close()
	s.Control(svc.Stop)
	return s.Delete()
}
