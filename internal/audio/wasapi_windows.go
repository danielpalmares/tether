//go:build windows

package audio

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"runtime"
	"sync"
	"syscall"
	"time"
	"unsafe"

	"github.com/go-ole/go-ole"
	"github.com/moutend/go-wca/pkg/wca"
)

const (
	waveFormatPCM        = 0x0001
	waveFormatIEEEFloat  = 0x0003
	waveFormatExtensible = 0xfffe
)

var (
	ksSubTypePCM       = [16]byte{0x01, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}
	ksSubTypeIEEEFloat = [16]byte{0x03, 0x00, 0x00, 0x00, 0x00, 0x00, 0x10, 0x00, 0x80, 0x00, 0x00, 0xaa, 0x00, 0x38, 0x9b, 0x71}
)

type wasapiLoopback struct {
	ctx      context.Context
	cancel   context.CancelFunc
	writerCh chan writeCloser
	done     chan error
	format   pcmFormat
}

type wasapiReady struct {
	format pcmFormat
	err    error
}

func newWASAPILoopback(parent context.Context) (pcmLoopback, error) {
	ctx, cancel := context.WithCancel(parent)
	w := &wasapiLoopback{
		ctx:      ctx,
		cancel:   cancel,
		writerCh: make(chan writeCloser, 1),
		done:     make(chan error, 1),
	}

	ready := make(chan wasapiReady, 1)
	go w.captureThread(ready)

	select {
	case r := <-ready:
		if r.err != nil {
			cancel()
			return nil, r.err
		}
		w.format = r.format
		return w, nil
	case <-time.After(3 * time.Second):
		cancel()
		return nil, fmt.Errorf("timeout inicializando WASAPI")
	case <-parent.Done():
		cancel()
		return nil, parent.Err()
	}
}

func (w *wasapiLoopback) Name() string {
	f := w.format
	return fmt.Sprintf("WASAPI loopback %s/%dHz/%dch", f.FFmpegFormat, f.SampleRate, f.Channels)
}

func (w *wasapiLoopback) Format() pcmFormat {
	return w.format
}

