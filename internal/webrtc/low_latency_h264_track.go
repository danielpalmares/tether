package webrtc

import (
	"strings"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/rtp/codecs"
	pionwebrtc "github.com/pion/webrtc/v4"
	"github.com/pion/webrtc/v4/pkg/media"
)

const (
	rtpOutboundMTU            = 1200
	playoutDelayExtensionURI  = "http://www.webrtc.org/experiments/rtp-hdrext/playout-delay"
	zeroPlayoutDelayExtension = "\x00\x00\x00"
)

type lowLatencyH264Track struct {
	mu         sync.RWMutex
	codec      pionwebrtc.RTPCodecCapability
	id         string
	streamID   string
	bindings   []lowLatencyTrackBinding
	packetizer rtp.Packetizer
	clockRate  float64
}

type lowLatencyTrackBinding struct {
	id             string
	ssrc           pionwebrtc.SSRC
	payloadType    pionwebrtc.PayloadType
	writeStream    pionwebrtc.TrackLocalWriter
	playoutDelayID uint8
}

func newLowLatencyH264Track(codec pionwebrtc.RTPCodecCapability, id, streamID string) *lowLatencyH264Track {
	return &lowLatencyH264Track{
		codec:    codec,
		id:       id,
		streamID: streamID,
		bindings: []lowLatencyTrackBinding{},
	}
}

func (t *lowLatencyH264Track) Bind(ctx pionwebrtc.TrackLocalContext) (pionwebrtc.RTPCodecParameters, error) {
	codec, ok := findCodec(t.codec, ctx.CodecParameters())
	if !ok {
		return pionwebrtc.RTPCodecParameters{}, pionwebrtc.ErrUnsupportedCodec
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	t.bindings = append(t.bindings, lowLatencyTrackBinding{
		id:             ctx.ID(),
		ssrc:           ctx.SSRC(),
		payloadType:    codec.PayloadType,
		writeStream:    ctx.WriteStream(),
		playoutDelayID: findHeaderExtensionID(ctx.HeaderExtensions(), playoutDelayExtensionURI),
	})

	if t.packetizer == nil {
		t.packetizer = rtp.NewPacketizer(
			rtpOutboundMTU,
			0,
			0,
			&codecs.H264Payloader{},
			rtp.NewRandomSequencer(),
			codec.ClockRate,
		)
		t.clockRate = float64(codec.ClockRate)
	}

	return codec, nil
}

func (t *lowLatencyH264Track) Unbind(ctx pionwebrtc.TrackLocalContext) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	for i := range t.bindings {
		if t.bindings[i].id == ctx.ID() {
			t.bindings[i] = t.bindings[len(t.bindings)-1]
			t.bindings = t.bindings[:len(t.bindings)-1]
			return nil
		}
	}

	return pionwebrtc.ErrUnbindFailed
}

func (t *lowLatencyH264Track) ID() string       { return t.id }
func (t *lowLatencyH264Track) StreamID() string { return t.streamID }
func (t *lowLatencyH264Track) RID() string      { return "" }

func (t *lowLatencyH264Track) Kind() pionwebrtc.RTPCodecType {
	return pionwebrtc.RTPCodecTypeVideo
}

func (t *lowLatencyH264Track) WriteSample(sample media.Sample) error {
	t.mu.RLock()
	packetizer := t.packetizer
	clockRate := t.clockRate
	bindings := append([]lowLatencyTrackBinding(nil), t.bindings...)
	t.mu.RUnlock()

	if packetizer == nil || len(bindings) == 0 {
		return nil
	}

	samples := uint32(sample.Duration.Seconds() * clockRate)
	packets := packetizer.Packetize(sample.Data, samples)
	if len(packets) == 0 {
		return nil
	}

	var writeErr error
	for _, pkt := range packets {
		if err := writePacketToBindings(pkt, bindings); err != nil && writeErr == nil {
			writeErr = err
		}
	}

	return writeErr
}

func writePacketToBindings(pkt *rtp.Packet, bindings []lowLatencyTrackBinding) error {
	var writeErr error
	for _, binding := range bindings {
		pkt.Header.SSRC = uint32(binding.ssrc)
		pkt.Header.PayloadType = uint8(binding.payloadType)
		if binding.playoutDelayID > 0 {
			_ = pkt.Header.SetExtension(binding.playoutDelayID, []byte(zeroPlayoutDelayExtension))
		}
		if _, err := binding.writeStream.WriteRTP(&pkt.Header, pkt.Payload); err != nil && writeErr == nil {
			writeErr = err
		}
	}

	return writeErr
}

func findCodec(want pionwebrtc.RTPCodecCapability, codecs []pionwebrtc.RTPCodecParameters) (pionwebrtc.RTPCodecParameters, bool) {
	for _, codec := range codecs {
		if strings.EqualFold(codec.MimeType, want.MimeType) &&
			codec.ClockRate == want.ClockRate &&
			strings.Contains(codec.SDPFmtpLine, "profile-level-id=42c02a") {
			return codec, true
		}
	}
	for _, codec := range codecs {
		if strings.EqualFold(codec.MimeType, want.MimeType) && codec.ClockRate == want.ClockRate {
			return codec, true
		}
	}
	return pionwebrtc.RTPCodecParameters{}, false
}

func findHeaderExtensionID(headers []pionwebrtc.RTPHeaderExtensionParameter, uri string) uint8 {
	for _, header := range headers {
		if header.URI == uri && header.ID > 0 && header.ID < 15 {
			return uint8(header.ID)
		}
	}
	return 0
}

var _ pionwebrtc.TrackLocal = (*lowLatencyH264Track)(nil)

type sampleWriter interface {
	WriteSample(media.Sample) error
}

type encodedFrame struct {
	data     []byte
	duration time.Duration
	keyframe bool
}
