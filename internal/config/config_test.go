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
	// GOP longo (3s) amortiza a frequência dos picos de IDR; VBV dimensionado em
	// milissegundos de bitrate dá folga para o rate control espalhar o IDR.
	fullHD := StreamConfig{Width: 1920, Height: 1080, FPS: 60, Bitrate: 8000}
	if got := fullHD.H264GOPFrames(); got != 180 {
		t.Fatalf("1080p GOP = %d, want 180", got)
	}
	if got := fullHD.H264VBVBufferKbps(); got != 240 { // 8000 * 30 / 1000
		t.Fatalf("1080p VBV = %d, want 240", got)
	}

	fourK := StreamConfig{Width: 3840, Height: 2160, FPS: 60, Bitrate: 42000}
	if got := fourK.H264GOPFrames(); got != 180 {
		t.Fatalf("4K GOP = %d, want 180", got)
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
	// Fila curta (~65ms) derivada do fps; pacer hold ≈ meio frame. Regra única
	// para toda resolução.
	t60 := StreamConfig{Width: 1920, Height: 1080, FPS: 60, Bitrate: 8000}.Tuning()
	if t60.FrameQueueDepth != 3 { // 65 * 60 / 1000 = 3
		t.Fatalf("60fps queue = %d, want 3", t60.FrameQueueDepth)
	}
	wantHold := (time.Second / 60) / 2 // ~8.3ms
	if t60.PacerMaxHold != wantHold {
		t.Fatalf("pacer hold = %s, want %s", t60.PacerMaxHold, wantHold)
	}
	if !t60.TemporalAQ {
		t.Fatalf("temporal-aq deve estar ligado")
	}
}

func TestTuningSurfacesScaleWithLoad(t *testing.T) {
	cases := []struct {
		name string
		cfg  StreamConfig
		want int
	}{
		{name: "1080p", cfg: StreamConfig{Width: 1920, Height: 1080, FPS: 60, Bitrate: 8000}, want: 3},
		{name: "1440p", cfg: StreamConfig{Width: 2560, Height: 1440, FPS: 60, Bitrate: 16000}, want: 4},
		{name: "4k", cfg: StreamConfig{Width: 3840, Height: 2160, FPS: 60, Bitrate: 42000}, want: 6},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.cfg.Tuning().Surfaces; got != tc.want {
				t.Fatalf("surfaces = %d, want %d", got, tc.want)
			}
		})
	}
}
