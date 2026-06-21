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
// e codifica em H.264, emitindo um Annex-B stream no stdout.
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
// Se o FFmpeg/GPU não aceitar o caminho D3D11 direto, a sessão falha em vez de
// cair silenciosamente para hwdownload -> CPU. Esse fallback é útil para
// diagnóstico, mas em notebooks híbridos custa caro e mascara o problema real.
// No modo universal/teste, usa hwdownload -> yuv420p -> libx264 para não
// depender do fabricante da GPU.
func (c *Capturer) Start(ctx context.Context) (io.ReadCloser, error) {
	ctx, c.cancel = context.WithCancel(ctx)

	c.cfg = c.cfg.Normalize()
	var lastErr error
	for _, mode := range pipelineOrder(c.cfg.Codec) {
		reader, cmd, err := c.startFFmpeg(ctx, mode)
		if err == nil {
			c.cmd = cmd
			logPipeline(mode, c.cfg.Codec)
			logEncodingTuning(c.cfg)
			return &procReader{Reader: reader, c: c}, nil
		}
		lastErr = err
		if ctx.Err() != nil {
			return nil, fmt.Errorf("iniciar ffmpeg: %w", ctx.Err())
		}
		if mode == pipelineD3D11 {
			if allowsCPUFallback(c.cfg.Codec) {
				fmt.Fprintf(os.Stderr, "[capture] pipeline d3d11 direto indisponível, tentando fallback CPU: %v\n", err)
			} else {
				fmt.Fprintf(os.Stderr, "[capture] pipeline d3d11 direto indisponível: %v\n", err)
			}
		}
	}

	return nil, fmt.Errorf("iniciar ffmpeg (instalado e no PATH?): nenhum pipeline produziu vídeo: %w", lastErr)
}

type pipelineMode string

const (
	pipelineD3D11 pipelineMode = "d3d11"
	pipelineCPU   pipelineMode = "cpu"
)

func pipelineOrder(codec string) []pipelineMode {
	if codec == config.CodecH264X264 {
		return []pipelineMode{pipelineCPU}
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TETHER_FFMPEG_PIPELINE"))) {
	case "cpu", "download":
		return []pipelineMode{pipelineCPU}
	case "auto", "fallback":
		return []pipelineMode{pipelineD3D11, pipelineCPU}
	case "d3d11", "direct", "gpu":
		return []pipelineMode{pipelineD3D11}
	default:
		return []pipelineMode{pipelineD3D11}
	}
}

func allowsCPUFallback(codec string) bool {
	if codec == config.CodecH264X264 {
		return false
	}
	switch strings.ToLower(strings.TrimSpace(os.Getenv("TETHER_FFMPEG_PIPELINE"))) {
	case "auto", "fallback":
		return true
	default:
		return false
	}
}

func logPipeline(mode pipelineMode, codec string) {
	switch mode {
	case pipelineD3D11:
		fmt.Fprintln(os.Stderr, "[capture] usando pipeline d3d11 direto para NVENC")
	case pipelineCPU:
		if codec == config.CodecH264X264 {
			fmt.Fprintln(os.Stderr, "[capture] usando pipeline hwdownload->CPU->libx264 (TESTE)")
		} else {
			fmt.Fprintln(os.Stderr, "[capture] usando fallback hwdownload->CPU->NVENC")
		}
	}
}

