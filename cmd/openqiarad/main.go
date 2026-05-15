package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/caligone/openqiara/internal/alarm"
	"github.com/caligone/openqiara/internal/camera"
	"github.com/caligone/openqiara/internal/charmux"
	"github.com/caligone/openqiara/internal/config"
	"github.com/caligone/openqiara/internal/fbxhomelog"
	"github.com/caligone/openqiara/internal/hlevents"
	"github.com/caligone/openqiara/internal/mdns"
	"github.com/caligone/openqiara/internal/mqtt"
	"github.com/caligone/openqiara/internal/ota"
	"github.com/caligone/openqiara/internal/publisher"
	"github.com/caligone/openqiara/internal/web"
	staticweb "github.com/caligone/openqiara/web"
)

// Build-time variables injected via -ldflags. Stay as "dev" / "" when
// building locally without the release workflow.
//
//	go build -ldflags="-X main.version=v0.1.0-alpha.1 -X main.commit=abc1234 -X main.date=2026-05-15"
var (
	version = "dev"
	commit  = ""
	date    = ""
)

// BuildInfo returns the version string shown in /api/status and the
// -version output. Always non-empty.
func BuildInfo() string {
	s := version
	if commit != "" {
		s += " (" + commit
		if date != "" {
			s += ", " + date
		}
		s += ")"
	}
	return s
}

