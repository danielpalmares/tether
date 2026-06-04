//go:build windows

package process

import (
	"os"

	"golang.org/x/sys/windows"
)

func BoostCurrent() error {
	return BoostPID(os.Getpid())
}

func BoostPID(pid int) error {
	h, err := windows.OpenProcess(windows.PROCESS_SET_INFORMATION, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(h)

	return windows.SetPriorityClass(h, windows.HIGH_PRIORITY_CLASS)
}
