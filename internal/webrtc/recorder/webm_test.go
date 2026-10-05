package recorder

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/at-wat/ebml-go/mkvcore"
	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// testSessionContext keys the session by the "session" string, as the
// recorder reads it back.
func testSessionContext() context.Context {
	//nolint:staticcheck // SA1029: the key the recorder reads
	return context.WithValue(context.Background(), "session", "test-session")
}

func TestWebmRecorder_ConcurrentAudioPush(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	// Initialize recorder with small buffer sizes to increase chance of race conditions
	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		10,    // audioPacketQueueSize
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)

	rec.SetHasAudio(true)

	var wg sync.WaitGroup

	numGoroutines := 10
	packetsPerGoroutine := 100

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()

			for j := 0; j < packetsPerGoroutine; j++ {
				packet := &rtp.Packet{
					Header: rtp.Header{
						SequenceNumber: uint16(goroutineID*packetsPerGoroutine + j), // #nosec G115 - controlled test values won't overflow
						Timestamp:      uint32(j * 1000),                            // #nosec G115 - controlled test values won't overflow
					},
					Payload: make([]byte, 100),
				}

				// Simulate network conditions with per-goroutine delays
				time.Sleep(time.Duration(goroutineID%10) * time.Millisecond)

				rec.PushAudio(packet)
			}
		}(i)
	}

	wg.Wait()

	rec.Close()
}

func TestWebmRecorder_AudioBufferOverflow(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		5,     // audioPacketQueueSize (very small to force overflow)
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)

	rec.SetHasAudio(true)

	// Push more packets than the buffer can hold
	for i := 0; i < 20; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 1000), // #nosec G115 - controlled test values won't overflow
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	rec.Close()
}

func TestWebmRecorder_DisorderedAudioPackets(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		10,    // audioPacketQueueSize
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)

	rec.SetHasAudio(true)

	// Push packets in reverse order
	for i := 10; i > 0; i-- {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 1000), // #nosec G115 - controlled test values won't overflow
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	rec.Close()
}

func TestWebmRecorder_NilAudioPacket(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		10,    // audioPacketQueueSize
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)

	rec.SetHasAudio(true)

	rec.PushAudio(nil)

	validPacket := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 1,
			Timestamp:      1000,
		},
		Payload: make([]byte, 100),
	}
	rec.PushAudio(validPacket)

	rec.Close()
}

func TestWebmRecorder_EmptyAudioPacket(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		10,    // audioPacketQueueSize
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)

	rec.SetHasAudio(true)

	packet := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 1,
			Timestamp:      1000,
		},
		Payload: []byte{},
	}
	rec.PushAudio(packet)

	rec.Close()
}

func TestWebmRecorder_RapidAudioPacketSequence(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		10,    // audioPacketQueueSize
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)

	rec.SetHasAudio(true)

	for i := 0; i < 100; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 1000), // #nosec G115 - controlled test values won't overflow
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	rec.Close()
}

// TestWebmRecorder_SampleBuilderRaceCondition tests the specific race condition
// that can occur when packets are being released while being accessed
func TestWebmRecorder_SampleBuilderRaceCondition(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	// Use very small buffer size to increase chance of race conditions
	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		5,     // videoPacketQueueSize
		5,     // audioPacketQueueSize
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	// The second goroutine swaps recorders while the first pushes.
	var current atomic.Pointer[WebmRecorder]
	current.Store(rec)

	var wg sync.WaitGroup

	// Goroutine 1: Rapidly push packets
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			packet := &rtp.Packet{
				Header: rtp.Header{
					SequenceNumber: uint16(i),
					Timestamp:      uint32(i * 1000), // #nosec G115 - controlled test values won't overflow
				},
				Payload: make([]byte, 100),
			}
			current.Load().PushAudio(packet)
			// Add a small delay to allow for race conditions
			time.Sleep(time.Microsecond)
		}
	}()

	// Goroutine 2: Rapidly close and reopen the recorder
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			current.Load().Close()
			next := NewWebmRecorder(
				tmpFile.Name(),
				0644,
				5,
				5,
				false,
				false,
			)
			next.WithContext(ctx)
			next.SetHasAudio(true)
			current.Store(next)
			time.Sleep(time.Millisecond * 10)
		}
	}()

	wg.Wait()
}

