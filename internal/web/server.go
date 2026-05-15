// Package web provides the embedded web UI and REST API.
package web

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/config"
	"github.com/caligone/openqiara/internal/hlevents"
)

// MQTTCallbacks allows the web server to trigger publisher updates on sensor changes.
// Despite the name, callbacks here are not MQTT-specific — HomeKit also uses
// OnSensorsChanged to rebuild its bridge when the sensor list mutates.
type MQTTCallbacks struct {
	// OnSensorRenamed is called when a sensor is renamed. It should re-publish discovery.
	OnSensorRenamed func(ctx context.Context, sensor camera.Sensor) error
	// OnSensorDeleted is called when a sensor is deleted. It should remove discovery.
	OnSensorDeleted func(ctx context.Context, sensorID int) error
	// OnSensorsChanged is called after the persisted sensor list mutates
	// (pairing complete, deletion). Publishers that need to rebuild their
	// view of the world from scratch (HomeKit bridge) wire this. Called
	// AFTER the per-sensor callbacks above so MQTT discovery is in sync first.
	OnSensorsChanged func(ctx context.Context)
	// OnAlarmStateChanged is called when the alarm state is changed from the web UI.
	// state is one of: "disarmed", "armed_away", "armed_night".
	OnAlarmStateChanged func(ctx context.Context, state string) error
	// OnAlarmCommand dispatches a UI alarm command (arm_away, arm_night, disarm)
	// to the same handler that processes MQTT/HomeKit commands. In alarmo mode
	// this forwards to alarmo/command; in standalone it feeds the local engine.
	OnAlarmCommand func(ctx context.Context, cmd string)
}

// AlarmSnapshot represents the current alarm state for the API.
// Fields match internal/alarm.Snapshot.
type AlarmSnapshot struct {
	State          string `json:"state"`
	ArmedAt        int64  `json:"armed_at,omitempty"`
	TriggeredBy    int    `json:"triggered_by,omitempty"`
	PreviousState  string `json:"previous_state,omitempty"`
	TimerRemaining int    `json:"timer_remaining,omitempty"`
}

// AlarmProvider is the interface the web server uses to query and command the alarm engine.
// It breaks the cyclic dependency between web and alarm packages.
//
// HandleCommand reçoit une commande sans qualification de source — les call
// sites internes au web (handler POST /api/alarm) sont par construction des
// commandes distantes (HTTP), donc l'implémentation route en SourceRemote.
type AlarmProvider interface {
	Snapshot() AlarmSnapshot
	HandleCommand(cmd string)
	SetTimings(arming, pending time.Duration)
}

// SensorListProvider returns the merged sensor list (MCU + persisted
// config). When the MCU is temporarily unresponsive (e.g. just after a
// reboot), the merged list still contains the sensors known to the
// config so the UI doesn't show an empty page. Provided by main.go to
// avoid creating a config↔camera package dependency cycle.
type SensorListProvider func(ctx context.Context) ([]camera.Sensor, error)

// Server serves the web UI and REST API for managing OpenQiara.
type Server struct {
	cam          camera.Client
	store        *config.Store
	mqttOK       func() bool
	mqttCB       *MQTTCallbacks
	sensorListFn SensorListProvider
	startTime    time.Time
	log          *slog.Logger
	srv          *http.Server
	staticFS     fs.FS

	alarmMu    sync.RWMutex
	alarmState string // fallback when no alarm provider is attached

	alarmProvider AlarmProvider

	cachedSensorCount int

	hub      *sseHub
	stopTick chan struct{}

	// kpdPairMu protects kpdPairedAt and pendingKPDCode.
	kpdPairMu      sync.Mutex
	kpdPairedAt    time.Time
	pendingKPDCode *pendingKPDCodeJob // si non nil, un job différé est en cours

	// debugEnabled active les endpoints /api/debug/* (envoi PKT brut au MCU,
	// séquences sirène arbitraires). Désactivé par défaut — ces endpoints
	// peuvent brick le MCU (opcodes 0x03, 0x08) ou stresser un capteur.
	debugEnabled bool

	// hlDispatcher route les events /events et notifs /notifications
	// (poussés par hl_event_collectd, le proxy cloud Free) vers les
	// publishers MQTT/HK. nil = on log seulement, pas de dispatch.
	hlDispatcher *hlevents.Dispatcher
}

// pendingKPDCodeJob représente une écriture de code PIN en attente que le
// bytecode push post-pairing soit terminé.
type pendingKPDCodeJob struct {
	password string
	label    string
	cancel   chan struct{}
}

// SetSensorListProvider injects the merged sensor list provider. Should
// be called once after NewServer, before Start. If never called, handlers
// fall back to cam.Sensors() — which is what the existing tests rely on.
func (s *Server) SetSensorListProvider(fn SensorListProvider) {
	s.sensorListFn = fn
}

// listSensors returns the merged sensor list when a provider is set,
// otherwise falls back to a direct MCU query. Centralised so all handlers
// share the same source of truth.
func (s *Server) listSensors(ctx context.Context) ([]camera.Sensor, error) {
	if s.sensorListFn != nil {
		return s.sensorListFn(ctx)
	}
	return s.cam.Sensors(ctx)
}

// SetAlarmProvider attaches the alarm engine to the web server.
// Safe to call after Start.
func (s *Server) SetAlarmProvider(p AlarmProvider) {
	s.alarmMu.Lock()
	defer s.alarmMu.Unlock()
	s.alarmProvider = p
}

// NewServer creates a new web server.
func NewServer(cam camera.Client, store *config.Store, mqttOK func() bool, mqttCB *MQTTCallbacks, staticFS fs.FS, logger *slog.Logger) *Server {
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{
		cam:        cam,
		store:      store,
		mqttOK:     mqttOK,
		mqttCB:     mqttCB,
		startTime:  time.Now(),
		log:        logger,
		staticFS:   staticFS,
		alarmState: "disarmed",
		hub:        newSSEHub(),
		stopTick:   make(chan struct{}),
	}
}

// EnableDebugEndpoints expose /api/debug/* (PKT brut, sirène raw).
// À n'activer qu'en développement.
func (s *Server) EnableDebugEndpoints() {
	s.debugEnabled = true
}

// SetHLEventsDispatcher attache le dispatcher qui consomme les events
// /events et /notifications poussés par hl_event_collectd. Si non
// appelé, les bodies sont juste loggués sans traitement.
func (s *Server) SetHLEventsDispatcher(d *hlevents.Dispatcher) {
	s.hlDispatcher = d
}

