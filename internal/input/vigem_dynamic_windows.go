//go:build windows && !cgo

package input

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	vigemErrorNone                = 0x20000000
	vigemErrorBusNotFound         = 0xE0000001
	vigemErrorNoFreeSlot          = 0xE0000002
	vigemErrorInvalidTarget       = 0xE0000003
	vigemErrorTargetUninitialized = 0xE0000006
	vigemErrorTargetNotPluggedIn  = 0xE0000007
	vigemErrorBusVersionMismatch  = 0xE0000008
	vigemErrorBusAccessFailed     = 0xE0000009
	vigemErrorBusAlreadyConnected = 0xE0000012
	vigemErrorBusInvalidHandle    = 0xE0000013
	vigemErrorInvalidParameter    = 0xE0000015
	vigemErrorWinAPI              = 0xE0000017
	vigemErrorTimedOut            = 0xE0000018
	vigemErrorIsDisposing         = 0xE0000019
)

const (
	xUp        = 0x0001
	xDown      = 0x0002
	xLeft      = 0x0004
	xRight     = 0x0008
	xStart     = 0x0010
	xBack      = 0x0020
	xLThumb    = 0x0040
	xRThumb    = 0x0080
	xLShoulder = 0x0100
	xRShoulder = 0x0200
	xGuide     = 0x0400
	xA         = 0x1000
	xB         = 0x2000
	xX         = 0x4000
	xY         = 0x8000
)

var vigemDLL = syscall.NewLazyDLL("vigemclient.dll")

type vigemProcs struct {
	alloc             *syscall.LazyProc
	free              *syscall.LazyProc
	connect           *syscall.LazyProc
	disconnect        *syscall.LazyProc
	targetX360Alloc   *syscall.LazyProc
	targetFree        *syscall.LazyProc
	targetAdd         *syscall.LazyProc
	targetRemove      *syscall.LazyProc
	targetX360Update  *syscall.LazyProc
	targetIsAttached  *syscall.LazyProc
	targetX360UserIdx *syscall.LazyProc
}

type vigemDynamicInjector struct {
	mu     sync.Mutex
	procs  vigemProcs
	client uintptr
	target uintptr
	report xusbReport
}

type xusbReport struct {
	buttons      uint16
	leftTrigger  uint8
	rightTrigger uint8
	leftThumbX   int16
	leftThumbY   int16
	rightThumbX  int16
	rightThumbY  int16
}

func newViGEmDynamicInjector() (Injector, error) {
	dllPath, dllDir := locateViGEmDLL()
	if dllDir != "" {
		if err := windows.SetDllDirectory(dllDir); err != nil {
			return nil, fmt.Errorf("SetDllDirectory(%s): %w", dllDir, err)
		}
	}

	procs := newViGEmProcs()
	if err := procs.find(); err != nil {
		return nil, fmt.Errorf("ViGEmClient.dll não encontrada/carregável (defina TETHER_VIGEM_DLL ou coloque a DLL ao lado do executável): %w", err)
	}

	client, err := procs.callPtr(procs.alloc)
	if err != nil {
		return nil, fmt.Errorf("vigem_alloc: %w", err)
	}
	if client == 0 {
		return nil, fmt.Errorf("vigem_alloc retornou handle nulo")
	}
	cleanupClient := true
	defer func() {
		if cleanupClient {
			_, _, _ = procs.free.Call(client)
		}
	}()

	if code, err := procs.callCode(procs.connect, client); err != nil {
		return nil, fmt.Errorf("vigem_connect: %w", err)
	} else if code != vigemErrorNone && code != vigemErrorBusAlreadyConnected {
		return nil, fmt.Errorf("vigem_connect: %s", vigemError(code))
	}

	target, err := procs.callPtr(procs.targetX360Alloc)
	if err != nil {
		return nil, fmt.Errorf("vigem_target_x360_alloc: %w", err)
	}
	if target == 0 {
		return nil, fmt.Errorf("vigem_target_x360_alloc retornou handle nulo")
	}
	cleanupTarget := true
	defer func() {
		if cleanupTarget {
			_, _, _ = procs.targetFree.Call(target)
			_, _, _ = procs.disconnect.Call(client)
		}
	}()

	if code, err := procs.callCode(procs.targetAdd, client, target); err != nil {
		return nil, fmt.Errorf("vigem_target_add: %w", err)
	} else if code != vigemErrorNone {
		return nil, fmt.Errorf("vigem_target_add: %s", vigemError(code))
	}

	v := &vigemDynamicInjector{procs: procs, client: client, target: target}
	if err := v.updateLocked(); err != nil {
		_ = v.Close()
		return nil, err
	}

	cleanupClient = false
	cleanupTarget = false
	if dllPath != "" {
		log.Printf("[input] injector ViGEm ativo (Xbox 360 virtual; DLL=%s)", dllPath)
	} else {
		log.Println("[input] injector ViGEm ativo (Xbox 360 virtual)")
	}
	return v, nil
}

