package camera

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/caligone/openqiara/internal/charmux"
	"github.com/caligone/openqiara/internal/domus"
)

// Compile-time check: CharmuxClient implements Client.
var _ Client = (*CharmuxClient)(nil)

// Known MCU node type bytes from the node table.
const (
	nodeTypeDWS byte = 0x04
	nodeTypeKPD byte = 0x06
	nodeTypePIR byte = 0x01
	nodeTypeSRN byte = 0x0E
)

// nodeEntrySize is the approximate size of one entry in the MCU node table.
const nodeEntrySize = 9

// CharmuxOption configures a CharmuxClient.
type CharmuxOption func(*CharmuxClient)

// WithCharmux sets the charmux client to use.
func WithCharmux(mux *charmux.Client) CharmuxOption {
	return func(c *CharmuxClient) { c.mux = mux }
}

// WithCharmuxLogger sets a custom logger.
func WithCharmuxLogger(l *slog.Logger) CharmuxOption {
	return func(c *CharmuxClient) { c.logger = l }
}

// WithVendorKeysPath sets the path to the vendor keys file.
func WithVendorKeysPath(path string) CharmuxOption {
	return func(c *CharmuxClient) { c.keysPath = path }
}

// WithKnownSensorTypes sets known sensor types (from persistent config) to
// resolve UNKNOWN types in the MCU node table.
func WithKnownSensorTypes(types map[int]string) CharmuxOption {
	return func(c *CharmuxClient) { c.knownTypes = types }
}

// WithDeletedIDs sets sensor IDs that have been deleted by the user.
func WithDeletedIDs(ids map[int]bool) CharmuxOption {
	return func(c *CharmuxClient) { c.deleted = ids }
}

// WithKPDCodes sets 4-digit PIN codes for KPD sensors (addr → code).
func WithKPDCodes(codes map[int]string) CharmuxOption {
	return func(c *CharmuxClient) { c.kpdCodes = codes }
}

// SetKPDCode updates the PIN code for a KPD sensor at runtime.
func (c *CharmuxClient) SetKPDCode(addr int, code string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if code == "" {
		delete(c.kpdCodes, addr)
	} else {
		c.kpdCodes[addr] = code
	}
}

// CharmuxClient implements camera.Client using direct MCU communication
// via charmux UDP sockets, bypassing the fbxhome daemon entirely.
type CharmuxClient struct {
	mux        *charmux.Client
	logger     *slog.Logger
	vendorKeys []domus.VendorKey
	keysPath   string

	events chan SensorEvent
	done   chan struct{}
	once   sync.Once
	wg     sync.WaitGroup

	mu              sync.RWMutex
	sensors         map[int]Sensor    // tracked sensors, keyed by MCU addr
	knownTypes      map[int]string    // persistent type mapping (addr → "DWS", "PIR", etc.)
	kpdCodes        map[int]string    // KPD PIN codes (addr → 4-digit code)
	manageCnt       uint32            // global managed frame counter (shared across all sensors, like fbxhome)
	deleted         map[int]bool      // sensor IDs deleted locally
	motionTimers    map[int]*time.Timer // per-PIR timers to clear Motion after N seconds
	pairing         *pairingState     // current async pairing, nil if none
	isPairing       bool              // true while pairing handshake is active
	lastReinitTime  map[int]time.Time // per-sensor cooldown: prevent reinit loop
	getNodesAt      time.Time         // last successful GetNodes — cache to avoid CTRL spam
}

// pairingState tracks an async pairing operation.
type pairingState struct {
	done   chan struct{}
	result *domus.PairingResult
	err    error
}

