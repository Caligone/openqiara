// Package web provides the embedded web UI and REST API.
package web

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/config"
	"github.com/caligone/openqiara/internal/mqtt"
	"github.com/caligone/openqiara/internal/ota"
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

// Server serves the web UI and REST API for managing OpenQiara.
type Server struct {
	cam       camera.Client
	store     *config.Store
	mqttOK    func() bool
	mqttCB    *MQTTCallbacks
	startTime time.Time
	log       *slog.Logger
	srv       *http.Server
	staticFS  fs.FS
	version   string // build info string ; empty = unknown

	alarmMu    sync.RWMutex
	alarmState string // fallback when no alarm provider is attached

	alarmProvider AlarmProvider

	cachedSensorCount int

	hub      *sseHub
	stopTick chan struct{}

	// debugEnabled active les endpoints /api/debug/* (envoi PKT brut au MCU,
	// séquences sirène arbitraires). Désactivé par défaut — ces endpoints
	// peuvent brick le MCU (opcodes 0x03, 0x08) ou stresser un capteur.
	debugEnabled bool

	// hlcamd permet le lazy healing du pipeline HLS (resume_streams si la
	// playlist n'est pas écrite depuis >maxAge). nil = pas de healing,
	// /api/stream/start fait un resume direct par exec.Command.
	hlcamd *camera.HlcamdResumer
	// hls serves /stream/ (internal/hlsserver).
	hls http.Handler

	// ota expose les endpoints /api/update/*. nil = endpoints renvoient
	// 503 (pas configuré en build local sans -ldflags).
	otaClient    *ota.Client
	otaInstaller *ota.Installer
}

