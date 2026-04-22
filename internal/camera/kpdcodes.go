package camera

import (
	"fmt"
	"os"
	"regexp"
	"strings"
	"time"
)

// KPDCode represents a code stored in the fbxhome XML config.
type KPDCode struct {
	Password     string `json:"password"`
	Label        string `json:"label"`
	CreationDate int64  `json:"creation_date"`
	Valid        bool   `json:"valid"`
}

var codeLineRe = regexp.MustCompile(`<Code\s+creation_date="(\d+)"\s+label="([^"]*)"\s+password="([^"]*)"\s+valid="(true|false)"\s*/>`)

// ReadKPDCodes parses KPD codes from the fbxhome XML config file.
func ReadKPDCodes(xmlPath string) ([]KPDCode, error) {
	data, err := os.ReadFile(xmlPath)
	if err != nil {
		return nil, fmt.Errorf("read xml: %w", err)
	}

	var codes []KPDCode
	for _, m := range codeLineRe.FindAllStringSubmatch(string(data), -1) {
		var date int64
		fmt.Sscanf(m[1], "%d", &date)
		codes = append(codes, KPDCode{
			Password:     m[3],
			Label:        m[2],
			CreationDate: date,
			Valid:        m[4] == "true",
		})
	}
	return codes, nil
}

// AddKPDCode adds a code to the KPD node in the fbxhome XML config.
func AddKPDCode(xmlPath string, password, label string) error {
	data, err := os.ReadFile(xmlPath)
	if err != nil {
		return fmt.Errorf("read xml: %w", err)
	}
	content := string(data)

	codeLine := fmt.Sprintf(
		`    <Code creation_date="%d" label="%s" password="%s" valid="true" />`,
		time.Now().Unix(), label, password,
	)

	// Insert before the closing </Node> of the KPD node.
	kpdIdx := strings.Index(content, `type="Node.DomusNode.HLKpd"`)
	if kpdIdx < 0 {
		return fmt.Errorf("KPD node not found in %s", xmlPath)
	}

	closeIdx := strings.Index(content[kpdIdx:], "</Node>")
	if closeIdx < 0 {
		return fmt.Errorf("closing </Node> not found for KPD node")
	}
	insertAt := kpdIdx + closeIdx

	// Match original XML indentation: "  </Node>" with code lines at "    " (4 spaces)
	newContent := content[:insertAt] + codeLine + "\n  " + content[insertAt:]

	// Normalize: ensure all <Code> lines inside KPD node have consistent 4-space indent
	return os.WriteFile(xmlPath, []byte(newContent), 0644)
}

// DeleteKPDCode removes a code by password from the KPD node in the fbxhome XML config.
func DeleteKPDCode(xmlPath string, password string) error {
	data, err := os.ReadFile(xmlPath)
	if err != nil {
		return fmt.Errorf("read xml: %w", err)
	}

	lines := strings.Split(string(data), "\n")
	found := false
	var result []string
	for _, line := range lines {
		if !found && strings.Contains(line, "<Code ") && strings.Contains(line, fmt.Sprintf(`password="%s"`, password)) {
			found = true
			continue // skip this line
		}
		result = append(result, line)
	}

	if !found {
		return fmt.Errorf("code with password %q not found", password)
	}

	return os.WriteFile(xmlPath, []byte(strings.Join(result, "\n")), 0644)
}
