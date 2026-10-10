package camera

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
	"github.com/caligone/openqiara/internal/config"
)

// fakeMCU stands for charmux: frames in through rx and ctrlRx, frames out
// on pkt.
type fakeMCU struct {
	events chan charmux.Event
	pkt    chan charmux.ManagedFrame

	mu   sync.Mutex
	ctrl [][]byte
}

func newFakeMCU() *fakeMCU {
	return &fakeMCU{events: make(chan charmux.Event, 16), pkt: make(chan charmux.ManagedFrame, 64)}
}

func (m *fakeMCU) Connect(context.Context) error { return nil }
func (m *fakeMCU) GetInfo(context.Context) (*charmux.MCUInfo, error) {
	return &charmux.MCUInfo{Address: 1}, nil
}
func (m *fakeMCU) GetNet(context.Context) (byte, error) { return 5, nil }
func (m *fakeMCU) SendShutter(bool) error               { return nil }
func (m *fakeMCU) Events() chan charmux.Event           { return m.events }
func (m *fakeMCU) Close() error                         { close(m.events); return nil }

func (m *fakeMCU) SendPKT(_ context.Context, b []byte) error {
	f, err := charmux.DeserializeManagedFrame(b)
	if err != nil {
		return err
	}
	m.pkt <- *f
	return nil
}

func (m *fakeMCU) SendRawCTRL(b []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.ctrl = append(m.ctrl, append([]byte(nil), b...))
	return nil
}

func (m *fakeMCU) rx(f charmux.ManagedFrame) {
	m.events <- charmux.Event{Channel: charmux.ChannelPKT, Data: f.Serialize()}
}

func (m *fakeMCU) ctrlRx(b ...byte) {
	m.events <- charmux.Event{Channel: charmux.ChannelCTRL, Data: b}
}

// sent is the next frame the client put on the air.
func (m *fakeMCU) sent(t *testing.T) charmux.ManagedFrame {
	t.Helper()
	select {
	case f := <-m.pkt:
		return f
	case <-time.After(2 * time.Second):
		t.Fatal("no frame sent")
		return charmux.ManagedFrame{}
	}
}

func (m *fakeMCU) quiet(t *testing.T) {
	t.Helper()
	select {
	case f := <-m.pkt:
		t.Fatalf("unexpected frame %+v", f)
	case <-time.After(100 * time.Millisecond):
	}
}

// A made-up vendor key: the real ones stay on the camera.
var testKey = [32]byte{0xa1, 0xa2, 0xa3, 0xa4, 0xa5, 0xa6, 31: 0xff}

// The production camera's sensors, with made-up UIDs and code.
var (
	kpd = config.SensorEntry{ID: 14, Type: "KPD", KPDCode: "1234", Radio: config.RadioNode{Addr: 2, UID: "0000000000000014", SystemIndex: 0}}
	dws = config.SensorEntry{ID: 23, Type: "DWS", Radio: config.RadioNode{Addr: 5, UID: "0000000000000023", SystemIndex: 2}}
	srn = config.SensorEntry{ID: 29, Type: "SRN", Radio: config.RadioNode{Addr: 6, UID: "0000000000000029", SystemIndex: 3}}
)