// listSensors returns the paired sensors with their live state.
func (s *Server) listSensors(context.Context) ([]camera.Sensor, error) {
	return s.cam.CachedSensors(), nil
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

// SetHlcamdResumer attache le helper de lazy healing du pipeline HLS.
// Si non appelé, /api/stream/start exec fbxbusctl directement et les
// requêtes GET /stream/ ne tentent aucun resume.
func (s *Server) SetHlcamdResumer(h *camera.HlcamdResumer) {
	s.hlcamd = h
}

// SetHLS serves /stream/ from h (internal/hlsserver) instead of hls's
// files.
func (s *Server) SetHLS(h http.Handler) {
	s.hls = h
}

// SetVersion sets the build version exposed in /api/status. Called from
// main.go with BuildInfo() — empty string is acceptable (= unknown).
func (s *Server) SetVersion(v string) {
	s.version = v
}

// SetOTA attache le client GitHub Releases et l'installer. Si non
// appelé, les endpoints /api/update/* renvoient 503.
func (s *Server) SetOTA(client *ota.Client, installer *ota.Installer) {
	s.otaClient = client
	s.otaInstaller = installer
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
	mux, err := s.routes()
	if err != nil {
		return err
	}

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

// routes construit le mux complet. Séparé de Start pour que les tests
// puissent router de vraies requêtes sans ouvrir de socket.
func (s *Server) routes() (*http.ServeMux, error) {
	mux := http.NewServeMux()

	// --- API v1 ---
	//
	// Deux familles cohabitent sous /api/v1, et la distinction est
	// sémantique, pas cosmétique :
	//
	//   RESSOURCES (/sensors, /config, /alarm, /kpd) — un état qu'on lit et
	//   qu'on écrit. GET est sûr, PUT est idempotent, la réponse décrit
	//   l'état obtenu.
	//
	//   COMMANDES (/commands/*) — un effet matériel déclenché sur le MCU ou
	//   la caméra : reboot, sirène, volet, flux, OTA. Non idempotentes,
	//   asynchrones, sans état propre à lire. Leur 200 signifie ACCEPTÉ,
	//   pas EFFECTUÉ : /commands/reboot répond avant de redémarrer, et
	//   /commands/alarm renvoie un état optimiste que le mode alarmo
	//   corrigera via MQTT quelques centaines de ms plus tard.
	//
	// Ne pas « corriger » les commandes en ressources REST : elles ne
	// modélisent pas un état, elles déclenchent un effet.

	// Ressources — capteurs
	mux.HandleFunc("GET /api/v1/sensors", s.cors(s.handleSensors))
	mux.HandleFunc("POST /api/v1/sensors/pair", s.cors(s.handleStartPairing))
	mux.HandleFunc("GET /api/v1/sensors/pair/{session}", s.cors(s.handlePollPairing))
	mux.HandleFunc("DELETE /api/v1/sensors/pair/{session}", s.cors(s.handleStopPairing))
	mux.HandleFunc("DELETE /api/v1/sensors/{id}", s.cors(s.handleDeleteSensor))
	mux.HandleFunc("PUT /api/v1/sensors/{id}", s.cors(s.handleUpdateSensor))

	// Ressources — codes du clavier. Mono-KPD assumé : s'il y a
	// plusieurs claviers appairés, on répond 409 plutôt que d'en choisir
	// un au hasard (cf. requireSingleKPD). /kpd/code est l'ancienne API à
	// un code, elle agit sur le premier.
	mux.HandleFunc("GET /api/v1/kpd/codes", s.cors(s.handleGetKPDCodes))
	mux.HandleFunc("PUT /api/v1/kpd/codes", s.cors(s.handlePutKPDCodes))
	mux.HandleFunc("GET /api/v1/kpd/code", s.cors(s.handleGetKPDCode))
	mux.HandleFunc("PUT /api/v1/kpd/code", s.cors(s.handleSetKPDCode))
	mux.HandleFunc("DELETE /api/v1/kpd/code", s.cors(s.handleDeleteKPDCode))

	// Ressources — configuration. Chaque sous-ressource a son GET
	// symétrique ; /api/v1/config reste l'agrégat de lecture.
	mux.HandleFunc("GET /api/v1/config", s.cors(s.handleGetConfig))
	mux.HandleFunc("GET /api/v1/config/mqtt", s.cors(s.handleGetMQTT))
	mux.HandleFunc("PUT /api/v1/config/mqtt", s.cors(s.handleUpdateMQTT))
	mux.HandleFunc("GET /api/v1/config/homekit", s.cors(s.handleGetHomeKit))
	mux.HandleFunc("PUT /api/v1/config/homekit", s.cors(s.handleUpdateHomeKit))
	mux.HandleFunc("GET /api/v1/config/admin", s.cors(s.handleGetAdmin))
	mux.HandleFunc("PUT /api/v1/config/admin", s.cors(s.handleUpdateAdmin))
	mux.HandleFunc("DELETE /api/v1/config/admin", s.cors(s.handleDeleteAdmin))
	mux.HandleFunc("GET /api/v1/config/alarm", s.cors(s.handleGetAlarmConfig))
	mux.HandleFunc("PUT /api/v1/config/alarm", s.cors(s.handleUpdateAlarm))
	mux.HandleFunc("GET /api/v1/config/web", s.cors(s.handleGetWeb))
	mux.HandleFunc("PUT /api/v1/config/web", s.cors(s.handleUpdateWeb))

	// Ressources — état courant (lecture seule)
	mux.HandleFunc("GET /api/v1/status", s.cors(s.handleStatus))
	mux.HandleFunc("GET /api/v1/alarm", s.cors(s.handleGetAlarm))
	mux.HandleFunc("GET /api/v1/update", s.cors(s.handleUpdateCheck))
	mux.HandleFunc("GET /api/v1/update/status", s.cors(s.handleUpdateStatus))

	// Commandes — 200 = accepté, pas effectué.
	mux.HandleFunc("POST /api/v1/commands/alarm", s.cors(s.handleSetAlarm))
	mux.HandleFunc("POST /api/v1/commands/reboot", s.cors(s.handleReboot))
	mux.HandleFunc("POST /api/v1/commands/shutter", s.cors(s.handleShutter))
	mux.HandleFunc("POST /api/v1/commands/siren/test", s.cors(s.handleSirenTest))
	mux.HandleFunc("POST /api/v1/commands/siren/alarm_test", s.cors(s.handleSirenAlarmTest))
	mux.HandleFunc("POST /api/v1/commands/stream/start", s.cors(s.handleStartStream))
	mux.HandleFunc("POST /api/v1/commands/update/install", s.cors(s.handleUpdateInstall))
	if s.debugEnabled {
		mux.HandleFunc("POST /api/v1/commands/debug/pkt", s.cors(s.handleDebugPKT))
		mux.HandleFunc("POST /api/v1/commands/debug/siren/sequence", s.cors(s.handleDebugSirenSeq))
	}

	// Flux d'événements (SSE) et segments HLS.
	mux.HandleFunc("GET /api/v1/events", s.cors(s.handleEvents))
	mux.HandleFunc("GET /stream/", s.cors(s.handleHLSStream))

	// CORS preflight
	mux.HandleFunc("OPTIONS /api/", s.handleOptions)

	// Deux filets distincts, à ne pas confondre :
	//
	//   /api/v1/… inconnu → 404. C'est une route absente DANS la version
	//   courante : une faute de frappe, une méthode non supportée, ou une
	//   route pas encore déployée.
	//
	//   /api/… non versionné → 410. La ressource a existé et ne reviendra
	//   pas. Sans ce filet, un vieux client tomberait sur le FileServer et
	//   recevrait l'index.html en 200 — un échec silencieux bien pire.
	//
	// Le pattern /api/v1/ est plus spécifique que /api/, donc ServeMux le
	// choisit en premier. Les confondre ferait répondre « utiliser /api/v1 »
	// à un client déjà sur /api/v1, et surtout 410 est terminal : un cache
	// est en droit de le mémoriser définitivement, ce qui condamnerait toute
	// route v1 ajoutée plus tard.
	//
	// Enregistrés par méthode et non en pattern nu : un pattern sans méthode
	// entre en conflit avec le `GET /` du FileServer (ServeMux panique au
	// montage, "matches fewer methods but has a more general path pattern").
	// HEAD est absent pour la même raison — ServeMux le traite comme un
	// sous-ensemble de GET, donc `HEAD /api/` entrerait en conflit avec
	// chaque `GET /api/v1/…`. Un HEAD est donc servi par le `GET /api/…`
	// correspondant, sans corps de réponse : sans conséquence.
	for _, m := range []string{"GET", "POST", "PUT", "DELETE", "PATCH"} {
		mux.HandleFunc(m+" /api/v1/", s.cors(s.handleNotFoundV1))
		mux.HandleFunc(m+" /api/", s.cors(s.handleGoneUnversioned))
	}

	// Static files (SPA)
	staticSub, err := fs.Sub(s.staticFS, "static")
	if err != nil {
		return nil, fmt.Errorf("embed sub: %w", err)
	}
	fileServer := http.FileServer(http.FS(staticSub))
	mux.Handle("GET /", fileServer)

	return mux, nil
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
		"version":        s.version,
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

		// Pas d'auth configurée → accès ouvert.
		if !cfg.Admin.AuthEnabled() {
			next.ServeHTTP(w, r)
			return
		}

		// Allow CORS preflight without auth (browsers don't send credentials on OPTIONS).
		if r.Method == http.MethodOptions {
			next.ServeHTTP(w, r)
			return
		}

		user, pass, ok := r.BasicAuth()
		if !ok || user != "admin" || !cfg.Admin.CheckPassword(pass) {
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
		"version":        s.version,
	})
}

func (s *Server) handleSensors(w http.ResponseWriter, r *http.Request) {
	sensors, err := s.listSensors(r.Context())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "impossible de lister les capteurs: "+err.Error())
		return
	}

	type sensorWithMeta struct {
		camera.Sensor
		Label        string `json:"label"`
		NightAllowed bool   `json:"night_allowed"`
		Instant      bool   `json:"instant"`
	}
	entries := make(map[int]config.SensorEntry)
	for _, se := range s.store.Get().Sensors {
		entries[se.ID] = se
	}
	result := make([]sensorWithMeta, 0, len(sensors))
	for _, sensor := range sensors {
		se := entries[sensor.ID]
		result = append(result, sensorWithMeta{Sensor: sensor, Label: se.Label, NightAllowed: se.NightAllowed, Instant: se.Instant})
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleStartPairing(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Type string `json:"type"`
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
	session, err := s.cam.StartPairing(r.Context(), body.Type)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec démarrage appairage: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{"session": session})
}