// SetAlarmState updates the alarm state from an external source (e.g. KPD event).
func (s *Server) SetAlarmState(state string) {
	s.alarmMu.Lock()
	defer s.alarmMu.Unlock()
	s.alarmState = state
}

// Start begins serving on the given address (e.g. ":80") in a new goroutine.
// Returns immediately. Call Shutdown to stop.
func (s *Server) Start(addr string) error {
	mux := http.NewServeMux()

	// API routes
	mux.HandleFunc("GET /api/status", s.cors(s.handleStatus))
	mux.HandleFunc("GET /api/sensors", s.cors(s.handleSensors))
	mux.HandleFunc("POST /api/sensors/pair", s.cors(s.handleStartPairing))
	mux.HandleFunc("GET /api/sensors/pair", s.cors(s.handlePollPairing))
	mux.HandleFunc("DELETE /api/sensors/pair", s.cors(s.handleStopPairing))
	mux.HandleFunc("DELETE /api/sensors/{id}", s.cors(s.handleDeleteSensor))
	mux.HandleFunc("PUT /api/sensors/{id}", s.cors(s.handleRenameSensor))
	mux.HandleFunc("POST /api/stream", s.cors(s.handleOpenStream))
	mux.HandleFunc("GET /api/config", s.cors(s.handleGetConfig))
	mux.HandleFunc("PUT /api/config/mqtt", s.cors(s.handleUpdateMQTT))
	mux.HandleFunc("PUT /api/config/homekit", s.cors(s.handleUpdateHomeKit))
	mux.HandleFunc("PUT /api/config/admin", s.cors(s.handleUpdateAdmin))
	mux.HandleFunc("PUT /api/config/alarm", s.cors(s.handleUpdateAlarm))
	mux.HandleFunc("GET /api/config/fbxhome_alarm", s.cors(s.handleGetFbxhomeAlarm))
	mux.HandleFunc("PUT /api/config/fbxhome_alarm", s.cors(s.handleUpdateFbxhomeAlarm))
	mux.HandleFunc("PUT /api/config/web", s.cors(s.handleUpdateWeb))
	mux.HandleFunc("GET /api/alarm", s.cors(s.handleGetAlarm))
	mux.HandleFunc("POST /api/alarm", s.cors(s.handleSetAlarm))
	mux.HandleFunc("GET /api/codes", s.cors(s.handleGetCodes))
	mux.HandleFunc("POST /api/codes", s.cors(s.handleAddCode))
	mux.HandleFunc("DELETE /api/codes", s.cors(s.handleDeleteCode))
	mux.HandleFunc("POST /api/reboot", s.cors(s.handleReboot))
	mux.HandleFunc("POST /api/siren/test", s.cors(s.handleSirenTest))
	mux.HandleFunc("POST /api/siren/alarm_test", s.cors(s.handleSirenAlarmTest))
	mux.HandleFunc("POST /api/stream/start", s.cors(s.handleStartStream))
	mux.HandleFunc("POST /api/shutter", s.cors(s.handleShutter))
	mux.HandleFunc("GET /stream/", s.cors(s.handleHLSStream))
	if s.debugEnabled {
		mux.HandleFunc("POST /api/debug/pkt", s.cors(s.handleDebugPKT))
		mux.HandleFunc("POST /api/debug/siren/sequence", s.cors(s.handleDebugSirenSeq))
	}
	// Server-sent events stream (alarm, sensors, status).
	mux.HandleFunc("GET /api/events", s.cors(s.handleEvents))

	// Push depuis hl_event_collectd (intercepte les webhooks cloud Free).
	// Le DNS local résout *.srv.home-labs.fr → 127.0.0.1 ; le collectd
	// pousse les events sur /events (sensor events, alarm transitions,
	// shutter, etc.) et les notifications sur /notifications (IV events
	// type human/pet detection). On accepte les POST sans auth — le
	// hostname EUPID.srv.home-labs.fr résolu localement suffit comme garde.
	//
	// Si on répond autre chose que 200, hl_event_collectd met les events
	// en queue retry et ne pousse plus rien d'autre tant que la queue n'est
	// pas vidée — il faut donc handler les DEUX routes en 200 OK.
	mux.HandleFunc("POST /events", s.handleFbxhomePush)
	mux.HandleFunc("POST /notifications", s.handleFbxhomePush)
	// CORS preflight
	mux.HandleFunc("OPTIONS /api/", s.handleOptions)

	// Static files (SPA)
	staticSub, err := fs.Sub(s.staticFS, "static")
	if err != nil {
		return fmt.Errorf("embed sub: %w", err)
	}
	fileServer := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /", fileServer)

	s.srv = &http.Server{
		Addr:        addr,
		Handler:     s.basicAuth(mux),
		ReadTimeout: 10 * time.Second,
		// No WriteTimeout: SSE connections stay open indefinitely.
		// Regular handlers have their own deadlines via context if needed.
	}

	go func() {
		s.log.Info("web server starting", "addr", addr)
		if err := s.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			s.log.Error("web server error", "error", err)
		}
	}()

	// Periodic status + alarm timer updates pushed into the SSE hub.
	go s.tickLoop()

	return nil
}

// tickLoop periodically pushes status and timer updates into the SSE hub.
// Runs at 1s during active timers (arming/pending/triggered) and 5s otherwise.
func (s *Server) tickLoop() {
	t := time.NewTicker(1 * time.Second)
	defer t.Stop()

	var lastStatusPush time.Time
	for {
		select {
		case <-s.stopTick:
			return
		case now := <-t.C:
			// Push alarm snapshot if the engine is in a timer state.
			s.alarmMu.RLock()
			provider := s.alarmProvider
			s.alarmMu.RUnlock()
			if provider != nil {
				snap := provider.Snapshot()
				if snap.TimerRemaining > 0 {
					s.hub.Publish(sseEvent{Type: "alarm", Data: snap})
				}
			}

			// Push a status update every 5 seconds.
			if now.Sub(lastStatusPush) >= 5*time.Second {
				lastStatusPush = now
				s.hub.Publish(sseEvent{Type: "status", Data: s.buildStatus()})
			}
		}
	}
}

// buildStatus returns the current status payload for periodic SSE pushes.
// IMPORTANT: this MUST NOT call cam.Sensors() or any other MCU call — it runs
// every 5s on a ticker and would interfere with pairing/reinit dialogs on the
// CTRL channel. The sensor count is cached via sensorCount()/setSensorCount().
func (s *Server) buildStatus() map[string]any {
	s.alarmMu.RLock()
	count := s.cachedSensorCount
	s.alarmMu.RUnlock()
	return map[string]any{
		"mqtt_connected": s.mqttOK != nil && s.mqttOK(),
		"sensor_count":   count,
		"uptime":         int(time.Since(s.startTime).Seconds()),
	}
}

