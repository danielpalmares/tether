package webrtc

import (
	"testing"
	"time"
)

// O pacing NÃO pode custar mais que uma fração do frame. A implementação
// anterior dormia por PACOTE e o sleep do Windows (~1-15ms de granularidade)
// transformava uma janela alvo de 4ms em >100ms reais, derrubando o fps para 21.
// Este teste mede o custo de parede do plano de pacing para um frame 4K.
func TestPacketPacingRealCostFitsFrame(t *testing.T) {
	frameDur := time.Second / 60
	packets := 200

	burst, pause := packetPacingPlan(frameDur, packets)
	if pause == 0 {
		t.Skip("pacing desativado para este tamanho")
	}

	start := time.Now()
	for i := 0; i < packets; i++ {
		if i > 0 && i%burst == 0 {
			time.Sleep(pause)
		}
	}
	elapsed := time.Since(start)

	t.Logf("custo real do pacing: %s (frame = %s, lotes de %d, pausa %s)",
		elapsed, frameDur, burst, pause)

	if elapsed > frameDur {
		t.Fatalf("pacing custou %s, MAIS que a duração do frame (%s): o pipeline "+
			"não consegue manter 60fps assim", elapsed, frameDur)
	}
}