func (s *Server) handlePollPairing(w http.ResponseWriter, r *http.Request) {
	session, err := strconv.Atoi(r.PathValue("session"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "session d'appairage invalide")
		return
	}

	sensor, done, err := s.cam.PollPairing(r.Context(), session)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "erreur polling appairage: "+err.Error())
		return
	}

	// The radio client saved the paired sensor in the config already.
	if done && sensor != nil {
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
	session, err := strconv.Atoi(r.PathValue("session"))
	if err != nil {
		writeErr(w, http.StatusBadRequest, "session d'appairage invalide")
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
	// `pair` est un segment littéral du même niveau que {id} : une requête
	// sur /sensors/pair sans session atterrit ici. Répondre « ID capteur
	// invalide » enverrait le client sur une fausse piste.
	if idStr == "pair" {
		writeErr(w, http.StatusNotFound, "session d'appairage manquante (/sensors/pair/{session})")
		return
	}
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

// handleUpdateSensor met à jour les métadonnées d'un capteur : libellé et
// flags de comportement alarme. Le nom d'origine (handleRenameSensor) ne
// décrivait plus que le premier des six champs acceptés.
func (s *Server) handleUpdateSensor(w http.ResponseWriter, r *http.Request) {
	idStr := r.PathValue("id")
	if idStr == "pair" {
		writeErr(w, http.StatusNotFound, "route inconnue (/sensors/pair n'accepte que POST et DELETE)")
		return
	}
	id, err := strconv.Atoi(idStr)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "ID capteur invalide")
		return
	}

	var body struct {
		Label        *string `json:"label,omitempty"`
		NightAllowed *bool   `json:"night_allowed,omitempty"`
		Instant      *bool   `json:"instant,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	if body.Label != nil {
		trimmed := strings.TrimSpace(*body.Label)
		body.Label = &trimmed
	}
	if body.Label == nil && body.NightAllowed == nil && body.Instant == nil {
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
		if body.Instant != nil {
			e.Instant = *body.Instant
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

// maskedSecret remplace tout secret renvoyé en lecture. Le handler d'écriture
// le reconnaît et laisse alors la valeur stockée intacte, ce qui permet à
// l'UI de renvoyer la config telle qu'elle l'a reçue sans effacer le mot de
// passe.
const maskedSecret = "********"

func (s *Server) handleGetConfig(w http.ResponseWriter, r *http.Request) {
	cfg := s.store.Get()
	masked := cfg.MQTT
	if masked.Password != "" {
		masked.Password = maskedSecret
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"mqtt":    masked,
		"homekit": maskHomeKit(cfg.HomeKit),
		"admin":   map[string]any{"password_set": cfg.Admin.AuthEnabled()},
		"web":     map[string]any{"enabled": cfg.WebEnabled()},
		"alarm":   s.alarmConfigPayload(),
	})
}

// handleNotFoundV1 répond aux routes /api/v1/* qui n'existent pas dans cette
// version : faute de frappe, méthode non supportée, ou route pas encore
// déployée. 404 et non 410 — la ressource n'a pas disparu, elle n'a jamais
// existé ici, et un client a le droit de réessayer après une mise à jour.
func (s *Server) handleNotFoundV1(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusNotFound, map[string]any{
		"error":  "route inconnue",
		"path":   r.URL.Path,
		"method": r.Method,
		"see":    "docs/api.md",
	})
}

// handleGoneUnversioned répond aux routes /api/* non versionnées, retirées au
// profit de /api/v1. 410 plutôt que de laisser le FileServer servir
// l'index.html en 200, ce qu'un vieux client parserait comme une réponse
// valide.
func (s *Server) handleGoneUnversioned(w http.ResponseWriter, r *http.Request) {
	// Pas de chemin de remplacement calculé : il serait faux pour les routes
	// déplacées (/api/alarm → /api/v1/commands/alarm, /api/codes →
	// /api/v1/kpd/code). La table de correspondance est dans docs/api.md.
	writeJSON(w, http.StatusGone, map[string]any{
		"error":   "API non versionnée supprimée, voir /api/v1",
		"path":    r.URL.Path,
		"see":     "docs/api.md",
		"version": "v1",
	})
}

func (s *Server) handleGetMQTT(w http.ResponseWriter, r *http.Request) {
	masked := s.store.Get().MQTT
	if masked.Password != "" {
		masked.Password = maskedSecret
	}
	writeJSON(w, http.StatusOK, masked)
}

func (s *Server) handleGetHomeKit(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, maskHomeKit(s.store.Get().HomeKit))
}

// maskHomeKit masque le setup code avant toute lecture. C'est un secret
// d'appairage au même titre que le mot de passe MQTT : qui le lit peut
// rattacher un contrôleur HomeKit, donc piloter l'alarme. Renvoyer la
// valeur masquée telle quelle en écriture laisse le PIN stocké intact.
func maskHomeKit(hk config.HomeKitConfig) config.HomeKitConfig {
	if hk.Pin != "" {
		hk.Pin = maskedSecret
	}
	return hk
}

// handleGetAdmin n'expose jamais le hash : seulement si l'auth est active.
func (s *Server) handleGetAdmin(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{
		"password_set": s.store.Get().Admin.AuthEnabled(),
	})
}

func (s *Server) handleGetWeb(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"enabled": s.store.Get().WebEnabled()})
}

func (s *Server) handleGetAlarmConfig(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, s.alarmConfigPayload())
}

// alarmConfigPayload construit la vue publique de la config alarme. Partagée
// entre GET /api/v1/config (agrégat) et GET /api/v1/config/alarm pour que les
// deux ne puissent pas diverger.
func (s *Server) alarmConfigPayload() map[string]any {
	cfg := s.store.Get()
	alarmCommand, alarmState := cfg.AlarmoTopics()
	return map[string]any{
		"mode":                  cfg.AlarmMode(),
		"alarmo_command_topic":  alarmCommand,
		"alarmo_state_topic":    alarmState,
		"siren_sounds":          cfg.SirenSoundsMode(),
		"arming_delay_seconds":  int(cfg.ArmingDelay().Seconds()),
		"pending_delay_seconds": int(cfg.PendingDelay().Seconds()),
		"wail_duration_seconds": int(cfg.WailDuration().Seconds()),
	}
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
	// Bornes de la sirène, qui compte les délais sur un octet (secondes)
	// et l'alarme par pas de 2 s.
	if body.ArmingDelaySeconds != nil && (*body.ArmingDelaySeconds < 0 || *body.ArmingDelaySeconds > 255) {
		writeErr(w, http.StatusBadRequest, "arming_delay_seconds doit être entre 0 et 255")
		return
	}
	if body.PendingDelaySeconds != nil && (*body.PendingDelaySeconds < 0 || *body.PendingDelaySeconds > 255) {
		writeErr(w, http.StatusBadRequest, "pending_delay_seconds doit être entre 0 et 255")
		return
	}
	if body.WailDurationSeconds != nil && (*body.WailDurationSeconds < 1 || *body.WailDurationSeconds > 510) {
		writeErr(w, http.StatusBadRequest, "wail_duration_seconds doit être entre 1 et 510")
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

// handleUpdateAdmin définit le mot de passe admin. Désactiver l'auth passe
// par DELETE : une chaîne vide ici est refusée, pour qu'aucun client ne
// puisse ouvrir l'API par accident en envoyant un champ vide (#45).
func (s *Server) handleUpdateAdmin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Password string `json:"password"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	// Un mot de passe trop court donnerait une fausse impression de
	// protection sur une UI qui pilote une alarme.
	if len(body.Password) < minAdminPasswordLen {
		writeErr(w, http.StatusBadRequest,
			fmt.Sprintf("mot de passe trop court (%d caractères minimum) — DELETE pour désactiver l'auth", minAdminPasswordLen))
		return
	}

	hash, err := config.HashAdminPassword(body.Password)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec hash: "+err.Error())
		return
	}
	s.setAdminPasswordHash(w, hash)
}

