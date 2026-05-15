package camera

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// CommandRunner abstracts os/exec for testability.
type CommandRunner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

type execRunner struct{}

func (execRunner) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	return exec.CommandContext(ctx, name, args...).Output()
}

// FbxhomeClient communicates with the fbxhome daemon over HTTP.
//
// fbxhome exposes two distinct sets of endpoints through nginx:
//   - `/rpc/*` on HTTP port 10000 (priv.conf, no auth surface)
//   - `/api/v1/home/*` on HTTPS port 64218 (qiet.conf, behind cloud tunnel
//     originally, but the local HTTPS listener accepts direct calls)
//
// Both endpoints accept the same `X-Hlcore-Session-Id` token. We keep one
// HTTP client (for /rpc/*) and one HTTPS client (for /api/v1/home/*) so
// each path uses the right scheme without templating per-call.
type FbxhomeClient struct {
	privURL   string // e.g. "http://[::1]:10000"     (for /rpc/*)
	homeURL   string // e.g. "https://[::1]:64218"    (for /api/v1/home/*)
	sessionID string
	http      *http.Client // shared (insecure TLS allowed for self-signed cert on homeURL)
	runner    CommandRunner
	logger    *slog.Logger

	events chan SensorEvent
	done   chan struct{}
	once   sync.Once

	mu          sync.RWMutex
	sensors     map[int]Sensor    // last known state, keyed by node ID
	fingerprints map[int]string   // pending fingerprints by pairing session ID
}

// Option configures a FbxhomeClient.
type Option func(*FbxhomeClient)

// WithHTTPClient sets a custom HTTP client.
func WithHTTPClient(c *http.Client) Option {
	return func(fc *FbxhomeClient) { fc.http = c }
}

// WithPrivURL overrides the priv API base URL (HTTP /rpc/*).
func WithPrivURL(url string) Option {
	return func(fc *FbxhomeClient) { fc.privURL = url }
}

// WithHomeURL overrides the home API base URL (HTTPS /api/v1/home/*).
func WithHomeURL(url string) Option {
	return func(fc *FbxhomeClient) { fc.homeURL = url }
}

// WithCommandRunner overrides the command runner (for tests).
func WithCommandRunner(r CommandRunner) Option {
	return func(fc *FbxhomeClient) { fc.runner = r }
}

// WithLogger sets a custom logger.
func WithLogger(l *slog.Logger) Option {
	return func(fc *FbxhomeClient) { fc.logger = l }
}

// NewFbxhomeClient creates a new fbxhome HTTP client.
func NewFbxhomeClient(opts ...Option) *FbxhomeClient {
	// Self-signed cert on port 64218 — skip verify.
	tr := &http.Transport{
		TLSClientConfig: &tls.Config{InsecureSkipVerify: true},
	}
	c := &FbxhomeClient{
		privURL: "http://[::1]:10000",
		homeURL: "https://[::1]:64218",
		http:    &http.Client{Timeout: 10 * time.Second, Transport: tr},
		runner:  execRunner{},
		logger:  slog.Default(),
		events:       make(chan SensorEvent, 64),
		done:         make(chan struct{}),
		sensors:      make(map[int]Sensor),
		fingerprints: make(map[int]string),
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Connect authenticates with fbxhome by calling fbxbusctl.
func (c *FbxhomeClient) Connect(ctx context.Context) error {
	return c.authenticate(ctx)
}

func (c *FbxhomeClient) authenticate(ctx context.Context) error {
	out, err := c.runner.Run(ctx, "fbxbusctl", "call", "fbxhome", "create_login_session", "1", "1")
	if err != nil {
		return fmt.Errorf("fbxbusctl auth: %w", err)
	}
	raw := strings.TrimSpace(string(out))
	// fbxbusctl output: session: "TOKEN_VALUE"
	token := raw
	if i := strings.Index(raw, `"`); i >= 0 {
		token = raw[i+1:]
		if j := strings.LastIndex(token, `"`); j >= 0 {
			token = token[:j]
		}
	}
	if token == "" {
		return fmt.Errorf("fbxbusctl returned empty session token")
	}
	c.sessionID = token
	c.logger.Info("authenticated with fbxhome", "session_id", token)
	return nil
}

// CachedSensors returns the last state observed by the poller, keyed by node id.
func (c *FbxhomeClient) CachedSensors() []Sensor {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]Sensor, 0, len(c.sensors))
	for _, s := range c.sensors {
		out = append(out, s)
	}
	return out
}