func main() {
	configPath := flag.String("config", "/data/openqiara.json", "path to config file")
	// poll : intervalle de sécurité. Les events temps réel arrivent désormais
	// via internal/fbxhomelog (tail du log fbxhome). Le polling sert juste
	// de fallback pour rafraîchir battery/temperature/reachable et rattraper
	// les events manqués pendant une rotation du log. 5min suffisent.
	//
	// Historique : 2s → saturation fbxhome (502 cascade, KPD instable post
	// batterie). 30s → safe mais events sensors latents. Tail → latence
	// sub-seconde sans pression sur fbxhome.
	pollInterval := flag.Duration("poll", 5*time.Minute, "sensor poll interval (fbxhome backend, fallback only)")
	webAddr := flag.String("web", ":80", "web UI listen address")
	mode := flag.String("mode", "auto", "backend mode: fbxhome, charmux, or auto")
	debugAPI := flag.Bool("debug", false, "enable /api/debug/* endpoints (PKT raw, siren raw — can brick the MCU)")
	showVersion := flag.Bool("version", false, "print version and exit")
	flag.Parse()

	if *showVersion {
		fmt.Println(BuildInfo())
		os.Exit(0)
	}

	logger := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)
	logger.Info("openqiarad starting", "version", BuildInfo(), "config", *configPath, "poll_interval", *pollInterval, "mode", *mode)

	store := config.NewStore(*configPath)
	if err := store.Load(); err != nil {
		logger.Error("failed to load config", "error", err)
		os.Exit(1)
	}
	cfg := store.Get()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// Build known sensor types from config for charmux UNKNOWN resolution
	knownTypes := make(map[int]string, len(cfg.Sensors))
	kpdCodes := make(map[int]string)
	for _, se := range cfg.Sensors {
		if se.Type != "" {
			knownTypes[se.ID] = se.Type
		}
		if se.KPDCode != "" {
			kpdCodes[se.ID] = se.KPDCode
		}
	}
	// Build deleted IDs set from config
	deletedIDs := make(map[int]bool, len(cfg.DeletedIDs))
	for _, id := range cfg.DeletedIDs {
		deletedIDs[id] = true
	}

	// Create camera client based on mode
	cam := createCamera(ctx, *mode, knownTypes, deletedIDs, kpdCodes, logger)
	if cam == nil {
		logger.Error("no camera backend available")
		os.Exit(1)
	}
	defer func() { _ = cam.Close() }()

	sensors, err := buildSensorList(ctx, cam, store, logger)
	if err != nil {
		logger.Warn("failed to list sensors from MCU", "error", err)
	}
	logger.Info("sensors loaded", "count", len(sensors))
	for _, s := range sensors {
		logger.Info("sensor", "id", s.ID, "type", s.Type, "reachable", s.Reachable)
	}

	// Publishers (MQTT and/or HomeKit)
	var pubs []publisher.Publisher

	// alarmState est lu/écrit par 5 goroutines (handlers MQTT, alarm engine,
	// SSE callback, alarmo subscribe). atomic.Pointer évite la data race
	// constatée par -race avant 2026-05-15. Helpers définis pour ne pas
	// répéter le boilerplate Load()/Store() partout.
	var alarmStateBox atomic.Pointer[string]
	setAlarmState := func(s string) { alarmStateBox.Store(&s) }
	getAlarmState := func() string {
		if p := alarmStateBox.Load(); p != nil {
			return *p
		}
		return ""
	}
	setAlarmState("disarmed")

	// Command handler shared by all publishers
	cmds := &publisher.CommandHandler{
		OnAlarmCommand: func(state string) {
			logger.Info("alarm command", "state", state)
			setAlarmState(state)
			webSrvSetAlarm(state, pubs, ctx, logger)
		},
		OnShutterCommand: func(open bool) {
			logger.Info("shutter command from HomeKit", "open", open)
			if err := cam.SetShutter(ctx, open); err != nil {
				logger.Error("shutter command failed", "error", err)
			}
		},
		OnSirenCommand: func(sensorID int, on bool) {
			// Délégué à l'interface cam.Client (fonctionne en fbxhome ET charmux).
			if on {
				logger.Info("siren command: wail", "sensor_id", sensorID)
				if err := cam.TriggerSirenAlarm(ctx, sensorID, store.Get().WailDuration()); err != nil {
					logger.Error("siren command failed", "error", err)
				}
			} else {
				logger.Info("siren command: stop", "sensor_id", sensorID)
				if err := cam.StopSiren(ctx, sensorID); err != nil {
					logger.Error("siren stop failed", "error", err)
				}
			}
		},
	}

	// MQTT publisher
	var mqttConnected bool
	var mqttPub *publisher.MQTTPublisher
	if cfg.MQTT.Broker != "" {
		mqttCfg := mqtt.Config{
			Broker:      cfg.MQTT.Broker,
			Username:    cfg.MQTT.Username,
			Password:    cfg.MQTT.Password,
			TopicPrefix: cfg.MQTT.TopicPrefix,
		}
		if mqttCfg.TopicPrefix == "" {
			mqttCfg.TopicPrefix = "openqiara"
		}
		mqttPub = publisher.NewMQTTPublisher(mqttCfg, logger)
		// In alarmo mode, don't publish our own alarm_control_panel entity —
		// Alarmo is the source of truth for the alarm state in HA.
		mqttPub.PublishAlarmEntity = cfg.AlarmMode() == "standalone"
		if err := mqttPub.Start(ctx, sensors, cmds); err != nil {
			logger.Error("failed to start MQTT publisher", "error", err)
		} else {
			pubs = append(pubs, mqttPub)
			mqttConnected = true
			logger.Info("MQTT publisher started", "broker", cfg.MQTT.Broker)

			// Setup shutter command handler
			mqttPub.HAPublisher().SetupShutterCommandHandler(func(open bool) {
				if err := cam.SetShutter(ctx, open); err != nil {
					logger.Warn("shutter command failed", "error", err)
				} else {
					if err := mqttPub.HAPublisher().PublishShutterState(ctx, open); err != nil {
						logger.Warn("publish shutter state failed", "error", err)
					}
				}
			}, logger)

			// Publish initial sensor states
			for _, s := range sensors {
				if s.Type == "KPD" {
					continue
				}
				updated, err := cam.ReadSensor(ctx, s.ID, []string{"state", "temperature", "battery"})
				if err != nil {
					// Expected at boot for sensors that haven't yet emitted a
					// PKT event since startup — c.sensors is populated lazily
					// from PKT events. Log at debug to avoid noisy WARNs.
					logger.Debug("initial read skipped (no live state yet)", "id", s.ID, "error", err)
					continue
				}
				updated.TypeName = s.TypeName
				updated.ItemID = s.ItemID
				updated.Type = s.Type
				updated.Reachable = s.Reachable
				if err := mqttPub.PublishSensorState(ctx, *updated); err != nil {
					logger.Warn("publish initial sensor state failed", "id", s.ID, "error", err)
				}
			}
		}
	} else {
		logger.Warn("no MQTT broker configured")
	}

	// HomeKit publisher
	var hkPub *publisher.HomeKitPublisher
	if cfg.HomeKit.Enabled {
		hkPub = publisher.NewHomeKitPublisher(publisher.HomeKitConfig{
			Pin:  cfg.HomeKit.Pin,
			Name: cfg.HomeKit.Name,
			// Don't expose our SecuritySystem when Alarmo already exposes one
			// via HA's HomeKit integration — avoids showing 2 alarm panels.
			ExposeAlarm: cfg.AlarmMode() != "alarmo",
			Camera: publisher.CameraConfig{
				Enabled: cfg.HomeKit.Camera.Enabled,
				Name:    cfg.HomeKit.Camera.Name,
				HLSPath: cfg.HomeKit.Camera.HLSPath,
			},
		}, logger)
		if err := hkPub.Start(ctx, sensors, cmds); err != nil {
			logger.Error("failed to start HomeKit publisher", "error", err)
		} else {
			pubs = append(pubs, hkPub)
			logger.Info("HomeKit publisher started")
		}
	}

	// Web UI
	mqttCB := &web.MQTTCallbacks{
		OnSensorRenamed: func(ctx context.Context, sensor camera.Sensor) error {
			if mqttPub == nil {
				return nil
			}
			hp := mqttPub.HAPublisher()
			if err := hp.PublishDiscovery(ctx, sensor); err != nil {
				return err
			}
			for _, extra := range mqtt.BuildExtraDiscoveryTopics(hp.Prefix(), sensor) {
				_ = hp.PublishRaw(ctx, extra.Topic, extra.Payload)
			}
			return nil
		},
		OnSensorDeleted: func(ctx context.Context, sensorID int) error {
			if mqttPub == nil {
				return nil
			}
			return mqttPub.HAPublisher().RemoveDiscovery(ctx, sensorID)
		},
		OnSensorsChanged: func(ctx context.Context) {
			// Rebuild HomeKit bridge so newly paired sensors appear and
			// removed sensors disappear. AIDs are derived from sensor.ID
			// so existing accessories survive the rebuild.
			if hkPub == nil {
				return
			}
			fresh, err := buildSensorList(ctx, cam, store, logger)
			if err != nil {
				logger.Warn("homekit rebuild: failed to list sensors", "error", err)
				return
			}
			if err := hkPub.RebuildBridge(fresh); err != nil {
				logger.Warn("homekit rebuild failed", "error", err)
			}
		},
		OnAlarmStateChanged: func(ctx context.Context, state string) error {
			setAlarmState(state)
			for _, p := range pubs {
				if err := p.PublishAlarmState(ctx, state); err != nil {
					logger.Warn("publish alarm state failed", "error", err)
				}
			}
			return nil
		},
	}
	var webSrv *web.Server
	if cfg.WebEnabled() {
		webSrv = web.NewServer(cam, store, func() bool { return mqttConnected }, mqttCB, staticweb.StaticFiles, logger)
		webSrv.SetVersion(BuildInfo())
		if *debugAPI {
			webSrv.EnableDebugEndpoints()
			logger.Warn("debug API endpoints enabled — DO NOT use in production")
		}
		// Inject the merged sensor list provider so the web UI sees
		// persisted sensors even when the MCU is temporarily silent
		// (post-reboot warmup). Without this, /api/sensors returns
		// an empty list while HomeKit/MQTT correctly show the sensors.
		webSrv.SetSensorListProvider(func(ctx context.Context) ([]camera.Sensor, error) {
			return buildSensorList(ctx, cam, store, logger)
		})
		webSrv.SetSensorCount(len(sensors))
		if err := webSrv.Start(*webAddr); err != nil {
			logger.Error("failed to start web server", "error", err)
			os.Exit(1)
		}
		defer func() {
			// Shutdown avec timeout : sans ça, les connexions SSE ouvertes
			// (clients UI temps réel) bloquent indéfiniment et killall finit
			// par avoir 2 instances qui se battent pour le port HK.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			_ = webSrv.Shutdown(shutdownCtx)
		}()

		// mDNS announce: openqiara.local on the web port.
		if port := parseListenPort(*webAddr); port > 0 {
			go func() {
				if err := mdns.Announce(ctx, port, logger); err != nil && ctx.Err() == nil {
					logger.Warn("mdns announce failed", "error", err)
				}
			}()
		}
	} else {
		logger.Info("web server disabled in config (headless mode)")
	}

	// hl_event_collectd → MQTT dispatcher (IntelliVision detections).
	//
	// Quand on intercepte les POST /events et /notifications du daemon
	// vendor `hl_event_collectd` (cf. internal/web/server.go), on récupère
	// les events IV (human/pet detection) — on les transforme en états
	// binary_sensor MQTT pour Home Assistant.
	//
	// Le dispatcher maintient un état par object_id avec auto-expire après
	// 30s sans Exit/Lost (au cas où la cam perd l'objet en cours). Le sink
	// reçoit chaque transition et publie sur MQTT.
	if webSrv != nil && mqttPub != nil {
		// Publish discovery une fois (HA va auto-créer les entités).
		if err := mqttPub.HAPublisher().PublishIVDiscovery(ctx); err != nil {
			logger.Warn("mqtt: publish IV discovery failed", "error", err)
		}
		// État initial false sur les deux topics — sans ça HA garde
		// "unknown" tant qu'aucune détection de cette classe n'est jamais
		// arrivée, ce qui est moche dans le dashboard.
		for _, kind := range []string{"human", "pet"} {
			if err := mqttPub.HAPublisher().PublishIVDetection(ctx, kind, false, 0, ""); err != nil {
				logger.Warn("mqtt: publish IV initial state failed", "kind", kind, "error", err)
			}
		}

		ivDispatcher := hlevents.NewDispatcher(logger, func(det hlevents.Detection) {
			kind := "human"
			if det.Class == hlevents.IVClassPet {
				kind = "pet"
			} else if det.Class != hlevents.IVClassHuman {
				// Classe inconnue (jamais observée en live) : skip.
				return
			}
			if err := mqttPub.HAPublisher().PublishIVDetection(ctx, kind, det.Present, det.Confidence, det.ObjectID); err != nil {
				logger.Warn("mqtt: publish IV detection failed",
					"kind", kind, "present", det.Present, "error", err)
			}
		}, 30*time.Second)

		webSrv.SetHLEventsDispatcher(ivDispatcher)
		logger.Info("hl_event_collectd dispatcher attached", "iv_kinds", []string{"human", "pet"})
	}

	// Lazy healing du pipeline HLS : hlcamd freeze silencieusement après
	// quelques heures (cf. feedback_hlcamd_freeze_after_hours.md). On
	// instancie un resumer partagé qui sera consulté à chaque requête
	// video (UI web + ouverture session HK) — pas de watchdog.
	// HLSPath par défaut si non configuré : voir homekit_camera.go.
	hlsPath := cfg.HomeKit.Camera.HLSPath
	if hlsPath == "" {
		hlsPath = "/tmp/out_stream/stream/720p/HLS_TEST.m3u8"
	}
	hlcamdResumer := camera.NewHlcamdResumer(hlsPath, 10*time.Second, 5*time.Second, logger)
	if webSrv != nil {
		webSrv.SetHlcamdResumer(hlcamdResumer)
	}
	if hkPub != nil && hkPub.Camera() != nil {
		hkPub.Camera().SetHlcamdResumer(hlcamdResumer)
	}

	// OTA updater — interroge GitHub Releases pour proposer des
	// updates depuis l'UI.
	//
	// onComplete : on délègue le swap final à un script shell détaché
	// parce que le binaire courant tient /data/openqiarad par inode :
	// rm ne libère pas l'espace tant qu'on est vivant, et /data est
	// trop plein pour héberger nouveau + ancien en parallèle. Le script
	// attend la mort du parent puis copie le binaire staged vers /data
	// et relance avec les mêmes args.
	if webSrv != nil {
		otaClient := ota.NewClient(BuildInfo(), logger)
		cfg := ota.DefaultInstallConfig()
		var otaInstaller *ota.Installer
		otaInstaller = ota.NewInstaller(otaClient, cfg, func() {
			staged := otaInstaller.Status().StagedAt
			if staged == "" {
				logger.Error("OTA onComplete: no staged path — aborting swap")
				return
			}
			if err := launchSwapScript(staged, cfg.TargetPath, os.Args, logger); err != nil {
				logger.Error("OTA: launch swap script failed", "error", err)
				return
			}
			logger.Info("OTA install complete — handing off to swap script")
			_ = syscall.Kill(os.Getpid(), syscall.SIGTERM)
		})
		webSrv.SetOTA(otaClient, otaInstaller)
	}

	// Alarm engine — standalone state machine for arm/disarm/triggered logic.
	// Config provider reads night_mode from the config store on each query.
	// alarmConfigFor mappe la config sensor vers le format attendu par
	// l'engine. Logique des modes nuit :
	//   - NightAlarm (nouveau, *bool) : true=déclenche en nuit, false=ignoré.
	//     Si nil (unset) → fallback à NightAllowed legacy inversé.
	//   - NightAllowed (legacy bool) : true=ignoré en nuit, false=déclenche.
	// L'engine consomme NightAllowed (sémantique "ignore en nuit"), donc on
	// inverse NightAlarm si présent.
	alarmConfigFor := func(sensorID int) alarm.SensorConfig {
		snap := store.Get()
		for _, se := range snap.Sensors {
			if se.ID != sensorID {
				continue
			}
			cfg := alarm.SensorConfig{NightAllowed: se.NightAllowed}
			if se.NightAlarm != nil {
				// NightAlarm=false (ne déclenche pas) → NightAllowed=true (ignoré).
				cfg.NightAllowed = !(*se.NightAlarm)
			}
			return cfg
		}
		return alarm.SensorConfig{}
	}
	// sirenCtrl pilote la sirène physique lors des transitions d'état de
	// la centrale d'alarme. Appelé depuis les deux modes (standalone via
	// alarmStateCallback, alarmo via MQTT). Logique testée dans
	// siren_controller_test.go.
	sirenCtrl := newSirenController(ctx, cam, store, logger)
	handleSirenForAlarmState := sirenCtrl.Handle

	alarmStateCallback := func(snap alarm.Snapshot) {
		logger.Info("alarm state", "state", snap.State, "prev", snap.PreviousState, "trigger", snap.TriggeredBy, "remaining", snap.TimerRemaining)
		setAlarmState(string(snap.State))
		handleSirenForAlarmState(string(snap.State), string(snap.PreviousState))
		if webSrv == nil {
			for _, p := range pubs {
				_ = p.PublishAlarmState(ctx, string(snap.State))
			}
			return
		}
		webSrv.SetAlarmState(string(snap.State))
		// Push to SSE clients immediately (don't wait for the next tick).
		webSrv.PublishEvent("alarm", web.AlarmSnapshot{
			State:          string(snap.State),
			ArmedAt:        snap.ArmedAt,
			TriggeredBy:    snap.TriggeredBy,
			PreviousState:  string(snap.PreviousState),
			TimerRemaining: snap.TimerRemaining,
		})
		for _, p := range pubs {
			_ = p.PublishAlarmState(ctx, string(snap.State))
		}
	}
	// Only spin up the local alarm engine in standalone mode. In alarmo
	// mode HA Alarmo is the source of truth and the engine would just
	// shadow its state, causing drift and useless persistence churn.
	var alarmEngine *alarm.Engine
	if store.Get().AlarmMode() == "standalone" {
		alarmEngine = alarm.New("/data/openqiara_alarm.json", alarmConfigFor, alarmStateCallback, logger)
		cfg := store.Get()
		alarmEngine.SetTimings(cfg.ArmingDelay(), cfg.PendingDelay())
		if err := alarmEngine.Load(); err != nil {
			logger.Warn("alarm: load failed, starting fresh", "error", err)
		}
		alarmEngine.Start()
		defer alarmEngine.Stop()
		if webSrv != nil {
			webSrv.SetAlarmProvider(&alarmAdapter{eng: alarmEngine})
			webSrv.SetAlarmState(string(alarmEngine.Snapshot().State))
		}
		initSnap := alarmEngine.Snapshot()
		setAlarmState(string(initSnap.State))
		logger.Info("alarm engine started", "state", initSnap.State)
	} else {
		logger.Info("alarm engine skipped (alarmo mode — HA Alarmo is the source of truth)")
	}

	// dispatchAlarmCommand routes a user alarm command (from web, HomeKit, MQTT or KPD)
	// according to the current config:
	//   - mode "standalone": feed the local alarm.Engine
	//   - mode "alarmo":     forward to alarmo/command MQTT topic (no local engine)
	// `source` qualifie l'origine : SourceLocal (KPD physique = délai armement
	// appliqué) vs SourceRemote (HK/web/MQTT = armement immédiat, sinon un intrus
	// aurait 60s pour fuir après alerte distante).
	// Config is re-read on every call so mode switches take effect at runtime.
	dispatchAlarmCommand := func(cmd string, source alarm.Source) {
		// Normalise "ARM_AWAY"/"armed_away" → "arm_away" etc.
		switch cmd {
		case "disarmed", "DISARM":
			cmd = "disarm"
		case "armed_away", "ARM_AWAY":
			cmd = "arm_away"
		case "armed_night", "ARM_NIGHT":
			cmd = "arm_night"
		}
		if cmd != "disarm" && cmd != "arm_away" && cmd != "arm_night" {
			return
		}

		cfg := store.Get()
		if cfg.AlarmMode() == "alarmo" {
			topic, _ := cfg.AlarmoTopics()
			var payload string
			switch cmd {
			case "arm_away":
				payload = "ARM_AWAY"
			case "arm_night":
				payload = "ARM_NIGHT"
			case "disarm":
				payload = "DISARM"
			}
			if mqttPub != nil {
				if err := mqttPub.HAPublisher().PublishRaw(ctx, topic, []byte(payload)); err != nil {
					logger.Warn("alarmo command publish failed", "topic", topic, "error", err)
				} else {
					logger.Info("alarm command → alarmo", "topic", topic, "payload", payload, "source", source)
				}
			}
			return
		}
		// standalone: feed the local engine.
		logger.Info("alarm command → engine", "cmd", cmd, "source", source)
		if alarmEngine != nil {
			alarmEngine.HandleCommand(cmd, source)
		}
	}

	// Wire up alarm commands from publishers (MQTT/HomeKit) to the dispatcher.
	// MQTT et HomeKit = sources distantes (skip délai d'armement).
	cmds.OnAlarmCommand = func(cmd string) { dispatchAlarmCommand(cmd, alarm.SourceRemote) }
	mqttCB.OnAlarmCommand = func(_ context.Context, cmd string) {
		dispatchAlarmCommand(cmd, alarm.SourceRemote)
	}

	// Subscribe to alarmo/state to reflect Alarmo's state in our UI when in
	// alarmo mode. We subscribe unconditionally so mode can be switched at
	// runtime. The handler only acts if current mode is "alarmo".
	if mqttPub != nil {
		_, alarmoStateTopic := store.Get().AlarmoTopics()
		mqttPub.HAPublisher().Subscribe(alarmoStateTopic, func(topic string, payload []byte) {
			if store.Get().AlarmMode() != "alarmo" {
				return
			}
			state := string(payload)
			prev := getAlarmState()
			logger.Info("alarmo state received", "state", state, "prev", prev)
			if webSrv != nil {
				webSrv.SetAlarmState(state)
				webSrv.PublishEvent("alarm", web.AlarmSnapshot{State: state})
			}
			for _, p := range pubs {
				_ = p.PublishAlarmState(ctx, state)
			}
			// Pilotage sirène physique sur les transitions alarmo.
			handleSirenForAlarmState(state, prev)
			setAlarmState(state)
		})
		logger.Info("subscribed to alarmo state", "topic", alarmoStateTopic)
	}

	// KPD log watcher (fbxhome mode only).
	//
	// Le watcher tail /var/log/fbxhome.log et émet des SensorEvent pour les
	// actions KPD (HlKpd: KPD_DAY_ALARM, KPD_ALARM_OFF, ...). L'ID du KPD est
	// résolu dynamiquement via lookupKPDID() à chaque event détecté — un
	// re-pair en runtime change l'ID sans nécessiter de restart openqiarad.
	kpdEvents := make(chan camera.SensorEvent, 16)
	lookupKPDID := func() int {
		for _, s := range store.Get().Sensors {
			if s.Type == "KPD" {
				return s.ID
			}
		}
		return 0
	}
	// LogWatcher uniquement pour le backend fbxhome (tail syslog). En charmux,
	// les events KPD viennent des PKT frames parsées par CharmuxClient.
	if _, isFbx := cam.(*camera.FbxhomeClient); isFbx {
		logWatcher := camera.NewLogWatcher("/var/log/fbxhome.log", lookupKPDID, camera.LogWatcherEventSink(kpdEvents), logger)
		logWatcher.Start()
		defer logWatcher.Stop()
		logger.Info("KPD log watcher started (dynamic kpd_id)")
	}

	// fbxhome log tail pour les events sensors (DWS open/close, PIR motion).
	// Source isolée dans internal/fbxhomelog — quand on aura identifié le
	// vrai mécanisme push de fbxhome, on remplace l'implémentation sans
	// toucher au reste. Le polling reste actif comme fallback (intervalle
	// long), pour rattraper si on rate des events ou si le log rotate
	// pendant un downtime.
	tailEvents := make(chan camera.SensorEvent, 32)
	if _, isFbx := cam.(*camera.FbxhomeClient); isFbx {
		startFbxhomeLogTail(ctx, cam, tailEvents, logger)
	}

	// Ensure publishers are closed on exit
	defer func() {
		for _, p := range pubs {
			_ = p.Close()
		}
	}()

	// In fbxhome mode the camera client polls /api/v1/home/endpoints_read
	// on a fixed interval and emits SensorEvents on state changes. In
	// charmux mode the cam.StartPolling is a no-op (charmux pushes events
	// directly from PKT frames). Calling it unconditionally is safe: the
	// no-op variants ignore the interval.
	if poller, ok := cam.(interface {
		StartPolling(context.Context, time.Duration)
	}); ok {
		poller.StartPolling(ctx, *pollInterval)
		logger.Info("sensor polling started", "interval", *pollInterval)
	}
	forwardEvents(ctx, cam, pubs, kpdEvents, tailEvents, webSrv, alarmEngine, store, dispatchAlarmCommand, logger)
}