// TestWebmRecorder_PacketReleaseRace tests the specific scenario where
// packets are being released while the samplebuilder is trying to access them
func TestWebmRecorder_PacketReleaseRace(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		5,
		5,
		false,
		false,
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	var wg sync.WaitGroup

	releaseSignal := make(chan struct{})

	// Goroutine 1: Push packets and signal when to start releasing
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 100; i++ {
			packet := &rtp.Packet{
				Header: rtp.Header{
					SequenceNumber: uint16(i),
					Timestamp:      uint32(i * 1000), // #nosec G115 - controlled test values won't overflow
				},
				Payload: make([]byte, 100),
			}
			rec.PushAudio(packet)
		}
		close(releaseSignal)
	}()

	// Goroutine 2: Wait for signal and then rapidly release packets
	wg.Add(1)
	go func() {
		defer wg.Done()
		<-releaseSignal
		for i := 0; i < 10; i++ {
			rec.Close()
			rec = NewWebmRecorder(
				tmpFile.Name(),
				0644,
				5,
				5,
				false,
				false,
			)
			rec.WithContext(ctx)
			rec.SetHasAudio(true)
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
}

// TestWebmRecorder_ConcurrentPushPop tests that concurrent push and pop
// operations do not panic
func TestWebmRecorder_ConcurrentPushPop(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		5,
		5,
		false,
		false,
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	// The second goroutine swaps recorders while the first pushes.
	var current atomic.Pointer[WebmRecorder]
	current.Store(rec)

	var wg sync.WaitGroup

	// Goroutine 1: Push packets
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 1000; i++ {
			packet := &rtp.Packet{
				Header: rtp.Header{
					SequenceNumber: uint16(i),
					Timestamp:      uint32(i * 1000), // #nosec G115 - controlled test values won't overflow
				},
				Payload: make([]byte, 100),
			}
			current.Load().PushAudio(packet)
		}
	}()

	// Goroutine 2: Rapidly close and reopen
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 10; i++ {
			current.Load().Close()
			next := NewWebmRecorder(
				tmpFile.Name(),
				0644,
				5,
				5,
				false,
				false,
			)
			next.WithContext(ctx)
			next.SetHasAudio(true)
			current.Store(next)
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()
}

// TestWebmRecorder_BufferOverflowWithRelease tests buffer overflow
// combined with packet release
func TestWebmRecorder_BufferOverflowWithRelease(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		5,
		5,
		false,
		false,
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	// Fill the buffer to capacity
	for i := 0; i < 10; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 1000), // #nosec G115 - controlled test values won't overflow
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	// Close the recorder while buffer is full
	rec.Close()

	// Reopen and immediately push more packets
	rec = NewWebmRecorder(
		tmpFile.Name(),
		0644,
		5,
		5,
		false,
		false,
	)
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	for i := 10; i < 20; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 1000), // #nosec G115 - controlled test values won't overflow
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	rec.Close()
}

// TestWebmRecorder_DisorderedPacketsWithRelease tests handling of
// disordered packets combined with packet release
func TestWebmRecorder_DisorderedPacketsWithRelease(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		5,
		5,
		false,
		false,
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	// Push packets in reverse order
	for i := 10; i > 0; i-- {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 1000), // #nosec G115 - controlled test values won't overflow
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	// Close while packets are still being processed
	rec.Close()

	// Reopen and push more packets
	rec = NewWebmRecorder(
		tmpFile.Name(),
		0644,
		5,
		5,
		false,
		false,
	)
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	for i := 11; i < 20; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 1000), // #nosec G115 - controlled test values won't overflow
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	rec.Close()
}

// TestWebmRecorder_Audio62PacketGap guards against a panic on a 62-packet
// gap in the audio stream
func TestWebmRecorder_Audio62PacketGap(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		100,   // audioPacketQueueSize - using larger buffer to ensure we can handle the gap
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	for i := 0; i < 50; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 960), // Opus typical frame size
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	gapPacket := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 50 + 62, // Create exactly 62 packet gap
			Timestamp:      uint32((50 + 62) * 960),
		},
		Payload: make([]byte, 100),
	}

	rec.PushAudio(gapPacket)

	for i := 0; i < 50; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(50 + 62 + 1 + i),
				Timestamp:      uint32((50 + 62 + 1 + i) * 960),
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	rec.Close()
}

