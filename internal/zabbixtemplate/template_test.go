package zabbixtemplate

import (
	"os"
	"strings"
	"testing"
)

// adrItemKeys are the item keys ADR-0161 names.
//
// This list is copied from the ADR's own sentence, not from the
// template. That is the point: if the template and the ADR disagree,
// one of these two tests fails and a human decides which is right.
var adrItemKeys = []string{
	"apiary.cluster.health.status",
	"apiary.node[{#NODEID}].health.status",
	"apiary.node[{#NODEID}].cpu.load1",
	"apiary.node[{#NODEID}].memory.available_bytes",
	"apiary.node[{#NODEID}].pool.worst_capacity_percent",
	"apiary.node[{#NODEID}].disk.worst_health",
}

func load(t *testing.T) *Template {
	t.Helper()
	tmpl, err := LoadDefault()
	if err != nil {
		t.Fatalf("loading the template: %v", err)
	}
	return tmpl
}

func TestTemplate_ExistsAtTheExpectedPath(t *testing.T) {
	if _, err := os.Stat(TemplatePath); err != nil {
		t.Fatalf("the template is not at %s: %v", TemplatePath, err)
	}
}

func TestTemplate_ExportsForZabbix7(t *testing.T) {
	tmpl := load(t)
	if tmpl.Version != "7.0" {
		t.Errorf("the export version is %q, want 7.0", tmpl.Version)
	}
}

func TestTemplate_ItemKeysMatchADR0161(t *testing.T) {
	tmpl := load(t)
	got := map[string]bool{}
	for _, key := range tmpl.ItemKeys() {
		got[key] = true
	}
	// Every key the ADR names must exist.
	for _, want := range adrItemKeys {
		if !got[want] {
			t.Errorf("the ADR names the item key %q, but the template does not have it; the template's keys are: %s", want, tmpl)
		}
	}
	// And no key may exist that the ADR does not name.
	for _, key := range tmpl.ItemKeys() {
		if key == "apiary.cluster.report" || key == "apiary.node.discovery" {
			// The two structural keys: the master item and the
			// discovery rule itself. They are machinery, not
			// measurements.
			continue
		}
		known := false
		for _, want := range adrItemKeys {
			if key == want {
				known = true
				break
			}
		}
		if !known {
			t.Errorf("the template has the item key %q, which ADR-0161 does not name; either the ADR or the template is wrong", key)
		}
	}
}

func TestTemplate_UsesTheADRsLLDMacro(t *testing.T) {
	tmpl := load(t)
	if len(tmpl.Rules) == 0 {
		t.Fatal("the template has no discovery rule")
	}
	for _, rule := range tmpl.Rules {
		if len(rule.LLDMacroPaths) == 0 {
			t.Errorf("the discovery rule %q maps no LLD macro", rule.Key)
			continue
		}
		for _, path := range rule.LLDMacroPaths {
			if path.Macro != "{#NODEID}" {
				t.Errorf("the discovery rule %q maps the LLD macro %q, want {#NODEID} - ADR-0161 names that macro, and the per-node item keys are built from it", rule.Key, path.Macro)
			}
		}
	}
}

func TestTemplate_HoldsTheCredentialInASecretMacro(t *testing.T) {
	tmpl := load(t)
	macro, ok := tmpl.MacroByName("{$APIARY.API_KEY}")
	if !ok {
		t.Fatal("the template has no {$APIARY.API_KEY} macro")
	}
	// SECRET_TEXT is what makes Zabbix mask the value in the
	// frontend, in exports, and in its logs. A plain macro would put
	// the API key in every exported YAML file.
	if macro.Type != "SECRET_TEXT" {
		t.Errorf("{$APIARY.API_KEY} has the type %q, want SECRET_TEXT", macro.Type)
	}
	if macro.Value != "" {
		t.Errorf("{$APIARY.API_KEY} carries a value in the template; a credential must never be committed")
	}
}

func TestTemplate_VerifiesTLS(t *testing.T) {
	tmpl := load(t)
	for _, item := range tmpl.AllItems() {
		if item.URL == "" {
			continue
		}
		// Zabbix's own default for verify_peer and verify_host is
		// OFF. A template that does not state otherwise accepts any
		// certificate, which ADR-0161 forbids outright.
		if item.VerifyPeer != "1" {
			t.Errorf("the item %q reads %q with verify_peer %q, want 1; Zabbix defaults this OFF, so leaving it out disables certificate validation", item.Key, item.URL, item.VerifyPeer)
		}
		if item.VerifyHost != "1" {
			t.Errorf("the item %q reads %q with verify_host %q, want 1; Zabbix defaults this OFF, so leaving it out disables hostname validation", item.Key, item.URL, item.VerifyHost)
		}
	}
}

