package recorder

import (
	"encoding/binary"
	"testing"

	"github.com/pion/rtp"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// The NAL units of three access units from libx264 (ffmpeg 4.2.7):
//
//	ffmpeg -f lavfi -i testsrc=size=320x180:rate=15 -frames:v 3 -c:v libx264 \
//	  -profile:v baseline -pix_fmt yuv420p -x264-params keyint=30:slices=2:aud=1 -crf 40 \
//	  -bsf:v filter_units=remove_types=6 -f h264 out.h264
//
// Each frame starts with an AUD and has two slices. The filter removes x264's SEI.
var h264TestAUs = [][]string{
	{
		"0910",
		"6742c00cd901419f9f0110000003001000000301e0f142a480",
		"68cb80e4b2",
		"6588840bf2628000853c9c44120382dc4271000109ffffc171f86ce140478df70082cf2267fcbfc6938b8bf5f09cc020800d181a37df" +
			"006fc759960463557b463557b062208c500010378c000100443400043111388824070bf104a200042c44148a001f1d93889050001" +
			"10a8e00021bf1c00040224e220901c2fc412880010b11042281fe4fc3a9fcf8683c2be2eb84a601040045600a829665e00aad4619" +
			"0c75fffa4c3e0a8a060069824000401b005f851ec381291b917dbc44c0e0a7109c40004688825141c47326ebae66092efda5937c8" +
			"0aa075a0a08debf82aa0dccec8893e1301512089e3b341481c41e40290210390cc97ae66092ca60a6b9992f5ccc1259330537a59" +
			"9fee7e9f3ce0a81370f125bde1650186cb65865ced679852e30246ac67ba982375d75c2d044023df47bfbfdf340a27c3a0b62012" +
			"77cb252de804b4d6ffff0d09aad78548820139dff180d2a46787330fe53325aba7a65ae664b574f4cb5ccfeb5c27fe9ec1502602" +
			"1308171181c7d930a0c0989b2cf79af6025dc69823aebaff686d0f82f04955f2b204a825023df0e724a107b99829052931103ef9" +
			"992f5d3d32d73325eba7a65ae9ebe9ffa5305c0a07e9d71740e4bf8895acf7fd304b5d75fd86184bd848124374cacbd7e9829ae6" +
			"64b574f4cb5ccc96ae9e996ba7fc787f822054e286286280c700c70001001fc421fe83f147140e9827aebae9e996b9992f5d3d32" +
			"d73325eba7a65ae9e98280008dbebeadac00227bfeea69825af",
		"6503c88840bf2628000853c9fc3e90f94124500f15f8c18d108251000216220a4500029fc4778890500010518e00020111c000432a" +
			"23fe220901c2fc412880011e220a45000d1c064e22414000442a3800086fc7000100888ef110480e17e209440008f1105228007c7" +
			"0188fd118a00020e31c0004052380008854441600e0484a98380638000800c980e04a52260882914000459bf88deabafa43e9fb0d" +
			"8249bc684d5df6ff0958d8cc14deb9993f9bda66092ca60a6b9992f37699824b2660a7d5d3e3fffe0ac130ac5003c4b88024a69e" +
			"2004c880133fffdad8722800e12264e7a1498e676070b54f880133ccc11bd4dfbe3fff1e08bb42fd6b1025418003037c765ad97f" +
			"ffff43bd7475ac6283dcda6dfff4b7fd030f3915ac1ce81a321a1a8f08f1305ef3e2942f52a043a9afd3ceafc0f98671237975bf" +
			"ff08482703000449062a59454bf1a964b97e2d6640203fa9ac2ea0c9e3f2e977fc0a5041521fe5dce88d02fe7586ea8a941c9e04" +
			"80c78092138ecb2b72fc2b964b97e13476bfa7fe06748a46032df01490404edfe1665f3a8515320dd7f8ffd043a577ef7114837b" +
			"f2ee2b07bc08b3041421281b72cadcbf062a592e5f8b56b819d7f6ff4297fe3b5ae34d79ac3b201c156120f896cbf012cf42a5df" +
			"f1688a82ff974bb9d1607cc452fc3fe10856bcb95e2c896b2e97706252ca252c2ea27ffff4d318c60f9ccfb7fffc423f5ad7d2a7" +
			"7836e5b77368599616c10fe8ffd316fc56c2523c43a0d23979332ca08a58a5a307ff2e859434cb7fe9a7a698d284018f399399c0" +
			"e88f2c3a10b2ce8b22050c2c8c34d7fff4d31b8151fe73227a758d3e2d63774d3f0b4a1dcb7fedb7b6d830550bb8312965129783" +
			"672cace58b458d37768d4bf8591bffb6df8d30557f83129616451d6bfffb6d8dc127fe6dffe3f41c23defc13827e9f8a58da0c1a" +
			"376dbc1a12cb2a472c2ca12b51fffb6d9484aae1665f807f8cda7ffffa0ef5e34cb14c0845c5dda21d16a1a0cb71a1ff4c5aa828" +
			"630ae96d34687cb00c441444ed6682027310a66be7a7cf40aa129a080b90b26507c0952b19913afffd267ec347bf7c01212c5b8c" +
			"f7f006ac38ecb03ccc2e24548e6210ad54cc382b115ad07c51b599c500027108071a0005275d4d0f0b9d84a231083f32cb5f0fff" +
			"061c0c2fe17af07cb8df7c018f67913e6d8be51f90344eabc27b083a61be9d40247cd1276d9f9a357abbcce6e5eb41f76ae6899f" +
			"aba79dedcf387fe0a814032d848227fb6b7cdb487920660a58ba65b53cdfa96679aa7a799f55fe43ffb0540a0405a0c000482f35" +
			"8c144a8619120ee1e0a54600a253cdbfe970",
	},
	{
		"0930",
		"419a3817e688fea12cb09713c90101fcdececb2c2597cb92bc6f960ab5e332cc77af1be53bd78df5",
		"4103c9a3817e485b2c2d9616cb0101fc82d96f9fa3f477a16c97b16cbe945b27b3fc112e0c3f409ba6ea64e5ae5c95cae0fbf56bea" +
			"df54c97a87a7b0fc23d87ebffa2debadf40d3ae55a522feb5f5bb82086fa9b19fc247e6836a3ad7ff6dbdb6c6e37d336f4fd5bea" +
			"dbc13511bebefa2f7f833c91d8989c746663c13cd0",
	},
	{
		"0930",
		"419a5405f81a315846",
		"4103c9a5405f8605bdf3bc49de7c106fd028e55f5a55f5a55f54abea65d7ad7eb5fa995fae4c9faa65f54cbeae57af6bff045cbffe" +
			"bffd77fd15fc1177e0fb56b5fad7eb5e0dabde1e9edfc137f677e046e31443e9bf56fd7dbf0b7808ded10b7f2de21076510bb80d" +
			"fea139bf111b09c0",
	},
}

// A minimal user_data_unregistered SEI and a filler NAL unit.
var (
	h264TestSEI    = []byte{0x06, 0x05, 0x11, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16, 0x42, 0x80}
	h264TestFiller = append(append([]byte{0x0c}, bytesOf(0xff, 300)...), 0x80)
)

func bytesOf(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

func testAU(t testing.TB, i int) [][]byte {
	t.Helper()
	nals := make([][]byte, len(h264TestAUs[i]))
	for j, s := range h264TestAUs[i] {
		nals[j] = mustHex(t, s)
	}
	return nals
}

func stapA(nals ...[]byte) []byte {
	var nri byte
	for _, n := range nals {
		nri = max(nri, n[0]&0x60)
	}
	out := []byte{nri | 24}
	for _, n := range nals {
		out = binary.BigEndian.AppendUint16(out, uint16(len(n)))
		out = append(out, n...)
	}
	return out
}

// fuA splits nal into FU-A fragments of at most size payload bytes. nal must
// need two fragments or more.
func fuA(nal []byte, size int) [][]byte {
	var out [][]byte
	rest := nal[1:]
	for first := true; len(rest) > 0; first = false {
		n := min(size, len(rest))
		header := nal[0] & 0x1f
		if first {
			header |= 0x80
		}
		if n == len(rest) {
			header |= 0x40
		}
		out = append(out, append([]byte{nal[0]&0xe0 | 28, header}, rest[:n]...))
		rest = rest[n:]
	}
	return out
}

// h264Packetize packs nals in packetization mode 1, as libwebrtc does. Runs of
// NAL units that fit go into one STAP-A, a NAL unit over mtu becomes FU-A
// fragments, and the others go alone.
func h264Packetize(nals [][]byte, mtu int) [][]byte {
	var out [][]byte
	for i := 0; i < len(nals); {
		if len(nals[i]) > mtu {
			out = append(out, fuA(nals[i], mtu-2)...)
			i++
			continue
		}
		size, j := 1, i
		for j < len(nals) && size+2+len(nals[j]) <= mtu {
			size += 2 + len(nals[j])
			j++
		}
		if j-i >= 2 {
			out = append(out, stapA(nals[i:j]...))
			i = j
			continue
		}
		out = append(out, nals[i])
		i++
	}
	return out
}

// h264TestSample builds the packets of one jitter-buffer sample.
type h264TestSample struct {
	seq     uint16
	packets []*rtp.Packet
}

// add appends one packet per payload, with the marker on the last one.
func (s *h264TestSample) add(ts uint32, payloads ...[]byte) *h264TestSample {
	for i, p := range payloads {
		s.packets = append(s.packets, &rtp.Packet{
			Header:  rtp.Header{SequenceNumber: s.seq, Timestamp: ts, Marker: i == len(payloads)-1},
			Payload: append([]byte{}, p...),
		})
		s.seq++
	}
	return s
}

// padding appends a padding packet as LiveKit 1.13.5-1.13.9 send it.
func (s *h264TestSample) padding(ts uint32) *h264TestSample {
	s.packets = append(s.packets, &rtp.Packet{
		Header:      rtp.Header{SequenceNumber: s.seq, Timestamp: ts, Padding: true},
		Payload:     make([]byte, 255),
		PaddingSize: 255,
	})
	s.seq++
	return s
}

func (s *h264TestSample) unmark() *h264TestSample {
	s.packets[len(s.packets)-1].Marker = false
	return s
}

func (s *h264TestSample) skip(n int) *h264TestSample {
	s.seq += uint16(n)
	return s
}

// wantUnit builds the expected unit for nals, independently of the code under test.
func wantUnit(ts uint32, firstSeq, lastSeq uint16, nals [][]byte) h264AccessUnit {
	u := h264AccessUnit{timestamp: ts, firstSeq: firstSeq, lastSeq: lastSeq}
	for _, n := range nals {
		switch n[0] & 0x1f {
		case 9, 12:
			continue
		case 1:
			u.slice = true
		case 5:
			u.slice, u.idr = true, true
		case 7:
			u.sps = append(u.sps, n)
		case 8:
			u.pps = append(u.pps, n)
		}
		u.data = binary.BigEndian.AppendUint32(u.data, uint32(len(n)))
		u.data = append(u.data, n...)
	}
	return u
}

func TestH264AccessUnitsSingleNALPackets(t *testing.T) {
	nals := testAU(t, 0)
	s := (&h264TestSample{seq: 100}).add(9000, nals...)

	units, err := h264AccessUnits(s.packets)
	require.NoError(t, err)
	require.Len(t, units, 1)
	assert.Equal(t, wantUnit(9000, 100, 104, nals), units[0])
	assert.True(t, units[0].idr)
	assert.True(t, units[0].slice)
	assert.Equal(t, [][]byte{nals[1]}, units[0].sps)
	assert.Equal(t, [][]byte{nals[2]}, units[0].pps)
}

func TestH264AccessUnitsPacketizations(t *testing.T) {
	for _, mtu := range []int{1200, 600, 300, 100, 40, 3} {
		s := &h264TestSample{seq: 65530}
		var want []h264AccessUnit
		for i := range h264TestAUs {
			nals := testAU(t, i)
			ts := uint32(1<<32 - 6000 + 6000*i)
			first := s.seq
			s.add(ts, h264Packetize(nals, mtu)...)
			want = append(want, wantUnit(ts, first, s.seq-1, nals))
		}

		units, err := h264AccessUnits(s.packets)
		require.NoError(t, err, "mtu %d", mtu)
		assert.Equal(t, want, units, "mtu %d", mtu)
		require.Len(t, units, 3)
		assert.True(t, units[0].idr, "mtu %d", mtu)
		for _, u := range units[1:] {
			assert.True(t, u.slice, "mtu %d", mtu)
			assert.False(t, u.idr, "mtu %d", mtu)
			assert.Nil(t, u.sps, "mtu %d", mtu)
			assert.Nil(t, u.pps, "mtu %d", mtu)
		}
	}
}

func TestH264AccessUnitsOrphanParameterSets(t *testing.T) {
	// At a simulcast start LiveKit forwards the low layer's SPS/PPS (ts T, no
	// marker), then the high layer's key frame at T+1, with no sequence gap.
	low := [][]byte{mustHex(t, h264TestSPS[0].nal), mustHex(t, h264TestSPS[0].pps)}
	high := testAU(t, 0)
	s := (&h264TestSample{seq: 10}).add(3000, stapA(low...)).unmark()
	s.add(3001, h264Packetize(high, 300)...)

	units, err := h264AccessUnits(s.packets)
	require.NoError(t, err)
	require.Len(t, units, 2)
	assert.Equal(t, wantUnit(3000, 10, 10, low), units[0])
	assert.False(t, units[0].slice)
	assert.Equal(t, wantUnit(3001, 11, s.seq-1, high), units[1])

	_, _, _, ok := h264KeyFrame(units[0])
	assert.False(t, ok)
	width, height, cp, ok := h264KeyFrame(units[1])
	require.True(t, ok)
	assert.Equal(t, 320, width)
	assert.Equal(t, 180, height)
	want, err := h264CodecPrivate([][]byte{high[1]}, [][]byte{high[2]})
	require.NoError(t, err)
	assert.Equal(t, want, cp)
}

func TestH264AccessUnitsSkipsPadding(t *testing.T) {
	au0, au1 := testAU(t, 0), testAU(t, 1)
	p0 := h264Packetize(au0, 300)

	s := (&h264TestSample{seq: 500}).padding(1000)
	s.add(1000, p0[:2]...).unmark()
	s.padding(1000)
	s.add(1000, p0[2:]...)
	s.padding(1000)
	s.packets = append(s.packets, &rtp.Packet{
		Header: rtp.Header{SequenceNumber: s.seq, Timestamp: 1000, Padding: true},
	})
	s.seq++
	first := s.seq
	s.add(4000, h264Packetize(au1, 300)...)
	s.padding(4000)

	units, err := h264AccessUnits(s.packets)
	require.NoError(t, err)
	require.Len(t, units, 2)
	assert.Equal(t, wantUnit(1000, 501, first-3, au0), units[0])
	assert.Equal(t, wantUnit(4000, first, s.seq-2, au1), units[1])
}

func TestH264AccessUnitsKeepsSEIDropsAUDAndFiller(t *testing.T) {
	au := testAU(t, 1)
	aud, p1, p2 := au[0], au[1], au[2]
	// Filler is sent alone, fragmented and aggregated.
	payloads := [][]byte{stapA(aud, h264TestSEI, p1), h264TestFiller}
	payloads = append(payloads, fuA(h264TestFiller, 120)...)
	payloads = append(payloads, stapA(h264TestFiller[:4], p2), aud)
	s := (&h264TestSample{seq: 7}).add(90, payloads...)

	units, err := h264AccessUnits(s.packets)
	require.NoError(t, err)
	require.Len(t, units, 1)
	assert.Equal(t, wantUnit(90, 7, s.seq-1, [][]byte{h264TestSEI, p1, p2}), units[0])
}

func TestH264AccessUnitsWithoutKeptNALUnits(t *testing.T) {
	s := (&h264TestSample{seq: 1}).add(10, []byte{0x09, 0x10})
	units, err := h264AccessUnits(s.packets)
	require.NoError(t, err)
	require.Len(t, units, 1)
	assert.Equal(t, h264AccessUnit{timestamp: 10, firstSeq: 1, lastSeq: 1}, units[0])

	units, err = h264AccessUnits(nil)
	assert.NoError(t, err)
	assert.Empty(t, units)

	s = (&h264TestSample{seq: 1}).padding(10).padding(10)
	units, err = h264AccessUnits(s.packets)
	assert.NoError(t, err)
	assert.Empty(t, units)
}

func TestH264AccessUnitsDoesNotAliasPackets(t *testing.T) {
	nals := testAU(t, 0)
	s := (&h264TestSample{seq: 1}).add(10, h264Packetize(nals, 1200)...)
	units, err := h264AccessUnits(s.packets)
	require.NoError(t, err)
	require.Len(t, units, 1)
	u := units[0]
	want := wantUnit(10, 1, s.seq-1, nals)

	for _, p := range s.packets {
		for i := range p.Payload {
			p.Payload[i] = 0
		}
	}
	// Growing a parameter set must not write into data.
	grown := append(u.sps[0], 0xaa, 0xbb, 0xcc, 0xdd)
	grown = append(grown, u.pps[0]...)
	assert.Len(t, grown, len(u.sps[0])+4+len(u.pps[0]))
	assert.Equal(t, want, u)
}

func TestH264AccessUnitsMalformed(t *testing.T) {
	p := testAU(t, 1)[1] // a P slice
	frag := fuA(testAU(t, 0)[3], 200)
	require.Greater(t, len(frag), 2)

	tests := []struct {
		name     string
		payloads [][]byte
		want     string
	}{
		{"STAP-B", [][]byte{{0x19, 0x00, 0x01, 0x00, 0x09, 0x41, 0x9a}}, "type 25"},
		{"MTAP16", [][]byte{{0x1a, 0x00, 0x01, 0x00, 0x0c, 0x00, 0x00, 0x01, 0x41, 0x9a}}, "type 26"},
		{"MTAP24", [][]byte{{0x1b, 0x00, 0x01, 0x00, 0x0d, 0x00, 0x00, 0x00, 0x01, 0x41, 0x9a}}, "type 27"},
		{"FU-B", [][]byte{{0x1d, 0x81, 0x00, 0x01, 0x9a}, {0x1d, 0x41, 0x9a}}, "type 29"},
		{"NAL type 0", [][]byte{{0x00, 0x9a}}, "type 0"},
		{"NAL type 30", [][]byte{{0x1e, 0x9a}}, "type 30"},
		{"NAL type 31", [][]byte{{0x1f, 0x9a}}, "type 31"},
		{"valid NAL unit after a malformed packet", [][]byte{{0x1e, 0x9a}, p}, "type 30"},
		{"empty payload", [][]byte{p, {}}, "empty payload"},
		{"forbidden bit on a single NAL unit", [][]byte{append([]byte{p[0] | 0x80}, p[1:]...)}, "forbidden"},
		{"forbidden bit on a STAP-A", [][]byte{append([]byte{0x98}, stapA(p)[1:]...)}, "forbidden"},
		{"forbidden bit inside a STAP-A", [][]byte{stapA(p, append([]byte{0xe1}, p[1:]...))}, "forbidden"},
		{"forbidden bit on an FU-A", [][]byte{append([]byte{frag[0][0] | 0x80}, frag[0][1:]...)}, "forbidden"},
		{"empty STAP-A", [][]byte{{0x18}}, "no NAL units"},
		{"STAP-A zero-length NAL unit", [][]byte{{0x18, 0x00, 0x00, 0x00, 0x02, 0x41, 0x9a}}, "zero-length"},
		{"STAP-A size overrun", [][]byte{{0x18, 0x00, 0x02, 0x41, 0x9a, 0x00, 0x05, 0x41, 0x9a}}, "overruns"},
		{"STAP-A size overrun by one byte", [][]byte{{0x18, 0x00, 0x02, 0x41, 0x9a, 0x00, 0x03, 0x41, 0x9a}}, "overruns"},
		{"STAP-A truncated size field", [][]byte{{0x18, 0x00, 0x02, 0x41, 0x9a, 0x00}}, "size field"},
		{"STAP-A inside a STAP-A", [][]byte{stapA(p, stapA(p))}, "type 24 inside STAP-A"},
		{"FU-A inside a STAP-A", [][]byte{stapA(p, frag[0])}, "type 28 inside STAP-A"},
		{"type 0 inside a STAP-A", [][]byte{stapA(p, []byte{0x00, 0x9a})}, "type 0 inside STAP-A"},
		{"FU-A without an FU header", [][]byte{{0x5c}}, "FU header"},
		{"FU-A without fragment data", [][]byte{{0x5c, 0x81}, {0x5c, 0x41, 0x9a}}, "without data"},
		{"FU-A with start and end bits", [][]byte{{0x5c, 0xc1, 0x9a}}, "start and end"},
		{"FU-A of type 0", [][]byte{{0x5c, 0x80, 0x9a}, {0x5c, 0x40, 0x9a}}, "type 0 inside FU-A"},
		{"FU-A of type 24", [][]byte{{0x5c, 0x98, 0x9a}, {0x5c, 0x58, 0x9a}}, "type 24 inside FU-A"},
		{"FU-A start while one is open", [][]byte{frag[0], frag[0], frag[len(frag)-1]}, "while another"},
		{"FU-A continuation without a start", frag[1:], "without a start"},
		{"FU-A end without a start", frag[len(frag)-1:], "without a start"},
		{"FU-A type changes", [][]byte{frag[0], append([]byte{frag[1][0], frag[1][1]&0xe0 | 1}, frag[1][2:]...)}, "type changes"},
		{"FU-A not ended", frag[:len(frag)-1], "not ended"},
		{"single NAL unit inside an open FU-A", [][]byte{frag[0], p, frag[len(frag)-1]}, "open FU-A"},
		{"STAP-A inside an open FU-A", [][]byte{frag[0], stapA(p, p), frag[len(frag)-1]}, "open FU-A"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			good0, good1 := testAU(t, 0), testAU(t, 2)
			s := (&h264TestSample{seq: 40}).add(1000, h264Packetize(good0, 300)...)
			want := []h264AccessUnit{wantUnit(1000, 40, s.seq-1, good0)}
			s.add(4000, tt.payloads...)
			s.add(7000, h264Packetize(good1, 300)...)

			units, err := h264AccessUnits(s.packets)
			require.Error(t, err)
			assert.ErrorContains(t, err, tt.want)
			assert.Equal(t, want, units)

			// At the end of the sample, the same unit is rejected too.
			s = (&h264TestSample{seq: 40}).add(1000, h264Packetize(good0, 300)...)
			s.add(4000, tt.payloads...)
			units, err = h264AccessUnits(s.packets)
			assert.ErrorContains(t, err, tt.want)
			assert.Equal(t, want, units)
		})
	}
}

func TestH264AccessUnitsSequenceGap(t *testing.T) {
	au0, au1, au2 := testAU(t, 0), testAU(t, 1), testAU(t, 2)

	t.Run("inside a unit", func(t *testing.T) {
		s := (&h264TestSample{seq: 1}).add(1000, h264Packetize(au0, 300)...)
		first := s.seq
		pk := h264Packetize(au1, 40)
		s.add(4000, pk[:1]...).skip(1).add(4000, pk[2:]...)
		s.add(7000, h264Packetize(au2, 300)...)

		units, err := h264AccessUnits(s.packets)
		assert.ErrorContains(t, err, "sequence gap")
		assert.Equal(t, []h264AccessUnit{wantUnit(1000, 1, first-1, au0)}, units)
	})

	t.Run("at a timestamp change", func(t *testing.T) {
		// The lost packets may belong to either unit, so the one before the gap is rejected.
		s := (&h264TestSample{seq: 1}).add(1000, h264Packetize(au0, 300)...)
		first := s.seq
		s.add(4000, h264Packetize(au1, 300)...).skip(2)
		s.add(7000, h264Packetize(au2, 300)...)

		units, err := h264AccessUnits(s.packets)
		assert.ErrorContains(t, err, "sequence gap")
		assert.Equal(t, []h264AccessUnit{wantUnit(1000, 1, first-1, au0)}, units)
	})

	t.Run("after a padding packet", func(t *testing.T) {
		s := (&h264TestSample{seq: 1}).padding(1000).skip(1).add(1000, h264Packetize(au0, 300)...)
		units, err := h264AccessUnits(s.packets)
		assert.ErrorContains(t, err, "sequence gap")
		assert.Empty(t, units)
	})

	t.Run("wrap-around is no gap", func(t *testing.T) {
		s := (&h264TestSample{seq: 65534}).add(1000, h264Packetize(au0, 100)...)
		require.Less(t, s.seq, uint16(100))
		units, err := h264AccessUnits(s.packets)
		require.NoError(t, err)
		assert.Equal(t, []h264AccessUnit{wantUnit(1000, 65534, s.seq-1, au0)}, units)
	})
}

func TestH264AccessUnitsNilPacket(t *testing.T) {
	s := (&h264TestSample{seq: 1}).add(1000, h264Packetize(testAU(t, 1), 300)...)
	packets := append(s.packets, nil)
	assert.NotPanics(t, func() {
		units, err := h264AccessUnits(packets)
		assert.Error(t, err)
		assert.Empty(t, units)
	})
}

func TestH264KeyFrame(t *testing.T) {
	au0 := testAU(t, 0)
	sps, pps, idr1, idr2 := au0[1], au0[2], au0[3], au0[4]
	p := testAU(t, 1)[1]
	blankSPS := mustHex(t, h264TestSPS[len(h264TestSPS)-1].nal)
	blankPPS := mustHex(t, h264TestSPS[len(h264TestSPS)-1].pps)
	blankIDR := []byte{0x65, 0x88, 0x84, 0x0a, 0xf2, 0x62, 0x80, 0x00, 0xa7, 0xbe}

	// 16x16 is accepted; 14x16 and 16x14 are not.
	small := func(right, bottom uint64) []byte {
		s := testSPS{profile: 66, constraints: 0xc0, level: 30, crop: []uint64{0, right, 0, bottom}}
		return s.nal()
	}

	unit := func(nals ...[]byte) h264AccessUnit {
		s := (&h264TestSample{seq: 1}).add(1000, h264Packetize(nals, 1200)...)
		units, err := h264AccessUnits(s.packets)
		require.NoError(t, err)
		require.Len(t, units, 1)
		return units[0]
	}

	width, height, cp, ok := h264KeyFrame(unit(au0...))
	require.True(t, ok)
	assert.Equal(t, 320, width)
	assert.Equal(t, 180, height)
	want, err := h264CodecPrivate([][]byte{sps}, [][]byte{pps})
	require.NoError(t, err)
	assert.Equal(t, want, cp)

	width, height, _, ok = h264KeyFrame(unit(small(0, 0), pps, idr1))
	assert.True(t, ok)
	assert.Equal(t, 16, width)
	assert.Equal(t, 16, height)

	// 8192x8192 is accepted; 8208 wide or high is not.
	large := func(widthMBsMinus1, heightMUsMinus1 uint64) []byte {
		s := testSPS{profile: 66, constraints: 0xc0, level: 30, widthMBsMinus1: widthMBsMinus1, heightMUsMinus1: heightMUsMinus1}
		return s.nal()
	}
	width, height, _, ok = h264KeyFrame(unit(large(511, 511), pps, idr1))
	assert.True(t, ok)
	assert.Equal(t, 8192, width)
	assert.Equal(t, 8192, height)

	for name, nals := range map[string][][]byte{
		"P frame":                       {p},
		"IDR without SPS and PPS":       {idr1, idr2},
		"IDR without PPS":               {sps, idr1, idr2},
		"IDR without SPS":               {pps, idr1, idr2},
		"SPS and PPS with a P slice":    {sps, pps, p},
		"SPS and PPS only":              {sps, pps},
		"LiveKit 2x2 blank key frame":   {blankSPS, blankPPS, blankIDR},
		"14 wide":                       {small(1, 0), pps, idr1},
		"14 high":                       {small(0, 1), pps, idr1},
		"8208 wide":                     {large(512, 511), pps, idr1},
		"8208 high":                     {large(511, 512), pps, idr1},
		"16777232 wide and high":        {large(1<<20, 1<<20), pps, idr1},
		"SPS that does not parse":       {sps[:6], pps, idr1},
		"second SPS under 16x16":        {sps, blankSPS, pps, idr1},
		"PPS that avcC cannot hold":     {sps, append([]byte{0x68}, make([]byte, 0x10000)...), idr1},
		"high profile SPS bit depth 16": {(&testSPS{profile: 100, level: 30, chromaFormat: 1, lumaMinus8: 8, widthMBsMinus1: 19, heightMUsMinus1: 14}).nal(), pps, idr1},
	} {
		s := (&h264TestSample{seq: 1}).add(1000, nals...)
		units, err := h264AccessUnits(s.packets)
		require.NoError(t, err, name)
		require.Len(t, units, 1, name)
		_, _, _, ok := h264KeyFrame(units[0])
		assert.False(t, ok, name)
	}
}

func TestIsH264KeyFrameStart(t *testing.T) {
	au0 := testAU(t, 0)
	aud, sps, pps, idr := au0[0], au0[1], au0[2], au0[3]
	spsFrags := fuA(sps, 10)

	pkt := func(payload []byte) *rtp.Packet { return &rtp.Packet{Payload: payload} }
	padded := pkt(sps)
	padded.Padding = true

	for name, tt := range map[string]struct {
		p    *rtp.Packet
		want bool
	}{
		"single SPS":                 {pkt(sps), true},
		"STAP-A starting with SPS":   {pkt(stapA(sps, pps)), true},
		"STAP-A holding SPS second":  {pkt(stapA(aud, sps, pps)), true},
		"FU-A start of an SPS":       {pkt(spsFrags[0]), true},
		"FU-A end of an SPS":         {pkt(spsFrags[len(spsFrags)-1]), false},
		"FU-A start of an IDR":       {pkt(fuA(idr, 100)[0]), false},
		"single IDR":                 {pkt(idr), false},
		"single PPS":                 {pkt(pps), false},
		"STAP-A without SPS":         {pkt(stapA(aud, pps)), false},
		"STAP-A with SPS after junk": {pkt(append(stapA(pps), 0x00, 0x30, 0x67)), false},
		"STAP-A cut before the SPS":  {pkt(stapA(pps, sps)[:9]), false},
		"STAP-A zero-length entry":   {pkt([]byte{0x18, 0x00, 0x00, 0x00, 0x01, 0x67}), false},
		"STAP-B holding SPS":         {pkt(append([]byte{0x19, 0x00, 0x00}, stapA(sps)[1:]...)), false},
		"padding packet":             {padded, false},
		"empty payload":              {pkt(nil), false},
		"FU-A without FU header":     {pkt([]byte{0x7c}), false},
		"nil packet":                 {nil, false},
	} {
		assert.Equal(t, tt.want, isH264KeyFrameStart(tt.p), name)
	}

	packets := h264Packetize(au0, 1200)
	assert.True(t, isH264KeyFrameStart(pkt(packets[0])))
	for _, p := range packets[1:] {
		assert.False(t, isH264KeyFrameStart(pkt(p)))
	}
}

// checkH264Unit checks the invariants of every unit h264AccessUnits returns.
func checkH264Unit(t *testing.T, u h264AccessUnit) {
	t.Helper()
	var idr, slice bool
	var sps, pps [][]byte
	for rest := u.data; len(rest) > 0; {
		require.GreaterOrEqual(t, len(rest), 5, "%x", u.data)
		n := int(binary.BigEndian.Uint32(rest))
		require.Positive(t, n)
		require.LessOrEqual(t, n, len(rest)-4)
		nal := rest[4 : 4+n]
		rest = rest[4+n:]

		assert.Zero(t, nal[0]&0x80)
		switch typ := nal[0] & 0x1f; typ {
		case 0, 9, 12, 24, 25, 26, 27, 28, 29, 30, 31:
			assert.Fail(t, "unexpected NAL unit type in data", "type %d", typ)
		case 1:
			slice = true
		case 5:
			slice, idr = true, true
		case 7:
			sps = append(sps, nal)
		case 8:
			pps = append(pps, nal)
		}
	}
	assert.Equal(t, idr, u.idr)
	assert.Equal(t, slice, u.slice)
	assert.Equal(t, sps, u.sps)
	assert.Equal(t, pps, u.pps)
	for _, s := range append(append([][]byte{}, u.sps...), u.pps...) {
		assert.Equal(t, len(s), cap(s))
	}
	assert.NotPanics(t, func() { h264KeyFrame(u) })
}

// FuzzH264AccessUnits reads its input in one of two modes, picked by the low
// bit of the first byte:
//   - packets: records of [flags, length, payload]. The flags mark padding,
//     a timestamp change, a sequence gap and the marker. Nothing may panic,
//     and every returned unit must be well formed and match its packets.
//   - round trip: an MTU byte, then records of [length, NAL unit]. The NAL
//     units are packetized as one access unit, which must come back exactly.
func FuzzH264AccessUnits(f *testing.F) {
	for i := range h264TestAUs {
		for _, mtu := range []int{1200, 100, 3} {
			var in []byte
			for j, p := range h264Packetize(testAU(f, i), mtu) {
				p = p[:min(len(p), 255)]
				flags := byte(0)
				if j == 2 {
					flags |= 2
				}
				in = append(in, flags, byte(len(p)))
				in = append(in, p...)
			}
			f.Add(append([]byte{0}, in...))
		}
		in := []byte{1, 40}
		for _, n := range testAU(f, i) {
			n = n[:min(len(n), 255)]
			in = append(in, byte(len(n)))
			in = append(in, n...)
		}
		f.Add(in)
	}
	f.Add([]byte{0, 1, 3, 0, 0, 0, 0x18, 0x00, 0x05})
	// A fragmented filler NAL unit, which leaves data empty.
	f.Add([]byte("1\x0309000000"))

	f.Fuzz(func(t *testing.T, in []byte) {
		if len(in) == 0 {
			return
		}
		if in[0]&1 == 0 {
			fuzzH264Packets(t, in[1:])
		} else {
			fuzzH264RoundTrip(t, in[1:])
		}
	})
}

func fuzzH264Packets(t *testing.T, in []byte) {
	s := &h264TestSample{seq: 65530}
	ts := uint32(1 << 31)
	for len(in) >= 2 {
		flags, n := in[0], int(in[1])
		in = in[2:]
		n = min(n, len(in))
		payload := in[:n]
		in = in[n:]

		if flags&2 != 0 {
			ts += 3000
		}
		if flags&4 != 0 {
			s.skip(1)
		}
		s.packets = append(s.packets, &rtp.Packet{
			Header:  rtp.Header{SequenceNumber: s.seq, Timestamp: ts, Padding: flags&1 != 0, Marker: flags&8 != 0},
			Payload: payload,
		})
		s.seq++
	}

	var runs []h264AccessUnit
	for _, p := range s.packets {
		assert.NotPanics(t, func() { isH264KeyFrameStart(p) })
		if p.Padding {
			continue
		}
		if len(runs) == 0 || runs[len(runs)-1].timestamp != p.Timestamp {
			runs = append(runs, h264AccessUnit{timestamp: p.Timestamp, firstSeq: p.SequenceNumber})
		}
		runs[len(runs)-1].lastSeq = p.SequenceNumber
	}

	units, err := h264AccessUnits(s.packets)
	require.LessOrEqual(t, len(units), len(runs))
	if err == nil {
		require.Len(t, units, len(runs))
	}
	for i, u := range units {
		assert.Equal(t, runs[i].timestamp, u.timestamp)
		assert.Equal(t, runs[i].firstSeq, u.firstSeq)
		assert.Equal(t, runs[i].lastSeq, u.lastSeq)
		checkH264Unit(t, u)
	}
}

func fuzzH264RoundTrip(t *testing.T, in []byte) {
	if len(in) == 0 {
		return
	}
	mtu := 3 + int(in[0])
	in = in[1:]
	var nals [][]byte
	for len(in) >= 1 {
		n := min(int(in[0]), len(in)-1)
		nal := append([]byte{}, in[1:1+n]...)
		in = in[1+n:]
		if len(nal) == 0 {
			continue
		}
		// A valid single NAL unit header: forbidden bit clear, type 1-23.
		nal[0] = nal[0]&0x60 | (1 + nal[0]%23)
		nals = append(nals, nal)
	}
	if len(nals) == 0 {
		return
	}

	s := (&h264TestSample{seq: 65000}).add(1234, h264Packetize(nals, mtu)...)
	units, err := h264AccessUnits(s.packets)
	require.NoError(t, err)
	require.Len(t, units, 1)
	u := units[0]
	checkH264Unit(t, u)
	if len(u.data) == 0 {
		u.data = nil
	}
	assert.Equal(t, wantUnit(1234, 65000, s.seq-1, nals), u)
}
