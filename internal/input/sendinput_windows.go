//go:build windows

package input

import (
	"fmt"
	"math"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

const (
	inputMouse    = 0
	inputKeyboard = 1

	keyeventfKeyUp = 0x0002

	mouseeventfMove       = 0x0001
	mouseeventfLeftDown   = 0x0002
	mouseeventfLeftUp     = 0x0004
	mouseeventfRightDown  = 0x0008
	mouseeventfRightUp    = 0x0010
	mouseeventfMiddleDown = 0x0020
	mouseeventfMiddleUp   = 0x0040
	mouseeventfWheel      = 0x0800

	vkBack    = 0x08
	vkTab     = 0x09
	vkEnter   = 0x0D
	vkShift   = 0x10
	vkControl = 0x11
	vkMenu    = 0x12
	vkEscape  = 0x1B
	vkSpace   = 0x20
	vkPrior   = 0x21
	vkNext    = 0x22
	vkEnd     = 0x23
	vkHome    = 0x24
	vkLeft    = 0x25
	vkUp      = 0x26
	vkRight   = 0x27
	vkDown    = 0x28
	vkInsert  = 0x2D
	vkDelete  = 0x2E
)

var sendInputProc = windows.NewLazySystemDLL("user32.dll").NewProc("SendInput")

type inputEvent struct {
	Type uint32
	_    uint32
	Data [32]byte
}

type keyboardInput struct {
	VK        uint16
	Scan      uint16
	Flags     uint32
	Time      uint32
	_         uint32
	ExtraInfo uintptr
}

type mouseInput struct {
	DX        int32
	DY        int32
	MouseData uint32
	Flags     uint32
	Time      uint32
	_         uint32
	ExtraInfo uintptr
}

func sendCommand(cmd Command) error {
	switch cmd.Type {
	case "key":
		vk, ok := virtualKey(cmd.Code)
		if !ok {
			return nil
		}
		return sendKey(vk, cmd.Down)
	case "mouseMove":
		return sendMouseMove(cmd.DX, cmd.DY)
	case "mouseButton":
		return sendMouseButton(cmd.Button, cmd.Down)
	case "wheel":
		return sendWheel(cmd.DeltaY)
	default:
		return nil
	}
}

func sendKey(vk uint16, down bool) error {
	flags := uint32(0)
	if !down {
		flags = keyeventfKeyUp
	}
	ev := inputEvent{Type: inputKeyboard}
	*(*keyboardInput)(unsafe.Pointer(&ev.Data[0])) = keyboardInput{VK: vk, Flags: flags}
	return sendInput([]inputEvent{ev})
}

func sendMouseMove(dx, dy int32) error {
	if dx == 0 && dy == 0 {
		return nil
	}
	ev := inputEvent{Type: inputMouse}
	*(*mouseInput)(unsafe.Pointer(&ev.Data[0])) = mouseInput{DX: dx, DY: dy, Flags: mouseeventfMove}
	return sendInput([]inputEvent{ev})
}

func sendMouseButton(button int, down bool) error {
	flags := uint32(0)
	switch button {
	case 0:
		if down {
			flags = mouseeventfLeftDown
		} else {
			flags = mouseeventfLeftUp
		}
	case 1:
		if down {
			flags = mouseeventfMiddleDown
		} else {
			flags = mouseeventfMiddleUp
		}
	case 2:
		if down {
			flags = mouseeventfRightDown
		} else {
			flags = mouseeventfRightUp
		}
	default:
		return nil
	}

	ev := inputEvent{Type: inputMouse}
	*(*mouseInput)(unsafe.Pointer(&ev.Data[0])) = mouseInput{Flags: flags}
	return sendInput([]inputEvent{ev})
}

func sendWheel(deltaY float64) error {
	if deltaY == 0 {
		return nil
	}
	wheel := int32(-math.Round(deltaY))
	if wheel > 120 {
		wheel = 120
	}
	if wheel < -120 {
		wheel = -120
	}
	ev := inputEvent{Type: inputMouse}
	*(*mouseInput)(unsafe.Pointer(&ev.Data[0])) = mouseInput{MouseData: uint32(wheel), Flags: mouseeventfWheel}
	return sendInput([]inputEvent{ev})
}

func sendInput(events []inputEvent) error {
	if len(events) == 0 {
		return nil
	}
	ret, _, err := sendInputProc.Call(
		uintptr(len(events)),
		uintptr(unsafe.Pointer(&events[0])),
		unsafe.Sizeof(inputEvent{}),
	)
	if ret == uintptr(len(events)) {
		return nil
	}
	if err != windows.ERROR_SUCCESS {
		return err
	}
	return fmt.Errorf("SendInput enviou %d/%d eventos", ret, len(events))
}

func virtualKey(code string) (uint16, bool) {
	code = strings.TrimSpace(code)
	if len(code) == 4 && strings.HasPrefix(code, "Key") {
		ch := code[3]
		if ch >= 'A' && ch <= 'Z' {
			return uint16(ch), true
		}
	}
	if len(code) == 6 && strings.HasPrefix(code, "Digit") {
		ch := code[5]
		if ch >= '0' && ch <= '9' {
			return uint16(ch), true
		}
	}
	switch code {
	case "Space":
		return vkSpace, true
	case "Enter":
		return vkEnter, true
	case "Escape":
		return vkEscape, true
	case "Tab":
		return vkTab, true
	case "Backspace":
		return vkBack, true
	case "ShiftLeft", "ShiftRight":
		return vkShift, true
	case "ControlLeft", "ControlRight":
		return vkControl, true
	case "AltLeft", "AltRight":
		return vkMenu, true
	case "ArrowUp":
		return vkUp, true
	case "ArrowDown":
		return vkDown, true
	case "ArrowLeft":
		return vkLeft, true
	case "ArrowRight":
		return vkRight, true
	case "Insert":
		return vkInsert, true
	case "Delete":
		return vkDelete, true
	case "Home":
		return vkHome, true
	case "End":
		return vkEnd, true
	case "PageUp":
		return vkPrior, true
	case "PageDown":
		return vkNext, true
	}
	if strings.HasPrefix(code, "F") {
		var n int
		if _, err := fmt.Sscanf(code, "F%d", &n); err == nil && n >= 1 && n <= 12 {
			return uint16(0x70 + n - 1), true
		}
	}
	return 0, false
}