// buildSensorList returns the merged sensor list (config + MCU). The
// config is the primary source of truth: it's instant and persists
// across reboots. The MCU is queried as a best-effort enrichment with a
// short timeout — if it doesn't respond fast (typical post-reboot
// state), we just return the config sensors so the UI never hangs
// waiting on an unresponsive radio chip.
//
// Sensors marked as deleted in the config are filtered from both
// sources.
func buildSensorList(ctx context.Context, cam camera.Client, store *config.Store, logger *slog.Logger) ([]camera.Sensor, error) {
	cfg := store.Get()

	deletedIDs := make(map[int]bool, len(cfg.DeletedIDs))
	for _, id := range cfg.DeletedIDs {
		deletedIDs[id] = true
	}

	// Start from the persisted config — instant, no I/O.
	sensors := make([]camera.Sensor, 0, len(cfg.Sensors))
	seen := make(map[int]bool, len(cfg.Sensors))
	for _, se := range cfg.Sensors {
		if se.Type == "" || deletedIDs[se.ID] {
			continue
		}
		sensors = append(sensors, camera.Sensor{
			ID:        se.ID,
			Type:      se.Type,
			Reachable: true,
		})
		seen[se.ID] = true
	}

	// Merge live state from the in-memory cache (open, motion, last_seen).
	// This is always up-to-date from PKT events, no MCU I/O needed.
	liveMap := make(map[int]camera.Sensor)
	for _, s := range cam.CachedSensors() {
		liveMap[s.ID] = s
	}
	for i, s := range sensors {
		if live, ok := liveMap[s.ID]; ok {
			sensors[i].Open = live.Open
			sensors[i].Motion = live.Motion
			sensors[i].Tamper = live.Tamper
			sensors[i].LastSeen = live.LastSeen
			sensors[i].Reachable = live.Reachable
			sensors[i].KPDState = live.KPDState
		}
	}

	// Add live-cache sensors that are neither in the persisted config nor
	// (yet) in the MCU node table. This covers freshly paired sensors —
	// StartPairing populates c.sensors immediately but the config write
	// happens through a separate path, and GetNodes can time out while
	// charmux is still warming up.
	for _, live := range liveMap {
		if seen[live.ID] || deletedIDs[live.ID] {
			continue
		}
		sensors = append(sensors, live)
		seen[live.ID] = true
	}

	// Best-effort MCU query with a short timeout. If the MCU is silent
	// (warming up after reboot), we skip without blocking the caller.
	mcuCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	mcuSensors, mcuErr := cam.Sensors(mcuCtx)
	if mcuErr != nil {
		if len(sensors) > 0 {
			return sensors, nil
		}
		return sensors, mcuErr
	}
	for _, s := range mcuSensors {
		if deletedIDs[s.ID] {
			continue
		}
		if seen[s.ID] {
			// MCU has fresher info (reachability, type metadata) —
			// replace the config-derived entry.
			for i := range sensors {
				if sensors[i].ID == s.ID {
					sensors[i] = s
					break
				}
			}
			continue
		}
		sensors = append(sensors, s)
		seen[s.ID] = true
	}
	return sensors, nil
}

