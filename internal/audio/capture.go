package audio

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"tether/internal/process"
)

const RTPPayloadType = 111

type Capturer struct {
	cmd    *exec.Cmd
	cancel context.CancelFunc
}

type RTPStream struct {
	conn  *net.UDPConn
	first []byte
	stop  func()
}

func New() *Capturer {
	return &Capturer{}
}

func (c *Capturer) Start(ctx context.Context) (*RTPStream, error) {
	ctx, c.cancel = context.WithCancel(ctx)

	if strings.TrimSpace(os.Getenv("TETHER_AUDIO_BACKEND")) != "dshow" {
		stream, err := c.startWASAPI(ctx)
		if err == nil {
			return stream, nil
		}
		fmt.Fprintf(os.Stderr, "[audio] WASAPI indisponível: %v; tentando DirectShow\n", err)
	}

	return c.startDirectShow(ctx)
}

func (c *Capturer) startWASAPI(ctx context.Context) (*RTPStream, error) {
	loopback, err := newWASAPILoopback(ctx)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		_ = loopback.Close()
		return nil, err
	}

	port := conn.LocalAddr().(*net.UDPAddr).Port
	cmd := exec.CommandContext(ctx, "ffmpeg", ffmpegPCMArgs(loopback.Format(), port)...)
	cmd.Stderr = os.Stderr
	stdin, err := cmd.StdinPipe()
	if err != nil {
		_ = loopback.Close()
		_ = conn.Close()
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		_ = loopback.Close()
		_ = conn.Close()
		return nil, err
	}
	if err := process.BoostPID(cmd.Process.Pid); err != nil {
		fmt.Fprintf(os.Stderr, "[audio] aviso: não foi possível elevar prioridade do FFmpeg áudio: %v\n", err)
	}
	if err := loopback.StartWriting(stdin); err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = loopback.Close()
		_ = conn.Close()
		return nil, err
	}

	first, err := waitFirstPacket(conn, 2500*time.Millisecond)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = loopback.Close()
		_ = conn.Close()
		return nil, err
	}

	c.cmd = cmd
	fmt.Fprintf(os.Stderr, "[audio] usando %s -> PCM -> Opus/RTP\n", loopback.Name())
	return &RTPStream{
		conn:  conn,
		first: first,
		stop: func() {
			_ = loopback.Close()
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = conn.Close()
		},
	}, nil
}

func (c *Capturer) startDirectShow(ctx context.Context) (*RTPStream, error) {
	device, err := audioDevice(ctx)
	if err != nil {
		return nil, err
	}

	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		return nil, err
	}

	port := conn.LocalAddr().(*net.UDPAddr).Port
	cmd := exec.CommandContext(ctx, "ffmpeg", ffmpegArgs(device, port)...)
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		_ = conn.Close()
		return nil, err
	}
	if err := process.BoostPID(cmd.Process.Pid); err != nil {
		fmt.Fprintf(os.Stderr, "[audio] aviso: não foi possível elevar prioridade do FFmpeg áudio: %v\n", err)
	}

	first, err := waitFirstPacket(conn, 2500*time.Millisecond)
	if err != nil {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		_ = conn.Close()
		return nil, err
	}

	c.cmd = cmd
	fmt.Fprintf(os.Stderr, "[audio] usando DirectShow '%s' -> Opus/RTP\n", device)
	return &RTPStream{
		conn:  conn,
		first: first,
		stop: func() {
			if cmd.Process != nil {
				_ = cmd.Process.Kill()
			}
			_ = conn.Close()
		},
	}, nil
}

func (c *Capturer) Stop() {
	if c.cancel != nil {
		c.cancel()
	}
	if c.cmd != nil && c.cmd.Process != nil {
		_ = c.cmd.Process.Kill()
	}
}

func (s *RTPStream) ReadPacket() ([]byte, error) {
	if s.first != nil {
		pkt := s.first
		s.first = nil
		return pkt, nil
	}
	buf := make([]byte, 1500)
	n, _, err := s.conn.ReadFromUDP(buf)
	if err != nil {
		return nil, err
	}
	return append([]byte(nil), buf[:n]...), nil
}

func (s *RTPStream) Close() error {
	if s.stop != nil {
		s.stop()
	}
	return nil
}