// UpdateCachedSensor met à jour le cache interne pour un node. Utilisé par
// les sources d'events temps réel (tail log, futur push fbxbus) pour que
// Sensors() reflète immédiatement le nouvel état sans attendre le polling.
//
// Merge non-destructif : on conserve les valeurs existantes (battery,
// temperature, item_id…) du cache et on remplace uniquement Open/Motion/
// Tamper/LastSeen. Les events temps réel ne portent pas ces métadonnées.
func (c *FbxhomeClient) UpdateCachedSensor(s Sensor) {
	if s.ID == 0 {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	cur := c.sensors[s.ID]
	cur.ID = s.ID
	if s.Type != "" {
		cur.Type = s.Type
	}
	cur.Open = s.Open
	cur.Motion = s.Motion
	cur.Tamper = s.Tamper
	if s.LastSeen > cur.LastSeen {
		cur.LastSeen = s.LastSeen
	}
	c.sensors[s.ID] = cur
}

// Sensors returns the paired sensor list from /rpc/get_domus_nodes, merged
// with the live state held by the poller (Open/Motion + fresh Battery/Temp).
// get_domus_nodes alone misses Open/Motion (carried only by endpoints_read),
// so the UI saw stale zeros until we merged the poller cache here.
func (c *FbxhomeClient) Sensors(ctx context.Context) ([]Sensor, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.privURL+"/rpc/get_domus_nodes", nil)
	if err != nil {
		return nil, fmt.Errorf("build domus_nodes request: %w", err)
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("get domus_nodes: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("get domus_nodes: status %d", resp.StatusCode)
	}

	var result domusNodesResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode domus_nodes: %w", err)
	}

	c.mu.RLock()
	cache := make(map[int]Sensor, len(c.sensors))
	for k, v := range c.sensors {
		cache[k] = v
	}
	c.mu.RUnlock()

	sensors := make([]Sensor, 0, len(result.Result))
	for _, n := range result.Result {
		s := nodeToSensor(n)
		if cached, ok := cache[s.ID]; ok {
			s.Open = cached.Open
			s.Motion = cached.Motion
			s.Tamper = cached.Tamper
			if cached.Battery > 0 {
				s.Battery = cached.Battery
			}
			if cached.Temperature != 0 {
				s.Temperature = cached.Temperature
			}
			if cached.LastSeen > s.LastSeen {
				s.LastSeen = cached.LastSeen
			}
		}
		sensors = append(sensors, s)
	}
	return sensors, nil
}

// shouldReauth returns true if the response indicates an expired session.
// fbxhome can signal this in two ways :
//   - 401 Unauthorized or 403 Forbidden (HTTP standard)
//   - 400 Bad Request with a JSON body containing `"reason":4` (vendor quirk,
//     vu après quelques heures d'uptime — la session X-Hlcore-Session-Id
//     expire silencieusement et fbxhome répond 400 sans WWW-Authenticate)
//
// Quand on détecte le 400 reason=4, on consomme le body — l'appelant doit
// utiliser le body retourné pour les fallbacks d'erreur (sinon il aurait
// un body déjà drainé).
func shouldReauth(resp *http.Response) (reauth bool, consumed []byte) {
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return true, nil
	}
	if resp.StatusCode == http.StatusBadRequest {
		body, _ := io.ReadAll(resp.Body)
		var probe struct {
			Reason int `json:"reason"`
		}
		if json.Unmarshal(body, &probe) == nil && probe.Reason == 4 {
			return true, body
		}
		return false, body
	}
	return false, nil
}

// ReadSensor reads endpoint values for a single sensor.
func (c *FbxhomeClient) ReadSensor(ctx context.Context, nodeID int, endpoints []string) (*Sensor, error) {
	body := endpointsReadRequest{
		List: []endpointQuery{{NodeID: nodeID, Endpoints: endpoints}},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal endpoints_read: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.homeURL+"/api/v1/home/endpoints_read", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("build endpoints_read request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hlcore-Session-Id", c.sessionID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("endpoints_read: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if reauth, _ := shouldReauth(resp); reauth {
		_ = resp.Body.Close()
		if err := c.authenticate(ctx); err != nil {
			return nil, fmt.Errorf("re-auth after %d: %w", resp.StatusCode, err)
		}
		return c.ReadSensor(ctx, nodeID, endpoints)
	}

	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("endpoints_read: status %d: %s", resp.StatusCode, body)
	}

	var result endpointsReadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode endpoints_read: %w", err)
	}

	if len(result.List) == 0 {
		return nil, fmt.Errorf("endpoints_read: empty response for node %d", nodeID)
	}

	s := endpointResultToSensor(nodeID, result.List[0])
	return &s, nil
}

