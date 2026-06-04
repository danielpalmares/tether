package webrtc

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/pion/webrtc/v4"

	"tether/internal/config"
	"tether/internal/input"
)

// TestAnswerAdvertisesNackPLI garante que a answer SDP gerada pelo host anuncia
// os feedbacks nack e nack pli no codec de vídeo. Sem eles, a TV não negocia
// retransmissão e a perda de pacote vira frame descartado (causa do delay/buf
// inflado). Este teste exercita o caminho real de NewSession + HandleOffer.
func TestAnswerAdvertisesNackPLI(t *testing.T) {
	disableCaptureForTest(t)

	// Client sintético: gera uma offer recvonly de vídeo, como o client.html faz.
	clientPC, err := webrtc.NewPeerConnection(webrtc.Configuration{})
	if err != nil {
		t.Fatalf("client pc: %v", err)
	}
	defer clientPC.Close()

	if _, err := clientPC.AddTransceiverFromKind(
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverInit{Direction: webrtc.RTPTransceiverDirectionRecvonly},
	); err != nil {
		t.Fatalf("add transceiver: %v", err)
	}

	offer, err := clientPC.CreateOffer(nil)
	if err != nil {
		t.Fatalf("create offer: %v", err)
	}
	if err := clientPC.SetLocalDescription(offer); err != nil {
		t.Fatalf("set local: %v", err)
	}

	// Host real: mesma construção de produção (NACK + RTCP reports + track).
	// Com CGO_ENABLED=0 o NewInjector devolve o noop (não toca ViGEm).
	injector, err := input.NewInjector()
	if err != nil {
		t.Fatalf("injector: %v", err)
	}
	sess, err := NewSession(config.Default(), injector, func() {})
	if err != nil {
		t.Fatalf("new session: %v", err)
	}
	defer sess.Close()

	answer, err := sess.HandleOffer(offer)
	if err != nil {
		t.Fatalf("handle offer: %v", err)
	}

	sdp := answer.SDP
	t.Logf("=== ANSWER DO HOST ===\n%s", sdp)

	// 1) NACK/PLI negociados (retransmissão de pacote perdido).
	if !strings.Contains(sdp, "nack pli") {
		t.Errorf("answer SDP não anuncia 'nack pli'")
	}

	// 2) EXATAMENTE UM codec H264, e com o profile-level-id 42c02a. Esta é a
	// regressão crítica: sem SetCodecPreferences o Pion ecoa todos os perfis H264
	// da offer (inclusive 42001f = Level 3.1/720p na posição preferencial), e a
	// TV casa o profile errado, estourando o decoder no bitstream 1080p60.
	h264Count := strings.Count(sdp, "H264/90000")
	if h264Count != 1 {
		t.Errorf("answer deve ter EXATAMENTE 1 codec H264, tem %d (SetCodecPreferences não restringiu)", h264Count)
	}
	if !strings.Contains(sdp, "profile-level-id=42c02a") {
		t.Errorf("answer não anuncia o profile-level-id=42c02a (Level 4.2/1080p)")
	}
	for _, bad := range []string{"42001f", "42e01f", "4d001f", "64001f"} {
		if strings.Contains(sdp, bad) {
			t.Errorf("answer contém profile indesejado %s (eco da offer; deveria ser só 42c02a)", bad)
		}
	}
}

func disableCaptureForTest(t *testing.T) {
	t.Helper()
	old := newCapturer
	newCapturer = func(config.StreamConfig) streamCapturer {
		return noopCapturer{}
	}
	t.Cleanup(func() {
		newCapturer = old
	})
}

type noopCapturer struct{}

func (noopCapturer) Start(context.Context) (io.ReadCloser, error) {
	return io.NopCloser(strings.NewReader("")), nil
}

func (noopCapturer) Stop() {}
