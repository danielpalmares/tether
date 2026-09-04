package webrtc

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"math/rand"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/rtcp"
	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"

	"tether/internal/audio"
	"tether/internal/capture"
	"tether/internal/config"
	"tether/internal/input"
	"tether/internal/steam"
)

// Session representa uma sessão de streaming com um client.
type Session struct {
	pc       *webrtc.PeerConnection
	cfg      config.StreamConfig
	cap      streamCapturer
	audioCap audioCapturer
	injector input.Injector
	input    inputStats
	audioOn  bool
	onClose  func()

	// paramResend é armado pelo receptor de RTCP quando chega um PLI/FIR e
	// consumido pelo writer de vídeo, que prefixa SPS/PPS no próximo frame.
	paramResend atomic.Bool

	// mu protege a troca do capturador durante um reinício da captura.
	mu sync.Mutex
	// closed/done encerram o supervisor de captura junto com a sessão, para que
	// o fim da sessão não seja confundido com uma queda do ddagrab.
	closed    atomic.Bool
	done      chan struct{}
	closeOnce sync.Once
}

func (s *Session) requestParamResend() { s.paramResend.Store(true) }

type streamCapturer interface {
	Start(context.Context) (io.ReadCloser, error)
	Stop()
}

var newCapturer = func(cfg config.StreamConfig) streamCapturer {
	return capture.New(cfg)
}

type audioCapturer interface {
	Start(context.Context) (*audio.RTPStream, error)
	Stop()
}

var newAudioCapturer = func() audioCapturer {
	return audio.New()
}

