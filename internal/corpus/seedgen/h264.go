package main

import (
	"fmt"
	"math/rand/v2"
	"slices"
	"sort"

	"github.com/bigbluebutton/bbb-webrtc-recorder/internal/corpus"
)

// h264Camera is the H.264 camera most seeds start from, 640x360 at 30 fps.
// A key frame is a STAP-A of SPS and PPS, then an IDR in 6 FU-A fragments.
// A delta frame is a P slice in 2 FU-A fragments.
func h264Camera(frames, keyInterval int, ssrc uint32) corpus.H264Options {
	return corpus.H264Options{FPS: 30, Frames: frames, KeyInterval: keyInterval, KeySlicePackets: 6, DeltaSlicePackets: 2,
		KeyPLen: 1100, DeltaPLen: 900, SpreadUs: 3000, StartSeq: 1000, StartTS: 90000, SSRC: ssrc}
}

// h264Gen is the generator note of an h264Camera trace.
func h264Gen(frames, keyInterval int, ops string) string {
	return fmt.Sprintf("corpus.GenerateH264{FPS:30,Frames:%d,KeyInterval:%d,KeySlicePackets:6,DeltaSlicePackets:2,"+
		"KeyPLen:1100,DeltaPLen:900,SpreadUs:3000}%s", frames, keyInterval, ops)
}

