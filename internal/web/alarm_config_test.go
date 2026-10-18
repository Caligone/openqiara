package web

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func alarmConfigRequest(t *testing.T, h http.Handler, method, body string) (int, map[string]any) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(method, "/api/v1/config/alarm", strings.NewReader(body)))
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func TestAlarmConfigShutterFollowsAlarm(t *testing.T) {
	h := newTestServer(t, nil)

	if _, got := alarmConfigRequest(t, h, http.MethodGet, ""); got["shutter_follows_alarm"] != false {
		t.Fatalf("default shutter_follows_alarm = %v, want false", got["shutter_follows_alarm"])
	}
	if code, _ := alarmConfigRequest(t, h, http.MethodPut, `{"shutter_follows_alarm": true}`); code != http.StatusOK {
		t.Fatalf("PUT = %d, want 200", code)
	}
	if _, got := alarmConfigRequest(t, h, http.MethodGet, ""); got["shutter_follows_alarm"] != true {
		t.Fatalf("shutter_follows_alarm = %v, want true", got["shutter_follows_alarm"])
	}
	// The aggregate view carries it too.
	_, agg := do(t, h, http.MethodGet, "/api/v1/config")
	alarm, _ := agg["alarm"].(map[string]any)
	if alarm["shutter_follows_alarm"] != true {
		t.Fatalf("aggregate alarm = %v", alarm)
	}
	// Other fields are left alone by a partial PUT.
	if code, _ := alarmConfigRequest(t, h, http.MethodPut, `{"siren_sounds": "none"}`); code != http.StatusOK {
		t.Fatalf("PUT = %d", code)
	}
	if _, got := alarmConfigRequest(t, h, http.MethodGet, ""); got["shutter_follows_alarm"] != true {
		t.Fatal("partial PUT reset shutter_follows_alarm")
	}
}

func TestAlarmConfigShutterFollowsAlarmRejectsNonBool(t *testing.T) {
	h := newTestServer(t, nil)
	for _, body := range []string{`{"shutter_follows_alarm": "yes"}`, `{"shutter_follows_alarm": 1}`} {
		if code, _ := alarmConfigRequest(t, h, http.MethodPut, body); code != http.StatusBadRequest {
			t.Errorf("PUT %s = %d, want 400", body, code)
		}
	}
}
