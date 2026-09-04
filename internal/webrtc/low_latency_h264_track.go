package webrtc

import (
	"math"
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	pionwebrtc "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	// LAN local sem TURN: 1350 reduz a quantidade de pacotes por frame 4K sem
	// encostar no limite prático de MTU Ethernet depois dos cabeçalhos RTP/SRTP.
	rtpOutboundMTU           = 1350
	playoutDelayExtensionURI = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"
	// playout-delay extension = min_delay (12 bits) + max_delay (12 bits), unidade
	// de 10ms. min=0, max=1 -> teto rígido de 10ms de playout.
	//
	// max=0 ("\x00\x00\x00") NÃO funciona na Tizen: o firmware Samsung interpreta
	// max=0 como "sem restrição" e cai no jitter buffer adaptativo, que em LAN
	// ainda assim infla para ~160ms. Um max pequeno porém não-zero é tratado como
	// limite superior real, prendendo o playout em ~10ms.
	// Bytes: 0x00 0x00 0x01 = min 0b000000000000, max 0b000000000001.
	zeroPlayoutDelayExtension = "\x00\x00\x01"

	// paceWindowRatio: percentual da duração do frame usado para espalhar seus
	// pacotes. 25% a 60fps = ~4ms de janela — suficiente para o rádio Wi-Fi
	// escoar a rajada, e curto o bastante para não somar latência perceptível.
	// Não usamos 100% de propósito: ocupar o frame inteiro deixaria o pipeline
	// sem folga para absorver o frame seguinte se ele chegar adiantado.
	paceWindowRatio = 25

	// packetPacingMinPackets: abaixo deste número de pacotes o frame não forma
	// rajada capaz de estourar o buffer do rádio, e o pacing é dispensado.
	//
	// 120 pacotes ≈ 160KB. O limiar anterior (40 ≈ 54KB) pegava o frame típico
	// de 1080p (medido: 63KB / ~48 pacotes) e o pacing passava a rodar SEMPRE:
	// ~4.2ms por frame × 60fps = 252ms de cada segundo gastos dormindo, sem
	// nenhuma rajada real para conter. O resultado foi latência acumulada — o
	// oposto do objetivo.
	//
	// O pacing existe para o frame 4K (200-280KB / 150-210 pacotes), onde a
	// rajada instantânea de fato estoura o buffer do rádio e gera NACK+freeze.
	packetPacingMinPackets = 120

	// packetPacingMinInterval: piso do espaçamento. O scheduler do Windows não
	// entrega sleeps confiáveis abaixo de ~1ms, então pedir menos que isso só
	// gastaria CPU sem espaçar de fato.
	packetPacingMinInterval = time.Millisecond

	// packetPacingSlices: em quantas fatias o frame é dividido. Poucos sleeps
	// longos são muito mais fiéis que muitos curtos neste SO — 4 fatias dão
	// ~1ms de pausa cada a 60fps, dentro da granularidade real do timer.
	packetPacingSlices = 4
)

type lowLatencyH264Track struct {
	mu         sync.RWMutex
	codec      pionwebrtc.RTPCodecCapability
	id         string
	streamID   string
	bindings   []lowLatencyTrackBinding
	packetizer rtp.Packetizer
	clockRate  float64
}

type lowLatencyTrackBinding struct {
	id             string
	ssrc           pionwebrtc.SSRC
	payloadType    pionwebrtc.PayloadType
	writeStream    pionwebrtc.TrackLocalWriter
	playoutDelayID uint8
}

func newLowLatencyH264Track(codec pionwebrtc.RTPCodecCapability, id, streamID string) *lowLatencyH264Track {
	return &lowLatencyH264Track{
		codec:    codec,
		id:       id,
		streamID: streamID,
		bindings: []lowLatencyTrackBinding{},
	}
}

