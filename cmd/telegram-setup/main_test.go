package main

import (
	"testing"

	"github.com/caligone/openqiara/internal/telegram"
)

func TestConfigFromUpdates(t *testing.T) {
	ups := []telegram.Update{
		{ID: 1},
		{ID: 2, ChatID: 9, Callback: "cb", Data: "x"},
		{ID: 4, ChatID: -100, FromID: 42, Text: "dans un groupe"},
		{ID: 3, ChatID: 42, FromID: 42, Text: "bonjour"},
	}
	id, ok := pickNewChat(nil, ups)
	if !ok || id != 42 {
		t.Fatalf("got %d ok=%v", id, ok)
	}
	if _, ok := pickNewChat(nil, ups[:3]); ok {
		t.Fatal("expected no chat without a private text message")
	}
}

func TestPickNewChatSkipsKnownAccounts(t *testing.T) {
	ups := []telegram.Update{
		{ID: 1, ChatID: 42, FromID: 42, Text: "moi, déjà présent"},
		{ID: 2, ChatID: -100, FromID: 77, Text: "groupe"},
		{ID: 3, ChatID: 77, FromID: 77, Text: "bonjour"},
	}
	id, ok := pickNewChat([]int64{42}, ups)
	if !ok || id != 77 {
		t.Fatalf("got %d ok=%v", id, ok)
	}
	if _, ok := pickNewChat([]int64{42, 77}, ups); ok {
		t.Fatal("known accounts must be ignored")
	}
}
