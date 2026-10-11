package camera

import (
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
	"github.com/caligone/openqiara/internal/config"
	"github.com/caligone/openqiara/internal/domus"
	"github.com/caligone/openqiara/internal/domusvm"
	"github.com/caligone/openqiara/internal/radio"
)

var _ Client = (*RadioClient)(nil)

// RadioClient is the Client of charmux mode: openqiarad is the radio
// gateway itself, fbxhome does not run. It feeds the radio engine
// (internal/radio) the frames the MCU delivers and sends what the engine
// answers. The engine is not safe for concurrent use: the receive loop,
// siren commands and config reloads take turns on mu.
//
// The config is the registry of paired sensors (config.SensorEntry.Radio).
// A sensor keeps the id it had under fbxhome, its fbxhome node id: Home
// Assistant and Alarmo entities are named after it.
type RadioClient struct {
	mcu   radioMCU
	store *config.Store
	log   *slog.Logger

	// Files on the camera; tests point them elsewhere.
	KeysPath    string // vendor keys, for pairing
	ManifestDir string // update_manifest.json: sensor firmware → bytecode
	BytecodeDir string // <bytecode hash>.bin
	FbxhomeXML  string // glob of fbxhome's state files, imported once

	// OnShutter is called once the shutter moved (the video pipeline
	// follows it).
	OnShutter func(ctx context.Context, open bool)

	keys    []domus.VendorKey // set by Connect
	gateway uint32            // set by Connect
	events  chan SensorEvent
	done    chan struct{}
	closing sync.Once
	wg      sync.WaitGroup

	mu      sync.Mutex
	engine  *radio.Engine
	closed  bool
	sensors map[int]Sensor     // live state of every sensor in the config, by id
	nodes   map[int]radio.Node // what the engine serves, by id
	ids     map[uint32]int     // radio address → id
	heard   map[int]time.Time  // last frame from the sensor, or when it began to be served
	// sirenCounter is the counter of the siren's last frame, to drop its
	// answers that arrive out of order.
	sirenCounter map[int]uint32
	lastFrame    time.Time // last frame of a paired sensor
	started      time.Time // when Connect took the radio
	pairing      *pairing
	failed       int // pairings that failed since the last success
}

// radioMCU is the part of charmux.Client the radio client uses.
type radioMCU interface {
	domus.MCU
	GetInfo(ctx context.Context) (*charmux.MCUInfo, error)
	GetNet(ctx context.Context) (byte, error)
	Connect(ctx context.Context) error
	SendPKT(ctx context.Context, data []byte) error
	SendShutter(open bool) error
	Events() chan charmux.Event
	Close() error
}

// pairing is a pairing in progress: the receive loop forwards it the
// MCU's CTRL frames.
type pairing struct {
	frames chan []byte
	cancel context.CancelFunc
	done   chan struct{}
	sensor *Sensor // set before done closes
	err    error
}

// pairingWindow is how long a pairing waits for the sensor, like the old
// charmux client.
const pairingWindow = 2 * time.Minute

// maxFailedPairings caps the pairings that may fail in a row: past ~8 the
// MCU stops answering on CTRL until the camera is power-cycled (memory
// feedback_pairing_protocol, docs/re-bypass/09).
const maxFailedPairings = 3

// silenceLimit is how long a served sensor may stay silent before it shows
// unreachable. Every sensor sends a status heartbeat every ~12 h 20 (trial
// logs 2026-10-02 → 10-08), the siren also a keepalive (55 0e) every
// 10 min: the limits let one beat go missing. Being heard is the only
// sign of life: a frame the MCU could not deliver says nothing, a
// sleeping sensor misses them all the time.
func silenceLimit(m radio.Model) time.Duration {
	if m == radio.SRN {
		return 30 * time.Minute
	}
	return 26 * time.Hour
}

var errRadioStopped = errors.New("radio: gateway stopped")

// NewRadioClient returns a client that serves the sensors of store's
// config through mcu, once connected.
func NewRadioClient(mcu radioMCU, store *config.Store, log *slog.Logger) *RadioClient {
	return &RadioClient{
		mcu: mcu, store: store, log: log,
		KeysPath:     "/etc/hl/vendors.keys",
		ManifestDir:  "/etc/hl",
		BytecodeDir:  "/lib/firmwares/bytecode",
		FbxhomeXML:   fbxhomeXMLGlob,
		events:       make(chan SensorEvent, 64),
		done:         make(chan struct{}),
		heard:        make(map[int]time.Time),
		sirenCounter: make(map[int]uint32),
	}
}

