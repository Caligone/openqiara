package publisher

import (
	"fmt"
	"log/slog"

	"github.com/brutella/hap/accessory"
	"github.com/brutella/hap/characteristic"
	"github.com/brutella/hap/service"

	"github.com/caligone/openqiara/internal/hksv"
)

// HomeKit Secure Video asks the camera accessory for more services
// (HAP-NodeJS CameraController.constructService):
//   - CameraRecordingManagement: what it can record, what iOS chose, and
//     whether recording is on;
//   - CameraOperatingMode: the user's camera switches in Home;
//   - DataStreamTransportManagement: how iOS opens the HomeKit Data Stream
//     the recordings travel on, linked to the recording service;
//   - MotionSensor: the trigger, fed by the person detection.
//
// Types, formats and permissions: HAP-NodeJS CharacteristicDefinitions.ts
// and ServiceDefinitions.ts.
const (
	typeCameraOperatingMode          = "21A"
	typeDataStreamTransportMgmt      = "129"
	typeRecordingAudioActive         = "226"
	typeHomeKitCameraActive          = "21B"
	typeEventSnapshotsActive         = "223"
	typePeriodicSnapshotsActive      = "225"
	typeSupportedDataStreamTransport = "130"
	typeSetupDataStreamTransport     = "131"
)

// recordingOffer is what the camera offers: a 4 s prebuffer, 4 s fragments
// (an IDR every 0.5 s starts each), HAP-NodeJS's example resolutions.
// hlcamd only makes 1920x1080 at 30 fps.
var recordingOffer = hksv.Offer{PrebufferMs: 4000, FragmentMs: 4000, Resolutions: [][3]int{
	{1920, 1080, 30}, {1600, 1200, 30}, {1280, 960, 30}, {1280, 720, 30}, {640, 480, 30},
	{640, 360, 30}, {480, 360, 30}, {480, 270, 30}, {320, 240, 30}, {320, 240, 15}, {320, 180, 30},
}}

// recording holds the camera's HKSV services.
type recording struct {
	log *slog.Logger

	mgmt        *service.CameraRecordingManagement
	active      *characteristic.Active
	audioActive *characteristic.Int
	setupStream *characteristic.Bytes
	motion      *service.MotionSensor
}

// switch01 is a 0/1 characteristic, as HomeKit's camera switches are.
func switch01(typ string, perms ...string) *characteristic.Int {
	c := characteristic.NewInt(typ)
	c.Format = characteristic.FormatUInt8
	c.Permissions = perms
	c.SetMinValue(0)
	c.SetMaxValue(1)
	c.ValidVals = []int{0, 1}
	_ = c.SetValue(1)
	return c
}

func newRecording(acc *accessory.Camera, logger *slog.Logger) *recording {
	r := &recording{log: logger}
	rw := []string{characteristic.PermissionRead, characteristic.PermissionWrite, characteristic.PermissionEvents}

	r.mgmt = service.NewCameraRecordingManagement()
	r.mgmt.SupportedCameraRecordingConfiguration.SetValue(recordingOffer.Camera())
	r.mgmt.SupportedVideoRecordingConfiguration.SetValue(recordingOffer.Video())
	r.mgmt.SupportedAudioRecordingConfiguration.SetValue(recordingOffer.Audio())
	r.active = characteristic.NewActive()
	r.mgmt.AddC(r.active.C)
	// HAP-NodeJS gives it timed write too, but hap v0.0.35 panics on a
	// timed-write characteristic written without a prepare
	// (characteristics.go:198, nil timedWr): the panic took the whole
	// request, live video setup included. Plain writes are accepted.
	r.audioActive = switch01(typeRecordingAudioActive, rw...)
	_ = r.audioActive.SetValue(0)
	r.mgmt.AddC(r.audioActive.C)

	mode := service.New(typeCameraOperatingMode)
	mode.AddC(switch01(typeEventSnapshotsActive, rw...).C)
	mode.AddC(switch01(typeHomeKitCameraActive, rw...).C)
	mode.AddC(switch01(typePeriodicSnapshotsActive, rw...).C)

	stream := service.New(typeDataStreamTransportMgmt)
	supported := characteristic.NewBytes(typeSupportedDataStreamTransport)
	supported.Format = characteristic.FormatTLV8
	supported.Permissions = []string{characteristic.PermissionRead}
	supported.SetValue(hksv.SupportedTransport())
	stream.AddC(supported.C)
	r.setupStream = characteristic.NewBytes(typeSetupDataStreamTransport)
	r.setupStream.Format = characteristic.FormatTLV8
	r.setupStream.Permissions = []string{characteristic.PermissionRead, characteristic.PermissionWrite, characteristic.PermissionWriteResponse}
	r.setupStream.SetValue([]byte{})
	stream.AddC(r.setupStream.C)
	version := characteristic.NewVersion()
	version.SetValue("1.0")
	stream.AddC(version.C)
	r.mgmt.AddS(stream)

	// The trigger: linked to the recording service and active, as
	// HAP-NodeJS does; the home hub turned the camera down without it.
	r.motion = service.NewMotionSensor()
	statusActive := characteristic.NewStatusActive()
	statusActive.SetValue(true)
	r.motion.AddC(statusActive.C)
	r.mgmt.AddS(r.motion.S)

	// The live stream's own switch, which HAP-NodeJS gives every stream
	// management service of a recording camera.
	streamActive := characteristic.NewActive()
	_ = streamActive.SetValue(characteristic.ActiveActive)
	acc.StreamManagement1.AddC(streamActive.C)

	acc.AddS(r.mgmt.S)
	acc.AddS(mode)
	acc.AddS(stream)
	acc.AddS(r.motion.S)

	r.mgmt.SelectedCameraRecordingConfiguration.OnValueRemoteUpdate(func(b []byte) {
		s, err := hksv.ParseSelected(b)
		if err != nil {
			r.log.Warn("hksv: bad selected configuration", "error", err, "tlv", fmt.Sprintf("% x", b))
			return
		}
		r.log.Info("hksv: recording configuration selected", "config", s.String())
	})
	r.active.OnValueRemoteUpdate(func(v int) { r.log.Info("hksv: recording", "active", v == 1) })
	r.audioActive.OnValueRemoteUpdate(func(v int) { r.log.Info("hksv: recording audio", "active", v == 1) })
	r.setupStream.OnValueRemoteUpdate(func(b []byte) {
		r.log.Info("hksv: data stream setup requested", "tlv", fmt.Sprintf("% x", b))
	})
	return r
}

// setMotion reports motion in front of the camera, HKSV's trigger.
func (r *recording) setMotion(on bool) {
	r.motion.MotionDetected.SetValue(on)
}
