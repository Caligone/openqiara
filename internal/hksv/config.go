package hksv

import (
	"errors"
	"fmt"
)

// TLV types, as HAP-NodeJS names them (RecordingManagement.ts).
const (
	// SupportedCameraRecordingConfiguration
	tPrebuffer      = 0x01 // int32 ms
	tEventTriggers  = 0x02 // 8 bytes, bit field
	tContainers     = 0x03 // list
	tContainerType  = 0x01 // in a container: 0 = fragmented MP4
	tContainerParam = 0x02
	tFragmentLength = 0x01 // in the container's parameters: int32 ms

	// Supported/selected video
	tVideoCodec  = 0x01 // in the supported list: a codec configuration
	tCodecType   = 0x01 // 0 = H.264
	tCodecParams = 0x02
	tAttributes  = 0x03 // list of {width u16, height u16, fps u8}
	tProfile     = 0x01 // H.264: 0 baseline, 1 main, 2 high
	tLevel       = 0x02 // H.264: 0 3.1, 1 3.2, 2 4.0
	tBitrate     = 0x03 // selected only: int32 kbit/s
	tIFrame      = 0x04 // selected only: int32 ms
	tWidth       = 0x01
	tHeight      = 0x02
	tFrameRate   = 0x03

	// Supported/selected audio
	tAudioCodec    = 0x01 // in the supported list: a codec configuration
	tChannels      = 0x01
	tBitrateMode   = 0x02 // 0 variable, 1 constant
	tSampleRate    = 0x03 // 0 8k, 1 16k, 2 24k, 3 32k, 4 44.1k, 5 48k
	tMaxAudioBRate = 0x04 // selected only: uint32 kbit/s

	// SelectedCameraRecordingConfiguration
	tSelRecording = 0x01
	tSelVideo     = 0x02
	tSelAudio     = 0x03
)

// Values.
const (
	TriggerMotion = 1 << 0

	ContainerFMP4 = 0

	CodecH264 = 0

	AudioAACLC      = 0
	AudioAACELD     = 1
	BitrateVariable = 0
	SampleRate16kHz = 1
	defaultChannels = 1
)

// Offer is what the camera offers HomeKit Secure Video. The home hub
// turned down a narrow offer (Baseline only, one resolution, AAC-LC
// 16 kHz: "error updating the camera's setting", nothing written, live
// 2026-10-10), so it offers as broadly as HAP-NodeJS's example camera;
// what iOS selects is what to deliver.
type Offer struct {
	PrebufferMs int      // what the camera keeps before an event, at least 4000
	FragmentMs  int      // duration of each fMP4 fragment
	Resolutions [][3]int // width, height, fps
}

// Camera is the SupportedCameraRecordingConfiguration value.
func (o Offer) Camera() []byte {
	container := encode(
		item{tContainerType, u8(ContainerFMP4)},
		item{tContainerParam, encode(item{tFragmentLength, u32(uint32(o.FragmentMs))})},
	)
	return encode(
		item{tPrebuffer, u32(uint32(o.PrebufferMs))},
		item{tEventTriggers, u64(TriggerMotion)},
		item{tContainers, container},
	)
}

// Video is the SupportedVideoRecordingConfiguration value: H.264, every
// profile and level the streaming side offers too.
func (o Offer) Video() []byte {
	var attrs [][]byte
	for _, r := range o.Resolutions {
		attrs = append(attrs, encode(
			item{tWidth, u16(uint16(r[0]))},
			item{tHeight, u16(uint16(r[1]))},
			item{tFrameRate, u8(byte(r[2]))},
		))
	}
	params := encode(append(
		list(tProfile, u8(0), u8(1), u8(2)),  // baseline, main, high
		list(tLevel, u8(0), u8(1), u8(2))..., // 3.1, 3.2, 4.0
	)...)
	codec := encode(append([]item{
		{tCodecType, u8(CodecH264)},
		{tCodecParams, params},
	}, list(tAttributes, attrs...)...)...)
	return encode(item{tVideoCodec, codec})
}

