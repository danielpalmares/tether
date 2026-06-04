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
// Bitrate/FPS conservadores para Wi-Fi: 8Mbps @ 30fps. Em Wi-Fi, 20Mbps satura
// e frames grandes (keyframes 1080p) são fragmentados em dezenas de pacotes
// RTP; perder um fragmento descarta o frame inteiro, e decoders rígidos de TV
// (Samsung Tizen) congelam na imagem. 8Mbps deixa folga para a rede e produz
// frames menores, mais resilientes à perda. 30fps reduz pela metade a pressão
// de pacotes/seg sem prejuízo perceptível para jogos casuais na TV.
func Default() StreamConfig {
	return StreamConfig{
		Width:   1920,
		Height:  1080,
		FPS:     30,
		Bitrate: 8000,
		Codec:   "h264",
		Display: 0,
	}
}
