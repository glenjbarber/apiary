package nodeconfig

import (
	"os"
	"path/filepath"
	"testing"
)

func TestSaveWithHistoryRecordsChangedFieldsAndRedactsSecrets(t *testing.T) {
	path := filepath.Join(t.TempDir(), "managerd.json")
	m := &Manager{Path: path}
	before := Config{Uplink: "re0", DNSServer: "10.0.0.1", PeerAPIKey: "old-secret", RaftdToken: "old-token"}
	if err := m.Save(before); err != nil {
		t.Fatal(err)
	}
	after := before
	after.Uplink = "em0"
	after.DNSServer = ""
	after.PeerAPIKey = "new-secret"
	after.RaftdToken = "new-token"
	if err := m.SaveWithHistory(after, OriginIncident, "Restore the uplink after the switch replacement.", "INC-42"); err != nil {
		t.Fatal(err)
	}
	changes, err := m.History()
	if err != nil {
		t.Fatal(err)
	}
	if len(changes) != 4 {
		t.Fatalf("got %d changes, want 4: %+v", len(changes), changes)
	}
	foundClear := false
	for _, change := range changes {
		if change.Origin != OriginIncident || change.Rationale == "" || change.Evidence != "INC-42" {
			t.Errorf("metadata missing: %+v", change)
		}
		if change.Field == "peer_api_key" || change.Field == "raftd_token" {
			if change.Previous == "old-secret" || change.Current == "new-secret" || change.Previous == "old-token" || change.Current == "new-token" {
				t.Errorf("secret leaked into history: %+v", change)
			}
		}
		if change.Field == "dhcp_dns_server" {
			foundClear = change.Previous == `"10.0.0.1"` && change.Current == "(unset)"
		}
	}
	if !foundClear {
		t.Errorf("clearing an omitted field was not recorded: %+v", changes)
	}
	body, err := os.ReadFile(m.historyPath())
	if err != nil {
		t.Fatal(err)
	}
	if string(body) == "" {
		t.Fatal("history file is empty")
	}
	for _, secret := range []string{"old-secret", "new-secret", "old-token", "new-token"} {
		if string(body) != "" && contains(string(body), secret) {
			t.Errorf("history file contains secret %q", secret)
		}
	}
}

func TestSaveWithHistoryRejectsInvalidOriginWithoutChangingConfig(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "managerd.json")}
	want := Config{Uplink: "re0"}
	if err := m.Save(want); err != nil {
		t.Fatal(err)
	}
	if err := m.SaveWithHistory(Config{Uplink: "em0"}, ChangeOrigin("guessed"), "", ""); err == nil {
		t.Fatal("SaveWithHistory accepted an unknown origin")
	}
	got, err := m.Load()
	if err != nil {
		t.Fatal(err)
	}
	if got.Uplink != want.Uplink {
		t.Fatalf("config changed to %q after rejected provenance", got.Uplink)
	}
}

func TestRecordCurrentAttestsWithoutChangingConfig(t *testing.T) {
	m := &Manager{Path: filepath.Join(t.TempDir(), "managerd.json")}
	want := Config{BhyveBridge: "bridge0"}
	if err := m.Save(want); err != nil {
		t.Fatal(err)
	}
	if err := m.RecordCurrent("bhyve_bridge", OriginOperator, "Keep legacy guests isolated", "OPS-8"); err != nil {
		t.Fatal(err)
	}
	got, err := m.Load()
	if err != nil || got.BhyveBridge != want.BhyveBridge {
		t.Fatalf("config after attestation = %+v, %v; want unchanged bridge %q", got, err, want.BhyveBridge)
	}
	history, err := m.History()
	if err != nil || len(history) != 1 {
		t.Fatalf("history = %+v, %v; want one attestation", history, err)
	}
	if !history[0].Attested || history[0].Previous != history[0].Current || history[0].Current != `"bridge0"` {
		t.Fatalf("history entry = %+v, want marked attestation of current value", history[0])
	}
}

func contains(value, fragment string) bool {
	for i := 0; i+len(fragment) <= len(value); i++ {
		if value[i:i+len(fragment)] == fragment {
			return true
		}
	}
	return false
}