// SetSensorCount updates the cached sensor count used by the tick loop.
// Called from main.go after Sensors() returns successfully.
func (s *Server) SetSensorCount(n int) {
	s.alarmMu.Lock()
	s.cachedSensorCount = n
	s.alarmMu.Unlock()
}

// Shutdown gracefully stops the server.
func (s *Server) Shutdown(ctx context.Context) error {
	if s.stopTick != nil {
		select {
		case <-s.stopTick:
			// already closed
		default:
			close(s.stopTick)
		}
	}
	if s.srv == nil {
		return nil
	}
	return s.srv.Shutdown(ctx)
}

// --- Middleware ---

func (s *Server) cors(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization")
		next(w, r)
	}
}

// basicAuth is a middleware that requires HTTP Basic Auth when the admin
// password is configured. If the password is empty, it is a no-op.
// Username is fixed to "admin".
func (s *Server) basicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cfg := s.store.Get()
		password := cfg.Admin.Password

		// No password configured → open access.
		if password == "" {
			next.ServeHTTP(w, r)
			return
		}

		// Allow CORS preflight without auth (browsers don't send credentials on OPTIONS).
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		// Allow fbxhome push without auth — c'est un hook intra-cam,
		// pas exposé sur le LAN (sauf si quelqu'un sait le hostname interne).
		if r.Method == http.MethodPost && (r.URL.Path == "/events" || r.URL.Path == "/notifications") {
			next.ServeHTTP(w, r)
			return
		}

		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || pass != password {
			w.Header().Set("WWW-Authenticate", `Basic realm="OpenQiara"`)
			http.Error(w, "Authentification requise", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) handleOptions(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
	w.WriteHeader(http.StatusNoContent)
}

// --- Handlers ---

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	sensors, err := s.listSensors(r.Context())
	sensorCount := 0
	if err == nil {
		sensorCount = len(sensors)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"uptime":         int(time.Since(s.startTime).Seconds()),
		"mqtt_connected": s.mqttOK(),
		"sensor_count":   sensorCount,
	})
}