// NewSession cria a peer connection, registra a track de vídeo e o data channel.
func NewSession(cfg config.StreamConfig, injector input.Injector, onClose func()) (*Session, error) {
	cfg = cfg.Normalize()
	videoNack := videoNackEnabled()

	// Codec H264 ÚNICO do host, definido uma vez e reusado em RegisterCodec, na
	// track e no SetCodecPreferences — os três PRECISAM ser idênticos byte a byte.
	//
	// profile-level-id dinâmico:
	// 42c02a = Baseline + profile-iop 0xc0 + level 4.2 (1080p60)
	// 42c033 = Baseline + profile-iop 0xc0 + level 5.1 (1440p60)
	// 42c034 = Baseline + profile-iop 0xc0 + level 5.2 (2160p60)
	// O profile-iop reflete os constraint_set flags reais do SPS H.264 Baseline,
	// medidos com trace_headers: constraint_set0=1, set1=1, set2=0 -> 0b11000000 = 0xc0.
	// Decoders de TV (Samsung Tizen) configuram o pipeline pelo profile-level-id da
	// SDP; se o SPS diverge — ou se a SDP anuncia um LEVEL menor que o bitstream —
	// o decoder de hardware estoura e congela. Chrome/Android toleram; a TV não.
	profileLevelID := cfg.H264ProfileLevelID()
	h264Codec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:     webrtc.MimeTypeH264,
			ClockRate:    90000,
			SDPFmtpLine:  "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=" + profileLevelID,
			RTCPFeedback: videoRTCPFeedback(videoNack),
		},
		PayloadType: 102,
	}
	opusCodec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeOpus,
			ClockRate:   48000,
			Channels:    2,
			SDPFmtpLine: "minptime=10;useinbandfec=0;stereo=1;sprop-stereo=1",
		},
		PayloadType: audio.RTPPayloadType,
	}

	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(h264Codec, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}
	if err := m.RegisterCodec(opusCodec, webrtc.RTPCodecTypeAudio); err != nil {
		return nil, err
	}
	if err := m.RegisterHeaderExtension(
		webrtc.RTPHeaderExtensionCapability{URI: playoutDelayExtensionURI},
		webrtc.RTPCodecTypeVideo,
		webrtc.RTPTransceiverDirectionSendonly,
	); err != nil {
		return nil, err
	}

	// RTCP Reports. NACK evita que uma perda pontual em frame de referência deixe
	// o decoder da TV preso enquanto o áudio continua. Se algum firmware voltar a
	// inflar o jitter target por causa de retransmissão, TETHER_VIDEO_NACK=0 desliga.
	ir := &interceptor.Registry{}
	if videoNack {
		if err := webrtc.ConfigureNack(m, ir); err != nil {
			return nil, err
		}
	}
	if err := webrtc.ConfigureRTCPReports(ir); err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(
		webrtc.WithMediaEngine(m),
		webrtc.WithInterceptorRegistry(ir),
	)
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		// Na LAN não precisamos de STUN/TURN; ICE resolve com candidatos locais.
		ICEServers: []webrtc.ICEServer{},
	})
	if err != nil {
		return nil, err
	}
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(opusCodec.RTPCodecCapability, "audio", "tether-audio")
	if err != nil {
		return nil, err
	}
	audioSender, err := pc.AddTrack(audioTrack)
	if err != nil {
		return nil, err
	}

	s := &Session{pc: pc, cfg: cfg, injector: injector, onClose: onClose, done: make(chan struct{})}

	// --- Track de vídeo ---
	// A track carrega o MESMO capability do codec registrado.
	videoTrack := newLowLatencyH264Track(h264Codec.RTPCodecCapability, "video", "tether-video")
	rtpSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		return nil, err
	}

	// FORÇA o transceiver a ofertar/aceitar SOMENTE o 42c02a. Sem isto, o Pion ao
	// processar a offer faz match parcial por MIME type e ADICIONA à negociação
	// todos os perfis H264 que o client oferece (42001f, 42e01f, ...). O 42001f
	// (Level 3.1 = 720p30) acabava na posição preferencial da answer; a TV casava
	// esse payload, configurava o decoder para 720p e recebia o bitstream 1080p60
	// do host — estouro de level -> descarte de frames e congelamento. Restringir
	// o transceiver ao nosso único codec garante que a answer descreva exatamente
	// o que o WriteSample empacota. (Pion: SetCodecPreferences ANTES do
	// CreateAnswer; getCodecs() do transceiver passa a retornar só estes.)
	for _, t := range pc.GetTransceivers() {
		if t.Sender() == rtpSender {
			if err := t.SetCodecPreferences([]webrtc.RTPCodecParameters{h264Codec}); err != nil {
				return nil, err
			}
			break
		}
	}
	// O RTCP do receiver (PLI, NACK, Receiver Reports) chega NESTE sender. É a
	// leitura de rtpSender que entrega esse RTCP aos interceptors — sem o loop, o
	// NACK responder nunca vê os pedidos de retransmissão e o feedback é ignorado
	// em silêncio. Buffer descartável; encerra quando a sessão fecha.
	go func() {
		for {
			packets, _, rtcpErr := rtpSender.ReadRTCP()
			if rtcpErr != nil {
				return
			}
			// PLI/FIR = "perdi o quadro de referência, me manda parâmetros e um
			// ponto de entrada". Antes esse feedback era lido e DESCARTADO, então a
			// TV só se recuperava no próximo IDR do GOP (até 3s de tela preta).
			// Agora ele arma o reenvio imediato de SPS/PPS no pump.
			for _, p := range packets {
				switch p.(type) {
				case *rtcp.PictureLossIndication, *rtcp.FullIntraRequest:
					s.requestParamResend()
				}
			}
		}
	}()
	go func() {
		buf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := audioSender.Read(buf); rtcpErr != nil {
				return
			}
		}
	}()

	// Pré-aquece a captura JÁ, em paralelo ao handshake WebRTC. O DXGI Desktop
	// Duplication tem ~750ms de custo de inicialização (criar device, primeiro
	// frame). Iniciando aqui — antes de "connected" — o pipeline FFmpeg já está
	// entregando 60fps quando a conexão sobe, eliminando o ramp-up visível no
	// começo do stream (qualidade Steam Link: vídeo fluido desde o frame 1).
	s.cap = newCapturer(cfg)
	stream, capErr := s.cap.Start(context.Background())
	if capErr != nil {
		log.Printf("[capture] erro ao pré-aquecer: %v", capErr)
	} else {
		go s.superviseVideo(stream, videoTrack)
	}
	var startAudioOnce sync.Once
	startAudio := func() {
		startAudioOnce.Do(func() {
			go func() {
				if s.pc == nil || s.pc.ConnectionState() != webrtc.PeerConnectionStateConnected {
					return
				}
				s.audioCap = newAudioCapturer()
				audioStream, audioErr := s.audioCap.Start(context.Background())
				if audioErr != nil {
					log.Printf("[audio] captura indisponível: %v", audioErr)
					return
				}
				s.pumpAudio(audioStream, audioTrack)
			}()
		})
	}

	// --- Data channel de input (negociado pelo client) ---
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "input" {
			return
		}
		log.Println("[webrtc] data channel de input aberto")
		s.input.lastNs.Store(time.Now().UnixNano())
		stopInputLog := make(chan struct{})
		go logInputStats(&s.input, stopInputLog)
		dc.OnClose(func() {
			close(stopInputLog)
			log.Println("[webrtc] data channel de input fechado")
		})
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			if err := s.handleInputMessage(msg.Data); err != nil {
				log.Printf("[input] apply: %v", err)
			}
		})
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[webrtc] estado: %s", state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			// A captura já está rodando (pré-aquecida no NewSession). Aqui só
			// disparamos o Big Picture.
			log.Println("[steam] abrindo Big Picture")
			if err := steam.LaunchBigPicture(); err != nil {
				log.Printf("[steam] aviso: %v", err)
			}
			if s.audioOn {
				startAudio()
			}
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			s.Close()
		}
	})

	return s, nil
}

func videoNackEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TETHER_VIDEO_NACK"))) {
	case "0", "false", "no", "off":
		return false
	default:
		return true
	}
}

func videoRTCPFeedback(enableNack bool) []webrtc.RTCPFeedback {
	if !enableNack {
		return nil
	}
	return []webrtc.RTCPFeedback{
		{Type: "nack"},
		{Type: "nack", Parameter: "pli"},
	}
}

// audioFrameDuration é a cadência alvo de saída do áudio. O FFmpeg empacota Opus
// em frames de 20ms (frame_duration=20), mas o muxer RTP os emite em RAJADA —
// vários pacotes colados (<2ms) seguidos de silêncio de ~90ms (medido: 77% dos
// pacotes saem agrupados). Essa rajada infla o jitter buffer de áudio da TV e,
// via sincronismo A/V, arrasta o vídeo junto (lip-sync). Nenhuma flag do FFmpeg
// corrige (testadas max_delay/muxdelay/muxpreload/avioflags=direct/pkt_size). A
// correção é re-pacear no host: ler o socket sem bloquear e escrever cada pacote
// no ritmo de 20ms.
const audioFrameDuration = 20 * time.Millisecond

