package utils

import (
	"github.com/pion/rtp"
	"time"
	log "github.com/sirupsen/logrus"
)

type JitterBuffer struct {
	nextStart int64
	end       int64
	packets   []*rtp.Packet
	size      int64
	missingPacketTimers map[int64]time.Time
	maxWaitTime         time.Duration
	lastSkippedSeq      int64
	// TODO add ctx for logging
}

func NewJitterBuffer(
	size uint16,
) *JitterBuffer {
	return &JitterBuffer{
		packets: make([]*rtp.Packet, size),
		size:    int64(size),
		missingPacketTimers: make(map[int64]time.Time),
		// NACK retransmission timeout is 10*40u ms. Add +50ms here to guarantee
		// that we don't skip packets that are still in transit.
		maxWaitTime:        time.Millisecond * 450,
		lastSkippedSeq:     0,
	}
}

func (s *JitterBuffer) Add(seq int64, packet *rtp.Packet) bool {
	if s.end-seq >= s.size {
		log.Warnf("Jitter buffer overflow: seq=%d, end=%d, size=%d", seq, s.end, s.size)
		return false
	}

	if seq <= s.end && s.packets[seq%s.size] != nil {
		log.Tracef("Duplicate packet in jitter buffer: seq=%d end=%d", seq, s.end)
		return false
	}

	if s.nextStart == 0 {
		log.Tracef("First packet in jitter buffer: seq=%d", seq)
		s.nextStart = seq
	}

	if seq > s.end {
		if seq-s.end >= s.size {
			log.Warnf("Large sequence jump in jitter buffer: from=%d to=%d, diff=%d, resetting buffer",
				s.end, seq, seq-s.end)
			s.packets = make([]*rtp.Packet, s.size)
		} else {
			if seq-s.end > 1 {
				log.Debugf("Sequence gap in jitter buffer: expected=%d, got=%d, missing=%d packets",
					s.end+1, seq, seq-s.end-1)
			}
			for i := s.end + 1; i < seq; i++ {
				s.packets[i%s.size] = nil
			}
		}
		s.end = seq
	}

	if s.nextStart < s.end-s.size+1 {
		oldStart := s.nextStart
		s.nextStart = s.end - s.size + 1
		log.Debugf("Jitter buffer sli_wi: old_start=%d, new_start=%d, dropped=%d packets",
			oldStart, s.nextStart, s.nextStart-oldStart)
	}

	s.packets[seq%s.size] = packet
	return true
}

func (s *JitterBuffer) NextPackets() ([]*rtp.Packet, bool) {
	skippedPacket := false

	if s.nextStart > s.end {
		return nil, false
	}

	if s.packets[s.nextStart%s.size] == nil {
		log.Tracef("Missing packet at buffer start: seq=%d", s.nextStart)

		if _, exists := s.missingPacketTimers[s.nextStart]; !exists {
			s.missingPacketTimers[s.nextStart] = time.Now()
			log.Debugf("Started tracking missing packet: seq=%d", s.nextStart)
			return nil, false
		}

		if time.Since(s.missingPacketTimers[s.nextStart]) < s.maxWaitTime {
			return nil, false
		}

		log.Warnf("Skipping missing packet after timeout: seq=%d", s.nextStart)
		delete(s.missingPacketTimers, s.nextStart)
		s.lastSkippedSeq = s.nextStart
		s.nextStart++
		skippedPacket = true

		pkts, moreSkips := s.NextPackets()

		if pkts == nil {
			return nil, skippedPacket
		}

		return pkts, skippedPacket || moreSkips
	}

	delete(s.missingPacketTimers, s.nextStart)

	end := s.end // return until sequence end unless a there is a missing packet
	for i := s.nextStart + 1; i <= s.end; i++ {
		if s.packets[i%s.size] == nil {
			end = i - 1 // missing packet found, return until previous packet
			log.Debugf("Missing packet in jitter buffer: seq=%d, returning packets %d-%d",
				i, s.nextStart, end)
			break
		}
	}

	packets := make([]*rtp.Packet, 0, end-s.nextStart+1)
	for i := s.nextStart; i <= end; i++ {
		packets = append(packets, s.packets[i%s.size])
		s.packets[i%s.size] = nil
	}

	s.nextStart = end + 1

	return packets, skippedPacket
}

// SkipPacket forces the jitter buffer to skip waiting for a specific sequence number
func (s *JitterBuffer) SkipPacket(seq int64) {
	if seq == s.nextStart {
		log.Debugf("Skipping missing packet in jitter buffer: seq=%d", seq)
			s.lastSkippedSeq = seq
			s.SetNextPacketsStart(seq + 1)
	}
}

func (s *JitterBuffer) SetNextPacketsStart(nextPacketsStart int64) {
	if s.nextStart < nextPacketsStart {
		log.Debugf("Setting next packet start in jitter buffer: old=%d, new=%d", s.nextStart, nextPacketsStart)
		for i := s.nextStart; i < nextPacketsStart; i++ {
			s.packets[i%s.size] = nil
		}
		s.nextStart = nextPacketsStart
	}
}

func (s *JitterBuffer) GetNextStart() int64 {
	return s.nextStart
}

func (s *JitterBuffer) GetAndClearLastSkipped() (int64, bool) {
	if s.lastSkippedSeq == 0 {
		return 0, false
	}

	skipped := s.lastSkippedSeq
	s.lastSkippedSeq = 0

	return skipped, true
}
