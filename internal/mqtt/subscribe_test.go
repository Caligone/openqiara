package mqtt

import (
	"slices"
	"testing"

	"github.com/caligone/openqiara/internal/camera"
)

// A broker restarted without persistence forgets the session: paho reconnects
// but never re-sends a SUBSCRIBE, and every HA command is lost without a
// trace (#59). Same at boot when the broker isn't reachable yet. Every
// connection must re-send all subscriptions.
func TestSubscriptionsResentOnEveryConnect(t *testing.T) {
	p, c := newRecordingPublisher()
	p.prefix = "openqiara"
	c.closed = true // broker unreachable at boot

	p.SetupAlarmCommandHandler(0, nil, p.log)
	p.SetupSirenCommandHandler([]camera.Sensor{{ID: 14, Type: "SRN"}}, nil, p.log)
	p.SetupShutterCommandHandler(func(bool) {}, p.log)
	p.Subscribe("alarmo/state", func(string, []byte) {})

	if len(c.subscribed) != 0 {
		t.Fatalf("subscribed while the connection is closed: %v", c.subscribed)
	}

	c.closed = false
	p.resubscribe() // first connection
	p.resubscribe() // reconnection to a broker that lost the session

	want := []string{
		"alarmo/state", "alarmo/state",
		"openqiara/alarm/set", "openqiara/alarm/set",
		"openqiara/shutter/set", "openqiara/shutter/set",
		"openqiara/siren/14/set", "openqiara/siren/14/set",
	}
	slices.Sort(c.subscribed)
	if !slices.Equal(c.subscribed, want) {
		t.Errorf("subscribed = %v, want %v", c.subscribed, want)
	}
}

// Once connected, a new subscription goes out right away.
func TestSubscribeSendsNowWhenConnected(t *testing.T) {
	p, c := newRecordingPublisher()

	p.Subscribe("alarmo/state", func(string, []byte) {})

	if !slices.Equal(c.subscribed, []string{"alarmo/state"}) {
		t.Errorf("subscribed = %v", c.subscribed)
	}
}
