package config

import "time"

// TuningProfile é o conjunto de parâmetros de encoder/transporte DERIVADOS de
// bitrate × resolução × fps. Centraliza num único lugar a "fórmula" que antes
// estava espalhada em divisores fixos (gop=fps, vbv=bitrate/8), espelhando a
// abordagem do Steam Link: os ajustes finos do encoder são função da carga, não
// constantes.
//
// Objetivo: eliminar as rajadas periódicas que travam a TV. As duas fontes de
// rajada num pipeline CBR são (1) o IDR de keyframe — pico de tamanho a cada
// GOP — e (2) o VBV curto demais, que força o encoder a despejar frames grandes
// de uma vez. O profile ataca ambos: GOP longo amortiza a frequência dos picos,
// e o VBV dimensionado em milissegundos de bitrate dá folga para o rate control
// "espalhar" o IDR ao longo de alguns frames em vez de soltá-lo em lote.
type TuningProfile struct {
	GOPFrames    int // distância entre IDRs, em frames
	VBVBufferKb  int // -bufsize do NVENC, em kbit
	Surfaces     int // -surfaces: fila interna do NVENC
	RateControl  string
	SpatialAQ    bool // distribui bits para reduzir picos em cenas complexas
	TemporalAQ   bool // distribui bits ENTRE frames -> achata o pico do IDR
	Level        string
	ProfileLevel string // profile-level-id da SDP

	// --- transporte/pacing (lado Go, consumido em pumpVideo) ---
	// FrameQueueDepth: fila entre o leitor de NALs e o escritor RTP. Curta
	// (~65ms): absorve uma rajada breve do writer sem acumular latência. Ver
	// queueMillis.
	FrameQueueDepth int
	// PacerMaxHold: teto de espera do pacer por frame, em duração. ~meio frameDur.
	// O pacer só apara frames adiantados; NUNCA impõe cadência. Limita rigidamente
	// o lag de input que a suavização pode adicionar.
	PacerMaxHold time.Duration
}

// Constantes de derivação. Documentadas aqui para que o ajuste fino seja
// auditável num só ponto.
const (
	// gopSeconds: distância alvo entre keyframes. GOP longo => menos IDRs/seg =>
	// menos picos de rajada. Em LAN com NACK/PLI ativos, uma perda em frame de
	// referência é recuperada por retransmissão ou por PLI sob demanda; não
	// precisamos de 1 IDR/seg "preventivo" como na config antiga. 3s é o
	// equilíbrio: raro o bastante para suavizar, frequente o bastante para um
	// recover rápido caso o PLI se perca.
	gopSeconds = 3

	// vbvMillisFloor / vbvMillisCeil: tamanho do VBV expresso em MILISSEGUNDOS de
	// bitrate (bufsize = bitrate * ms / 1000). Esta é a mudança central vs. o
	// divisor fixo. Um VBV em ms dá ao rate control uma janela temporal estável
	// para amortizar o IDR, independente do bitrate absoluto. Janela curta
	// (baixa latência) mas não tão curta a ponto de obrigar o despejo do IDR num
	// único frame. Resoluções maiores ganham janela um pouco maior porque o IDR
	// é proporcionalmente maior.
	// Janela VBV em ms de bitrate, por resolução. 2K/4K ganham janela maior: o log
	// mostrou que o gap de envio acompanha o TAMANHO do frame (4K frameMax 171KB vs
	// 1080p 34KB), e o frame grande pulsa quando o VBV é apertado. Mais janela dá
	// ao rate control espaço para achatar o frame -> chegada mais regular na TV.
	// 1080p está ok e fica intocado.
	vbvMillisFloor = 30 // 1080p
	vbvMillis2K    = 55
	vbvMillis4K    = 75

	// queueMillis: folga temporal da fila de vídeo. Curta de propósito (~65ms a
	// 60fps ≈ 4 frames): só precisa absorver uma rajada breve do writer RTP sem
	// virar acumulador de latência. Fila grande (250ms na tentativa anterior) NÃO
	// ajudou — o log mostrou pico real de 2-4 frames; o excedente só somava lag.
	queueMillis = 65

	// O libx264 é o caminho de compatibilidade/teste: ele já paga custo de cópia
	// GPU->CPU e encode CPU. Para não mascarar esse custo com mais atraso no
	// transporte, usa uma janela mais curta que o NVENC.
	x264VBVMillis   = 20
	x264QueueMillis = 35

	// pacerHoldPercent*: o teto de espera do pacer é uma PORCENTAGEM do frameDur,
	// ESCALADA por resolução. Frame grande (2K/4K) leva mais tempo para
	// packetizar/enviar e chega mais irregular; meio-frame (1080p) é pouca folga
	// para alisar 2K/4K. Resolução maior => mais folga. O pacer continua só
	// APARANDO frames adiantados (nunca impõe cadência), então o hold maior só dá
	// teto à suavização — o lag de input real continua limitado a < 1 frame.
	pacerHoldPercent1080 = 50  // frameDur*50% ≈ 8ms
	pacerHoldPercent2K   = 75  // ≈ 12.5ms
	pacerHoldPercent4K   = 100 // ≈ 16.6ms (um frame inteiro de teto)
	x264PacerHoldPercent = 25  // ≈ 4ms em 60fps: só tira micro-rajada

	// rateControlMode: VBR com -maxrate == -b:v (teto rígido), NÃO cbr.
	//
	// Em CBR o NVENC é obrigado a ATINGIR a taxa alvo mesmo quando a cena não
	// precisa, e completa a diferença com NAL de filler data (tipo 12) —
	// literalmente padding descartado pelo decoder. Medido neste projeto a
	// 1080p60/24Mbps com a tela em repouso: 21.3 Mbps dos 23.9 Mbps enviados eram
	// filler (89% da banda), para 2.6 Mbps de vídeo real. Num link Wi-Fi para a
	// TV esse padding compete com o vídeo útil e com o áudio, enche o buffer do
	// receptor e provoca engasgo — pagando latência por bits que não são imagem.
	//
	// VBR com teto mantém o mesmo pico disponível para cenas de movimento intenso
	// (é o -maxrate que define a qualidade máxima) e gasta apenas o necessário no
	// resto do tempo. Mesma qualidade, fração da banda.
	rateControlMode = "vbr"
)