func newRadio(t *testing.T, sensors ...config.SensorEntry) (*RadioClient, *fakeMCU, *config.Store) {
	t.Helper()
	dir := t.TempDir()
	store := config.NewStore(filepath.Join(dir, "config.json"))
	if err := store.Update(func(c *config.Config) { c.Sensors = sensors }); err != nil {
		t.Fatal(err)
	}
	keys := filepath.Join(dir, "vendors.keys")
	if err := os.WriteFile(keys, []byte("test: "+base64.StdEncoding.EncodeToString(testKey[:])+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	mcu := newFakeMCU()
	c := NewRadioClient(mcu, store, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c.KeysPath, c.ManifestDir, c.BytecodeDir = keys, dir, dir
	c.FbxhomeXML = filepath.Join(dir, "fbxhome.xml.*")
	if err := c.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = c.Close() })
	return c, mcu, store
}

func fromSensor(addr, counter uint32, wflags byte, payload ...byte) charmux.ManagedFrame {
	return charmux.ManagedFrame{GWDst: 1, GWSrc: addr, Counter: counter, Src: addr,
		Flags: 0x80 | charmux.FlagZ | charmux.FlagW, WFlags: wflags, Payload: payload}
}

// state is a sensor's state report: 55 01 <ts:4> <kind|signal> <value>.
func state(addr, counter uint32, value byte) charmux.ManagedFrame {
	return fromSensor(addr, counter, 0x01, 0x55, 0x01, 0, 0, 0, 0, 0x01, value)
}

func nextEvent(t *testing.T, c *RadioClient) SensorEvent {
	t.Helper()
	select {
	case ev := <-c.Events():
		return ev
	case <-time.After(2 * time.Second):
		t.Fatal("no sensor event")
		return SensorEvent{}
	}
}

// TestRadioEvents: frames of sensors imported from fbxhome come out under
// their fbxhome ids, and get answered.
func TestRadioEvents(t *testing.T) {
	siren := srn
	siren.Battery = 76 // last level saved: no 0 published before its heartbeat
	c, mcu, store := newRadio(t, kpd, dws, siren)
	// Saved raw, shown in percent as fbxhome did: 76×98/255+2 = 31.
	if s, _ := c.ReadSensor(context.Background(), 29); s == nil || s.Battery != 31 {
		t.Errorf("siren at start = %+v, want its saved battery, 31 %%", s)
	}
	if f := mcu.sent(t); f.Route != 6 || !bytes.Equal(f.Payload, []byte{0x55, 0x06}) {
		t.Fatalf("first frame = %+v, want the siren asked for its state", f)
	}

	if _, err := c.ReadSensor(context.Background(), 23); err == nil {
		t.Error("door state read before the door reported it")
	}
	mcu.rx(state(5, 10, 0)) // door opened
	if ev := nextEvent(t, c); ev.SensorID != 23 || !ev.Sensor.Open || ev.Sensor.Type != "DWS" {
		t.Errorf("event = %+v, want door 23 open", ev)
	}
	if ack := mcu.sent(t); ack.GWDst != 5 || ack.AckCnt != 10 || ack.Flags != 0x0084 {
		t.Errorf("ack = %+v, want fbxhome's DWS ack", ack)
	}
	if s, err := c.ReadSensor(context.Background(), 23); err != nil || !s.Open {
		t.Errorf("read %+v %v, want open", s, err)
	}
	// Opened again (its close was lost): it must go out again.
	mcu.rx(state(5, 11, 0))
	if ev := nextEvent(t, c); ev.SensorID != 23 || !ev.Sensor.Open {
		t.Errorf("event = %+v, want door 23 open again", ev)
	}
	mcu.sent(t)

	mcu.rx(state(2, 12, 1)) // keypad day button
	if ev := nextEvent(t, c); ev.SensorID != 14 || ev.Sensor.KPDState != "armed_away" {
		t.Errorf("event = %+v, want keypad 14 armed_away", ev)
	}
	mcu.sent(t)

	// A battery report must not replay the keypad's last button: main
	// turns any KPDState into an alarm command.
	mcu.rx(fromSensor(2, 13, 0x82, 0x81, 0xff))
	if ev := nextEvent(t, c); ev.Sensor.KPDState != "" || ev.Sensor.Battery != 100 {
		t.Errorf("event = %+v, want a battery update without action", ev)
	}
	if b := store.Get().Sensors[0].Battery; b != 255 {
		t.Errorf("battery saved as %d, want 255 for the next start", b)
	}
	if f := mcu.sent(t); f.GWDst != 2 || f.WFlags != 0xcc {
		t.Errorf("answer = %+v, want read_status", f)
	}
}

// TestRadioSiren: the test sound carries its duration, in quarter
// seconds, like fbxhome's: the siren stops by itself, no stop frame
// follows and the call does not last the sound. No 55 0b before it.
func TestRadioSiren(t *testing.T) {
	c, mcu, _ := newRadio(t, dws, srn)
	mcu.sent(t) // get-state

	for _, tc := range []struct {
		d    time.Duration
		want byte
	}{{60 * time.Second, 0xf0}, {2 * time.Minute, 0xff}} {
		if err := c.TriggerSirenAlarm(context.Background(), 29, tc.d); err != nil {
			t.Fatal(err)
		}
		want := []byte{0x55, 0x05, 0x01, 0x64, tc.want}
		if f := mcu.sent(t); f.Route != 6 || f.WFlags != 0x01 || !bytes.Equal(f.Payload, want) {
			t.Errorf("frame = %+v, want %x to the siren", f, want)
		}
		mcu.quiet(t)
	}
	if err := c.TriggerSirenAlarm(context.Background(), 23, 0); err == nil {
		t.Error("a door was made to wail")
	}
}

// TestRadioSirenNative: arming carries the delays and one bit per system
// index of the sensors; the siren's commands and a relayed alarm are the
// frames the siren took on the hardware (2026-10-09).
func TestRadioSirenNative(t *testing.T) {
	c, mcu, _ := newRadio(t, kpd, dws, srn)
	mcu.sent(t) // get-state
	ctx := context.Background()
	send := func(err error, want ...[]byte) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		for _, w := range want {
			if f := mcu.sent(t); f.Route != 6 || !bytes.Equal(f.Payload, w) {
				t.Errorf("frame = %x, want %x", f.Payload, w)
			}
		}
	}
	send(c.ArmSiren(ctx, 29, SirenArming{ExitDelay: 10 * time.Second, EntryDelay: 300 * time.Second,
		Alert: 7 * time.Second, Active: []int{23, 14, 99}, Delayed: []int{23}}),
		[]byte{0x55, 0x04, 10, 255, 4, 0x05, 0x64, 0x02, 0, 0, 0, 0, 0, 0, 0, 0x05, 0, 0, 0, 0, 0, 0, 0, 0x04}, []byte{0x55, 0x06})
	send(c.ArmSiren(ctx, 29, SirenArming{Quiet: true}),
		[]byte{0x55, 0x04, 0, 0, 0, 0, 0x64, 0x03, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}, []byte{0x55, 0x06})
	send(c.SirenEntryDelay(ctx, 29), []byte{0x55, 0x05, 0x04})
	send(c.SirenAlert(ctx, 29), []byte{0x55, 0x05, 0x05})
	send(c.StopSiren(ctx, 29), []byte{0x55, 0x05, 0x00})
	if err := c.ArmSiren(ctx, 23, SirenArming{}); err == nil {
		t.Error("a door was armed as a siren")
	}

	// Its state reports come out as events; one that arrives after a later
	// one, as on the hardware, is dropped.
	report := func(counter uint32, state byte) {
		mcu.rx(charmux.ManagedFrame{GWDst: 1, GWSrc: 6, Counter: counter, Src: 6, Flags: 0x0086, WFlags: 0x01,
			Payload: []byte{0x55, 0x01, 1, 2, 3, 4, 0, state}})
	}
	report(1000, 0x05)
	if ev := nextEvent(t, c); ev.SensorID != 29 || ev.Sensor.SirenState != "alert" || !ev.SirenReport {
		t.Errorf("event = %+v, want siren 29 in alert", ev)
	}
	report(999, 0x03) // the answer to an earlier command
	report(1001, 0x06)
	if ev := nextEvent(t, c); ev.Sensor.SirenState != "alert_over" {
		t.Errorf("event = %+v, want the stale report dropped, then alert_over", ev)
	}
	report(5, 0x00) // far behind: counting again from the start
	if ev := nextEvent(t, c); ev.Sensor.SirenState != "off" {
		t.Errorf("event = %+v, want off", ev)
	}
}

