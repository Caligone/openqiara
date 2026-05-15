package mqtt

import (
	"encoding/json"
	"flag"
	"os"
	"path/filepath"
	"testing"

	"github.com/caligone/openqiara/internal/camera"
)

// updateSnapshots permet de regénérer les fichiers golden quand la doc
// HA discovery évolue intentionnellement.
//   go test ./internal/mqtt -update
var updateSnapshots = flag.Bool("update", false, "update golden snapshot files")

// snapshotTest compare le payload marshalé à un fichier golden dans testdata/.
// Si le fichier n'existe pas (ou flag -update), il est créé/écrasé.
func snapshotTest(t *testing.T, name string, payload any) {
	t.Helper()
	got, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	got = append(got, '\n')

	path := filepath.Join("testdata", name+".json")
	if *updateSnapshots {
		if err := os.MkdirAll("testdata", 0o755); err != nil {
			t.Fatalf("mkdir testdata: %v", err)
		}
		if err := os.WriteFile(path, got, 0o644); err != nil {
			t.Fatalf("write golden: %v", err)
		}
		t.Logf("updated golden file %s", path)
		return
	}

	want, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read golden %s (run with -update to create): %v", path, err)
	}
	if string(got) != string(want) {
		t.Errorf("snapshot mismatch for %s\n--- want ---\n%s\n--- got ---\n%s\n(run `go test ./internal/mqtt -update` to refresh if the change is intended)", name, want, got)
	}
}

func TestDiscoverySnapshot_DWS(t *testing.T) {
	p, ok := buildDiscoveryPayload("openqiara", camera.Sensor{ID: 14, Type: "DWS", Label: "Porte d'entrée"})
	if !ok {
		t.Fatal("ok=false")
	}
	snapshotTest(t, "discovery_dws", p)
}

func TestDiscoverySnapshot_PIR(t *testing.T) {
	p, ok := buildDiscoveryPayload("openqiara", camera.Sensor{ID: 20, Type: "PIR", Label: "Salon"})
	if !ok {
		t.Fatal("ok=false")
	}
	snapshotTest(t, "discovery_pir", p)
}

func TestDiscoverySnapshot_SRN(t *testing.T) {
	p, ok := buildDiscoveryPayload("openqiara", camera.Sensor{ID: 32, Type: "SRN", Label: "Sirène"})
	if !ok {
		t.Fatal("ok=false")
	}
	snapshotTest(t, "discovery_srn", p)
}

func TestDiscoverySnapshot_UnlabelledFallback(t *testing.T) {
	// Sans label, doit générer un nom par défaut. Si on touche au fallback,
	// le snapshot rouge nous alerte avant que HA ré-affiche des entités
	// renommées chez les users.
	p, ok := buildDiscoveryPayload("openqiara", camera.Sensor{ID: 99, Type: "DWS"})
	if !ok {
		t.Fatal("ok=false")
	}
	snapshotTest(t, "discovery_dws_unlabelled", p)
}

func TestDiscoverySnapshot_Shutter(t *testing.T) {
	topic, payload := ShutterDiscoveryPayload("openqiara")
	combined := map[string]any{
		"topic":   topic,
		"payload": json.RawMessage(payload),
	}
	snapshotTest(t, "discovery_shutter", combined)
}

// extrasView décode les payloads []byte (JSON brut) en map pour avoir un
// snapshot lisible plutôt qu'une string base64.
type extrasView struct {
	Topic   string         `json:"topic"`
	Payload map[string]any `json:"payload"`
}

func extrasAsView(t *testing.T, extras []ExtraDiscovery) []extrasView {
	t.Helper()
	out := make([]extrasView, len(extras))
	for i, e := range extras {
		var m map[string]any
		if err := json.Unmarshal(e.Payload, &m); err != nil {
			t.Fatalf("unmarshal extra %d: %v", i, err)
		}
		out[i] = extrasView{Topic: e.Topic, Payload: m}
	}
	return out
}

func TestDiscoverySnapshot_ExtraTopics_DWS(t *testing.T) {
	extras := BuildExtraDiscoveryTopics("openqiara", camera.Sensor{ID: 14, Type: "DWS"})
	snapshotTest(t, "discovery_extras_dws", extrasAsView(t, extras))
}

func TestDiscoverySnapshot_ExtraTopics_PIR(t *testing.T) {
	extras := BuildExtraDiscoveryTopics("openqiara", camera.Sensor{ID: 20, Type: "PIR"})
	snapshotTest(t, "discovery_extras_pir", extrasAsView(t, extras))
}

func TestDiscoverySnapshot_ExtraTopics_SRN(t *testing.T) {
	extras := BuildExtraDiscoveryTopics("openqiara", camera.Sensor{ID: 32, Type: "SRN"})
	snapshotTest(t, "discovery_extras_srn", extrasAsView(t, extras))
}

func TestDiscoverySnapshot_ExtraTopics_KPD(t *testing.T) {
	extras := BuildExtraDiscoveryTopics("openqiara", camera.Sensor{ID: 29, Type: "KPD"})
	snapshotTest(t, "discovery_extras_kpd", extrasAsView(t, extras))
}
