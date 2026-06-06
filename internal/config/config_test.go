package config

import "testing"

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