func (s *Server) handleSensors(w http.ResponseWriter, r *http.Request) {
	sensors, err := s.listSensors(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "impossible de lister les capteurs: "+err.Error())
		return
	}

	cfg := s.store.Get()
	type sensorCfg struct {
		label        string
		nightAllowed bool
		dayAlarm     *bool
		nightAlarm   *bool
		dayTimed     *bool
		nightTimed   *bool
	}
	cfgBySensor := make(map[int]sensorCfg, len(cfg.Sensors))
	for _, se := range cfg.Sensors {
		cfgBySensor[se.ID] = sensorCfg{
			label:        se.Label,
			nightAllowed: se.NightAllowed,
			dayAlarm:     se.DayAlarm,
			nightAlarm:   se.NightAlarm,
			dayTimed:     se.DayTimed,
			nightTimed:   se.NightTimed,
		}
	}

	type sensorWithMeta struct {
		camera.Sensor
		Label        string `json:"label"`
		NightAllowed bool   `json:"night_allowed"`
		DayAlarm     *bool  `json:"day_alarm,omitempty"`
		NightAlarm   *bool  `json:"night_alarm,omitempty"`
		DayTimed     *bool  `json:"day_timed,omitempty"`
		NightTimed   *bool  `json:"night_timed,omitempty"`
	}
	// Don't call ReadSensor per-sensor here: each one waits for the MCU
	// (~3s timeout when the MCU is silent), so 5 sensors blocked the
	// page for 15s and the frontend gave up. Live battery/temperature/
	// state come through PKT events forwarded over SSE — the initial
	// fetch only needs metadata (label, type, reachability).
	result := make([]sensorWithMeta, 0, len(sensors))
	for _, sensor := range sensors {
		sc := cfgBySensor[sensor.ID]
		result = append(result, sensorWithMeta{
			Sensor: sensor, Label: sc.label, NightAllowed: sc.nightAllowed,
			DayAlarm: sc.dayAlarm, NightAlarm: sc.nightAlarm,
			DayTimed: sc.dayTimed, NightTimed: sc.nightTimed,
		})
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleStartPairing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type        string `json:"type"`
		Fingerprint string `json:"fingerprint"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	body.Type = strings.ToUpper(strings.TrimSpace(body.Type))
	if body.Type == "" {
		writeErr(w, http.StatusBadRequest, "champ 'type' requis (DWS, PIR, SRN, KPD)")
		return
	}
	fp := normalizeFingerprint(body.Fingerprint)

	session, err := s.cam.StartPairing(r.Context(), body.Type, fp)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec démarrage appairage: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"session": session})
}

// normalizeFingerprint sanitize l'input utilisateur d'un fingerprint QR :
// supprime espaces et tirets (ex: "a4f4-d1ec-8ff1-376e" → "a4f4d1ec8ff1376e"),
// passe en minuscules, et tronque aux 16 premiers chars hex.
//
// fbxhome attend exactement 16 chars hex (= 8 premiers octets du QR raw).
// Les utilisateurs collent parfois la chaîne complète 32 chars ou avec des
// tirets UUID-like ; on accepte les deux.
func normalizeFingerprint(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	s = strings.ReplaceAll(s, "-", "")
	s = strings.ReplaceAll(s, " ", "")
	if len(s) > 16 {
		s = s[:16]
	}
	return s
}

func (s *Server) handlePollPairing(w http.ResponseWriter, r *http.Request) {
	sessionStr := r.URL.Query().Get("session")
	session, err := strconv.Atoi(sessionStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "paramètre 'session' invalide")
		return
	}

	sensor, done, err := s.cam.PollPairing(r.Context(), session)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "erreur polling appairage: "+err.Error())
		return
	}

	if done && sensor != nil {
		_ = s.store.Update(func(c *config.Config) {
			for i := range c.Sensors {
				if c.Sensors[i].ID == sensor.ID {
					c.Sensors[i].Type = sensor.Type
					return
				}
			}
			c.Sensors = append(c.Sensors, config.SensorEntry{ID: sensor.ID, Type: sensor.Type})
		})
		// Marque le timestamp pour le KPD — son cycle bytecode post-pair
		// prend ~10s ; les écritures de code PIN doivent attendre pour ne pas
		// le corrompre. Cf. handleAddCode.
		if sensor.Type == "KPD" {
			s.kpdPairMu.Lock()
			s.kpdPairedAt = time.Now()
			s.kpdPairMu.Unlock()
		}
		if s.mqttCB != nil && s.mqttCB.OnSensorsChanged != nil {
			s.mqttCB.OnSensorsChanged(r.Context())
		}
	}

	resp := map[string]any{"done": done}
	if sensor != nil {
		resp["sensor"] = sensor
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleStopPairing(w http.ResponseWriter, r *http.Request) {
	sessionStr := r.URL.Query().Get("session")
	session, err := strconv.Atoi(sessionStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "paramètre 'session' invalide")
		return
	}

	if err := s.cam.StopPairing(r.Context(), session); err != nil {
		writeErr(w, http.StatusInternalServerError, "échec annulation appairage: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleDeleteSensor(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ID capteur invalide")
		return
	}

	if err := s.cam.DeleteSensor(r.Context(), id); err != nil {
		writeErr(w, http.StatusInternalServerError, "échec suppression capteur: "+err.Error())
		return
	}

	// Remove from MQTT
	if s.mqttCB != nil && s.mqttCB.OnSensorDeleted != nil {
		if err := s.mqttCB.OnSensorDeleted(r.Context(), id); err != nil {
			s.log.Warn("mqtt remove discovery failed", "id", id, "error", err)
		}
	}

	// Remove from config and persist deletion
	_ = s.store.Update(func(c *config.Config) {
		for i := range c.Sensors {
			if c.Sensors[i].ID == id {
				c.Sensors = append(c.Sensors[:i], c.Sensors[i+1:]...)
				break
			}
		}
		// Persist deleted ID so it doesn't come back from GetNodes after reboot
		for _, did := range c.DeletedIDs {
			if did == id {
				return // already in the list
			}
		}
		c.DeletedIDs = append(c.DeletedIDs, id)
	})

	if s.mqttCB != nil && s.mqttCB.OnSensorsChanged != nil {
		s.mqttCB.OnSensorsChanged(r.Context())
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleRenameSensor(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ID capteur invalide")
		return
	}

	var body struct {
		Label        *string `json:"label,omitempty"`
		NightAllowed *bool   `json:"night_allowed,omitempty"`
		DayAlarm     *bool   `json:"day_alarm,omitempty"`
		NightAlarm   *bool   `json:"night_alarm,omitempty"`
		DayTimed     *bool   `json:"day_timed,omitempty"`
		NightTimed   *bool   `json:"night_timed,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	if body.Label != nil {
		trimmed := strings.TrimSpace(*body.Label)
		body.Label = &trimmed
	}
	if body.Label == nil && body.NightAllowed == nil && body.DayAlarm == nil &&
		body.NightAlarm == nil && body.DayTimed == nil && body.NightTimed == nil {
		writeErr(w, http.StatusBadRequest, "au moins un champ requis")
		return
	}

	apply := func(e *config.SensorEntry) {
		if body.Label != nil {
			e.Label = *body.Label
		}
		if body.NightAllowed != nil {
			e.NightAllowed = *body.NightAllowed
		}
		if body.DayAlarm != nil {
			e.DayAlarm = body.DayAlarm
		}
		if body.NightAlarm != nil {
			e.NightAlarm = body.NightAlarm
		}
		if body.DayTimed != nil {
			e.DayTimed = body.DayTimed
		}
		if body.NightTimed != nil {
			e.NightTimed = body.NightTimed
		}
	}

	if err := s.store.Update(func(c *config.Config) {
		for i := range c.Sensors {
			if c.Sensors[i].ID == id {
				apply(&c.Sensors[i])
				return
			}
		}
		entry := config.SensorEntry{ID: id}
		apply(&entry)
		c.Sensors = append(c.Sensors, entry)
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "échec sauvegarde: "+err.Error())
		return
	}

	// Push fbxhome ExportLink flags via endpoints_write for fields that changed.
	// charmux backend doesn't expose ExportLinks — flags stay in our config
	// only and are honoured by the local alarm engine.
	if _, isFbx := s.cam.(*camera.FbxhomeClient); isFbx {
		type epWrite struct {
			name string
			val  *bool
		}
		for _, ep := range []epWrite{
			{"day_alarm", body.DayAlarm},
			{"night_alarm", body.NightAlarm},
			{"day_timed", body.DayTimed},
			{"night_timed", body.NightTimed},
		} {
			if ep.val == nil {
				continue
			}
			err := s.cam.EndpointsWrite(r.Context(), id, []camera.EndpointWriteEntry{
				{EPName: ep.name, Value: *ep.val},
			})
			if err != nil {
				s.log.Warn("sensor flag push failed", "sensor", id, "ep", ep.name, "error", err)
			}
		}
	}

	// Re-publish MQTT discovery with new name (only if label was updated).
	if body.Label != nil && s.mqttCB != nil && s.mqttCB.OnSensorRenamed != nil {
		sensor := camera.Sensor{ID: id, Label: *body.Label}
		// Enrich with type info from camera
		if sensors, err := s.listSensors(r.Context()); err == nil {
			for _, cs := range sensors {
				if cs.ID == id {
					sensor.Type = cs.Type
					sensor.TypeName = cs.TypeName
					break
				}
			}
		}
		if err := s.mqttCB.OnSensorRenamed(r.Context(), sensor); err != nil {
			s.log.Warn("mqtt rename discovery failed", "id", id, "error", err)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleOpenStream(w http.ResponseWriter, r *http.Request) {
	info, err := s.cam.OpenStream(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec ouverture flux: "+err.Error())
		return
	}

	// Build SRT URL for the client. Use the request host for the IP.
	host := r.Host
	if i := strings.Index(host, ":"); i >= 0 {
		host = host[:i]
	}
	srtURL := fmt.Sprintf("srt://%s:%d?passphrase=%s&mode=caller", host, info.Port, info.Passphrase)

	writeJSON(w, http.StatusOK, map[string]any{
		"srt_url":    srtURL,
		"passphrase": info.Passphrase,
		"port":       info.Port,
	})
}

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	masked := cfg.MQTT
	if masked.Password != "" {
		masked.Password = "********"
	}
	alarmCommand, alarmState := cfg.AlarmoTopics()
	// camera_mode lets the UI know whether to expose openqiarad-only
	// timings (charmux mode) or hide them because fbxhome owns them.
	cameraMode := "fbxhome"
	if _, ok := s.cam.(*camera.CharmuxClient); ok {
		cameraMode = "charmux"
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mqtt":        masked,
		"homekit":     cfg.HomeKit,
		"admin":       map[string]any{"password_set": cfg.Admin.Password != ""},
		"web":         map[string]any{"enabled": cfg.WebEnabled()},
		"camera_mode": cameraMode,
		"alarm": map[string]any{
			"mode":                  cfg.AlarmMode(),
			"alarmo_command_topic":  alarmCommand,
			"alarmo_state_topic":    alarmState,
			"siren_sounds":          cfg.SirenSoundsMode(),
			"arming_delay_seconds":  int(cfg.ArmingDelay().Seconds()),
			"pending_delay_seconds": int(cfg.PendingDelay().Seconds()),
			"wail_duration_seconds": int(cfg.WailDuration().Seconds()),
		},
	})
}