// Connect takes the radio over: it fails while another process (fbxhome)
// holds the charmux ports.
func (c *RadioClient) Connect(ctx context.Context) error {
	if err := c.mcu.Connect(ctx); err != nil {
		return fmt.Errorf("radio: %w", err)
	}
	info, err := c.gatewayInfo(ctx)
	if err != nil {
		_ = c.mcu.Close()
		return err
	}
	c.gateway = uint32(info.Address)

	if ids, err := ImportFbxhomeRadio(c.store, c.FbxhomeXML); err != nil {
		c.log.Error("radio: fbxhome.xml import failed", "error", err)
	} else if len(ids) > 0 {
		c.log.Info("radio: sensors imported from fbxhome.xml", "ids", ids)
	}
	if c.keys, err = domus.LoadVendorKeys(c.KeysPath); err != nil {
		c.log.Warn("radio: no vendor keys, pairing unavailable", "error", err)
	}
	e, err := radio.New(radio.Options{Gateway: c.gateway, Bytecode: c.bytecodeSource()})
	if err != nil {
		_ = c.mcu.Close()
		return err
	}

	c.mu.Lock()
	c.engine = e
	c.reload()
	for id, s := range c.sensors {
		if _, served := c.nodes[id]; !served {
			c.log.Error("radio: sensor not served, pair it again", "id", id, "type", s.Type)
		}
	}
	c.log.Info("radio: gateway up", "addr", info.Address, "netid", info.NetworkID, "sensors", len(c.nodes))
	_ = c.send(e.Start())
	c.mu.Unlock()

	c.started = time.Now()
	c.wg.Add(1)
	go c.run()
	return nil
}

// gatewayInfo reads the gateway's radio address, retrying: right after a
// boot, charmux can take a few seconds to wire the UART up. GetNet
// follows, as with fbxhome.
func (c *RadioClient) gatewayInfo(ctx context.Context) (*charmux.MCUInfo, error) {
	var err error
	for range 3 {
		var info *charmux.MCUInfo
		if info, err = c.mcu.GetInfo(ctx); err == nil {
			if _, err := c.mcu.GetNet(ctx); err != nil {
				c.log.Warn("radio: GetNet failed", "error", err)
			}
			return info, nil
		}
	}
	return nil, fmt.Errorf("radio: MCU GetInfo: %w", err)
}

// bytecodeSource returns where the engine gets the bytecode of a sensor
// that lost it, nil without the camera's update manifest.
func (c *RadioClient) bytecodeSource() func(fwHash []byte) ([][]byte, error) {
	m, err := domusvm.LoadManifest(c.ManifestDir + "/update_manifest.json")
	if err != nil {
		c.log.Warn("radio: no update manifest, a sensor that loses its bytecode stays mute", "error", err)
		return nil
	}
	return func(fwHash []byte) ([][]byte, error) { return m.Frames(c.BytecodeDir, fwHash) }
}

var radioModels = map[string]radio.Model{"DWS": radio.DWS, "PIR": radio.PIR, "KPD": radio.KPD, "SRN": radio.SRN}

// Reload applies the config's sensors and keypad codes to the engine;
// codes go out at the keypad's next wake.
func (c *RadioClient) Reload() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reload()
}

// reload makes the engine serve the sensors of the config, the source of
// truth: pairing, deletion and keypad codes all go through it. A sensor
// with no radio address is not served; neither is a keypad whose code is
// not valid, rather than served without a code (its off button alone
// would disarm). Called with mu held.
func (c *RadioClient) reload() {
	nodes := make(map[int]radio.Node)
	sensors := make(map[int]Sensor)
	for _, se := range c.store.Get().Sensors {
		model, ok := radioModels[se.Type]
		if !ok {
			continue
		}
		s, ok := c.sensors[se.ID]
		if !ok {
			s = Sensor{ID: se.ID, Reachable: true, Battery: batteryPercent(se.Type, se.Battery), Temperature: se.Temperature}
		}
		s.Type, s.ItemID = se.Type, se.Radio.UID
		if se.Radio.Addr == 0 {
			s.Reachable = false
		} else {
			n := radio.Node{Addr: se.Radio.Addr, Model: model, SystemIndex: se.Radio.SystemIndex}
			if model == radio.KPD {
				// An empty code is not valid: the engine refuses it.
				n.PINs = []string{se.KPDCode}
			}
			nodes[se.ID] = n
		}
		sensors[se.ID] = s
	}

	for id, n := range c.nodes {
		if f, ok := nodes[id]; !ok || f.Addr != n.Addr {
			c.engine.RemoveNode(n.Addr)
		}
	}
	c.ids = make(map[uint32]int, len(nodes))
	var siren uint32
	for _, id := range slices.Sorted(maps.Keys(nodes)) {
		n := nodes[id]
		if err := c.engine.SetNode(n); err != nil {
			c.log.Error("radio: sensor not served", "id", id, "error", err)
			c.engine.RemoveNode(n.Addr)
			delete(nodes, id)
			s := sensors[id]
			s.Reachable = false
			sensors[id] = s
			continue
		}
		c.ids[n.Addr] = id
		if _, ok := c.heard[id]; !ok {
			c.heard[id] = time.Now() // silent from now on, not since the epoch
		}
		if siren == 0 && n.Model == radio.SRN {
			siren = n.Addr // the lowest id, as fbxhome takes the first HlSrn it finds
		}
	}
	c.engine.SetAlarmSiren(siren)
	for id := range c.heard {
		if _, ok := nodes[id]; !ok {
			delete(c.heard, id)
		}
	}
	c.nodes, c.sensors = nodes, sensors
}

