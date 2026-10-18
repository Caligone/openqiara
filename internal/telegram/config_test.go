package telegram

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func writeCfg(t *testing.T, content string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "telegram.json")
	if err := os.WriteFile(p, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestLoadConfigMissing(t *testing.T) {
	cfg, ok, err := LoadConfig(filepath.Join(t.TempDir(), "absent.json"))
	if ok || err != nil || cfg.BotToken != "" || cfg.ChatIDs != nil {
		t.Fatalf("got %+v, %v, %v", cfg, ok, err)
	}
}

func TestLoadConfigValid(t *testing.T) {
	cfg, ok, err := LoadConfig(writeCfg(t, `{"bot_token":"123:abc","chat_id":42}`))
	if err != nil || !ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
	if cfg.BotToken != "123:abc" || cfg.ChatID != 0 || !reflect.DeepEqual(cfg.ChatIDs, []int64{42}) {
		t.Fatalf("got %+v", cfg)
	}
}

func TestLoadConfigChatIDs(t *testing.T) {
	cfg, ok, err := LoadConfig(writeCfg(t, `{"bot_token":"t","chat_ids":[42,77]}`))
	if err != nil || !ok || !reflect.DeepEqual(cfg.ChatIDs, []int64{42, 77}) {
		t.Fatalf("got %+v ok=%v err=%v", cfg, ok, err)
	}
}

func TestLoadConfigMergesLegacyDeduplicated(t *testing.T) {
	cfg, ok, err := LoadConfig(writeCfg(t, `{"bot_token":"t","chat_id":42,"chat_ids":[77,42,77]}`))
	if err != nil || !ok || cfg.ChatID != 0 || !reflect.DeepEqual(cfg.ChatIDs, []int64{42, 77}) {
		t.Fatalf("got %+v ok=%v err=%v", cfg, ok, err)
	}
}

func TestLoadConfigInvalidJSON(t *testing.T) {
	if _, ok, err := LoadConfig(writeCfg(t, `{not json`)); err == nil || ok {
		t.Fatalf("ok=%v err=%v", ok, err)
	}
}

func TestLoadConfigIncomplete(t *testing.T) {
	const secret = "999:secrettoken"
	for name, content := range map[string]string{
		"empty token":  `{"bot_token":"","chat_id":5}`,
		"no chat":      `{"bot_token":"` + secret + `","chat_id":0}`,
		"group chat":   `{"bot_token":"x","chat_id":-42}`,
		"empty list":   `{"bot_token":"x","chat_ids":[]}`,
		"bad in list":  `{"bot_token":"` + secret + `","chat_ids":[42,-7]}`,
		"zero in list": `{"bot_token":"x","chat_ids":[0]}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, ok, err := LoadConfig(writeCfg(t, content))
			if err == nil || ok {
				t.Fatalf("ok=%v err=%v", ok, err)
			}
			if strings.Contains(err.Error(), secret) {
				t.Fatalf("error leaks token: %v", err)
			}
		})
	}
}

func TestLoadConfigParseErrorNoToken(t *testing.T) {
	const secret = "999:secrettoken"
	_, _, err := LoadConfig(writeCfg(t, `{"bot_token":"`+secret+`","chat_id":"x"}`))
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("err=%v", err)
	}
}