func (w *wasapiLoopback) StartWriting(writer writeCloser) error {
	select {
	case w.writerCh <- writer:
		return nil
	case <-w.done:
		return fmt.Errorf("WASAPI encerrou antes de receber o pipe")
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
}

func (w *wasapiLoopback) Close() error {
	w.cancel()
	select {
	case <-w.done:
	case <-time.After(750 * time.Millisecond):
	}
	return nil
}

func (w *wasapiLoopback) captureThread(ready chan<- wasapiReady) {
	runtime.LockOSThread()
	defer runtime.UnlockOSThread()

	err := w.runCapture(ready)
	w.done <- err
}

func (w *wasapiLoopback) runCapture(ready chan<- wasapiReady) error {
	if err := ole.CoInitializeEx(0, ole.COINIT_MULTITHREADED); err != nil && !oleCode(err, 1) {
		ready <- wasapiReady{err: fmt.Errorf("CoInitializeEx: %w", err)}
		return err
	}
	defer ole.CoUninitialize()

	var enumerator *wca.IMMDeviceEnumerator
	if err := wca.CoCreateInstance(
		wca.CLSID_MMDeviceEnumerator,
		0,
		wca.CLSCTX_ALL,
		wca.IID_IMMDeviceEnumerator,
		&enumerator,
	); err != nil {
		ready <- wasapiReady{err: fmt.Errorf("MMDeviceEnumerator: %w", err)}
		return err
	}
	defer enumerator.Release()

	var device *wca.IMMDevice
	if err := enumerator.GetDefaultAudioEndpoint(wca.ERender, wca.EMultimedia, &device); err != nil {
		ready <- wasapiReady{err: fmt.Errorf("endpoint de saída padrão: %w", err)}
		return err
	}
	defer device.Release()

	var audioClient *wca.IAudioClient
	if err := activateAudioClient(device, &audioClient); err != nil {
		ready <- wasapiReady{err: fmt.Errorf("IAudioClient: %w", err)}
		return err
	}
	defer audioClient.Release()

	format, err := initializeLoopbackClient(audioClient)
	if err != nil {
		ready <- wasapiReady{err: err}
		return err
	}
	ready <- wasapiReady{format: format}

	var captureClient *wca.IAudioCaptureClient
	if err := audioClient.GetService(wca.IID_IAudioCaptureClient, &captureClient); err != nil {
		return fmt.Errorf("IAudioCaptureClient: %w", err)
	}
	defer captureClient.Release()

	var writer writeCloser
	select {
	case writer = <-w.writerCh:
	case <-w.ctx.Done():
		return w.ctx.Err()
	}
	defer writer.Close()

	if err := audioClient.Start(); err != nil {
		return fmt.Errorf("WASAPI start: %w", err)
	}
	defer audioClient.Stop()

	return capturePCM(w.ctx, captureClient, writer, format)
}

func activateAudioClient(device *wca.IMMDevice, audioClient **wca.IAudioClient) error {
	hr, _, _ := syscall.Syscall6(
		device.VTable().Activate,
		5,
		uintptr(unsafe.Pointer(device)),
		uintptr(unsafe.Pointer(wca.IID_IAudioClient)),
		uintptr(wca.CLSCTX_ALL),
		0,
		uintptr(unsafe.Pointer(audioClient)),
		0,
	)
	if hr != 0 {
		return ole.NewError(hr)
	}
	return nil
}

func initializeLoopbackClient(audioClient *wca.IAudioClient) (pcmFormat, error) {
	desiredFormat := wca.WAVEFORMATEX{
		WFormatTag:      waveFormatIEEEFloat,
		NChannels:       2,
		NSamplesPerSec:  48000,
		WBitsPerSample:  32,
		NBlockAlign:     2 * 4,
		NAvgBytesPerSec: 48000 * 2 * 4,
	}
	desired := pcmFormat{FFmpegFormat: "f32le", SampleRate: 48000, Channels: 2, BlockAlign: 8}
	flags := uint32(wca.AUDCLNT_STREAMFLAGS_LOOPBACK | wca.AUDCLNT_STREAMFLAGS_AUTOCONVERTPCM | wca.AUDCLNT_STREAMFLAGS_SRC_DEFAULT_QUALITY)
	if err := initializeShared(audioClient, flags, &desiredFormat); err == nil {
		return desired, nil
	}

	var mix *wca.WAVEFORMATEX
	if err := audioClient.GetMixFormat(&mix); err != nil {
		return pcmFormat{}, fmt.Errorf("WASAPI mix format: %w", err)
	}
	defer ole.CoTaskMemFree(uintptr(unsafe.Pointer(mix)))

	format, err := pcmFormatFromWave(mix)
	if err != nil {
		return pcmFormat{}, err
	}
	if err := initializeShared(audioClient, wca.AUDCLNT_STREAMFLAGS_LOOPBACK, mix); err != nil {
		return pcmFormat{}, fmt.Errorf("WASAPI initialize: %w", err)
	}
	return format, nil
}

func initializeShared(audioClient *wca.IAudioClient, flags uint32, format *wca.WAVEFORMATEX) error {
	for _, duration := range []wca.REFERENCE_TIME{200000, 0} {
		err := audioClient.Initialize(
			wca.AUDCLNT_SHAREMODE_SHARED,
			flags,
			duration,
			0,
			format,
			nil,
		)
		if err == nil {
			return nil
		}
	}
	return fmt.Errorf("formato não aceito pelo engine de áudio")
}

func pcmFormatFromWave(wfx *wca.WAVEFORMATEX) (pcmFormat, error) {
	tag := wfx.WFormatTag
	if tag == waveFormatExtensible && wfx.CbSize >= 22 {
		subFormat := *(*[16]byte)(unsafe.Pointer(uintptr(unsafe.Pointer(wfx)) + 24))
		switch {
		case bytes.Equal(subFormat[:], ksSubTypePCM[:]):
			tag = waveFormatPCM
		case bytes.Equal(subFormat[:], ksSubTypeIEEEFloat[:]):
			tag = waveFormatIEEEFloat
		default:
			return pcmFormat{}, fmt.Errorf("WASAPI subformato não suportado: %x", subFormat)
		}
	}

	format := pcmFormat{
		SampleRate: int(wfx.NSamplesPerSec),
		Channels:   int(wfx.NChannels),
		BlockAlign: int(wfx.NBlockAlign),
	}
	switch tag {
	case waveFormatIEEEFloat:
		switch wfx.WBitsPerSample {
		case 32:
			format.FFmpegFormat = "f32le"
		case 64:
			format.FFmpegFormat = "f64le"
		default:
			return pcmFormat{}, fmt.Errorf("WASAPI float %d-bit não suportado", wfx.WBitsPerSample)
		}
	case waveFormatPCM:
		switch wfx.WBitsPerSample {
		case 8:
			format.FFmpegFormat = "u8"
		case 16:
			format.FFmpegFormat = "s16le"
		case 24:
			format.FFmpegFormat = "s24le"
		case 32:
			format.FFmpegFormat = "s32le"
		default:
			return pcmFormat{}, fmt.Errorf("WASAPI PCM %d-bit não suportado", wfx.WBitsPerSample)
		}
	default:
		return pcmFormat{}, fmt.Errorf("WASAPI formato não suportado: tag=0x%x", wfx.WFormatTag)
	}
	if err := format.validate(); err != nil {
		return pcmFormat{}, err
	}
	return format, nil
}

func capturePCM(ctx context.Context, captureClient *wca.IAudioCaptureClient, writer writeCloser, format pcmFormat) error {
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()

	pacer := newPCMPacer(ctx, writer, format, 10*time.Millisecond, 40*time.Millisecond)
	defer pacer.Close()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-pacer.Done():
			return err
		case <-ticker.C:
			if _, err := drainWASAPIPackets(captureClient, pacer, format); err != nil {
				return err
			}
		}
	}
}