// NewCharmuxClient creates a new CharmuxClient with the given options.
func NewCharmuxClient(opts ...CharmuxOption) *CharmuxClient {
	c := &CharmuxClient{
		logger:     slog.Default(),
		keysPath:   "/etc/hl/vendors.keys",
		events:     make(chan SensorEvent, 64),
		done:       make(chan struct{}),
		sensors:      make(map[int]Sensor),
		knownTypes:   make(map[int]string),
		kpdCodes:     make(map[int]string),
		deleted:      make(map[int]bool),
		motionTimers:   make(map[int]*time.Timer),
		lastReinitTime: make(map[int]time.Time),
		manageCnt:    2,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Connect establishes the charmux connection and starts the event listener.
func (c *CharmuxClient) Connect(ctx context.Context) error {
	if c.mux == nil {
		return errors.New("charmux_client: no charmux.Client configured (use WithCharmux)")
	}
	if err := c.mux.Connect(ctx); err != nil {
		return fmt.Errorf("charmux_client: connect: %w", err)
	}

	// Load vendor keys for pairing
	keys, err := domus.LoadVendorKeys(c.keysPath)
	if err != nil {
		c.logger.Warn("vendor keys not loaded (pairing will be unavailable)", "error", err)
	} else {
		c.vendorKeys = keys
		c.logger.Info("vendor keys loaded", "count", len(keys))
	}

	// MCU init sequence — match fbxhome exactly:
	// 1. Send 0x02 on CTRL (GetInfo)
	// 2. Send 0x05 on CTRL (GetNet / watchdog init)
	// NOTE: Do NOT send vendor key (0x01 + 32 bytes) — fbxhome doesn't do this
	// and it may put the MCU in a state that prevents the KPD from staying active.
	//
	// Retry up to 3 times: at boot the charmux service often takes a few
	// seconds to wire up the UART, and the first GetInfo can timeout while
	// the MCU is still warming up. Without retry the daemon ends up with
	// no MCU state info, which propagates as `MCU GetInfo failed` warnings
	// and a stale view of the radio bus.
	for attempt := 1; attempt <= 3; attempt++ {
		info, err := c.mux.GetInfo(ctx)
		if err == nil {
			c.logger.Info("MCU state", "netid", info.NetworkID, "addr", info.Address,
				"flags", fmt.Sprintf("%02x%02x%02x", info.Flags[0], info.Flags[1], info.Flags[2]),
				"state", info.State)
			break
		}
		if attempt == 3 {
			c.logger.Warn("MCU GetInfo failed after retries", "error", err)
			break
		}
		c.logger.Info("MCU GetInfo timed out, retrying", "attempt", attempt)
	}
	for attempt := 1; attempt <= 3; attempt++ {
		net, err := c.mux.GetNet(ctx)
		if err == nil {
			c.logger.Info("MCU GetNet", "response", fmt.Sprintf("0x%02x", net))
			break
		}
		if attempt == 3 {
			c.logger.Warn("MCU GetNet failed after retries", "error", err)
			break
		}
		c.logger.Info("MCU GetNet timed out, retrying", "attempt", attempt)
	}

	c.wg.Add(1)
	go c.eventLoop()

	return nil
}

// Sensors returns all known sensors, combining the MCU node table
// with state accumulated from PKT events.
// CachedSensors returns the in-memory sensor map without any MCU I/O.
// This always reflects the latest live state (open, motion, last_seen)
// from PKT events.
func (c *CharmuxClient) CachedSensors() []Sensor {
	c.mu.RLock()
	defer c.mu.RUnlock()
	result := make([]Sensor, 0, len(c.sensors))
	for _, s := range c.sensors {
		result = append(result, s)
	}
	return result
}

func (c *CharmuxClient) Sensors(ctx context.Context) ([]Sensor, error) {
	if c.mux == nil {
		return nil, errors.New("charmux_client: not connected")
	}

	// During pairing, CTRL is busy — return cached sensors
	c.mu.RLock()
	if c.isPairing {
		result := make([]Sensor, 0, len(c.sensors))
		for _, s := range c.sensors {
			result = append(result, s)
		}
		c.mu.RUnlock()
		return result, nil
	}
	// Throttle GetNodes — the MCU CTRL channel is also used by reinit
	// dialogs and pairing handshakes, and a CTRL call mid-flight can
	// break them. The UI polls /api/sensors every few seconds, so without
	// a cache we'd send dozens of GetNodes per minute. 5s is short enough
	// that a freshly paired sensor still appears quickly.
	if !c.getNodesAt.IsZero() && time.Since(c.getNodesAt) < 5*time.Second {
		result := make([]Sensor, 0, len(c.sensors))
		for addr, s := range c.sensors {
			if c.deleted[addr] {
				continue
			}
			result = append(result, s)
		}
		c.mu.RUnlock()
		return result, nil
	}
	c.mu.RUnlock()

	raw, err := c.mux.GetNodes(ctx)
	if err != nil {
		return nil, fmt.Errorf("charmux_client: get nodes: %w", err)
	}

	discovered := parseNodeTable(raw)

	c.mu.Lock()
	c.getNodesAt = time.Now()
	// Merge discovered nodes into our sensor map, skipping deleted ones.
	for addr, s := range discovered {
		if c.deleted[addr] {
			delete(c.sensors, addr)
			continue
		}
		if existing, ok := c.sensors[addr]; ok {
			// Keep runtime state, update static fields.
			existing.Type = s.Type
			existing.Reachable = true
			existing.LastSeen = time.Now().Unix()
			c.sensors[addr] = existing
		} else {
			s.Reachable = true
			s.LastSeen = time.Now().Unix()
			c.sensors[addr] = s
		}
	}

	// Enrich UNKNOWN types from persistent config
	if c.knownTypes != nil {
		for addr, s := range c.sensors {
			if s.Type == "UNKNOWN" {
				if t, ok := c.knownTypes[addr]; ok {
					s.Type = t
					c.sensors[addr] = s
				}
			}
		}
	}

	// Final filter: exclude any sensor marked as deleted (belt-and-suspenders).
	result := make([]Sensor, 0, len(c.sensors))
	for addr, s := range c.sensors {
		if c.deleted[addr] {
			continue
		}
		result = append(result, s)
	}
	c.mu.Unlock()

	return result, nil
}

// ReadSensor returns the last known state for a sensor from the internal map.
// The endpoints parameter is accepted for interface compatibility but ignored —
// charmux state is updated via PKT events, not on-demand reads.
func (c *CharmuxClient) ReadSensor(_ context.Context, nodeID int, _ []string) (*Sensor, error) {
	c.mu.RLock()
	s, ok := c.sensors[nodeID]
	c.mu.RUnlock()

	if !ok {
		return nil, fmt.Errorf("charmux_client: sensor %d not found", nodeID)
	}
	return &s, nil
}

// StartPairing launches the full DomusRF pairing handshake asynchronously.
// The sensor must be put in pairing mode within 90s.
func (c *CharmuxClient) StartPairing(ctx context.Context, sensorType string, _ string) (int, error) {
	if c.mux == nil {
		return 0, errors.New("charmux_client: not connected")
	}
	if len(c.vendorKeys) == 0 {
		return 0, errors.New("charmux_client: no vendor keys loaded")
	}

	_, ok := NodeType[sensorType]
	if !ok {
		return 0, fmt.Errorf("charmux_client: unknown sensor type: %q", sensorType)
	}

	c.mu.Lock()
	state := &pairingState{done: make(chan struct{})}
	c.pairing = state
	c.isPairing = true
	c.mu.Unlock()

	go func() {
		defer close(state.done)
		defer func() {
			c.mu.Lock()
			c.isPairing = false
			c.mu.Unlock()
		}()
		pairCtx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
		defer cancel()
		nextAddr := c.findNextFreeAddress()
		result, err := domus.PairSensor(pairCtx, c.mux, c.vendorKeys, nextAddr, c.logger)
		state.result = result
		state.err = err

		if result != nil {
			addr := int(result.Address)
			c.mu.Lock()
			c.sensors[addr] = Sensor{
				ID:        addr,
				Type:      sensorType,
				Reachable: true,
				LastSeen:  time.Now().Unix(),
			}
			// Persist the type in knownTypes so Sensors() doesn't relabel it
			// as UNKNOWN on the next call.
			if c.knownTypes == nil {
				c.knownTypes = make(map[int]string)
			}
			c.knownTypes[addr] = sensorType
			// Also remove from deleted list if the address was previously deleted.
			delete(c.deleted, addr)
			// Block reinit for 120s after pairing — the pairing flow already
			// sent bytecode+config+watchdog, a second reinit would desync
			// the MCU counters and cause f1 ff infinite loop.
			c.lastReinitTime[addr] = time.Now()
			c.mu.Unlock()
		}
	}()

	c.logger.Info("pairing started", "type", sensorType)
	return 1, nil
}

// PollPairing checks if the async pairing handshake has completed.
func (c *CharmuxClient) PollPairing(_ context.Context, _ int) (*Sensor, bool, error) {
	c.mu.RLock()
	state := c.pairing
	c.mu.RUnlock()

	if state == nil {
		return nil, false, errors.New("charmux_client: no pairing in progress")
	}

	select {
	case <-state.done:
		if state.err != nil {
			return nil, false, state.err
		}
		if state.result == nil {
			return nil, false, errors.New("charmux_client: pairing completed but no result")
		}
		addr := int(state.result.Address)
		c.mu.RLock()
		s, ok := c.sensors[addr]
		c.mu.RUnlock()
		if !ok {
			s = Sensor{ID: addr, Type: "UNKNOWN", Reachable: true}
		}
		return &s, true, nil
	default:
		return nil, false, nil
	}
}

// StopPairing sends a stop command. In charmux mode, there is no explicit
// stop opcode — this is a no-op that logs the action.
func (c *CharmuxClient) StopPairing(_ context.Context, session int) error {
	c.logger.Info("pairing stopped (no-op in charmux mode)", "session", session)
	return nil
}

// DeleteSensor removes a sensor from the internal map.
// TODO: needs MCU opcode research to actually unpair from the radio network.
func (c *CharmuxClient) DeleteSensor(_ context.Context, nodeID int) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	delete(c.sensors, nodeID)
	// Mark as deleted so future PKT events and GetNodes results are filtered.
	if c.deleted == nil {
		c.deleted = make(map[int]bool)
	}
	c.deleted[nodeID] = true
	c.logger.Warn("sensor removed from local map (MCU unpair not implemented)", "node_id", nodeID)
	return nil
}

// OpenStream is not supported in charmux mode — the video stream requires
// the fbxhome HTTP API.
func (c *CharmuxClient) OpenStream(_ context.Context) (StreamInfo, error) {
	return StreamInfo{}, errors.New("charmux_client: OpenStream not supported in charmux mode")
}

// EndpointsRead is not supported in charmux mode.
func (c *CharmuxClient) EndpointsRead(_ context.Context, _ int, _ []string) ([]EndpointValue, error) {
	return nil, errors.New("charmux_client: EndpointsRead not supported in charmux mode")
}

// EndpointsWrite is not supported in charmux mode.
func (c *CharmuxClient) EndpointsWrite(_ context.Context, _ int, _ []EndpointWriteEntry) error {
	return errors.New("charmux_client: EndpointsWrite not supported in charmux mode")
}

// SendPKT sends a raw packet on the PKT channel.
func (c *CharmuxClient) SendPKT(ctx context.Context, data []byte) error {
	return c.mux.SendPKT(ctx, data)
}

// nextManagedCnt returns and increments the global managed frame counter.
// fbxhome uses a single counter shared across all sensors — the KPD expects
// to see gaps in its counter (when fbxhome talks to other sensors).
func (c *CharmuxClient) nextManagedCnt() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	cnt := c.manageCnt
	c.manageCnt++
	return cnt
}

