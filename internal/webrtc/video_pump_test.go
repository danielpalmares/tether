package webrtc

import (
	"bytes"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

func TestReadH264AccessUnitsGroupsOneSamplePerFrame(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		testNAL(nalTypeAUD, 0xf0),
		testNAL(nalTypeSPS, 0x42, 0xc0, 0x2a),
		testNAL(nalTypePPS, 0xce, 0x06),
		testNAL(nalTypeIDR, 0x88, 0x84),
		testNAL(nalTypeAUD, 0xf0),
		testNAL(nalTypeNonIDR, 0x9a, 0x22),
	}, nil))

	out := make(chan encodedFrame, 4)
	var stats videoPumpStats
	frameDur := time.Second / 60

	if err := readH264AccessUnits(stream, frameDur, out, &stats); err != nil {
		t.Fatalf("read access units: %v", err)
	}

	if got := len(out); got != 2 {
		t.Fatalf("frames = %d, want 2", got)
	}

	first := <-out
	if !first.keyframe {
		t.Fatalf("first frame should be keyframe")
	}
	if first.duration != frameDur {
		t.Fatalf("duration = %s, want %s", first.duration, frameDur)
	}
	if got := bytes.Count(first.data, []byte(annexBStartCode)); got != 4 {
		t.Fatalf("first frame NAL count = %d, want 4", got)
	}

	second := <-out
	if second.keyframe {
		t.Fatalf("second frame should not be keyframe")
	}
	if got := bytes.Count(second.data, []byte(annexBStartCode)); got != 2 {
		t.Fatalf("second frame NAL count = %d, want 2", got)
	}
	if got := stats.readFrames.Load(); got != 2 {
		t.Fatalf("readFrames = %d, want 2", got)
	}
	if got := stats.maxQueueDepth.Load(); got != 2 {
		t.Fatalf("maxQueueDepth = %d, want 2", got)
	}
}

// O pacer anti-jitter pode segurar frames ADIANTADOS (até um frameDur) para
// suavizar a chegada na TV, mas NUNCA pode acumular atraso: frames que já estão
// disponíveis em lote (chegaram atrasados / em rajada) devem sair imediatamente.
// Este teste guarda a invariante de [[no-pacing-immediate-send]] na sua forma
// correta: alimentar um lote pronto de uma vez NÃO pode resultar em pacing — o
// relógio de saída realinha ao agora a cada frame atrasado, sem backlog.
func TestWriteVideoFramesDoesNotBacklogBurst(t *testing.T) {
	frameDur := time.Second / 60 // 16.6ms

	frames := make(chan encodedFrame, 10)
	for i := 1; i <= 10; i++ {
		frames <- encodedFrame{data: []byte{byte(i)}, duration: frameDur}
	}
	close(frames)

	writer := &recordingSampleWriter{}
	var stats videoPumpStats

	// Todos os 10 frames já estão na fila (rajada). Como nenhum está "adiantado"
	// em relação ao relógio de parede, todos devem sair quase imediatamente. Se o
	// pacer estivesse dormindo frameDur por frame (o bug de 1s), levaria ~166ms.
	start := time.Now()
	if err := writeVideoFrames(frames, writer, frameDur, frameDur/2, &stats); err != nil {
		t.Fatalf("write frames: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*frameDur {
		t.Fatalf("writeVideoFrames segurou rajada por %s (>%s); pacer está acumulando backlog", elapsed, 5*frameDur)
	}
	if got := len(writer.samples); got != 10 {
		t.Fatalf("samples = %d, want 10", got)
	}
	for i, sample := range writer.samples {
		want := byte(i + 1)
		if got := sample.Data[0]; got != want {
			t.Fatalf("sample %d data = %d, want %d", i, got, want)
		}
	}
}

type recordingSampleWriter struct {
	samples []media.Sample
}

func (w *recordingSampleWriter) WriteSample(sample media.Sample) error {
	w.samples = append(w.samples, sample)
	return nil
}

func testNAL(nalType byte, payload ...byte) []byte {
	nal := []byte{0x00, 0x00, 0x00, 0x01, nalType}
	return append(nal, payload...)
}
