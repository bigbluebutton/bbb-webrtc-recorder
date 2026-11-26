package main

import (
	"fmt"
	"math/rand/v2"

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

	// A simulcast start: LiveKit forwards the low layer's SPS and PPS, then
	// switches to the high layer's key frame before the low one's IDR.
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

	return out
}
