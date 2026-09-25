package nodeconfig

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"time"
)

// ChangeOrigin records how an operator says a configuration change began.
// It is intentionally explicit: Apiary never infers an incident or
// automation origin from a value or a nearby event.
type ChangeOrigin string

const (
	OriginDefault    ChangeOrigin = "default"
	OriginOperator   ChangeOrigin = "operator"
	OriginMigration  ChangeOrigin = "migration"
	OriginIncident   ChangeOrigin = "incident"
	OriginAutomation ChangeOrigin = "automation"
)

// ConfigChange is an append-only local record. Secret values are always
// redacted; history is evidence supplied at change time, not proof that the
// rationale remains valid today.
type ConfigChange struct {
	Field     string       `json:"field"`
	Previous  string       `json:"previous,omitempty"`
	Current   string       `json:"current,omitempty"`
	Origin    ChangeOrigin `json:"origin"`
	Rationale string       `json:"rationale,omitempty"`
	Evidence  string       `json:"evidence,omitempty"`
	ChangedAt time.Time    `json:"changed_at"`
	Attested  bool         `json:"attested,omitempty"`
}

func (m *Manager) History() ([]ConfigChange, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.readHistory()
}

func (m *Manager) readHistory() ([]ConfigChange, error) {
	body, err := os.ReadFile(m.historyPath())
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var changes []ConfigChange
	if err := json.Unmarshal(body, &changes); err != nil {
		return nil, fmt.Errorf("nodeconfig: decode history: %w", err)
	}
	return changes, nil
}

// RecordChanges records only values that actually changed. The file is
// rewritten atomically so readers see either the old or complete journal.
func (m *Manager) RecordChanges(before, after Config, origin ChangeOrigin, rationale, evidence string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.recordChanges(before, after, origin, rationale, evidence)
}