// audioPaceQueueDepth é a folga máxima antes de descartar. Mantém latência baixa:
// se a fila passa disto, preferimos soltar o pacote mais antigo a acumular atraso.
// Fila de 16 (~320ms): espaço para a rajada do FFmpeg sem o LEITOR ter de
// descartar. O controle de latência é feito no escritor (audioBacklogTarget),
// que decide com critério; o leitor descartando por fila cheia perdia áudio bom
// só porque a saída estava momentaneamente ocupada.
const audioPaceQueueDepth = 16

// audioBacklogTarget é a folga tolerada na fila de áudio (em pacotes de 20ms)
// ANTES de considerar que há atraso acumulado. Só o que exceder isso é
// descartado; o resto é ENVIADO, não jogado fora.
//
// Cuidado ao mexer: o ticker do Go no Windows tem resolução de ~15.6ms, então um
// NewTicker(20ms) dispara ~24x/s, não 50x/s. Emitir 1 pacote por tick limita a
// vazão a ~24 pkt/s — metade do necessário — e o áudio fica entrecortado
// (medido: 24 pkt/s, 40kbps de 96kbps, 480ms de áudio por segundo real). Por
// isso o writer drena TODOS os pacotes prontos a cada tick.
// 5 pacotes = 100ms de folga: absorve a rajada natural do FFmpeg (medida em
// ~80ms) sem descartar áudio bom, e ainda assim limita o atraso acumulado.
const audioBacklogTarget = 5

// audioSamplesPerFrame: 20ms a 48kHz = 960 amostras por frame Opus. É o passo do
// timestamp RTP reescrito no envio.
const audioSamplesPerFrame = 960

func (s *Session) pumpAudio(stream *audio.RTPStream, track *webrtc.TrackLocalStaticRTP) {
	defer stream.Close()

	// Leitor: drena o socket UDP o mais rápido possível para o buffer não
	// transbordar durante as rajadas do FFmpeg. Nunca bloqueia no pacing.
	queue := make(chan []byte, audioPaceQueueDepth)
	go func() {
		defer close(queue)
		for {
			raw, err := stream.ReadPacket()
			if err != nil {
				log.Printf("[audio] leitura RTP: %v", err)
				return
			}
			select {
			case queue <- raw:
			default:
				// Fila cheia: descarta o pacote mais antigo e enfileira o novo.
				// Áudio em tempo real prefere perder um frame velho a atrasar.
				select {
				case <-queue:
				default:
				}
				select {
				case queue <- raw:
				default:
				}
			}
		}
	}()

	// Escritor: suaviza a rajada do FFmpeg num fluxo de 20ms/pacote SEM depender
	// da resolução do timer do SO.
	//
	// Um time.Ticker(20ms) no Windows dispara só ~24x/s (resolução de ~15.6ms):
	// como cada disparo escoava um pacote, a vazão ficava travada em ~24 pkt/s
	// contra os 50 necessários — metade do áudio se perdia e o som saía
	// entrecortado. Aqui o relógio de saída é ABSOLUTO (nextSlot += 20ms por
	// pacote), então o atraso de um sleep não reduz a vazão: ele apenas faz o
	// pacote seguinte sair imediatamente, e a média se mantém em 50 pkt/s.
	logTicker := time.NewTicker(time.Second)
	defer logTicker.Stop()

	var nextSlot time.Time

	var packets, bytes, dropped int64
	var maxGap time.Duration
	var last time.Time

	// Sequência/timestamp próprios: ver a reescrita no envio. Começam em valores
	// aleatórios como manda o RFC 3550 para não colidir entre sessões.
	audioSeq := uint16(rand.Uint32())
	audioTS := rand.Uint32()

	for {
		// Log é oportunista: não pode atrasar o áudio, então roda entre pacotes.
		select {
		case <-logTicker.C:
			log.Printf("[audio] enviados=%d pkt/s desc=%d taxa=%d KB/s gapMax=%s fila=%d/%d", packets, dropped, bytes/1024, maxGap, len(queue), audioPaceQueueDepth)
			packets = 0
			bytes = 0
			dropped = 0
			maxGap = 0
		default:
		}

		// Bloqueia esperando o próximo pacote: a cadência é ditada pelo FFmpeg
		// (que já produz 50 frames/s de 20ms), não por um timer do SO.
		raw, ok := <-queue
		if !ok {
			return
		}

		// Anti-atraso: se a fila acumulou acima do alvo, o excedente é backlog
		// real (a saída não está acompanhando a entrada) e vira latência
		// permanente. Só nesse caso descartamos, ficando com o pacote mais novo.
		for len(queue) > audioBacklogTarget {
			select {
			case newer, okk := <-queue:
				if !okk {
					return
				}
				dropped++
				raw = newer
			default:
			}
		}

		var pkt rtp.Packet
		if err := pkt.Unmarshal(raw); err != nil {
			log.Printf("[audio] pacote RTP inválido: %v", err)
			continue
		}

		// Como podemos ter descartado excedente, os sequence numbers do FFmpeg
		// ficam com buracos — e um buraco é indistinguível de PERDA para o
		// receptor, que dispara NACK e ocultação de erro (chiado). Reescrevemos
		// sequence e timestamp para um fluxo contínuo: o que sai é sempre
		// consecutivo, avançando 20ms de relógio Opus (48kHz) por pacote.
		pkt.Header.SequenceNumber = audioSeq
		pkt.Header.Timestamp = audioTS
		audioSeq++
		audioTS += audioSamplesPerFrame

		// Pacing por relógio ABSOLUTO: espera só o que falta para o slot do
		// pacote. Um sleep que durma demais (resolução do timer) não reduz a
		// vazão — o próximo pacote sai na hora, e a média fica em 50 pkt/s.
		// Se estamos atrasados, envia imediato e realinha o relógio.
		now := time.Now()
		if nextSlot.IsZero() {
			nextSlot = now
		}
		if wait := nextSlot.Sub(now); wait > 0 {
			if wait > audioFrameDuration {
				wait = audioFrameDuration
			}
			time.Sleep(wait)
			nextSlot = nextSlot.Add(audioFrameDuration)
		} else {
			nextSlot = time.Now().Add(audioFrameDuration)
		}

		if err := track.WriteRTP(&pkt); err != nil {
			log.Printf("[audio] WriteRTP: %v", err)
			return
		}

		sent := time.Now()
		if !last.IsZero() && sent.Sub(last) > maxGap {
			maxGap = sent.Sub(last)
		}
		last = sent
		packets++
		bytes += int64(len(raw))
	}
}

