// Package gpu expõe a saúde da GPU durante o streaming.
//
// O objetivo é responder uma pergunta concreta do usuário: "está travando por
// causa da minha GPU ou por causa da rede?". Sem isso, a única saída é tentar
// configurações no escuro.
package gpu

import (
	"context"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Status é o retrato da GPU num instante.
type Status struct {
	Available  bool   `json:"available"` // false = sem nvidia-smi (AMD/Intel ou driver ausente)
	Name       string `json:"name"`
	GPUUtil    int    `json:"gpuUtil"`    // % de uso do núcleo gráfico
	EncUtil    int    `json:"encUtil"`    // % de uso do encoder NVENC (independente do núcleo)
	Throttle   string `json:"throttle"`   // "", "power" ou "thermal"
	Saturated  bool   `json:"saturated"`  // núcleo saturado: o JOGO está espremendo a GPU
	EncoderHot bool   `json:"encoderHot"` // encoder saturado: a STREAM está pesada demais
	Advice     string `json:"advice"`     // recomendação em português, vazia se estiver tudo bem
}

const (
	// gpuSaturatedPct: acima disto o núcleo gráfico não tem folga, e é aí que o
	// DWM/Desktop Duplication passa a compor irregularmente — a causa medida das
	// quedas de fps na captura. O gargalo é o JOGO, não a stream.
	gpuSaturatedPct = 95
	// encoderHotPct: o NVENC é um bloco dedicado e raramente satura; quando
	// satura, é a STREAM que está pesada demais (resolução/bitrate/fps).
	encoderHotPct = 90

	sampleTTL    = 900 * time.Millisecond
	queryTimeout = 2 * time.Second
)

var (
	mu         sync.Mutex
	cached     Status
	cachedAt   time.Time
	smiMissing bool
)

// Query devolve o estado atual da GPU, com cache curto para não subir um
// processo nvidia-smi a cada request de estatísticas do painel.
func Query() Status {
	mu.Lock()
	defer mu.Unlock()

	if smiMissing {
		return Status{Available: false}
	}
	if !cachedAt.IsZero() && time.Since(cachedAt) < sampleTTL {
		return cached
	}

	s, err := querySMI()
	if err != nil {
		// Sem nvidia-smi não há por que tentar de novo a cada segundo: marca
		// como ausente e o painel simplesmente omite a seção de GPU.
		smiMissing = true
		return Status{Available: false}
	}

	cached = s
	cachedAt = time.Now()
	return s
}

func querySMI() (Status, error) {
	ctx, cancel := context.WithTimeout(context.Background(), queryTimeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, "nvidia-smi",
		"--query-gpu=name,utilization.gpu,utilization.encoder,clocks_throttle_reasons.sw_power_cap,clocks_throttle_reasons.sw_thermal_slowdown,clocks_throttle_reasons.hw_slowdown",
		"--format=csv,noheader,nounits",
	).Output()
	if err != nil {
		return Status{}, err
	}

	line := strings.TrimSpace(string(out))
	if i := strings.IndexByte(line, '\n'); i > 0 {
		line = line[:i] // primeira GPU
	}
	f := strings.Split(line, ",")
	if len(f) < 6 {
		return Status{}, errShortOutput
	}

	st := Status{
		Available: true,
		Name:      strings.TrimSpace(f[0]),
		GPUUtil:   atoi(f[1]),
		EncUtil:   atoi(f[2]),
	}
	if active(f[3]) {
		st.Throttle = "power"
	} else if active(f[4]) || active(f[5]) {
		st.Throttle = "thermal"
	}

	st.Saturated = st.GPUUtil >= gpuSaturatedPct
	st.EncoderHot = st.EncUtil >= encoderHotPct
	st.Advice = advise(st)
	return st, nil
}

// advise traduz os números na ação que o usuário precisa tomar. A distinção que
// importa: gargalo no NÚCLEO pede baixar a qualidade do JOGO; gargalo no ENCODER
// pede baixar a qualidade da STREAM. Confundir os dois leva a mexer no lugar
// errado e não resolver nada.
func advise(s Status) string {
	switch {
	case s.EncoderHot && s.Saturated:
		return "GPU e encoder no limite: reduza a qualidade do jogo e a resolução da stream."
	case s.EncoderHot:
		return "Encoder no limite: reduza a resolução ou o bitrate da stream."
	case s.Throttle == "thermal":
		return "GPU em throttle térmico: verifique a ventilação; o desempenho está sendo cortado."
	case s.Throttle == "power":
		return "GPU limitada por energia: no notebook, ligue na tomada e use o perfil de alto desempenho."
	case s.Saturated:
		return "GPU no limite pelo jogo: reduza os gráficos do jogo ou limite o fps dele."
	}
	return ""
}

func atoi(s string) int {
	n, _ := strconv.Atoi(strings.TrimSpace(s))
	return n
}

func active(s string) bool {
	return strings.EqualFold(strings.TrimSpace(s), "Active")
}

type shortOutputError struct{}

func (shortOutputError) Error() string { return "saída inesperada do nvidia-smi" }

var errShortOutput = shortOutputError{}