// run delivers what the MCU sends.
func (c *RadioClient) run() {
	defer c.wg.Done()
	events := c.mcu.Events()
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-c.done:
			return
		case now := <-tick.C:
			c.mu.Lock()
			c.markSilent(now)
			c.mu.Unlock()
		case ev, ok := <-events:
			if !ok {
				return
			}
			c.mu.Lock()
			switch ev.Channel {
			case charmux.ChannelPKT:
				c.receive(ev.Data)
			case charmux.ChannelCTRL:
				c.ctrl(ev.Data)
			}
			c.mu.Unlock()
		}
	}
}

// receive hands a frame to the engine. Called with mu held.
func (c *RadioClient) receive(data []byte) {
	rx, err := charmux.DeserializeManagedFrame(data)
	if err != nil {
		c.log.Warn("radio: unreadable frame", "len", len(data), "error", err)
		return
	}
	c.trace("rx", rx.Src, *rx)
	id, back := c.ids[rx.Src], false
	if _, ok := c.ids[rx.Src]; ok && rx.Flags&(charmux.FlagZ|charmux.FlagW|charmux.FlagA) != 0 {
		// A sensor frame, not an MCU report: the sensor is there.
		s := c.sensors[id]
		s.LastSeen = time.Now().Unix()
		back, s.Reachable = !s.Reachable, true
		c.sensors[id] = s
		c.heard[id] = time.Now()
		c.lastFrame = time.Now()
	}
	res := c.engine.Receive(time.Now(), *rx)
	if c.staleSiren(id, *rx) {
		// An answer to an earlier command, overtaken by a later one: its
		// state is no longer the siren's.
		res.Events = slices.DeleteFunc(res.Events, func(e radio.Event) bool { return e.Kind == radio.SirenState })
	}
	_ = c.send(res)
	if back {
		// Published after the frame's own events: before, it would repeat
		// the state the sensor had when it went missing, and a repeated
		// open is an intrusion to the alarm.
		c.emit(c.sensors[id])
	}
}

// staleSiren tells whether a siren frame is older than the last one heard:
// the siren answers commands sent back to back out of order (hardware,
// 2026-10-09: counters 27432, 27433 then 27431). A counter far behind is
// a siren that started counting again, not an old frame. Called with mu
// held.
func (c *RadioClient) staleSiren(id int, rx charmux.ManagedFrame) bool {
	if n, ok := c.nodes[id]; !ok || n.Model != radio.SRN || rx.Flags&(charmux.FlagZ|charmux.FlagW|charmux.FlagA) == 0 {
		return false
	}
	last, seen := c.sirenCounter[id]
	if seen && last-rx.Counter > 0 && last-rx.Counter < 256 {
		return true
	}
	c.sirenCounter[id] = rx.Counter
	return false
}

// markSilent shows unreachable the sensors silent past their limit.
// Called with mu held.
func (c *RadioClient) markSilent(now time.Time) {
	for _, id := range slices.Sorted(maps.Keys(c.nodes)) {
		s := c.sensors[id]
		if !s.Reachable || now.Sub(c.heard[id]) <= silenceLimit(c.nodes[id].Model) {
			continue
		}
		s.Reachable = false
		c.sensors[id] = s
		c.log.Warn("radio: sensor silent, unreachable", "id", id, "type", s.Type, "since", c.heard[id].Format(time.RFC3339))
		c.emit(s)
	}
}

// send puts the engine's frames on the air and publishes its events. A
// frame that cannot be sent is lost for the engine too. Called with mu
// held.
func (c *RadioClient) send(res radio.Result) error {
	var errs []error
	for _, f := range res.Send {
		c.trace("tx", f.GWDst, f)
		if err := c.mcu.SendPKT(context.Background(), f.Serialize()); err != nil {
			c.log.Warn("radio: send failed", "dst", f.GWDst, "error", err)
			res.Events = append(res.Events, c.engine.Lost(f.Counter).Events...)
			errs = append(errs, err)
		}
	}
	for _, ev := range res.Events {
		c.publish(ev)
	}
	return errors.Join(errs...)
}