// handleGetFbxhomeAlarm reads the persisted HlAlarm timings from the
// rotated fbxhome.xml.N files (endpoints_read is ACL-blocked).
func (s *Server) handleGetFbxhomeAlarm(w http.ResponseWriter, r *http.Request) {
	if _, ok := s.cam.(*camera.FbxhomeClient); !ok {
		writeErr(w, http.StatusBadRequest, "indisponible en mode charmux")
		return
	}
	t, err := camera.ReadAlarmTimings()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "lecture XML fbxhome: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, t)
}

// handleUpdateFbxhomeAlarm pushes new HlAlarm timings via endpoints_write
// on node 2. Only fields > 0 are written.
func (s *Server) handleUpdateFbxhomeAlarm(w http.ResponseWriter, r *http.Request) {
	fbx, ok := s.cam.(*camera.FbxhomeClient)
	if !ok {
		writeErr(w, http.StatusBadRequest, "indisponible en mode charmux")
		return
	}
	var body camera.AlarmTimings
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	if err := fbx.WriteAlarmTimings(r.Context(), body); err != nil {
		writeErr(w, http.StatusInternalServerError, "écriture fbxhome: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpdateAlarm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Mode                *string `json:"mode,omitempty"`
		AlarmoCommandTopic  *string `json:"alarmo_command_topic,omitempty"`
		AlarmoStateTopic    *string `json:"alarmo_state_topic,omitempty"`
		SirenSounds         *string `json:"siren_sounds,omitempty"`
		ArmingDelaySeconds  *int    `json:"arming_delay_seconds,omitempty"`
		PendingDelaySeconds *int    `json:"pending_delay_seconds,omitempty"`
		WailDurationSeconds *int    `json:"wail_duration_seconds,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	if body.Mode != nil {
		if *body.Mode != "standalone" && *body.Mode != "alarmo" {
			writeErr(w, http.StatusBadRequest, "mode invalide (standalone ou alarmo)")
			return
		}
	}
	if body.SirenSounds != nil {
		if *body.SirenSounds != "all" && *body.SirenSounds != "alarm_only" && *body.SirenSounds != "none" {
			writeErr(w, http.StatusBadRequest, "siren_sounds invalide (all, alarm_only ou none)")
			return
		}
	}
	// Bornes raisonnables : 0-600s pour les délais alarme, 1-60s pour le wail.
	if body.ArmingDelaySeconds != nil && (*body.ArmingDelaySeconds < 0 || *body.ArmingDelaySeconds > 600) {
		writeErr(w, http.StatusBadRequest, "arming_delay_seconds doit être entre 0 et 600")
		return
	}
	if body.PendingDelaySeconds != nil && (*body.PendingDelaySeconds < 0 || *body.PendingDelaySeconds > 600) {
		writeErr(w, http.StatusBadRequest, "pending_delay_seconds doit être entre 0 et 600")
		return
	}
	if body.WailDurationSeconds != nil && (*body.WailDurationSeconds < 1 || *body.WailDurationSeconds > 60) {
		writeErr(w, http.StatusBadRequest, "wail_duration_seconds doit être entre 1 et 60")
		return
	}
	err := s.store.Update(func(cfg *config.Config) {
		if body.Mode != nil {
			cfg.Alarm.Mode = *body.Mode
		}
		if body.AlarmoCommandTopic != nil {
			cfg.Alarm.AlarmoCommandTopic = *body.AlarmoCommandTopic
		}
		if body.AlarmoStateTopic != nil {
			cfg.Alarm.AlarmoStateTopic = *body.AlarmoStateTopic
		}
		if body.SirenSounds != nil {
			cfg.Alarm.SirenSounds = *body.SirenSounds
		}
		if body.ArmingDelaySeconds != nil {
			cfg.Alarm.ArmingDelaySeconds = *body.ArmingDelaySeconds
		}
		if body.PendingDelaySeconds != nil {
			cfg.Alarm.PendingDelaySeconds = *body.PendingDelaySeconds
		}
		if body.WailDurationSeconds != nil {
			cfg.Alarm.WailDurationSeconds = *body.WailDurationSeconds
		}
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec sauvegarde: "+err.Error())
		return
	}
	// Propage les nouveaux délais au moteur en cours d'exécution. Sans ça,
	// l'engine garde les valeurs initiales chargées au boot et ignore les
	// changements de config jusqu'au prochain restart.
	if body.ArmingDelaySeconds != nil || body.PendingDelaySeconds != nil {
		s.alarmMu.RLock()
		p := s.alarmProvider
		s.alarmMu.RUnlock()
		cfg := s.store.Get()
		if p != nil {
			p.SetTimings(cfg.ArmingDelay(), cfg.PendingDelay())
			s.log.Info("alarm timings updated",
				"arming", cfg.ArmingDelay(),
				"pending", cfg.PendingDelay())
		} else {
			s.log.Warn("alarm timings updated in config but engine not attached",
				"arming", cfg.ArmingDelay(),
				"pending", cfg.PendingDelay())
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpdateWeb(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled *bool `json:"enabled,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	if body.Enabled == nil {
		writeErr(w, http.StatusBadRequest, "champ 'enabled' requis")
		return
	}
	err := s.store.Update(func(cfg *config.Config) {
		cfg.Web.Enabled = body.Enabled
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec sauvegarde: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "restart_required": true})
}

func (s *Server) handleUpdateAdmin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password *string `json:"password,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	if body.Password == nil {
		writeErr(w, http.StatusBadRequest, "champ 'password' requis (vide pour désactiver l'auth)")
		return
	}

	err := s.store.Update(func(cfg *config.Config) {
		cfg.Admin.Password = *body.Password
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec sauvegarde: "+err.Error())
		return
	}

	s.log.Info("admin password updated", "auth_enabled", *body.Password != "")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth_enabled": *body.Password != ""})
}

func (s *Server) handleUpdateMQTT(w http.ResponseWriter, r *http.Request) {
	var body config.MQTTConfig
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}

	err := s.store.Update(func(cfg *config.Config) {
		if body.Broker != "" {
			cfg.MQTT.Broker = body.Broker
		}
		if body.Username != "" {
			cfg.MQTT.Username = body.Username
		}
		if body.Password != "" && body.Password != "********" {
			cfg.MQTT.Password = body.Password
		}
		if body.TopicPrefix != "" {
			cfg.MQTT.TopicPrefix = body.TopicPrefix
		}
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec sauvegarde config: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleUpdateHomeKit(w http.ResponseWriter, r *http.Request) {
	var body config.HomeKitConfig
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}

	err := s.store.Update(func(cfg *config.Config) {
		cfg.HomeKit.Enabled = body.Enabled
		if body.Pin != "" {
			cfg.HomeKit.Pin = body.Pin
		}
		if body.Name != "" {
			cfg.HomeKit.Name = body.Name
		}
		cfg.HomeKit.Camera = body.Camera
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec sauvegarde config: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// --- Alarm & KPD codes ---

func (s *Server) handleGetAlarm(w http.ResponseWriter, r *http.Request) {
	// In alarmo mode, the engine snapshot is stale (Alarmo is the source of
	// truth). Use s.alarmState which is kept in sync by the alarmo/state
	// subscription in main.go.
	if s.store != nil && s.store.Get().AlarmMode() == "alarmo" {
		s.alarmMu.RLock()
		state := s.alarmState
		s.alarmMu.RUnlock()
		writeJSON(w, http.StatusOK, AlarmSnapshot{State: state})
		return
	}

	s.alarmMu.RLock()
	provider := s.alarmProvider
	s.alarmMu.RUnlock()

	if provider != nil {
		writeJSON(w, http.StatusOK, provider.Snapshot())
		return
	}
	// Fallback to the legacy string state.
	s.alarmMu.RLock()
	state := s.alarmState
	s.alarmMu.RUnlock()
	writeJSON(w, http.StatusOK, AlarmSnapshot{State: state})
}

func (s *Server) handleSetAlarm(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}

	// Validate action.
	switch body.Action {
	case "arm_away", "arm_night", "disarm":
		// ok
	default:
		writeErr(w, http.StatusBadRequest, "action invalide (arm_away, arm_night, disarm)")
		return
	}

	// Route via the unified command dispatcher (same path as MQTT/HomeKit).
	// It knows whether to feed the local engine (standalone) or forward to
	// alarmo/command (alarmo mode).
	if s.mqttCB != nil && s.mqttCB.OnAlarmCommand != nil {
		s.mqttCB.OnAlarmCommand(r.Context(), body.Action)
		// Optimistic SSE push so the UI reflects the change immediately. In
		// alarmo mode the real state comes back a few hundred ms later via
		// the alarmo/state subscription, which will correct any mismatch.
		var optimistic string
		switch body.Action {
		case "arm_away":
			optimistic = "armed_away"
		case "arm_night":
			optimistic = "armed_night"
		case "disarm":
			optimistic = "disarmed"
		}
		s.SetAlarmState(optimistic)
		s.PublishEvent("alarm", AlarmSnapshot{State: optimistic})

		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": optimistic})
		return
	}

	s.alarmMu.RLock()
	provider := s.alarmProvider
	s.alarmMu.RUnlock()

	if provider != nil {
		provider.HandleCommand(body.Action)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": provider.Snapshot().State})
		return
	}

	// Legacy fallback: direct state mutation (no engine available).
	var state string
	switch body.Action {
	case "arm_away":
		state = "armed_away"
	case "arm_night":
		state = "armed_night"
	case "disarm":
		state = "disarmed"
	}
	s.alarmMu.Lock()
	s.alarmState = state
	s.alarmMu.Unlock()
	if s.mqttCB != nil && s.mqttCB.OnAlarmStateChanged != nil {
		if err := s.mqttCB.OnAlarmStateChanged(r.Context(), state); err != nil {
			s.log.Warn("mqtt alarm state publish failed", "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "state": state})
}

// findKPDSensor returns the first KPD sensor entry from config.
func (s *Server) findKPDSensor() (int, *config.SensorEntry) {
	cfg := s.store.Get()
	for i, se := range cfg.Sensors {
		if se.Type == "KPD" {
			return i, &cfg.Sensors[i]
		}
	}
	return -1, nil
}

func (s *Server) handleGetCodes(w http.ResponseWriter, r *http.Request) {
	_, kpd := s.findKPDSensor()
	if kpd == nil {
		writeJSON(w, http.StatusOK, map[string]any{"code": nil})
		return
	}
	var code any
	if kpd.KPDCode != "" {
		code = map[string]any{
			"password": kpd.KPDCode,
			"label":    kpd.KPDCodeLabel,
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"code": code})
}

func (s *Server) handleAddCode(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
		Label    string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	for _, c := range body.Password {
		if c < '0' || c > '9' {
			writeErr(w, http.StatusBadRequest, "le code doit être composé de 4 chiffres")
			return
		}
	}
	if len(body.Password) != 4 {
		writeErr(w, http.StatusBadRequest, "le code doit être composé de 4 chiffres")
		return
	}
	if body.Label == "" {
		body.Label = "Code"
	}

	idx, kpd := s.findKPDSensor()
	if kpd == nil {
		writeErr(w, http.StatusNotFound, "aucun clavier (KPD) appairé")
		return
	}

	if err := s.store.Update(func(cfg *config.Config) {
		cfg.Sensors[idx].KPDCode = body.Password
		cfg.Sensors[idx].KPDCodeLabel = body.Label
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "échec sauvegarde: "+err.Error())
		return
	}

	// Update runtime code for next reinit.
	if cc, ok := s.cam.(*camera.CharmuxClient); ok {
		cc.SetKPDCode(kpd.ID, body.Password)
	}
	// In fbxhome mode : push le code via endpoints_write pwd. Si le KPD vient
	// d'être pairé (< 30s), on diffère l'écriture : fbxhome est encore en
	// cours de push bytecode/config, et écrire le code maintenant interromp
	// le cycle radio (KPD se retrouve en boucle f10001 vert post-pair sans
	// jamais finaliser sa config — bug observé 2026-05-14).
	if fc, ok := s.cam.(*camera.FbxhomeClient); ok {
		s.scheduleKPDCodeWrite(fc, kpd.ID, body.Password, body.Label)
	}

	s.log.Info("KPD code updated", "label", body.Label)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// kpdCodePostPairDelay est la fenêtre minimale entre un pairing KPD réussi
// et la première écriture de code PIN. Pendant ce temps fbxhome push le
// bytecode et la config initiale au KPD ; écrire le code trop tôt
// désynchronise le pairing crypto.
const kpdCodePostPairDelay = 30 * time.Second

// scheduleKPDCodeWrite écrit le code PIN immédiatement si on est hors fenêtre
// post-pair, sinon diffère en background. Tout job pendant en cours est
// annulé pour ne garder que la dernière valeur souhaitée.
func (s *Server) scheduleKPDCodeWrite(fc *camera.FbxhomeClient, kpdID int, password, label string) {
	s.kpdPairMu.Lock()
	pairedAt := s.kpdPairedAt
	// Cancel previous pending job to avoid double-write.
	if s.pendingKPDCode != nil {
		close(s.pendingKPDCode.cancel)
		s.pendingKPDCode = nil
	}
	s.kpdPairMu.Unlock()

	wait := time.Until(pairedAt.Add(kpdCodePostPairDelay))
	if wait <= 0 {
		// Hors fenêtre post-pair : écriture immédiate.
		if err := fc.SetKPDPassword(context.Background(), kpdID, password, label); err != nil {
			s.log.Warn("fbxhome SetKPDPassword failed", "error", err)
		}
		return
	}

	// Différer en background. L'API retourne OK avant ce délai.
	job := &pendingKPDCodeJob{password: password, label: label, cancel: make(chan struct{})}
	s.kpdPairMu.Lock()
	s.pendingKPDCode = job
	s.kpdPairMu.Unlock()

	s.log.Info("KPD code write deferred (post-pair bytecode cycle in progress)",
		"wait", wait.Round(time.Second), "kpd_id", kpdID)

	go func() {
		select {
		case <-time.After(wait):
			s.kpdPairMu.Lock()
			current := s.pendingKPDCode
			if current == job {
				s.pendingKPDCode = nil
			}
			s.kpdPairMu.Unlock()
			if current != job {
				return // job remplacé par une nouvelle écriture, abandonner.
			}
			if err := fc.SetKPDPassword(context.Background(), kpdID, password, label); err != nil {
				s.log.Warn("fbxhome SetKPDPassword (deferred) failed", "error", err)
				return
			}
			s.log.Info("KPD code written (deferred)", "label", label, "kpd_id", kpdID)
		case <-job.cancel:
			// Annulé par un nouvel appel scheduleKPDCodeWrite.
		}
	}()
}

func (s *Server) handleDeleteCode(w http.ResponseWriter, r *http.Request) {
	idx, kpd := s.findKPDSensor()
	if kpd == nil {
		writeErr(w, http.StatusNotFound, "aucun clavier (KPD) appairé")
		return
	}

	if err := s.store.Update(func(cfg *config.Config) {
		cfg.Sensors[idx].KPDCode = ""
		cfg.Sensors[idx].KPDCodeLabel = ""
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "échec sauvegarde: "+err.Error())
		return
	}

	if cc, ok := s.cam.(*camera.CharmuxClient); ok {
		cc.SetKPDCode(kpd.ID, "")
	}
	if fc, ok := s.cam.(*camera.FbxhomeClient); ok {
		if err := fc.ClearKPDPassword(r.Context(), kpd.ID); err != nil {
			s.log.Warn("fbxhome ClearKPDPassword failed", "error", err)
		}
	}

	s.log.Info("KPD code deleted")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleShutter(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Open bool `json:"open"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	if err := s.cam.SetShutter(r.Context(), body.Open); err != nil {
		writeErr(w, http.StatusInternalServerError, "échec shutter: "+err.Error())
		return
	}
	s.log.Info("shutter set", "open", body.Open)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "open": body.Open})
}