// setManageCnt sets the global counter (used post-reinit).
func (c *CharmuxClient) setManageCnt(cnt uint32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.manageCnt = cnt
}

// motionResetDelay is how long Motion stays true after the last PIR event
// before being automatically reset to false.
const motionResetDelay = 10 * time.Second

// scheduleMotionReset (re)starts a timer that clears the Motion flag after
// motionResetDelay. If a new motion event arrives before the timer fires,
// the previous timer is cancelled and a fresh one starts.
func (c *CharmuxClient) scheduleMotionReset(addr int) {
	c.mu.Lock()
	if t, ok := c.motionTimers[addr]; ok && t != nil {
		t.Stop()
	}
	c.motionTimers[addr] = time.AfterFunc(motionResetDelay, func() {
		c.mu.Lock()
		sensor, ok := c.sensors[addr]
		if !ok || !sensor.Motion {
			delete(c.motionTimers, addr)
			c.mu.Unlock()
			return
		}
		sensor.Motion = false
		c.sensors[addr] = sensor
		delete(c.motionTimers, addr)
		c.mu.Unlock()

		select {
		case c.events <- SensorEvent{SensorID: addr, Sensor: sensor}:
		default:
		}
	})
	c.mu.Unlock()
}

// findNextFreeAddress finds the lowest sensor address >= 2 that is neither
// currently in use, nor in the deleted list, nor in knownTypes. Addresses 0
// and 1 are reserved for broadcast and gateway respectively.
func (c *CharmuxClient) findNextFreeAddress() byte {
	c.mu.RLock()
	defer c.mu.RUnlock()
	for addr := 2; addr < 128; addr++ {
		if _, used := c.sensors[addr]; used {
			continue
		}
		if c.deleted[addr] {
			continue
		}
		if t := c.knownTypes[addr]; t != "" && t != "UNKNOWN" {
			continue
		}
		return byte(addr)
	}
	return 2
}

// SendConfig sends a config payload to a sensor via a managed frame.
func (c *CharmuxClient) SendConfig(ctx context.Context, addr int, payload []byte) error {
	cnt := c.nextManagedCnt()
	frame := charmux.NewConfigFrame(uint32(addr), cnt, payload)
	return c.mux.SendManaged(ctx, frame)
}

// TriggerSiren makes the siren emit the test beep (tonalité sirène à
// faible puissance), comme fbxhome le fait sur son endpoint "test".
// Format : 55 05 01 0a 28 (power=10, duration=10).
// Utilisé par le bouton "Test sirène" du web UI.
func (c *CharmuxClient) TriggerSiren(ctx context.Context, sensorID int) error {
	return c.SendSirenDebug(ctx, sensorID,
		[]byte{0x01, 0x55, 0x05, 0x01, 0x0a, 0x28}, true, true, 3000)
}

// TriggerSirenAlarm fires the full-power intrusion wail for `duration`.
func (c *CharmuxClient) TriggerSirenAlarm(ctx context.Context, sensorID int, duration time.Duration) error {
	if duration <= 0 {
		duration = 3 * time.Second
	}
	return c.SendSirenAlarm(ctx, sensorID, duration)
}

// StopSiren sends the "stop" frame (55 05 00 84). Used to interrupt a
// running wail or beep.
func (c *CharmuxClient) StopSiren(ctx context.Context, sensorID int) error {
	return c.SendSirenDebug(ctx, sensorID,
		[]byte{0x01, 0x55, 0x05, 0x00, 0x84}, false, false, 0)
}