// EndpointsRead reads raw endpoint values for a node (public API).
func (c *FbxhomeClient) EndpointsRead(ctx context.Context, nodeID int, endpoints []string) ([]EndpointValue, error) {
	body := endpointsReadRequest{
		List: []endpointQuery{{NodeID: nodeID, Endpoints: endpoints}},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return nil, fmt.Errorf("marshal endpoints_read: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.homeURL+"/api/v1/home/endpoints_read", bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("build endpoints_read request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hlcore-Session-Id", c.sessionID)

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, fmt.Errorf("endpoints_read: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if reauth, _ := shouldReauth(resp); reauth {
		_ = resp.Body.Close()
		if err := c.authenticate(ctx); err != nil {
			return nil, fmt.Errorf("re-auth: %w", err)
		}
		return c.EndpointsRead(ctx, nodeID, endpoints)
	}

	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("endpoints_read: status %d: %s", resp.StatusCode, b)
	}

	var result endpointsReadResponse
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return nil, fmt.Errorf("decode endpoints_read: %w", err)
	}

	if len(result.List) == 0 {
		return nil, nil
	}

	out := make([]EndpointValue, len(result.List[0].EPValues))
	for i, ep := range result.List[0].EPValues {
		out[i] = EndpointValue(ep)
	}
	return out, nil
}

// EndpointsWrite writes raw endpoint values for a node (public API).
func (c *FbxhomeClient) EndpointsWrite(ctx context.Context, nodeID int, eps []EndpointWriteEntry) error {
	writeEps := make([]endpointWrite, len(eps))
	for i, e := range eps {
		writeEps[i] = endpointWrite(e)
	}
	body := endpointsWriteRequest{
		List: []endpointWriteQuery{{NodeID: nodeID, Endpoints: writeEps}},
	}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal endpoints_write: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.homeURL+"/api/v1/home/endpoints_write", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build endpoints_write request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hlcore-Session-Id", c.sessionID)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("endpoints_write: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if reauth, _ := shouldReauth(resp); reauth {
		_ = resp.Body.Close()
		if err := c.authenticate(ctx); err != nil {
			return fmt.Errorf("re-auth: %w", err)
		}
		return c.EndpointsWrite(ctx, nodeID, eps)
	}

	// fbxhome répond 200 OK même sur "Not allowed" — sniffer la réponse.
	// shouldReauth peut avoir drainé le body sur un 400 reason!=4 ; dans ce
	// cas io.ReadAll renvoie 0 byte (Close idempotent), donc la heuristique
	// "reason+message" tombera silencieusement à false — ce cas reste géré
	// par la branche StatusCode != 200 manquante ici (fbxhome répond 200
	// même sur reject, donc on ne perd rien).
	respBody, _ := io.ReadAll(resp.Body)
	if bytes.Contains(respBody, []byte(`"reason"`)) && bytes.Contains(respBody, []byte(`"message"`)) {
		return fmt.Errorf("endpoints_write rejected: %s", bytes.TrimSpace(respBody))
	}

	// Note: the KPD firmware always returns success=0 but the operation does work.
	return nil
}

// StartPairing initiates a pairing session for the given sensor type.
// fingerprint is the 16-char hex prefix of the sensor's QR code raw bytes
// (e.g. "cca878cd4128e306" for the DWS). Required: fbxhome's pairing flow
// asks for it at the QRCode layout page and refuses to continue without.
func (c *FbxhomeClient) StartPairing(ctx context.Context, sensorType string, fingerprint string) (int, error) {
	nt, ok := NodeType[sensorType]
	if !ok {
		return 0, fmt.Errorf("unknown sensor type: %q", sensorType)
	}
	at := AdapterType[sensorType]

	body := pairingRequest{
		Op:          "start_adapter",
		NodeType:    nt,
		AdapterType: at,
	}

	var result pairingResponse
	if err := c.postPairing(ctx, body, &result); err != nil {
		return 0, err
	}

	if fingerprint != "" {
		c.mu.Lock()
		c.fingerprints[result.Session] = fingerprint
		c.mu.Unlock()
	}

	c.logger.Info("pairing started", "session", result.Session, "type", sensorType, "fingerprint", fingerprint)
	return result.Session, nil
}