func (s *Server) handleStartStream(w http.ResponseWriter, r *http.Request) {
	// Open shutter first
	if err := s.cam.SetShutter(r.Context(), true); err != nil {
		s.log.Warn("shutter open failed (continuing)", "error", err)
	}

	// Activate HLS streams
	cmd := exec.Command("fbxbusctl", "call", "hlcamd", "resume_streams")
	if err := cmd.Run(); err != nil {
		writeErr(w, http.StatusInternalServerError, "échec activation flux: "+err.Error())
		return
	}
	s.log.Info("video stream started")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":  true,
		"hls": "/stream/HLS_TEST.m3u8",
		"720": "/stream/720p/HLS_TEST.m3u8",
	})
}

func (s *Server) handleHLSStream(w http.ResponseWriter, r *http.Request) {
	// Serve HLS files from /tmp/out_stream/stream/
	path := r.URL.Path[len("/stream/"):]
	filePath := "/tmp/out_stream/stream/" + path

	// Set correct content types
	switch {
	case strings.HasSuffix(path, ".m3u8"):
		w.Header().Set("Content-Type", "application/vnd.apple.mpegurl")
	case strings.HasSuffix(path, ".m4s"):
		// hlcamd/hls writes MPEG-TS content with .m4s extension
		w.Header().Set("Content-Type", "video/mp2t")
	case strings.HasSuffix(path, ".ts"):
		w.Header().Set("Content-Type", "video/mp2t")
	}
	w.Header().Set("Cache-Control", "no-cache")
	http.ServeFile(w, r, filePath)
}