// trace logs a frame at debug level. Keypad payloads carry its codes: of
// a keypad, or of an address not paired, only the length is logged.
func (c *RadioClient) trace(dir string, peer uint32, f charmux.ManagedFrame) {
	if !c.log.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	payload := fmt.Sprintf("(%d bytes)", len(f.Payload))
	if id, ok := c.ids[peer]; ok && c.nodes[id].Model != radio.KPD {
		payload = hex.EncodeToString(f.Payload)
	}
	c.log.Debug("radio "+dir, "peer", peer, "cnt", f.Counter, "flags", fmt.Sprintf("%04x", f.Flags),
		"wflags", fmt.Sprintf("%02x", f.WFlags), "payload", payload)
}

// sirenStates names the states the siren reports (its bytecode, 7 states).
var sirenStates = map[int]string{
	0: "off", 1: "test", 2: "exit_delay", 3: "armed", 4: "entry_delay", 5: "alert", 6: "alert_over",
}

var keypadActions = map[radio.EventKind]string{
	radio.ArmedAway: "armed_away", radio.ArmedNight: "armed_night", radio.Disarmed: "disarmed",
}

// publish turns an engine event into the sensor state openqiarad
// publishes. Every door and motion report goes out, repeated or not: a
// lost close must not hide the next open. A keypad button is an action,
// not a state: it goes out once as KPDState, which forwardEvents turns
// into an alarm command. Called with mu held.
func (c *RadioClient) publish(ev radio.Event) {
	id, ok := c.ids[ev.Addr]
	if !ok {
		c.log.Debug("radio: frame from an address not paired", "addr", ev.Addr)
		return
	}
	s := c.sensors[id]
	before := s
	report := false
	switch ev.Kind {
	case radio.Opened, radio.Closed:
		s.Open, report = ev.Kind == radio.Opened, true
	case radio.MotionStart, radio.MotionEnd:
		s.Motion, report = ev.Kind == radio.MotionStart, true
	case radio.ArmedAway, radio.ArmedNight, radio.Disarmed:
		action := s
		action.KPDState = keypadActions[ev.Kind]
		c.emit(action)
		return
	case radio.Battery:
		if pct := batteryPercent(s.Type, ev.Value); pct != s.Battery {
			s.Battery = pct
			c.save(id, func(se *config.SensorEntry) { se.Battery = ev.Value })
		}
	case radio.Temperature:
		if s.Temperature == nil || *s.Temperature != ev.Value {
			t := ev.Value
			s.Temperature = &t
			c.save(id, func(se *config.SensorEntry) { se.Temperature = &t })
		}
	case radio.DeliveryFailed:
		// Not a sign the sensor is gone (silenceLimit).
		c.log.Warn("radio: frame not delivered", "id", id, "counter", ev.Value)
	case radio.NoBytecode:
		s.Reachable = false
		c.log.Error("radio: no bytecode for the sensor's firmware, it stays mute", "id", id)
	case radio.Tamper:
		c.log.Warn("radio: sensor tampered with", "id", id)
	case radio.Emergency:
		c.log.Warn("radio: keypad emergency button, not handled", "id", id)
	case radio.SirenState:
		c.log.Info("radio: siren state", "id", id, "state", ev.Value)
		s.SirenState = sirenStates[ev.Value]
		c.sensors[id] = s
		c.event(SensorEvent{SensorID: id, Sensor: s, SirenReport: true})
		return
	case radio.Rebooted:
		c.log.Info("radio: sensor rebooted, provisioning it", "id", id)
	case radio.Unhandled:
		c.log.Warn("radio: frame not understood", "id", id, "value", ev.Value)
	}
	if report {
		s.Reported = true
	}
	c.sensors[id] = s
	if report || s != before {
		c.emit(s)
	}
}

// batteryPercent is a sensor's raw battery level as fbxhome showed it:
// raw×98/255+2 for a siren (HlSrn FUN_000bc20c, 0 and 1 as is),
// raw×99/255+1 for the others (0xb4250); 255 is 100 %. 0, never reported,
// stays 0.
func batteryPercent(typ string, raw int) int {
	switch {
	case raw <= 0:
		return 0
	case typ == "SRN" && raw <= 1:
		return raw
	case typ == "SRN":
		return raw*98/255 + 2
	}
	return raw*99/255 + 1
}