// h264Seeds come after every VP8 and Opus seed, so that those seeds keep
// their SSRCs.
func h264Seeds(next func() uint32) []seed {
	var out []seed

	// A clean camera, with light seeded reordering.
	{
		const sd = 7349
		t := must(corpus.GenerateH264(h264Camera(600, 60, next())))
		t.Records = must(corpus.Apply(t.Records, corpus.Reorder(2, 0.01, rand.New(rand.NewPCG(sd, sd))), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, h264Gen(600, 60, "+Reorder(2,0.01,PCG(7349,7349))+EOFAt"), seedp(sd), "h264-baseline/camera",
			"Clean 640x360 H.264 camera, 20 s at 30 fps with a key frame every 2 s. Reordering moves 1% of packets 1-2 slots later, no loss.")
		out = append(out, seed{name: "h264-baseline", class: "h264", tracks: []*corpus.Trace{t}})
	}

	// A simulcast layer switch. LiveKit cuts the low layer's frame 44 after
	// its first FU-A fragment. The high layer's key frame follows with the
	// next sequence number and a timestamp one tick later.
	{
		low := must(corpus.GenerateH264(corpus.H264Options{FPS: 30, Frames: 45, KeyInterval: 60, KeySlicePackets: 4, DeltaSlicePackets: 3,
			Width: 320, Height: 180, KeyPLen: 900, DeltaPLen: 300, SpreadUs: 2000, StartSeq: 5000, StartTS: 90000, SSRC: next()}))
		high := must(corpus.GenerateH264(h264Camera(90, 60, 0)))
		cut := seqOf(low.Records, 44, 0)
		r := must(corpus.Apply(low.Records, corpus.Switch(cut, high.Records)))
		t := &corpus.Trace{Header: low.Header, Records: must(corpus.Apply(r, corpus.EOFAt(endAt(r)+50000)))}
		synth(t, "corpus.GenerateH264{FPS:30,Frames:45,KeyInterval:60,KeySlicePackets:4,DeltaSlicePackets:3,Width:320,Height:180,KeyPLen:900,DeltaPLen:300,SpreadUs:2000}"+
			fmt.Sprintf("+Switch(%d,", cut)+h264Gen(90, 60, ")+EOFAt"), nil, "h264-layer-switch/camera",
			fmt.Sprintf("Frames 0-43 of a 320x180 layer, then frame 44's first FU-A fragment (seq %d) only. "+
				"The 640x360 layer's key frame follows at the next seq and one tick later, then its 89 frames, with a key frame at 60.", cut))
		out = append(out, seed{name: "h264-layer-switch", class: "h264", tracks: []*corpus.Trace{t}})
	}

	// A simulcast start, as LiveKit switches to the high layer right after
	// the low layer's parameter sets, before the low IDR.
	{
		low := must(corpus.GenerateH264(corpus.H264Options{FPS: 30, Frames: 1, KeySlicePackets: 4, Width: 320, Height: 180,
			StartSeq: 7000, StartTS: 450000, SSRC: next()}))
		high := must(corpus.GenerateH264(h264Camera(90, 60, 0)))
		stap := low.Records[0].Seq
		r := must(corpus.Apply(low.Records, corpus.Switch(stap, high.Records)))
		t := &corpus.Trace{Header: low.Header, Records: must(corpus.Apply(r, corpus.EOFAt(endAt(r)+50000)))}
		synth(t, fmt.Sprintf("corpus.GenerateH264{FPS:30,Frames:1,KeySlicePackets:4,Width:320,Height:180}+Switch(%d,", stap)+h264Gen(90, 60, ")+EOFAt"),
			nil, "h264-simulcast-orphan-sps/camera",
			"The 320x180 layer's STAP-A of SPS and PPS, with no marker, then the 640x360 layer's key frame one tick later and its 89 frames. "+
				"The jitter buffer releases both timestamps in one sample.")
		out = append(out, seed{name: "h264-simulcast-orphan-sps", class: "h264", tracks: []*corpus.Trace{t}})
	}

	// LiveKit's blank frames as a downtrack closes, then the end.
	{
		t := must(corpus.GenerateH264(h264Camera(60, 30, next())))
		r := must(corpus.Apply(t.Records, corpus.H264BlankClose(6)))
		t.Records = must(corpus.Apply(r, corpus.EOFAt(endAt(r)+20000)))
		synth(t, h264Gen(60, 30, "+H264BlankClose(6)+EOFAt"), nil, "h264-blank-close/camera",
			"2 s of camera, then 6 blank frames at 30 fps as LiveKit sends at a downtrack's close: each one STAP-A of a 2x2 SPS, PPS and IDR.")
		out = append(out, seed{name: "h264-blank-close", class: "h264", tracks: []*corpus.Trace{t}})
	}

	// A key frame whose parameter sets are lost.
	{
		t := must(corpus.GenerateH264(h264Camera(90, 30, next())))
		lost := seqOf(t.Records, 30, 0)
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(lost), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, h264Gen(90, 30, fmt.Sprintf("+DropSeqs(%d)+EOFAt", lost)), nil, "h264-lost-stapa/camera",
			fmt.Sprintf("Key frame 30's STAP-A of SPS and PPS (seq %d) is lost and never repaired; its IDR arrives whole.", lost))
		out = append(out, seed{name: "h264-lost-stapa", class: "h264", tracks: []*corpus.Trace{t}})
	}

	// A P frame of two slices that loses its first.
	{
		o := h264Camera(90, 60, next())
		o.Slices, o.KeySlicePackets, o.DeltaSlicePackets, o.DeltaPLen = 2, 3, 1, 450
		t := must(corpus.GenerateH264(o))
		lost := seqOf(t.Records, 20, 0)
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(lost), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, fmt.Sprintf("corpus.GenerateH264{FPS:30,Frames:90,KeyInterval:60,Slices:2,KeySlicePackets:3,DeltaSlicePackets:1,"+
			"KeyPLen:1100,DeltaPLen:450,SpreadUs:3000}+DropSeqs(%d)+EOFAt", lost), nil, "h264-lost-first-slice/camera",
			fmt.Sprintf("P frame 20's first slice (seq %d) is lost and never repaired; its second slice, with the marker, arrives.", lost))
		out = append(out, seed{name: "h264-lost-first-slice", class: "h264", tracks: []*corpus.Trace{t}})
	}

	// A key frame that loses the FU-A fragment that ends its IDR. The next
	// key frame comes 3 s later, and the track goes on 1 s past it.
	{
		t := must(corpus.GenerateH264(h264Camera(210, 90, next())))
		lost := seqOf(t.Records, 90, 6)
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(lost), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, h264Gen(210, 90, fmt.Sprintf("+DropSeqs(%d)+EOFAt", lost)), nil, "h264-lost-fua-end/camera",
			fmt.Sprintf("Key frame 90's last IDR fragment (seq %d), the one with the end bit and the marker, is lost and never repaired. "+
				"Key frame 180 is the next.", lost))
		out = append(out, seed{name: "h264-lost-fua-end", class: "h264", tracks: []*corpus.Trace{t}})
	}

	// Packetization mode 0: every NAL unit in a packet of its own, 4 slices a
	// frame. A slice of a P frame is lost, and the next key frame comes 3 s
	// later.
	{
		t := must(corpus.GenerateH264(corpus.H264Options{FPS: 30, Frames: 210, KeyInterval: 90, Slices: 4, Mode0: true,
			KeyPLen: 1100, DeltaPLen: 300, SpreadUs: 3000, StartSeq: 1000, StartTS: 90000, SSRC: next()}))
		lost := seqOf(t.Records, 100, 2)
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(lost), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, fmt.Sprintf("corpus.GenerateH264{FPS:30,Frames:210,KeyInterval:90,Slices:4,Mode0:true,KeyPLen:1100,DeltaPLen:300,SpreadUs:3000}"+
			"+DropSeqs(%d)+EOFAt", lost), nil, "h264-mode0-slices/camera",
			fmt.Sprintf("Mode 0: a key frame is an SPS, a PPS and 4 IDR slices, each in a packet of its own, and a delta 4 P slices. "+
				"Slice 2 of P frame 100 (seq %d) is lost and never repaired. Key frame 180 is the next.", lost))
		out = append(out, seed{name: "h264-mode0-slices", class: "h264", tracks: []*corpus.Trace{t}})
	}

	// LiveKit 1.13.5-1.13.9 padding: the padding bit and a 255-byte payload,
	// at frame boundaries.
	{
		t := must(corpus.GenerateH264(h264Camera(90, 60, next())))
		var ops []corpus.Op
		var after []uint16
		for _, f := range []int{10, 25, 40} {
			fr := frames(t.Records)[f]
			last := t.Records[fr[len(fr)-1]].Seq
			// Each insert moves the later frames up by 3.
			last += uint16(3 * len(after))
			after = append(after, last)
			ops = append(ops, corpus.Padding(last, 3, 255))
		}
		ops = append(ops, corpus.EOFAt(endAt(t.Records)+50000))
		t.Records = must(corpus.Apply(t.Records, ops...))
		synth(t, h264Gen(90, 60, fmt.Sprintf("+Padding(%d,3,255)+Padding(%d,3,255)+Padding(%d,3,255)+EOFAt", after[0], after[1], after[2])),
			nil, "h264-padding-payload/camera",
			"3 padding packets after the markers of frames 10, 25 and 40, each with the padding bit and 255 zero bytes, as LiveKit 1.13.5-1.13.9 sends them.")
		out = append(out, seed{name: "h264-padding-payload", class: "h264", tracks: []*corpus.Trace{t}})
	}

	return append(out, h264Twins(next)...)
}