func (s *Server) handleSirenTest(w http.ResponseWriter, r *http.Request) {
	// Find the first SRN sensor
	sensors, err := s.listSensors(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec liste capteurs: "+err.Error())
		return
	}
	var srnID int
	for _, sensor := range sensors {
		if sensor.Type == "SRN" {
			srnID = sensor.ID
			break
		}
	}
	if srnID == 0 {
		writeErr(w, http.StatusNotFound, "aucune sirène appairée")
		return
	}

	if err := s.cam.TriggerSiren(r.Context(), srnID); err != nil {
		writeErr(w, http.StatusInternalServerError, "échec test sirène: "+err.Error())
		return
	}

	s.log.Info("siren test triggered", "id", srnID)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleSirenAlarmTest déclenche le wail d'alarme intrusion sur la
// première sirène appairée. En mode fbxhome la durée est gérée côté
// firmware ; en mode charmux on respecte WailDuration de la config.
func (s *Server) handleSirenAlarmTest(w http.ResponseWriter, r *http.Request) {
	sensors, err := s.listSensors(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec liste capteurs: "+err.Error())
		return
	}
	var srnID int
	for _, sensor := range sensors {
		if sensor.Type == "SRN" {
			srnID = sensor.ID
			break
		}
	}
	if srnID == 0 {
		writeErr(w, http.StatusNotFound, "aucune sirène appairée")
		return
	}

	wailDur := s.store.Get().WailDuration()
	ctx, cancel := context.WithTimeout(r.Context(), wailDur+5*time.Second)
	defer cancel()

	s.log.Warn("siren alarm test triggered", "id", srnID, "wail_seconds", int(wailDur.Seconds()))
	if err := s.cam.TriggerSirenAlarm(ctx, srnID, wailDur); err != nil {
		writeErr(w, http.StatusInternalServerError, "échec alarm test: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "id": srnID})
}

func (s *Server) handleDebugPKT(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Hex string `json:"hex"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	data, err := hex.DecodeString(body.Hex)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "hex invalide: "+err.Error())
		return
	}
	if err := s.cam.SendPKT(r.Context(), data); err != nil {
		writeErr(w, http.StatusInternalServerError, "send PKT failed: "+err.Error())
		return
	}
	s.log.Info("debug PKT sent", "hex", body.Hex, "len", len(data))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "len": len(data)})
}

// handleDebugSirenSeq envoie un payload hex arbitraire à une SRN, avec
// option handshake 55 0b préalable et stop 55 05 00 84 final. Sert à
// tester de nouveaux opcodes SRN sans rebuild.
//
//	POST /api/debug/siren/sequence
//	{
//	  "addr": 18,                  // SRN addr (défaut 18)
//	  "payload": "01550b00...",    // payload hex à envoyer
//	  "handshake": true,           // envoyer 55 0b avant (défaut true)
//	  "stop": true,                // envoyer 55 05 00 84 après (défaut true)
//	  "hold_ms": 3400              // attente entre payload et stop (défaut 3400)
//	}
func (s *Server) handleDebugSirenSeq(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Addr      int    `json:"addr"`
		Payload   string `json:"payload"`
		Handshake *bool  `json:"handshake"`
		Stop      *bool  `json:"stop"`
		HoldMs    int    `json:"hold_ms"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	if body.Addr == 0 {
		body.Addr = 18
	}
	if body.Payload == "" {
		writeErr(w, http.StatusBadRequest, "payload hex requis")
		return
	}
	payload, err := hex.DecodeString(body.Payload)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "payload hex invalide: "+err.Error())
		return
	}
	withHandshake := true
	if body.Handshake != nil {
		withHandshake = *body.Handshake
	}
	withStop := true
	if body.Stop != nil {
		withStop = *body.Stop
	}
	holdMs := body.HoldMs
	if holdMs == 0 {
		holdMs = 3400
	}

	cc, ok := s.cam.(*camera.CharmuxClient)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "caméra non CharmuxClient")
		return
	}

	// Timeout : handshake (300ms) + hold + stop, marge généreuse.
	timeout := time.Duration(holdMs+2000) * time.Millisecond
	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	if err := cc.SendSirenDebug(ctx, body.Addr, payload, withHandshake, withStop, holdMs); err != nil {
		writeErr(w, http.StatusInternalServerError, "séquence sirène: "+err.Error())
		return
	}

	s.log.Info("siren debug sequence done",
		"addr", body.Addr, "payload", body.Payload,
		"handshake", withHandshake, "stop", withStop, "hold_ms", holdMs)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// maxFbxhomePushBody protège contre un client malveillant qui enverrait un