func (t *lowLatencyH264Track) Bind(ctx pionwebrtc.TrackLocalContext) (pionwebrtc.RTPCodecParameters, error) {
	codec, ok := findCodec(t.codec, ctx.CodecParameters())
	if !ok {
		return pionwebrtc.RTPCodecParameters{}, pionwebrtc.ErrUnsupportedCodec
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.bindings = append(t.bindings, lowLatencyTrackBinding{
		id:             ctx.ID(),
		ssrc:           ctx.SSRC(),
		payloadType:    codec.PayloadType,
		writeStream:    ctx.WriteStream(),
		playoutDelayID: findHeaderExtensionID(ctx.HeaderExtensions(), playoutDelayExtensionURI),
	})

	if t.packetizer == nil {
		t.packetizer = rtp.NewPacketizer(
			rtpOutboundMTU,
			0,
			0,
			&codecs.H264Payloader{},
			rtp.NewRandomSequencer(),
			codec.ClockRate,
		)
		t.clockRate = float64(codec.ClockRate)
	}

	return codec, nil
}

func (t *lowLatencyH264Track) Unbind(ctx pionwebrtc.TrackLocalContext) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	for i := range t.bindings {
		if t.bindings[i].id == ctx.ID() {
			t.bindings[i] = t.bindings[len(t.bindings)-1]
			t.bindings = t.bindings[:len(t.bindings)-1]
			return nil
		}
	}

	return pionwebrtc.ErrUnbindFailed
}

func (t *lowLatencyH264Track) ID() string       { return t.id }
func (t *lowLatencyH264Track) StreamID() string { return t.streamID }
func (t *lowLatencyH264Track) RID() string      { return "" }

func (t *lowLatencyH264Track) Kind() pionwebrtc.RTPCodecType {
	return pionwebrtc.RTPCodecTypeVideo
}

// Bound informa se a track já está ligada a pelo menos um receptor, ou seja, se
// um WriteSample agora resulta em RTP na rede em vez de ser descartado.
//
// Existe por causa do pré-aquecimento da captura: o FFmpeg começa a produzir
// ANTES do Bind (para matar o ramp-up do DXGI). Sem esse gate, o primeiro IDR —
// o único que carrega SPS/PPS com -forced-idr — era empacotado no vazio e
// perdido, e a TV ficava em tela preta esperando o próximo keyframe (até 3s de
// GOP, ou para sempre se ele se perdesse).
func (t *lowLatencyH264Track) Bound() bool {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.packetizer != nil && len(t.bindings) > 0
}

func (t *lowLatencyH264Track) WriteSample(sample media.Sample) error {
	t.mu.RLock()
	packetizer := t.packetizer
	clockRate := t.clockRate
	bindings := append([]lowLatencyTrackBinding(nil), t.bindings...)
	t.mu.RUnlock()

	if packetizer == nil || len(bindings) == 0 {
		return nil
	}

	samples := rtpTimestampSamples(sample.Duration, clockRate)
	packets := packetizer.Packetize(sample.Data, samples)
	if len(packets) == 0 {
		return nil
	}

	// Pacing de PACOTES (o que o Steam Link faz): espalha a rajada do frame ao
	// longo de uma fração da sua duração em vez de despejar tudo de uma vez.
	//
	// Um frame 4K de ~265KB vira ~200 pacotes RTP. Disparados no mesmo instante,
	// eles estouram o buffer do AP/rádio Wi-Fi: o excedente é DESCARTADO pela
	// rede, a TV pede retransmissão (NACK) e congela esperando o frame completo —
	// e a retransmissão consome ainda mais banda, realimentando o problema.
	// Medido: 3.5k pacotes/s de média com picos de 6k, em rajadas de 16ms.
	//
	// Espaçá-los faz o mesmo volume caber no link sem estourar o buffer. O custo
	// de latência é limitado a paceWindowRatio do frame (~25% = 4ms a 60fps), bem
	// abaixo do ganho de não perder o frame inteiro.
	burst, pause := packetPacingPlan(sample.Duration, len(packets))

	var writeErr error
	for i, pkt := range packets {
		// Pausa a cada LOTE, não a cada pacote: o sleep do Windows tem
		// granularidade de ~1-15ms, então dormir por pacote (200x) transformaria
		// uma janela alvo de 4ms em >100ms reais e estrangularia o pipeline —
		// medido: writeMax de 126ms e fps caindo para 21. Em lotes, o número de
		// sleeps fica pequeno e previsível.
		if pause > 0 && i > 0 && i%burst == 0 {
			time.Sleep(pause)
		}
		if err := writePacketToBindings(pkt, bindings); err != nil && writeErr == nil {
			writeErr = err
		}
	}

	return writeErr
}