func createCamera(ctx context.Context, mode string, knownTypes map[int]string, deletedIDs map[int]bool, kpdCodes map[int]string, logger *slog.Logger) camera.Client {
	switch mode {
	case "charmux":
		return createCharmuxClient(ctx, knownTypes, deletedIDs, kpdCodes, logger)
	case "fbxhome":
		return createFbxhomeClient(ctx, logger)
	default:
		c := createCharmuxClient(ctx, knownTypes, deletedIDs, kpdCodes, logger)
		if c != nil {
			return c
		}
		logger.Info("charmux unavailable, falling back to fbxhome")
		return createFbxhomeClient(ctx, logger)
	}
}

func createCharmuxClient(ctx context.Context, knownTypes map[int]string, deletedIDs map[int]bool, kpdCodes map[int]string, logger *slog.Logger) camera.Client {
	mux := charmux.New(charmux.WithLogger(logger))
	c := camera.NewCharmuxClient(
		camera.WithCharmux(mux),
		camera.WithCharmuxLogger(logger),
		camera.WithKnownSensorTypes(knownTypes),
		camera.WithDeletedIDs(deletedIDs),
		camera.WithKPDCodes(kpdCodes),
	)
	if err := c.Connect(ctx); err != nil {
		logger.Warn("charmux connect failed", "error", err)
		return nil
	}
	return c
}

