package webrtc

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"time"

	"github.com/pion/interceptor"
	"github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
	"github.com/pion/webrtc/v4/pkg/media/h264reader"

	"tether/internal/capture"
	"tether/internal/config"
	"tether/internal/input"
	"tether/internal/steam"
)

// Session representa uma sessão de streaming com um client.
type Session struct {
	pc        *webrtc.PeerConnection
	cfg       config.StreamConfig
	cap       *capture.Capturer
	injector  input.Injector
	onClose   func()
}

// NewSession cria a peer connection, registra a track de vídeo e o data channel.
func NewSession(cfg config.StreamConfig, injector input.Injector, onClose func()) (*Session, error) {
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

	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(h264Codec, webrtc.RTPCodecTypeVideo); err != nil {
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

	s := &Session{pc: pc, cfg: cfg, injector: injector, onClose: onClose}

	// --- Track de vídeo ---
	// A track carrega o MESMO capability do codec registrado.
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		h264Codec.RTPCodecCapability,
		"video", "tether-screen",
	)
	if err != nil {
		return nil, err
	}
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

	// Pré-aquece a captura JÁ, em paralelo ao handshake WebRTC. O DXGI Desktop
	// Duplication tem ~750ms de custo de inicialização (criar device, primeiro
	// frame). Iniciando aqui — antes de "connected" — o pipeline FFmpeg já está
	// entregando 60fps quando a conexão sobe, eliminando o ramp-up visível no
	// começo do stream (qualidade Steam Link: vídeo fluido desde o frame 1).
	s.cap = capture.New(cfg)
	stream, capErr := s.cap.Start(context.Background())
	if capErr != nil {
		log.Printf("[capture] erro ao pré-aquecer: %v", capErr)
	} else {
		go s.pumpVideo(stream, videoTrack)
	}

	// --- Data channel de input (negociado pelo client) ---
	pc.OnDataChannel(func(dc *webrtc.DataChannel) {
		if dc.Label() != "input" {
			return
		}
		log.Println("[webrtc] data channel de input aberto")
		dc.OnMessage(func(msg webrtc.DataChannelMessage) {
			var st input.GamepadState
			if err := json.Unmarshal(msg.Data, &st); err != nil {
				return
			}
			if err := s.injector.Apply(st); err != nil {
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
		case webrtc.PeerConnectionStateFailed,
			webrtc.PeerConnectionStateClosed,
			webrtc.PeerConnectionStateDisconnected:
			s.Close()
		}
	})

	return s, nil
}

// Tipos de NAL relevantes para o agrupamento por access unit (H.264).
const (
	nalTypeNonIDR  = 1 // slice de frame não-IDR (P-frame)
	nalTypeIDR     = 5 // slice de keyframe (IDR)
	nalTypeSEI     = 6
	nalTypeSPS     = 7
	nalTypePPS     = 8
	nalTypeAUD     = 9 // access unit delimiter
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
// Pacing: NENHUM. O FFmpeg/NVENC em CBR já produz os frames em tempo real (60fps
// reais), então a cadência de chegada no pipe É o relógio. Cada access unit é
// enviado ao Pion ASSIM QUE FECHA — latência mínima, que é o que jogo exige.
//
// Tentativas anteriores de "suavizar" a saída (relógio de apresentação com
// time.Sleep, e canal bufferizado) PIORARAM o resultado e ficam aqui registradas
// para não repetir:
//   - time.Sleep na própria goroutine de leitura: para de drenar o stdout
//     enquanto dorme -> backpressure no pipe -> NVENC estola -> micro-stalls.
//   - canal bufferizado + sleep em goroutine separada: como o encoder produz
//     ~61fps e o pacing liberava a 60, a fila enchia e ficava cheia, virando um
//     jitter buffer de ~8 frames (~130ms) somado a input lag de ~1s.
// A rajada do pipe que o pacing tentava corrigir é pequena; o jitter buffer do
// PLAYER já a absorve. Trocar suavidade marginal por latência é mau negócio aqui.
func (s *Session) pumpVideo(stream io.ReadCloser, track *webrtc.TrackLocalStaticSample) {
	defer stream.Close()

	h264, err := h264reader.NewReader(stream)
	if err != nil {
		log.Printf("[video] h264reader: %v", err)
		return
	}

	frameDur := time.Second / time.Duration(s.cfg.FPS)

	var au []byte     // access unit em construção (Annex-B)
	haveVCL := false  // já vimos uma slice (VCL) no AU atual?
	auHasIDR := false // o AU atual contém um keyframe?

	// instrumentação
	var frames, keyframes, bytes int
	lastLog := time.Now()

	// flush envia o access unit acumulado como um único sample, imediatamente.
	flush := func() error {
		if len(au) == 0 {
			return nil
		}
		err := track.WriteSample(media.Sample{Data: au, Duration: frameDur})
		frames++
		bytes += len(au)
		if auHasIDR {
			keyframes++
		}
		if now := time.Now(); now.Sub(lastLog) >= time.Second {
			log.Printf("[video] saída: %d frames/s, %d keyframes, %d KB/s",
				frames, keyframes, bytes/1024)
			frames, keyframes, bytes = 0, 0, 0
			lastLog = now
		}
		au = au[:0]
		haveVCL = false
		auHasIDR = false
		return err
	}

	for {
		nal, err := h264.NextNAL()
		if err != nil {
			if err == io.EOF {
				log.Println("[video] fim do stream")
			} else {
				log.Printf("[video] NextNAL: %v", err)
			}
			_ = flush()
			return
		}

		nalType := nal.UnitType
		isVCL := nalType == nalTypeNonIDR || nalType == nalTypeIDR

		// Fronteira de access unit: AUD, ou nova NAL de parâmetro/slice quando já
		// temos uma slice acumulada, fecha o frame anterior.
		boundary := nalType == nalTypeAUD ||
			(haveVCL && (isVCL || nalType == nalTypeSPS || nalType == nalTypePPS || nalType == nalTypeSEI))
		if boundary {
			if err := flush(); err != nil {
				log.Printf("[video] WriteSample: %v", err)
				return
			}
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
	if s.pc != nil {
		_ = s.pc.Close()
	}
	if s.onClose != nil {
		s.onClose()
		s.onClose = nil
	}
}