func newViGEmProcs() vigemProcs {
	return vigemProcs{
		alloc:             vigemDLL.NewProc("vigem_alloc"),
		free:              vigemDLL.NewProc("vigem_free"),
		connect:           vigemDLL.NewProc("vigem_connect"),
		disconnect:        vigemDLL.NewProc("vigem_disconnect"),
		targetX360Alloc:   vigemDLL.NewProc("vigem_target_x360_alloc"),
		targetFree:        vigemDLL.NewProc("vigem_target_free"),
		targetAdd:         vigemDLL.NewProc("vigem_target_add"),
		targetRemove:      vigemDLL.NewProc("vigem_target_remove"),
		targetX360Update:  vigemDLL.NewProc("vigem_target_x360_update"),
		targetIsAttached:  vigemDLL.NewProc("vigem_target_is_attached"),
		targetX360UserIdx: vigemDLL.NewProc("vigem_target_x360_get_user_index"),
	}
}

func (p vigemProcs) find() error {
	for _, proc := range []*syscall.LazyProc{
		p.alloc,
		p.free,
		p.connect,
		p.disconnect,
		p.targetX360Alloc,
		p.targetFree,
		p.targetAdd,
		p.targetRemove,
		p.targetX360Update,
	} {
		if err := proc.Find(); err != nil {
			return err
		}
	}
	return nil
}

func (p vigemProcs) callPtr(proc *syscall.LazyProc, args ...uintptr) (uintptr, error) {
	ret, _, err := proc.Call(args...)
	if ret != 0 {
		return ret, nil
	}
	if err != syscall.Errno(0) {
		return ret, err
	}
	return ret, nil
}

func (p vigemProcs) callCode(proc *syscall.LazyProc, args ...uintptr) (uint32, error) {
	ret, _, err := proc.Call(args...)
	code := uint32(ret)
	if code == vigemErrorNone || code == vigemErrorBusAlreadyConnected {
		return code, nil
	}
	if err != syscall.Errno(0) {
		return code, err
	}
	return code, nil
}

func (v *vigemDynamicInjector) Apply(s GamepadState) error {
	v.mu.Lock()
	defer v.mu.Unlock()

	var buttons uint16
	set := func(cond bool, mask uint16) {
		if cond {
			buttons |= mask
		}
	}
	set(pressed(s, BtnDUp), xUp)
	set(pressed(s, BtnDDown), xDown)
	set(pressed(s, BtnDLeft), xLeft)
	set(pressed(s, BtnDRight), xRight)
	set(pressed(s, BtnStart), xStart)
	set(pressed(s, BtnBack), xBack)
	set(pressed(s, BtnLStick), xLThumb)
	set(pressed(s, BtnRStick), xRThumb)
	set(pressed(s, BtnLB), xLShoulder)
	set(pressed(s, BtnRB), xRShoulder)
	set(pressed(s, BtnGuide), xGuide)
	set(pressed(s, BtnA), xA)
	set(pressed(s, BtnB), xB)
	set(pressed(s, BtnX), xX)
	set(pressed(s, BtnY), xY)

	v.report = xusbReport{
		buttons:      buttons,
		leftTrigger:  triggerByteDynamic(s, BtnLT, 4),
		rightTrigger: triggerByteDynamic(s, BtnRT, 5),
		leftThumbX:   stickDynamic(axis(s, 0)),
		leftThumbY:   stickDynamic(-axis(s, 1)),
		rightThumbX:  stickDynamic(axis(s, 2)),
		rightThumbY:  stickDynamic(-axis(s, 3)),
	}
	return v.updateLocked()
}

func (v *vigemDynamicInjector) Command(cmd Command) error {
	return sendCommand(cmd)
}

func (v *vigemDynamicInjector) Close() error {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.report = xusbReport{}
	_ = v.updateLocked()
	if v.target != 0 {
		_, _, _ = v.procs.targetRemove.Call(v.client, v.target)
		_, _, _ = v.procs.targetFree.Call(v.target)
		v.target = 0
	}
	if v.client != 0 {
		_, _, _ = v.procs.disconnect.Call(v.client)
		_, _, _ = v.procs.free.Call(v.client)
		v.client = 0
	}
	return nil
}