// h264Twins are the H.264 twins of VP8 seeds: the same schedule shape and
// mutation, for the defect class the VP8 seed guards.
func h264Twins(next func() uint32) []seed {
	var out []seed
	gen := func(o corpus.H264Options) string {
		return fmt.Sprintf("corpus.GenerateH264{FPS:%.4g,Frames:%d,KeyInterval:%d,KeySlicePackets:%d,DeltaSlicePackets:%d,SpreadUs:%d}",
			o.FPS, o.Frames, o.KeyInterval, o.KeySlicePackets, o.DeltaSlicePackets, o.SpreadUs)
	}
	// Frames have as many packets as in the VP8 seed. A key frame's STAP-A
	// counts as one of them.
	camera := func(frames, keyInterval, keyPackets, deltaPackets int, spreadUs int64) corpus.H264Options {
		return corpus.H264Options{FPS: 30, Frames: frames, KeyInterval: keyInterval, KeySlicePackets: keyPackets - 1,
			DeltaSlicePackets: deltaPackets, SpreadUs: spreadUs, StartSeq: 1000, StartTS: 90000, SSRC: next()}
	}
	// Frames read in the 700 ms after a gap come 300 ms late, as in the VP8
	// c24 seeds.
	const lateUs, lateForUs = 300000, 700000

	// c03-d11-30fps: a packet in the middle of P frame 30 is lost and never
	// repaired.
	{
		o := camera(150, 48, 8, 8, 7000)
		t := must(corpus.GenerateH264(o))
		lost, reveal := seqOf(t.Records, 30, 2), atOf(t.Records, 30, 3)
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(lost), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, gen(o)+fmt.Sprintf("+DropSeqs(%d)+EOFAt", lost), nil, "h264-c03-d11-30fps/camera",
			fmt.Sprintf("FU-A fragment 3 of P frame 30 (seq %d) lost, never repaired; seq %d reveals it at %.3f ms.", lost, lost+1, ms(reveal)))
		out = append(out, seed{name: "h264-c03-d11-30fps", class: "3", tracks: []*corpus.Trace{t}})
	}

	// c11-dups-30fps: a duplicate inside a frame still assembling, and one
	// after its frame left the buffer.
	{
		o := camera(120, 60, 15, 4, 2000)
		t := must(corpus.GenerateH264(o))
		r := slices.Clone(t.Records)
		f50 := frames(r)[50]
		late := f50[2]
		lateSeq := r[late].Seq
		r[late].AtUs += 10000
		sort.SliceStable(r, func(i, j int) bool { return r[i].AtUs < r[j].AtUs })
		p1, dupIn := seqOf(r, 50, 1), atOf(r, 50, 1)+5000
		p80, dupAfter := seqOf(r, 80, 1), atOf(r, 80, 1)+40000
		t.Records = must(corpus.Apply(r, corpus.Duplicate(p1, dupIn), corpus.Duplicate(p80, dupAfter), corpus.EOFAt(endAt(r)+50000)))
		synth(t, gen(o)+fmt.Sprintf("+late(%d,+10ms)+Duplicate(%d,%d)+Duplicate(%d,%d)+EOFAt", lateSeq, p1, dupIn, p80, dupAfter), nil,
			"h264-c11-dups-30fps/camera", "As c11-dups-30fps: frame 50's packet 3 read 10 ms late with a copy of packet 2 in between, and a copy of frame 80's packet 2 40 ms later.")
		out = append(out, seed{name: "h264-c11-dups-30fps", class: "11", tracks: []*corpus.Trace{t}})
	}

	// c12-lkprobe-rtxdup: LiveKit's RTX probe resends the last 6 packets,
	// at a frame boundary and mid-frame in turn.
	{
		o := camera(120, 60, 15, 4, 2000)
		t := must(corpus.GenerateH264(o))
		r := t.Records
		fr := frames(r)
		var ops []corpus.Op
		for i, f := 0, 15; f < 115; i, f = i+1, f+15 {
			last := fr[f][len(fr[f])-1]
			if i%2 == 1 {
				last = fr[f][1]
			}
			at := r[last].AtUs + 200
			for k := range 6 {
				ops = append(ops, corpus.Retransmit(r[last].Seq-5+uint16(k), at+int64(k)*50))
			}
		}
		ops = append(ops, corpus.EOFAt(endAt(r)+50000))
		t.Records = must(corpus.Apply(r, ops...))
		synth(t, gen(o)+"+Retransmit x6 every 15 frames+EOFAt", nil, "h264-c12-lkprobe-rtxdup/camera",
			"As c12-lkprobe-rtxdup: probes at frames 15, 45, 75, 105 after the marker, and at 30, 60, 90 after the frame's second packet.")
		out = append(out, seed{name: "h264-c12-lkprobe-rtxdup", class: "12", tracks: []*corpus.Trace{t}})
	}

	// c15-start-midframe: the subscription starts inside the first key frame.
	{
		o := camera(120, 60, 15, 4, 7000)
		t := must(corpus.GenerateH264(o))
		t.Records = must(corpus.Apply(t.Records, corpus.StartAt(3), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, gen(o)+"+StartAt(3)+EOFAt", nil, "h264-c15-start-midframe/camera",
			"The subscription starts at packet 4 of the 15-packet first key frame, past its STAP-A and first FU-A fragments; next key frame at frame 60 (2 s).")
		out = append(out, seed{name: "h264-c15-start-midframe", class: "15", tracks: []*corpus.Trace{t}})
	}

	// c15-start-kfloss: the first key frame loses a packet.
	{
		o := camera(120, 60, 15, 4, 7000)
		t := must(corpus.GenerateH264(o))
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(1005), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, gen(o)+"+DropSeqs(1005)+EOFAt", nil, "h264-c15-start-kfloss/camera",
			"FU-A fragment 5 of the first key frame's IDR (seq 1005) lost, never repaired.")
		out = append(out, seed{name: "h264-c15-start-kfloss", class: "15", tracks: []*corpus.Trace{t}})
	}

	// c17-eof-after-gap: eof right after a gap inside a P frame. The key frame
	// and deltas behind it are still held, and no gap request is due yet.
	{
		o := camera(66, 61, 4, 3, 2000)
		t := must(corpus.GenerateH264(o))
		lost, reveal := seqOf(t.Records, 60, 1), atOf(t.Records, 60, 2)
		eof := endAt(t.Records) + 10000
		t.Records = must(corpus.Apply(t.Records, corpus.DropSeqs(lost), corpus.EOFAt(eof)))
		synth(t, gen(o)+fmt.Sprintf("+DropSeqs(%d)+EOFAt(%d)", lost, eof), nil, "h264-c17-eof-after-gap/camera",
			fmt.Sprintf("Reveal at %.3f ms, eof at %.3f ms (%.0f ms later, before the 200 ms gap request).", ms(reveal), ms(eof), ms(eof-reveal)))
		out = append(out, seed{name: "h264-c17-eof-after-gap", class: "17", tracks: []*corpus.Trace{t}})
	}

	// c24-resume-4s: a camera paused for 4 s and resumed, as LiveKit sends
	// it.
	{
		o := camera(360, 90, 8, 3, 3000)
		t := must(corpus.GenerateH264(o))
		n, end := len(t.Records), endAt(t.Records)
		r := must(corpus.Apply(t.Records, corpus.PauseH264(5e6, 9e6)))
		r = must(corpus.Apply(r, corpus.DelayFrames(lateFrames(r, 9e6, 9e6+lateForUs, lateUs))))
		t.Records = must(corpus.Apply(r, corpus.EOFAt(end+50000)))
		synth(t, gen(o)+"+PauseH264(5s,9s)+DelayFrames{read in [9s,9.7s):+300ms}+EOFAt", nil, "h264-c24-resume-4s/camera",
			fmt.Sprintf("Frames 150-269 (%d packets) are not sent; the seqs after them follow on. Every frame read in the 700 ms after the resume is read 300 ms later. "+
				"Key frame 270 is read at %.3f ms, its timestamp 4.033 s after frame 149's.", n-len(t.Records)+1, ms(atOf(t.Records, 150, 0))))
		out = append(out, seed{name: "h264-c24-resume-4s", class: "24", tracks: []*corpus.Trace{t}})
	}

	// c01-paced-6x45: each frame's packets read over 225 ms.
	{
		o := camera(12, 4, 6, 6, 225000)
		o.FPS, o.StartSeq, o.StartTS = 3.4, 45308, 3164626761
		t := must(corpus.GenerateH264(o))
		t.Records = must(corpus.Apply(t.Records, corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, gen(o)+"+EOFAt", nil, "h264-c01-paced-6x45/camera",
			"As c01-paced-6x45: each frame's 6 packets are read 45 ms apart (225 ms). A key frame is a STAP-A and 5 FU-A fragments.")
		out = append(out, seed{name: "h264-c01-paced-6x45", class: "1", tracks: []*corpus.Trace{t}})
	}

	// c01-trickle-11x50: each frame's packets read over 500 ms, the next
	// frame right behind.
	{
		o := camera(8, 4, 11, 11, 500000)
		o.FPS, o.StartSeq, o.StartTS = 1e6/550000.0, 45308, 3164626761
		t := must(corpus.GenerateH264(o))
		t.Records = must(corpus.Apply(t.Records, corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, gen(o)+"+EOFAt", nil, "h264-c01-trickle-11x50/camera",
			"As c01-trickle-11x50: each frame's 11 packets are read 50 ms apart (500 ms), the next frame right behind. "+
				"A key frame is a STAP-A and 10 FU-A fragments.")
		out = append(out, seed{name: "h264-c01-trickle-11x50", class: "1", tracks: []*corpus.Trace{t}})
	}

	// c24-blackout-9s: a camera whose path carries nothing for 9 s mid-file,
	// at 240 and 360 packets/s.
	for _, c := range []struct {
		tag     string
		packets int
	}{{"seq2160", 8}, {"seq3240", 12}} {
		name := "h264-c24-blackout-9s-" + c.tag
		o := camera(480, 63, c.packets, c.packets, 3000)
		t := must(corpus.GenerateH264(o))
		end := endAt(t.Records)
		r := must(corpus.Apply(t.Records, corpus.Blackout(3e6, 12e6)))
		r = must(corpus.Apply(r, corpus.DelayFrames(lateFrames(r, 12e6, 12e6+lateForUs, lateUs))))
		t.Records = must(corpus.Apply(r, corpus.EOFAt(end+50000)))
		last, resume := seqOf(t.Records, 89, c.packets-1), seqOf(t.Records, 90, 0)
		synth(t, gen(o)+"+Blackout(3s,12s)+DelayFrames{read in [12s,12.7s):+300ms}+EOFAt", nil, name+"/camera",
			fmt.Sprintf("Frames 90-359 are never read: seqs %d-%d (%d) and 9 s of RTP time. "+
				"Every frame read in the 700 ms after the blackout is read 300 ms later. "+
				"Delta 360 is read at %.3f ms. Key frames 126-315 fall in the blackout, so the next one is frame 378.",
				last+1, resume-1, resume-last-1, ms(atOf(t.Records, 90, 0))))
		out = append(out, seed{name: name, class: "24", tracks: []*corpus.Trace{t}})
	}

	// c24-start-stall-2500ms: nothing read for the first 2.5 s, then the
	// backlog at once.
	{
		o := camera(240, 60, 8, 3, 3000)
		t := must(corpus.GenerateH264(o))
		t.Records = must(corpus.Apply(t.Records, corpus.Stall(0, 2.5e6), corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, gen(o)+"+Stall(0,2.5s)+EOFAt", nil, "h264-c24-start-stall-2500ms/camera",
			"As c24-start-stall-2500ms: frames 0-74 are all read at 2500 ms; frame 75, sent then, is on time.")
		out = append(out, seed{name: "h264-c24-start-stall-2500ms", class: "24", tracks: []*corpus.Trace{t}})
	}

	// c15-start-blank-key: a first key frame of 8x8, which the writer
	// rejects.
	{
		o := camera(120, 20, 8, 3, 3000)
		t := must(corpus.GenerateH264(o))
		// A key frame's first packet is its STAP-A of SPS and PPS.
		small := must(corpus.GenerateH264(corpus.H264Options{FPS: 30, Frames: 1, Width: 8, Height: 8}))
		t.Records[0].Pay, t.Records[0].PLen = small.Records[0].Pay, small.Records[0].PLen
		t.Records = must(corpus.Apply(t.Records, corpus.EOFAt(endAt(t.Records)+50000)))
		synth(t, gen(o)+"+frame 0's SPS at 8x8+EOFAt", nil, "h264-c15-start-blank-key/camera",
			"Key frame 0's STAP-A holds an 8x8 SPS. Key frame 20, the next, is 666.667 ms of RTP time after it.")
		out = append(out, seed{name: "h264-c15-start-blank-key", class: "15", tracks: []*corpus.Trace{t}})
	}

	return out
}