// PollPairing checks pairing status. Returns (sensor, done, error).
// Handles fbxhome's QRCode prompt automatically by submitting the stored
// fingerprint via op="next".
func (c *FbxhomeClient) PollPairing(ctx context.Context, session int) (*Sensor, bool, error) {
	body := pairingRequest{Op: "poll", Session: session}

	var result pairingResponse
	if err := c.postPairing(ctx, body, &result); err != nil {
		return nil, false, err
	}

	// fbxhome shows the QRCode prompt — submit the stored fingerprint.
	if result.LayoutName == "QRCode" {
		c.mu.RLock()
		fp := c.fingerprints[session]
		c.mu.RUnlock()
		if fp == "" {
			return nil, false, fmt.Errorf("pairing: fbxhome requested QRCode but no fingerprint was provided at StartPairing")
		}
		nextBody := pairingRequest{
			Op:      "next",
			Session: session,
			Fields:  []string{fp},
		}
		var nextResult pairingResponse
		if err := c.postPairing(ctx, nextBody, &nextResult); err != nil {
			return nil, false, fmt.Errorf("pairing: submit fingerprint: %w", err)
		}
		c.logger.Info("pairing: fingerprint submitted", "session", session, "next_layout", nextResult.LayoutName)
		return nil, false, nil
	}

	if result.LayoutName == "Terminated" {
		shortType := sensorTypeFromNodeType[result.NodeType]
		s := Sensor{
			ID:       result.NodeID,
			Type:     shortType,
			TypeName: result.NodeType,
		}
		c.mu.Lock()
		delete(c.fingerprints, session)
		c.mu.Unlock()
		return &s, true, nil
	}

	return nil, false, nil
}

// DeleteSensor removes a paired sensor by node ID.
func (c *FbxhomeClient) DeleteSensor(ctx context.Context, nodeID int) error {
	body := deleteRequest{Op: "delete", List: []int{nodeID}}
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal delete request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.homeURL+"/api/v1/home/delete", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build delete request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hlcore-Session-Id", c.sessionID)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("delete sensor %d: %w", nodeID, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("delete sensor %d: status %d: %s", nodeID, resp.StatusCode, respBody)
	}
	return nil
}

