package rtspserver

import (
	"bytes"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/bluenviron/gortsplib/v5/pkg/format"
)

var (
	testSPS = []byte{0x67, 0x42, 0xc0, 0x28, 0xda, 0x01, 0xe0}
	testPPS = []byte{0x68, 0xce, 0x3c, 0x80}
)

func TestParamsRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "params")
	if _, _, ok := loadParams(path); ok {
		t.Fatal("missing file loaded")
	}
	if err := saveParams(path, testSPS, testPPS); err != nil {
		t.Fatal(err)
	}
	sps, pps, ok := loadParams(path)
	if !ok || !bytes.Equal(sps, testSPS) || !bytes.Equal(pps, testPPS) {
		t.Fatalf("round trip: %x %x %v", sps, pps, ok)
	}
	if _, err := os.Stat(path + ".tmp"); !os.IsNotExist(err) {
		t.Fatalf("temporary file left behind: %v", err)
	}
}

func TestLoadParamsRejectsGarbage(t *testing.T) {
	path := filepath.Join(t.TempDir(), "params")
	for _, data := range []string{"", "Zm9v\n", "!!\n!!\n", "aM48gA==\nZ0LAKNoB4A==\n"} { // last: PPS, SPS swapped
		if err := os.WriteFile(path, []byte(data), 0o644); err != nil {
			t.Fatal(err)
		}
		if _, _, ok := loadParams(path); ok {
			t.Fatalf("%q loaded", data)
		}
	}
}

// The SPS/PPS seen on the stream reach the DESCRIBE parameters and the
// file; an AU with the same sets again changes nothing.
func TestUpdateParamsAdvertisesAndPersists(t *testing.T) {
	path := filepath.Join(t.TempDir(), "params")
	s := &Server{
		cfg:   Config{ParamsFile: path},
		log:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		forma: &format.H264{PayloadTyp: 96, PacketizationMode: 1},
	}
	s.updateParams(testSPS, nil)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("persisted without a PPS")
	}
	s.updateParams(nil, testPPS)
	fmtp := s.forma.FMTP()
	if fmtp["profile-level-id"] != "42C028" || fmtp["sprop-parameter-sets"] == "" {
		t.Fatalf("FMTP not updated: %v", fmtp)
	}
	if _, _, ok := loadParams(path); !ok {
		t.Fatal("parameter sets not persisted")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	s.updateParams(testSPS, testPPS)
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("unchanged parameter sets written again")
	}
}
