package gpu

import "testing"

func TestQueryRealGPU(t *testing.T) {
	s := Query()
	t.Logf("available=%v name=%q gpu=%d%% enc=%d%% throttle=%q sat=%v hot=%v advice=%q",
		s.Available, s.Name, s.GPUUtil, s.EncUtil, s.Throttle, s.Saturated, s.EncoderHot, s.Advice)
	if s.Available && s.Name == "" {
		t.Fatal("GPU disponível mas sem nome")
	}
}

// O conselho precisa distinguir gargalo de encoder (stream) de gargalo de
// núcleo (jogo) — mexer no lugar errado não resolve nada.
func TestAdviceDistinguishesBottleneck(t *testing.T) {
	if a := advise(Status{EncoderHot: true}); a == "" {
		t.Fatal("encoder saturado deveria gerar conselho")
	}
	if a := advise(Status{Saturated: true}); a == "" {
		t.Fatal("GPU saturada deveria gerar conselho")
	}
	enc := advise(Status{EncoderHot: true})
	gpu := advise(Status{Saturated: true})
	if enc == gpu {
		t.Fatal("conselho de encoder e de núcleo têm de ser diferentes")
	}
	if a := advise(Status{GPUUtil: 40}); a != "" {
		t.Fatalf("GPU folgada não deveria gerar conselho, veio %q", a)
	}
}