// packetPacingPlan decide de quantos em quantos pacotes pausar, e por quanto.
//
// Devolve pause=0 (envio contínuo, sem custo) para frames pequenos, que não
// formam rajada capaz de estourar o buffer do rádio.
func packetPacingPlan(frameDur time.Duration, packetCount int) (burst int, pause time.Duration) {
	if packetCount <= packetPacingMinPackets || frameDur <= 0 {
		return packetCount, 0
	}

	// Alvo: dividir o frame em poucas fatias. Poucos sleeps longos são muito
	// mais fiéis que muitos sleeps curtos neste SO.
	slices := packetPacingSlices
	burst = packetCount / slices
	if burst < 1 {
		burst = 1
	}

	window := frameDur * paceWindowRatio / 100
	pause = window / time.Duration(slices)
	if pause < packetPacingMinInterval {
		return packetCount, 0
	}
	return burst, pause
}

// packetPacingInterval calcula o espaçamento entre pacotes de um mesmo frame.
//
// Devolve 0 (envio imediato, sem custo) quando o frame é pequeno o bastante para
// não formar rajada relevante — o caso comum em 1080p, onde a otimização não é
// necessária e dormir por pacote só adicionaria overhead de scheduler.
func packetPacingInterval(frameDur time.Duration, packetCount int) time.Duration {
	if packetCount <= packetPacingMinPackets || frameDur <= 0 {
		return 0
	}
	window := frameDur * paceWindowRatio / 100
	interval := window / time.Duration(packetCount)
	if interval < packetPacingMinInterval {
		// Abaixo disto o sleep custa mais em scheduler do que ajuda na rede.
		return 0
	}
	return interval
}

func rtpTimestampSamples(duration time.Duration, clockRate float64) uint32 {
	if duration <= 0 || clockRate <= 0 {
		return 0
	}

	samples := math.Round(duration.Seconds() * clockRate)
	if samples < 1 {
		return 1
	}
	maxUint32 := ^uint32(0)
	if samples > float64(maxUint32) {
		return maxUint32
	}
	return uint32(samples)
}

func writePacketToBindings(pkt *rtp.Packet, bindings []lowLatencyTrackBinding) error {
	var writeErr error
	for _, binding := range bindings {
		pkt.Header.SSRC = uint32(binding.ssrc)
		pkt.Header.PayloadType = uint8(binding.payloadType)
		if binding.playoutDelayID > 0 {
			_ = pkt.Header.SetExtension(binding.playoutDelayID, []byte(zeroPlayoutDelayExtension))
		}
		if _, err := binding.writeStream.WriteRTP(&pkt.Header, pkt.Payload); err != nil && writeErr == nil {
			writeErr = err
		}
	}

	return writeErr
}

func findCodec(want pionwebrtc.RTPCodecCapability, codecs []pionwebrtc.RTPCodecParameters) (pionwebrtc.RTPCodecParameters, bool) {
	wantProfile := profileLevelID(want.SDPFmtpLine)
	for _, codec := range codecs {
		if strings.EqualFold(codec.MimeType, want.MimeType) &&
			codec.ClockRate == want.ClockRate &&
			(wantProfile == "" || strings.Contains(codec.SDPFmtpLine, "profile-level-id="+wantProfile)) {
			return codec, true
		}
	}
	for _, codec := range codecs {
		if strings.EqualFold(codec.MimeType, want.MimeType) && codec.ClockRate == want.ClockRate {
			return codec, true
		}
	}
	return pionwebrtc.RTPCodecParameters{}, false
}

func profileLevelID(fmtp string) string {
	for _, part := range strings.Split(fmtp, ";") {
		part = strings.TrimSpace(part)
		if strings.HasPrefix(part, "profile-level-id=") {
			return strings.TrimPrefix(part, "profile-level-id=")
		}
	}
	return ""
}

func findHeaderExtensionID(headers []pionwebrtc.RTPHeaderExtensionParameter, uri string) uint8 {
	for _, header := range headers {
		if header.URI == uri && header.ID > 0 && header.ID < 15 {
			return uint8(header.ID)
		}
	}
	return 0
}

var _ pionwebrtc.TrackLocal = (*lowLatencyH264Track)(nil)

type sampleWriter interface {
	WriteSample(media.Sample) error
}

type encodedFrame struct {
	data     []byte
	duration time.Duration
	keyframe bool
}