func (m *Manager) recordChanges(before, after Config, origin ChangeOrigin, rationale, evidence string) error {
	if !validOrigin(origin) {
		return fmt.Errorf("nodeconfig: invalid change origin %q", origin)
	}
	if len(rationale) > 2048 || len(evidence) > 2048 {
		return fmt.Errorf("nodeconfig: rationale and evidence must be at most 2048 characters")
	}
	oldValues, err := configValues(before)
	if err != nil {
		return err
	}
	newValues, err := configValues(after)
	if err != nil {
		return err
	}
	keys := make([]string, 0, len(newValues)+len(oldValues))
	for key := range newValues {
		keys = append(keys, key)
	}
	for key := range oldValues {
		if _, exists := newValues[key]; !exists {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	now := time.Now().UTC()
	var added []ConfigChange
	for _, key := range keys {
		oldValue, newValue := oldValues[key], newValues[key]
		if oldValue == newValue {
			continue
		}
		if secretConfigField(key) {
			oldValue, newValue = redactedConfigValue(oldValues[key]), redactedConfigValue(newValues[key])
		} else {
			oldValue, newValue = normalizedConfigValue(oldValue), normalizedConfigValue(newValue)
		}
		added = append(added, ConfigChange{Field: key, Previous: oldValue, Current: newValue, Origin: origin, Rationale: strings.TrimSpace(rationale), Evidence: strings.TrimSpace(evidence), ChangedAt: now})
	}
	if len(added) == 0 {
		return nil
	}
	return m.appendChanges(added)
}

// RecordCurrent adds a retrospective operator attestation for one current
// field value without changing managerd.json.
func (m *Manager) RecordCurrent(field string, origin ChangeOrigin, rationale, evidence string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ValidateOrigin(origin); err != nil {
		return err
	}
	rationale, evidence = strings.TrimSpace(rationale), strings.TrimSpace(evidence)
	if rationale == "" {
		return fmt.Errorf("nodeconfig: retrospective rationale is required")
	}
	if len(rationale) > 2048 || len(evidence) > 2048 {
		return fmt.Errorf("nodeconfig: rationale and evidence must be at most 2048 characters")
	}
	if !knownConfigField(field) {
		return fmt.Errorf("nodeconfig: unknown configuration field %q", field)
	}
	cfg, err := m.Load()
	if err != nil {
		return err
	}
	values, err := configValues(cfg)
	if err != nil {
		return err
	}
	value, exists := values[field]
	if !exists {
		value = ""
	}
	if secretConfigField(field) {
		value = redactedConfigValue(value)
	} else {
		value = normalizedConfigValue(value)
	}
	return m.appendChanges([]ConfigChange{{Field: field, Previous: value, Current: value, Origin: origin, Rationale: rationale, Evidence: evidence, ChangedAt: time.Now().UTC(), Attested: true}})
}

func knownConfigField(name string) bool {
	t := reflect.TypeOf(Config{})
	for i := 0; i < t.NumField(); i++ {
		field := strings.Split(t.Field(i).Tag.Get("json"), ",")[0]
		if field == name {
			return true
		}
	}
	return false
}

func (m *Manager) appendChanges(added []ConfigChange) error {
	history, err := m.readHistory()
	if err != nil {
		return err
	}
	history = append(history, added...)
	body, err := json.MarshalIndent(history, "", "  ")
	if err != nil {
		return err
	}
	return atomicWriteFile(m.historyPath(), body)
}

// SaveWithHistory serializes one local config update with its provenance
// record. If writing the history journal fails, it restores the prior config
// file (or removes a newly created one) before returning the error.
func (m *Manager) SaveWithHistory(cfg Config, origin ChangeOrigin, rationale, evidence string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if err := ValidateOrigin(origin); err != nil {
		return err
	}
	if len(rationale) > 2048 || len(evidence) > 2048 {
		return fmt.Errorf("nodeconfig: rationale and evidence must be at most 2048 characters")
	}
	before, err := m.Load()
	if err != nil {
		return err
	}
	oldBody, readErr := os.ReadFile(m.path())
	wasMissing := os.IsNotExist(readErr)
	if readErr != nil && !wasMissing {
		return readErr
	}
	if err := m.save(cfg); err != nil {
		return err
	}
	if err := m.recordChanges(before, cfg, origin, rationale, evidence); err != nil {
		var rollbackErr error
		if wasMissing {
			rollbackErr = os.Remove(m.path())
			if os.IsNotExist(rollbackErr) {
				rollbackErr = nil
			}
		} else {
			rollbackErr = atomicWriteFile(m.path(), oldBody)
		}
		if rollbackErr != nil {
			return fmt.Errorf("recording config history: %v; restoring config: %w", err, rollbackErr)
		}
		return fmt.Errorf("recording config history; config update rolled back: %w", err)
	}
	return nil
}

func (m *Manager) historyPath() string {
	return filepath.Join(filepath.Dir(m.path()), "managerd.config-history.json")
}

func configValues(cfg Config) (map[string]string, error) {
	body, err := json.Marshal(cfg)
	if err != nil {
		return nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, err
	}
	values := make(map[string]string, len(raw))
	for key, value := range raw {
		values[key] = string(value)
	}
	return values, nil
}

func secretConfigField(key string) bool {
	return key == "peer_api_key" || key == "raftd_token"
}

func redactedConfigValue(value string) string {
	if value == "\"\"" || value == "null" || value == "" {
		return "(unset)"
	}
	return "(set; value redacted)"
}

func normalizedConfigValue(value string) string {
	if value == "" || value == "\"\"" || value == "null" {
		return "(unset)"
	}
	return value
}

func validOrigin(origin ChangeOrigin) bool {
	switch origin {
	case OriginDefault, OriginOperator, OriginMigration, OriginIncident, OriginAutomation:
		return true
	default:
		return false
	}
}

func ValidateOrigin(origin ChangeOrigin) error {
	if !validOrigin(origin) {
		return fmt.Errorf("nodeconfig: invalid change origin %q", origin)
	}
	return nil
}
