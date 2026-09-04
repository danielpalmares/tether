package capture

import "testing"

// Integração: sonda os monitores reais desta máquina. Não afirma quantidade
// (varia por máquina), mas garante que a detecção devolve algo coerente e que
// nenhum índice listado é dos que o ddagrab recusa.
func TestDisplaysProbeReal(t *testing.T) {
	ds := Displays()
	if len(ds) == 0 {
		t.Fatal("Displays() nunca pode devolver lista vazia")
	}
	for _, d := range ds {
		t.Logf("idx=%d label=%q %dx%d disponivel=%v", d.Index, d.Label, d.Width, d.Height, d.Available)
		if !d.Available {
			t.Errorf("display %d listado como indisponível; a lista deve conter só capturáveis", d.Index)
		}
	}
}
