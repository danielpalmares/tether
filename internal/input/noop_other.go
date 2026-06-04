//go:build !windows

package input

import "log"

// noopInjector apenas loga os estados — permite desenvolver o pipeline fora
// do Windows sem o ViGEmBus.
type noopInjector struct{}

// NewInjector retorna um injetor que só registra em log (dev fora do Windows).
func NewInjector() (Injector, error) {
	log.Println("[input] injetor noop ativo (não-Windows): input será logado, não injetado")
	return &noopInjector{}, nil
}

func (n *noopInjector) Apply(s GamepadState) error {
	// descomente para debug detalhado:
	// log.Printf("[input] botões=%v eixos=%v", s.Buttons, s.Axes)
	return nil
}

func (n *noopInjector) Command(cmd Command) error {
	return nil
}

func (n *noopInjector) Close() error { return nil }
