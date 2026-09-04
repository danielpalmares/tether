package config

// Preset é uma combinação pronta de resolução + bitrate recomendado.
//
// O bitrate recomendado NÃO é o máximo que o encoder aguenta — é o que passa
// confortavelmente por Wi-Fi doméstico sem virar rajada. Medições feitas neste
// projeto (1080p60 e 4K60, NVENC, VBR):
//
//   - 1080p a 24 Mbps: 9.5% dos segundos com queda de fps, picos de 114 Mbps na
//     recuperação de engasgo. A 12 Mbps: 0% de quedas.
//   - 4K a 40 Mbps: banda média real de 37 Mbps, ~3.5k pacotes/s, com picos de
//     64 Mbps. Um frame vira ~200 pacotes em rajada — é onde o Wi-Fi perde
//     pacote, a TV manda NACK e a imagem congela.
//
// Daí a régua: o recomendado busca a qualidade máxima que o LINK sustenta, não a
// que a GPU consegue produzir.
type Preset struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Width       int    `json:"width"`
	Height      int    `json:"height"`
	Bitrate     int    `json:"bitrate"` // kbps recomendado
	Description string `json:"description"`
}

// Presets são as opções oferecidas no painel, da mais leve para a mais pesada.
var Presets = []Preset{
	{
		ID:          "720p",
		Label:       "720p60 · leve",
		Width:       1280,
		Height:      720,
		Bitrate:     6000,
		Description: "Wi-Fi fraco ou 2,4 GHz. Prioriza estabilidade sobre nitidez.",
	},
	{
		ID:          "1080p",
		Label:       "1080p60 · equilibrado",
		Width:       1920,
		Height:      1080,
		Bitrate:     12000,
		Description: "Melhor relação nitidez/estabilidade na maioria das redes 5 GHz.",
	},
	{
		ID:          "1080p-alto",
		Label:       "1080p60 · alta qualidade",
		Width:       1920,
		Height:      1080,
		Bitrate:     20000,
		Description: "Wi-Fi 5 GHz forte ou cabo. Mais detalhe em cenas de movimento.",
	},
	{
		ID:          "1440p",
		Label:       "1440p60 · nítido",
		Width:       2560,
		Height:      1440,
		Bitrate:     25000,
		Description: "Exige Wi-Fi 5 GHz forte. Bom meio-termo antes do 4K.",
	},
	{
		ID:          "4k",
		Label:       "4K60 · máximo",
		Width:       3840,
		Height:      2160,
		Bitrate:     35000,
		Description: "Só com Wi-Fi 6 ou cabo. Em rede fraca causa travamentos.",
	},
}

// PresetFor devolve o preset que corresponde à resolução dada, se houver.
func PresetFor(width, height int) (Preset, bool) {
	for _, p := range Presets {
		if p.Width == width && p.Height == height {
			return p, true
		}
	}
	return Preset{}, false
}

// PresetByID busca um preset pelo identificador.
func PresetByID(id string) (Preset, bool) {
	for _, p := range Presets {
		if p.ID == id {
			return p, true
		}
	}
	return Preset{}, false
}

// RecommendPreset escolhe o preset mais alto que a banda medida sustenta.
//
// A margem existe porque o teste de rede mede o pico instantâneo, enquanto o
// stream precisa sustentar a taxa continuamente e ainda dividir o link com o
// áudio, o RTCP e as retransmissões. Rodar o vídeo no limite exato do link é
// exatamente a condição em que a TV começa a mandar NACK e congelar.
func RecommendPreset(measuredMbps float64) Preset {
	usable := measuredMbps * linkUsableRatio / 100

	best := Presets[0]
	for _, p := range Presets {
		if float64(p.Bitrate)/1000 <= usable {
			best = p
		}
	}
	return best
}

// linkUsableRatio: fração da banda medida que consideramos utilizável para o
// vídeo. 60% é conservador de propósito — Wi-Fi doméstico oscila com
// interferência e distância, e o custo de errar para cima (imagem congelando) é
// muito pior do que o de errar para baixo (um pouco menos de nitidez).
const linkUsableRatio = 60