// TestWebmRecorder_AudioPacketGaps tests various gap sizes to see if any cause panics
func TestWebmRecorder_AudioPacketGaps(t *testing.T) {
	gapSizes := []int{1, 10, 30, 61, 62, 63, 64, 100, 200}

	for _, gapSize := range gapSizes {
		t.Run(fmt.Sprintf("Gap_%d", gapSize), func(t *testing.T) {
			tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
			require.NoError(t, err)
			defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

			rec := NewWebmRecorder(
				tmpFile.Name(),
				0644,
				10,    // videoPacketQueueSize
				100,   // audioPacketQueueSize
				false, // useCustomSampler
				false, // writeIVFCopy
			)

			ctx := testSessionContext()
			rec.WithContext(ctx)
			rec.SetHasAudio(true)

			for i := 0; i < 50; i++ {
				packet := &rtp.Packet{
					Header: rtp.Header{
						SequenceNumber: uint16(i),
						Timestamp:      uint32(i * 960),
					},
					Payload: make([]byte, 100),
				}
				rec.PushAudio(packet)
			}

			gapPacket := &rtp.Packet{
				Header: rtp.Header{
					SequenceNumber: uint16(50 + gapSize),         // #nosec G115 - controlled test values won't overflow
					Timestamp:      uint32((50 + gapSize) * 960), // #nosec G115 - controlled test values won't overflow
				},
				Payload: make([]byte, 100),
			}
			rec.PushAudio(gapPacket)

			for i := 0; i < 50; i++ {
				packet := &rtp.Packet{
					Header: rtp.Header{
						SequenceNumber: uint16(50 + gapSize + 1 + i),         // #nosec G115 - controlled test values won't overflow
						Timestamp:      uint32((50 + gapSize + 1 + i) * 960), // #nosec G115 - controlled test values won't overflow
					},
					Payload: make([]byte, 100),
				}
				rec.PushAudio(packet)
			}

			rec.Close()
		})
	}
}

// TestWebmRecorder_Audio62PacketGapConcurrent tests the 62-packet gap with
// the packets after it pushed concurrently
func TestWebmRecorder_Audio62PacketGapConcurrent(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		100,   // audioPacketQueueSize
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	var wg sync.WaitGroup

	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < 50; i++ {
			packet := &rtp.Packet{
				Header: rtp.Header{
					SequenceNumber: uint16(i),
					Timestamp:      uint32(i * 960),
				},
				Payload: make([]byte, 100),
			}
			rec.PushAudio(packet)
			time.Sleep(time.Millisecond)
		}
	}()

	wg.Wait()

	gapPacket := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 50 + 62, // Create exactly 62 packet gap
			Timestamp:      uint32((50 + 62) * 960),
		},
		Payload: make([]byte, 100),
	}
	rec.PushAudio(gapPacket)

	numGoroutines := 5
	packetsPerGoroutine := 20

	for i := 0; i < numGoroutines; i++ {
		wg.Add(1)
		go func(goroutineID int) {
			defer wg.Done()
			startSeq := 50 + 62 + 1 + (goroutineID * packetsPerGoroutine)

			for j := 0; j < packetsPerGoroutine; j++ {
				packet := &rtp.Packet{
					Header: rtp.Header{
						SequenceNumber: uint16(startSeq + j),         // #nosec G115 - controlled test values won't overflow
						Timestamp:      uint32((startSeq + j) * 960), // #nosec G115 - controlled test values won't overflow
					},
					Payload: make([]byte, 100),
				}
				rec.PushAudio(packet)
				time.Sleep(time.Millisecond)
			}
		}(i)
	}

	wg.Wait()

	rec.Close()
}

// TestWebmRecorder_ExactIssueScenario replays the 62-packet gap at the
// sequence numbers of the original panic
func TestWebmRecorder_ExactIssueScenario(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		100,   // audioPacketQueueSize
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	startSeq := uint16(49800)
	for i := uint16(0); i < 62; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: startSeq + i,
				Timestamp:      uint32((startSeq + i) * 960),
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	defer func() {
		if r := recover(); r != nil {
			assert.Failf(t, "test panicked", "%v", r)
		}
	}()

	gapPacket := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: startSeq + 62 + 62, // Exact 62 packet gap
			Timestamp:      uint32((startSeq + 62 + 62) * 960),
		},
		Payload: make([]byte, 100),
	}

	rec.PushAudio(gapPacket)

	for i := uint16(0); i < 10; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: startSeq + 62 + 62 + 1 + i,
				Timestamp:      uint32((startSeq + 62 + 62 + 1 + i) * 960),
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	rec.Close()
}

