package main

import (
	"log"
	"net"
	"os/exec"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

func setProcAttrs(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: 0x08000000} // CREATE_NO_WINDOW
}

// bindChildren puts this process in a job object so that every ffmpeg child
// dies with it, even if this process is killed. Otherwise an orphaned ffmpeg
// would keep the camera locked and block the restarted recorder.
func bindChildren() {
	job, err := windows.CreateJobObject(nil, nil)
	if err != nil {
		log.Printf("job object 생성 실패: %v", err)
		return
	}
	var info windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION
	info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
	if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
		uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
		log.Printf("job object 설정 실패: %v", err)
		return
	}
	if err := windows.AssignProcessToJobObject(job, windows.CurrentProcess()); err != nil {
		log.Printf("job object 연결 실패: %v", err)
	}
	// the handle is intentionally kept open for the life of the process
}

func hideConsole() {
	hwnd, _, _ := windows.NewLazySystemDLL("kernel32.dll").NewProc("GetConsoleWindow").Call()
	if hwnd != 0 {
		windows.NewLazySystemDLL("user32.dll").NewProc("ShowWindow").Call(hwnd, 0)
	}
}

// multicastLoop lets a multicast listener also hear senders on this computer
// (Go turns that off by default; on Windows it is a receiver-side option).
func multicastLoop(c *net.UDPConn) {
	if rc, err := c.SyscallConn(); err == nil {
		rc.Control(func(fd uintptr) {
			windows.SetsockoptInt(windows.Handle(fd), windows.IPPROTO_IP, windows.IP_MULTICAST_LOOP, 1)
		})
	}
}
