package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

const (
	CodecH264NVENC = "h264_nvenc"
	CodecH264X264  = "h264_x264"
	CodecH264AMF   = "h264_amf" // AMD
	CodecH264QSV   = "h264_qsv" // Intel QuickSync

	// CodecAuto pede a detecção automática (ver DetectCodec). É o valor padrão:
	// a escolha do encoder saiu do painel porque não significa nada para o
	// usuário final e escolher errado quebra o stream.
	CodecAuto = "auto"
)

// StreamConfig guarda as configurações de streaming definidas no painel do host.
type StreamConfig struct {
	Width   int    `json:"width"`
	Height  int    `json:"height"`
	FPS     int    `json:"fps"`
	Bitrate int    `json:"bitrate"` // kbps
	Codec   string `json:"codec"`   // detectado automaticamente; ver DetectCodec
	Display int    `json:"display"` // índice do monitor (0 = primário)

	// Latency é o compromisso entre resposta e suavidade. Ver latency.go.
	Latency LatencyProfile `json:"latency"`
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
		Bitrate: 12000, // recomendado do preset 1080p; ver presets.go
		Codec:   CodecAuto,
		Display: 0,
		Latency: LatencyBalanced,
	}
}

type ResolutionPreset struct {
	Width  int
	Height int
}

var ResolutionPresets = []ResolutionPreset{
	{Width: 1280, Height: 720},
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

// H264GOPFrames e H264VBVBufferKbps delegam ao Tuning() — fonte única de verdade
// para a derivação adaptativa. Mantidos como wrappers por compatibilidade dos
// chamadores e logs existentes.
func (c StreamConfig) H264GOPFrames() int {
	return c.Tuning().GOPFrames
}

func (c StreamConfig) H264VBVBufferKbps() int {
	return c.Tuning().VBVBufferKb
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
	switch strings.ToLower(strings.TrimSpace(c.Codec)) {
	case "", "h264", CodecAuto:
		// Detecção automática: o painel não expõe mais essa escolha.
		c.Codec = DetectCodec()
	case CodecH264X264, "libx264", "x264":
		c.Codec = CodecH264X264
	case CodecH264NVENC:
		c.Codec = CodecH264NVENC
	case CodecH264AMF:
		c.Codec = CodecH264AMF
	case CodecH264QSV:
		c.Codec = CodecH264QSV
	default:
		c.Codec = DetectCodec()
	}
	if c.Display < 0 {
		c.Display = d.Display
	}
	c.Latency = NormalizeLatency(c.Latency)
	return c
}

// Path retorna o caminho do arquivo de persistência da config.
//
// Prioriza %APPDATA%\tether\config.json (Windows) / ~/.config/tether/config.json;
// se o diretório do usuário for inacessível, usa config.json ao lado do executável.
func Path() string {
	if dir, err := os.UserConfigDir(); err == nil && dir != "" {
		return filepath.Join(dir, "tether", "config.json")
	}
	if exe, err := os.Executable(); err == nil {
		return filepath.Join(filepath.Dir(exe), "config.json")
	}
	return "config.json"
}

// Load lê a config do disco. Se o arquivo não existir ou for inválido, retorna
// o Default() normalizado — nunca falha o boot do host por config corrompida.
func Load(path string) (StreamConfig, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Default(), err
	}
	var c StreamConfig
	if err := json.Unmarshal(raw, &c); err != nil {
		return Default(), err
	}
	return c.Normalize(), nil
}

// Save grava a config no disco (cria o diretório se necessário), de forma
// atômica via arquivo temporário + rename para não corromper em caso de crash.
func Save(path string, c StreamConfig) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(c.Normalize(), "", "  ")
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
