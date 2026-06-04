//go:build windows && !cgo

package input

import (
	"log"
	"sync"
)

type sendInputInjector struct {
	mu   sync.Mutex
	held map[uint16]bool
}

func NewInjector() (Injector, error) {
	if vigem, err := newViGEmDynamicInjector(); err == nil {
		return vigem, nil
	} else {
		log.Printf("[input] ViGEm indisponível: %v", err)
	}

	log.Println("[input] injector SendInput ativo (fallback teclado/mouse; jogos XInput precisam de ViGEmClient.dll)")
	return &sendInputInjector{held: map[uint16]bool{}}, nil
}

func (s *sendInputInjector) Apply(state GamepadState) error {
	desired := map[uint16]bool{}

	set := func(vk uint16, down bool) {
		if down {
			desired[vk] = true
		}
	}

	lx := axis(state, 0)
	ly := axis(state, 1)
	set(vkLeft, pressed(state, BtnDLeft) || lx < -0.45)
	set(vkRight, pressed(state, BtnDRight) || lx > 0.45)
	set(vkUp, pressed(state, BtnDUp) || ly < -0.45)
	set(vkDown, pressed(state, BtnDDown) || ly > 0.45)

	set(vkEnter, pressed(state, BtnA) || pressed(state, BtnStart))
	set(vkEscape, pressed(state, BtnB))
	set(uint16('X'), pressed(state, BtnX))
	set(uint16('Y'), pressed(state, BtnY))
	set(uint16('Q'), pressed(state, BtnLB))
	set(uint16('E'), pressed(state, BtnRB))
	set(vkBack, pressed(state, BtnBack))
	set(vkShift, pressed(state, BtnLStick))
	set(vkControl, pressed(state, BtnRStick))

	s.mu.Lock()
	defer s.mu.Unlock()

	for vk, isHeld := range s.held {
		if isHeld && !desired[vk] {
			if err := sendKey(vk, false); err != nil {
				return err
			}
			delete(s.held, vk)
		}
	}
	for vk := range desired {
		if !s.held[vk] {
			if err := sendKey(vk, true); err != nil {
				return err
			}
			s.held[vk] = true
		}
	}
	return nil
}

func (s *sendInputInjector) Command(cmd Command) error {
	return sendCommand(cmd)
}

func (s *sendInputInjector) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	for vk := range s.held {
		_ = sendKey(vk, false)
	}
	clear(s.held)
	return nil
}
