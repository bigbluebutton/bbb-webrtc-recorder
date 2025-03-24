package recorder

import (
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"sync"
)

type IVFWriter struct {
	file     *os.File
	mu       sync.Mutex
	enabled  bool
	filePath string
	closed   bool
	frameCount uint64
}

type RTPPacketWithMeta struct {
	Payload    []byte
	Timestamp  uint32
	SequenceNo uint16
	IsKeyFrame bool
	Marker     bool
}

// VP8 only for now
func NewIVFWriter(basePath string) (*IVFWriter, error) {
	if basePath == "" {
		return &IVFWriter{enabled: false}, nil
	}

	ext := filepath.Ext(basePath)
	ivfPath := basePath[:len(basePath)-len(ext)] + ".ivf"

	file, err := os.Create(ivfPath)
	if err != nil {
		return nil, err
	}

	ivf := &IVFWriter{
		file:     file,
		enabled:  true,
		filePath: ivfPath,
		closed:   false,
	}

	// Reminder: update size on keyframes
	ivf.writeIVFHeader(640, 480)

	return ivf, nil
}

func (writer *IVFWriter) writeIVFHeader(width, height uint16) error {
	if !writer.enabled {
		return nil
	}

	writer.mu.Lock()
	defer writer.mu.Unlock()

	header := []byte{
    // Signature: "DKIF"
    'D', 'K', 'I', 'F',
    // Version: 0
    0, 0,
    // Header size: 32
    32, 0,
    // FourCC: "VP80"
    'V', 'P', '8', '0',
    // Width & Height (to be filled in later)
    0, 0, 0, 0,
    // Timebase denominator (to be filled in later)
    0, 0, 0, 0,
    // Timebase numerator (to be filled in later)
    0, 0, 0, 0,
    // Frame count (to be filled in later)
    0, 0, 0, 0,
    // Reserved
    0, 0, 0, 0,
}

	binary.LittleEndian.PutUint16(header[12:14], width)
	binary.LittleEndian.PutUint16(header[14:16], height)
	// Rate (VP8)
	binary.LittleEndian.PutUint32(header[16:20], 90000)
	// tb
	binary.LittleEndian.PutUint32(header[20:24], 1)

	_, err := writer.file.Write(header)

	if err != nil {
		return err
	}

	_, err = writer.file.Seek(0, io.SeekStart)
	return err
}

func (writer *IVFWriter) UpdateDimensions(width, height uint16) error {
	if !writer.enabled {
		return nil
	}

	writer.mu.Lock()
	defer writer.mu.Unlock()

	_, err := writer.file.Seek(12, io.SeekStart)
	if err != nil {
		return err
	}

	buf := make([]byte, 4)
	binary.LittleEndian.PutUint16(buf[0:2], width)
	binary.LittleEndian.PutUint16(buf[2:4], height)
	_, err = writer.file.Write(buf)


	_, err = writer.file.Seek(0, io.SeekEnd)
	return err
}

func (writer *IVFWriter) WriteFrame(frameData []byte, timestamp uint32, isKeyFrame bool) error {
	if !writer.enabled || writer.closed {
		return nil
	}

	writer.mu.Lock()
	defer writer.mu.Unlock()

	header := make([]byte, 12)
	binary.LittleEndian.PutUint32(header[0:4], uint32(len(frameData)))
	binary.LittleEndian.PutUint64(header[4:12], uint64(timestamp))

	_, err := writer.file.Write(header)
	if err != nil {
		return err
	}

	_, err = writer.file.Write(frameData)
	if err != nil {
		return err
	}

	writer.frameCount++

	if writer.frameCount%100 == 0 {
		currentPos, _ := writer.file.Seek(0, io.SeekCurrent)
		writer.file.Seek(24, io.SeekStart)
		binary.Write(writer.file, binary.LittleEndian, uint32(writer.frameCount))
		writer.file.Seek(currentPos, io.SeekStart)
	}

	return nil
}

func (writer *IVFWriter) Close() error {
	if !writer.enabled || writer.closed {
		return nil
	}

	writer.mu.Lock()
	defer writer.mu.Unlock()

	// Update final frame count
	writer.file.Seek(24, io.SeekStart)
	binary.Write(writer.file, binary.LittleEndian, uint32(writer.frameCount))

	writer.closed = true
	return writer.file.Close()
}
