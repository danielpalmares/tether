package capture

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// Display descreve um monitor que o pipeline de captura consegue usar.
//
// O índice é o output_idx do ddagrab, que NÃO é o mesmo que o número do monitor
// no Windows: o ddagrab enumera as saídas do adaptador DXGI que está usando, e
// num notebook híbrido (NVIDIA + iGPU) os monitores ligados ao iGPU simplesmente
// não aparecem para o adaptador da NVIDIA. Por isso a lista é montada TESTANDO a
// captura de cada índice em vez de traduzir a enumeração do Windows — o painel
// não pode oferecer uma opção que resulta em tela preta.
type Display struct {
	Index     int    `json:"index"`
	Label     string `json:"label"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	Available bool   `json:"available"`
}

const (
	// maxDisplayProbe: até onde sondamos por saídas. Ninguém liga mais de 8
	// monitores num adaptador, e cada sonda custa um processo FFmpeg.
	maxDisplayProbe = 8
	// displayProbeTimeout: uma sonda que não responde nisso é tratada como
	// indisponível. Generoso o bastante para a inicialização do DXGI (~750ms).
	displayProbeTimeout = 4 * time.Second
)

var (
	displayCacheMu   sync.Mutex
	displayCache     []Display
	displayCacheTime time.Time
)

// displayCacheTTL evita re-sondar a cada request do painel: cada sonda sobe um
// FFmpeg, e o conjunto de monitores muda raramente.
const displayCacheTTL = 30 * time.Second

// Displays devolve os monitores capturáveis, sondando o ddagrab uma vez e
// reaproveitando o resultado por displayCacheTTL.
func Displays() []Display {
	displayCacheMu.Lock()
	defer displayCacheMu.Unlock()

	if displayCache != nil && time.Since(displayCacheTime) < displayCacheTTL {
		return displayCache
	}

	found := probeDisplays()
	displayCache = found
	displayCacheTime = time.Now()
	return found
}

// InvalidateDisplays força a próxima chamada de Displays a re-sondar. Usado
// quando o usuário pede atualização explícita no painel.
func InvalidateDisplays() {
	displayCacheMu.Lock()
	displayCache = nil
	displayCacheMu.Unlock()
}

func probeDisplays() []Display {
	var out []Display
	for idx := 0; idx < maxDisplayProbe; idx++ {
		w, h, err := probeDisplay(idx)
		if err != nil {
			// A enumeração DXGI é densa: o primeiro índice que falha marca o fim
			// das saídas desse adaptador. Parar aqui evita 8 sondas inúteis.
			break
		}
		out = append(out, Display{
			Index:     idx,
			Label:     displayLabel(idx, w, h),
			Width:     w,
			Height:    h,
			Available: true,
		})
	}

	if len(out) == 0 {
		// Nunca devolve lista vazia: o painel precisa de ao menos uma opção, e o
		// índice 0 é o padrão que o resto do sistema assume.
		out = append(out, Display{Index: 0, Label: "Monitor principal", Available: true})
	}
	return out
}

func displayLabel(idx, w, h int) string {
	name := "Monitor principal"
	if idx > 0 {
		name = fmt.Sprintf("Monitor %d", idx+1)
	}
	if w > 0 && h > 0 {
		return fmt.Sprintf("%s (%dx%d)", name, w, h)
	}
	return name
}

// probeDisplay testa se o ddagrab captura o índice dado, devolvendo a resolução
// nativa detectada. Um único frame basta: se o output não existe, o FFmpeg falha
// já na configuração do filtro.
func probeDisplay(idx int) (int, int, error) {
	ctx, cancel := context.WithTimeout(context.Background(), displayProbeTimeout)
	defer cancel()

	cmd := exec.CommandContext(ctx, "ffmpeg",
		"-hide_banner",
		"-f", "lavfi",
		"-i", fmt.Sprintf("ddagrab=output_idx=%d:framerate=30:dup_frames=1", idx),
		"-frames:v", "1",
		"-f", "null", "-",
	)
	// O FFmpeg escreve o resumo do stream (com a resolução) no stderr.
	output, err := cmd.CombinedOutput()
	text := string(output)

	if strings.Contains(text, "Failed to enumerate DXGI output") ||
		strings.Contains(text, "Failed to configure output pad") {
		return 0, 0, fmt.Errorf("output %d não existe neste adaptador", idx)
	}
	if err != nil && !strings.Contains(text, "Stream #0:0") {
		return 0, 0, fmt.Errorf("sonda do output %d falhou: %w", idx, err)
	}

	w, h := parseDimensions(text)
	return w, h, nil
}

// parseDimensions extrai "1920x1080" da linha de stream do FFmpeg.
func parseDimensions(text string) (int, int) {
	for _, line := range strings.Split(text, "\n") {
		if !strings.Contains(line, "Stream #0:0") {
			continue
		}
		for _, field := range strings.Split(line, ",") {
			field = strings.TrimSpace(field)
			// procura o token NxN, ignorando sufixos como "[SAR 1:1 DAR 16:9]"
			if i := strings.Index(field, " "); i > 0 {
				field = field[:i]
			}
			var w, h int
			if n, err := fmt.Sscanf(field, "%dx%d", &w, &h); n == 2 && err == nil && w > 0 && h > 0 {
				return w, h
			}
		}
	}
	return 0, 0
}
