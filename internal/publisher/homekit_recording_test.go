package publisher

import (
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"slices"
	"testing"

	"github.com/brutella/hap"
	"github.com/brutella/hap/accessory"

	"github.com/caligone/openqiara/internal/hksv"
)

// The camera accessory carries HomeKit Secure Video's services as iOS
// expects them (HAP-NodeJS ServiceDefinitions.ts).
func TestCameraHasRecordingServices(t *testing.T) {
	cam := NewHomeKitCamera(CameraConfig{Enabled: true}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	// The server numbers the services: links point at those ids.
	bridge := accessory.NewBridge(accessory.Info{Name: "bridge"})
	if _, err := hap.NewServer(hap.NewMemStore(), bridge.A, cam.Accessory().A); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cam.Accessory().A)
	if err != nil {
		t.Fatal(err)
	}
	var acc struct {
		Services []struct {
			IID    uint64   `json:"iid"`
			Type   string   `json:"type"`
			Linked []uint64 `json:"linked"`
			Cs     []struct {
				Type  string   `json:"type"`
				Perms []string `json:"perms"`
			} `json:"characteristics"`
		} `json:"services"`
	}
	if err := json.Unmarshal(b, &acc); err != nil {
		t.Fatal(err)
	}
	byType := map[string]int{}
	for i, s := range acc.Services {
		byType[s.Type] = i
	}
	for _, typ := range []string{"204", "21A", "129", "85"} {
		if _, ok := byType[typ]; !ok {
			t.Fatalf("service %s missing", typ)
		}
	}
	mgmt, stream, motion := acc.Services[byType["204"]], acc.Services[byType["129"]], acc.Services[byType["85"]]
	if !slices.Contains(mgmt.Linked, stream.IID) || !slices.Contains(mgmt.Linked, motion.IID) {
		t.Fatal("recording management not linked to the data stream service and the motion sensor")
	}
	perms := map[string][]string{}
	for _, s := range acc.Services {
		for _, c := range s.Cs {
			perms[c.Type] = c.Perms
		}
	}
	for typ, want := range map[string][]string{
		"B0":  {"pr", "pw", "ev"},
		"209": {"pr", "pw", "ev"},
		"131": {"pr", "pw", "wr"},
		"130": {"pr"},
		"226": {"pr", "pw", "ev"}, // no tw: hap panics on unprepared timed writes
		"21B": {"pr", "pw", "ev"},
	} {
		if !slices.Equal(perms[typ], want) {
			t.Errorf("characteristic %s: perms %v, want %v", typ, perms[typ], want)
		}
	}
}

// A recording is refused until recording is on and iOS chose a
// configuration, which survive a restart.
func TestRecordingStateSurvivesRestart(t *testing.T) {
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	store := hap.NewMemStore()
	cam := NewHomeKitCamera(CameraConfig{Enabled: true}, logger)
	cam.rec.setStore(store)
	if _, reason := cam.rec.openRecording(); reason != hksv.ReasonNotAllowed {
		t.Fatalf("recording off: reason %d", reason)
	}

	// What iOS writes: recording on, a configuration (prebuffer, H.264).
	req := httptest.NewRequest("PUT", "/characteristics", nil)
	cam.rec.active.SetValueRequest(1, req)
	selected := []byte{0x01, 0x06, 0x01, 0x04, 0xa0, 0x0f, 0, 0, 0x02, 0x03, 0x01, 0x01, 0x00}
	cam.rec.mgmt.SelectedCameraRecordingConfiguration.SetValueRequest(base64.StdEncoding.EncodeToString(selected), req)
	if record, reason := cam.rec.openRecording(); record == nil {
		t.Fatalf("refused: reason %d", reason)
	}
	cam.rec.recorder.SetActive(false)

	restarted := NewHomeKitCamera(CameraConfig{Enabled: true}, logger)
	if _, reason := restarted.rec.openRecording(); reason != hksv.ReasonNotAllowed {
		t.Fatalf("fresh camera: reason %d", reason)
	}
	restarted.rec.setStore(store)
	defer restarted.rec.recorder.SetActive(false)
	if record, reason := restarted.rec.openRecording(); record == nil {
		t.Fatalf("after restart: reason %d", reason)
	}
}
