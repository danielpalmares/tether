package config

import "time"

// LatencyProfile controla o compromisso entre suavidade e atraso.
//
// É o equivalente ao "Streaming Quality / Latency" do Steam Link: o mesmo
// hardware pode privilegiar resposta imediata (competitivo) ou imagem estável
// (single-player em rede ruim). Os dois extremos são legítimos e a escolha é do
// usuário — o que muda é quanto o pipeline pode segurar um frame para suavizar,
// e quanto buffer o cliente mantém.
type LatencyProfile string

const (
	// LatencyUltra: nada de suavização. O frame vai para a rede assim que sai do
	// encoder. Menor input lag possível; em rede instável, mais engasgo visível.
	LatencyUltra LatencyProfile = "ultra"

	// LatencyBalanced: padrão. Permite ao pacer aparar frames adiantados dentro
	// de meio quadro, o que absorve o jitter da captura sem custo perceptível.
	LatencyBalanced LatencyProfile = "balanced"

	// LatencySmooth: prioriza continuidade. Aceita segurar até um quadro inteiro
	// e mantém um buffer maior no cliente — troca alguns ms de resposta por uma
	// imagem que não pisca em Wi-Fi ruim.
	LatencySmooth LatencyProfile = "smooth"
)

// LatencySettings são os parâmetros derivados de um perfil.
type LatencySettings struct {
	// PacerHoldPercent: teto de espera do pacer, em % da duração do frame.
	PacerHoldPercent int
	// QueueMillis: folga da fila de vídeo, em ms.
	QueueMillis int
	// ClientJitterBufferMs: alvo de jitter buffer sugerido ao cliente.
	ClientJitterBufferMs int
	// GOPSeconds: distância entre keyframes. Menor = recuperação mais rápida de
	// perda, ao custo de picos de bitrate mais frequentes.
	GOPSeconds  int
	Label       string
	Description string
}

// Settings devolve os parâmetros do perfil. Perfil desconhecido cai no padrão.
func (p LatencyProfile) Settings() LatencySettings {
	switch p {
	case LatencyUltra:
		return LatencySettings{
			PacerHoldPercent:     0, // sem hold: envio imediato
			QueueMillis:          35,
			ClientJitterBufferMs: 0,
			GOPSeconds:           2,
			Label:                "Ultra baixa latência",
			Description:          "Resposta mais rápida. Ideal para jogos competitivos em rede boa.",
		}
	case LatencySmooth:
		return LatencySettings{
			// "Suave" NÃO pode significar "acumular fila": cada frame parado na
			// fila é ~16.7ms de atraso na tela. Com fila de 100ms ela vivia cheia
			// (medido: 5-6 de 6) e somava ~100ms fixos ao lag, sensação de imagem
			// atrasada em relação ao comando. A suavização real vem do HOLD do
			// pacer (que apara frames adiantados) e do buffer do cliente — não de
			// empilhar frames no host.
			PacerHoldPercent:     100, // até um quadro inteiro de suavização
			QueueMillis:          50,
			ClientJitterBufferMs: 60,
			GOPSeconds:           2,
			Label:                "Suave",
			Description:          "Imagem mais estável em Wi-Fi fraco, com alguns ms a mais de resposta.",
		}
	default:
		return LatencySettings{
			PacerHoldPercent: 50,
			// 40ms ≈ 2 frames a 60fps: absorve a rajada de chegada do ddagrab sem
			// virar acumulador. Ver a nota do perfil "smooth" sobre fila = latência.
			QueueMillis: 40,
			// GOP de 2s (não 3s): o keyframe é o ÚNICO ponto de entrada válido no
			// stream, então o GOP define quanto tempo a TV pode ficar em tela preta
			// ao conectar, ao se recuperar de perda, ou depois de a captura ser
			// reiniciada por um jogo trocando de modo de display. Com VBR o IDR
			// deixou de ser um pico caro (o filler sumiu), então encurtar custa
			// pouca banda e reduz bastante o pior caso de espera.
			ClientJitterBufferMs: 30,
			GOPSeconds:           2,
			Label:                "Equilibrado",
			Description:          "Padrão recomendado: resposta rápida com proteção contra jitter.",
		}
	}
}

// NormalizeLatency valida o perfil vindo do painel.
func NormalizeLatency(p LatencyProfile) LatencyProfile {
	switch p {
	case LatencyUltra, LatencyBalanced, LatencySmooth:
		return p
	default:
		return LatencyBalanced
	}
}

// LatencyProfiles lista os perfis para o painel montar o seletor.
func LatencyProfiles() []map[string]any {
	out := make([]map[string]any, 0, 3)
	for _, p := range []LatencyProfile{LatencyUltra, LatencyBalanced, LatencySmooth} {
		s := p.Settings()
		out = append(out, map[string]any{
			"id":          string(p),
			"label":       s.Label,
			"description": s.Description,
		})
	}
	return out
}

// PacerHold converte o percentual do perfil na duração concreta de espera.
func (s LatencySettings) PacerHold(fps int) time.Duration {
	if fps <= 0 {
		fps = 60
	}
	frameDur := time.Second / time.Duration(fps)
	return frameDur * time.Duration(s.PacerHoldPercent) / 100
}