// OpenStream opens the video stream and returns SRT connection info.
func (c *FbxhomeClient) OpenStream(ctx context.Context) (StreamInfo, error) {
	data := []byte(`{}`)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.homeURL+"/api/v1/home/open_stream", bytes.NewReader(data))
	if err != nil {
		return StreamInfo{}, fmt.Errorf("build open_stream request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hlcore-Session-Id", c.sessionID)

	resp, err := c.http.Do(req)
	if err != nil {
		return StreamInfo{}, fmt.Errorf("open_stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var result struct {
		Passphrase string `json:"passphrase"`
		Success    int    `json:"success"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return StreamInfo{}, fmt.Errorf("decode open_stream: %w", err)
	}
	if result.Success != 1 {
		return StreamInfo{}, fmt.Errorf("open_stream failed")
	}

	return StreamInfo{Passphrase: result.Passphrase, Port: 9000}, nil
}

// StopPairing cancels a pairing session.
func (c *FbxhomeClient) StopPairing(ctx context.Context, session int) error {
	body := pairingRequest{Op: "stop", Session: session}
	var result pairingResponse
	return c.postPairing(ctx, body, &result)
}

// Events returns the sensor event channel.
// SendPKT is not supported in fbxhome mode.
func (c *FbxhomeClient) SendPKT(_ context.Context, _ []byte) error {
	return fmt.Errorf("fbxhome_client: SendPKT not supported")
}

// TriggerSiren sends a test command to a siren sensor via endpoints_write.
// This is the discrete "test sirène" sound (power=10, duration=10s).
func (c *FbxhomeClient) TriggerSiren(ctx context.Context, sensorID int) error {
	eps := []EndpointWriteEntry{{EPName: "test", Value: true}}
	return c.EndpointsWrite(ctx, sensorID, eps)
}

// TriggerSirenAlarm fires the SRN siren as a wail full-power.
//
// On pousse `test_duration`, `test_power=100` puis `test=true` dans un
// SEUL endpoints_write. fbxhome convertit en payload radio
// `5505 01 PP DD` où PP=power et DD=duration en quarts de seconde
// (cap radio = 255 quarts ≈ 63s ; au-delà la SRN tronque).
//
// L'API fbxhome attend les durations en SECONDES (pas en quarts) — la
// conversion *4 est faite côté fbxhome. Cap pratique côté API : 63s.
//
// Découvert 2026-05-15 : la limitation "Not allowed reason=5" sur
// test_power/test_duration observée en avril était un artefact de
// l'ordre ou du state SRN. La combinaison correcte est validée audible
// (cf. feedback_srn_wail_limit.md).
//
// Pour stop : appeler StopSiren (endpoints_write test=false suffit en
// cas normal, reboot_srn en kill-switch).
func (c *FbxhomeClient) TriggerSirenAlarm(ctx context.Context, sensorID int, duration time.Duration) error {
	seconds := int(duration / time.Second)
	if seconds <= 0 {
		seconds = 10
	}
	if seconds > 63 {
		seconds = 63
	}

	// Ordre IMPORTANT : test_duration et test_power AVANT test, sinon
	// fbxhome ignore les nouveaux params et joue avec les valeurs courantes.
	eps := []EndpointWriteEntry{
		{EPName: "test_duration", Value: seconds},
		{EPName: "test_power", Value: 100},
		{EPName: "test", Value: true},
	}
	return c.EndpointsWrite(ctx, sensorID, eps)
}

// StopSiren stops an ongoing siren.
//
// reboot_srn est le seul kill-switch fiable. `endpoints_write test=false`
// a été testé 2026-05-15 et observé re-déclencher la trame radio
// `5505 01 PP DD` au lieu de stopper — fbxhome semble bufferiser un
// push en attente que le `false` ne purge pas.
//
// Side effect reboot_srn : la SRN passe par OFF + re-handshake (~5s)
// avant d'être ré-utilisable. Acceptable pour un stop d'alarme.
//
// Cas dégradé : si reboot_srn échoue, on se contente d'écrire test=false
// (best-effort) — au pire la SRN s'arrête d'elle-même au bout de
// test_duration (max 63s).
func (c *FbxhomeClient) StopSiren(ctx context.Context, sensorID int) error {
	if _, err := c.runner.Run(ctx, "fbxbusctl", "call", "fbxhome", "reboot_srn"); err != nil {
		// Best-effort fallback : tente test=false pour clear le buffer
		// fbxhome. N'arrête PAS le wail en cours, mais limite la durée
		// si update_status était encore en attente de push.
		eps := []EndpointWriteEntry{{EPName: "test", Value: false}}
		_ = c.EndpointsWrite(ctx, sensorID, eps)
		return fmt.Errorf("reboot_srn: %w", err)
	}
	return nil
}

// SetShutter opens/closes the shutter via fbxhome endpoints_write.
// fbxhome uses inverted logic: true=close, false=open.
func (c *FbxhomeClient) SetShutter(ctx context.Context, open bool) error {
	eps := []EndpointWriteEntry{{EPName: "shutter", Value: !open}}
	return c.EndpointsWrite(ctx, 3, eps) // node 3 = camera
}

// SetKPDPassword stores a 4-digit PIN code for the KPD via fbxhome's
// `pwd` endpoint. fbxhome persists it in the XML as a <Code> child of the
// HLKpd node and pushes it to the keypad radio at the next reinit cycle.
// Replaces the existing code list (use ClearKPDPassword to remove all).
func (c *FbxhomeClient) SetKPDPassword(ctx context.Context, sensorID int, password, label string) error {
	if label == "" {
		label = "Code"
	}
	value := map[string]any{
		"list": []map[string]any{
			{"password": password, "label": label},
		},
	}
	return c.EndpointsWrite(ctx, sensorID, []EndpointWriteEntry{
		{EPName: "pwd", Value: value},
	})
}

// ClearKPDPassword removes all stored PIN codes from the KPD.
func (c *FbxhomeClient) ClearKPDPassword(ctx context.Context, sensorID int) error {
	value := map[string]any{"list": []map[string]any{}}
	return c.EndpointsWrite(ctx, sensorID, []EndpointWriteEntry{
		{EPName: "pwd", Value: value},
	})
}

func (c *FbxhomeClient) Events() <-chan SensorEvent {
	return c.events
}

// StartPolling begins periodic sensor polling in a goroutine.
// Call Close to stop it.
func (c *FbxhomeClient) StartPolling(ctx context.Context, interval time.Duration) {
	go c.pollLoop(ctx, interval)
}

func (c *FbxhomeClient) pollLoop(ctx context.Context, interval time.Duration) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		select {
		case <-c.done:
			return
		case <-ctx.Done():
			return
		case <-ticker.C:
			c.pollOnce(ctx)
		}
	}
}

func (c *FbxhomeClient) pollOnce(ctx context.Context) {
	sensors, err := c.Sensors(ctx)
	if err != nil {
		c.logger.Error("poll sensors failed", "error", err)
		return
	}

	for _, s := range sensors {
		// KPD/SRN have no "state" endpoint; asking returns
		// `request malformed` and floods the log every poll tick.
		eps := []string{"state", "temperature", "battery"}
		if s.Type == "KPD" || s.Type == "SRN" {
			eps = []string{"battery"}
		}
		updated, err := c.ReadSensor(ctx, s.ID, eps)
		if err != nil {
			c.logger.Error("poll read sensor failed", "node_id", s.ID, "error", err)
			continue
		}
		// Merge fields from Sensors() that ReadSensor doesn't return.
		updated.TypeName = s.TypeName
		updated.ItemID = s.ItemID
		updated.Type = s.Type
		updated.Reachable = s.Reachable

		c.mu.RLock()
		prev, exists := c.sensors[s.ID]
		c.mu.RUnlock()

		if !exists || hasChanged(prev, *updated) {
			c.mu.Lock()
			c.sensors[s.ID] = *updated
			c.mu.Unlock()

			select {
			case c.events <- SensorEvent{SensorID: s.ID, Sensor: *updated}:
			default:
				c.logger.Warn("event channel full, dropping event", "node_id", s.ID)
			}
		}
	}
}

// Close stops polling and closes the event channel.
func (c *FbxhomeClient) Close() error {
	c.once.Do(func() {
		close(c.done)
		close(c.events)
	})
	return nil
}

// --- helpers ---

func (c *FbxhomeClient) postPairing(ctx context.Context, body pairingRequest, result *pairingResponse) error {
	data, err := json.Marshal(body)
	if err != nil {
		return fmt.Errorf("marshal pairing request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.homeURL+"/api/v1/home/pairing", bytes.NewReader(data))
	if err != nil {
		return fmt.Errorf("build pairing request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Hlcore-Session-Id", c.sessionID)

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("pairing %s: %w", body.Op, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("pairing %s: status %d: %s", body.Op, resp.StatusCode, respBody)
	}

	if err := json.NewDecoder(resp.Body).Decode(result); err != nil {
		return fmt.Errorf("decode pairing %s: %w", body.Op, err)
	}
	return nil
}

func nodeToSensor(n domusNode) Sensor {
	return Sensor{
		ID:          n.ID,
		Type:        sensorTypeFromNodeType[n.TypeName],
		TypeName:    n.TypeName,
		ItemID:      n.ItemID,
		Battery:     n.Values.Battery,
		Temperature: n.Values.Temperature.Value,
		Reachable:   n.Values.Reachable != 0,
		LastSeen:    n.Values.Temperature.Timestamp,
	}
}

func endpointResultToSensor(nodeID int, er endpointResult) Sensor {
	s := Sensor{ID: nodeID}
	for _, ep := range er.EPValues {
		switch ep.EPName {
		case "state":
			if v, ok := ep.Value.(bool); ok {
				s.Open = v
				s.Motion = v
			}
		case "temperature":
			if m, ok := ep.Value.(map[string]interface{}); ok {
				if v, ok := m["value"].(float64); ok {
					s.Temperature = int(v)
				}
				if v, ok := m["timestamp"].(float64); ok {
					// fbxhome retourne parfois un timestamp aberrant
					// (~2.4e18) tant qu'aucune mesure n'a été reçue depuis le
					// pairing. On filtre : un timestamp Unix raisonnable est
					// < 2e10 (≈ année 2603), tout au-delà = donnée invalide.
					ts := int64(v)
					if ts > 0 && ts < 20000000000 {
						s.LastSeen = ts
					}
				}
			}
		case "battery":
			if v, ok := ep.Value.(float64); ok {
				s.Battery = int(v)
			}
		}
	}
	return s
}

func hasChanged(prev, curr Sensor) bool {
	return prev.Open != curr.Open ||
		prev.Motion != curr.Motion ||
		prev.Battery != curr.Battery ||
		prev.Temperature != curr.Temperature ||
		prev.Reachable != curr.Reachable
}
