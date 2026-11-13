//go:build !windows

package main

import (
	"context"
	"errors"
	"os/exec"
)

func setProcAttrs(cmd *exec.Cmd) {}
func bindChildren()              {}
func hideConsole()               {}

var errNoAutostart = errors.New("이 운영체제의 자동 시작 설치는 아직 지원하지 않습니다 (systemd/launchd 예정)")

func runAsService(run func(ctx context.Context)) (bool, error) { return false, nil }
func installAutostart(userMode bool, dataDir string) error     { return errNoAutostart }
func uninstallAutostart() error                                { return errNoAutostart }
