// Command seedgen writes the synthetic and pcap-extract traces of the LiveKit
// replay seed corpus. The expect files beside them are written by hand; see
// internal/webrtc/livekit/testdata/corpus/README.md.
//
// Usage, from the repository root:
//
//	go run ./internal/corpus/seedgen [-out internal/webrtc/livekit/testdata/corpus]
package main

import (
	"embed"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand/v2"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
)

// schedules holds the read schedules extracted from the header-only pcaps of
// live test runs, which are not in the repository.
//
//go:embed schedules/*.json
var schedules embed.FS

const keyMin = 10 // packets: the captures' deltas are 6-8, their key frames 11-21

func seedp(v int64) *int64          { return &v }
func ms(atUs int64) float64         { return float64(atUs) / 1000 }
func endAt(r []corpus.Record) int64 { return r[len(r)-1].AtUs }

type seed struct {
	name, class string
	tracks      []*corpus.Trace
	suffixes    []string // "" for a single track
}

func must[T any](v T, err error) T {
	if err != nil {
		fmt.Fprintln(os.Stderr, "seedgen:", err)
		os.Exit(1)
	}
	return v
}

// frames returns, per frame in order of first appearance, the record indices
// of its original packets.
func frames(recs []corpus.Record) [][]int {
	var out [][]int
	at := map[uint32]int{}
	for i, r := range recs {
		if r.T != corpus.RecordRTP || r.RTX || r.Dup {
			continue
		}
		f, ok := at[r.TS]
		if !ok {
			f = len(out)
			at[r.TS] = f
			out = append(out, nil)
		}
		out[f] = append(out[f], i)
	}
	return out
}

func seqOf(recs []corpus.Record, f, j int) uint16 { return recs[frames(recs)[f][j]].Seq }
func atOf(recs []corpus.Record, f, j int) int64   { return recs[frames(recs)[f][j]].AtUs }

func synth(t *corpus.Trace, gen string, sd *int64, trackID, note string) *corpus.Trace {
	t.Header.Generator = gen
	t.Header.Seed = sd
	t.Header.TrackID = trackID
	t.Header.Note = note
	return t
}

type schedPkt struct {
	Seq  uint16 `json:"seq"`
	TUs  int64  `json:"tUs"`
	TS   uint32 `json:"ts"`
	M    int    `json:"m"`
	Pad  int    `json:"pad"`
	PT   uint8  `json:"pt"`
	PLen int    `json:"plen"`
}

type d1Packet struct {
	Seq uint16 `json:"seq"`
	M   int    `json:"m"`
}

type schedFile struct {
	Pcap    string     `json:"pcap"`
	SSRC    uint32     `json:"ssrc"`
	Packets []schedPkt `json:"packets"`
}

func loadSched(name string) schedFile {
	raw := must(schedules.ReadFile("schedules/" + name + ".json"))
	var s schedFile
	must(0, json.Unmarshal(raw, &s))
	return s
}

// fromSchedule turns a captured read schedule into records with synthesized
// header-mode VP8 payloads, leaving out the packets in drop.
func fromSchedule(pk []schedPkt, drop map[uint16]bool) (recs []corpus.Record) {
	type fr struct {
		n     int
		head  uint16
		key   bool
		blank bool
		pic   uint16
	}
	base := pk[0].Seq
	byTS := map[uint32]*fr{}
	var order []*fr
	for _, p := range pk {
		if p.Pad == 1 {
			continue
		}
		f, ok := byTS[p.TS]
		if !ok {
			f = &fr{head: p.Seq}
			byTS[p.TS] = f
			order = append(order, f)
		}
		f.n++
		if int16(p.Seq-base) < int16(f.head-base) {
			f.head = p.Seq
		}
		// LiveKit's close-out blank frame: one 8x8 VP8 key frame per packet (31 bytes + descriptor).
		f.blank = f.n == 1 && p.M == 1 && p.PLen < 64
	}
	for i, f := range order {
		f.pic = uint16(1000+i) & 0x7FFF
		f.key = i == 0 || f.n >= keyMin || f.blank
	}

	var t0 int64 = -1
	for _, p := range pk {
		if drop[p.Seq] {
			continue
		}
		if t0 < 0 {
			t0 = p.TUs
		}
		r := corpus.Record{T: corpus.RecordRTP, AtUs: p.TUs - t0, Seq: p.Seq, TS: p.TS, M: p.M == 1, PT: p.PT}
		if p.Pad == 0 {
			f := byTS[p.TS]
			w, h := 640, 480
			if f.blank {
				w, h = 8, 8
			}
			r.Pay = corpus.VP8Payload(f.pic, p.Seq == f.head, f.key, w, h)
			r.PLen = max(p.PLen, len(r.Pay))
		}
		recs = append(recs, r)
	}
	return recs
}

