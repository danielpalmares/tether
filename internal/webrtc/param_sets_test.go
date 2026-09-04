package webrtc

import (
	"bytes"
	"testing"
	"time"

	"github.com/pion/webrtc/v4/pkg/media"
)

// boundTrack simula uma track cujo Bind ainda não aconteceu — o cenário do
// pré-aquecimento da captura, em que o FFmpeg já produz frames mas o RTP ainda
// não tem para onde ir.
type boundTrack struct {
	recordingSampleWriter
	bound bool
}

func (t *boundTrack) Bound() bool { return t.bound }

// Regressão da TELA PRETA: enquanto a track não está ligada, o WriteSample é
// descartado pelo Pion. Se o writer emitisse mesmo assim, o IDR inicial — o
// único que carrega SPS/PPS — seria perdido e a TV ficaria sem parâmetros de
// decode. O writer não pode emitir nada antes de a track estar ligada.
func TestWriteVideoFramesWaitsForBoundTrack(t *testing.T) {
	frameDur := time.Second / 60

	frames := make(chan encodedFrame, 2)
	frames <- encodedFrame{data: keyframeAU(), duration: frameDur, keyframe: true}
	close(frames)

	track := &boundTrack{bound: false}
	if err := writeVideoFrames(frames, track, frameDur, frameDur/2, nil, nil); err != nil {
		t.Fatalf("write frames: %v", err)
	}
	if got := len(track.samples); got != 0 {
		t.Fatalf("samples = %d, want 0 (track não ligada)", got)
	}
}

// O stream só pode COMEÇAR num keyframe. Entrar num P-frame deixa o decoder da
// TV sem referência: imagem quebrada ou preta até o próximo IDR.
func TestWriteVideoFramesStartsOnlyOnKeyframe(t *testing.T) {
	frameDur := time.Second / 60

	frames := make(chan encodedFrame, 4)
	frames <- encodedFrame{data: []byte{1}, duration: frameDur}                    // P: descartado
	frames <- encodedFrame{data: []byte{2}, duration: frameDur}                    // P: descartado
	frames <- encodedFrame{data: keyframeAU(), duration: frameDur, keyframe: true} // entra aqui
	frames <- encodedFrame{data: []byte{4}, duration: frameDur}                    // segue
	close(frames)

	track := &boundTrack{bound: true}
	if err := writeVideoFrames(frames, track, frameDur, frameDur/2, nil, nil); err != nil {
		t.Fatalf("write frames: %v", err)
	}
	if got := len(track.samples); got != 2 {
		t.Fatalf("samples = %d, want 2 (keyframe + seguinte)", got)
	}
}

// Um PLI/FIR do receptor deve provocar o reenvio IMEDIATO de SPS/PPS. Sem isso,
// a TV que perdeu os parâmetros espera o próximo IDR do GOP (até 3s de tela
// preta) para se recuperar.
func TestWriteVideoFramesResendsParamSetsOnPLI(t *testing.T) {
	frameDur := time.Second / 60

	frames := make(chan encodedFrame, 3)
	frames <- encodedFrame{data: keyframeAU(), duration: frameDur, keyframe: true}
	frames <- encodedFrame{data: nonIDRAU(), duration: frameDur}
	close(frames)

	// O PLI real chega DEPOIS do keyframe inicial (a TV perdeu os parâmetros no
	// meio do stream). O keyframe já carrega SPS/PPS e satisfaz qualquer pedido
	// pendente; o pedido que importa é o que surge a partir dali.
	track := &boundTrack{bound: true}
	sent := 0
	resend := func() bool {
		sent++
		return sent > 1 // só a partir do segundo frame há PLI pendente
	}

	if err := writeVideoFrames(frames, track, frameDur, frameDur/2, nil, resend); err != nil {
		t.Fatalf("write frames: %v", err)
	}
	if got := len(track.samples); got != 2 {
		t.Fatalf("samples = %d, want 2", got)
	}
	// O segundo frame (P-frame) deve ter recebido SPS/PPS prefixados por causa do PLI.
	if !hasParameterSets(track.samples[1].Data) {
		t.Fatal("PLI não provocou reenvio de SPS/PPS no frame seguinte")
	}
}

func TestExtractAndDetectParameterSets(t *testing.T) {
	au := keyframeAU()
	if !hasParameterSets(au) {
		t.Fatal("keyframe deveria conter SPS")
	}
	ps := extractParameterSets(au)
	if !bytes.Contains(ps, []byte{nalTypeSPS}) {
		t.Fatalf("SPS ausente nos parameter sets extraídos: %v", ps)
	}
	if hasParameterSets(nonIDRAU()) {
		t.Fatal("P-frame não deveria conter SPS")
	}
}

func keyframeAU() []byte {
	return bytes.Join([][]byte{
		testNAL(nalTypeAUD, 0xf0),
		testNAL(nalTypeSPS, 0x42, 0xc0, 0x2a),
		testNAL(nalTypePPS, 0xce, 0x06),
		testNAL(nalTypeIDR, 0x88, 0x84),
	}, nil)
}

func nonIDRAU() []byte {
	return bytes.Join([][]byte{
		testNAL(nalTypeAUD, 0xf0),
		testNAL(nalTypeNonIDR, 0x9a, 0x22),
	}, nil)
}

var _ sampleWriter = (*boundTrack)(nil)
var _ media.Sample = media.Sample{}