type inputWireMessage struct {
	Type    string    `json:"type"`
	Buttons []bool    `json:"buttons"`
	Axes    []float64 `json:"axes"`
	Code    string    `json:"code"`
	Down    bool      `json:"down"`
	Button  int       `json:"button"`
	DX      int32     `json:"dx"`
	DY      int32     `json:"dy"`
	DeltaY  float64   `json:"deltaY"`
}

type inputStats struct {
	gamepad atomic.Int64
	key     atomic.Int64
	mouse   atomic.Int64
	wheel   atomic.Int64
	lastNs  atomic.Int64
}

func (s *Session) handleInputMessage(raw []byte) error {
	if s.injector == nil {
		return nil
	}

	var msg inputWireMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}

	switch msg.Type {
	case "", "gamepad":
		s.recordInput("gamepad")
		return s.injector.Apply(input.GamepadState{Buttons: msg.Buttons, Axes: msg.Axes})
	case "key":
		s.recordInput("key")
		return s.injector.Command(input.Command{
			Type:   msg.Type,
			Code:   msg.Code,
			Down:   msg.Down,
			Button: msg.Button,
			DX:     msg.DX,
			DY:     msg.DY,
			DeltaY: msg.DeltaY,
		})
	case "mouseMove", "mouseButton":
		s.recordInput("mouse")
		return s.injector.Command(input.Command{
			Type:   msg.Type,
			Code:   msg.Code,
			Down:   msg.Down,
			Button: msg.Button,
			DX:     msg.DX,
			DY:     msg.DY,
			DeltaY: msg.DeltaY,
		})
	case "wheel":
		s.recordInput("wheel")
		return s.injector.Command(input.Command{
			Type:   msg.Type,
			Code:   msg.Code,
			Down:   msg.Down,
			Button: msg.Button,
			DX:     msg.DX,
			DY:     msg.DY,
			DeltaY: msg.DeltaY,
		})
	default:
		return nil
	}
}

func (s *Session) recordInput(kind string) {
	now := time.Now().UnixNano()
	s.input.lastNs.Store(now)
	switch kind {
	case "gamepad":
		s.input.gamepad.Add(1)
	case "key":
		s.input.key.Add(1)
	case "mouse":
		s.input.mouse.Add(1)
	case "wheel":
		s.input.wheel.Add(1)
	}
}

func logInputStats(stats *inputStats, stop <-chan struct{}) {
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-ticker.C:
			gamepad := stats.gamepad.Swap(0)
			key := stats.key.Swap(0)
			mouse := stats.mouse.Swap(0)
			wheel := stats.wheel.Swap(0)
			lastNs := stats.lastNs.Load()
			idle := time.Duration(0)
			if lastNs > 0 {
				idle = time.Since(time.Unix(0, lastNs))
			}
			log.Printf("[input] gamepad=%d/s key=%d/s mouse=%d/s wheel=%d/s idle=%s", gamepad, key, mouse, wheel, idle.Round(time.Millisecond))
		case <-stop:
			return
		}
	}
}

// Tipos de NAL relevantes para o agrupamento por access unit (H.264).
const (
	nalTypeNonIDR = 1 // slice de frame não-IDR (P-frame)
	nalTypeIDR    = 5 // slice de keyframe (IDR)
	nalTypeSEI    = 6
	nalTypeSPS    = 7
	nalTypePPS    = 8
	nalTypeAUD    = 9 // access unit delimiter
)

const annexBStartCode = "\x00\x00\x00\x01"

// paramSetInterval: periodicidade do reenvio de SPS/PPS em frames que não são
// keyframe. Barato (~30 bytes) e garante que um client que entre no meio do
// stream — ou que perca o IDR — consiga inicializar o decoder em ~1s.
const paramSetInterval = time.Second

