package webrtc

import (
	"testing"
	"time"
)

// 1080p: frames pequenos não devem sofrer pacing (custo sem benefício).
func TestPacketPacingSkippedForSmallFrames(t *testing.T) {
	frameDur := time.Second / 60
	_, pause := packetPacingPlan(frameDur, 30)
	if pause != 0 {
		t.Fatalf("frame de 30 pacotes (1080p típico) recebeu pausa de %s, esperado 0", pause)
	}
}

// Regressão do LAG: o frame REAL de 1080p medido em sessão de jogo tem ~63KB,
// ou seja ~48 pacotes. Com o limiar antigo (40) o pacing rodava em todo frame e
// consumia 252ms de cada segundo dormindo — sem nenhuma rajada para conter.
// O pacing só se justifica onde a rajada é de fato grande (4K).
func TestPacketPacingSkippedForTypical1080pFrames(t *testing.T) {
	frameDur := time.Second / 60
	const fps = 60

	// 63KB / 1350B por pacote ≈ 48 pacotes (medido em sessão real).
	burst, pause := packetPacingPlan(frameDur, 48)
	if pause != 0 {
		sleeps := 48 / burst
		perSecond := pause * time.Duration(sleeps) * fps
		t.Fatalf("frame 1080p típico (48 pacotes) recebeu pacing: %s por lote, "+
			"%s de cada segundo dormindo", pause, perSecond)
	}

	// Um frame grande de 1080p em cena complexa (100KB ≈ 76 pacotes) também não
	// deve pagar pacing: continua longe da rajada de um frame 4K.
	if _, pause := packetPacingPlan(frameDur, 76); pause != 0 {
		t.Fatalf("frame 1080p grande (76 pacotes) recebeu pacing de %s", pause)
	}
}

// O custo do pacing, quando ativo, tem de caber no orçamento de um segundo.
func TestPacketPacingCostPerSecondIsBounded(t *testing.T) {
	frameDur := time.Second / 60
	const fps = 60
	packets := 200 // frame 4K

	burst, pause := packetPacingPlan(frameDur, packets)
	if pause == 0 {
		t.Fatal("frame 4K deveria receber pacing")
	}
	sleeps := packets / burst
	perSecond := pause * time.Duration(sleeps) * fps

	// Acima de ~30% do segundo o pacing deixa de ser suavização e vira gargalo.
	max := 300 * time.Millisecond
	if perSecond > max {
		t.Fatalf("pacing consumiria %s por segundo (teto %s)", perSecond, max)
	}
	t.Logf("4K: %d sleeps/frame × %s × %dfps = %s por segundo", sleeps, pause, fps, perSecond)
}

// 4K: frames grandes viram rajada e PRECISAM ser espalhados — mas em poucos
// lotes, porque o sleep do Windows tem granularidade de ~1-15ms.
func TestPacketPacingUsesFewSleepsForLargeFrames(t *testing.T) {
	frameDur := time.Second / 60 // 16.6ms
	packets := 200               // frame 4K de ~265KB

	burst, pause := packetPacingPlan(frameDur, packets)
	if pause == 0 {
		t.Fatal("frame de 200 pacotes (4K) não recebeu pacing")
	}
	if pause < time.Millisecond {
		t.Fatalf("pausa de %s é menor que a granularidade real do timer do SO", pause)
	}

	sleeps := packets / burst
	if sleeps > 8 {
		t.Fatalf("%d sleeps por frame: muitos. Sleeps curtos e numerosos estouram "+
			"o orçamento do frame no Windows (medido: writeMax de 126ms)", sleeps)
	}

	// O custo total precisa caber com folga na duração do frame, senão o pacing
	// vira o gargalo e derruba o fps.
	total := pause * time.Duration(sleeps)
	if total > frameDur/2 {
		t.Fatalf("janela de pacing = %s, mais que metade do frame (%s)", total, frameDur)
	}
	t.Logf("200 pacotes: lotes de %d, pausa %s, %d sleeps, total %s (frame %s)",
		burst, pause, sleeps, total, frameDur)
}

func TestPacketPacingHandlesZeroDuration(t *testing.T) {
	if _, pause := packetPacingPlan(0, 500); pause != 0 {
		t.Fatalf("duração zero deveria desativar pacing, veio %s", pause)
	}
}
