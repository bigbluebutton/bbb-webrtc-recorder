package utils

import (
	"context"
	"github.com/pion/rtp"
	"time"
	log "github.com/sirupsen/logrus"
)

type JitterBuffer struct {
	nextStart            int64
	end                  int64
	packets              []*rtp.Packet
	size                 int64
	missingPacketTimers  map[int64]time.Time
	packetTimeout        time.Duration
	lastSkippedSeq       int64
	ctx                  context.Context
}

func NewJitterBuffer(
	size          uint16,
	packetTimeout uint16,
	ctx           context.Context,
) *JitterBuffer {
	log.WithField("session", ctx.Value("session")).
		Tracef("Created jitter buffer: size=%d, packet_timeout=%d", size, packetTimeout)

	return &JitterBuffer{
		packets: make([]*rtp.Packet, size),
		size:    int64(size),
		missingPacketTimers: make(map[int64]time.Time),
		packetTimeout: 			time.Duration(packetTimeout) * time.Millisecond,
		lastSkippedSeq:     0,
		ctx: ctx,
	}
}

func (s *JitterBuffer) Add(seq int64, packet *rtp.Packet) bool {
	if s.end-seq >= s.size {
		log.WithField("session", s.ctx.Value("session")).
			Warnf("Jitter buffer overflow: seq=%d, end=%d, size=%d", seq, s.end, s.size)
		return false
	}

	if seq <= s.end && s.packets[seq%s.size] != nil {
		log.WithField("session", s.ctx.Value("session")).
			Tracef("Duplicate packet in jitter buffer: seq=%d end=%d", seq, s.end)
		return false
	}

	if s.nextStart == 0 {
		log.WithField("session", s.ctx.Value("session")).
			Tracef("First packet in jitter buffer: seq=%d", seq)
		s.nextStart = seq
	}

	if seq > s.end {
		if seq-s.end >= s.size {
			log.WithField("session", s.ctx.Value("session")).
				Warnf("Large sequence jump in jitter buffer: from=%d to=%d, diff=%d, resetting buffer",
					s.end, seq, seq-s.end)
			s.packets = make([]*rtp.Packet, s.size)
		} else {
			if seq-s.end > 1 {
				log.WithField("session", s.ctx.Value("session")).
					Debugf("Sequence gap in jitter buffer: expected=%d, got=%d, missing=%d packets",
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
		log.WithField("session", s.ctx.Value("session")).
			Debugf("Jitter buffer sli_wi: old_start=%d, new_start=%d, dropped=%d packets",
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
		log.WithField("session", s.ctx.Value("session")).
			Tracef("Missing packet at buffer start: seq=%d", s.nextStart)

		if _, exists := s.missingPacketTimers[s.nextStart]; !exists {
			s.missingPacketTimers[s.nextStart] = time.Now()
			log.WithField("session", s.ctx.Value("session")).
				Debugf("Started tracking missing packet: seq=%d", s.nextStart)

			return nil, false
		}

		if time.Since(s.missingPacketTimers[s.nextStart]) < s.packetTimeout {
			return nil, false
		}

		log.WithField("session", s.ctx.Value("session")).
			Warnf("Skipping missing packet after timeout: seq=%d", s.nextStart)
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
			log.WithField("session", s.ctx.Value("session")).
				Debugf("Missing packet in jitter buffer: seq=%d, returning packets %d-%d",
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
		log.WithField("session", s.ctx.Value("session")).
			Debugf("Skipping missing packet in jitter buffer: seq=%d", seq)
				s.lastSkippedSeq = seq
				s.SetNextPacketsStart(seq + 1)
	}
}

func (s *JitterBuffer) SetNextPacketsStart(nextPacketsStart int64) {
	if s.nextStart < nextPacketsStart {
		log.WithField("session", s.ctx.Value("session")).
			Debugf("Setting next packet start in jitter buffer: old=%d, new=%d", s.nextStart, nextPacketsStart)

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