func logEncodingTuning(cfg config.StreamConfig) {
	cfg = cfg.Normalize()
	t := cfg.Tuning()
	if cfg.Codec == config.CodecH264X264 {
		fmt.Fprintf(
			os.Stderr,
			"[capture] h264_x264 TESTE %dx%d@%d %dkbps level=%s gop=%d vbv=%dk\n",
			cfg.Width,
			cfg.Height,
			cfg.FPS,
			cfg.Bitrate,
			t.Level,
			t.GOPFrames,
			t.VBVBufferKb,
		)
		return
	}

	fmt.Fprintf(
		os.Stderr,
		"[capture] h264_nvenc %dx%d@%d %dkbps level=%s gop=%d vbv=%dk surfaces=%d aq=%t\n",
		cfg.Width,
		cfg.Height,
		cfg.FPS,
		cfg.Bitrate,
		t.Level,
		t.GOPFrames,
		t.VBVBufferKb,
		t.Surfaces,
		t.SpatialAQ,
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
	cfg := c.cfg.Normalize()
	// Todos os ajustes finos vêm do TuningProfile adaptativo (função de
	// bitrate × resolução × fps), não de constantes. Ver internal/config/tuning.go.
	tune := cfg.Tuning()
	gop := tune.GOPFrames

	// VBV em milissegundos de bitrate: janela temporal estável para o rate
	// control AMORTIZAR o IDR ao longo de alguns frames em vez de despejá-lo num
	// lote (a rajada periódica que inflava o jitter buffer da TV). Curto o
	// bastante para manter a latência de saída baixa.
	bufsize := tune.VBVBufferKb

	input := fmt.Sprintf(
		"ddagrab=output_idx=%d:framerate=%d:video_size=%dx%d:dup_frames=1",
		cfg.Display,
		cfg.FPS,
		cfg.Width,
		cfg.Height,
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
		filter := "hwdownload,format=bgra,format=nv12"
		if cfg.Codec == config.CodecH264X264 {
			filter = "hwdownload,format=bgra,format=yuv420p"
		}
		args = append(args,
			// Fallback compatível: baixa o frame do d3d11 pra CPU e converte para
			// o formato esperado pelo encoder selecionado.
			"-vf", filter,
		)
	}

	if cfg.Codec == config.CodecH264X264 {
		args = appendX264Args(args, cfg, tune, gop, bufsize)
	} else {
		args = appendNVENCArgs(args, cfg, tune, gop, bufsize)
	}

	args = append(args,
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

func appendNVENCArgs(args []string, cfg config.StreamConfig, tune config.TuningProfile, gop, bufsize int) []string {
	return append(args,
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
		"-level", tune.Level,
		"-rc", tune.RateControl,
		"-b:v", fmt.Sprintf("%dk", cfg.Bitrate),
		"-maxrate", fmt.Sprintf("%dk", cfg.Bitrate),
		"-bufsize", fmt.Sprintf("%dk", bufsize),
		"-g", fmt.Sprintf("%d", gop),
		"-bf", "0", // sem B-frames (latência)
		"-delay", "0", // sem reordenação/atraso de saída do encoder
		"-rc-lookahead", "0", // NVENC não segura frames analisando o futuro
		// surfaces escalado pela carga (pixels×fps). 2 fixas estrangulavam o
		// pipeline em bitrate/resolução alto e geravam rajada de saída; mais
		// surfaces suavizam a SAÍDA do encoder sem latência de reordenação
		// (continuamos -bf 0 / -delay 0).
		"-surfaces", fmt.Sprintf("%d", tune.Surfaces),
		// spatial-aq: redistribui bits dentro do frame para regiões complexas,
		// achatando os picos de tamanho que viram rajada em cenas movimentadas.
		"-spatial-aq", boolFlag(tune.SpatialAQ),
		// temporal-aq: redistribui bits ENTRE frames, suavizando o pico de
		// tamanho do IDR (medido ~50% maior que o P-frame médio) — é o pico que
		// estoura o gap de envio a cada keyframe.
		"-temporal-aq", boolFlag(tune.TemporalAQ),
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
	)
}

func appendX264Args(args []string, cfg config.StreamConfig, tune config.TuningProfile, gop, bufsize int) []string {
	return append(args,
		// --- encoder universal/teste: CPU libx264 low-latency ---
		"-c:v", "libx264",
		"-preset", "ultrafast",
		"-tune", "zerolatency",
		// Mantém o contrato do SDP: H.264 Baseline sem B-frames, com level
		// derivado da resolução.
		"-profile:v", "baseline",
		"-level", tune.Level,
		"-pix_fmt", "yuv420p",
		"-b:v", fmt.Sprintf("%dk", cfg.Bitrate),
		"-maxrate", fmt.Sprintf("%dk", cfg.Bitrate),
		"-bufsize", fmt.Sprintf("%dk", bufsize),
		"-g", fmt.Sprintf("%d", gop),
		"-keyint_min", fmt.Sprintf("%d", gop),
		"-bf", "0",
		"-sc_threshold", "0",
		"-x264-params", "repeat-headers=1:sliced-threads=1:sync-lookahead=0:rc-lookahead=0",
		// emite access unit delimiters (NAL type 9) -> fronteira de frame
		// inequívoca para o agrupador do lado Go.
		"-aud", "1",
	)
}

func boolFlag(b bool) string {
	if b {
		return "1"
	}
	return "0"
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