// staleBacklogFor decide, a partir da CAPACIDADE da fila, quando um P-frame é
// considerado obsoleto e descartado em vez de enviado.
//
// Tem de ser relativo, não uma constante: a profundidade da fila vem do perfil
// de latência (ultra=2, balanced=3, smooth=6 frames). Um limiar fixo de 3
// descartava sem parar justamente no perfil "smooth" — cuja fila é 6 e que
// existe para SUAVIZAR — jogando fora 123 frames numa sessão de 2 minutos e
// produzindo o engasgo que deveria evitar.
//
// O descarte existe só para o caso patológico: a captura trava por contenção de
// GPU e depois despeja o represado de uma vez (medido: fps=378 num segundo).
// Enviar essa avalanche entrega imagem velha e trava a TV. Disparar apenas
// quando a fila está praticamente cheia mantém o comportamento normal intacto.
func staleBacklogFor(queueCap int) int {
	if queueCap <= 0 {
		return staleBacklogFloor
	}
	// Um frame esperando na fila é latência pura: a 60fps, cada posição ocupada
	// custa ~16.7ms na tela. Deixar a fila INTEIRA como espaço operacional
	// (tentativa anterior) fez com que ela vivesse cheia — 6 frames = 100ms
	// fixos de atraso, com gapEnvio medido em 50-83ms. O usuário sentiu como
	// "lag de 1s" somado ao buffer da TV.
	//
	// O equilíbrio é permitir folga para absorver jitter, mas não a fila toda:
	// acima da metade, o excedente é atraso acumulado e sai fora.
	t := queueCap / 2
	if t < staleBacklogFloor {
		t = staleBacklogFloor
	}
	return t
}

// staleBacklogFloor: piso do limiar. Com filas curtas (perfil ultra, fila=2)
// um limiar menor que isto descartaria em operação normal.
const staleBacklogFloor = 3

// pumpVideo lê NAL units do H.264 Annex-B, agrupa-as em access units (frames)
// e entrega cada frame completo ao Pion como um único media.Sample.
//
// Por que agrupar: h264reader.NextNAL() devolve UMA NAL por chamada, mas um
// frame de vídeo é um access unit composto por várias NALs (ex.: SPS+PPS+IDR
// num keyframe). Enviar cada NAL como um sample separado — cada um com a
// duração de um frame inteiro — faz o timestamp RTP avançar errado e o decoder
// do browser congela na primeira imagem. O Duration só pode ser contado uma vez
// por frame, e todas as NALs do mesmo frame compartilham o mesmo timestamp.
//
// Pacing: o FFmpeg/ddagrab entrega frames em RAJADA (gap de chegada medido
// 28-72ms num alvo de 16.6ms). A profundidade da fila e o teto de hold do pacer
// agora vêm do TuningProfile (config.Tuning), derivados do fps — regra única que
// escala para todas as resoluções. Ver internal/config/tuning.go.

type videoPumpStats struct {
	readFrames      atomic.Int64
	sentFrames      atomic.Int64
	keyframes       atomic.Int64
	bytes           atomic.Int64
	maxFrameBytes   atomic.Int64
	maxReadGapNs    atomic.Int64
	maxSendGapNs    atomic.Int64
	maxWriteNs      atomic.Int64
	maxQueueDepth   atomic.Int64
	maxQueueBlockNs atomic.Int64
	droppedFrames   atomic.Int64
	lateFrames      atomic.Int64
}

// superviseVideo mantém a captura viva pelo tempo da sessão, reiniciando o
// FFmpeg quando o pipeline de captura morre.
//
// Por que é necessário: o ddagrab (Desktop Duplication) é derrubado pelo Windows
// quando a swapchain do desktop é recriada — o caso comum é um jogo entrando em
// tela cheia exclusiva ou trocando a resolução/monitor. O FFmpeg reporta
// "Error during demuxing: Generic error in an external library" e encerra.
//
// Antes, pumpVideo simplesmente retornava: a PeerConnection continuava
// conectada, o áudio seguia tocando e o vídeo ficava PRETO para sempre, sem
// nenhuma chance de recuperação a não ser reconectar na mão. Como a sessão
// segue perfeitamente utilizável, o certo é levantar a captura de novo.
func (s *Session) superviseVideo(stream io.ReadCloser, track sampleWriter) {
	attempt := 0
	for {
		s.pumpVideo(stream, track)

		// A sessão acabou (usuário saiu, conexão caiu): não reinicia nada.
		if s.closed.Load() {
			return
		}

		attempt++
		if attempt > captureRestartLimit {
			log.Printf("[capture] captura falhou %d vezes seguidas; desistindo. "+
				"Reconecte para tentar novamente.", attempt)
			return
		}

		delay := captureRestartDelay * time.Duration(attempt)
		if delay > captureRestartMaxDelay {
			delay = captureRestartMaxDelay
		}
		log.Printf("[capture] pipeline de vídeo caiu (jogo em tela cheia ou troca de "+
			"modo de display?); reiniciando em %s (tentativa %d/%d)", delay, attempt, captureRestartLimit)

		select {
		case <-time.After(delay):
		case <-s.done:
			return
		}
		if s.closed.Load() {
			return
		}

		// Sobe uma captura nova. A anterior já se encerrou junto com o FFmpeg.
		newCap := newCapturer(s.cfg)
		newStream, err := newCap.Start(context.Background())
		if err != nil {
			log.Printf("[capture] falha ao reiniciar: %v", err)
			continue
		}

		s.mu.Lock()
		old := s.cap
		s.cap = newCap
		s.mu.Unlock()
		if old != nil {
			old.Stop()
		}

		log.Println("[capture] captura restabelecida")
		stream = newStream
		attempt = 0 // recuperou: zera o contador para futuras quedas
	}
}