func createFbxhomeClient(ctx context.Context, logger *slog.Logger) camera.Client {
	c := camera.NewFbxhomeClient(camera.WithLogger(logger))
	if err := c.Connect(ctx); err != nil {
		logger.Warn("fbxhome connect failed", "error", err)
		return nil
	}
	return c
}

// webSrvSetAlarm updates the web server alarm state and publishes to all publishers.
func webSrvSetAlarm(state string, pubs []publisher.Publisher, ctx context.Context, logger *slog.Logger) {
	for _, p := range pubs {
		if err := p.PublishAlarmState(ctx, state); err != nil {
			logger.Error("publish alarm state failed", "error", err)
		}
	}
}

func forwardEvents(
	ctx context.Context,
	cam camera.Client,
	pubs []publisher.Publisher,
	kpdEvents <-chan camera.SensorEvent,
	tailEvents <-chan camera.SensorEvent,
	webSrv *web.Server,
	alarmEngine *alarm.Engine,
	store *config.Store,
	dispatchCmd func(cmd string, source alarm.Source),
	logger *slog.Logger,
) {
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)

	// Publie un event sensor à tous les consommateurs (MQTT, HomeKit, SSE, alarm).
	publishSensor := func(evt camera.SensorEvent) {
		logger.Info("sensor event",
			"id", evt.SensorID,
			"type", evt.Sensor.Type,
			"open", evt.Sensor.Open,
			"motion", evt.Sensor.Motion,
			"battery", evt.Sensor.Battery,
			"src", "tail-or-poll",
		)
		for _, p := range pubs {
			if err := p.PublishSensorState(ctx, evt.Sensor); err != nil {
				logger.Error("publish sensor failed", "error", err)
			}
		}
		if webSrv != nil {
			webSrv.PublishEvent("sensor", evt.Sensor)
		}
		if alarmEngine != nil && store.Get().AlarmMode() == "standalone" {
			alarmEngine.HandleSensorEvent(evt.SensorID, evt.Sensor.Type, sensorInAlarm(evt.Sensor))
		}
	}

	for {
		select {
		case evt := <-kpdEvents:
			// fbxhome legacy log watcher path. KPD physique = source locale
			// → délai d'armement appliqué.
			dispatchCmd(evt.Sensor.Label, alarm.SourceLocal)

		case evt := <-tailEvents:
			// Push depuis fbxhomelog (tail /var/log/fbxhome.log).
			publishSensor(evt)

		case evt, ok := <-cam.Events():
			if !ok {
				return
			}
			if evt.Sensor.Type == "KPD" && (evt.Sensor.KPDState != "" || evt.Sensor.Label != "") {
				action := evt.Sensor.KPDState
				if action == "" {
					action = evt.Sensor.Label // fbxhome mode fallback
				}
				logger.Info("KPD command", "id", evt.SensorID, "action", action)
				// KPD physique → source locale, délai d'armement appliqué.
				dispatchCmd(action, alarm.SourceLocal)
			} else {
				logger.Info("sensor event",
					"id", evt.SensorID,
					"type", evt.Sensor.Type,
					"open", evt.Sensor.Open,
					"motion", evt.Sensor.Motion,
					"battery", evt.Sensor.Battery,
				)
				// Publish to MQTT/HomeKit regardless of alarm mode — sensors are
				// always exposed as binary_sensors so Alarmo (or any other
				// consumer) can react to them.
				for _, p := range pubs {
					if err := p.PublishSensorState(ctx, evt.Sensor); err != nil {
						logger.Error("publish sensor failed", "error", err)
					}
				}
				// Push the single-sensor update to SSE clients (no MCU call).
				if webSrv != nil {
					webSrv.PublishEvent("sensor", evt.Sensor)
				}
				// Only feed the local alarm engine in standalone mode.
				// In alarmo mode, Alarmo has its own trigger logic via HA.
				if alarmEngine != nil && store.Get().AlarmMode() == "standalone" {
					inAlarm := sensorInAlarm(evt.Sensor)
					alarmEngine.HandleSensorEvent(evt.SensorID, evt.Sensor.Type, inAlarm)
				}
			}
		case <-sig:
			logger.Info("shutting down")
			return
		}
	}
}

