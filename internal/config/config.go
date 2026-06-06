package config

// StreamConfig guarda as configurações de streaming definidas no painel do host.
type StreamConfig struct {
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	FPS     int    `json:"fps"`
	Bitrate int    `json:"bitrate"` // kbps
	Codec   string `json:"codec"`   // "h264"
	Display int    `json:"display"` // índice do monitor (0 = primário)
}

// Default retorna uma config sensata para a LAN.
//
// Perfil padrão para TV browser: 1080p60 @ 8Mbps.
//
// Mantém Full HD para TV 4K, mas reduz o bitrate em relação a 12Mbps para aliviar
// o decoder/browser da TV e diminuir bursts de pacotes em cenas complexas. O
// ganho principal para input lag continua sendo preservar 60 frames/s.
func Default() StreamConfig {
	return StreamConfig{
		Width:   1920,
		Height:  1080,
		FPS:     60,
		Bitrate: 8000,
		Codec:   "h264",
		Display: 0,
	}
}

type ResolutionPreset struct {
	Width  int
	Height int
}

var ResolutionPresets = []ResolutionPreset{
	{Width: 1920, Height: 1080},
	{Width: 2560, Height: 1440},
	{Width: 3840, Height: 2160},
}

func (c StreamConfig) H264Level() string {
	c = c.Normalize()
	switch {
	case c.Width >= 3840 || c.Height >= 2160:
		return "5.2"
	case c.Width >= 2560 || c.Height >= 1440:
		return "5.1"
	default:
		return "4.2"
	}
}

func (c StreamConfig) H264ProfileLevelID() string {
	switch c.H264Level() {
	case "5.2":
		return "42c034"
	case "5.1":
		return "42c033"
	default:
		return "42c02a"
	}
}

func (c StreamConfig) Is4K() bool {
	c = c.Normalize()
	return c.Width >= 3840 || c.Height >= 2160
}

func (c StreamConfig) H264GOPFrames() int {
	c = c.Normalize()
	gop := c.FPS
	if gop <= 0 {
		gop = 60
	}
	if c.Is4K() {
		return gop * 2
	}
	return gop
}

func (c StreamConfig) H264VBVBufferKbps() int {
	c = c.Normalize()
	divisor := 8
	if c.Is4K() {
		divisor = 16
	}
	buf := c.Bitrate / divisor
	if buf <= 0 {
		return c.Bitrate
	}
	return buf
}

// Normalize preenche campos inválidos com o perfil padrão 1080p60.
func (c StreamConfig) Normalize() StreamConfig {
	d := Default()
	validResolution := false
	for _, preset := range ResolutionPresets {
		if c.Width == preset.Width && c.Height == preset.Height {
			validResolution = true
			break
		}
	}
	if !validResolution {
		c.Width = d.Width
		c.Height = d.Height
	}
	c.FPS = 60
	if c.Bitrate <= 0 {
		c.Bitrate = d.Bitrate
	}
	if c.Codec == "" {
		c.Codec = d.Codec
	}
	if c.Display < 0 {
		c.Display = d.Display
	}
	return c
}