// handleDeleteAdmin désactive l'authentification : l'API devient ouverte.
func (s *Server) handleDeleteAdmin(w http.ResponseWriter, r *http.Request) {
	s.setAdminPasswordHash(w, "")
}

func (s *Server) setAdminPasswordHash(w http.ResponseWriter, hash string) {
	err := s.store.Update(func(cfg *config.Config) {
		cfg.Admin.PasswordHash = hash
		// Vide systématiquement le champ legacy : on ne veut surtout pas
		// laisser l'ancien clair traîner.
		cfg.Admin.Password = ""
	})
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "échec sauvegarde: "+err.Error())
		return
	}

	authEnabled := hash != ""
	s.log.Info("admin password updated", "auth_enabled", authEnabled)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "auth_enabled": authEnabled})
}

func (s *Server) handleUpdateMQTT(w http.ResponseWriter, r *http.Request) {
	// TLS is an advanced, file-only setting: the tls_* fields are deliberately
	// absent here so this handler can never touch them. It leaves the
	// file-configured TLS setup untouched (no partial-PUT wipe possible) and
	// keeps certificate paths out of reach of the web API.
	var body struct {
		Broker      string `json:"broker"`
		Username    string `json:"username"`
		Password    string `json:"password"`
		TopicPrefix string `json:"topic_prefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	if err := validateBrokerURL(body.Broker); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	// With file-configured TLS, a plaintext broker would make the MQTT
	// publisher fail at next boot: refuse it here, where the user sees it.
	if body.Broker != "" && s.store.Get().MQTT.HasTLS() && !mqtt.IsTLSScheme(body.Broker) {
		writeErr(w, http.StatusBadRequest, "broker invalide : TLS configuré par fichier, schéma TLS requis (ssl://, mqtts://…)")
		return
	}

	err := s.store.Update(func(cfg *config.Config) {
		if body.Broker != "" {
			cfg.MQTT.Broker = body.Broker
		}
		if body.Username != "" {
			cfg.MQTT.Username = body.Username
		}
		if body.Password != "" && body.Password != maskedSecret {
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

	// Broker settings are only read at boot; a reboot is required to apply them.
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "reboot_required": true})
}

func (s *Server) handleUpdateHomeKit(w http.ResponseWriter, r *http.Request) {
	var body config.HomeKitConfig
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	if err := validateHomeKitPin(body.Pin); err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}

	err := s.store.Update(func(cfg *config.Config) {
		cfg.HomeKit.Enabled = body.Enabled
		// La valeur masquée renvoyée en lecture ne doit jamais devenir le
		// PIN réel : l'UI relit la config puis la réécrit telle quelle.
		if body.Pin != "" && body.Pin != maskedSecret {
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

// --- Validation helpers ---

// minAdminPasswordLen est la longueur minimale du mot de passe admin quand
// l'auth est activée. Volontairement bas : l'UI n'est pas exposée à
// Internet, et un seuil trop haut pousserait à désactiver l'auth.
const minAdminPasswordLen = 8

// validateBrokerURL vérifie qu'un broker MQTT est d'une forme exploitable.
// Vide = champ non modifié (les handlers config ignorent les chaînes vides),
// donc accepté ici. On n'impose pas de schéma (paho accepte tcp://, ssl://,
// ws://, wss:// et host:port nu) mais on rejette ce qui ne pourra jamais
// se connecter : espaces internes, absence d'hôte.
func validateBrokerURL(broker string) error {
	if broker == "" {
		return nil
	}
	if strings.ContainsAny(broker, " \t\r\n") {
		return fmt.Errorf("broker invalide : espaces non autorisés")
	}
	hostPort := broker
	if i := strings.Index(hostPort, "://"); i >= 0 {
		scheme := hostPort[:i]
		switch scheme {
		case "tcp", "ssl", "tls", "mqtt", "mqtts", "ws", "wss":
		default:
			return fmt.Errorf("schéma broker invalide : %q (tcp, ssl, mqtt, ws, wss)", scheme)
		}
		hostPort = hostPort[i+3:]
	}
	if hostPort == "" {
		return fmt.Errorf("broker invalide : hôte manquant")
	}
	// Port explicite → doit être numérique et dans les bornes. SplitHostPort
	// gère IPv6 entre crochets ; une erreur signifie "pas de port", ce qui
	// est valide (paho applique 1883 par défaut).
	if host, port, err := net.SplitHostPort(hostPort); err == nil {
		if host == "" {
			return fmt.Errorf("broker invalide : hôte manquant")
		}
		n, perr := strconv.Atoi(port)
		if perr != nil || n < 1 || n > 65535 {
			return fmt.Errorf("port broker invalide : %q", port)
		}
	}
	return nil
}

// validateHomeKitPin vérifie le setup code HomeKit : exactement 8 chiffres.
// Vide = inchangé (le handler ne réécrit pas un Pin vide) et le défaut
// 00102003 est appliqué plus bas par le publisher. Apple interdit quelques
// codes triviaux — les accepter produirait un accessoire qu'iOS refuse
// d'appairer, donc autant le dire ici.
func validateHomeKitPin(pin string) error {
	// Vide = inchangé ; masqué = l'UI a relu puis réécrit la config sans
	// toucher au PIN. Les deux laissent la valeur stockée intacte.
	if pin == "" || pin == maskedSecret {
		return nil
	}
	if len(pin) != 8 {
		return fmt.Errorf("pin HomeKit invalide : 8 chiffres attendus")
	}
	for _, c := range pin {
		if c < '0' || c > '9' {
			return fmt.Errorf("pin HomeKit invalide : chiffres uniquement")
		}
	}
	switch pin {
	case "00000000", "11111111", "22222222", "33333333", "44444444",
		"55555555", "66666666", "77777777", "88888888", "99999999",
		"12345678", "87654321":
		return fmt.Errorf("pin HomeKit interdit par Apple (code trivial)")
	}
	return nil
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

// errNoKPD / errMultipleKPD qualifient les deux échecs de résolution du
// clavier. Le second existe parce que l'ancien findKPDSensor prenait
// silencieusement le premier KPD du slice : avec deux claviers appairés,
// l'écriture du code partait vers l'un ou l'autre selon l'ordre de
// persistance, sans que l'utilisateur puisse le savoir.
var (
	errNoKPD       = fmt.Errorf("aucun clavier (KPD) appairé")
	errMultipleKPD = fmt.Errorf("plusieurs claviers appairés, ciblage requis")
)

// requireSingleKPD résout LE clavier de l'installation. L'API assume un
// KPD unique : s'il y en a plusieurs, on refuse explicitement au lieu de
// choisir.
func (s *Server) requireSingleKPD() (*config.SensorEntry, error) {
	cfg := s.store.Get()
	idx := -1
	for i, se := range cfg.Sensors {
		if se.Type != "KPD" {
			continue
		}
		if idx >= 0 {
			return nil, errMultipleKPD
		}
		idx = i
	}
	if idx < 0 {
		return nil, errNoKPD
	}
	return &cfg.Sensors[idx], nil
}

// writeKPDErr traduit une erreur de résolution clavier en réponse HTTP :
// 404 quand il n'y en a aucun, 409 quand le ciblage est ambigu.
func writeKPDErr(w http.ResponseWriter, err error) {
	if err == errMultipleKPD {
		writeErr(w, http.StatusConflict, err.Error())
		return
	}
	writeErr(w, http.StatusNotFound, err.Error())
}

// maxKPDCodes borne la liste de codes d'un clavier. Ce n'est pas une
// limite connue du clavier (deux codes vérifiés sur le matériel), mais une
// marge prudente : la liste entière part dans une trame radio à chaque
// réveil du clavier.
const maxKPDCodes = 10

// validateKPDCodes vérifie une liste de codes avant de l'enregistrer :
// 4 chiffres chacun (en désarmant, le clavier ne remonte que 4 chiffres),
// pas de doublon, au plus maxKPDCodes. Elle renvoie la liste aux libellés
// nettoyés de leurs espaces.
func validateKPDCodes(codes []config.KPDCode) ([]config.KPDCode, error) {
	if len(codes) > maxKPDCodes {
		return nil, fmt.Errorf("au plus %d codes par clavier", maxKPDCodes)
	}
	out := make([]config.KPDCode, 0, len(codes))
	for _, c := range codes {
		if len(c.Password) != 4 || strings.Trim(c.Password, "0123456789") != "" {
			return nil, errors.New("le code doit être composé de 4 chiffres")
		}
		if slices.ContainsFunc(out, func(o config.KPDCode) bool { return o.Password == c.Password }) {
			return nil, errors.New("deux codes identiques")
		}
		out = append(out, config.KPDCode{Password: c.Password, Label: strings.TrimSpace(c.Label)})
	}
	return out, nil
}

// updateKPDCodes applique edit à la liste de codes du clavier, la valide,
// l'enregistre, et la fait partir au prochain réveil du clavier.
func (s *Server) updateKPDCodes(w http.ResponseWriter, edit func([]config.KPDCode) []config.KPDCode) {
	kpd, err := s.requireSingleKPD()
	if err != nil {
		writeKPDErr(w, err)
		return
	}
	codes, err := validateKPDCodes(edit(kpd.KPDCodes))
	if err != nil {
		writeErr(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := s.store.Update(func(cfg *config.Config) {
		if i := slices.IndexFunc(cfg.Sensors, func(se config.SensorEntry) bool { return se.ID == kpd.ID }); i >= 0 {
			cfg.Sensors[i].KPDCodes = codes
		}
	}); err != nil {
		writeErr(w, http.StatusInternalServerError, "échec sauvegarde: "+err.Error())
		return
	}

	// Radio backend: the codes go out at the keypad's next wake.
	if rc, ok := s.cam.(*camera.RadioClient); ok {
		rc.Reload()
	}

	s.log.Info("KPD codes updated", "count", len(codes))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handleGetKPDCodes lit les codes du clavier, en clair : l'UI les affiche
// pour que l'utilisateur les retrouve. 404 si aucun clavier n'est appairé,
// liste vide pour un clavier sans code.
func (s *Server) handleGetKPDCodes(w http.ResponseWriter, r *http.Request) {
	kpd, err := s.requireSingleKPD()
	if err != nil {
		writeKPDErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sensor_id": kpd.ID,
		"codes":     append([]config.KPDCode{}, kpd.KPDCodes...),
	})
}

// handlePutKPDCodes remplace la liste entière : un client la relit, la
// modifie et la renvoie. Une liste vide est acceptée ; le clavier n'est
// alors plus servi, car sans code son bouton OFF suffirait à désarmer.
func (s *Server) handlePutKPDCodes(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Codes []config.KPDCode `json:"codes"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	s.updateKPDCodes(w, func([]config.KPDCode) []config.KPDCode { return body.Codes })
}

// handleGetKPDCode, handleSetKPDCode et handleDeleteKPDCode sont l'API
// d'avant /kpd/codes, quand le clavier n'avait qu'un code. Gardées pour les
// clients existants, elles agissent sur le premier code de la liste.
func (s *Server) handleGetKPDCode(w http.ResponseWriter, r *http.Request) {
	kpd, err := s.requireSingleKPD()
	if err != nil {
		writeKPDErr(w, err)
		return
	}
	if len(kpd.KPDCodes) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"sensor_id": kpd.ID, "code": nil})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"sensor_id": kpd.ID,
		"code": map[string]any{
			"password": kpd.KPDCodes[0].Password,
			"label":    kpd.KPDCodes[0].Label,
		},
	})
}