// silent lets the sensors go silent for d, as the minute tick sees it.
func silent(c *RadioClient, d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.markSilent(time.Now().Add(d))
}

// TestRadioReachability: a frame the siren never got leaves it reachable;
// silent past its limit it shows unreachable, its next frame reachable
// again. A door gets far longer, its heartbeat coming every ~12 h.
func TestRadioReachability(t *testing.T) {
	c, mcu, _ := newRadio(t, srn, dws)
	start := mcu.sent(t)
	// UNREACHABLE report, as the MCU words it (charmux TestMCUDeliveryReport).
	report := []byte{0x01, 0x01, 0x00, 0x2a, 0x01, 0x40, 0x00, 0x01, byte(start.Counter)}
	mcu.events <- charmux.Event{Channel: charmux.ChannelPKT, Data: report}
	mcu.quiet(t)
	silent(c, 29*time.Minute)
	for _, s := range c.CachedSensors() {
		if !s.Reachable {
			t.Errorf("sensor %d unreachable before its limit", s.ID)
		}
	}
	silent(c, 31*time.Minute)
	if ev := nextEvent(t, c); ev.SensorID != 29 || ev.Sensor.Reachable {
		t.Errorf("event = %+v, want siren 29 unreachable", ev)
	}
	silent(c, 25*time.Hour)
	silent(c, 27*time.Hour)
	if ev := nextEvent(t, c); ev.SensorID != 23 || ev.Sensor.Reachable {
		t.Errorf("event = %+v, want door 23 unreachable", ev)
	}
	mcu.rx(charmux.ManagedFrame{GWDst: 1, GWSrc: 6, Counter: 40, Src: 6, Flags: 0x0082, WFlags: 0x01, Payload: []byte{0x55, 0x0e}})
	if ev := nextEvent(t, c); ev.SensorID != 29 || !ev.Sensor.Reachable {
		t.Errorf("event = %+v, want siren 29 reachable", ev)
	}
}

