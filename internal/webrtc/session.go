package webrtc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/pion/interceptor"
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
	audioCap audioCapturer
	injector input.Injector
	input    inputStats
	audioOn  bool
	onClose  func()

	// onConfigSaved persiste uma config aplicada com sucesso via hot-swap
	// (injetado pelo signaling server — a Session não conhece o disco).
	onConfigSaved func(config.StreamConfig)

	// mu protege o estado do pipeline de vídeo (cfg/cap/videoDone/closed)
	// contra corrida entre Reconfigure (hot-swap) e Close.
	mu        sync.Mutex
	cfg       config.StreamConfig
	cap       streamCapturer
	videoDone chan struct{}
	closed    bool

	videoTrack *lowLatencyH264Track

	// dc é o data channel "input" — bidirecional: input client->host e
	// controle/status host->client.
	dc            atomic.Pointer[webrtc.DataChannel]
	reconfiguring atomic.Bool
}

// ErrNeedsRestart sinaliza que a mudança pedida não cabe num hot-swap do
// encoder (ex.: troca de codec) e exige fechar a sessão e renegociar.
var ErrNeedsRestart = errors.New("mudança de config exige reiniciar a sessão")

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
// onConfigSaved é chamado após um hot-swap de config bem-sucedido para o caller
// persistir/propagar a nova config (pode ser nil).
func NewSession(cfg config.StreamConfig, injector input.Injector, onClose func(), onConfigSaved func(config.StreamConfig)) (*Session, error) {
	cfg = cfg.Normalize()
	videoNack := videoNackEnabled()

	// Codec H264 ÚNICO do host, definido uma vez e reusado em RegisterCodec, na
	// track e no SetCodecPreferences — os três PRECISAM ser idênticos byte a byte.
	//
	// profile-level-id FIXO no level máximo do pipeline:
	// 42c034 = Baseline + profile-iop 0xc0 + level 5.2 (2160p60)
	// O profile-iop reflete os constraint_set flags reais do SPS H.264 Baseline,
	// medidos com trace_headers: constraint_set0=1, set1=1, set2=0 -> 0b11000000 = 0xc0.
	// Decoders de TV (Samsung Tizen) configuram o pipeline pelo profile-level-id da
	// SDP; se o SPS diverge — ou se a SDP anuncia um LEVEL menor que o bitstream —
	// o decoder de hardware estoura e congela. Chrome/Android toleram; a TV não.
	// Anunciar SEMPRE o level máximo (capacidade, não conteúdo) é seguro e permite
	// o hot-swap de resolução sem renegociar a SDP (ver Reconfigure).
	profileLevelID := config.H264MaxProfileLevelID()
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

	s := &Session{pc: pc, cfg: cfg, injector: injector, onClose: onClose, onConfigSaved: onConfigSaved}

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
		buf := make([]byte, 1500)
		for {
			if _, _, rtcpErr := rtpSender.Read(buf); rtcpErr != nil {
				return
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
	s.videoTrack = videoTrack
	if capErr := s.startVideoPipeline(cfg); capErr != nil {
		log.Printf("[capture] erro ao pré-aquecer: %v", capErr)
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

	// --- Data channels (negociados pelo client) ---
	// "input": unreliable/unordered (perder um frame de gamepad é melhor que
	// atrasá-lo). "control": confiável — config/ping client->host e
	// respostas/status host->client NÃO podem se perder.
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		switch dc.Label() {
		case "input":
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
		case "control":
			log.Println("[webrtc] data channel de controle aberto")
			s.dc.Store(dc)
			dc.OnClose(func() {
				s.dc.CompareAndSwap(dc, nil)
				log.Println("[webrtc] data channel de controle fechado")
			})
			dc.OnMessage(func(msg webrtc.DataChannelMessage) {
				if err := s.handleInputMessage(msg.Data); err != nil {
					log.Printf("[control] apply: %v", err)
				}
			})
		}
	})

	pc.OnConnectionStateChange(func(state webrtc.PeerConnectionState) {
		log.Printf("[webrtc] estado: %s", state)
		switch state {
		case webrtc.PeerConnectionStateConnected:
			// O track acabou de fazer bind: reinicia o FFmpeg para o stream
			// abrir com IDR+SPS/PPS que o client realmente recebe (o IDR do
			// pré-aquecimento foi descartado antes do bind; com intra-refresh
			// não existe outro keyframe — sem isto, tela preta).
			go func() {
				if err := s.restartVideoPipeline(); err != nil {
					log.Printf("[webrtc] restart do pipeline ao conectar: %v", err)
				}
			}()
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

// startVideoPipeline cria o capturer e inicia o pump de vídeo para cfg,
// escrevendo no track RTP já existente. Chamado no NewSession (pré-aquecimento,
// sessão ainda não publicada) e no Reconfigure (hot-swap, sob s.mu).
func (s *Session) startVideoPipeline(cfg config.StreamConfig) error {
	capturer := newCapturer(cfg)
	stream, err := capturer.Start(context.Background())
	if err != nil {
		return err
	}
	done := make(chan struct{})
	s.cap = capturer
	s.videoDone = done
	go func() {
		s.pumpVideo(cfg, stream, s.videoTrack)
		close(done)
		s.maybeRestartAfterPumpExit(done)
	}()
	return nil
}

// maybeRestartAfterPumpExit religa a captura quando o pump morre SOZINHO —
// ddagrab perde o device na transição de fullscreen exclusivo (fechar/abrir
// jogo) e o FFmpeg cai; sem isto a tela fica preta para sempre. Saídas
// deliberadas (Reconfigure/Close) trocam s.videoDone ou marcam closed antes,
// então não disparam o restart.
func (s *Session) maybeRestartAfterPumpExit(done chan struct{}) {
	s.mu.Lock()
	unexpected := s.videoDone == done && !s.closed
	s.mu.Unlock()
	if !unexpected {
		return
	}
	log.Println("[video] captura terminou inesperadamente (provável transição de fullscreen); religando em 1s")
	time.Sleep(time.Second)
	if err := s.restartVideoPipeline(); err != nil {
		log.Printf("[video] religamento da captura falhou: %v", err)
	}
}

// Reconfigure troca o encoder a quente: mata o FFmpeg atual e sobe outro com a
// nova config, mantendo a peer connection e o track RTP vivos. O novo stream
// abre com IDR+SPS/PPS (-forced-idr + dump_extra), então o decoder absorve os
// novos parâmetros — inclusive resolução — sem renegociar a SDP (que anuncia
// sempre o level máximo; ver NewSession).
func (s *Session) Reconfigure(cfg config.StreamConfig) error {
	cfg = cfg.Normalize()

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("sessão encerrada")
	}
	if cfg.Codec != s.cfg.Codec {
		// Trocar o encoder muda o caminho do pipeline (GPU vs CPU) e os
		// parâmetros de transporte derivados; mais seguro renegociar do zero.
		return ErrNeedsRestart
	}
	if cfg == s.cfg {
		return nil
	}
	return s.swapPipelineLocked(cfg)
}

// restartVideoPipeline reinicia o FFmpeg com a config corrente para emitir
// IDR+SPS/PPS frescos COM o track já bound. Necessário ao conectar: o
// pré-aquecimento começa antes do handshake, e com intra-refresh o ÚNICO
// IDR do stream é o frame 0 — que o Pion descarta enquanto o track não está
// conectado. Sem este restart o decoder do client nunca recebe headers e a
// tela fica preta.
func (s *Session) restartVideoPipeline() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return errors.New("sessão encerrada")
	}
	return s.swapPipelineLocked(s.cfg)
}

// swapPipelineLocked troca o pipeline de vídeo (caller segura s.mu): para o
// FFmpeg atual, ESPERA o pump drenar — dois pumps nunca escrevem intercalado
// no mesmo track — e sobe o novo com cfg.
func (s *Session) swapPipelineLocked(cfg config.StreamConfig) error {
	if s.cap != nil {
		s.cap.Stop()
	}
	if s.videoDone != nil {
		select {
		case <-s.videoDone:
		case <-time.After(5 * time.Second):
			return errors.New("timeout drenando o pipeline de vídeo atual")
		}
	}

	old := s.cfg
	s.cfg = cfg
	if err := s.startVideoPipeline(cfg); err != nil {
		s.cfg = old
		if rbErr := s.startVideoPipeline(old); rbErr != nil {
			return fmt.Errorf("troca de pipeline falhou (%w) e rollback também (%v)", err, rbErr)
		}
		return err
	}
	log.Printf("[webrtc] pipeline de vídeo reiniciado: %dx%d@%dfps %dkbps", cfg.Width, cfg.Height, cfg.FPS, cfg.Bitrate)
	return nil
}

// Config devolve a config corrente da sessão.
func (s *Session) Config() config.StreamConfig {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// sendToClient envia uma mensagem JSON host->client pelo data channel de input.
// Silencioso se o canal ainda não abriu — status é best-effort.
func (s *Session) sendToClient(v any) {
	dc := s.dc.Load()
	if dc == nil {
		return
	}
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	if err := dc.Send(data); err != nil {
		log.Printf("[webrtc] envio host->client: %v", err)
	}
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

// audioFrameDuration é a cadência alvo de saída do áudio — DEVE casar com o
// frame_duration do Opus em internal/audio (hoje 10ms). O FFmpeg empacota os
// frames, mas o muxer RTP os emite em RAJADA —
// vários pacotes colados (<2ms) seguidos de silêncio de ~90ms (medido: 77% dos
// pacotes saem agrupados). Essa rajada infla o jitter buffer de áudio da TV e,
// via sincronismo A/V, arrasta o vídeo junto (lip-sync). Nenhuma flag do FFmpeg
// corrige (testadas max_delay/muxdelay/muxpreload/avioflags=direct/pkt_size). A
// correção é re-pacear no host: ler o socket sem bloquear e escrever cada pacote
// no ritmo de 20ms.
const audioFrameDuration = 10 * time.Millisecond

// audioPaceQueueDepth é a folga máxima antes de descartar. Mantém latência baixa:
// se a fila passa disto, preferimos soltar o pacote mais antigo a acumular atraso.
const audioPaceQueueDepth = 8

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

	// Escritor: emite em cadência fixa de 20ms, transformando a rajada do FFmpeg
	// num fluxo suave que a TV consome sem inflar o jitter buffer.
	ticker := time.NewTicker(audioFrameDuration)
	defer ticker.Stop()
	logTicker := time.NewTicker(time.Second)
	defer logTicker.Stop()

	var packets, bytes int64
	var maxGap time.Duration
	var last time.Time

	for {
		select {
		case <-ticker.C:
			var raw []byte
			select {
			case r, ok := <-queue:
				if !ok {
					return
				}
				raw = r
			default:
				// Sem pacote pronto neste tick: silêncio breve, sem stall.
				continue
			}
			var pkt rtp.Packet
			if err := pkt.Unmarshal(raw); err != nil {
				log.Printf("[audio] pacote RTP inválido: %v", err)
				continue
			}
			if err := track.WriteRTP(&pkt); err != nil {
				log.Printf("[audio] WriteRTP: %v", err)
				return
			}

			now := time.Now()
			if !last.IsZero() && now.Sub(last) > maxGap {
				maxGap = now.Sub(last)
			}
			last = now
			packets++
			bytes += int64(len(raw))

		case <-logTicker.C:
			log.Printf("[audio] enviados=%d pkt/s taxa=%d KB/s gapMax=%s fila=%d/%d", packets, bytes/1024, maxGap, len(queue), audioPaceQueueDepth)
			packets = 0
			bytes = 0
			maxGap = 0
		}
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

	// --- controle (type=config/configGet/ping) ---
	Width   int     `json:"width"`
	Height  int     `json:"height"`
	FPS     int     `json:"fps"`
	Bitrate int     `json:"bitrate"` // kbps
	T       float64 `json:"t"`       // timestamp do ping (eco no pong)

	// --- telemetria do decoder do client (type=stats), p/ diagnóstico no log
	// do host sem depender do HUD da TV ---
	RTT    float64 `json:"rtt"`    // ms (candidate-pair)
	Buf    float64 `json:"buf"`    // jitterBufferDelay por frame emitido, ms
	Tgt    float64 `json:"tgt"`    // jitterBufferTargetDelay por frame, ms
	Jit    float64 `json:"jit"`    // jitter RTP, ms
	DecFPS float64 `json:"decfps"` // frames decodificados/s
	Lost   float64 `json:"lost"`   // pacotes perdidos/s
	Drop   float64 `json:"drop"`   // frames descartados/s
}

type inputStats struct {
	gamepad atomic.Int64
	key     atomic.Int64
	mouse   atomic.Int64
	wheel   atomic.Int64
	lastNs  atomic.Int64
}

func (s *Session) handleInputMessage(raw []byte) error {
	var msg inputWireMessage
	if err := json.Unmarshal(raw, &msg); err != nil {
		return err
	}

	switch msg.Type {
	case "stats":
		// Telemetria do decoder remoto: a visão da TV (buffer/jitter/fps) ao
		// lado dos stats de envio no mesmo log — fecha o ciclo de diagnóstico.
		log.Printf("[client] rtt=%.0fms buf=%.0fms tgt=%.0fms jit=%.1fms fps=%.0f lost=%.0f/s drop=%.0f/s",
			msg.RTT, msg.Buf, msg.Tgt, msg.Jit, msg.DecFPS, msg.Lost, msg.Drop)
		return nil
	case "ping":
		// RTT de aplicação para o teste de rede do client.
		s.sendToClient(map[string]any{"type": "pong", "t": msg.T})
		return nil
	case "configGet":
		s.sendToClient(map[string]any{"type": "configResult", "ok": true, "config": s.Config()})
		return nil
	case "config":
		s.applyClientConfig(msg)
		return nil
	}

	if s.injector == nil {
		return nil
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

// applyClientConfig trata o pedido de config vindo do client: hot-swap do
// encoder em goroutine própria — Reconfigure leva ~1-2s (restart do FFmpeg) e
// não pode bloquear o callback do data channel, que entrega o input na mesma
// fila. Pedidos sobrepostos são rejeitados.
func (s *Session) applyClientConfig(msg inputWireMessage) {
	if !s.reconfiguring.CompareAndSwap(false, true) {
		s.sendToClient(map[string]any{"type": "configResult", "ok": false, "error": "reconfiguração em andamento"})
		return
	}
	go func() {
		defer s.reconfiguring.Store(false)

		cfg := s.Config()
		if msg.Width > 0 {
			cfg.Width = msg.Width
		}
		if msg.Height > 0 {
			cfg.Height = msg.Height
		}
		if msg.FPS > 0 {
			cfg.FPS = msg.FPS
		}
		if msg.Bitrate > 0 {
			cfg.Bitrate = msg.Bitrate
		}
		cfg = cfg.Normalize()

		if err := s.Reconfigure(cfg); err != nil {
			log.Printf("[webrtc] config do client rejeitada: %v", err)
			s.sendToClient(map[string]any{"type": "configResult", "ok": false, "error": err.Error()})
			return
		}
		if s.onConfigSaved != nil {
			s.onConfigSaved(cfg)
		}
		s.sendToClient(map[string]any{"type": "configResult", "ok": true, "config": cfg})
	}()
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
}

// pumpVideo recebe a cfg do SEU pipeline por parâmetro (não lê s.cfg): durante
// um hot-swap o pump antigo continua drenando com o tuning antigo enquanto a
// sessão já aponta para a config nova.
func (s *Session) pumpVideo(cfg config.StreamConfig, stream io.ReadCloser, track sampleWriter) {
	defer stream.Close()

	tune := cfg.Tuning()
	frameDur := time.Second / time.Duration(cfg.FPS)
	frames := make(chan encodedFrame, tune.FrameQueueDepth)
	var stats videoPumpStats

	go func() {
		defer close(frames)
		if err := readH264AccessUnits(stream, frameDur, frames, &stats); err != nil {
			log.Printf("[video] leitura H.264: %v", err)
		}
	}()

	monitor := &overloadMonitor{targetFPS: cfg.FPS, notify: func(overloaded bool, capturedFPS, targetFPS int) {
		status := "ok"
		if overloaded {
			status = "overloaded"
		}
		log.Printf("[video] host %s: captura %dfps de %dfps alvo", status, capturedFPS, targetFPS)
		s.sendToClient(map[string]any{
			"type":        "hostStatus",
			"status":      status,
			"capturedFps": capturedFPS,
			"targetFps":   targetFPS,
		})
	}}

	if err := writeVideoFrames(frames, track, frameDur, tune.PacerMaxHold, &stats, monitor); err != nil {
		log.Printf("[video] WriteSample: %v", err)
	}
}

// overloadMonitor detecta captura/encoder saturado — a assinatura conhecida de
// GPU sob carga é o DWM compondo irregular e o ddagrab entregando abaixo do
// alvo de forma SUSTENTADA (queda pontual é só cena pesada). Critério: lidos <
// 85% do fps alvo por 3 ticks de 1s consecutivos. Notifica o client na entrada
// e na saída do estado (uma vez cada, sem spam).
type overloadMonitor struct {
	targetFPS  int
	lowTicks   int
	overloaded bool
	notify     func(overloaded bool, capturedFPS, targetFPS int)
}

func (m *overloadMonitor) observe(readFPS int64) {
	if m == nil || m.targetFPS <= 0 || m.notify == nil {
		return
	}
	if readFPS < int64(m.targetFPS)*85/100 {
		m.lowTicks++
	} else {
		m.lowTicks = 0
		if m.overloaded {
			m.overloaded = false
			m.notify(false, int(readFPS), m.targetFPS)
		}
	}
	if !m.overloaded && m.lowTicks >= 3 {
		m.overloaded = true
		m.notify(true, int(readFPS), m.targetFPS)
	}
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
				recordMaxDuration(&stats.maxReadGapNs, now.Sub(lastRead))
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

func writeVideoFrames(frames <-chan encodedFrame, track sampleWriter, frameDur, maxHold time.Duration, stats *videoPumpStats, monitor *overloadMonitor) error {
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

	paced := func(frame encodedFrame, backlog int) error {
		now := time.Now()
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
			read := logVideoPumpStats(stats)
			monitor.observe(read)
		}
	}
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

// logVideoPumpStats loga e zera os contadores do tick; devolve os frames LIDOS
// no intervalo (entrada do overloadMonitor).
func logVideoPumpStats(stats *videoPumpStats) int64 {
	if stats == nil {
		return 0
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

	if read == 0 && sent == 0 && keyframes == 0 && bytes == 0 {
		return read
	}

	log.Printf("[video] lidos=%d fps enviados=%d fps keyframes=%d taxa=%d KB/s frameMax=%d KB gapMax leitura/envio=%s/%s writeMax=%s filaPicoMax=%d filaBloqMax=%s",
		read, sent, keyframes, bytes/1024, maxFrameBytes/1024, maxReadGap, maxSendGap, maxWrite, maxQueueDepth, maxQueueBlock)
	return read
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
	s.mu.Lock()
	alreadyClosed := s.closed
	s.closed = true
	capturer := s.cap
	onClose := s.onClose
	s.onClose = nil
	s.mu.Unlock()
	if alreadyClosed {
		return
	}

	if capturer != nil {
		capturer.Stop()
	}
	if s.audioCap != nil {
		s.audioCap.Stop()
	}
	if s.pc != nil {
		_ = s.pc.Close()
	}
	if onClose != nil {
		onClose()
	}
}
