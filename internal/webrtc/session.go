package webrtc

import (
	"context"
	"encoding/json"
	"io"
	"log"
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
	cfg      config.StreamConfig
	cap      streamCapturer
	audioCap audioCapturer
	injector input.Injector
	input    inputStats
	onClose  func()
}

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

	// Codec H264 ÚNICO do host, definido uma vez e reusado em RegisterCodec, na
	// track e no SetCodecPreferences — os três PRECISAM ser idênticos byte a byte.
	//
	// 42c02a = profile_idc 0x42 (Baseline) + profile-iop 0xc0 + level 0x2a (4.2).
	// O profile-iop reflete os constraint_set flags reais do SPS do NVENC, medidos
	// com trace_headers: constraint_set0=1, set1=1, set2=0 -> 0b11000000 = 0xc0.
	// Decoders de TV (Samsung Tizen) configuram o pipeline pelo profile-level-id da
	// SDP; se o SPS diverge — ou se a SDP anuncia um LEVEL menor que o bitstream —
	// o decoder de hardware estoura e congela. Chrome/Android toleram; a TV não.
	h264Codec := webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42c02a",
			// Os feedbacks PRECISAM viver no próprio codec: o SetCodecPreferences
			// abaixo substitui a lista negociada pelo transceiver por este codec,
			// então um RTCPFeedback nil aqui apagaria o nack/pli que o ConfigureNack
			// registra no MediaEngine. Declarados aqui, nack e nack pli sobrevivem à
			// restrição e aparecem na answer.
			RTCPFeedback: []webrtc.RTCPFeedback{
				{Type: "nack"},
				{Type: "nack", Parameter: "pli"},
			},
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

	// NACK (retransmissão de pacotes perdidos) + RTCP Reports. Sem isto, perder
	// 1 fragmento de keyframe (~50 pacotes a 1080p60) descarta o frame inteiro e
	// a TV infla o jitter buffer defensivamente (buf travado ~80ms — a causa é
	// perda, não config de playout). NÃO usamos RegisterDefaultInterceptors: ele
	// também liga TWCC + header-extensions que incham a SDP, e o decoder Tizen é
	// estrito (vide o bug do profile-level-id). Registramos só nack e nack pli.
	ir := &interceptor.Registry{}
	if err := webrtc.ConfigureNack(m, ir); err != nil {
		return nil, err
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
	audioTrack, err := webrtc.NewTrackLocalStaticRTP(opusCodec.RTPCodecCapability, "audio", "tether-screen")
	if err != nil {
		return nil, err
	}
	audioSender, err := pc.AddTrack(audioTrack)
	if err != nil {
		return nil, err
	}

	s := &Session{pc: pc, cfg: cfg, injector: injector, onClose: onClose}

	// --- Track de vídeo ---
	// A track carrega o MESMO capability do codec registrado.
	videoTrack := newLowLatencyH264Track(h264Codec.RTPCodecCapability, "video", "tether-screen")
	rtpSender, err := pc.AddTrack(videoTrack)
	if err != nil {
		return nil, err
	}

	// FORÇA o transceiver a ofertar/aceitar SOMENTE o 42c02a. Sem isto, o Pion ao
	// processar a offer faz match parcial por MIME type e ADICIONA à negociação
	// todos os perfis H264 que o client oferece (42001f, 42e01f, ...). O 42001f
	// (Level 3.1 = 720p30) acabava na posição preferencial da answer; a TV casava
	// esse payload, configurava o decoder para 720p e recebia o bitstream 1080p60
	// do NVENC — estouro de level -> descarte de frames e congelamento. Restringir
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
	s.cap = newCapturer(cfg)
	stream, capErr := s.cap.Start(context.Background())
	if capErr != nil {
		log.Printf("[capture] erro ao pré-aquecer: %v", capErr)
	} else {
		go s.pumpVideo(stream, videoTrack)
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
			startAudio()
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			s.Close()
		}
	})

	return s, nil
}