// TestRadioSensorsNotServed: a sensor without a radio address, or a keypad
// whose code is not valid or not set, is not served and shows
// unreachable; the keypad is not left to disarm with its off button alone.
func TestRadioSensorsNotServed(t *testing.T) {
	for _, code := range []string{"12a4", ""} {
		badKPD := kpd
		badKPD.KPDCode = code
		testRadioSensorsNotServed(t, badKPD)
	}
}

func testRadioSensorsNotServed(t *testing.T, badKPD config.SensorEntry) {
	c, mcu, _ := newRadio(t, badKPD, config.SensorEntry{ID: 40, Type: "PIR"})
	mcu.rx(fromSensor(2, 3, 0x01, 0x55, 0x09))
	mcu.quiet(t)
	for _, s := range c.CachedSensors() {
		if s.Reachable {
			t.Errorf("sensor %d shows reachable", s.ID)
		}
	}
	if len(c.CachedSensors()) != 2 {
		t.Errorf("sensors = %+v, want both listed", c.CachedSensors())
	}
}

// TestRadioPairing: a new sensor gets the next id, address and system
// index, and the engine serves it; paired again, it keeps its id.
func TestRadioPairing(t *testing.T) {
	c, mcu, store := newRadio(t, kpd, dws, srn)
	mcu.sent(t)

	uid := []byte{1, 2, 3, 4, 5, 6, 7, 8}
	pair := func(addr byte) *Sensor {
		t.Helper()
		if _, err := c.StartPairing(context.Background(), "PIR"); err != nil {
			t.Fatal(err)
		}
		mcu.ctrlRx(append(append(append([]byte{0x17}, testKey[:6]...), uid...), "HOMELABPIR00ACFD"...)...)
		mcu.ctrlRx(append([]byte{0x1f}, uid...)...)
		mcu.ctrlRx(append(append([]byte{0x1e}, uid...), addr, 0, 0, 0)...)
		mcu.ctrlRx(0x16)
		for range 100 {
			s, done, err := c.PollPairing(context.Background(), 1)
			if err != nil {
				t.Fatal(err)
			}
			if done {
				return s
			}
			time.Sleep(50 * time.Millisecond)
		}
		t.Fatal("pairing never finished")
		return nil
	}

	s := pair(7)
	if s.ID != 30 || s.Type != "PIR" {
		t.Errorf("paired %+v, want PIR 30", s)
	}
	se := store.Get().Sensors[3]
	if se.ID != 30 || se.Radio.Addr != 7 || se.Radio.SystemIndex != 1 || store.Get().RadioNextAddr != 8 {
		t.Errorf("saved %+v next %d, want addr 7, index 1, next 8", se, store.Get().RadioNextAddr)
	}
	mcu.mu.Lock()
	start := mcu.ctrl[0]
	mcu.mu.Unlock()
	if start[0] != 0x15 || start[1] != 7 {
		t.Errorf("pairing started with %x, want address 7", start)
	}

	mcu.rx(fromSensor(7, 0, 0x82, 0xf1, 0x00, 0x01))
	if f := mcu.sent(t); f.GWDst != 7 || f.WFlags != 0xcc {
		t.Errorf("answer = %+v, want read_status to the new sensor", f)
	}

	if s := pair(8); s.ID != 30 {
		t.Errorf("paired again as %d, want 30", s.ID)
	}
	if se := store.Get().Sensors[3]; se.Radio.Addr != 8 || se.Radio.SystemIndex != 1 || len(store.Get().Sensors) != 4 {
		t.Errorf("saved %+v, want addr 8 and the same index", se)
	}
}