// SendSirenBeep demande à la SRN à l'adresse addr d'émettre le bip
// sonore de pré-armement. C'est utilisé comme proxy pour le "test
// sirène" côté UI faute d'un son de test neutre : le bytecode SRN ne
// contient que 2 sons mappés (pré-arm subtype 0x02 et désarm 0x03),
// tous les autres subtypes 0x00-0x0F sont silencieux (testé 2026-04-09).
//
// Format validé audible le 2026-04-09. Trois trames dans l'ordre :
//
//  1. handshake  01 55 0b 00*6     → la SRN répond 01 55 0d 00 (ACK).
//     C'est le maillon découvert qui débloque tout : sans lui, toutes
//     les commandes 55 XX suivantes sont ignorées silencieusement par
//     le µC de la sirène.
//  2. beep       01 55 04 1e 1e 96 05 64 02 <7 zéros> 03 <7 zéros> 03
//     → la SRN répond 01 55 01 <handle:4> 00 02 (exécuté). Le µC joue
//     un son one-shot indexé par l'octet subtype=0x02 dans sa table
//     interne (pré-armement).
//  3. stop       01 55 05 00 84    → la SRN répond 01 55 01 <handle> 00 00.
//
// Envelope managed-frame identique pour les 3 trames :
// flags=0x0D43 (Z+W+E, no A), wflags=addr, gwdst=addr, gwsrc=1.
// Extrait de la pcap historique /data/fbxhome_full.pcap 2026-04-06.
//
// Mapping subtypes (octet 8 du payload beep) validé par tests audibles :
//   0x02 → bip pré-armement (utilisé ici)
//   0x03 → bip désarmement
//   0x00/0x01/0x04/0x05 → acceptés mais silencieux
//
// Les paramètres durées (1e 1e), fréquence (96 05) et volume (64)
// n'ont aucun effet audible : le son est hardcodé côté bytecode SRN.
// Seul le subtype compte.
func (c *CharmuxClient) SendSirenBeep(ctx context.Context, addr int) error {
	return c.sendSirenBeepSubtype(ctx, addr, 0x02)
}