// body énorme. 64 KiB suffit largement pour la JSON de notifications.
const maxFbxhomePushBody = 64 << 10

// handleFbxhomePush reçoit les events push de hl_event_collectd qui pense
// envoyer au cloud Free. La réponse mime celle du cloud ({"result":"ok"})
// pour qu'il considère la livraison comme réussie et ne retry pas.
//
// Deux routes câblées sur ce handler :
//
//   - POST /events        → body {"events":[{ts,ev:{type,...}}]}
//   - POST /notifications → body {"notifications":[{ts,notif:{type,data}}]}
//
// Les events IV (IntelliVision: détection humain/pet) arrivent sur
// /notifications avec type "iv_event". Les events sensor/alarm/shutter
// arrivent sur /events. Pour l'instant on les loggue tous, et on dispatch
// les iv_event vers s.ivDispatcher si attaché.
//
// CRITIQUE : doit répondre 200 sur les DEUX routes, sinon hl_event_collectd
// met sa queue en retry et ne flush plus rien — on aurait des events qui
// sortent au compte-gouttes.
func (s *Server) handleFbxhomePush(w http.ResponseWriter, r *http.Request) {
	defer func() { _ = r.Body.Close() }()
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxFbxhomePushBody))
	if err != nil {
		s.log.Warn("fbxhome push: read body failed", "err", err)
		// On répond OK quand même pour ne pas faire retry l'expéditeur — le
		// body partiel est dans `body`, ce qui est mieux que rien.
	}

	s.log.Info("fbxhome push received",
		"host", r.Host,
		"path", r.URL.Path,
		"content_type", r.Header.Get("Content-Type"),
		"body_len", len(body),
		"body", string(body),
	)

	// Dispatch selon le path. Pas de fail-fast sur les erreurs de parse :
	// on veut TOUJOURS répondre 200 pour que la queue se vide.
	switch r.URL.Path {
	case "/events":
		if env, perr := hlevents.ParseEvents(body); perr == nil && s.hlDispatcher != nil {
			for _, item := range env.Events {
				s.hlDispatcher.HandleEvent(r.Context(), item)
			}
		}
	case "/notifications":
		if env, perr := hlevents.ParseNotifications(body); perr == nil && s.hlDispatcher != nil {
			for _, item := range env.Notifications {
				s.hlDispatcher.HandleNotification(r.Context(), item)
			}
		}
	}

	// Mime la réponse cloud Free pour que hl_event_collectd flush sa queue.
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Server", "nginx/1.14.2")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write([]byte(`{"result":"ok"}`)); err != nil {
		s.log.Debug("fbxhome push: response write failed", "err", err)
	}
}

func (s *Server) handleReboot(w http.ResponseWriter, r *http.Request) {
	s.log.Info("reboot requested via web UI")
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})

	// Reboot after response is sent
	go func() {
		time.Sleep(500 * time.Millisecond)
		s.log.Info("rebooting camera...")
		cmd := exec.Command("reboot")
		_ = cmd.Run()
	}()
}

// --- JSON helpers ---

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}
