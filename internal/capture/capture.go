package capture

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"

	"tether/internal/config"
	"tether/internal/process"
)

// Capturer encapsula um processo FFmpeg que captura a tela via DXGI (ddagrab)
// e codifica em H.264 usando NVENC, emitindo um Annex-B stream no stdout.
type Capturer struct {
	cfg    config.StreamConfig
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

func New(cfg config.StreamConfig) *Capturer {
	return &Capturer{cfg: cfg.Normalize()}
}

// Start inicia o FFmpeg e devolve um Reader do bitstream H.264 Annex-B.
//
// Pipeline preferencial:
//
//	ddagrab (D3D11) -> h264_nvenc -> H.264 Annex-B (stdout)
//
// Se o FFmpeg/GPU não aceitar o caminho D3D11 direto, cai para o fallback
// hwdownload -> nv12 -> h264_nvenc. O preset "p1" + "ull" + zerolatency tuning
// minimiza fila interna do encoder.
func (c *Capturer) Start(ctx context.Context) (io.ReadCloser, error) {
	ctx, c.cancel = context.WithCancel(ctx)

	c.cfg = c.cfg.Normalize()
	var lastErr error
	for _, mode := range pipelineOrder() {
		reader, cmd, err := c.startFFmpeg(ctx, mode)
		if err == nil {
			c.cmd = cmd
			logPipeline(mode)
			logEncodingTuning(c.cfg)
			return &procReader{Reader: reader, c: c}, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, fmt.Errorf("iniciar ffmpeg: %w", ctx.Err())
		}
		if mode == pipelineD3D11 {
			fmt.Fprintf(os.Stderr, "[capture] pipeline d3d11 direto indisponível, tentando fallback CPU: %v\n", err)
		}
	}

	return nil, fmt.Errorf("iniciar ffmpeg (instalado e no PATH?): nenhum pipeline produziu vídeo: %w", lastErr)
}

type pipelineMode string

const (
	pipelineD3D11 pipelineMode = "d3d11"
	pipelineCPU   pipelineMode = "cpu"
)

func pipelineOrder() []pipelineMode {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TETHER_FFMPEG_PIPELINE"))) {
	case "cpu", "download":
		return []pipelineMode{pipelineCPU}
	case "d3d11", "direct", "gpu":
		return []pipelineMode{pipelineD3D11}
	default:
		return []pipelineMode{pipelineD3D11, pipelineCPU}
	}
}

func logPipeline(mode pipelineMode) {
	switch mode {
	case pipelineD3D11:
		fmt.Fprintln(os.Stderr, "[capture] usando pipeline d3d11 direto para NVENC")
	case pipelineCPU:
		fmt.Fprintln(os.Stderr, "[capture] usando fallback hwdownload->CPU->NVENC")
	}
}

func logEncodingTuning(cfg config.StreamConfig) {
	cfg = cfg.Normalize()
	fmt.Fprintf(
		os.Stderr,
		"[capture] h264 %dx%d@%d %dkbps level=%s gop=%d vbv=%dk\n",
		cfg.Width,
		cfg.Height,
		cfg.FPS,
		cfg.Bitrate,
		cfg.H264Level(),
		cfg.H264GOPFrames(),
		cfg.H264VBVBufferKbps(),
	)
}

