//go:build windows && cgo

package input

/*
Injetor de input para Windows via ViGEmBus.

PRÉ-REQUISITO: o driver ViGEmBus precisa estar instalado no host:
  https://github.com/ViGEm/ViGEmBus/releases

Esta implementação usa a biblioteca cgo de binding do ViGEmClient. Para o MVP,
o caminho mais simples em Go é usar o pacote github.com/google/... porém não há
binding oficial puro-Go estável. As duas rotas práticas:

  (A) cgo + ViGEmClient.dll  (recomendado p/ produção)
  (B) chamar um helper externo

Aqui deixamos a estrutura pronta com cgo. Ajuste o caminho do header/lib do
ViGEmClient conforme sua instalação do SDK.
*/

// #cgo CFLAGS: -I${SRCDIR}/vigem/include
// #cgo LDFLAGS: -L${SRCDIR}/vigem/lib -lViGEmClient -lsetupapi
// #include <ViGEm/Client.h>
// #include <stdlib.h>
//
// // helpers para alocar e submeter estado
// static PVIGEM_CLIENT mk_client() { return vigem_alloc(); }
import "C"

import (
	"fmt"
)

type vigemInjector struct {
	client C.PVIGEM_CLIENT
	pad    C.PVIGEM_TARGET
	report C.XUSB_REPORT
}

// NewInjector conecta no ViGEmBus e cria um Xbox 360 virtual.
func NewInjector() (Injector, error) {
	cli := C.mk_client()
	if cli == nil {
		return nil, fmt.Errorf("vigem_alloc falhou")
	}
	if r := C.vigem_connect(cli); r != C.VIGEM_ERROR_NONE {
		return nil, fmt.Errorf("vigem_connect falhou (ViGEmBus instalado?): code=%d", int(r))
	}
	pad := C.vigem_target_x360_alloc()
	if r := C.vigem_target_add(cli, pad); r != C.VIGEM_ERROR_NONE {
		return nil, fmt.Errorf("vigem_target_add falhou: code=%d", int(r))
	}
	return &vigemInjector{client: cli, pad: pad}, nil
}

// xinput button bitmasks
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

func (v *vigemInjector) Apply(s GamepadState) error {
	var btns C.USHORT
	set := func(cond bool, mask int) {
		if cond {
			btns |= C.USHORT(mask)
		}
	}
	set(pressed(s, BtnA), xA)
	set(pressed(s, BtnB), xB)
	set(pressed(s, BtnX), xX)
	set(pressed(s, BtnY), xY)
	set(pressed(s, BtnLB), xLShoulder)
	set(pressed(s, BtnRB), xRShoulder)
	set(pressed(s, BtnBack), xBack)
	set(pressed(s, BtnStart), xStart)
	set(pressed(s, BtnLStick), xLThumb)
	set(pressed(s, BtnRStick), xRThumb)
	set(pressed(s, BtnDUp), xUp)
	set(pressed(s, BtnDDown), xDown)
	set(pressed(s, BtnDLeft), xLeft)
	set(pressed(s, BtnDRight), xRight)
	set(pressed(s, BtnGuide), xGuide)

	v.report.wButtons = btns

	// gatilhos: a Gamepad API standard expõe LT/RT como botões 6/7 com valor
	// analógico; o client envia esse valor em Axes[4]/Axes[5] se disponível,
	// senão usamos o estado pressionado como 0/255.
	v.report.bLeftTrigger = triggerByte(s, BtnLT, 4)
	v.report.bRightTrigger = triggerByte(s, BtnRT, 5)

	// sticks: -1..1 -> -32768..32767
	v.report.sThumbLX = stick(axis(s, 0))
	v.report.sThumbLY = stick(-axis(s, 1)) // Y invertido (browser: pra baixo = +)
	v.report.sThumbRX = stick(axis(s, 2))
	v.report.sThumbRY = stick(-axis(s, 3))

	if r := C.vigem_target_x360_update(v.client, v.pad, v.report); r != C.VIGEM_ERROR_NONE {
		return fmt.Errorf("x360_update falhou: code=%d", int(r))
	}
	return nil
}

func (v *vigemInjector) Command(cmd Command) error {
	return sendCommand(cmd)
}

func (v *vigemInjector) Close() error {
	if v.client != nil {
		if v.pad != nil {
			C.vigem_target_remove(v.client, v.pad)
			C.vigem_target_free(v.pad)
		}
		C.vigem_disconnect(v.client)
		C.vigem_free(v.client)
	}
	return nil
}

func stick(f float64) C.SHORT {
	if f > 1 {
		f = 1
	}
	if f < -1 {
		f = -1
	}
	return C.SHORT(f * 32767)
}

func triggerByte(s GamepadState, btnIdx, axisIdx int) C.BYTE {
	// prioriza valor analógico se o client mandar no array de eixos estendido
	if axisIdx < len(s.Axes) {
		v := s.Axes[axisIdx]
		if v < 0 {
			v = 0
		}
		if v > 1 {
			v = 1
		}
		return C.BYTE(v * 255)
	}
	if pressed(s, btnIdx) {
		return 255
	}
	return 0
}