const (
	// captureRestartLimit: tentativas seguidas antes de desistir. Sem teto, uma
	// falha permanente (GPU removida, driver caído) viraria um laço infinito de
	// processos FFmpeg.
	captureRestartLimit = 5
	// captureRestartDelay: espera base entre tentativas, multiplicada pelo número
	// da tentativa. Dá tempo ao Windows de terminar a troca de modo de display
	// antes de tentarmos capturar de novo.
	captureRestartDelay    = 700 * time.Millisecond
	captureRestartMaxDelay = 3 * time.Second
)

func (s *Session) pumpVideo(stream io.ReadCloser, track sampleWriter) {
	defer stream.Close()

	tune := s.cfg.Tuning()
	frameDur := time.Second / time.Duration(s.cfg.FPS)
	frames := make(chan encodedFrame, tune.FrameQueueDepth)
	var stats videoPumpStats

	go func() {
		defer close(frames)
		if err := readH264AccessUnits(stream, frameDur, frames, &stats); err != nil {
			log.Printf("[video] leitura H.264: %v", err)
		}
	}()

	if err := writeVideoFrames(frames, track, frameDur, tune.PacerMaxHold, &stats, s.takeParamResend); err != nil {
		log.Printf("[video] WriteSample: %v", err)
	}
}

// takeParamResend consome (e limpa) o pedido de reenvio de parâmetros vindo do
// PLI/FIR do receptor.
func (s *Session) takeParamResend() bool {
	return s.paramResend.Swap(false)
}

// bindWaiter é satisfeito pela track real; permite ao pump saber se o RTP já tem
// para onde ir. Tracks de teste que não implementam a interface são tratadas
// como sempre ligadas.
type bindWaiter interface {
	Bound() bool
}

func trackBound(track sampleWriter) bool {
	if bw, ok := track.(bindWaiter); ok {
		return bw.Bound()
	}
	return true
}

func readH264AccessUnits(stream io.Reader, frameDur time.Duration, out chan encodedFrame, stats *videoPumpStats) error {
	h264, err := h264reader.NewReader(stream)
	if err != nil {
		return err
	}

	var au []byte     // access unit em construção (Annex-B)
	haveVCL := false  // já vimos uma slice (VCL) no AU atual?
	auHasIDR := false // o AU atual contém um keyframe?
	auHasAUD := false // o AU atual começou com AUD; nesse caso AUD marca o frame

	var lastRead time.Time

	// flush publica o access unit acumulado como um frame completo.
	flush := func() {
		if len(au) == 0 || !haveVCL {
			return
		}

		frame := encodedFrame{
			data:     au,
			duration: frameDur,
			keyframe: auHasIDR,
		}
		au = nil

		if stats != nil {
			stats.readFrames.Add(1)
			stats.bytes.Add(int64(len(frame.data)))
			recordMaxInt(&stats.maxFrameBytes, int64(len(frame.data)))
			if frame.keyframe {
				stats.keyframes.Add(1)
			}
			now := time.Now()
			if !lastRead.IsZero() {
				gap := now.Sub(lastRead)
				recordMaxDuration(&stats.maxReadGapNs, gap)
				// Conta os frames que chegaram FORA da cadência (mais de 2x o
				// intervalo esperado). O máximo por segundo sozinho engana: um
				// único pico de 100ms produz a mesma leitura de uma rajada
				// constante, e são situações opostas. O que o olho percebe como
				// "não suave" é a FREQUÊNCIA de frames irregulares, não o pior.
				if gap > 2*frameDur {
					stats.lateFrames.Add(1)
				}
			}
			lastRead = now
		}

		if stats != nil {
			recordMaxInt(&stats.maxQueueDepth, boundedQueueDepth(len(out)+1, cap(out)))
		}
		enqueueStart := time.Now()
		out <- frame
		if stats != nil {
			recordMaxDuration(&stats.maxQueueBlockNs, time.Since(enqueueStart))
		}
		haveVCL = false
		auHasIDR = false
		auHasAUD = false
	}

	for {
		nal, err := h264.NextNAL()
		if err != nil {
			if err == io.EOF {
				log.Println("[video] fim do stream")
				flush()
				return nil
			} else {
				flush()
				return err
			}
		}

		nalType := nal.UnitType
		isVCL := nalType == nalTypeNonIDR || nalType == nalTypeIDR

		// Fronteira de access unit: se o encoder emite AUD, ele é a fonte de
		// verdade. Isso importa para x264, que pode emitir múltiplas slices VCL
		// para o mesmo frame; sem respeitar o AUD, cada slice vira um "frame" e o
		// RTP sai a centenas de fps. Encoders sem AUD continuam no heurístico
		// antigo por nova NAL de parâmetro/slice.
		boundary := nalType == nalTypeAUD ||
			(haveVCL && !auHasAUD && (isVCL || nalType == nalTypeSPS || nalType == nalTypePPS || nalType == nalTypeSEI))
		if boundary {
			flush()
		}

		// Reconstrói o Annex-B (o h264reader remove o start code).
		au = append(au, []byte(annexBStartCode)...)
		au = append(au, nal.Data...)
		if nalType == nalTypeAUD {
			auHasAUD = true
		}
		if isVCL {
			haveVCL = true
		}
		if nalType == nalTypeIDR {
			auHasIDR = true
		}
	}
}

