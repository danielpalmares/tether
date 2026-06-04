package webrtc

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"time"

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
	m := &webrtc.MediaEngine{}
	if err := m.RegisterCodec(webrtc.RTPCodecParameters{
		RTPCodecCapability: webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			// 42c02a = profile_idc 0x42 (Baseline) + profile-iop 0xc0 + level 0x2a (4.2).
			// O byte profile-iop DEVE refletir os constraint_set flags reais do SPS
			// que o NVENC emite, byte a byte. Medição com trace_headers no bitstream:
			//   constraint_set0=1, set1=1, set2=0  ->  0b11000000 = 0xc0  (NÃO 0xe0).
			// O 0xe0 (=42e02a) anuncia constraint_set2=1; o SPS tem set2=0. Decoders
			// de hardware de TV (Samsung Tizen) configuram o pipeline pelo profile-iop
			// da SDP e congelam após o primeiro IDR quando o SPS diverge em 1 bit.
			// Chrome/Android toleram (decode flexível); a TV não.
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42c02a",
		},
		PayloadType: 102,
	}, webrtc.RTPCodecTypeVideo); err != nil {
		return nil, err
	}

	api := webrtc.NewAPI(webrtc.WithMediaEngine(m))
	pc, err := api.NewPeerConnection(webrtc.Configuration{
		// Na LAN não precisamos de STUN/TURN; ICE resolve com candidatos locais.
		ICEServers: []webrtc.ICEServer{},
	})
	if err != nil {
		return nil, err
	}

	s := &Session{pc: pc, cfg: cfg, injector: injector, onClose: onClose}

	// --- Track de vídeo ---
	// A track carrega o MESMO fmtp registrado no MediaEngine. Sem isso o Pion
	// casa a track só pelo MimeType e a answer pode referenciar um payload type
	// sem o profile-level-id correto — ambiguidade que o decoder estrito da
	// Samsung resolve travando. O fmtp idêntico garante que o codec negociado e
	// anunciado na answer seja exatamente o 42c02a que o bitstream produz.
	videoTrack, err := webrtc.NewTrackLocalStaticSample(
		webrtc.RTPCodecCapability{
			MimeType:    webrtc.MimeTypeH264,
			ClockRate:   90000,
			SDPFmtpLine: "level-asymmetry-allowed=1;packetization-mode=1;profile-level-id=42c02a",
		},
		"video", "tether-screen",
	)
	if err != nil {
		return nil, err
	}
	if _, err = pc.AddTrack(videoTrack); err != nil {
		return nil, err
	}

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
// Pacing: NÃO usamos ticker. O FFmpeg já produz frames em tempo real (60fps
// reais, medidos com speed=1x), então a própria cadência de chegada no pipe é o
// relógio. Um time.Ticker artificial acumula ticks atrasados e os dispara em
// rajada quando o NextNAL() (bloqueante) finalmente retorna, gerando jitter e
// travadas. Enviamos cada access unit assim que ele fecha; o Duration serve só
// para o Pion calcular o timestamp RTP, não para regular a saída.
func (s *Session) pumpVideo(stream io.ReadCloser, track *webrtc.TrackLocalStaticSample) {
	defer stream.Close()

	h264, err := h264reader.NewReader(stream)
	if err != nil {
		log.Printf("[video] h264reader: %v", err)
		return
	}

	frameDur := time.Second / time.Duration(s.cfg.FPS)

	var au []byte    // access unit em construção (Annex-B)
	haveVCL := false // já vimos uma slice (VCL) no AU atual?
	auHasIDR := false // o AU atual contém um keyframe?

	// --- instrumentação: medir o que sai do host ---
	var frames, keyframes, bytes int
	lastLog := time.Now()

	// flush envia o access unit acumulado como um único sample e reinicia o
	// buffer. Envio imediato — o pacing vem do FFmpeg.
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
		au = nil
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

		// Fronteira de access unit: um AUD, ou uma nova NAL de
		// parâmetro/slice quando já temos uma slice acumulada, fecha o frame
		// anterior. (O FFmpeg sem AUD nos obriga a inferir pela transição.)
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