// TestWebmRecorder_HighSequenceNumber62Gap tests if starting with a high sequence number
// and then having a 62-packet gap panics
func TestWebmRecorder_HighSequenceNumber62Gap(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		100,   // audioPacketQueueSize
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	// Start with a high sequence number (close to wrapping around)
	startSeq := uint16(65500)
	for i := uint16(0); i < 20; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: startSeq + i,
				Timestamp:      uint32((startSeq + i) * 960),
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	gapPacket := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: startSeq + 20 + 62, // Will wrap around
			Timestamp:      uint32((startSeq + 20 + 62) * 960),
		},
		Payload: make([]byte, 100),
	}
	rec.PushAudio(gapPacket)

	rec.Close()
}

// TestWebmRecorder_Multiple62Gaps tests multiple 62-packet gaps in sequence
func TestWebmRecorder_Multiple62Gaps(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		100,   // audioPacketQueueSize
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	for i := 0; i < 50; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 960),
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(packet)
	}

	currentSeq := uint16(50)
	for j := 0; j < 5; j++ {
		currentSeq += 62

		gapPacket := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: currentSeq,
				Timestamp:      uint32(currentSeq * 960),
			},
			Payload: make([]byte, 100),
		}
		rec.PushAudio(gapPacket)

		for i := 0; i < 10; i++ {
			currentSeq++
			packet := &rtp.Packet{
				Header: rtp.Header{
					SequenceNumber: currentSeq,
					Timestamp:      uint32(currentSeq * 960),
				},
				Payload: make([]byte, 100),
			}
			rec.PushAudio(packet)
		}
	}

	rec.Close()
}

// TestWebmRecorder_Audio62Opus tests 62-packet gap with Opus-like data
func TestWebmRecorder_Audio62Opus(t *testing.T) {
	tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
	require.NoError(t, err)
	defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

	rec := NewWebmRecorder(
		tmpFile.Name(),
		0644,
		10,    // videoPacketQueueSize
		100,   // audioPacketQueueSize
		false, // useCustomSampler
		false, // writeIVFCopy
	)

	ctx := testSessionContext()
	rec.WithContext(ctx)
	rec.SetHasAudio(true)

	// Create fake Opus packet (just need header bytes that look like Opus)
	opusData := []byte{0xF8, 0xFF, 0xFE}
	opusData = append(opusData, make([]byte, 97)...)

	for i := 0; i < 50; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(i),
				Timestamp:      uint32(i * 960),
				PayloadType:    111, // Typical Opus payload type
			},
			Payload: opusData,
		}
		rec.PushAudio(packet)
	}

	gapPacket := &rtp.Packet{
		Header: rtp.Header{
			SequenceNumber: 50 + 62,
			Timestamp:      uint32((50 + 62) * 960),
			PayloadType:    111,
		},
		Payload: opusData,
	}
	rec.PushAudio(gapPacket)

	for i := 0; i < 50; i++ {
		packet := &rtp.Packet{
			Header: rtp.Header{
				SequenceNumber: uint16(50 + 62 + 1 + i),
				Timestamp:      uint32((50 + 62 + 1 + i) * 960),
				PayloadType:    111,
			},
			Payload: opusData,
		}
		rec.PushAudio(packet)
	}

	rec.Close()
}