// save keeps what a sensor reported for the next start, which would
// otherwise publish nothing until it reports again, hours later. Called
// with mu held.
func (c *RadioClient) save(id int, set func(*config.SensorEntry)) {
	err := c.store.Update(func(cfg *config.Config) {
		if i := slices.IndexFunc(cfg.Sensors, func(se config.SensorEntry) bool { return se.ID == id }); i >= 0 {
			set(&cfg.Sensors[i])
		}
	})
	if err != nil {
		c.log.Warn("radio: sensor report not saved", "id", id, "error", err)
	}
}

// emit publishes a sensor's state. Called with mu held.
func (c *RadioClient) emit(s Sensor) {
	c.event(SensorEvent{SensorID: s.ID, Sensor: s})
}

func (c *RadioClient) event(ev SensorEvent) {
	if c.closed {
		return
	}
	select {
	case c.events <- ev:
	default:
		c.log.Warn("radio: event channel full, dropping event", "id", ev.SensorID)
	}
}

// ctrl hands a CTRL frame to the pairing in progress. Called with mu
// held.
func (c *RadioClient) ctrl(data []byte) {
	if p := c.pairing; p != nil && !isClosed(p.done) {
		select {
		case p.frames <- data:
		default:
			c.log.Warn("radio: CTRL frame dropped, pairing too slow")
		}
		return
	}
	if len(data) > 0 {
		c.log.Info("radio: CTRL frame", "op", fmt.Sprintf("%02x", data[0]), "len", len(data))
	}
}