// sendSirenBeepSubtype exécute le cycle handshake → beep → stop avec
// un subtype paramétrable (utile pour les tests de protocole).
func (c *CharmuxClient) sendSirenBeepSubtype(ctx context.Context, addr int, subtype byte) error {
	send := func(payload []byte, tag string) error {
		cnt := c.nextManagedCnt()
		frame := charmux.ManagedFrame{
			GWDst:   uint32(addr),
			GWSrc:   1,
			RFByte:  0,
			Counter: cnt,
			Src:     1,
			Flags:   0x0D43,
			AckDst:  uint32(addr),
			AckCnt:  0,
			WFlags:  byte(addr),
			Payload: payload,
		}
		c.logger.Info("siren TX",
			"tag", tag, "addr", addr, "cnt", cnt,
			"payload", fmt.Sprintf("%x", payload))
		return c.mux.SendPKT(ctx, frame.Serialize())
	}

	wait := func(d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// 1. handshake 55 0b (débloque l'acceptation des commandes)
	if err := send([]byte{0x01, 0x55, 0x0b, 0, 0, 0, 0, 0, 0}, "handshake"); err != nil {
		return fmt.Errorf("siren handshake: %w", err)
	}
	if err := wait(300 * time.Millisecond); err != nil {
		return err
	}

	// 2. beep avec subtype
	beep := []byte{
		0x01, 0x55, 0x04, 0x1e, 0x1e, 0x96, 0x05, 0x64, subtype,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03,
		0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0x03,
	}
	if err := send(beep, "beep"); err != nil {
		return fmt.Errorf("siren beep: %w", err)
	}
	if err := wait(3400 * time.Millisecond); err != nil {
		return err
	}

	// 3. stop
	if err := send([]byte{0x01, 0x55, 0x05, 0x00, 0x84}, "stop"); err != nil {
		return fmt.Errorf("siren stop: %w", err)
	}
	return nil
}

// SendSirenDebug est un helper d'exploration protocole SRN. Il envoie
// un payload arbitraire vers une SRN avec l'envelope managed-frame
// validée (flags=0x0D43, wflags=addr) — utile depuis l'endpoint
// /api/debug/siren/sequence pour tester de nouveaux opcodes sans
// rebuild. Optionnellement précédé du handshake 55 0b.
//
// La séquence complète est : [handshake?] → payload → [stop?].
//
// Retourne sans lire les réponses : observation via logs ingress PKT.
func (c *CharmuxClient) SendSirenDebug(ctx context.Context, addr int, payload []byte, withHandshake, withStop bool, holdMs int) error {
	send := func(p []byte, tag string) error {
		cnt := c.nextManagedCnt()
		frame := charmux.ManagedFrame{
			GWDst: uint32(addr), GWSrc: 1, RFByte: 0, Counter: cnt, Src: 1,
			Flags: 0x0D43, AckDst: uint32(addr), AckCnt: 0, WFlags: byte(addr),
			Payload: p,
		}
		c.logger.Info("siren debug TX",
			"tag", tag, "addr", addr, "cnt", cnt,
			"payload", fmt.Sprintf("%x", p))
		return c.mux.SendPKT(ctx, frame.Serialize())
	}
	wait := func(d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	if withHandshake {
		if err := send([]byte{0x01, 0x55, 0x0b, 0, 0, 0, 0, 0, 0}, "handshake"); err != nil {
			return fmt.Errorf("handshake: %w", err)
		}
		if err := wait(300 * time.Millisecond); err != nil {
			return err
		}
	}
	if err := send(payload, "debug"); err != nil {
		return fmt.Errorf("debug payload: %w", err)
	}
	if holdMs > 0 {
		if err := wait(time.Duration(holdMs) * time.Millisecond); err != nil {
			return err
		}
	}
	if withStop {
		if err := send([]byte{0x01, 0x55, 0x05, 0x00, 0x84}, "stop"); err != nil {
			return fmt.Errorf("stop: %w", err)
		}
	}
	return nil
}

// SendSirenAlarm déclenche un burst d'alarme intrusion de 3 secondes
// sur la SRN à addr, puis stoppe automatiquement.
//
// Format découvert 2026-04-10 par RE de HlSrn::on_write @ 0xab734
// dans fbxhome et validé audiblement à pleine puissance :
//
//  1. handshake  01 55 0b 00*6        (obligatoire)
//  2. wait       300 ms
//  3. wail       01 55 05 01 64 3c    → sirène pleine puissance
//  4. wait       3000 ms
//  5. stop       01 55 05 00 84       → coupe
//
// Format wire `55 05 <subtype> <power> <duration_shifted>` :
//   - subtype toujours 0x01 (hardcodé dans fbxhome @ 0xab7e0)
//   - power : 0x0a (10) = test discret, 0x64 (100) = pleine puissance
//   - duration_shifted : champ duration << 2 (10 → 0x28, 60 → 0xF0)
//     Ici 0x3c = duration_field=60, mais le calcul exact dépend du
//     firmware SRN ; 0x3c est la valeur qui produit ~3s de wail.
//
// ⚠️ TRÈS FORT (~90+ dB). Sécuriser la sirène avant tout appel.
//
// wailDuration controls how long the wail plays before being stopped. Pass 0
// to use the legacy 3s default.
func (c *CharmuxClient) SendSirenAlarm(ctx context.Context, addr int, wailDuration time.Duration) error {
	if wailDuration <= 0 {
		wailDuration = 3 * time.Second
	}
	send := func(payload []byte, tag string) error {
		cnt := c.nextManagedCnt()
		frame := charmux.ManagedFrame{
			GWDst:   uint32(addr),
			GWSrc:   1,
			RFByte:  0,
			Counter: cnt,
			Src:     1,
			Flags:   0x0D43,
			AckDst:  uint32(addr),
			AckCnt:  0,
			WFlags:  byte(addr),
			Payload: payload,
		}
		c.logger.Info("siren alarm TX",
			"tag", tag, "addr", addr, "cnt", cnt,
			"payload", fmt.Sprintf("%x", payload))
		return c.mux.SendPKT(ctx, frame.Serialize())
	}

	wait := func(d time.Duration) error {
		select {
		case <-time.After(d):
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}

	// handshake
	if err := send([]byte{0x01, 0x55, 0x0b, 0, 0, 0, 0, 0, 0}, "handshake"); err != nil {
		return fmt.Errorf("siren alarm handshake: %w", err)
	}
	if err := wait(300 * time.Millisecond); err != nil {
		return err
	}
	// wail : power=100 (0x64), duration=60 (0x3c)
	if err := send([]byte{0x01, 0x55, 0x05, 0x01, 0x64, 0x3c}, "wail"); err != nil {
		return fmt.Errorf("siren alarm wail: %w", err)
	}
	if err := wait(wailDuration); err != nil {
		_ = send([]byte{0x01, 0x55, 0x05, 0x00, 0x84}, "alarm-stop-cancel")
		return err
	}
	if err := send([]byte{0x01, 0x55, 0x05, 0x00, 0x84}, "alarm-stop"); err != nil {
		return fmt.Errorf("siren alarm stop: %w", err)
	}
	return nil
}

// SendSirenTest is kept as an alias for SendSirenBeep so callers that
// referenced the old name still compile.
func (c *CharmuxClient) SendSirenTest(ctx context.Context, addr int) error {
	return c.SendSirenBeep(ctx, addr)
}

// SetShutter opens or closes the camera shutter via the charmux Shutter channel.
func (c *CharmuxClient) SetShutter(_ context.Context, open bool) error {
	return c.mux.SendShutter(open)
}

// Events returns the sensor event channel.
func (c *CharmuxClient) Events() <-chan SensorEvent {
	return c.events
}

// Close stops the event listener and closes the charmux client.
func (c *CharmuxClient) Close() error {
	var err error
	c.once.Do(func() {
		close(c.done)
		c.wg.Wait()
		close(c.events)
		if c.mux != nil {
			err = c.mux.Close()
		}
	})
	return err
}

// eventLoop reads raw PKT events from charmux and translates them
// into typed SensorEvents.
//
// While isPairing is true, the loop yields CPU and does NOT read from the
// events channel — PairSensor needs exclusive access to it during the CTRL
// handshake and subsequent PKT config dialog.
func (c *CharmuxClient) eventLoop() {
	defer c.wg.Done()

	events := c.mux.Events()
	for {
		// If a pairing handshake is running, wait for it to finish before
		// consuming events. Otherwise we race with PairSensor for channel reads.
		c.mu.RLock()
		paired := c.isPairing
		c.mu.RUnlock()
		if paired {
			select {
			case <-c.done:
				return
			case <-time.After(100 * time.Millisecond):
				continue
			}
		}

		select {
		case <-c.done:
			return
		case evt, ok := <-events:
			if !ok {
				return
			}
			// Check for reinit heartbeat BEFORE handlePKTEvent.
			// If detected, run reinit synchronously — this blocks eventLoop
			// so sendConfig becomes the sole reader of the events channel.
			if addr, ok := c.detectReinitHeartbeat(evt); ok {
				c.reinitSensor(addr, evt, events)
				continue
			}
			c.handlePKTEvent(evt)
		}
	}
}

// handlePKTEvent parses a raw PKT event and updates sensor state.
//
// PKT payload format (after the 0x55 marker):
//
//	DWS: payload ending 00 01 = open, 40 00 = close
//	KPD: 55 09 = heartbeat (wake), 55 01 <ts:4> 04 01 = arm away,
//	     55 01 <ts:4> 04 02 = arm night, 55 01 <ts:4> 84 00 <BCD:4> 00 00 = disarm with code
//
// detectReinitHeartbeat checks if a PKT event is an f1 reinit heartbeat
// indicating the sensor just powered on and needs bytecode+config reinit.
// Returns the sensor address and true if reinit should be triggered.
func (c *CharmuxClient) detectReinitHeartbeat(evt charmux.Event) (int, bool) {
	data := evt.Data
	if len(data) < 4 || data[0] != 0x01 {
		return 0, false
	}
	addr := int(data[1])

	// Classic f1 reinit heartbeat (11-14 bytes). Only `f1 ff XX` is a
	// "need everything" post-battery beacon that warrants a reinit. Other
	// f1 variants — notably `f1 00 XX` (normal heartbeat) and `f1 db XX`
	// (partial state) — are routine traffic that must be ACKed only.
	// Triggering reinit on every f1 byte sends the sensor back into the
	// post-battery loop, which after a few cycles exhausts its NVM retry
	// budget and silences it permanently.
	if len(data) >= 11 && len(data) <= 14 {
		for i := 8; i+1 < len(data) && i < 12; i++ {
			if data[i] == 0xf1 && data[i+1] == 0xff {
				c.mu.RLock()
				knownType := c.knownTypes[addr]
				// Long cooldown — empirically, sensors stuck in the f1 ff
				// post-battery loop emit f1 ff every ~1.5s for hours and
				// re-running reinit on each one only burns CPU + risks
				// further NVM corruption. 5 minutes is enough that a sensor
				// that genuinely recovers can be picked up on the next
				// retry, while wasting nothing on permanently-stuck ones.
				cooldownDuration := 5 * time.Minute
				cooldown := time.Since(c.lastReinitTime[addr]) < cooldownDuration
				c.mu.RUnlock()
				if cooldown {
					return 0, false
				}
				if knownType != "" && knownType != "UNKNOWN" {
					c.logger.Info("reinit heartbeat detected", "addr", addr, "type", knownType)
					return addr, true
				}
			}
		}
	}

	// SRN fingerprint broadcast (28-35 bytes): the SRN broadcasts its
	// fingerprint repeatedly when it needs re-pairing. Treat as reinit
	// trigger so we push bytecode+config and stop the broadcast.
	// Only match for SRN sensors — DWS/PIR FNV heartbeats also contain
	// 0x81 0xd1 at similar offsets and must not trigger a reinit loop.
	if len(data) >= 28 && len(data) <= 35 {
		c.mu.RLock()
		knownType := c.knownTypes[addr]
		cooldown := time.Since(c.lastReinitTime[addr]) < 120*time.Second
		c.mu.RUnlock()
		if knownType == "SRN" && !cooldown {
			for i := 6; i < 12 && i < len(data); i++ {
				if data[i] == 0x81 && (i+1 < len(data)) && (data[i+1] == 0xd1 || data[i+1] == 0xc1) {
					c.logger.Info("reinit heartbeat detected (broadcast)", "addr", addr, "type", knownType, "len", len(data))
					return addr, true
				}
			}
		}
	}

	return 0, false
}

// handlePKTEvent parses an incoming PKT frame and:
//  1. Silently drops FNV verification frames (wflags=0x81) — seeing these means the
//     KPD has entered failure mode; cf. docs/kpd.md for the root cause.
//  2. Sends the correct ACK based on the payload type:
//     - 5501 (KPD button/code) → pure ACK (flags 0x0544, no payload)
//     - 5509 (KPD heartbeat)  → NO standard ACK; sendKPDPostResponse sends kpd-post instead
//     - anything else         → standard ACK (flags 0x0547, wflags=0xCC, payload=0x78)
//  3. Emits a SensorEvent if the sensor state changed.
func (c *CharmuxClient) handlePKTEvent(evt charmux.Event) {
	data := evt.Data
	// Throttle logging: skip FNV heartbeats (28-35 byte frames with
	// 0x81 flag byte) which flood at 8/sec and saturate CPU + disk.
	isFNV := false
	if len(data) >= 28 && len(data) <= 35 {
		for i := 6; i < 12 && i < len(data); i++ {
			if data[i] == 0x81 {
				isFNV = true
				break
			}
		}
	}
	if !isFNV {
		c.logger.Info("PKT raw", "len", len(data), "hex", fmt.Sprintf("%x", data))
	}

	if len(data) < 4 || data[0] != 0x01 {
		return
	}

	addr := int(data[1])

	// Skip events from sensors the user has deleted. We still ACK via ackPKTEvent
	// below to avoid the sensor spamming retries, but we don't emit SensorEvents
	// or persist them.
	if c.deleted[addr] {
		c.mu.Lock()
		delete(c.sensors, addr)
		c.mu.Unlock()
		return
	}

	// Detect FNV verification requests (33-byte frames with WFlags=0x81).
	// Ignore them silently ONLY for KPDs — see docs/kpd.md for the root
	// cause. For other sensor types (notably SRN), wflags=0x81 is a
	// legitimate "announce with fw_fnv" frame that MUST be ACKed,
	// otherwise the sensor stays in a stuck state waiting for an ack.
	//
	// TODO(battery/temperature): In charmux mode we don't have a way to read
	// sensor battery/temperature. fbxhome used HTTP endpoints_read to query
	// these values directly from the MCU on demand. The equivalent managed
	// frame format is unknown — needs more RE. For now Battery stays at 0
	// and the web UI hides the field when unset.
	if parsed, err := charmux.DeserializeManagedFrame(data); err == nil {
		if parsed.GWSrc != 1 && len(parsed.Payload) > 10 && parsed.Flags&charmux.FlagA != 0 && parsed.WFlags == 0x81 {
			c.mu.RLock()
			knownType := c.knownTypes[addr]
			c.mu.RUnlock()
			if knownType == "KPD" {
				return
			}
			// Non-KPD: fall through to the ACK path so the sensor knows
			// its announce was received.
		}
	}

	// Find 0x55 marker in the payload area (typically at offset 9 or 10).
	payloadStart := -1
	for i := 8; i < len(data)-1; i++ {
		if data[i] == 0x55 {
			payloadStart = i
			break
		}
	}

	// Determine ACK type based on payload (to match fbxhome behavior).
	pureAck := false
	skipAck := false
	if payloadStart >= 0 && len(data) > payloadStart+1 && data[payloadStart] == 0x55 {
		switch data[payloadStart+1] {
		case 0x01:
			// 5501 = KPD button/code event → pure ACK (flags 0x0544, no payload)
			pureAck = true
		case 0x09:
			// 5509 = KPD heartbeat → skip standard ACK, sendKPDPostResponse will respond
			skipAck = true
		}
	}

	// Note: a previous attempt sent a bytecode chunk in reply to status
	// heartbeats with bit 0x40 (need_bytecode) set. Empirically (2026-05-13)
	// this did NOT calm sensors — the KPD continued spamming at 1.5s
	// intervals after thousands of chunks delivered, suggesting the sensor
	// rejects the chunks (probably wrong wflags or lost crypto state). The
	// extra TX from us only added noise. Reverted to plain cc78 ack.
	if !skipAck {
		c.ackPKTEvent(data, pureAck)
	}

	if payloadStart < 0 {
		return
	}

	payload := data[payloadStart:]

	c.mu.RLock()
	sensor, known := c.sensors[addr]
	c.mu.RUnlock()

	if !known {
		t := "DWS"
		if ktype, ok := c.knownTypes[addr]; ok && ktype != "" {
			t = ktype
		}
		sensor = Sensor{
			ID:        addr,
			Type:      t,
			Reachable: true,
		}
	}
	sensor.LastSeen = time.Now().Unix()

	changed := false

	// Dispatch based on the known sensor type first — the payload format
	// varies by sensor type and a 5501 frame from a PIR is NOT the same as
	// from a KPD (even though both use the same prefix).
	c.mu.RLock()
	knownType := c.knownTypes[addr]
	c.mu.RUnlock()

	// Fall back to format-based detection when type is unknown yet.
	effectiveType := sensor.Type
	if effectiveType == "" || effectiveType == "UNKNOWN" {
		effectiveType = knownType
	}

	switch effectiveType {
	case "DWS":
		sensor.Type = "DWS"
		if isDWSEvent(payload) {
			open := isDWSOpen(payload)
			if sensor.Open != open || !known {
				sensor.Open = open
				changed = true
			}
		}

	case "PIR":
		sensor.Type = "PIR"
		// PIR 5501 = motion event. The PIR never sends a "motion cleared"
		// event — we reset Motion to false after 30s of inactivity via
		// scheduleMotionReset below.
		if len(payload) >= 2 && payload[0] == 0x55 && payload[1] == 0x01 {
			if !sensor.Motion || !known {
				sensor.Motion = true
				changed = true
			}
			c.scheduleMotionReset(addr)
		}

	case "KPD":
		sensor.Type = "KPD"
		switch {
		case isKPDHeartbeat(payload):
			c.mu.RLock()
			code := c.kpdCodes[addr]
			c.mu.RUnlock()
			if code != "" {
				c.sendKPDPostResponse(data, code)
			}
		case isKPDCommand(payload):
			if state := parseKPDState(payload); state != "" {
				if sensor.KPDState != state {
					sensor.KPDState = state
					changed = true
				}
			} else {
				changed = true
			}
		}

	default:
		// Unknown type — try format detection to guess.
		switch {
		case isDWSEvent(payload):
			sensor.Type = "DWS"
			sensor.Open = isDWSOpen(payload)
			changed = true
		case isKPDHeartbeat(payload) || isKPDCommand(payload):
			// Ambiguous — could be KPD or PIR. Leave as-is until known.
		}
	}

	c.mu.Lock()
	c.sensors[addr] = sensor
	c.mu.Unlock()

	if changed {
		select {
		case c.events <- SensorEvent{SensorID: addr, Sensor: sensor}:
		default:
			c.logger.Warn("event channel full, dropping event", "addr", addr)
		}
	}
}

// ackPKTEvent sends a managed frame ACK for a sensor event.
// If pureAck is true, sends a bare ACK (flags=0x0544, no WFlags/payload).
// Otherwise sends an ACK with keep-alive payload (flags=0x0547, WFlags=0xCC, payload=0x78).
// Pure ACKs are used for KPD button/code events (5501...) to prevent FNV loop.
func (c *CharmuxClient) ackPKTEvent(data []byte, pureAck bool) {
	if c.mux == nil || len(data) < 4 {
		return
	}
	sensorAddr := data[1]
	sensorCntRaw, _ := charmux.DecodeVarint(data[3:])
	sensorCnt := sensorCntRaw / 2

	cnt := c.nextManagedCnt()

	ack := &charmux.ManagedFrame{
		GWDst:   uint32(sensorAddr),
		GWSrc:   1,
		RFByte:  0,
		Counter: cnt,
		Src:     1,
		AckDst:  uint32(sensorAddr),
		AckCnt:  sensorCnt,
	}
	if pureAck {
		ack.Flags = 0x0544 // A+E+... no W, no payload
	} else {
		ack.Flags = 0x0547 // Z+W+A+E+... with payload
		ack.WFlags = 0xCC
		ack.Payload = []byte{0x78}
	}
	raw := ack.Serialize()
	if err := c.mux.SendPKT(context.Background(), raw); err != nil {
		c.logger.Warn("PKT ACK failed", "addr", sensorAddr, "error", err)
	}
}

// sendKPDPostResponse sends a kpd-post frame in response to a 5509 KPD heartbeat.
// This is CRITICAL: the KPD uses the 5509 → kpd-post exchange as a liveness check.
// If we respond with a standard ACK instead of a kpd-post, the KPD enters FNV
// verification loop and becomes unresponsive until the next battery cycle.
//
// Frame format: flags=0x0547, wflags=1, payload=[0x03, 0x00, 0x04, <BCD:4>]
// where BCD is the 4-digit PIN code encoded as 2 bytes with swapped nibbles
// (e.g. "1234" → 10 32, "1903" → 80 2A).
//
// See docs/kpd.md for the full protocol.
func (c *CharmuxClient) sendKPDPostResponse(data []byte, code string) {
	if c.mux == nil || len(data) < 4 {
		return
	}
	sensorAddr := data[1]
	sensorCntRaw, _ := charmux.DecodeVarint(data[3:])
	sensorCnt := sensorCntRaw / 2

	cnt := c.nextManagedCnt()

	bcd := domus.KpdBCDPublic(code)
	payload := make([]byte, 0, 3+len(bcd))
	payload = append(payload, 0x03, 0x00, byte(len(code)))
	payload = append(payload, bcd...)

	frame := &charmux.ManagedFrame{
		GWDst:   uint32(sensorAddr),
		GWSrc:   1,
		RFByte:  0,
		Counter: cnt,
		Src:     1,
		Flags:   0x0547,
		AckDst:  uint32(sensorAddr),
		AckCnt:  sensorCnt,
		WFlags:  0x01,
		Payload: payload,
	}
	raw := frame.Serialize()
	if err := c.mux.SendPKT(context.Background(), raw); err != nil {
		c.logger.Warn("kpd-post heartbeat response failed", "addr", sensorAddr, "error", err)
	}
}

// reinitSensor re-sends bytecode + config to a sensor after boot.
// Runs synchronously in eventLoop — events channel is exclusively ours.
// initialEvt is the f1 heartbeat that triggered the reinit (needed for ackCnt).
func (c *CharmuxClient) reinitSensor(addr int, initialEvt charmux.Event, events <-chan charmux.Event) {
	c.mu.Lock()
	c.isPairing = true
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		c.isPairing = false
		c.lastReinitTime[addr] = time.Now()
		c.mu.Unlock()
	}()

	c.mu.RLock()
	sensorType := c.knownTypes[addr]
	c.mu.RUnlock()

	model := domus.ModelForType(sensorType)
	if model == "" {
		c.logger.Warn("reinit: unknown model", "type", sensorType, "addr", addr)
		return
	}

	// Detect if triggered by FNV (33-byte frame) vs f1 heartbeat (11-14 bytes).
	fromFNV := len(initialEvt.Data) >= 30

	// Extract sensor counter from the initial event.
	var initialAckCnt uint32
	var heartbeatByte1 byte
	if parsed, err := charmux.DeserializeManagedFrame(initialEvt.Data); err == nil {
		initialAckCnt = parsed.Counter
		if !fromFNV && len(parsed.Payload) >= 2 && parsed.Payload[0] == 0xf1 {
			heartbeatByte1 = parsed.Payload[1]
			c.logger.Info("reinit: heartbeat payload", "byte1", fmt.Sprintf("0x%02x", heartbeatByte1))
		}
	}

	// Always re-register the sensor via double 0x15 before reinit.
	// Refreshes the MCU's radio association (same sequence as boot.sh).
	c.logger.Info("reinit: re-registering sensor via double 0x15", "addr", addr)
	ctxInit, cancelInit := context.WithTimeout(context.Background(), 3*time.Second)
	if _, err := c.mux.GetInfo(ctxInit); err != nil {
		c.logger.Warn("reinit: GetInfo failed", "error", err)
	}
	cancelInit()
	ctxNet, cancelNet := context.WithTimeout(context.Background(), 3*time.Second)
	if _, err := c.mux.GetNet(ctxNet); err != nil {
		c.logger.Warn("reinit: GetNet failed", "error", err)
	}
	cancelNet()
	cmd := make([]byte, 18)
	cmd[0] = 0x15
	cmd[1] = byte(addr)
	cmd[5] = byte(addr)
	if err := c.mux.SendRawCTRL(cmd); err != nil {
		c.logger.Warn("reinit: SendRawCTRL 0x15 #1 failed", "error", err)
	}
	c.mux.SendWatchdog()
	time.Sleep(500 * time.Millisecond)
	if err := c.mux.SendRawCTRL(cmd); err != nil {
		c.logger.Warn("reinit: SendRawCTRL 0x15 #2 failed", "error", err)
	}
	time.Sleep(1 * time.Second)
	if err := c.mux.SendRawCTRL([]byte{0x16}); err != nil {
		c.logger.Warn("reinit: SendRawCTRL 0x16 failed", "error", err)
	}
	c.mux.SendWatchdog()

	kpdCode := c.kpdCodes[addr]

	ctx := context.Background()

	// fbxhome starts its own gateway counter at 2 (independent of sensor counter).
	// The sensor's ackCnt is used for AckCnt field, not for our Counter.
	startCnt := uint32(2)
	c.logger.Info("reinit: starting", "addr", addr, "model", model, "hasCode", kpdCode != "", "fromFNV", fromFNV, "initialAckCnt", initialAckCnt, "startCounter", startCnt)
	domus.SendConfigReinitFull(ctx, c.mux, uint32(addr), model, kpdCode, initialAckCnt, startCnt, false, 0, fromFNV, events, c.logger)

	// Reset per-sensor counter to continue just after reinit frames.
	// Reinit uses ~8 frames (heartbeat-ack + 4 bytecode + end-marker + config + kpd-post).
	c.setManageCnt(startCnt+10)

	// Send watchdog to commit reinit config to NVM (same as post-pairing).
	time.Sleep(2 * time.Second)
	c.mux.SendWatchdog()
	c.logger.Info("reinit: watchdog commit sent", "addr", addr)

	c.logger.Info("reinit: complete", "addr", addr)

	// A successful reinit means the sensor is alive on the radio, so it
	// must reappear in the local map even if it was previously deleted
	// (delete only removes the local entry — the MCU keeps the address
	// in its NVM, so a stock heartbeat will still trigger reinit). Without
	// this, calls like SendSirenBeep below succeed at the radio level but
	// /api/sensors and MQTT discovery report the sensor as gone, and any
	// API that resolves an addr from the sensors map (siren test, etc)
	// fails with "aucune sirène appairée".
	c.mu.Lock()
	delete(c.deleted, addr)
	if _, ok := c.sensors[addr]; !ok {
		c.sensors[addr] = Sensor{
			ID:        addr,
			Type:      sensorType,
			Reachable: true,
			LastSeen:  time.Now().Unix(),
		}
	}
	c.mu.Unlock()

	// SRN sleep workaround: the siren stops responding to PKT frames a
	// few seconds after pairing completes (radio sleep). Send a beep
	// immediately while the sensor is still awake — this both validates
	// the actuator frame format and gives the user audible confirmation
	// that the siren paired successfully. Best-effort.
	if sensorType == "SRN" {
		c.logger.Info("siren: auto-test beep right after reinit", "addr", addr)
		if err := c.SendSirenBeep(ctx, addr); err != nil {
			c.logger.Warn("siren: auto-test beep failed", "error", err)
		}
	}
}

// --- PKT payload parsing ---

// isDWSEvent checks if the payload looks like a DWS open/close event.
// Minimum: 55 XX ... with at least 2 bytes after the 0x55 marker.
func isDWSEvent(payload []byte) bool {
	if len(payload) < 3 {
		return false
	}
	last2 := payload[len(payload)-2:]
	return (last2[0] == 0x00 && last2[1] == 0x01) || // open
		(last2[0] == 0x40 && last2[1] == 0x00) // close
}

// isDWSOpen returns true if the DWS payload indicates "open".
// Real hardware shows: payload ending in 40 00 = open, 00 01 = closed.
func isDWSOpen(payload []byte) bool {
	if len(payload) < 2 {
		return false
	}
	last2 := payload[len(payload)-2:]
	return last2[0] == 0x40 && last2[1] == 0x00
}

// isKPDHeartbeat checks for the KPD heartbeat pattern: 55 09.
func isKPDHeartbeat(payload []byte) bool {
	return len(payload) >= 2 && payload[0] == 0x55 && payload[1] == 0x09
}

// isKPDCommand checks for KPD arm/disarm commands: 55 01 ... (8+ bytes).
func isKPDCommand(payload []byte) bool {
	return len(payload) >= 8 && payload[0] == 0x55 && payload[1] == 0x01
}

// parseKPDState extracts the alarm state from a KPD 5501 event payload.
//
// Payload format (after 0x55 marker at data[9]):
//
//	55 01 [4 bytes var] [action_byte] [params...]
//
// action_byte (offset 6 from payload start):
//   - 0x04: button press → last byte = 0x01 (arm_away), 0x02 (arm_night)
//   - 0x84: code entry (bit 7 set) → BCD-encoded PIN follows, then 0x00 padding = disarm
//
// Captured examples:
//
//	5501 fb8c880f 04 01          → ON button = armed_away
//	5501 ff8c880f 84 0010320000 → code 1234 = disarmed
func parseKPDState(payload []byte) string {
	// payload = data[9:], starts with 55 01
	if len(payload) < 8 || payload[0] != 0x55 || payload[1] != 0x01 {
		return ""
	}

	action := payload[6]

	// Button press (no code)
	if action&0x80 == 0 {
		if len(payload) < 8 {
			return ""
		}
		switch payload[7] {
		case 0x01:
			return "armed_away"
		case 0x02:
			return "armed_night"
		default:
			return ""
		}
	}

	// Code entry (bit 7 set) → disarm
	return "disarmed"
}

// --- Node table parsing ---

// parseNodeTable extracts sensors from the raw 74-byte MCU node table.
//
// Format (discovered from testing):
//   byte 0 = opcode echo (0x07)
//   byte 1 = max slots (0x08)
//   bytes 2+ = node entries, 9 bytes each
//
// Each 9-byte entry: [addr, b1, b2, b3, b4, b5, b6, b7, b8]
// addr=0 means empty slot.
// The sensor type is determined by matching the addr against paired devices.
// Since we can't reliably detect the type from the table alone,
// we register all non-zero addr entries and let PKT events determine the type.
func parseNodeTable(raw []byte) map[int]Sensor {
	sensors := make(map[int]Sensor)

	if len(raw) < 2 || raw[0] != 0x07 {
		return sensors
	}

	maxSlots := int(raw[1])
	offset := 2
	for i := 0; i < maxSlots && offset+nodeEntrySize <= len(raw); i++ {
		entry := raw[offset : offset+nodeEntrySize]
		addr := int(entry[0])
		offset += nodeEntrySize

		if addr == 0 {
			continue
		}

		// Register the sensor with unknown type — PKT events will identify it
		sensors[addr] = Sensor{
			ID:   addr,
			Type: "UNKNOWN",
		}
	}

	return sensors
}
