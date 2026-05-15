package hlevents

import "testing"

// Body réels capturés en live dans /data/openqiarad.log session 2026-05-15.
// Ces fixtures servent de garde-fou : si le format change un jour, les
// tests le détecteront avant la prod.

func TestParseEvents_ShutterOpen(t *testing.T) {
	body := []byte(`{"events":[{"ts":1778832323,"ev":{
  "area" : "",
  "room" : "",
  "type" : "shutter_open"
}}]}`)
	env, err := ParseEvents(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(env.Events) != 1 {
		t.Fatalf("expected 1 event, got %d", len(env.Events))
	}
	if env.Events[0].Event.Type != "shutter_open" {
		t.Errorf("type = %q, want shutter_open", env.Events[0].Event.Type)
	}
}

func TestParseEvents_Empty(t *testing.T) {
	for _, body := range [][]byte{nil, []byte(""), []byte("{}")} {
		env, err := ParseEvents(body)
		if err != nil {
			t.Errorf("parse %q: %v", body, err)
		}
		if len(env.Events) != 0 {
			t.Errorf("empty body should yield no events, got %d", len(env.Events))
		}
	}
}

func TestParseNotifications_IVEntered(t *testing.T) {
	body := []byte(`{"notifications":[{"ts":1778834870,"notif":{"data":{"events":[{"eventType":"eObjectEnteredEvent","timestamp":1778834865}],"objects":[{"class":"eOC_Human","confidence":1,"id":"26"}]},"type":"iv_event"}}]}`)
	env, err := ParseNotifications(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(env.Notifications) != 1 {
		t.Fatalf("expected 1 notif")
	}
	n := env.Notifications[0]
	if n.Notif.Type != "iv_event" {
		t.Errorf("type = %q, want iv_event", n.Notif.Type)
	}
	if len(n.Notif.Data.Events) != 1 {
		t.Errorf("expected 1 event, got %d", len(n.Notif.Data.Events))
	}
	if n.Notif.Data.Events[0].EventType != IVEventEntered {
		t.Errorf("eventType = %q, want %q", n.Notif.Data.Events[0].EventType, IVEventEntered)
	}
	if len(n.Notif.Data.Objects) != 1 {
		t.Fatalf("expected 1 object")
	}
	o := n.Notif.Data.Objects[0]
	if o.Class != IVClassHuman {
		t.Errorf("class = %q, want %q", o.Class, IVClassHuman)
	}
	if o.Confidence != 1 {
		t.Errorf("confidence = %f, want 1", o.Confidence)
	}
	if o.ID != "26" {
		t.Errorf("id = %q, want 26", o.ID)
	}
}

func TestParseNotifications_IVExitedWithoutObjects(t *testing.T) {
	// Body réel : exit event sans objects (cam ne ré-émet pas l'ID).
	body := []byte(`{"notifications":[{"ts":1778834891,"notif":{"data":{"events":[{"eventType":"eObjectExitedEvent","timestamp":1778834886}]},"type":"iv_event"}}]}`)
	env, err := ParseNotifications(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	n := env.Notifications[0]
	if n.Notif.Data.Events[0].EventType != IVEventExited {
		t.Errorf("eventType = %q", n.Notif.Data.Events[0].EventType)
	}
	if len(n.Notif.Data.Objects) != 0 {
		t.Errorf("expected no objects, got %d", len(n.Notif.Data.Objects))
	}
}

func TestParseNotifications_IVClassifiedOnly(t *testing.T) {
	// Body réel : classified pur sans events, juste un object remis à jour.
	body := []byte(`{"notifications":[{"ts":1778834960,"notif":{"data":{"objects":[{"class":"eOC_Human","confidence":1,"id":"26"}]},"type":"iv_event"}}]}`)
	env, err := ParseNotifications(body)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	n := env.Notifications[0]
	if len(n.Notif.Data.Events) != 0 {
		t.Errorf("expected no events")
	}
	if len(n.Notif.Data.Objects) != 1 {
		t.Errorf("expected 1 object")
	}
}

func TestParseNotifications_Empty(t *testing.T) {
	for _, body := range [][]byte{nil, []byte(""), []byte("{}")} {
		env, err := ParseNotifications(body)
		if err != nil {
			t.Errorf("parse %q: %v", body, err)
		}
		if len(env.Notifications) != 0 {
			t.Errorf("empty body should yield no notifs")
		}
	}
}