func isClosed(ch chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// StartPairing waits for a sensor of sensorType in pairing mode.
func (c *RadioClient) StartPairing(_ context.Context, sensorType string) (int, error) {
	model, ok := NodeType[sensorType]
	if !ok {
		return 0, fmt.Errorf("radio: unknown sensor type %q", sensorType)
	}
	if len(c.keys) == 0 {
		return 0, errors.New("radio: no vendor keys, pairing unavailable")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.pairing != nil && !isClosed(c.pairing.done) {
		return 0, errors.New("radio: a pairing is already running")
	}
	if c.failed >= maxFailedPairings {
		return 0, fmt.Errorf("radio: %d pairings failed in a row, restart the camera before trying again", c.failed)
	}
	addr, err := c.reserveAddr()
	if err != nil {
		return 0, err
	}
	ctx, cancel := context.WithTimeout(context.Background(), pairingWindow)
	p := &pairing{frames: make(chan []byte, 16), cancel: cancel, done: make(chan struct{})}
	c.pairing = p
	go func() {
		defer close(p.done)
		defer cancel()
		res, err := domus.Pair(ctx, c.mcu, p.frames, c.keys, addr, model, c.log)
		if err == nil {
			p.sensor, err = c.adopt(res)
		}
		c.mu.Lock()
		if err != nil {
			c.failed++
		} else {
			c.failed = 0
		}
		c.mu.Unlock()
		p.err = err
	}()
	return 1, nil
}

// reserveAddr takes the address the next pairing gives: above every
// address ever given, as the MCU keeps the sensors it knew, and never the
// gateway's own. It is saved before the MCU hears of it: a pairing that
// fails half way may have used it. Called with mu held.
func (c *RadioClient) reserveAddr() (byte, error) {
	var addr uint32
	err := c.store.Update(func(cfg *config.Config) {
		addr = max(cfg.RadioNextAddr, 2) // 0 broadcast, 1 gateway on production cameras
		for _, se := range cfg.Sensors {
			addr = max(addr, se.Radio.Addr+1)
		}
		if addr == c.gateway {
			addr++
		}
		cfg.RadioNextAddr = addr + 1
	})
	if err != nil {
		return 0, fmt.Errorf("radio: reserve an address: %w", err)
	}
	if addr > 0xff {
		return 0, errors.New("radio: no radio address left")
	}
	return byte(addr), nil
}

// adopt registers a sensor the MCU just paired. A sensor paired again
// keeps its id and its system index, so that Home Assistant and Alarmo
// keep their entities.
func (c *RadioClient) adopt(res *domus.PairingResult) (*Sensor, error) {
	typ := sensorTypeOfModel(res.Model)
	if typ == "" {
		return nil, fmt.Errorf("radio: unknown sensor model %q", res.Model)
	}
	node := config.RadioNode{Addr: uint32(res.Address), UID: hex.EncodeToString(res.DeviceUID[:])}
	var id int
	var full bool
	err := c.store.Update(func(cfg *config.Config) {
		i := slices.IndexFunc(cfg.Sensors, func(se config.SensorEntry) bool { return strings.EqualFold(se.Radio.UID, node.UID) })
		if i >= 0 {
			node.SystemIndex = cfg.Sensors[i].Radio.SystemIndex
		} else if node.SystemIndex, full = freeSystemIndex(cfg.Sensors); full {
			return
		} else {
			cfg.Sensors = append(cfg.Sensors, config.SensorEntry{ID: nextID(*cfg)})
			i = len(cfg.Sensors) - 1
		}
		for j := range cfg.Sensors {
			if j != i && cfg.Sensors[j].Radio.Addr == node.Addr {
				// The MCU gave that address away: the sensor it belonged
				// to is no longer reachable.
				cfg.Sensors[j].Radio = config.RadioNode{}
			}
		}
		cfg.Sensors[i].Type, cfg.Sensors[i].Radio = typ, node
		id = cfg.Sensors[i].ID
	})
	if err != nil {
		return nil, fmt.Errorf("radio: save the paired sensor: %w", err)
	}
	if full {
		return nil, errors.New("radio: 64 sensors paired, no system index left")
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.reload()
	c.log.Info("radio: sensor paired", "id", id, "type", typ, "addr", node.Addr)
	s := c.sensors[id]
	return &s, nil
}

// sensorTypeOfModel reads the type out of a beacon model, e.g.
// HOMELABDWS00ACFD.
func sensorTypeOfModel(model string) string {
	for typ, prefix := range NodeType {
		if strings.HasPrefix(model, prefix) {
			return typ
		}
	}
	return ""
}

// nextID is the id of a new sensor: above every id ever used, deleted
// ones included, so that no Home Assistant entity changes sensor.
func nextID(cfg config.Config) int {
	id := 0
	for _, se := range cfg.Sensors {
		id = max(id, se.ID)
	}
	for _, d := range cfg.DeletedIDs {
		id = max(id, d)
	}
	return id + 1
}

// freeSystemIndex is the lowest system index no paired sensor has, as
// fbxhome allocates them (bitmap 0..63); full when there is none.
func freeSystemIndex(sensors []config.SensorEntry) (index uint8, full bool) {
	for i := range uint8(64) {
		if !slices.ContainsFunc(sensors, func(se config.SensorEntry) bool {
			return se.Radio.Addr != 0 && se.Radio.SystemIndex == i
		}) {
			return i, false
		}
	}
	return 0, true
}

// PollPairing reports the pairing in progress: done with the sensor, or
// its error.
func (c *RadioClient) PollPairing(context.Context, int) (*Sensor, bool, error) {
	c.mu.Lock()
	p := c.pairing
	c.mu.Unlock()
	if p == nil {
		return nil, false, errors.New("radio: no pairing running")
	}
	if !isClosed(p.done) {
		return nil, false, nil
	}
	return p.sensor, p.err == nil, p.err
}

// StopPairing gives up the pairing in progress: the MCU leaves pairing
// mode.
func (c *RadioClient) StopPairing(context.Context, int) error {
	c.mu.Lock()
	p := c.pairing
	c.mu.Unlock()
	if p != nil {
		p.cancel()
	}
	return nil
}

// DeleteSensor stops serving a sensor. The MCU keeps it, having no way to
// forget one: its frames keep coming, unanswered.
func (c *RadioClient) DeleteSensor(_ context.Context, id int) error {
	err := c.store.Update(func(cfg *config.Config) {
		cfg.Sensors = slices.DeleteFunc(cfg.Sensors, func(se config.SensorEntry) bool { return se.ID == id })
	})
	if err != nil {
		return err
	}
	c.Reload()
	return nil
}

// RadioHealth tells whether the radio is alive, for the MCU watchdog: with
// a siren served, which sends a keepalive every 10 min, a frame heard in
// the last 30 min (from Connect on). Without one, sensors may stay silent
// for hours: nothing to tell. It takes the gateway's lock: a gateway stuck
// holding it blocks the call, which the watchdog takes as a failure.
func (c *RadioClient) RadioHealth() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errRadioStopped
	}
	siren := false
	for _, n := range c.nodes {
		siren = siren || n.Model == radio.SRN
	}
	since := c.lastFrame
	if since.IsZero() {
		since = c.started
	}
	if siren && time.Since(since) > 30*time.Minute {
		return fmt.Errorf("radio: no frame for %s", time.Since(since).Round(time.Minute))
	}
	return nil
}

// CachedSensors returns the sensors' live state, by id.
func (c *RadioClient) CachedSensors() []Sensor {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Sensor, 0, len(c.sensors))
	for _, id := range slices.Sorted(maps.Keys(c.sensors)) {
		out = append(out, c.sensors[id])
	}
	return out
}

// ReadSensor returns a sensor's live state. A door or motion sensor that
// has not reported since the start has none (Sensor.StateKnown).
func (c *RadioClient) ReadSensor(_ context.Context, id int) (*Sensor, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s, ok := c.sensors[id]
	if !ok {
		return nil, fmt.Errorf("radio: no sensor %d", id)
	}
	if !s.StateKnown() {
		return nil, fmt.Errorf("radio: sensor %d has not reported its state yet", id)
	}
	return &s, nil
}

