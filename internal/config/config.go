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
// 1080p60 @ 12Mbps. A 60fps cada frame chega 2x mais rápido, então o atraso
// real de playout (medido em nº de frames bufferizados pelo player Tizen) cai
// pela metade em tempo absoluto vs. 30fps — alavanca direta sobre o delay
// percebido sem perder resolução. O bitrate sobe de 8 para 12Mbps porque a
// 60fps há o dobro de frames dividindo o orçamento de bits; 12Mbps mantém a
// qualidade por frame e ainda deixa folga no Wi-Fi (longe dos ~20Mbps que
// saturam e fragmentam keyframes em excesso). GOP curto (em capture.go) mantém
// os keyframes pequenos o bastante para resistir à perda de pacote.
func Default() StreamConfig {
	return StreamConfig{
		Width:   1920,
		Height:  1080,
		FPS:     60,
		Bitrate: 12000,
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