// TestRadioDeleteSensor: a deleted sensor is no longer answered.
func TestRadioDeleteSensor(t *testing.T) {
	c, mcu, store := newRadio(t, dws, srn)
	mcu.sent(t)
	if err := c.DeleteSensor(context.Background(), 23); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(store.Get().Sensors, func(se config.SensorEntry) bool { return se.ID == 23 }) {
		t.Error("sensor 23 still in the config")
	}
	mcu.rx(state(5, 10, 0))
	mcu.quiet(t)
	if len(c.CachedSensors()) != 1 {
		t.Errorf("sensors = %+v, want the siren only", c.CachedSensors())
	}
}

// TestRadioKeypadCode: a new code in the config goes out at the keypad's
// next wake after Reload.
func TestRadioKeypadCode(t *testing.T) {
	c, mcu, store := newRadio(t, kpd)
	if err := store.Update(func(cfg *config.Config) {
		cfg.Sensors = []config.SensorEntry{kpd}
		cfg.Sensors[0].KPDCode = "5678"
	}); err != nil {
		t.Fatal(err)
	}
	c.Reload()
	mcu.rx(fromSensor(2, 3, 0x01, 0x55, 0x09))
	// '5'..'8' are 4..7, two digits a byte, low nibble first.
	if f := mcu.sent(t); !bytes.Equal(f.Payload, []byte{0x03, 0x00, 0x04, 0x54, 0x76}) {
		t.Errorf("code list = %x", f.Payload)
	}
}

const fbxhomeXMLTemplate = `<?xml version="1.0" encoding="utf-8"?>
<domus counter="%COUNTER%" max_id="31">
  <Adapter discarded="false" domus_addr="1" domus_next_addr="%NEXT%" id="12" type="Adapter.DomusAdapter">
  </Adapter>
  <Node alarm_type="0" discarded="false" id="2" type="Node.HlAlarm">
  </Node>
  <Node adapter="12" discarded="false" domus_addr="2" domus_ch_key="secret" domus_item_id="00000000000000aa" domus_key="secret" id="14" system_index="0" type="Node.DomusNode.HLKpd">
    <Code label="admin" password="0000" valid="true" />
  </Node>
  <Node adapter="12" battery="255" discarded="false" domus_addr="3" domus_item_id="00000000000000bb" id="17" system_index="1" type="Node.DomusNode.HlPir">
  </Node>
  <Node adapter="12" discarded="false" domus_addr="4" domus_item_id="00000000000000cc" id="20" system_index="4" type="Node.DomusNode.HlDws">
  </Node>
  <Node adapter="12" discarded="false" domus_addr="6" domus_item_id="00000000000000dd" id="29" system_index="3" type="Node.DomusNode.HlSrn">
  </Node>
</domus>
`