func (s *Server) handleSetKPDCode(w http.ResponseWriter, r *http.Request) {
	var body config.KPDCode
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeErr(w, http.StatusBadRequest, "JSON invalide")
		return
	}
	if body.Label == "" {
		body.Label = "Code"
	}
	s.updateKPDCodes(w, func(codes []config.KPDCode) []config.KPDCode {
		if len(codes) == 0 {
			return []config.KPDCode{body}
		}
		return append([]config.KPDCode{body}, codes[1:]...)
	})
}

func (s *Server) handleDeleteKPDCode(w http.ResponseWriter, r *http.Request) {
	s.updateKPDCodes(w, func(codes []config.KPDCode) []config.KPDCode {
		if len(codes) == 0 {
			return nil
		}
		return codes[1:]
	})
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

	// Wake hlcamd now (cooldown + log in the resumer): the viewer asked.
	if s.hlcamd != nil {
		if err := s.hlcamd.ForceResume(r.Context()); err != nil {
			// Skipped (cooldown/inflight) n'est pas une erreur fatale —
			// hlcamd a probablement déjà été réveillé il y a <Xs.
			s.log.Debug("resume skipped on stream start", "error", err)
		}
	}
	s.log.Info("video stream started")
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":  true,
		"hls": "/stream/HLS_TEST.m3u8",
		"720": "/stream/720p/HLS_TEST.m3u8",
	})
}