func (v *vigemDynamicInjector) updateLocked() error {
	code, err := v.procs.callCode(
		v.procs.targetX360Update,
		v.client,
		v.target,
		uintptr(unsafe.Pointer(&v.report)),
	)
	if err != nil {
		return fmt.Errorf("vigem_target_x360_update: %w", err)
	}
	if code != vigemErrorNone {
		return fmt.Errorf("vigem_target_x360_update: %s", vigemError(code))
	}
	return nil
}

func locateViGEmDLL() (dllPath, dllDir string) {
	for _, candidate := range vigemDLLCandidates() {
		if candidate == "" {
			continue
		}
		path := candidate
		info, err := os.Stat(path)
		if err == nil && info.IsDir() {
			path = filepath.Join(path, "ViGEmClient.dll")
		}
		if _, err := os.Stat(path); err == nil {
			return path, filepath.Dir(path)
		}
	}
	return "", ""
}

func vigemDLLCandidates() []string {
	var candidates []string
	addDir := func(dir string) {
		if strings.TrimSpace(dir) == "" {
			return
		}
		candidates = append(candidates,
			filepath.Join(dir, "ViGEmClient.dll"),
			filepath.Join(dir, "vigemclient.dll"),
		)
	}

	if env := strings.TrimSpace(os.Getenv("TETHER_VIGEM_DLL")); env != "" {
		candidates = append(candidates, env)
	}
	if env := strings.TrimSpace(os.Getenv("TETHER_VIGEM_DIR")); env != "" {
		addDir(env)
	}
	if exe, err := os.Executable(); err == nil {
		addDir(filepath.Dir(exe))
	}
	if cwd, err := os.Getwd(); err == nil {
		addDir(cwd)
		addDir(filepath.Join(cwd, "bin"))
		addDir(filepath.Join(cwd, "internal", "input", "vigem", "bin"))
		addDir(filepath.Join(cwd, "internal", "input", "vigem", "bin", "x64"))
	}
	addDir(filepath.Join(os.Getenv("LOCALAPPDATA"), "Tether", "bin"))
	addDir(filepath.Join(os.Getenv("ProgramFiles"), "Nefarius Software Solutions", "ViGEm Bus Driver"))
	addDir(filepath.Join(os.Getenv("ProgramFiles(x86)"), "Nefarius Software Solutions", "ViGEm Bus Driver"))
	return candidates
}

func vigemError(code uint32) string {
	name := map[uint32]string{
		vigemErrorNone:                "VIGEM_ERROR_NONE",
		vigemErrorBusNotFound:         "VIGEM_ERROR_BUS_NOT_FOUND",
		vigemErrorNoFreeSlot:          "VIGEM_ERROR_NO_FREE_SLOT",
		vigemErrorInvalidTarget:       "VIGEM_ERROR_INVALID_TARGET",
		vigemErrorTargetUninitialized: "VIGEM_ERROR_TARGET_UNINITIALIZED",
		vigemErrorTargetNotPluggedIn:  "VIGEM_ERROR_TARGET_NOT_PLUGGED_IN",
		vigemErrorBusVersionMismatch:  "VIGEM_ERROR_BUS_VERSION_MISMATCH",
		vigemErrorBusAccessFailed:     "VIGEM_ERROR_BUS_ACCESS_FAILED",
		vigemErrorBusAlreadyConnected: "VIGEM_ERROR_BUS_ALREADY_CONNECTED",
		vigemErrorBusInvalidHandle:    "VIGEM_ERROR_BUS_INVALID_HANDLE",
		vigemErrorInvalidParameter:    "VIGEM_ERROR_INVALID_PARAMETER",
		vigemErrorWinAPI:              "VIGEM_ERROR_WINAPI",
		vigemErrorTimedOut:            "VIGEM_ERROR_TIMED_OUT",
		vigemErrorIsDisposing:         "VIGEM_ERROR_IS_DISPOSING",
	}[code]
	if name == "" {
		name = "VIGEM_ERROR_UNKNOWN"
	}
	return fmt.Sprintf("%s(0x%08x)", name, code)
}

func stickDynamic(f float64) int16 {
	if f > 1 {
		f = 1
	}
	if f < -1 {
		f = -1
	}
	if f >= 0 {
		return int16(math.Round(f * 32767))
	}
	return int16(math.Round(f * 32768))
}

func triggerByteDynamic(s GamepadState, btnIdx, axisIdx int) uint8 {
	if axisIdx < len(s.Axes) {
		v := axis(s, axisIdx)
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		return uint8(math.Round(v * 255))
	}
	if pressed(s, btnIdx) {
		return 255
	}
	return 0
}
