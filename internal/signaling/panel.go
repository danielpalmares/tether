package signaling

import (
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"time"

	"tether/internal/capture"
	"tether/internal/config"
	"tether/internal/gpu"
)

// Este arquivo expõe o que o painel único (no cliente) precisa para configurar o
// host sem que o usuário abra a interface do PC: opções disponíveis, saúde da
// GPU e um teste de banda que mede o caminho REAL do stream.

// HandleOptions devolve tudo que o painel precisa para se montar: presets de
// resolução com bitrate recomendado, perfis de latência, monitores realmente
// capturáveis e o encoder detectado.
func (s *Server) HandleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	if r.URL.Query().Get("refresh") == "1" {
		capture.InvalidateDisplays()
	}

	codec := config.DetectCodec()
	_ = json.NewEncoder(w).Encode(map[string]any{
		"presets":  config.Presets,
		"latency":  config.LatencyProfiles(),
		"displays": capture.Displays(),
		"codec": map[string]any{
			"id":    codec,
			"label": config.CodecLabel(codec),
		},
	})
}

// HandleStats devolve a saúde do host durante o stream: estado da GPU e o
// diagnóstico de por que a imagem pode estar travando.
func (s *Server) HandleStats(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	s.mu.Lock()
	streaming := s.active != nil
	cfg := s.cfg
	s.mu.Unlock()

	_ = json.NewEncoder(w).Encode(map[string]any{
		"streaming": streaming,
		"gpu":       gpu.Query(),
		"config":    cfg,
	})
}

// --- Teste de banda -------------------------------------------------------
//
// Medir "internet" não serve de nada aqui: o stream nunca sai da LAN. O que
// importa é quanto o caminho host->TV sustenta, que é justamente o link que
// satura e faz a TV pedir NACK e congelar. Por isso o teste baixa um bloco de
// dados do próprio host, pelo mesmo Wi-Fi e a mesma porta que o vídeo usa.

// bandwidthChunk é o tamanho de bloco enviado no teste. 4 MB é grande o
// bastante para o TCP sair do slow-start e a medida refletir a taxa sustentada,
// e pequeno o bastante para o teste terminar rápido numa rede ruim.
const bandwidthChunk = 4 << 20

// maxBandwidthBytes limita o que um cliente pode pedir, para o endpoint não
// virar um gerador de tráfego arbitrário.
const maxBandwidthBytes = 32 << 20

// HandleBandwidth despeja bytes aleatórios para o cliente medir a taxa de
// download real do link até o host.
//
// Os bytes são aleatórios de propósito: um bloco de zeros seria comprimido por
// qualquer camada intermediária e a medição sairia otimista.
func (s *Server) HandleBandwidth(w http.ResponseWriter, r *http.Request) {
	size := bandwidthChunk
	if q := r.URL.Query().Get("bytes"); q != "" {
		if n, err := strconv.Atoi(q); err == nil && n > 0 {
			size = n
		}
	}
	if size > maxBandwidthBytes {
		size = maxBandwidthBytes
	}

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Content-Length", strconv.Itoa(size))

	// Buffer aleatório reutilizado: gerar 4MB de aleatoriedade por request
	// custaria mais CPU do que a rede, e distorceria a medida.
	buf := make([]byte, 64<<10)
	if _, err := rand.Read(buf); err != nil {
		http.Error(w, "falha ao gerar dados", http.StatusInternalServerError)
		return
	}

	remaining := size
	for remaining > 0 {
		n := len(buf)
		if n > remaining {
			n = remaining
		}
		if _, err := w.Write(buf[:n]); err != nil {
			return // cliente desistiu; não é erro
		}
		remaining -= n
	}
}

// HandleBandwidthUp mede o sentido cliente->host (relevante para o canal de
// input e para o RTCP de volta). Apenas drena o corpo e reporta o tempo.
func (s *Server) HandleBandwidthUp(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if r.Method != http.MethodPost {
		http.Error(w, "método não permitido", http.StatusMethodNotAllowed)
		return
	}

	start := time.Now()
	n, _ := io.Copy(io.Discard, http.MaxBytesReader(w, r.Body, maxBandwidthBytes))
	elapsed := time.Since(start)

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"bytes":      n,
		"millis":     elapsed.Milliseconds(),
		"mbpsUpload": mbps(n, elapsed),
	})
}

func mbps(bytes int64, d time.Duration) float64 {
	if d <= 0 {
		return 0
	}
	return float64(bytes) * 8 / d.Seconds() / 1e6
}

// HandleRecommend converte uma banda medida (Mbps, enviada pelo cliente) no
// preset que o link sustenta com folga.
func (s *Server) HandleRecommend(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Access-Control-Allow-Origin", "*")

	measured, _ := strconv.ParseFloat(r.URL.Query().Get("mbps"), 64)
	if measured <= 0 {
		http.Error(w, "parâmetro mbps obrigatório", http.StatusBadRequest)
		return
	}

	p := config.RecommendPreset(measured)
	_ = json.NewEncoder(w).Encode(map[string]any{
		"measuredMbps": measured,
		"preset":       p,
	})
}
