// Package domus implements the DomusRF 868MHz radio protocol
// for pairing and communicating with Qiara sensors.
package domus

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"os"
	"strings"
)

// VendorKey holds a named vendor key used for sensor pairing.
type VendorKey struct {
	Name string
	Key  [32]byte
}

// LoadVendorKeys reads vendor keys from the camera's key file.
// Format: "name: base64encodedkey" per line.
func LoadVendorKeys(path string) ([]VendorKey, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open vendor keys: %w", err)
	}
	defer func() { _ = f.Close() }()

	var keys []VendorKey
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || line[0] == '#' {
			continue
		}
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}
		name := strings.TrimSpace(parts[0])
		b64 := strings.TrimSpace(parts[1])

		decoded, err := base64.StdEncoding.DecodeString(b64)
		if err != nil {
			continue
		}
		if len(decoded) != 32 {
			continue
		}

		var key VendorKey
		key.Name = name
		copy(key.Key[:], decoded)
		keys = append(keys, key)
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("read vendor keys: %w", err)
	}
	return keys, nil
}

// MatchBeacon finds the vendor key matching a beacon's prefix (first 6 bytes after opcode).
func MatchBeacon(beacon []byte, keys []VendorKey) (VendorKey, bool) {
	if len(beacon) < 7 {
		return VendorKey{}, false
	}
	prefix := beacon[1:7] // bytes 1-6 of beacon are vendor prefix
	for _, k := range keys {
		if matchPrefix(prefix, k.Key[:6]) {
			return k, true
		}
	}
	return VendorKey{}, false
}

func matchPrefix(a, b []byte) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
