package hlevents

import (
	"encoding/json"
	"fmt"
)

// ParseEvents parse un body POST `/events` en EventEnvelope.
// Tolère un body vide ({}) — retourne envelope vide sans erreur.
func ParseEvents(body []byte) (EventEnvelope, error) {
	var env EventEnvelope
	if len(body) == 0 || string(body) == "{}" {
		return env, nil
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return env, fmt.Errorf("parse /events body: %w", err)
	}
	return env, nil
}

// ParseNotifications parse un body POST `/notifications` en
// NotificationEnvelope. Tolère un body vide ({}).
func ParseNotifications(body []byte) (NotificationEnvelope, error) {
	var env NotificationEnvelope
	if len(body) == 0 || string(body) == "{}" {
		return env, nil
	}
	if err := json.Unmarshal(body, &env); err != nil {
		return env, fmt.Errorf("parse /notifications body: %w", err)
	}
	return env, nil
}