// parseListenPort extracts the numeric port from a listen address like ":8080" or "0.0.0.0:80".
func parseListenPort(addr string) int {
	// Simple split: take the last colon-separated token.
	i := len(addr) - 1
	for i >= 0 && addr[i] != ':' {
		i--
	}
	if i < 0 || i == len(addr)-1 {
		return 0
	}
	n := 0
	for _, c := range addr[i+1:] {
		if c < '0' || c > '9' {
			return 0
		}
		n = n*10 + int(c-'0')
	}
	return n
}

// alarmAdapter bridges alarm.Engine to web.AlarmProvider.
type alarmAdapter struct {
	eng *alarm.Engine
}

func (a *alarmAdapter) Snapshot() web.AlarmSnapshot {
	s := a.eng.Snapshot()
	return web.AlarmSnapshot{
		State:          string(s.State),
		ArmedAt:        s.ArmedAt,
		TriggeredBy:    s.TriggeredBy,
		PreviousState:  string(s.PreviousState),
		TimerRemaining: s.TimerRemaining,
	}
}

// HandleCommand est appelé uniquement depuis le handler web fallback (quand
// le dispatcher MQTT n'est pas câblé). Source = remote (HTTP).
func (a *alarmAdapter) HandleCommand(cmd string) { a.eng.HandleCommand(cmd, alarm.SourceRemote) }