// SendPKT sends raw bytes to the MCU, outside the engine: debug only.
func (c *RadioClient) SendPKT(ctx context.Context, data []byte) error {
	return c.mcu.SendPKT(ctx, data)
}

// Siren payloads, after the application class byte (HlSrn, RE 2026-10-08,
// checked on the siren's bytecode and on the hardware 2026-10-09):
//
//   - 55 04 <exit> <entry> <alert/2> <05> <64> <S> <active:u64> <delayed:u64>
//     arms it, from off only: S 02 after the exit delay, 03 at once. The
//     masks hold one bit per system index. The siren keeps the delays and
//     plays their beeps, and it hears the sensors itself when the gateway
//     is gone.
//   - 55 05 <S> sets its state: 00 off (from any state), 01 the test
//     sound <power> <quarter seconds> (from off), 04 the entry delay (from
//     armed), 05 the alert (from armed, entry delay or after an alert).
//   - 55 01 <ts:4> <0x40|index> <value> is a sensor's alarm report, which
//     the sensors send the siren when the gateway is gone: armed, it starts
//     the entry delay or the alert by itself. Relayed by the gateway, it
//     works but the siren then reports nothing, not even to 55 06: the
//     gateway sends 55 05 04 or 05 instead, as fbxhome did.
//   - 55 06 asks for its state, reported as 55 01 <ts:4> 00 <state>.
var (
	sirenOff      = []byte{0x55, 0x05, 0x00}
	sirenGetState = []byte{0x55, 0x06}
	// sirenWake is 55 0b, the siren's Sigfox credentials, here empty: found
	// by ear in April 2026 and long sent before every sound, it is needed
	// by none (hardware, 2026-10-09). Kept for the debug API only.
	sirenWake = []byte{0x55, 0x0b, 0, 0, 0, 0, 0, 0}
)

func sirenSound(power byte, d time.Duration) []byte {
	return []byte{0x55, 0x05, 0x01, power, byte(min(d/(time.Second/4), 0xff))}
}

// SirenArming is what the siren needs to keep the alarm by itself.
type SirenArming struct {
	ExitDelay  time.Duration // 0: armed at once, no exit beeps
	EntryDelay time.Duration
	Alert      time.Duration // how long it wails
	Active     []int         // sensors that set it off
	Delayed    []int         // among them, those that start the entry delay
	// Quiet zeroes the byte fbxhome always sends as 05, which the siren's
	// bytecode hands to the sound of its delays: no beeps, if that byte is
	// their volume (not tried on the hardware yet).
	Quiet bool
}

// sirenSeconds is a delay as the siren takes it: whole seconds, at most 255.
func sirenSeconds(d time.Duration, unit time.Duration) byte {
	return byte(min((d+unit-1)/unit, 0xff))
}

// ArmSiren arms the siren, which must be off: it ignores the frame
// otherwise. Its state is asked for after.
func (c *RadioClient) ArmSiren(_ context.Context, id int, a SirenArming) error {
	addr, err := c.sirenAddr(id)
	if err != nil {
		return err
	}
	target, beeps := byte(0x02), byte(0x05)
	if a.ExitDelay <= 0 {
		target = 0x03
	}
	if a.Quiet {
		beeps = 0
	}
	c.mu.Lock()
	active, delayed := c.mask(a.Active), c.mask(a.Delayed)
	c.mu.Unlock()
	f := []byte{0x55, 0x04, sirenSeconds(a.ExitDelay, time.Second), sirenSeconds(a.EntryDelay, time.Second),
		sirenSeconds(a.Alert, 2*time.Second), beeps, 0x64, target}
	f = binary.BigEndian.AppendUint64(f, active)
	f = binary.BigEndian.AppendUint64(f, delayed)
	// Its state, whether it took the arming or not.
	return errors.Join(c.command(addr, f), c.command(addr, sirenGetState))
}

// mask sets one bit per system index of the sensors served. Called with
// mu held.
func (c *RadioClient) mask(ids []int) uint64 {
	var m uint64
	for _, id := range ids {
		if n, ok := c.nodes[id]; ok && n.SystemIndex < 64 {
			m |= 1 << n.SystemIndex
		}
	}
	return m
}

// SirenEntryDelay starts the entry delay of an armed siren.
func (c *RadioClient) SirenEntryDelay(_ context.Context, id int) error {
	return c.sirenCommand(id, []byte{0x55, 0x05, 0x04})
}

// SirenAlert sets an armed siren off.
func (c *RadioClient) SirenAlert(_ context.Context, id int) error {
	return c.sirenCommand(id, []byte{0x55, 0x05, 0x05})
}