func pcapHeader(ssrc uint32, trackID, note string) corpus.Header {
	h := corpus.NewSyntheticHeader(corpus.KindVideo, ssrc, "fromSchedule: a captured read schedule with synthesized VP8 headers")
	h.Provenance = corpus.ProvenancePcapExtract
	h.TrackID = trackID
	h.Note = note
	return h
}

const pcapNote = "Read schedule taken from a header-only SRTP pcap of a live test run (fake-device media, no field data): " +
	"seq, ts, marker and capture time per packet; atUs is capture time, used as read time. No media payload: " +
	"pay is a synthesized VP8 descriptor plus frame header from corpus.GenerateVP8's layout, so the writer accepts it; " +
	"S on each RTP timestamp's lowest seq, a picture ID per frame, a key frame for the first frame and for every frame of >=10 packets " +
	"(checked against the subscriber's decoded frame list where one exists); plen estimated from the wire length " +
	"(UDP payload - 12 RTP - 16 SRTP tag), padding-only probes as plen 0. "

func seeds() []seed {
	var out []seed
	ssrc := uint32(0x5EED0000)
	next := func() uint32 { ssrc++; return ssrc }

	// Class 1: paced key frames. Each frame's packets are read over longer
	// than 200 ms, so an expiry counted from the first packet's read drops
	// every frame; the production 2 s window keeps them all.
	{
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 3.4, Frames: 12, KeyPackets: 6, DeltaPackets: 6, KeyInterval: 4,
			SpreadUs: 225000, StartSeq: 45308, StartTS: 3164626761, StartPictureID: 14932, SSRC: next()}))
		t.Records = must(corpus.Apply(t.Records, corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, "corpus.GenerateVP8{FPS:3.4,Frames:12,KeyPackets:6,DeltaPackets:6,KeyInterval:4,SpreadUs:225000}+EOFAt", nil,
			"c01-paced-6x45/camera", "Each frame's 6 packets are read 45 ms apart (225 ms): at a 200 ms video latency none of the 12 frames is written and all 72 packets drop.")
		out = append(out, seed{name: "c01-paced-6x45", class: "1", tracks: []*corpus.Trace{t}})
	}
	{
		// Back-to-back trickle: 11 packets 50 ms apart, the next frame right behind (550 ms period).
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 1e6 / 550000.0, Frames: 8, KeyPackets: 11, DeltaPackets: 11, KeyInterval: 4,
			SpreadUs: 500000, StartSeq: 45308, StartTS: 3164626761, StartPictureID: 14932, SSRC: next()}))
		t.Records = must(corpus.Apply(t.Records, corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, "corpus.GenerateVP8{FPS:1.818,Frames:8,KeyPackets:11,DeltaPackets:11,KeyInterval:4,SpreadUs:500000}+EOFAt", nil,
			"c01-trickle-11x50/camera", "Each frame's 11 packets are read 50 ms apart (500 ms), the next frame right behind: at a 200 ms video latency none of the 8 frames is written and all 88 packets drop.")
		out = append(out, seed{name: "c01-trickle-11x50", class: "1", tracks: []*corpus.Trace{t}})
	}
	{
		s := loadSched("px-ml-ab-1-reccam")
		recs := fromSchedule(s.Packets, nil)
		recs = must(corpus.Apply(recs, corpus.EOFAt(endAt(recs)+50000)))
		t := &corpus.Trace{Header: pcapHeader(s.SSRC, "px-ml-ab-1/rec-camera", pcapNote+
			"Source: the recorder's camera downtrack (SSRC 3731031843) of a live run with mixed join and leave, caught in the keyframe loop. "+
			"Ends with LiveKit's six close-out blank frames, synthesized as 8x8 key frames (VP8KeyFrame8x8 in livekit-server v1.13.7)."), Records: recs}
		out = append(out, seed{name: "c01-px-ml-ab-1-reccam", class: "1", tracks: []*corpus.Trace{t}})
	}

	// Class 2: LiveKit's close-out blank Opus pair, the first one tick past the last frame.
	{
		t := must(corpus.GenerateOpus(corpus.OpusOptions{Frames: 250, StartSeq: 2000, StartTS: 48000, SSRC: next()}))
		last := t.Records[len(t.Records)-1]
		silence := []byte{0xF8, 0xFF, 0xFE} // OpusSilenceFrame prefix, downtrack.go@v1.13.7 (80 bytes)
		t.Records = append(t.Records,
			corpus.Record{T: corpus.RecordRTP, AtUs: last.AtUs + 20000, Seq: last.Seq + 1, TS: last.TS + 1, PT: last.PT, PLen: 80, Pay: silence},
			corpus.Record{T: corpus.RecordRTP, AtUs: last.AtUs + 40000, Seq: last.Seq + 2, TS: last.TS + 1 + 1968, PT: last.PT, PLen: 80, Pay: silence})
		t.Records = must(corpus.Apply(t.Records, corpus.EOFAt(endAt(t.Records)+20000)))
		synth(t, "corpus.GenerateOpus{Frames:250}+blank pair at ts+1 and ts+1+1968+EOFAt", nil, "c02-opus-tail-blank/microphone",
			"As in webm_audio_test.go: the blank pair's first frame lands on the last frame's millisecond.")
		out = append(out, seed{name: "c02-opus-tail-blank", class: "2", tracks: []*corpus.Trace{t}})
	}

	// Class 3: one unrepaired mid-frame loss, request bound by frame rate
	// (trackbuffer_test.go TestTrackBufferLowFrameRateRetransmit bursts: 8 packets
	// 1 ms apart, packet 3 of frame k lost, packet 4 reveals it). A key frame
	// follows ~600-900 ms after the loss, as the publisher's answer would.
	for _, c := range []struct {
		tag            string
		fps            float64
		frames, k, key int
	}{
		{"30fps", 30, 150, 30, 48},
		{"5fps", 5, 30, 10, 14},
		{"3.4fps", 3.4, 24, 6, 9},
	} {
		name := "c03-d11-" + c.tag
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: c.fps, Frames: c.frames, KeyPackets: 8, DeltaPackets: 8, KeyInterval: c.key,
			SpreadUs: 7000, StartSeq: 1000, StartTS: 90000, StartPictureID: 100, SSRC: next()}))
		lost, reveal := seqOf(t.Records, c.k, 2), atOf(t.Records, c.k, 3)
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(lost), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, fmt.Sprintf("corpus.GenerateVP8{FPS:%v,Frames:%d,KeyPackets:8,DeltaPackets:8,KeyInterval:%d,SpreadUs:7000}+DropSeqs(%d)+EOFAt",
			c.fps, c.frames, c.key, lost), nil, name+"/camera", fmt.Sprintf("Packet 3 of frame %d (seq %d) lost, never repaired; seq %d reveals it at %.3f ms.",
			c.k, lost, lost+1, ms(reveal)))
		out = append(out, seed{name: name, class: "3", tracks: []*corpus.Trace{t}})
	}

	// Class 5: repaired losses at 5 fps bring no request. The NACK goes on
	// the first read 20 ms after the reveal (the next frame's first packet), the
	// retransmit lands 70 ms later: once in band, once over RTX (read right
	// after the next main-stream packet).
	{
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 5, Frames: 30, KeyPackets: 8, DeltaPackets: 8, SpreadUs: 7000,
			StartSeq: 1000, StartTS: 90000, StartPictureID: 100, SSRC: next()}))
		r := t.Records
		l1, l2 := seqOf(r, 10, 2), seqOf(r, 20, 2)
		inBand := atOf(r, 11, 0) + 70000
		rtxAt := atOf(r, 22, 0) // 21's first packet + 70 ms lands before 22's
		t.Records = must(corpus.Apply(r, corpus.Retransmit(l1, inBand), corpus.Retransmit(l2, rtxAt), corpus.DropSeqs(l1, l2),
			corpus.EOFAt(endAt(r)+50000)))
		synth(t, fmt.Sprintf("corpus.GenerateVP8{FPS:5,Frames:30,KeyPackets:8,DeltaPackets:8,SpreadUs:7000}+Retransmit(%d,%d)+Retransmit(%d,%d)+DropSeqs+EOFAt",
			l1, inBand, l2, rtxAt), nil, "c05-repaired-5fps/camera",
			"Retransmit records carry rtx:true; the first one's read time is the in-band case of TestTrackBufferLowFrameRateRetransmit, the second the RTX case.")
		out = append(out, seed{name: "c05-repaired-5fps", class: "5", tracks: []*corpus.Trace{t}})
	}

	// Class 9: push-lag window, live schedules.
	{
		var in struct {
			DropSeq uint16     `json:"dropSeq"`
			Packets []d1Packet `json:"packets"`
		}
		must(0, json.Unmarshal(must(schedules.ReadFile("schedules/mx-rev5b-2-drop1.json")), &in))
		full := loadSched("mx-rev5b-2")
		bySeq := map[uint16]schedPkt{}
		for _, p := range full.Packets {
			bySeq[p.Seq] = p
		}
		// Start at the first frame head, after the first marker.
		start := slices.IndexFunc(in.Packets, func(p d1Packet) bool { return p.M == 1 }) + 1
		var pk []schedPkt
		for _, p := range in.Packets[start:] {
			pk = append(pk, bySeq[p.Seq])
		}
		recs := fromSchedule(pk, map[uint16]bool{in.DropSeq: true})
		dropAt := bySeq[in.DropSeq].TUs - pk[0].TUs
		recs = must(corpus.Apply(recs, corpus.EOFAt(endAt(recs)+100000)))
		t := &corpus.Trace{Header: pcapHeader(full.SSRC, "mx-rev5b-2/camera", pcapNote+fmt.Sprintf(
			"Source: a slice of the camera track of a live run with mixed join and leave. "+
				"It starts at a frame head, marked key; seq %d (pos 2/6, would-be read at %.1f ms) is withheld. "+
				"The size rule also marks the last 20-packet frame key, as the subscriber decoded it.", in.DropSeq, ms(dropAt))),
			Records: recs}
		out = append(out, seed{name: "c09-mx-rev5b-2-drop1", class: "9", tracks: []*corpus.Trace{t}})
	}
	{
		const lost = 13100
		full := loadSched("mx-rev5-1")
		recs := fromSchedule(full.Packets, map[uint16]bool{lost: true})
		var dropAt int64
		for _, p := range full.Packets {
			if p.Seq == lost {
				dropAt = p.TUs - full.Packets[0].TUs
			}
		}
		recs = must(corpus.Apply(recs, corpus.EOFAt(endAt(recs)+100000)))
		t := &corpus.Trace{Header: pcapHeader(full.SSRC, "mx-rev5-1/camera", pcapNote+fmt.Sprintf(
			"Source: the camera track of a live run with mixed join and leave. "+
				"Seq %d (pos 1/21 of a paced key frame, would-be read at %.1f ms) is withheld: of every packet withheld in turn, the one "+
				"whose frames reach the writer latest. The live run's injected drops (12979, 13089) are not applied.", lost, ms(dropAt))), Records: recs}
		out = append(out, seed{name: "c09-mx-rev5-1-residual", class: "9", tracks: []*corpus.Trace{t}})
	}

	// Class 10: two unrepaired losses 1.5 s apart, each followed 600 ms later by a key frame.
	{
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 150, KeyPackets: 8, DeltaPackets: 8, KeyInterval: 45, SpreadUs: 7000,
			StartSeq: 1000, StartTS: 90000, StartPictureID: 100, SSRC: next()}))
		l1, l2 := seqOf(t.Records, 27, 2), seqOf(t.Records, 72, 2)
		r1, r2 := atOf(t.Records, 27, 3), atOf(t.Records, 72, 3)
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(l1, l2), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, fmt.Sprintf("corpus.GenerateVP8{FPS:30,Frames:150,KeyPackets:8,DeltaPackets:8,KeyInterval:45,SpreadUs:7000}+DropSeqs(%d,%d)+EOFAt", l1, l2), nil,
			"c10-two-losses-1500ms/camera", fmt.Sprintf("Reveals at %.3f and %.3f ms; the second loss sits behind the first one's 2 s hold.", ms(r1), ms(r2)))
		out = append(out, seed{name: "c10-two-losses-1500ms", class: "10", tracks: []*corpus.Trace{t}})
	}

	// Class 11: duplicates must not underflow PacketsLost.
	{
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 120, KeyPackets: 15, DeltaPackets: 4, KeyInterval: 60, SpreadUs: 2000,
			StartSeq: 1000, StartTS: 90000, StartPictureID: 100, SSRC: next()}))
		r := slices.Clone(t.Records)
		f50 := frames(r)[50]
		late := f50[2]
		lateSeq := r[late].Seq
		r[late].AtUs += 10000 // packet 3 of frame 50 is read 10 ms late
		sort.SliceStable(r, func(i, j int) bool { return r[i].AtUs < r[j].AtUs })
		p1 := seqOf(r, 50, 1)
		dupIn := atOf(r, 50, 1) + 5000
		p80 := seqOf(r, 80, 1)
		dupAfter := atOf(r, 80, 1) + 40000
		t.Records = must(corpus.Apply(r, corpus.Duplicate(p1, dupIn), corpus.Duplicate(p80, dupAfter), corpus.EOFAt(endAt(r)+50000)))
		synth(t, fmt.Sprintf("corpus.GenerateVP8{FPS:30,Frames:120,KeyPackets:15,DeltaPackets:4,KeyInterval:60,SpreadUs:2000}+late(%d,+10ms)+Duplicate(%d,%d)+Duplicate(%d,%d)+EOFAt",
			lateSeq, p1, dupIn, p80, dupAfter), nil, "c11-dups-30fps/camera",
			"A duplicate inside a frame still assembling (frame 50: packet 3 read 10 ms late, a copy of packet 2 in between) and one after its frame left the buffer (frame 80, copy 40 ms later).")
		out = append(out, seed{name: "c11-dups-30fps", class: "11", tracks: []*corpus.Trace{t}})
	}
	{
		// 700 packets/s, 5-packet frames; packet 3 never arrives, so frame 1 holds
		// everything behind it for the full latency, and a copy of seq 60 comes
		// ~1050 sequence numbers behind the newest.
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 140, Frames: 222, KeyPackets: 5, DeltaPackets: 5, KeyInterval: 20, SpreadUs: 5714,
			StartSeq: 1, StartTS: 3000, StartPictureID: 1, SSRC: next()}))
		r := t.Records
		lastAt := endAt(r)
		tail := corpus.Record{T: corpus.RecordRTP, AtUs: 5714 + 2000000 + 1000, Seq: 1111, TS: 3000 + uint32(math.Round(222*90000/140.0)), M: true, PT: 96,
			PLen: 1000, Pay: corpus.VP8Payload(1+222, true, true, 640, 480)}
		r = must(corpus.Apply(r, corpus.DropSeqs(3), corpus.Duplicate(60, lastAt+100)))
		r = append(r, tail)
		t.Records = must(corpus.Apply(r, corpus.EOFAt(tail.AtUs+50000)))
		synth(t, "corpus.GenerateVP8{FPS:140,Frames:222,KeyPackets:5,DeltaPackets:5,KeyInterval:20,SpreadUs:5714}+DropSeqs(3)+Duplicate(60)+key frame seq 1111 after the expiry+EOFAt",
			nil, "c11-dup-held-700pps/camera", "trackbuffer_test.go TestTrackBufferDropsDuplicateOfHeldPacketAtHighRate as a trace.")
		out = append(out, seed{name: "c11-dup-held-700pps", class: "11", tracks: []*corpus.Trace{t}})
	}

	// Class 12: LiveKit's padding probe with RTX resends the last 6 forwarded packets.
	{
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 120, KeyPackets: 15, DeltaPackets: 4, KeyInterval: 60, SpreadUs: 2000,
			StartSeq: 1000, StartTS: 90000, StartPictureID: 100, SSRC: next()}))
		r := t.Records
		fr := frames(r)
		var ops []corpus.Op
		for i, f := 0, 15; f < 115; i, f = i+1, f+15 {
			last := fr[f][len(fr[f])-1] // at the frame boundary
			if i%2 == 1 {
				last = fr[f][1] // mid-frame
			}
			at := r[last].AtUs + 200
			for k := range 6 {
				ops = append(ops, corpus.Retransmit(r[last].Seq-5+uint16(k), at+int64(k)*50))
			}
		}
		ops = append(ops, corpus.EOFAt(endAt(r)+50000))
		t.Records = must(corpus.Apply(r, ops...))
		synth(t, "corpus.GenerateVP8{FPS:30,Frames:120,KeyPackets:15,DeltaPackets:4,KeyInterval:60,SpreadUs:2000}+Retransmit x6 every 15 frames+EOFAt", nil,
			"c12-lkprobe-rtxdup/camera", "LiveKit's RTX padding probe, at a frame boundary and mid-frame in turn: probes at frames 15, 45, 75, 105 after the marker, "+
				"30, 60, 90 after the frame's second packet; copies 0.2 ms after, 50 µs apart (WriteProbePackets in livekit-server v1.13.7).")
		out = append(out, seed{name: "c12-lkprobe-rtxdup", class: "12", tracks: []*corpus.Trace{t}})
	}

	// Class 15: stream start.
	{
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 120, KeyPackets: 15, DeltaPackets: 4, KeyInterval: 60, SpreadUs: 7000,
			StartSeq: 1000, StartTS: 90000, StartPictureID: 100, SSRC: next()}))
		t.Records = must(corpus.Apply(t.Records, corpus.StartAt(3), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, "corpus.GenerateVP8{FPS:30,Frames:120,KeyPackets:15,DeltaPackets:4,KeyInterval:60,SpreadUs:7000}+StartAt(3)+EOFAt", nil,
			"c15-start-midframe/camera", "The subscription starts at packet 4 of the 15-packet first key frame; next key frame at frame 60 (2 s).")
		out = append(out, seed{name: "c15-start-midframe", class: "15", tracks: []*corpus.Trace{t}})
	}
	{
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 120, KeyPackets: 15, DeltaPackets: 4, KeyInterval: 60, SpreadUs: 7000,
			StartSeq: 1000, StartTS: 90000, StartPictureID: 100, SSRC: next()}))
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(1005), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, "corpus.GenerateVP8{FPS:30,Frames:120,KeyPackets:15,DeltaPackets:4,KeyInterval:60,SpreadUs:7000}+DropSeqs(1005)+EOFAt", nil,
			"c15-start-kfloss/camera", "Packet 6 of the first key frame (seq 1005) lost, never repaired.")
		out = append(out, seed{name: "c15-start-kfloss", class: "15", tracks: []*corpus.Trace{t}})
	}

	// Class 17: eof right after a gap. Frame 60 loses a middle packet;
	// a key frame and 4 deltas follow, complete but held behind it, and the
	// track ends before the gap request is due.
	{
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 66, KeyPackets: 4, DeltaPackets: 3, KeyInterval: 61, SpreadUs: 2000,
			StartSeq: 1000, StartTS: 90000, StartPictureID: 100, SSRC: next()}))
		lost, reveal := seqOf(t.Records, 60, 1), atOf(t.Records, 60, 2)
		eof := endAt(t.Records) + 10000
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(lost), corpus.EOFAt(eof)))
		synth(t, fmt.Sprintf("corpus.GenerateVP8{FPS:30,Frames:66,KeyPackets:4,DeltaPackets:3,KeyInterval:61,SpreadUs:2000}+DropSeqs(%d)+EOFAt(%d)", lost, eof), nil,
			"c17-eof-after-gap/camera", fmt.Sprintf("Reveal at %.3f ms, eof at %.3f ms (%.0f ms later, before the 200 ms gap request).", ms(reveal), ms(eof), ms(eof-reveal)))
		out = append(out, seed{name: "c17-eof-after-gap", class: "17", tracks: []*corpus.Trace{t}})
	}

	// Sequence number, RTP timestamp and picture ID all wrap, with a repaired loss at seq 0.
	{
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 180, KeyInterval: 90, SpreadUs: 2000,
			StartSeq: 65400, StartTS: 1<<32 - 3*90000, StartPictureID: 0x7FFF - 60, SSRC: next()}))
		r := t.Records
		var at int64
		for _, x := range r {
			if x.Seq == 0 {
				at = x.AtUs
			}
		}
		t.Records = must(corpus.Apply(r, corpus.Retransmit(0, at+50000), corpus.DropSeqs(0), corpus.EOFAt(endAt(r)+50000)))
		synth(t, "corpus.GenerateVP8{FPS:30,Frames:180,KeyInterval:90,SpreadUs:2000,StartSeq:65400,StartTS:2^32-270000,StartPictureID:32707}+Retransmit(0,+50ms)+DropSeqs(0)+EOFAt",
			nil, "wrap-seq-ts-pic/camera", "Seq wraps at ~2.3 s, ts at 3 s, picture ID at frame 61.")
		out = append(out, seed{name: "wrap-seq-ts-pic", class: "wrap", tracks: []*corpus.Trace{t}})
	}

	// Clean long camera + microphone baseline, light seeded reordering on video.
	{
		const sd = 2843
		v := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Duration: 60e9, KeyPackets: 8, DeltaPackets: 3, KeyInterval: 90, SpreadUs: 3000,
			StartSeq: 31000, StartTS: 1234567, StartPictureID: 500, SSRC: next()}))
		v.Records = must(corpus.Apply(v.Records, corpus.Reorder(2, 0.01, rand.New(rand.NewPCG(sd, sd))), corpus.EOFAt(endAt(v.Records)+50000)))
		synth(v, "corpus.GenerateVP8{FPS:30,Duration:60s,KeyPackets:8,DeltaPackets:3,KeyInterval:90,SpreadUs:3000}+Reorder(2,0.01,PCG(2843,2843))+EOFAt",
			seedp(sd), "baseline-av/camera", "Clean baseline video; reordering moves 1% of packets 1-2 slots later, no loss.")
		a := must(corpus.GenerateOpus(corpus.OpusOptions{Duration: 60e9, StartSeq: 9000, StartTS: 7654321, SSRC: next()}))
		// The microphone starts 15 ms after the camera, so the replay does not depend on how two reads at one instant interleave.
		for i := range a.Records {
			a.Records[i].AtUs += 15000
		}
		a.Records = must(corpus.Apply(a.Records, corpus.EOFAt(endAt(a.Records)+50000)))
		synth(a, "corpus.GenerateOpus{Duration:60s}+15 ms offset+EOFAt", nil, "baseline-av/microphone", "Clean baseline audio, in order, no loss, read from 15 ms.")
		out = append(out, seed{name: "baseline-av", class: "baseline", tracks: []*corpus.Trace{v, a},
			suffixes: []string{".video", ".audio"}})
	}

	// Seeds appended after the baseline keep every earlier seed's SSRC.

	// An unrepaired mid-frame loss across the sequence wrap: the gap opens at
	// 65535 and seq 0 reveals it, so a gap tracker that takes 0 for a jump
	// back drops it and the request waits for the 2 s expiry.
	{
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 150, KeyPackets: 8, DeltaPackets: 8, KeyInterval: 48,
			SpreadUs: 7000, StartSeq: 65293, StartTS: 90000, StartPictureID: 100, SSRC: next()}))
		lost, reveal := seqOf(t.Records, 30, 2), atOf(t.Records, 30, 3)
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(lost), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, fmt.Sprintf("corpus.GenerateVP8{FPS:30,Frames:150,KeyPackets:8,DeltaPackets:8,KeyInterval:48,SpreadUs:7000,StartSeq:65293}+DropSeqs(%d)+EOFAt", lost),
			nil, "wrap-unrepaired-loss/camera", fmt.Sprintf("Packet 3 of frame 30 (seq %d) lost, never repaired; seq %d, past the wrap, reveals it at %.3f ms.",
				lost, lost+1, ms(reveal)))
		out = append(out, seed{name: "wrap-unrepaired-loss", class: "wrap", tracks: []*corpus.Trace{t}})
	}

	// LiveKit's RTX padding probe right after the wrap: each copy of a
	// pre-wrap packet is a duplicate only to a window that follows the wrap.
	{
		t := must(corpus.GenerateVP8(corpus.VP8Options{FPS: 30, Frames: 120, KeyPackets: 15, DeltaPackets: 4, KeyInterval: 60, SpreadUs: 2000,
			StartSeq: 65403, StartTS: 90000, StartPictureID: 100, SSRC: next()}))
		r := t.Records
		fr := frames(r)
		// Frame 30 is seqs 65534-1: one probe after its marker, one after
		// frame 31's second packet.
		marker, mid := fr[30][len(fr[30])-1], fr[31][1]
		var ops []corpus.Op
		for _, last := range []int{marker, mid} {
			at := r[last].AtUs + 200
			for k := range 6 {
				ops = append(ops, corpus.Retransmit(r[last].Seq-5+uint16(k), at+int64(k)*50))
			}
		}
		ops = append(ops, corpus.EOFAt(endAt(r)+50000))
		t.Records = must(corpus.Apply(r, ops...))
		synth(t, "corpus.GenerateVP8{FPS:30,Frames:120,KeyPackets:15,DeltaPackets:4,KeyInterval:60,SpreadUs:2000,StartSeq:65403}+Retransmit x6 after frame 30 and mid frame 31+EOFAt",
			nil, "wrap-dup-probe/camera", fmt.Sprintf("Frame 30 is seqs %d-%d, across the wrap. A probe after its marker resends %d-%d and one after frame 31's second packet "+
				"resends %d-%d, copies 0.2 ms after, 50 µs apart, as LiveKit's RTX padding probe does.",
				r[fr[30][0]].Seq, r[marker].Seq, r[marker].Seq-5, r[marker].Seq, r[mid].Seq-5, r[mid].Seq))
		out = append(out, seed{name: "wrap-dup-probe", class: "wrap", tracks: []*corpus.Trace{t}})
	}

	return out
}