func TestTemplate_ReadsOnlyOverHTTPS(t *testing.T) {
	tmpl := load(t)
	// The base URL is a macro, so the check is made on the macro's
	// own default value and on the template's shape: the URL is
	// "{$APIARY.URL}" followed by a path, with no scheme of its own.
	// A template that hard-coded "http://" would be caught here.
	base, ok := tmpl.MacroByName("{$APIARY.URL}")
	if !ok {
		t.Fatal("the template has no {$APIARY.URL} macro")
	}
	if !strings.HasPrefix(base.Value, "https://") {
		t.Errorf("{$APIARY.URL} defaults to %q; ADR-0161 allows no plaintext mode", base.Value)
	}
	for _, url := range tmpl.URLs() {
		if strings.HasPrefix(url, "http://") {
			t.Errorf("the template reads %q over plaintext; ADR-0161 allows no plaintext mode", url)
		}
		if !strings.HasPrefix(url, "{$APIARY.URL}/") {
			t.Errorf("the template reads %q, which does not use the {$APIARY.URL} base macro", url)
		}
	}
}

func TestTemplate_UsesTheMonitoringEndpoints(t *testing.T) {
	tmpl := load(t)
	var found bool
	for _, url := range tmpl.URLs() {
		if strings.Contains(url, "/v1/monitoring/health") || strings.Contains(url, "/v1/monitoring/nodes") {
			found = true
		}
	}
	if !found {
		t.Errorf("the template reads none of ADR-0160's monitoring endpoints; the URLs are %v", tmpl.URLs())
	}
}

func TestTemplate_IsReadOnly(t *testing.T) {
	tmpl := load(t)
	// Every item in this template is an HTTP_AGENT or a DEPENDENT.
	// There is no SCRIPT, no TRAPPER, and no command item, so there is
	// nothing here that can change Apiary's state.
	for _, item := range tmpl.AllItems() {
		switch item.Type {
		case "HTTP_AGENT", "DEPENDENT":
		default:
			t.Errorf("the item %q has the type %q; this template is read-only, so only HTTP_AGENT and DEPENDENT items belong in it", item.Key, item.Type)
		}
	}
	// And no item may address a mutating path.
	for _, url := range tmpl.URLs() {
		for _, verb := range []string{"POST", "PUT", "DELETE", "PATCH"} {
			if strings.Contains(url, verb) {
				t.Errorf("the template reads %q, which names the mutating verb %s", url, verb)
			}
		}
	}
}

func TestTemplate_SendsTheCredentialInTheAuthorizationHeader(t *testing.T) {
	tmpl := load(t)
	for _, item := range tmpl.AllItems() {
		if item.URL == "" {
			continue
		}
		var found bool
		for _, header := range item.Headers {
			if strings.EqualFold(header.Name, "Authorization") {
				found = true
				if !strings.Contains(header.Value, "{$APIARY.API_KEY}") {
					t.Errorf("the Authorization header of %q is %q, want it to carry {$APIARY.API_KEY}", item.Key, header.Value)
				}
			}
		}
		if !found {
			t.Errorf("the item %q reads %q without an Authorization header", item.Key, item.URL)
		}
	}
}

func TestTemplate_PerNodeItemsSelectTheirOwnNode(t *testing.T) {
	tmpl := load(t)
	for _, rule := range tmpl.Rules {
		for _, item := range rule.Items {
			if !strings.Contains(item.Key, "{#NODEID}") {
				continue
			}
			// Every per-node item must select its own row out of the
			// report's node array. Without the selection, every node
			// reports the CLUSTER's numbers, which looks like working
			// monitoring and is not.
			var selects bool
			for _, step := range item.Preprocessing {
				for _, param := range step.Parameters {
					if strings.Contains(param, "@.node_id == \"{#NODEID}\"") {
						selects = true
					}
				}
			}
			if !selects {
				t.Errorf("the per-node item %q does not select its own row from the report; every node would report the colony's values", item.Key)
			}
		}
	}
}

func TestTemplate_HasAnUnavailableTriggerSeparateFromTheUnhealthyOne(t *testing.T) {
	tmpl := load(t)
	for _, rule := range tmpl.Rules {
		var unhealthy, unavailable bool
		for _, trigger := range rule.Triggers {
			switch {
			case strings.Contains(trigger.Expression, "nodata("):
				unavailable = true
			case strings.Contains(trigger.Expression, "health.status"):
				unhealthy = true
			}
		}
		if !unhealthy {
			t.Errorf("the discovery rule %q has no trigger for a Comb whose health is not healthy", rule.Key)
		}
		if !unavailable {
			t.Errorf("the discovery rule %q has no trigger for a Comb whose observations stopped arriving", rule.Key)
		}
	}
}

func TestTemplate_PollsTheColonyOncePerInterval(t *testing.T) {
	tmpl := load(t)
	// Exactly one item performs an HTTP request. Every other item is
	// dependent on it, so a Colony of fifty Combs is polled once per
	// interval rather than fifty times.
	var agents int
	for _, item := range tmpl.AllItems() {
		if item.Type == "HTTP_AGENT" {
			agents++
		}
	}
	if agents != 1 {
		t.Errorf("the template has %d HTTP agent items, want exactly 1; more than one means the colony is polled repeatedly per interval", agents)
	}
}
