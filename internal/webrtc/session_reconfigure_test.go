package webrtc

import (
	"errors"
	"testing"

	"tether/internal/config"
	"tether/internal/input"
)

// TestReconfigureHotSwap cobre o contrato do hot-swap: mudança de
// resolução/bitrate troca o pipeline sem fechar a sessão; mudança de codec
// exige restart; sessão fechada rejeita.
func TestReconfigureHotSwap(t *testing.T) {
	disableCaptureForTest(t)

	injector, err := input.NewInjector()
	if err != nil {
		t.Fatalf("injector: %v", err)
	}
	closed := false
	sess, err := NewSession(config.Default(), injector, func() { closed = true }, nil)
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	next := sess.Config()
	next.Width, next.Height = 3840, 2160
	next.Bitrate = 30000
	if err := sess.Reconfigure(next); err != nil {
		t.Fatalf("hot-swap de resolução/bitrate deveria funcionar: %v", err)
	}
	if got := sess.Config(); got.Width != 3840 || got.Bitrate != 30000 {
		t.Errorf("config não aplicada: %+v", got)
	}
	if closed {
		t.Error("hot-swap não pode fechar a sessão")
	}

	// Mesma config: no-op sem erro.
	if err := sess.Reconfigure(next); err != nil {
		t.Errorf("reconfig idêntica deveria ser no-op: %v", err)
	}

	// Troca de codec: exige restart da sessão.
	codecChange := next
	codecChange.Codec = config.CodecH264X264
	if err := sess.Reconfigure(codecChange); !errors.Is(err, ErrNeedsRestart) {
		t.Errorf("troca de codec deveria devolver ErrNeedsRestart, veio: %v", err)
	}

	sess.Close()
	if !closed {
		t.Error("Close não disparou onClose")
	}
	if err := sess.Reconfigure(next); err == nil {
		t.Error("Reconfigure após Close deveria falhar")
	}
}