type pcmPacer struct {
	ctx            context.Context
	writer         io.Writer
	chunkDuration  time.Duration
	chunkBytes     int
	maxBufferBytes int
	mu             sync.Mutex
	buffer         []byte
	closed         bool
	done           chan error
}

func newPCMPacer(ctx context.Context, writer io.Writer, format pcmFormat, chunkDuration, maxBufferDuration time.Duration) *pcmPacer {
	chunkFrames := int(time.Duration(format.SampleRate) * chunkDuration / time.Second)
	if chunkFrames <= 0 {
		chunkFrames = format.SampleRate / 100
	}
	if chunkFrames <= 0 {
		chunkFrames = 480
	}
	chunkBytes := chunkFrames * format.BlockAlign

	maxFrames := int(time.Duration(format.SampleRate) * maxBufferDuration / time.Second)
	if maxFrames < chunkFrames {
		maxFrames = chunkFrames
	}

	p := &pcmPacer{
		ctx:            ctx,
		writer:         writer,
		chunkDuration:  chunkDuration,
		chunkBytes:     chunkBytes,
		maxBufferBytes: maxFrames * format.BlockAlign,
		done:           make(chan error, 1),
	}
	go p.run()
	return p
}

func (p *pcmPacer) Done() <-chan error {
	return p.done
}

func (p *pcmPacer) Write(data []byte) (int, error) {
	if len(data) == 0 {
		return 0, nil
	}

	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return 0, io.ErrClosedPipe
	}

	p.buffer = append(p.buffer, data...)
	if over := len(p.buffer) - p.maxBufferBytes; over > 0 {
		copy(p.buffer, p.buffer[over:])
		p.buffer = p.buffer[:len(p.buffer)-over]
	}
	return len(data), nil
}

func (p *pcmPacer) Close() error {
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()

	select {
	case err := <-p.done:
		return err
	case <-time.After(500 * time.Millisecond):
		return nil
	}
}

func (p *pcmPacer) run() {
	err := p.runLoop()
	p.mu.Lock()
	p.closed = true
	p.mu.Unlock()
	p.done <- err
}

func (p *pcmPacer) runLoop() error {
	next := time.Now()
	timer := time.NewTimer(0)
	defer timer.Stop()

	for {
		select {
		case <-p.ctx.Done():
			return p.ctx.Err()
		case <-timer.C:
			if err := writeAll(p.writer, p.nextChunk()); err != nil {
				return err
			}
			next = next.Add(p.chunkDuration)
			delay := time.Until(next)
			if delay < -p.chunkDuration {
				next = time.Now().Add(p.chunkDuration)
				delay = p.chunkDuration
			}
			if delay < 0 {
				delay = 0
			}
			timer.Reset(delay)
		}
	}
}

func (p *pcmPacer) nextChunk() []byte {
	chunk := make([]byte, p.chunkBytes)

	p.mu.Lock()
	defer p.mu.Unlock()
	n := copy(chunk, p.buffer)
	if n > 0 {
		copy(p.buffer, p.buffer[n:])
		p.buffer = p.buffer[:len(p.buffer)-n]
	}
	return chunk
}

func drainWASAPIPackets(captureClient *wca.IAudioCaptureClient, writer io.Writer, format pcmFormat) (bool, error) {
	var packetFrames uint32
	if err := captureClient.GetNextPacketSize(&packetFrames); err != nil {
		return false, fmt.Errorf("WASAPI next packet: %w", err)
	}

	wrote := false
	for packetFrames > 0 {
		var data *byte
		var frames uint32
		var flags uint32
		var devicePosition uint64
		var qpcPosition uint64
		if err := captureClient.GetBuffer(&data, &frames, &flags, &devicePosition, &qpcPosition); err != nil {
			return wrote, fmt.Errorf("WASAPI get buffer: %w", err)
		}

		byteCount := int(frames) * format.BlockAlign
		if byteCount > 0 {
			if flags&wca.AUDCLNT_BUFFERFLAGS_SILENT != 0 || data == nil {
				if err := writeAll(writer, make([]byte, byteCount)); err != nil {
					_ = captureClient.ReleaseBuffer(frames)
					return wrote, err
				}
			} else {
				src := unsafe.Slice(data, byteCount)
				if err := writeAll(writer, src); err != nil {
					_ = captureClient.ReleaseBuffer(frames)
					return wrote, err
				}
			}
			wrote = true
		}

		if err := captureClient.ReleaseBuffer(frames); err != nil {
			return wrote, fmt.Errorf("WASAPI release buffer: %w", err)
		}
		if err := captureClient.GetNextPacketSize(&packetFrames); err != nil {
			return wrote, fmt.Errorf("WASAPI next packet: %w", err)
		}
	}
	return wrote, nil
}

func writeAll(w io.Writer, p []byte) error {
	for len(p) > 0 {
		n, err := w.Write(p)
		if err != nil {
			return err
		}
		if n == 0 {
			return io.ErrShortWrite
		}
		p = p[n:]
	}
	return nil
}

func oleCode(err error, code uintptr) bool {
	oleErr, ok := err.(*ole.OleError)
	return ok && oleErr.Code() == code
}
