//go:build e2e && linux

package e2e

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// requireFFmpeg skips t unless ffmpeg and ffprobe are on PATH. With
// E2E_FFMPEG_REQUIRED=1, it fails t instead.
func requireFFmpeg(t *testing.T) {
	t.Helper()

	for _, bin := range []string{"ffmpeg", "ffprobe"} {
		if _, err := exec.LookPath(bin); err != nil {
			if os.Getenv("E2E_FFMPEG_REQUIRED") == "1" {
				require.FailNow(t, bin+" is not on PATH, and E2E_FFMPEG_REQUIRED=1")
			}

			t.Skipf("%s is not on PATH: the ffmpeg checks need ffmpeg and ffprobe", bin)
		}
	}
}

// fpsModeFlag returns the name of ffmpeg's video sync option, read once from
// ffmpeg -h full. ffmpeg before 5.1 has only -vsync, and ffmpeg 9 only
// -fps_mode.
var fpsModeFlag = sync.OnceValue(func() string {
	out, _ := exec.Command("ffmpeg", "-hide_banner", "-h", "full").Output()
	if strings.Contains(string(out), "-fps_mode") {
		return "-fps_mode"
	}

	return "-vsync"
})

// checkDecodes checks the H.264 recording at path with ffmpeg. The decode
// must print no error and conceal no frame, and ffprobe's frame count must
// equal frames. The file must also remux with -c copy to Matroska, as
// BBB's archive step does, with a duration within interval of span.
func checkDecodes(t *testing.T, path string, frames int, span, interval time.Duration) {
	t.Helper()
	requireFFmpeg(t)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	// ffmpeg conceals a slice it cannot decode, and logs that below the
	// error level. -xerror makes it exit on such a frame, and -err_detect
	// explode on a minor bitstream error as well.
	//
	// At a layer switch, two layers' key frames for one captured frame can
	// share a ms PTS. The null muxer reports that as a dts error. vfr drops
	// the repeat from the null output, and every frame is still decoded.
	out, err := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-xerror", "-err_detect", "explode",
		"-i", path, fpsModeFlag(), "vfr", "-f", "null", "-").CombinedOutput()
	require.NoError(t, err, "ffmpeg decode: %s", out)
	assert.Empty(t, strings.TrimSpace(string(out)), "ffmpeg decode errors")

	var probe struct {
		Streams []struct {
			CodecName    string `json:"codec_name"`
			Width        int    `json:"width"`
			Height       int    `json:"height"`
			NbReadFrames string `json:"nb_read_frames"`
		} `json:"streams"`
	}
	out, err = exec.CommandContext(ctx, "ffprobe", "-v", "error", "-select_streams", "v:0", "-count_frames",
		"-show_entries", "stream=codec_name,width,height,nb_read_frames", "-of", "json", path).Output()
	require.NoError(t, err, "ffprobe")
	require.NoError(t, json.Unmarshal(out, &probe))
	require.Len(t, probe.Streams, 1, "ffprobe: video streams")

	st := probe.Streams[0]
	t.Logf("ffprobe: %s %dx%d, %s frames decoded", st.CodecName, st.Width, st.Height, st.NbReadFrames)
	assert.Equal(t, "h264", st.CodecName)
	assert.Equal(t, strconv.Itoa(frames), st.NbReadFrames, "ffprobe: frames decoded")

	archived := strings.TrimSuffix(path, filepath.Ext(path)) + ".archive.mkv"
	out, err = exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-i", path, "-c", "copy", "-y", archived).CombinedOutput()
	require.NoError(t, err, "ffmpeg -c copy: %s", out)

	out, err = exec.CommandContext(ctx, "ffprobe", "-v", "error", "-show_entries", "format=duration",
		"-of", "default=noprint_wrappers=1:nokey=1", archived).Output()
	require.NoError(t, err, "ffprobe duration")

	duration, err := strconv.ParseFloat(strings.TrimSpace(string(out)), 64)
	require.NoError(t, err, "ffprobe duration %q", out)
	t.Logf("remuxed to %s: duration %.3f s, media span %v", filepath.Base(archived), duration, span)
	// ffmpeg may leave the last frame's duration out. PTS are whole ms.
	assert.InDelta(t, span.Seconds(), duration, (interval + time.Millisecond).Seconds(), "the remuxed file's duration")
}

// checkClipPictures checks that each picture decoded from the recording at
// path is the clip's next one, and that their count equals pictures. The
// first may be any of the clip's, and the clip loops. A frame with a slice
// lost or damaged decodes to a picture the clip does not have, though
// ffmpeg may report no error.
func checkClipPictures(t *testing.T, path, clip string, pictures int) {
	t.Helper()
	requireFFmpeg(t)

	ctx, cancel := context.WithTimeout(t.Context(), time.Minute)
	defer cancel()

	src := pictureHashes(ctx, t, clip)
	got := pictureHashes(ctx, t, path)
	require.NotEmpty(t, got, "pictures decoded from the recording")
	assert.Len(t, got, pictures, "pictures decoded from the recording")

	first := slices.Index(src, got[0])
	require.GreaterOrEqual(t, first, 0, "the recording's first picture is not one of the clip's")

	var wrong []int
	for i, h := range got {
		if h != src[(first+i)%len(src)] {
			wrong = append(wrong, i)
		}
	}

	t.Logf("pictures: %d decoded, from the clip's picture %d; %d not the clip's next", len(got), first, len(wrong))
	assert.Empty(t, wrong, "decoded pictures that are not the clip's next")
}

// pictureHashes decodes the H.264 stream at path and returns the MD5 of
// each picture, in output order.
func pictureHashes(ctx context.Context, t *testing.T, path string) []string {
	t.Helper()

	out, err := exec.CommandContext(ctx, "ffmpeg", "-nostdin", "-v", "error", "-i", path, "-f", "framemd5", "-").Output()
	require.NoError(t, err, "ffmpeg framemd5 %s", path)

	var hashes []string
	for line := range strings.Lines(string(out)) {
		if line = strings.TrimSpace(line); line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		fields := strings.Split(line, ",")
		hashes = append(hashes, strings.TrimSpace(fields[len(fields)-1]))
	}

	return hashes
}