func writeVideoFrames(frames <-chan encodedFrame, track sampleWriter, frameDur, maxHold time.Duration, stats *videoPumpStats, paramResendRequested func() bool) error {
	logTicker := time.NewTicker(time.Second)
	defer logTicker.Stop()

	var lastSend time.Time

	send := func(frame encodedFrame) error {
		start := time.Now()
		if err := track.WriteSample(media.Sample{Data: frame.data, Duration: frame.duration}); err != nil {
			return err
		}
		now := time.Now()
		if stats != nil {
			stats.sentFrames.Add(1)
			recordMaxDuration(&stats.maxWriteNs, now.Sub(start))
			if !lastSend.IsZero() {
				recordMaxDuration(&stats.maxSendGapNs, now.Sub(lastSend))
			}
			lastSend = now
		}
		return nil
	}

	// Anti-jitter de saída (NÃO é o pacing de atraso fixo de [[no-pacing-immediate-send]]):
	// o ddagrab entrega frames com chegada irregular. O pacer SÓ apara frames que
	// chegaram ADIANTADOS em relação ao relógio de saída, e por no máximo meio
	// frameDur (maxHold). NUNCA impõe cadência — tentar impor cadência somava
	// espera em cima do jitter natural do WriteSample (picos de ~50ms medidos) e
	// PIOROU o engasgo, além de adicionar lag de input.
	//
	// Invariante anti-backlog: se a fila tem outros frames esperando (backlog>0),
	// o pipeline está em rajada — drenamos imediato e realinhamos o relógio ao
	// agora. Frame atrasado nunca espera. Latência adicionada em regime: zero
	// quando em dia; no pior caso < maxHold (meio frame).
	var nextSlot time.Time

	// Gate de início: enquanto a track não estiver ligada (Bind), o WriteSample é
	// descartado silenciosamente pelo Pion. O stream é pré-aquecido antes do
	// handshake, então os primeiros frames — INCLUSIVE o IDR inicial que carrega
	// SPS/PPS — cairiam no vazio, e a TV ficaria sem parâmetros de decode (tela
	// preta) até o próximo keyframe do GOP. Guardamos o último SPS/PPS visto e só
	// começamos a emitir a partir de um keyframe, prefixando os parâmetros se o
	// keyframe vier sem eles.
	started := false
	var paramSets []byte
	var lastParamSend time.Time

	// Limiar de obsolescência relativo à fila deste perfil de latência.
	staleBacklog := staleBacklogFor(cap(frames))

	paced := func(frame encodedFrame, backlog int) error {
		if ps := extractParameterSets(frame.data); len(ps) > 0 {
			paramSets = ps
		}

		if !started {
			// Espera a track ligar (durante o pré-aquecimento o RTP não tem para
			// onde ir) E um ponto de entrada válido para o decoder.
			if !trackBound(track) {
				return nil
			}
			// O ponto de entrada TEM de ser um keyframe: um P-frame depende de
			// referências que o decoder não tem, e prefixá-lo com SPS/PPS não muda
			// isso — daria imagem quebrada em vez de imagem nenhuma.
			//
			// O custo de esperar é real (até um GOP inteiro de tela preta), e por
			// isso o GOP foi encurtado — ver gopSecondsFor em tuning.go.
			if !frame.keyframe {
				return nil
			}
			started = true
		}

		// Descarte de frames OBSOLETOS. Quando a captura engasga (contenção de GPU:
		// o ddagrab para de entregar e depois despeja o represado de uma vez —
		// medido fps=378 num segundo, com pico de 114 Mbps num teto de 24), enviar
		// tudo é contraproducente: são imagens do passado que chegam atrasadas,
		// estouram o buffer da TV e travam justamente o que deveriam mostrar.
		// Numa live, o frame mais recente é o único que importa.
		//
		// Só descartamos P-frames: soltar um keyframe quebraria a referência do
		// decoder. E só quando o backlog passa do limiar, para não interferir na
		// operação normal (backlog típico medido: 1-3).
		if backlog > staleBacklog && !frame.keyframe {
			if stats != nil {
				stats.droppedFrames.Add(1)
			}
			return nil
		}

		// Reenvio periódico de SPS/PPS: sem isso, os parâmetros só trafegam junto
		// do IDR (a cada GOP). Se ESSE pacote se perde no Wi-Fi, a TV fica sem como
		// inicializar o decoder e exibe tela preta até o próximo keyframe — ou para
		// sempre, se a perda se repetir. Reemitir a cada ~1s custa ~30 bytes/s e
		// torna o stream auto-recuperável em qualquer ponto de entrada.
		now := time.Now()
		if hasParameterSets(frame.data) {
			// O frame já carrega os parâmetros (keyframe): serve como reenvio e
			// satisfaz qualquer PLI pendente.
			lastParamSend = now
			if paramResendRequested != nil {
				paramResendRequested()
			}
		} else if len(paramSets) > 0 {
			// Só consome o pedido de PLI quando ele pode de fato ser atendido —
			// consumi-lo num frame que já tinha SPS/PPS perderia o sinal.
			forced := paramResendRequested != nil && paramResendRequested()
			if forced || lastParamSend.IsZero() || now.Sub(lastParamSend) >= paramSetInterval {
				frame.data = append(append([]byte(nil), paramSets...), frame.data...)
				lastParamSend = now
			}
		}

		if nextSlot.IsZero() {
			nextSlot = now
		}

		wait := nextSlot.Sub(now)
		if backlog == 0 && wait > 0 {
			if wait > maxHold {
				wait = maxHold
			}
			time.Sleep(wait)
			nextSlot = time.Now().Add(frameDur)
		} else {
			// Rajada (backlog>0) ou frame atrasado: vai imediato, relógio realinha.
			nextSlot = now.Add(frameDur)
		}
		return send(frame)
	}

	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				logVideoPumpStats(stats)
				return nil
			}
			if err := paced(frame, len(frames)); err != nil {
				return err
			}

		case <-logTicker.C:
			logVideoPumpStats(stats)
		}
	}
}