func waitFirstPacket(conn *net.UDPConn, timeout time.Duration) ([]byte, error) {
	if err := conn.SetReadDeadline(time.Now().Add(timeout)); err != nil {
		return nil, err
	}
	defer conn.SetReadDeadline(time.Time{})

	buf := make([]byte, 1500)
	n, _, err := conn.ReadFromUDP(buf)
	if err != nil {
		return nil, fmt.Errorf("sem pacote RTP de áudio: %w", err)
	}
	return append([]byte(nil), buf[:n]...), nil
}

func ffmpegArgs(device string, port int) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-f", "dshow",
		"-audio_buffer_size", "50",
		"-i", "audio=" + device,
		"-vn",
		"-ac", "2",
		"-ar", "48000",
		"-c:a", "libopus",
		"-application", "lowdelay",
		"-frame_duration", "20",
		"-b:a", "96k",
		"-vbr", "off",
		"-compression_level", "0",
		"-flush_packets", "1",
		// Ver nota em ffmpegPCMArgs: zera o buffer do muxer RTP que agregava 90ms.
		"-max_delay", "0",
		"-muxdelay", "0",
		"-muxpreload", "0",
		"-f", "rtp",
		"-payload_type", fmt.Sprintf("%d", RTPPayloadType),
		fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=400", port),
	}
}

func ffmpegPCMArgs(format pcmFormat, port int) []string {
	return []string{
		"-hide_banner", "-loglevel", "error",
		"-fflags", "nobuffer",
		"-flags", "low_delay",
		"-probesize", "32",
		"-analyzeduration", "0",
		"-f", format.FFmpegFormat,
		"-ar", fmt.Sprintf("%d", format.SampleRate),
		"-ac", fmt.Sprintf("%d", format.Channels),
		"-i", "pipe:0",
		"-vn",
		"-ac", "2",
		"-ar", "48000",
		"-c:a", "libopus",
		"-application", "lowdelay",
		"-frame_duration", "20",
		"-b:a", "96k",
		"-vbr", "off",
		"-compression_level", "0",
		"-flush_packets", "1",
		// max_delay/muxdelay/muxpreload = 0: o muxer RTP, por padrão, bufferiza
		// ~90ms de áudio antes de despejar (medido como gapMax=90ms no pumpAudio,
		// pkt_size sozinho não quebra isso porque o muxer agrega por TEMPO, não
		// por tamanho). Zerar os três força a emissão de cada frame Opus (~20ms)
		// imediatamente -> gap cai de 90ms para ~20ms, lip-sync deixa de arrastar
		// o vídeo.
		"-max_delay", "0",
		"-muxdelay", "0",
		"-muxpreload", "0",
		"-f", "rtp",
		"-payload_type", fmt.Sprintf("%d", RTPPayloadType),
		fmt.Sprintf("rtp://127.0.0.1:%d?pkt_size=400", port),
	}
}

func audioDevice(ctx context.Context) (string, error) {
	if device := strings.TrimSpace(os.Getenv("TETHER_AUDIO_DEVICE")); device != "" {
		return device, nil
	}

	devices, err := listDShowAudioDevices(ctx)
	if err != nil {
		return "", err
	}
	for _, device := range devices {
		if isLoopbackLike(device) {
			return device, nil
		}
	}
	return "", fmt.Errorf("nenhuma fonte de áudio loopback/virtual encontrada no DirectShow (defina TETHER_AUDIO_DEVICE; disponíveis: %s)", strings.Join(devices, ", "))
}

func listDShowAudioDevices(ctx context.Context) ([]string, error) {
	cmd := exec.CommandContext(ctx, "ffmpeg", "-hide_banner", "-list_devices", "true", "-f", "dshow", "-i", "dummy")
	out, _ := cmd.CombinedOutput()

	re := regexp.MustCompile(`"([^"]+)" \(audio\)`)
	matches := re.FindAllStringSubmatch(string(out), -1)
	devices := make([]string, 0, len(matches))
	for _, match := range matches {
		devices = append(devices, match[1])
	}
	if len(devices) == 0 {
		return nil, fmt.Errorf("nenhum dispositivo DirectShow de áudio encontrado")
	}
	return devices, nil
}

func isLoopbackLike(device string) bool {
	name := strings.ToLower(device)
	for _, token := range []string{
		"stereo mix",
		"mixagem estéreo",
		"what u hear",
		"virtual-audio-capturer",
		"cable output",
		"voicemeeter",
		"steam streaming speaker",
		"steam streaming speakers",
	} {
		if strings.Contains(name, token) {
			return true
		}
	}
	return false
}
