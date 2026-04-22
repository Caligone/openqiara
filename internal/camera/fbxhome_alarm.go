package camera

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
)

// AlarmTimings mirrors the configurable HlAlarm node attributes.
// Values are seconds; unset (zero) means "unknown / not yet read".
type AlarmTimings struct {
	TimeoutBeforeArmed int  `json:"timeout_before_armed"`
	TimeoutBeforeAlert int  `json:"timeout_before_alert"`
	TimeoutAlert       int  `json:"timeout_alert"`
	HistoryWhenArmed   bool `json:"history_when_armed"`
}

// ReadAlarmTimings parses the most recent fbxhome.xml rotation file to
// extract the HlAlarm node attributes. fbxhome's endpoints_read is ACL-
// blocked for node 2 even with a valid session, so we read the persisted
// XML directly. fbxhome rotates /data/fbxhome.xml.0..9 in a wear-levelled
// way, so we pick the file with the highest counter="N" attribute.
func ReadAlarmTimings() (AlarmTimings, error) {
	matches, err := filepath.Glob("/data/fbxhome.xml.*")
	if err != nil {
		return AlarmTimings{}, fmt.Errorf("glob xml: %w", err)
	}
	type rotated struct {
		path    string
		counter int
	}
	var rs []rotated
	cntRe := regexp.MustCompile(`counter="(\d+)"`)
	for _, p := range matches {
		// Skip .bak and any file that's not strictly .N (digit).
		base := filepath.Base(p)
		suf := base[len("fbxhome.xml."):]
		if _, err := strconv.Atoi(suf); err != nil {
			continue
		}
		data, err := os.ReadFile(p)
		if err != nil {
			continue
		}
		m := cntRe.FindSubmatch(data)
		if m == nil {
			continue
		}
		c, _ := strconv.Atoi(string(m[1]))
		rs = append(rs, rotated{path: p, counter: c})
	}
	if len(rs) == 0 {
		return AlarmTimings{}, fmt.Errorf("no fbxhome.xml.N found")
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].counter > rs[j].counter })
	data, err := os.ReadFile(rs[0].path)
	if err != nil {
		return AlarmTimings{}, fmt.Errorf("read latest xml: %w", err)
	}

	t := AlarmTimings{}
	intAttr := func(name string) int {
		re := regexp.MustCompile(name + `="(\d+)"`)
		m := re.FindSubmatch(data)
		if m == nil {
			return 0
		}
		v, _ := strconv.Atoi(string(m[1]))
		return v
	}
	boolAttr := func(name string) bool {
		re := regexp.MustCompile(name + `="(true|false)"`)
		m := re.FindSubmatch(data)
		return m != nil && string(m[1]) == "true"
	}
	t.TimeoutBeforeArmed = intAttr("timeout_before_armed")
	t.TimeoutBeforeAlert = intAttr("timeout_before_alert")
	t.TimeoutAlert = intAttr("timeout_alert")
	t.HistoryWhenArmed = boolAttr("history_when_armed")
	return t, nil
}

// WriteAlarmTimings pushes the given timings to fbxhome HlAlarm (node 2)
// via endpoints_write. Only non-zero/non-nil fields are written.
func (c *FbxhomeClient) WriteAlarmTimings(ctx context.Context, t AlarmTimings) error {
	var eps []EndpointWriteEntry
	if t.TimeoutBeforeArmed > 0 {
		eps = append(eps, EndpointWriteEntry{EPName: "timeout_before_armed", Value: t.TimeoutBeforeArmed})
	}
	if t.TimeoutBeforeAlert > 0 {
		eps = append(eps, EndpointWriteEntry{EPName: "timeout_before_alert", Value: t.TimeoutBeforeAlert})
	}
	if t.TimeoutAlert > 0 {
		eps = append(eps, EndpointWriteEntry{EPName: "timeout_alert", Value: t.TimeoutAlert})
	}
	// HistoryWhenArmed is a bool — always pushed if any timing is also
	// being updated (caller passes the full snapshot).
	if len(eps) > 0 {
		eps = append(eps, EndpointWriteEntry{EPName: "history_when_armed", Value: t.HistoryWhenArmed})
	}
	if len(eps) == 0 {
		return nil
	}
	return c.EndpointsWrite(ctx, 2, eps)
}
