package camera

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"github.com/caligone/openqiara/internal/config"
)

// fbxhomeXMLGlob is where fbxhome keeps its state.
const fbxhomeXMLGlob = "/data/fbxhome.xml.*"

var errNoFbxhomeXML = errors.New("no fbxhome.xml.N found")

var counterRe = regexp.MustCompile(`counter="(\d+)"`)

// fbxhomeStates reads fbxhome's state files, newest first. fbxhome writes
// fbxhome.xml.0..9 wear-levelled, not round-robin: the newest is the one
// with the highest counter="N", not .0. A file cut short (fbxhome stopped
// while writing it) is for the caller to skip: that is what the other
// nine are for.
func fbxhomeStates(glob string) ([][]byte, error) {
	matches, err := filepath.Glob(glob)
	if err != nil {
		return nil, fmt.Errorf("glob xml: %w", err)
	}
	type state struct {
		counter int
		data    []byte
	}
	var states []state
	for _, p := range matches {
		// Skip .bak and any file that's not strictly .N (digit).
		if _, err := strconv.Atoi(strings.TrimPrefix(filepath.Ext(p), ".")); err != nil {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		if m := counterRe.FindSubmatch(data); m != nil {
			c, _ := strconv.Atoi(string(m[1]))
			states = append(states, state{c, data})
		}
	}
	if len(states) == 0 {
		return nil, errNoFbxhomeXML
	}
	slices.SortFunc(states, func(a, b state) int { return b.counter - a.counter })
	out := make([][]byte, len(states))
	for i, st := range states {
		out[i] = st.data
	}
	return out, nil
}

// sensorTypeFromNodeType maps fbxhome's node types to sensor types.
var sensorTypeFromNodeType = map[string]string{
	"Node.DomusNode.HlDws": "DWS",
	"Node.DomusNode.HlPir": "PIR",
	"Node.DomusNode.HlSrn": "SRN",
	"Node.DomusNode.HLKpd": "KPD",
}

// fbxhomeRadio is what openqiarad takes from fbxhome's state when it
// takes the radio over. The sensors' keys it holds are not read.
type fbxhomeRadio struct {
	Adapter struct {
		NextAddr uint32 `xml:"domus_next_addr,attr"`
	} `xml:"Adapter"`
	Nodes []struct {
		ID          int           `xml:"id,attr"`
		Type        string        `xml:"type,attr"`
		Discarded   bool          `xml:"discarded,attr"`
		Addr        uint32        `xml:"domus_addr,attr"`
		UID         string        `xml:"domus_item_id,attr"`
		SystemIndex uint8         `xml:"system_index,attr"`
		Battery     int           `xml:"battery,attr"`
		Codes       []fbxhomeCode `xml:"Code"`
	} `xml:"Node"`
}

// fbxhomeCode is a keypad code fbxhome pushes.
type fbxhomeCode struct {
	Password string `xml:"password,attr"`
	Label    string `xml:"label,attr"`
	Valid    bool   `xml:"valid,attr"`
}

// ImportFbxhomeRadio gives each sensor the radio identity fbxhome knew it
// by, so that charmux mode serves the sensors fbxhome paired without
// pairing them again. Sensors match on their id, which is fbxhome's node
// id; the ones fbxhome knew and the config does not are added, unless
// they were deleted. A sensor that already has a radio identity keeps it.
// A keypad without a code in the config gets fbxhome's valid ones: the
// engine pushes the config's codes, and an empty list would let its off
// button alone disarm. It returns the ids imported, none without fbxhome
// state.
func ImportFbxhomeRadio(store *config.Store, glob string) ([]int, error) {
	states, err := fbxhomeStates(glob)
	if errors.Is(err, errNoFbxhomeXML) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var st fbxhomeRadio
	for _, data := range states {
		if err = xml.Unmarshal(data, &st); err == nil {
			break
		}
	}
	if err != nil {
		return nil, fmt.Errorf("parse fbxhome.xml: %w", err)
	}

	cfg := store.Get()
	var imported []int
	for _, n := range st.Nodes {
		typ := sensorTypeFromNodeType[n.Type]
		i := slices.IndexFunc(cfg.Sensors, func(se config.SensorEntry) bool { return se.ID == n.ID })
		if typ == "" || n.Discarded || n.Addr == 0 || slices.Contains(cfg.DeletedIDs, n.ID) ||
			i >= 0 && cfg.Sensors[i].Radio.Addr != 0 {
			continue
		}
		imported = append(imported, n.ID)
	}
	if len(imported) == 0 && cfg.RadioNextAddr >= st.Adapter.NextAddr {
		return nil, nil
	}
	err = store.Update(func(cfg *config.Config) {
		for _, n := range st.Nodes {
			if !slices.Contains(imported, n.ID) {
				continue
			}
			i := slices.IndexFunc(cfg.Sensors, func(se config.SensorEntry) bool { return se.ID == n.ID })
			if i < 0 {
				cfg.Sensors = append(cfg.Sensors, config.SensorEntry{ID: n.ID})
				i = len(cfg.Sensors) - 1
			}
			se := &cfg.Sensors[i]
			se.Type = sensorTypeFromNodeType[n.Type]
			se.Radio = config.RadioNode{Addr: n.Addr, UID: n.UID, SystemIndex: n.SystemIndex}
			se.Battery = n.Battery
			if se.Type == "KPD" && len(se.KPDCodes) == 0 {
				for _, c := range n.Codes {
					if c.Valid {
						se.KPDCodes = append(se.KPDCodes, config.KPDCode{Password: c.Password, Label: c.Label})
					}
				}
			}
		}
		cfg.RadioNextAddr = max(cfg.RadioNextAddr, st.Adapter.NextAddr)
	})
	return imported, err
}