func (s *Session) pumpAudio(stream *audio.RTPStream, track *webrtc.TrackLocalStaticRTP) {
	defer stream.Close()

	logTicker := time.NewTicker(time.Second)
	defer logTicker.Stop()

	var packets int64
	var bytes int64
	var maxGap time.Duration
	var last time.Time

	for {
		raw, err := stream.ReadPacket()
		if err != nil {
			log.Printf("[audio] leitura RTP: %v", err)
			return
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

		select {
		case <-logTicker.C:
			log.Printf("[audio] enviados=%d pkt/s taxa=%d KB/s gapMax=%s", packets, bytes/1024, maxGap)
			packets = 0
			bytes = 0
			maxGap = 0
		default:
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
// Pacing: o FFmpeg/ddagrab já entrega frames cadenciados em tempo real. O host
// não deve dormir entre frames aqui; qualquer atraso artificial vira backlog e
// empurra o jitter buffer do client. Mantemos só uma fila curta para absorver
// travadas breves do writer RTP sem acumular latência silenciosa.
const videoFrameQueueDepth = 2

type videoPumpStats struct {
	readFrames      atomic.Int64
	sentFrames      atomic.Int64
	keyframes       atomic.Int64
	bytes           atomic.Int64
	maxReadGapNs    atomic.Int64
	maxSendGapNs    atomic.Int64
	maxWriteNs      atomic.Int64
	maxQueueDepth   atomic.Int64
	maxQueueBlockNs atomic.Int64
}

func (s *Session) pumpVideo(stream io.ReadCloser, track sampleWriter) {
	defer stream.Close()

	frameDur := time.Second / time.Duration(s.cfg.FPS)
	frames := make(chan encodedFrame, videoFrameQueueDepth)
	var stats videoPumpStats

	go func() {
		defer close(frames)
		if err := readH264AccessUnits(stream, frameDur, frames, &stats); err != nil {
			log.Printf("[video] leitura H.264: %v", err)
		}
	}()

	if err := writeVideoFrames(frames, track, &stats); err != nil {
		log.Printf("[video] WriteSample: %v", err)
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

		// Fronteira de access unit: AUD, ou nova NAL de parâmetro/slice quando já
		// temos uma slice acumulada, fecha o frame anterior.
		boundary := nalType == nalTypeAUD ||
			(haveVCL && (isVCL || nalType == nalTypeSPS || nalType == nalTypePPS || nalType == nalTypeSEI))
		if boundary {
			flush()
		}

		// Reconstrói o Annex-B (o h264reader remove o start code).
		au = append(au, []byte(annexBStartCode)...)
		au = append(au, nal.Data...)
		if isVCL {
			haveVCL = true
		}
		if nalType == nalTypeIDR {
			auHasIDR = true
		}
	}
}

func writeVideoFrames(frames <-chan encodedFrame, track sampleWriter, stats *videoPumpStats) error {
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

	for {
		select {
		case frame, ok := <-frames:
			if !ok {
				logVideoPumpStats(stats)
				return nil
			}
			if err := send(frame); err != nil {
				return err
			}

		case <-logTicker.C:
			logVideoPumpStats(stats)
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

func logVideoPumpStats(stats *videoPumpStats) {
	if stats == nil {
		return
	}

	read := stats.readFrames.Swap(0)
	sent := stats.sentFrames.Swap(0)
	keyframes := stats.keyframes.Swap(0)
	bytes := stats.bytes.Swap(0)
	maxReadGap := time.Duration(stats.maxReadGapNs.Swap(0))
	maxSendGap := time.Duration(stats.maxSendGapNs.Swap(0))
	maxWrite := time.Duration(stats.maxWriteNs.Swap(0))
	maxQueueDepth := stats.maxQueueDepth.Swap(0)
	maxQueueBlock := time.Duration(stats.maxQueueBlockNs.Swap(0))

	if read == 0 && sent == 0 && keyframes == 0 && bytes == 0 {
		return
	}

	log.Printf("[video] lidos=%d fps enviados=%d fps keyframes=%d taxa=%d KB/s gapMax leitura/envio=%s/%s writeMax=%s filaMax=%d/%d filaBloqMax=%s",
		read, sent, keyframes, bytes/1024, maxReadGap, maxSendGap, maxWrite, maxQueueDepth, videoFrameQueueDepth, maxQueueBlock)
}

// HandleOffer processa a SDP offer do client e devolve a answer.
func (s *Session) HandleOffer(offer webrtc.SessionDescription) (webrtc.SessionDescription, error) {
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

// Close encerra a sessão.
func (s *Session) Close() {
	if s.cap != nil {
		s.cap.Stop()
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