// RequestSirenState asks the siren for its state, which comes back as an
// event.
func (c *RadioClient) RequestSirenState(_ context.Context, id int) error {
	return c.sirenCommand(id, sirenGetState)
}

// TriggerSiren plays the discreet test sound fbxhome plays: power 10 for
// 10 s. Only an unarmed siren plays it.
func (c *RadioClient) TriggerSiren(_ context.Context, id int) error {
	return c.sirenCommand(id, sirenSound(10, 10*time.Second))
}

// TriggerSirenAlarm plays the test sound at full power, for duration (10 s
// if unset): the wail of an unarmed siren. It stops by itself.
func (c *RadioClient) TriggerSirenAlarm(_ context.Context, id int, duration time.Duration) error {
	if duration <= 0 {
		duration = 10 * time.Second
	}
	return c.sirenCommand(id, sirenSound(100, duration))
}

// StopSiren disarms the siren and stops whatever it plays.
func (c *RadioClient) StopSiren(_ context.Context, id int) error {
	return c.sirenCommand(id, sirenOff)
}

func (c *RadioClient) sirenCommand(id int, payload []byte) error {
	addr, err := c.sirenAddr(id)
	if err != nil {
		return err
	}
	return c.command(addr, payload)
}

// SendSirenDebug sends any payload to a paired radio address, for the
// debug API: [55 0b] → payload → [hold, 55 05 00].
func (c *RadioClient) SendSirenDebug(ctx context.Context, addr uint32, payload []byte, wake, stop bool, hold time.Duration) error {
	c.mu.Lock()
	_, ok := c.ids[addr]
	c.mu.Unlock()
	if !ok {
		return fmt.Errorf("radio: address %d not paired", addr)
	}
	return c.sequence(ctx, addr, payload, wake, stop, hold)
}

func (c *RadioClient) sirenAddr(id int) (uint32, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	n, ok := c.nodes[id]
	if !ok || n.Model != radio.SRN {
		return 0, fmt.Errorf("radio: no paired siren %d", id)
	}
	return n.Addr, nil
}

// sequence sends 55 0b, the command, and after hold 55 05 00, sent even
// when ctx ends first.
func (c *RadioClient) sequence(ctx context.Context, addr uint32, cmd []byte, wake, stop bool, hold time.Duration) error {
	if wake {
		if err := c.command(addr, sirenWake); err != nil {
			return err
		}
		if err := sleepCtx(ctx, 300*time.Millisecond); err != nil {
			return err
		}
	}
	if err := c.command(addr, cmd); err != nil || !stop {
		return err
	}
	return errors.Join(sleepCtx(ctx, hold), c.command(addr, sirenOff))
}

// command sends an application payload to a siren, which listens all the
// time.
func (c *RadioClient) command(addr uint32, payload []byte) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errRadioStopped
	}
	return c.send(c.engine.Command(addr, payload))
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	select {
	case <-time.After(d):
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// Values of hlcamd's config.sensor.night_day_mode (the app's "night
// vision" setting).
const (
	nightDayModeAuto     = 1
	nightDayModeForceDay = 2 // IR-cut fixed, IR LED off
)

// SetShutter moves the privacy shutter through the MCU, and sets hlcamd's
// IR-cut mode like fbxhome did for it (PR #41): forced day while closed,
// or the IR-cut relay clicks all night behind the shutter.
func (c *RadioClient) SetShutter(ctx context.Context, open bool) error {
	if err := c.mcu.SendShutter(open); err != nil {
		return fmt.Errorf("radio: shutter: %w", err)
	}
	mode := nightDayModeForceDay
	if open {
		mode = nightDayModeAuto
	}
	settings := fmt.Sprintf(`{"parameters":{"config.sensor.night_day_mode":{"val":%d}}}`, mode)
	if out, err := exec.CommandContext(ctx, "fbxbusctl", "set", "hlcamd", "video_settings", settings).CombinedOutput(); err != nil {
		return fmt.Errorf("radio: hlcamd night mode: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if c.OnShutter != nil {
		c.OnShutter(ctx, open)
	}
	return nil
}

// Events returns the sensor state changes and keypad actions.
func (c *RadioClient) Events() <-chan SensorEvent {
	return c.events
}

// Close stops the gateway and cancels a pairing in progress.
func (c *RadioClient) Close() error {
	var err error
	c.closing.Do(func() {
		c.mu.Lock()
		c.closed = true
		if c.pairing != nil {
			c.pairing.cancel()
		}
		c.mu.Unlock()
		close(c.done)
		c.wg.Wait()
		close(c.events)
		err = c.mcu.Close()
	})
	return err
}
