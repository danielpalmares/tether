package audio

import (
	"net"
	"testing"
)

func TestCapturerStopClosesActiveStream(t *testing.T) {
	conn, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.ParseIP("127.0.0.1"), Port: 0})
	if err != nil {
		t.Fatalf("listen udp: %v", err)
	}

	stopped := make(chan struct{})
	c := &Capturer{
		stream: &RTPStream{
			conn: conn,
			stop: func() {
				_ = conn.Close()
				close(stopped)
			},
		},
	}

	c.Stop()

	select {
	case <-stopped:
	default:
		t.Fatalf("Stop should close active RTP stream")
	}
	if c.stream != nil {
		t.Fatalf("capturer should clear active stream")
	}
	if c.cmd != nil {
		t.Fatalf("capturer should clear active command")
	}
	if _, _, err := conn.ReadFromUDP(make([]byte, 1)); err == nil {
		t.Fatalf("UDP conn should be closed")
	}
}
