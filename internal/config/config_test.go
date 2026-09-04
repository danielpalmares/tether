package config

import (
	"testing"
	"time"
)

func TestNormalizeKeepsOnlySupportedResolutionAndForces60FPS(t *testing.T) {
	cfg := StreamConfig{Width: 1234, Height: 567, FPS: 120, Bitrate: 9000}.Normalize()
	if cfg.Width != 1920 || cfg.Height != 1080 {
		t.Fatalf("unsupported resolution should fall back to 1080p, got %dx%d", cfg.Width, cfg.Height)
	}
	if cfg.FPS != 60 {
		t.Fatalf("fps should be forced to 60, got %d", cfg.FPS)
	}
}

func TestNormalizeCodec(t *testing.T) {
	cases := []struct {
		name  string
		codec string
		want  string
	}{
		{name: "empty", codec: "", want: CodecH264NVENC},
		{name: "legacy h264", codec: "h264", want: CodecH264NVENC},
		{name: "nvenc", codec: CodecH264NVENC, want: CodecH264NVENC},
		{name: "x264", codec: CodecH264X264, want: CodecH264X264},
		{name: "libx264 alias", codec: "libx264", want: CodecH264X264},
		{name: "invalid", codec: "vp8", want: CodecH264NVENC},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := StreamConfig{Width: 1920, Height: 1080, FPS: 60, Bitrate: 8000, Codec: tc.codec}.Normalize()
			if cfg.Codec != tc.want {
				t.Fatalf("codec = %s, want %s", cfg.Codec, tc.want)
			}
		})
	}
}

func TestH264ProfileLevelIDByResolution(t *testing.T) {
	cases := []struct {
		name string
		cfg  StreamConfig
		want string
	}{
		{name: "full hd", cfg: StreamConfig{Width: 1920, Height: 1080, FPS: 60}, want: "42c02a"},
		{name: "2k", cfg: StreamConfig{Width: 2560, Height: 1440, FPS: 60}, want: "42c033"},
		{name: "4k", cfg: StreamConfig{Width: 3840, Height: 2160, FPS: 60}, want: "42c034"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.H264ProfileLevelID(); got != tc.want {
				t.Fatalf("profile-level-id = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestH264LowLatencyTuningByResolution(t *testing.T) {
	// GOP de 2s: o keyframe é o único ponto de entrada válido do stream, então o
	// GOP limita quanto tempo a TV fica em tela preta ao conectar ou ao se
	// recuperar (perda, ou captura reiniciada por troca de modo de display). Com
	// VBR o IDR deixou de ser um pico caro, então encurtar custa pouca banda.
	// VBV segue dimensionado em milissegundos de bitrate.
	fullHD := StreamConfig{Width: 1920, Height: 1080, FPS: 60, Bitrate: 8000}
	if got := fullHD.H264GOPFrames(); got != 120 {
		t.Fatalf("1080p GOP = %d, want 120", got)
	}
	if got := fullHD.H264VBVBufferKbps(); got != 240 { // 8000 * 30 / 1000
		t.Fatalf("1080p VBV = %d, want 240", got)
	}

	fourK := StreamConfig{Width: 3840, Height: 2160, FPS: 60, Bitrate: 42000}
	if got := fourK.H264GOPFrames(); got != 120 {
		t.Fatalf("4K GOP = %d, want 120", got)
	}
	if got := fourK.H264VBVBufferKbps(); got != 3150 { // 42000 * 75 / 1000
		t.Fatalf("4K VBV = %d, want 3150", got)
	}

	twoK := StreamConfig{Width: 2560, Height: 1440, FPS: 60, Bitrate: 24000}
	if got := twoK.H264VBVBufferKbps(); got != 1320 { // 24000 * 55 / 1000
		t.Fatalf("2K VBV = %d, want 1320", got)
	}
}

func TestTuningPacerHoldScalesWithResolution(t *testing.T) {
	frameDur := time.Second / 60
	cases := []struct {
		name string
		cfg  StreamConfig
		want time.Duration
	}{
		{name: "1080p", cfg: StreamConfig{Width: 1920, Height: 1080, FPS: 60, Bitrate: 8000}, want: frameDur * 50 / 100},
		{name: "1440p", cfg: StreamConfig{Width: 2560, Height: 1440, FPS: 60, Bitrate: 24000}, want: frameDur * 75 / 100},
		{name: "4k", cfg: StreamConfig{Width: 3840, Height: 2160, FPS: 60, Bitrate: 42000}, want: frameDur * 100 / 100},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Tuning().PacerMaxHold; got != tc.want {
				t.Fatalf("hold = %s, want %s", got, tc.want)
			}
		})
	}
}

func TestTuningFrameQueueScalesWithFPS(t *testing.T) {
	// Fila curta (~40ms no perfil padrão) derivada do fps; pacer hold ≈ meio
	// frame. Cada frame na fila é ~16.7ms de latência na tela, então a fila
	// absorve jitter sem virar acumulador de atraso.
	t60 := StreamConfig{Width: 1920, Height: 1080, FPS: 60, Bitrate: 8000}.Tuning()
	if t60.FrameQueueDepth != 2 { // 40 * 60 / 1000 = 2
		t.Fatalf("60fps queue = %d, want 2", t60.FrameQueueDepth)
	}
	wantHold := (time.Second / 60) / 2 // ~8.3ms
	if t60.PacerMaxHold != wantHold {
		t.Fatalf("pacer hold = %s, want %s", t60.PacerMaxHold, wantHold)
	}
	if !t60.TemporalAQ {
		t.Fatalf("temporal-aq deve estar ligado")
	}
}

func TestX264TuningUsesShorterLatencyBudget(t *testing.T) {
	frameDur := time.Second / 60
	tune := StreamConfig{
		Width:   1920,
		Height:  1080,
		FPS:     60,
		Bitrate: 8000,
		Codec:   CodecH264X264,
	}.Tuning()

	if tune.VBVBufferKb != 160 { // 8000 * 20 / 1000
		t.Fatalf("x264 VBV = %d, want 160", tune.VBVBufferKb)
	}
	if tune.FrameQueueDepth != 2 { // 35 * 60 / 1000 = 2
		t.Fatalf("x264 queue = %d, want 2", tune.FrameQueueDepth)
	}
	if tune.PacerMaxHold != frameDur/4 {
		t.Fatalf("x264 pacer hold = %s, want %s", tune.PacerMaxHold, frameDur/4)
	}
}

// Surfaces são buffers de trabalho do NVENC (não fila de reordenação: seguimos
// com -bf 0 / -delay 0). Medido a 1080p60: 3 surfaces => 13% dos frames fora da
// cadência de 16.7ms; 8 surfaces => 10%. Mais folga evita que o encoder espere
// por um buffer livre para iniciar o frame seguinte.
func TestTuningSurfacesScaleWithLoad(t *testing.T) {
	cases := []struct {
		name string
		cfg  StreamConfig
		want int
	}{
		{name: "1080p", cfg: StreamConfig{Width: 1920, Height: 1080, FPS: 60, Bitrate: 8000}, want: 8},
		{name: "1440p", cfg: StreamConfig{Width: 2560, Height: 1440, FPS: 60, Bitrate: 16000}, want: 10},
		{name: "4k", cfg: StreamConfig{Width: 3840, Height: 2160, FPS: 60, Bitrate: 42000}, want: 12},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Tuning().Surfaces; got != tc.want {
				t.Fatalf("surfaces = %d, want %d", got, tc.want)
			}
		})
	}
}