// handleHLSStream serves the camera as HLS (internal/hlsserver).
func (s *Server) handleHLSStream(w http.ResponseWriter, r *http.Request) {
	if s.hls == nil {
		writeErr(w, http.StatusNotFound, "flux vidéo non configuré")
		return
	}
	s.hls.ServeHTTP(w, r)
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
// première sirène appairée, pour la durée WailDuration de la config.
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
//	  "addr": 6,                   // adresse radio de la SRN (requis)
//	  "payload": "5505010a28",     // payload applicatif hex, à partir du 55
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
	if body.Addr <= 0 {
		writeErr(w, http.StatusBadRequest, "addr requis (adresse radio de la sirène)")
		return
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

	rc, ok := s.cam.(*camera.RadioClient)
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "passerelle radio indisponible")
		return
	}

	// Timeout : handshake (300ms) + hold + stop, marge généreuse.
	timeout := time.Duration(holdMs+2000) * time.Millisecond
	if timeout < 10*time.Second {
		timeout = 10 * time.Second
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()

	if err := rc.SendSirenDebug(ctx, uint32(body.Addr), payload, withHandshake, withStop, time.Duration(holdMs)*time.Millisecond); err != nil {
		writeErr(w, http.StatusInternalServerError, "séquence sirène: "+err.Error())
		return
	}

	s.log.Info("siren debug sequence done",
		"addr", body.Addr, "payload", body.Payload,
		"handshake", withHandshake, "stop", withStop, "hold_ms", holdMs)
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
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