// TestWebmRecorder_AdditionalEdgeCases tests edge cases that might combine
// with the 62-packet gap
func TestWebmRecorder_AdditionalEdgeCases(t *testing.T) {
	testCases := []struct {
		name     string
		testFunc func(t *testing.T)
	}{
		{"BufferSizeEqual62", func(t *testing.T) {
			tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
			require.NoError(t, err)
			defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

			rec := NewWebmRecorder(
				tmpFile.Name(),
				0644,
				10,    // videoPacketQueueSize
				62,    // audioPacketQueueSize - exact same as the gap
				false, // useCustomSampler
				false, // writeIVFCopy
			)

			ctx := testSessionContext()
			rec.WithContext(ctx)
			rec.SetHasAudio(true)

			// Fill the buffer exactly
			for i := 0; i < 62; i++ {
				packet := &rtp.Packet{
					Header: rtp.Header{
						SequenceNumber: uint16(i),
						Timestamp:      uint32(i * 960),
					},
					Payload: make([]byte, 100),
				}
				rec.PushAudio(packet)
			}

			gapPacket := &rtp.Packet{
				Header: rtp.Header{
					SequenceNumber: 62 + 62,
					Timestamp:      uint32((62 + 62) * 960),
				},
				Payload: make([]byte, 100),
			}
			rec.PushAudio(gapPacket)

			rec.Close()
		}},
		{"RapidPushAfterGap", func(t *testing.T) {
			tmpFile, err := os.CreateTemp("", "test-recording-*.webm")
			require.NoError(t, err)
			defer os.Remove(tmpFile.Name()) //nolint:errcheck // Test cleanup, error not critical

			rec := NewWebmRecorder(
				tmpFile.Name(),
				0644,
				10,    // videoPacketQueueSize
				100,   // audioPacketQueueSize
				false, // useCustomSampler
				false, // writeIVFCopy
			)

			ctx := testSessionContext()
			rec.WithContext(ctx)
			rec.SetHasAudio(true)

			for i := 0; i < 50; i++ {
				packet := &rtp.Packet{
					Header: rtp.Header{
						SequenceNumber: uint16(i),
						Timestamp:      uint32(i * 960),
					},
					Payload: make([]byte, 100),
				}
				rec.PushAudio(packet)
			}

			gapPacket := &rtp.Packet{
				Header: rtp.Header{
					SequenceNumber: 50 + 62,
					Timestamp:      uint32((50 + 62) * 960),
				},
				Payload: make([]byte, 100),
			}
			rec.PushAudio(gapPacket)

			for i := 0; i < 200; i++ {
				packet := &rtp.Packet{
					Header: rtp.Header{
						SequenceNumber: uint16(50 + 62 + 1 + i),
						Timestamp:      uint32((50 + 62 + 1 + i) * 960),
					},
					Payload: make([]byte, 100),
				}
				rec.PushAudio(packet)
			}

			rec.Close()
		}},
	}

	for _, tc := range testCases {
		t.Run(tc.name, tc.testFunc)
	}
}

// TestPacketRetentionAndCopying tests to ensure packets are properly copied and not shared
func TestPacketRetentionAndCopying(t *testing.T) {
	file := filepath.Join(t.TempDir(), "audio.webm")
	rec := NewWebmRecorder(file, 0o600, 10, 100, false, false)
	rec.WithContext(testSessionContext())
	rec.SetHasAudio(true)

	// Every packet shares one payload buffer, overwritten by the next push,
	// as a caller that reuses its read buffer does. Frame n carries byte n.
	payload := make([]byte, 100)
	push := func(seq uint16) {
		for i := range payload {
			payload[i] = byte(seq)
		}
		rec.PushAudio(&rtp.Packet{
			Header:  rtp.Header{SequenceNumber: seq, Timestamp: uint32(seq) * 960},
			Payload: payload,
		})
	}

	// 10 arrives late, so the frames after it wait in the recorder.
	for seq := range uint16(100) {
		switch seq {
		case 10:
		case 30:
			push(10)
			push(seq)
		default:
			push(seq)
		}
	}
	clear(payload)

	rec.Close()

	f, err := os.Open(file)
	require.NoError(t, err)
	defer func() { _ = f.Close() }()

	readers, err := mkvcore.NewSimpleBlockReader(f)
	require.NoError(t, err)
	require.Len(t, readers, 1)

	blocks := 0
	for {
		data, _, ts, err := readers[0].Read()
		if err == io.EOF {
			break
		}
		require.NoError(t, err)
		// 20 ms frames: a block's PTS names the frame it must hold.
		require.Equal(t, bytes.Repeat([]byte{byte(ts / 20)}, len(payload)), data, "block at %d ms", ts)
		blocks++
	}
	assert.Equal(t, 100, blocks)
}
