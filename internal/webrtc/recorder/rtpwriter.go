package recorder

import (
	"net"
	"os"
	"sync"
	"time"

	"github.com/pion/rtp"
	"github.com/pion/webrtc/v4/pkg/media/rtpdump"
)

// RTPWriter writes RTP packets to a file using the rtpdump format.
// Inspired by https://gist.github.com/Sean-Der/1cf72cec548d070a19d3f846e14221f6
type RTPWriter struct {
	writer    *rtpdump.Writer
	file      *os.File
	mu        sync.Mutex
	path      string
	closed    bool
	startTime time.Time
}

func NewRTPWriter(path string, sourceIP net.IP, sourcePort uint16) (*RTPWriter, error) {
	file, err := os.Create(path)

	if err != nil {
		return nil, err
	}

	startTime := time.Now()
	header := rtpdump.Header{
		Start:  startTime,
		Source: sourceIP,
		Port:   sourcePort,
	}

	writer, err := rtpdump.NewWriter(file, header)

	if err != nil {
		_ = file.Close()
		return nil, err
	}

	return &RTPWriter{
		writer:    writer,
		file:      file,
		path:      path,
		startTime: startTime,
	}, nil
}

// WriteRTP marshals an RTP packet and writes it to the rtpdump file.
func (w *RTPWriter) WriteRTP(packet *rtp.Packet) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}

	payload, err := packet.Marshal()

	if err != nil {
		return err
	}

	p := rtpdump.Packet{
		Offset:  time.Since(w.startTime),
		IsRTCP:  false,
		Payload: payload,
	}

	return w.writer.WritePacket(p)
}

// Close terminates the rtpdump writer and closes the rtpdump file.
// It is safe to call Close multiple times.
func (w *RTPWriter) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	if w.closed {
		return nil
	}

	w.closed = true

	return w.file.Close()
}