// Tuning calcula o profile a partir da config já normalizada.
func (c StreamConfig) Tuning() TuningProfile {
	c = c.Normalize()

	fps := c.FPS
	if fps <= 0 {
		fps = 60
	}

	// O perfil de latência é a fonte de verdade para GOP, fila e hold do pacer:
	// é ele que o usuário escolhe conscientemente no painel.
	lat := NormalizeLatency(c.Latency).Settings()
	gop := lat.GOPSeconds * fps

	// VBV e hold do pacer escalam JUNTOS com a resolução (mesma causa: frame maior
	// pulsa mais e chega mais irregular).
	vbvMs := vbvMillisFloor
	switch {
	case c.Width >= 3840 || c.Height >= 2160:
		vbvMs = vbvMillis4K
	case c.Width >= 2560 || c.Height >= 1440:
		vbvMs = vbvMillis2K
	}
	// Hold do pacer = intenção do usuário (perfil) × escala da resolução.
	//
	// O perfil define QUANTO suavizar; a resolução ajusta porque frame maior
	// leva mais tempo para packetizar/enviar e chega mais irregular (medido:
	// 4K frameMax 227KB vs 1080p 65KB). Manter só o perfil deixaria o 4K com
	// pouca folga; manter só a resolução ignoraria a escolha do usuário. O
	// perfil "ultra" continua zerado em qualquer resolução — é o seu contrato.
	holdPct := lat.PacerHoldPercent
	if holdPct > 0 {
		switch {
		case c.Width >= 3840 || c.Height >= 2160:
			holdPct = holdPct * pacerHoldPercent4K / pacerHoldPercent1080
		case c.Width >= 2560 || c.Height >= 1440:
			holdPct = holdPct * pacerHoldPercent2K / pacerHoldPercent1080
		}
	}
	vbv := c.Bitrate * vbvMs / 1000
	if vbv <= 0 {
		vbv = c.Bitrate
	}

	// Fila em frames = folga(ms) × fps / 1000, com piso de 2 (nunca regredir ao
	// comportamento sem buffer). Regra única: escala sozinha com qualquer fps.
	queueMs := lat.QueueMillis
	if c.Codec == CodecH264X264 {
		vbv = c.Bitrate * x264VBVMillis / 1000
		queueMs = x264QueueMillis
		holdPct = x264PacerHoldPercent
		if vbv <= 0 {
			vbv = c.Bitrate
		}
	}
	queueDepth := queueMs * fps / 1000
	if queueDepth < 2 {
		queueDepth = 2
	}

	// Hold do pacer = frameDur × holdPct% (escalado por resolução). Derivado do
	// fps + resolução; vale para qualquer cadência.
	frameDur := time.Second / time.Duration(fps)
	pacerHold := frameDur * time.Duration(holdPct) / 100

	return TuningProfile{
		GOPFrames:       gop,
		VBVBufferKb:     vbv,
		Surfaces:        c.surfaces(),
		RateControl:     rateControlMode,
		SpatialAQ:       true,
		TemporalAQ:      true,
		Level:           c.H264Level(),
		ProfileLevel:    c.H264ProfileLevelID(),
		FrameQueueDepth: queueDepth,
		PacerMaxHold:    pacerHold,
	}
}

// surfaces escala a fila interna do NVENC pela carga (pixels × fps). Com apenas
// 2 surfaces fixas (config antiga), em resolução/bitrate alto o encoder não tem
// onde pipelinizar e despeja frames em lote — exatamente a rajada que trava a
// TV. Mais surfaces suavizam a SAÍDA do encoder sem reintroduzir latência de
// reordenação (continuamos sem B-frames e com -delay 0). Mantém um teto baixo
// para não inflar a latência de fila.
// Medição (1080p60, NVENC, 10s por rodada): com 3 surfaces, 13% dos frames
// saíam do encoder fora da cadência de 16.7ms; com 8, caiu para 10%. As
// surfaces são buffers de trabalho do NVENC, não fila de reordenação — com
// -bf 0 e -delay 0 elas não somam latência, apenas evitam que o encoder tenha
// de esperar um buffer livre para começar o próximo frame.
func (c StreamConfig) surfaces() int {
	megapixels := (c.Width * c.Height) / 1_000_000
	switch {
	case megapixels >= 8: // 4K
		return 12
	case megapixels >= 3: // 1440p
		return 10
	default: // 1080p e abaixo
		return 8
	}
}
