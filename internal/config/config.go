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

// Normalize preenche campos inválidos com o perfil padrão 1080p60.
func (c StreamConfig) Normalize() StreamConfig {
	d := Default()
	if c.Width <= 0 {
		c.Width = d.Width
	}
	if c.Height <= 0 {
		c.Height = d.Height
	}
	if c.FPS <= 0 {
		c.FPS = d.FPS
	}
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
