package hksv

import (
	"bytes"
	"testing"
)

var offer = Offer{PrebufferMs: 4000, FragmentMs: 4000, Resolutions: [][3]int{{1920, 1080, 30}, {1280, 720, 30}}}

// The offer's TLVs, worked out by hand from HAP-NodeJS's encoders.
func TestOfferCamera(t *testing.T) {
	want := []byte{
		0x01, 4, 0xa0, 0x0f, 0, 0, // prebuffer 4000 ms
		0x02, 8, 1, 0, 0, 0, 0, 0, 0, 0, // triggers: motion
		0x03, 11, // containers
		0x01, 1, 0, // fMP4
		0x02, 6, 0x01, 4, 0xa0, 0x0f, 0, 0, // fragment 4000 ms
	}
	if got := offer.Camera(); !bytes.Equal(got, want) {
		t.Fatalf("\n got % x\nwant % x", got, want)
	}
}

func TestOfferVideo(t *testing.T) {
	want := []byte{
		0x01, 59, // codec configuration
		0x01, 1, 0, // H.264
		0x02, 26, // parameters
		0x01, 1, 0, 0, 0, 0x01, 1, 1, 0, 0, 0x01, 1, 2, // profiles baseline, main, high
		0x02, 1, 0, 0, 0, 0x02, 1, 1, 0, 0, 0x02, 1, 2, // levels 3.1, 3.2, 4.0
		0x03, 11, 0x01, 2, 0x80, 0x07, 0x02, 2, 0x38, 0x04, 0x03, 1, 30, // 1920x1080@30
		0, 0,
		0x03, 11, 0x01, 2, 0x00, 0x05, 0x02, 2, 0xd0, 0x02, 0x03, 1, 30, // 1280x720@30
	}
	if got := offer.Video(); !bytes.Equal(got, want) {
		t.Fatalf("\n got % x\nwant % x", got, want)
	}
}

func TestOfferAudio(t *testing.T) {
	want := []byte{
		0x01, 39,
		0x01, 1, 0, // AAC-LC
		0x02, 34, 0x01, 1, 1, 0x02, 1, 0, // mono, variable
		0x03, 1, 0, 0, 0, 0x03, 1, 1, 0, 0, 0x03, 1, 2, 0, 0, // 8, 16, 24 kHz
		0x03, 1, 3, 0, 0, 0x03, 1, 4, 0, 0, 0x03, 1, 5, // 32, 44.1, 48 kHz
	}
	if got := offer.Audio(); !bytes.Equal(got, want) {
		t.Fatalf("\n got % x\nwant % x", got, want)
	}
}

func TestParseSelected(t *testing.T) {
	rec := encode(
		item{tPrebuffer, u32(4000)},
		item{tEventTriggers, u64(TriggerMotion)},
		item{tContainers, encode(item{tContainerType, u8(0)}, item{tContainerParam, encode(item{tFragmentLength, u32(4000)})})},
	)
	video := encode(
		item{tCodecType, u8(CodecH264)},
		item{tCodecParams, encode(item{tProfile, u8(0)}, item{tLevel, u8(2)}, item{tBitrate, u32(2000)}, item{tIFrame, u32(4000)})},
		item{tAttributes, encode(item{tWidth, u16(1920)}, item{tHeight, u16(1080)}, item{tFrameRate, u8(30)})},
	)
	audio := encode(
		item{tCodecType, u8(AudioAACLC)},
		item{tCodecParams, encode(item{tChannels, u8(1)}, item{tBitrateMode, u8(0)}, item{tSampleRate, u8(1)}, item{tMaxAudioBRate, u32(24)})},
	)
	s, err := ParseSelected(encode(item{tSelRecording, rec}, item{tSelVideo, video}, item{tSelAudio, audio}))
	if err != nil {
		t.Fatal(err)
	}
	want := Selected{PrebufferMs: 4000, Triggers: 1, FragmentMs: 4000, Level: 2, BitrateKbps: 2000, IFrameMs: 4000,
		Width: 1920, Height: 1080, FPS: 30, Channels: 1, SampleRate: 1, AudioMaxKbps: 24}
	if s != want {
		t.Fatalf("\n got %+v\nwant %+v", s, want)
	}
	if _, err := ParseSelected([]byte{1, 5, 0}); err == nil {
		t.Fatal("truncated value accepted")
	}
}

func TestLongValuesAreChunked(t *testing.T) {
	long := bytes.Repeat([]byte{7}, 300)
	b := encode(item{9, long})
	if len(b) != 2+255+2+45 || b[0] != 9 || b[1] != 255 || b[257] != 9 || b[258] != 45 {
		t.Fatalf("chunking: %d bytes", len(b))
	}
	items, err := decode(b)
	if err != nil || len(items) != 1 || !bytes.Equal(items[0].v, long) {
		t.Fatalf("decode: %d items, %v", len(items), err)
	}
}