func writeFbxhomeXML(t *testing.T, path string, counter, next string) {
	t.Helper()
	x := bytes.ReplaceAll([]byte(fbxhomeXMLTemplate), []byte("%COUNTER%"), []byte(counter))
	x = bytes.ReplaceAll(x, []byte("%NEXT%"), []byte(next))
	if err := os.WriteFile(path, x, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestImportFbxhomeRadio: the newest fbxhome state that reads gives the
// sensors their radio identity by id, and the keypad fbxhome's code when
// the config has none; deleted sensors and sensors that have one are
// left alone.
func TestImportFbxhomeRadio(t *testing.T) {
	dir := t.TempDir()
	writeFbxhomeXML(t, filepath.Join(dir, "fbxhome.xml.3"), "12", "7")
	writeFbxhomeXML(t, filepath.Join(dir, "fbxhome.xml.0"), "9", "5") // older
	writeFbxhomeXML(t, filepath.Join(dir, "fbxhome.xml.5"), "13", "8")
	cut, err := os.ReadFile(filepath.Join(dir, "fbxhome.xml.5"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "fbxhome.xml.5"), cut[:len(cut)/2], 0o600); err != nil {
		t.Fatal(err) // the newest, cut short: fbxhome stopped while writing it
	}
	store := config.NewStore(filepath.Join(dir, "config.json"))
	paired := config.RadioNode{Addr: 9, UID: "00000000000000ee", SystemIndex: 5}
	if err := store.Update(func(c *config.Config) {
		c.Sensors = []config.SensorEntry{{ID: 14, Type: "KPD"}, {ID: 29, Type: "SRN", Radio: paired}}
		c.DeletedIDs = []int{20}
	}); err != nil {
		t.Fatal(err)
	}

	ids, err := ImportFbxhomeRadio(store, filepath.Join(dir, "fbxhome.xml.*"))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(ids, []int{14, 17}) {
		t.Errorf("imported %v, want 14 and 17", ids)
	}
	cfg := store.Get()
	want := []config.SensorEntry{
		{ID: 14, Type: "KPD", KPDCode: "0000", Radio: config.RadioNode{Addr: 2, UID: "00000000000000aa"}},
		{ID: 29, Type: "SRN", Radio: paired},
		{ID: 17, Type: "PIR", Radio: config.RadioNode{Addr: 3, UID: "00000000000000bb", SystemIndex: 1}, Battery: 255},
	}
	if !slices.Equal(cfg.Sensors, want) || cfg.RadioNextAddr != 7 {
		t.Errorf("config = %+v next %d,\nwant %+v next 7", cfg.Sensors, cfg.RadioNextAddr, want)
	}

	if ids, err := ImportFbxhomeRadio(store, filepath.Join(dir, "fbxhome.xml.*")); err != nil || ids != nil {
		t.Errorf("second import = %v %v, want nothing", ids, err)
	}
}

// TestRadioPairingLimit: after maxFailedPairings failures in a row, pairing
// is refused, so as not to wedge the MCU.
func TestRadioPairingLimit(t *testing.T) {
	c, mcu, _ := newRadio(t, srn)
	mcu.sent(t)
	for i := range maxFailedPairings {
		if _, err := c.StartPairing(context.Background(), "PIR"); err != nil {
			t.Fatalf("attempt %d refused: %v", i+1, err)
		}
		if err := c.StopPairing(context.Background(), 1); err != nil {
			t.Fatal(err)
		}
		for {
			_, done, err := c.PollPairing(context.Background(), 1)
			if done || err != nil {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
	}
	if _, err := c.StartPairing(context.Background(), "PIR"); err == nil {
		t.Fatal("pairing allowed after too many failures")
	}
}

// TestRadioBackPublishesFreshState: a door that went unreachable while open
// and comes back closed is published closed, never open again (a repeated
// open is an intrusion).
func TestRadioBackPublishesFreshState(t *testing.T) {
	c, mcu, _ := newRadio(t, dws)
	mcu.rx(state(5, 10, 0)) // opened
	if ev := nextEvent(t, c); !ev.Sensor.Open {
		t.Fatalf("event = %+v, want open", ev)
	}
	mcu.sent(t) // ack
	silent(c, 27*time.Hour)
	if ev := nextEvent(t, c); ev.Sensor.Reachable {
		t.Fatalf("event = %+v, want unreachable", ev)
	}
	mcu.rx(state(5, 11, 1)) // closed
	for range 2 {
		if ev := nextEvent(t, c); ev.Sensor.Open {
			t.Fatalf("event = %+v: stale open published", ev)
		}
	}
}

// TestRadioDoorStateOnlyOnceReported: after a start, a door has no state
// until it reports one. Its heartbeat goes out without making one up, as
// closed; its report sets it, and a reload of the config keeps it.
func TestRadioDoorStateOnlyOnceReported(t *testing.T) {
	c, mcu, _ := newRadio(t, dws)
	mcu.rx(fromSensor(5, 10, 0x82, 0x01, 150)) // heartbeat: its battery only
	if ev := nextEvent(t, c); ev.SensorID != 23 || ev.Sensor.StateKnown() {
		t.Fatalf("heartbeat event = %+v, want door 23 with no state", ev)
	}
	mcu.rx(state(5, 11, 1)) // closed
	if ev := nextEvent(t, c); !ev.Sensor.StateKnown() || ev.Sensor.Open {
		t.Fatalf("report event = %+v, want door 23 closed", ev)
	}
	c.Reload()
	if s, err := c.ReadSensor(context.Background(), 23); err != nil || !s.StateKnown() {
		t.Errorf("after a reload: %+v, %v, want the reported state kept", s, err)
	}
}

// TestBatteryPercent: the raw level as fbxhome showed it.
func TestBatteryPercent(t *testing.T) {
	for _, tc := range []struct {
		typ      string
		raw, pct int
	}{
		{"DWS", 255, 100}, {"PIR", 0, 0}, {"KPD", 128, 50},
		{"SRN", 73, 30}, {"SRN", 81, 33}, {"SRN", 1, 1}, {"SRN", 255, 100},
	} {
		if got := batteryPercent(tc.typ, tc.raw); got != tc.pct {
			t.Errorf("%s %d: %d %%, want %d %%", tc.typ, tc.raw, got, tc.pct)
		}
	}
}

// TestRadioTemperature: 55 0a <t> is the sensor's temperature, signed, kept
// for the next start.
func TestRadioTemperature(t *testing.T) {
	c, mcu, store := newRadio(t, dws, srn)
	mcu.sent(t) // get-state
	mcu.rx(fromSensor(6, 30, 0x01, 0x55, 0x0a, 25))
	if ev := nextEvent(t, c); ev.SensorID != 29 || ev.Sensor.Temperature == nil || *ev.Sensor.Temperature != 25 {
		t.Errorf("event = %+v, want the siren at 25 °C", ev)
	}
	mcu.rx(fromSensor(5, 31, 0x01, 0x55, 0x0a, 0xfd))
	if ev := nextEvent(t, c); ev.SensorID != 23 || ev.Sensor.Temperature == nil || *ev.Sensor.Temperature != -3 {
		t.Errorf("event = %+v, want the door at -3 °C", ev)
	}
	for _, se := range store.Get().Sensors {
		if se.ID == 29 && (se.Temperature == nil || *se.Temperature != 25) {
			t.Errorf("siren saved with %v, want 25", se.Temperature)
		}
	}
}

// TestRadioHealth: with a siren served, a radio silent for 30 min is not
// healthy; without one, silence tells nothing.
func TestRadioHealth(t *testing.T) {
	c, _, _ := newRadio(t, dws, srn)
	if err := c.RadioHealth(); err != nil {
		t.Errorf("just connected: %v", err)
	}
	c.mu.Lock()
	c.lastFrame = time.Now().Add(-31 * time.Minute)
	c.mu.Unlock()
	if c.RadioHealth() == nil {
		t.Error("healthy after 31 min of silence with a siren")
	}

	d, _, _ := newRadio(t, dws)
	d.mu.Lock()
	d.lastFrame = time.Now().Add(-5 * time.Hour)
	d.mu.Unlock()
	if err := d.RadioHealth(); err != nil {
		t.Errorf("no siren: %v", err)
	}
}
