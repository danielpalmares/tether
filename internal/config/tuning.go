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

	// pacerHoldPercent*: o teto de espera do pacer é uma PORCENTAGEM do frameDur,
	// ESCALADA por resolução. Frame grande (2K/4K) leva mais tempo para
	// packetizar/enviar e chega mais irregular; meio-frame (1080p) é pouca folga
	// para alisar 2K/4K. Resolução maior => mais folga. O pacer continua só
	// APARANDO frames adiantados (nunca impõe cadência), então o hold maior só dá
	// teto à suavização — o lag de input real continua limitado a < 1 frame.
	pacerHoldPercent1080 = 50  // frameDur*50% ≈ 8ms
	pacerHoldPercent2K   = 75  // ≈ 12.5ms
	pacerHoldPercent4K   = 100 // ≈ 16.6ms (um frame inteiro de teto)
)

// Tuning calcula o profile a partir da config já normalizada.
func (c StreamConfig) Tuning() TuningProfile {
	c = c.Normalize()

	fps := c.FPS
	if fps <= 0 {
		fps = 60
	}

	gop := gopSeconds * fps

	// VBV e hold do pacer escalam JUNTOS com a resolução (mesma causa: frame maior
	// pulsa mais e chega mais irregular).
	vbvMs := vbvMillisFloor
	holdPct := pacerHoldPercent1080
	switch {
	case c.Width >= 3840 || c.Height >= 2160:
		vbvMs = vbvMillis4K
		holdPct = pacerHoldPercent4K
	case c.Width >= 2560 || c.Height >= 1440:
		vbvMs = vbvMillis2K
		holdPct = pacerHoldPercent2K
	}
	vbv := c.Bitrate * vbvMs / 1000
	if vbv <= 0 {
		vbv = c.Bitrate
	}

	// Fila em frames = folga(ms) × fps / 1000, com piso de 2 (nunca regredir ao
	// comportamento sem buffer). Regra única: escala sozinha com qualquer fps.
	queueDepth := queueMillis * fps / 1000
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
		RateControl:     "cbr",
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
func (c StreamConfig) surfaces() int {
	megapixels := (c.Width * c.Height) / 1_000_000
	switch {
	case megapixels >= 8: // 4K
		return 6
	case megapixels >= 3: // 1440p
		return 4
	default: // 1080p e abaixo
		return 3
	}
}