// forEachNAL percorre as NALs de um buffer Annex-B chamando fn(tipo, nal
// completa com start code).
func forEachNAL(au []byte, fn func(nalType byte, nal []byte)) {
	starts := []int{}
	for i := 0; i+3 < len(au); i++ {
		if au[i] == 0 && au[i+1] == 0 && au[i+2] == 1 {
			starts = append(starts, i)
		}
	}
	for idx, start := range starts {
		end := len(au)
		if idx+1 < len(starts) {
			end = starts[idx+1]
		}
		payload := start + 3
		if payload >= end {
			continue
		}
		fn(au[payload]&0x1f, au[start:end])
	}
}

// extractParameterSets devolve SPS+PPS (com start codes) presentes no access
// unit, na ordem em que aparecem.
func extractParameterSets(au []byte) []byte {
	var out []byte
	forEachNAL(au, func(nalType byte, nal []byte) {
		if nalType == nalTypeSPS || nalType == nalTypePPS {
			out = append(out, nal...)
		}
	})
	return out
}

func hasParameterSets(au []byte) bool {
	found := false
	forEachNAL(au, func(nalType byte, _ []byte) {
		if nalType == nalTypeSPS {
			found = true
		}
	})
	return found
}

func recordMaxDuration(target *atomic.Int64, d time.Duration) {
	n := int64(d)
	recordMaxInt(target, n)
}

func recordMaxInt(target *atomic.Int64, n int64) {
	for {
		old := target.Load()
		if n <= old || target.CompareAndSwap(old, n) {
			return
		}
	}
}

func boundedQueueDepth(depth, capacity int) int64 {
	if capacity > 0 && depth > capacity {
		return int64(capacity)
	}
	return int64(depth)
}

func logVideoPumpStats(stats *videoPumpStats) {
	if stats == nil {
		return
	}

	read := stats.readFrames.Swap(0)
	sent := stats.sentFrames.Swap(0)
	keyframes := stats.keyframes.Swap(0)
	bytes := stats.bytes.Swap(0)
	maxFrameBytes := stats.maxFrameBytes.Swap(0)
	maxReadGap := time.Duration(stats.maxReadGapNs.Swap(0))
	maxSendGap := time.Duration(stats.maxSendGapNs.Swap(0))
	maxWrite := time.Duration(stats.maxWriteNs.Swap(0))
	maxQueueDepth := stats.maxQueueDepth.Swap(0)
	maxQueueBlock := time.Duration(stats.maxQueueBlockNs.Swap(0))
	dropped := stats.droppedFrames.Swap(0)
	late := stats.lateFrames.Swap(0)

	if read == 0 && sent == 0 && keyframes == 0 && bytes == 0 {
		return
	}

	log.Printf("[video] lidos=%d fps enviados=%d fps desc=%d atras=%d keyframes=%d taxa=%d KB/s frameMax=%d KB gapMax leitura/envio=%s/%s writeMax=%s filaPicoMax=%d filaBloqMax=%s",
		read, sent, dropped, late, keyframes, bytes/1024, maxFrameBytes/1024, maxReadGap, maxSendGap, maxWrite, maxQueueDepth, maxQueueBlock)
}

// HandleOffer processa a SDP offer do client e devolve a answer.
func (s *Session) HandleOffer(offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
	s.audioOn = offerIncludesAudio(offer.SDP)
	if err := s.pc.SetRemoteDescription(offer); err != nil {
		return webrtc.SessionDescription{}, err
	}

	answer, err := s.pc.CreateAnswer(nil)
	if err != nil {
		return webrtc.SessionDescription{}, err
	}

	// Espera o ICE gathering completar para mandar uma answer "completa"
	// (non-trickle) — mais simples para o MVP.
	gatherComplete := webrtc.GatheringCompletePromise(s.pc)
	if err := s.pc.SetLocalDescription(answer); err != nil {
		return webrtc.SessionDescription{}, err
	}
	<-gatherComplete

	return *s.pc.LocalDescription(), nil
}

func offerIncludesAudio(sdp string) bool {
	return strings.Contains(sdp, "\nm=audio") || strings.Contains(sdp, "\r\nm=audio")
}

// Close encerra a sessão.
func (s *Session) Close() {
	// Sinaliza ANTES de parar a captura: assim o supervisor sabe que o fim do
	// stream é o encerramento da sessão, e não uma queda do ddagrab a recuperar.
	s.closed.Store(true)
	s.closeOnce.Do(func() {
		if s.done != nil {
			close(s.done)
		}
	})

	s.mu.Lock()
	cap := s.cap
	s.mu.Unlock()
	if cap != nil {
		cap.Stop()
	}
	if s.audioCap != nil {
		s.audioCap.Stop()
	}
	if s.pc != nil {
		_ = s.pc.Close()
	}
	if s.onClose != nil {
		s.onClose()
		s.onClose = nil
	}
}
