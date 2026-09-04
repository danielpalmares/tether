package webrtc

import (
	"context"
	"io"
	"sync/atomic"
	"testing"
	"time"

	"tether/internal/config"
)

// capturador falso: entrega um stream que termina sozinho, simulando o ddagrab
// sendo derrubado pelo Windows (jogo entrando em tela cheia exclusiva).
type dyingCapturer struct {
	starts atomic.Int32
}

func (d *dyingCapturer) Start(context.Context) (io.ReadCloser, error) {
	d.starts.Add(1)
	// Stream vazio: o leitor de NALs vê EOF imediato, como no
	// "Error during demuxing" real.
	return io.NopCloser(emptyReader{}), nil
}
func (d *dyingCapturer) Stop() {}

type emptyReader struct{}

func (emptyReader) Read([]byte) (int, error) { return 0, io.EOF }

// Regressão da TELA PRETA no Silent Hill: quando o jogo entra em tela cheia, o
// Windows recria a swapchain e derruba o Desktop Duplication. O FFmpeg morre com
// "Error during demuxing" e ANTES o pipeline simplesmente parava: a sessão
// continuava conectada, o áudio seguia tocando e o vídeo ficava preto para
// sempre. O supervisor tem de levantar a captura de novo.
func TestSuperviseVideoRestartsCaptureAfterDeath(t *testing.T) {
	cap := &dyingCapturer{}
	orig := newCapturer
	newCapturer = func(config.StreamConfig) streamCapturer { return cap }
	defer func() { newCapturer = orig }()

	s := &Session{cfg: config.Default(), done: make(chan struct{})}
	track := &recordingSampleWriter{}

	done := make(chan struct{})
	go func() {
		s.superviseVideo(io.NopCloser(emptyReader{}), track)
		close(done)
	}()

	// Dá tempo para algumas tentativas de reinício acontecerem.
	time.Sleep(2500 * time.Millisecond)
	got := cap.starts.Load()

	s.Close()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("supervisor não encerrou após Close")
	}

	if got < 1 {
		t.Fatalf("captura não foi reiniciada após a morte do pipeline (starts=%d)", got)
	}
	t.Logf("captura reiniciada %d vezes antes do Close", got)
}

// Encerrar a sessão NÃO pode ser confundido com queda da captura: o supervisor
// tem de parar, e não ficar subindo FFmpeg para uma sessão que acabou.
func TestSuperviseVideoStopsOnSessionClose(t *testing.T) {
	cap := &dyingCapturer{}
	orig := newCapturer
	newCapturer = func(config.StreamConfig) streamCapturer { return cap }
	defer func() { newCapturer = orig }()

	s := &Session{cfg: config.Default(), done: make(chan struct{})}
	s.Close() // fechada ANTES de o supervisor rodar

	done := make(chan struct{})
	go func() {
		s.superviseVideo(io.NopCloser(emptyReader{}), &recordingSampleWriter{})
		close(done)
	}()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("supervisor não respeitou a sessão encerrada")
	}
	if n := cap.starts.Load(); n != 0 {
		t.Fatalf("sessão encerrada não deve reiniciar captura, mas houve %d starts", n)
	}
}