func (a *alarmAdapter) SetTimings(arming, pending time.Duration) {
	a.eng.SetTimings(arming, pending)
}

// startFbxhomeLogTail démarre le tail de /var/log/fbxhome.log et forward les
// events DWS/PIR convertis vers eventsOut. Les events KPD sont ignorés (déjà
// gérés par camera.LogWatcher en amont).
//
// Lance 2 goroutines : (1) le tail lui-même, (2) le forwarder qui consomme le
// channel et publie. Les deux s'arrêtent quand ctx est annulé.
func startFbxhomeLogTail(
	ctx context.Context,
	cam camera.Client,
	eventsOut chan<- camera.SensorEvent,
	logger *slog.Logger,
) {
	tail := fbxhomelog.NewLogTail("/var/log/fbxhome.log", logger)
	logger.Info("fbxhome log tail started", "path", "/var/log/fbxhome.log")

	go func() {
		if err := tail.Run(ctx); err != nil && !errors.Is(err, context.Canceled) {
			logger.Error("fbxhome log tail ended with error", "err", err)
		}
	}()

	// Si le client cam expose un cache updatable (FbxhomeClient), on l'utilise
	// pour que GET /api/sensors reflète le nouvel état immédiatement.
	type cacheWriter interface {
		UpdateCachedSensor(camera.Sensor)
	}
	cw, _ := cam.(cacheWriter)

	go func() {
		for evt := range tail.Events() {
			se, ok := convertLogEvent(evt, cam)
			if !ok {
				continue
			}
			if cw != nil {
				cw.UpdateCachedSensor(se.Sensor)
			}
			select {
			case eventsOut <- se:
			case <-ctx.Done():
				return
			default:
				logger.Warn("log tail event channel full, dropping", "kind", evt.Kind, "id", evt.NodeID)
			}
		}
	}()
}

