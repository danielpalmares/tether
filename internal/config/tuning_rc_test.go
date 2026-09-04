package config

import "testing"

// Regressão de BANDA: em CBR o NVENC preenche a taxa alvo com NAL de filler
// data (padding descartado pelo decoder). Medido a 1080p60/24Mbps com a tela em
// repouso: 21.3 dos 23.9 Mbps eram filler — 89% da banda gasta sem imagem, o que
// no Wi-Fi da TV disputa espaço com o vídeo útil e o áudio. VBR com maxrate
// mantém o mesmo teto de qualidade e gasta só o necessário.
func TestTuningUsesVBRToAvoidFillerData(t *testing.T) {
	for _, res := range ResolutionPresets {
		cfg := StreamConfig{Width: res.Width, Height: res.Height, FPS: 60, Bitrate: 24000}
		if got := cfg.Tuning().RateControl; got != "vbr" {
			t.Fatalf("%dx%d RateControl = %q, want vbr", res.Width, res.Height, got)
		}
	}
}
