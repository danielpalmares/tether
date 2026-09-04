package webrtc

import "testing"

// Regressão do ENGASGO no perfil "suave": o limiar de descarte era a constante
// 3, enquanto a fila do perfil smooth é 6. Resultado: descarte contínuo em
// operação normal (medido: 123 frames jogados fora em ~2min, sempre com
// fila=5 ou 6) justamente no perfil que existe para SUAVIZAR.
//
// O limiar tem de ser relativo à capacidade da fila.
func TestStaleBacklogScalesWithQueue(t *testing.T) {
	cases := []struct {
		name     string
		queueCap int
	}{
		{"ultra", 2},
		{"balanced", 2},
		{"smooth", 3},
	}
	for _, c := range cases {
		got := staleBacklogFor(c.queueCap)
		t.Logf("%-9s fila=%d -> limiar=%d", c.name, c.queueCap, got)

		// O limiar nunca pode ficar abaixo do piso, senão descarta a cada frame
		// em operação normal (foi o bug que produziu 123 frames perdidos).
		if got < staleBacklogFloor {
			t.Errorf("%s: limiar %d abaixo do piso %d", c.name, got, staleBacklogFloor)
		}
	}

	// Com filas grandes o limiar tem de crescer junto, mas SEM permitir que a
	// fila inteira vire espaço operacional: fila cheia é latência acumulada
	// (medido: 6 frames = ~100ms fixos de atraso na tela).
	if got := staleBacklogFor(12); got >= 12 {
		t.Fatalf("fila de 12 com limiar %d deixa a fila encher: vira lag acumulado", got)
	}
}