// Audio is the SupportedAudioRecordingConfiguration value: AAC-LC, mono,
// every sample rate.
func (o Offer) Audio() []byte {
	codec := encode(
		item{tCodecType, u8(AudioAACLC)},
		item{tCodecParams, encode(append([]item{
			{tChannels, u8(defaultChannels)},
			{tBitrateMode, u8(BitrateVariable)},
		}, list(tSampleRate, u8(0), u8(1), u8(2), u8(3), u8(4), u8(5))...)...)},
	)
	return encode(item{tAudioCodec, codec})
}

// Selected is the configuration iOS chose among the offer, written to
// SelectedCameraRecordingConfiguration.
type Selected struct {
	PrebufferMs int
	Triggers    uint64
	Container   int
	FragmentMs  int

	VideoCodec   int
	Profile      int
	Level        int
	BitrateKbps  int
	IFrameMs     int
	Width        int
	Height       int
	FPS          int
	AudioCodec   int
	Channels     int
	BitrateMode  int
	SampleRate   int
	AudioMaxKbps int
}

// ParseSelected decodes a SelectedCameraRecordingConfiguration value.
func ParseSelected(b []byte) (Selected, error) {
	var s Selected
	top, err := decode(b)
	if err != nil {
		return s, err
	}
	rec, video, audio := first(top, tSelRecording), first(top, tSelVideo), first(top, tSelAudio)
	if rec == nil || video == nil {
		return s, errors.New("hksv: selected configuration without recording or video")
	}

	r, err := decode(rec)
	if err != nil {
		return s, err
	}
	s.PrebufferMs = int(num(first(r, tPrebuffer)))
	s.Triggers = num(first(r, tEventTriggers))
	if c := first(r, tContainers); c != nil {
		ci, err := decode(c)
		if err != nil {
			return s, err
		}
		s.Container = int(num(first(ci, tContainerType)))
		if p := first(ci, tContainerParam); p != nil {
			pi, err := decode(p)
			if err != nil {
				return s, err
			}
			s.FragmentMs = int(num(first(pi, tFragmentLength)))
		}
	}

	v, err := decode(video)
	if err != nil {
		return s, err
	}
	s.VideoCodec = int(num(first(v, tCodecType)))
	if p := first(v, tCodecParams); p != nil {
		pi, err := decode(p)
		if err != nil {
			return s, err
		}
		s.Profile = int(num(first(pi, tProfile)))
		s.Level = int(num(first(pi, tLevel)))
		s.BitrateKbps = int(num(first(pi, tBitrate)))
		s.IFrameMs = int(num(first(pi, tIFrame)))
	}
	if a := first(v, tAttributes); a != nil {
		ai, err := decode(a)
		if err != nil {
			return s, err
		}
		s.Width = int(num(first(ai, tWidth)))
		s.Height = int(num(first(ai, tHeight)))
		s.FPS = int(num(first(ai, tFrameRate)))
	}

	if audio != nil {
		a, err := decode(audio)
		if err != nil {
			return s, err
		}
		s.AudioCodec = int(num(first(a, tCodecType)))
		if p := first(a, tCodecParams); p != nil {
			pi, err := decode(p)
			if err != nil {
				return s, err
			}
			s.Channels = int(num(first(pi, tChannels)))
			s.BitrateMode = int(num(first(pi, tBitrateMode)))
			s.SampleRate = int(num(first(pi, tSampleRate)))
			s.AudioMaxKbps = int(num(first(pi, tMaxAudioBRate)))
		}
	}
	return s, nil
}

func (s Selected) String() string {
	return fmt.Sprintf("prebuffer %d ms, triggers %#x, container %d, fragments %d ms, video codec %d profile %d level %d %dx%d@%d %d kbit/s I every %d ms, audio codec %d %d ch rate %d max %d kbit/s",
		s.PrebufferMs, s.Triggers, s.Container, s.FragmentMs,
		s.VideoCodec, s.Profile, s.Level, s.Width, s.Height, s.FPS, s.BitrateKbps, s.IFrameMs,
		s.AudioCodec, s.Channels, s.SampleRate, s.AudioMaxKbps)
}

// SupportedTransport is the SupportedDataStreamTransportConfiguration
// value: the HomeKit Data Stream over TCP (transport type 0).
func SupportedTransport() []byte {
	return encode(item{0x01, encode(item{0x01, u8(0)})})
}
