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

func TestReadH264AccessUnitsKeepsMultipleSlicesInOneAUDFrame(t *testing.T) {
	stream := bytes.NewReader(bytes.Join([][]byte{
		testNAL(nalTypeAUD, 0xf0),
		testNAL(nalTypeSPS, 0x42, 0xc0, 0x2a),
		testNAL(nalTypePPS, 0xce, 0x06),
		testNAL(nalTypeIDR, 0x88, 0x84),
		testNAL(nalTypeIDR, 0x12, 0x34),
		testNAL(nalTypeAUD, 0xf0),
		testNAL(nalTypeNonIDR, 0x9a, 0x22),
		testNAL(nalTypeNonIDR, 0x56, 0x78),
	}, nil))

	out := make(chan encodedFrame, 4)
	frameDur := time.Second / 60

	if err := readH264AccessUnits(stream, frameDur, out, nil); err != nil {
		t.Fatalf("read access units: %v", err)
	}

	if got := len(out); got != 2 {
		t.Fatalf("frames = %d, want 2", got)
	}

	first := <-out
	if !first.keyframe {
		t.Fatalf("first frame should be keyframe")
	}
	if got := bytes.Count(first.data, []byte(annexBStartCode)); got != 5 {
		t.Fatalf("first frame NAL count = %d, want 5", got)
	}

	second := <-out
	if second.keyframe {
		t.Fatalf("second frame should not be keyframe")
	}
	if got := bytes.Count(second.data, []byte(annexBStartCode)); got != 3 {
		t.Fatalf("second frame NAL count = %d, want 3", got)
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
		// O primeiro frame precisa ser keyframe: o writer só começa a emitir a
		// partir de um ponto de entrada válido para o decoder.
		frames <- encodedFrame{data: []byte{byte(i)}, duration: frameDur, keyframe: i == 1}
	}
	close(frames)

	writer := &recordingSampleWriter{}
	var stats videoPumpStats

	// Todos os 10 frames já estão na fila (rajada). Como nenhum está "adiantado"
	// em relação ao relógio de parede, todos devem sair quase imediatamente. Se o
	// pacer estivesse dormindo frameDur por frame (o bug de 1s), levaria ~166ms.
	start := time.Now()
	if err := writeVideoFrames(frames, writer, frameDur, frameDur/2, &stats, nil); err != nil {
		t.Fatalf("write frames: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 5*frameDur {
		t.Fatalf("writeVideoFrames segurou rajada por %s (>%s); pacer está acumulando backlog", elapsed, 5*frameDur)
	}
	// Sob rajada, P-frames obsoletos são DESCARTADOS em vez de enviados
	// atrasados (ver staleFrameBacklog): o que sai é sempre o presente. O teste
	// garante que algo saiu, que nada ficou preso e que a ordem foi preservada.
	if len(writer.samples) == 0 {
		t.Fatal("nenhum sample enviado")
	}
	if got := len(writer.samples); got > 10 {
		t.Fatalf("samples = %d, não pode exceder os 10 enfileirados", got)
	}
	var last byte
	for i, sample := range writer.samples {
		if got := sample.Data[0]; got <= last && i > 0 {
			t.Fatalf("sample %d fora de ordem: %d após %d", i, got, last)
		} else {
			last = sample.Data[0]
		}
	}
	// O último frame da rajada é o mais recente e NUNCA pode ser descartado:
	// descartá-lo deixaria a imagem congelada num frame velho.
	if got := writer.samples[len(writer.samples)-1].Data[0]; got != 10 {
		t.Fatalf("último sample = %d, want 10 (o frame mais recente tem de chegar)", got)
	}
}

// Um keyframe NUNCA pode ser descartado por backlog: sem ele o decoder perde a
// referência e a imagem quebra até o próximo IDR.
func TestWriteVideoFramesNeverDropsKeyframes(t *testing.T) {
	frameDur := time.Second / 60

	frames := make(chan encodedFrame, 12)
	frames <- encodedFrame{data: []byte{1}, duration: frameDur, keyframe: true}
	for i := 2; i <= 11; i++ {
		frames <- encodedFrame{data: []byte{byte(i)}, duration: frameDur, keyframe: i%5 == 0}
	}
	close(frames)

	writer := &recordingSampleWriter{}
	if err := writeVideoFrames(frames, writer, frameDur, frameDur/2, nil, nil); err != nil {
		t.Fatalf("write frames: %v", err)
	}

	got := map[byte]bool{}
	for _, s := range writer.samples {
		got[s.Data[0]] = true
	}
	for _, kf := range []byte{1, 5, 10} {
		if !got[kf] {
			t.Fatalf("keyframe %d foi descartado", kf)
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