// convertLogEvent traduit un fbxhomelog.Event en camera.SensorEvent. Le
// second retour est false si l'event n'est pas à relayer (ex: KPD, déjà géré
// par camera.LogWatcher).
//
// Pour DWS/PIR, fusionne avec la dernière vue cache (CachedSensors) afin de
// préserver battery/temp/etc. que le log ne contient pas — sinon l'UI verrait
// battery=0 à chaque event open/close.
func convertLogEvent(evt fbxhomelog.Event, cam camera.Client) (camera.SensorEvent, bool) {
	if evt.Kind == fbxhomelog.KindKPD {
		return camera.SensorEvent{}, false
	}

	base := lookupCached(cam, evt.NodeID)
	base.ID = evt.NodeID
	base.LastSeen = evt.Time.Unix()
	switch evt.Kind {
	case fbxhomelog.KindDWSOpen:
		base.Type = "DWS"
		base.Open = true
	case fbxhomelog.KindDWSClose:
		base.Type = "DWS"
		base.Open = false
	case fbxhomelog.KindPIRStart:
		base.Type = "PIR"
		base.Motion = true
	case fbxhomelog.KindPIREnd:
		base.Type = "PIR"
		base.Motion = false
	default:
		return camera.SensorEvent{}, false
	}
	return camera.SensorEvent{SensorID: evt.NodeID, Sensor: base}, true
}

// lookupCached retourne le sensor caché pour nodeID, ou un Sensor zéro.
func lookupCached(cam camera.Client, nodeID int) camera.Sensor {
	for _, s := range cam.CachedSensors() {
		if s.ID == nodeID {
			return s
		}
	}
	return camera.Sensor{}
}

// sensorInAlarm returns true if the sensor is currently in its "alarm" state,
// i.e. open door, motion detected, or tamper.
func sensorInAlarm(s camera.Sensor) bool {
	switch s.Type {
	case "DWS":
		return s.Open
	case "PIR":
		return s.Motion
	case "SRN":
		return s.Tamper
	default:
		return false
	}
}