func main() {
	dir := flag.String("out", "internal/webrtc/livekit/testdata/corpus", "output directory")
	flag.Parse()
	writeSeeds(*dir, os.Stdout)
}

// writeSeeds writes every seed's traces into dir, and a summary table to w.
func writeSeeds(dir string, w io.Writer) {
	must(0, os.MkdirAll(dir, 0o755))

	must(fmt.Fprintln(w, "| Name | Class | Provenance | Packets | Duration (s) | Size (B) |"))
	must(fmt.Fprintln(w, "|---|---|---|---|---|---|"))
	total := int64(0)
	for _, s := range seeds() {
		var prov []string
		pkts, dur, size := 0, int64(0), int64(0)
		for i, t := range s.tracks {
			suffix := ""
			if s.suffixes != nil {
				suffix = s.suffixes[i]
			}
			path := filepath.Join(dir, s.name+suffix+".ndjson.gz")
			must(0, t.WriteFile(path))
			back := must(corpus.ReadFile(path)) // Read validates
			for _, r := range back.Records {
				if r.T == corpus.RecordRTP {
					pkts++
				}
			}
			dur = max(dur, endAt(back.Records))
			size += must(os.Stat(path)).Size()
			prov = append(prov, string(back.Header.Provenance))
		}
		total += size
		must(fmt.Fprintf(w, "| %s | %s | %s | %d | %.2f | %d |\n", s.name, s.class, strings.Join(slices.Compact(prov), "+"), pkts, float64(dur)/1e6, size))
	}
	must(fmt.Fprintln(w, "total trace bytes:", total))
}