func (c *Capturer) startFFmpeg(ctx context.Context, mode pipelineMode) (*bufio.Reader, *exec.Cmd, error) {
	args := c.ffmpegArgs(mode)
	cmd := exec.CommandContext(ctx, "ffmpeg", args...)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, fmt.Errorf("stdout pipe: %w", err)
	}
	// stderr do FFmpeg vai pro log do processo pai pra debug (erros de
	// captura/encoder ficam visíveis em vez de sumirem).
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, nil, err
	}
	if err := process.BoostPID(cmd.Process.Pid); err != nil {
		fmt.Fprintf(os.Stderr, "[capture] aviso: não foi possível elevar prioridade do FFmpeg: %v\n", err)
	}

	// Confirma que o pipeline escolhido realmente produz H.264. O FFmpeg pode
	// iniciar e só falhar ao abrir o encoder depois; esperar o primeiro byte
	// permite cair para o fallback sem devolver um stream morto ao WebRTC.
	reader := bufio.NewReaderSize(stdout, 64<<10)
	ready := make(chan error, 1)
	go func() {
		_, err := reader.Peek(1)
		ready <- err
	}()

	select {
	case err := <-ready:
		if err != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
			return nil, nil, err
		}
		return reader, cmd, nil
	case <-time.After(3 * time.Second):
		_ = cmd.Process.Kill()
		<-ready
		_ = cmd.Wait()
		return nil, nil, fmt.Errorf("timeout esperando primeiro byte do stream")
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		<-ready
		_ = cmd.Wait()
		return nil, nil, ctx.Err()
	}
}

func (c *Capturer) ffmpegArgs(mode pipelineMode) []string {
	gop := c.cfg.H264GOPFrames()

	// VBV buffer mínimo. Quanto menor o bufsize,
	// menos o encoder "guarda" antes de emitir — reduz a latência de saída do
	// NVENC além de eliminar o ramp-up inicial. 4K usa um VBV ainda mais curto
	// para impedir IDRs muito grandes em 40Mbps+.
	bufsize := c.cfg.H264VBVBufferKbps()

	input := fmt.Sprintf(
		"ddagrab=output_idx=%d:framerate=%d:video_size=%dx%d:dup_frames=1",
		c.cfg.Display,
		c.cfg.FPS,
		c.cfg.Width,
		c.cfg.Height,
	)

	args := []string{
		"-hide_banner", "-loglevel", "error",

		// --- flags globais de baixa latência na entrada ---
		"-fflags", "nobuffer", // não acumula pacotes no demuxer
		"-flags", "low_delay", // pipeline de decode/demux em low delay
		"-probesize", "32", // não fica "provando" o input antes de começar

		// --- entrada: captura de tela DXGI (ddagrab via lavfi) ---
		"-f", "lavfi",
		"-i", input,
	}

	if mode == pipelineCPU {
		args = append(args,
			// Fallback compatível: baixa o frame do d3d11 pra CPU e converte pra
			// nv12. O caminho preferencial evita esta cópia.
			"-vf", "hwdownload,format=bgra,format=nv12",
		)
	}

	args = append(args,
		// --- encoder NVENC low-latency ---
		"-c:v", "h264_nvenc",
		"-preset", "p1", // mais rápido
		"-tune", "ull", // ultra low latency
		// Constrained Baseline + level compatível com a resolução: casa com o SDP
		// (profile-level-id dinâmico; profile-iop 0xc0 medido no SPS via
		// trace_headers). NVENC por padrão emite Main profile
		// (CABAC); decoders rígidos de TV (Tizen/webOS/Android TV) travam na
		// imagem quando o stream diverge do profile/level anunciado no SDP.
		// Baseline (sem CABAC, sem B-frames) é o denominador comum compatível.
		"-profile:v", "baseline",
		"-level", c.cfg.H264Level(),
		"-rc", "cbr",
		"-b:v", fmt.Sprintf("%dk", c.cfg.Bitrate),
		"-maxrate", fmt.Sprintf("%dk", c.cfg.Bitrate),
		"-bufsize", fmt.Sprintf("%dk", bufsize),
		"-g", fmt.Sprintf("%d", gop),
		"-bf", "0", // sem B-frames (latência)
		"-delay", "0", // sem reordenação/atraso de saída do encoder
		"-rc-lookahead", "0", // NVENC não segura frames analisando o futuro
		"-surfaces", "2", // limita a fila interna do NVENC
		"-multipass", "disabled",
		"-strict_gop", "1",
		// zerolatency: desliga o atraso interno de 1 quadro do rate control do
		// NVENC. Cada frame codificado é emitido imediatamente, sem o "pipeline
		// delay" que o encoder normalmente mantém para suavizar bitrate. Essencial
		// para streaming interativo em LAN.
		"-zerolatency", "1",
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
	)

	return args
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
