package capture

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"

	"tether/internal/config"
)

// Capturer encapsula um processo FFmpeg que captura a tela via DXGI (ddagrab)
// e codifica em H.264 usando NVENC, emitindo um Annex-B stream no stdout.
type Capturer struct {
	cfg    config.StreamConfig
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

func New(cfg config.StreamConfig) *Capturer {
	return &Capturer{cfg: cfg}
}

// Start inicia o FFmpeg e devolve um Reader do bitstream H.264 Annex-B.
//
// Pipeline:
//   ddagrab (captura DXGI da GPU) -> hwupload -> h264_nvenc -> H.264 Annex-B (stdout)
//
// O preset "p1" + "ll" (low latency) + zerolatency tuning minimiza o atraso.
func (c *Capturer) Start(ctx context.Context) (io.ReadCloser, error) {
	ctx, c.cancel = context.WithCancel(ctx)

	// GOP de 1s: keyframe a cada segundo. Em WebRTC na LAN não há retransmissão
	// de pacote perdido aqui; um GOP curto garante recuperação rápida sem
	// depender de PLI, ao custo de banda aceitável a 20Mbps.
	gop := fmt.Sprintf("%d", c.cfg.FPS)
	if gop == "0" {
		gop = "60"
	}

	// VBV buffer enxuto: um quarto de segundo de bitrate. Quanto menor o
	// bufsize, menos o encoder "guarda" antes de emitir — elimina o ramp-up de
	// vários segundos que enchia um buffer grande no início do stream.
	bufsize := c.cfg.Bitrate / 4
	if bufsize == 0 {
		bufsize = c.cfg.Bitrate
	}

	args := []string{
		"-hide_banner", "-loglevel", "error",

		// --- flags globais de baixa latência na entrada ---
		"-fflags", "nobuffer",   // não acumula pacotes no demuxer
		"-flags", "low_delay",   // pipeline de decode/demux em low delay
		"-probesize", "32",      // não fica "provando" o input antes de começar

		// --- entrada: captura de tela DXGI (ddagrab via lavfi) ---
		"-f", "lavfi",
		"-i", fmt.Sprintf("ddagrab=output_idx=%d:framerate=%d:dup_frames=1", c.cfg.Display, c.cfg.FPS),

		// baixa o frame do d3d11 pra CPU e converte pra nv12 (formato do nvenc)
		"-vf", "hwdownload,format=bgra,format=nv12",

		"-r", fmt.Sprintf("%d", c.cfg.FPS),

		// --- encoder NVENC low-latency ---
		"-c:v", "h264_nvenc",
		"-preset", "p1", // mais rápido
		"-tune", "ll", // low latency
		// Constrained Baseline + Level 4.2: casa com o SDP (profile-level-id
		// 42c02a — profile-iop 0xc0, medido no SPS via trace_headers) e comporta
		// 1920x1080@60. NVENC por padrão emite Main profile
		// (CABAC); decoders rígidos de TV (Tizen/webOS/Android TV) travam na
		// imagem quando o stream diverge do profile/level anunciado no SDP.
		// Baseline (sem CABAC, sem B-frames) é o denominador comum compatível
		// com qualquer hardware de TV. Level 4.2 cobre 1080p60 (3.1 só dá 720p).
		"-profile:v", "baseline",
		"-level", "4.2",
		"-rc", "cbr",
		"-b:v", fmt.Sprintf("%dk", c.cfg.Bitrate),
		"-maxrate", fmt.Sprintf("%dk", c.cfg.Bitrate),
		"-bufsize", fmt.Sprintf("%dk", bufsize),
		"-g", gop, // GOP de 1s
		"-bf", "0", // sem B-frames (latência)
		"-delay", "0", // sem reordenação/atraso de saída do encoder
		"-rc-lookahead", "0", // NVENC não segura frames analisando o futuro
		"-no-scenecut", "1",
		// força um keyframe (IDR) logo no primeiro frame -> o client tem o que
		// decodificar imediatamente, sem esperar o primeiro GOP.
		"-forced-idr", "1",
		// emite access unit delimiters (NAL type 9) -> fronteira de frame
		// inequívoca para o agrupador do lado Go.
		"-aud", "1",
		// força SPS/PPS antes de cada keyframe (idempotência se o client
		// perder o início do stream ou um IDR).
		"-bsf:v", "dump_extra",
		// baixa latência de saída: não enche o buffer antes de despejar.
		"-flush_packets", "1",

		// --- saída: Annex-B cru pro Pion samplear ---
		"-f", "h264",
		"-",
	}

	c.cmd = exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := c.cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// stderr do FFmpeg vai pro log do processo pai pra debug (erros de
	// captura/encoder ficam visíveis em vez de sumirem).
	c.cmd.Stderr = os.Stderr

	if err := c.cmd.Start(); err != nil {
		return nil, fmt.Errorf("iniciar ffmpeg (instalado e no PATH?): %w", err)
	}

	// Buffer pequeno (64KB): o suficiente para leitura eficiente sem reter
	// frames. Um buffer grande adicionaria latência segurando vídeo já pronto.
	return &procReader{Reader: bufio.NewReaderSize(stdout, 64<<10), c: c}, nil
}

// Stop encerra o FFmpeg.
func (c *Capturer) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

type procReader struct {
	io.Reader
	c *Capturer
}

func (p *procReader) Close() error {
	p.c.Stop()
	return nil
}
