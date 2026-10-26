// Package zabbixtemplate reads the Apiary Zabbix template and answers
// questions about it.
//
// WHY THIS EXISTS. The template and ADR-0161 make a contract with each
// other: the ADR names the item keys, the macros, the polling interval
// and the security properties, and the template is the artefact that
// actually implements them. A template that drifts from its ADR is the
// worst kind of documentation drift, because the template RUNS: a
// renamed macro or a dropped item does not fail loudly, it fails by
// silently monitoring nothing.
//
// So the contract is checked by a test rather than trusted to review.
// The checks here are the ones that would otherwise only be caught in
// production:
//
//   - Every item key the ADR names exists in the template, and every
//     key the template has is one the ADR names. Both directions
//     matter: a missing key is a silently unmonitored metric, and an
//     extra key is a contract nobody agreed to.
//
//   - The API credential is a SECRET macro, so Zabbix does not print
//     it in the frontend, in an exported YAML, or in a log line.
//
//   - TLS certificate verification is ENABLED on the HTTP agent.
//     Zabbix's own default for verify_peer is OFF, so a template that
//     does not say so is quietly accepting any certificate - which
//     ADR-0161 forbids outright.
//
//   - Every URL is HTTPS. ADR-0161 allows no plaintext mode.
//
//   - The template is read-only: no item's URL addresses a mutating
//     endpoint, and no item key implies a write.
//
//   - The LLD macro is the one the ADR names, so the per-node item
//     keys are exactly the keys the ADR documents.
package zabbixtemplate

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

// TemplatePath is where the template lives, relative to the
// repository root.
//
// A test that reads a template by a path it hard-codes would be a
// test that passes on the machine that wrote it and fails on the
// machine that reads it, so the path is derived from this file's own
// location instead. The package therefore has to live two directories
// below the repository root, which is where it is.
const TemplatePath = "../../etc/zabbix/apiary-zabbix-template.yaml"

// Template is the parsed Zabbix export.
//
// Only the parts this codebase reasons about are typed. The full
// Zabbix export schema is large, and typing all of it would mean
// maintaining a copy of Zabbix's schema that goes stale and tells us
// nothing. What matters here is the shape of the items, the discovery
// rules and the macros.
type Template struct {
	Version string
	Groups  []string
	Items   []Item
	Rules   []DiscoveryRule
	Macros  []Macro
}

// Item is one item or item prototype.
type Item struct {
	Name          string
	Key           string
	Type          string
	MasterKey     string
	URL           string
	Headers       []Header
	Preprocessing []Preprocessing
	VerifyPeer    string
	VerifyHost    string
	Units         string
	ValueType     string
	Delay         string
}

// Header is one HTTP header.
type Header struct {
	Name  string
	Value string
}

// Preprocessing is one preprocessing step.
type Preprocessing struct {
	Type       string
	Parameters []string
}

// DiscoveryRule is one low-level discovery rule.
type DiscoveryRule struct {
	Name          string
	Key           string
	Type          string
	MasterKey     string
	LLDMacroPaths []LLDMacroPath
	Items         []Item
	Triggers      []Trigger
}

// LLDMacroPath maps an LLD macro to a JSONPath.
type LLDMacroPath struct {
	Macro string
	Path  string
}

// Trigger is one trigger or trigger prototype.
type Trigger struct {
	Name       string
	Expression string
}

// Macro is one template macro.
type Macro struct {
	Name        string
	Type        string
	Value       string
	Description string
}

// Load reads and parses the template at path.
func Load(path string) (*Template, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("zabbixtemplate: reading %s: %w", path, err)
	}
	return Parse(data)
}

// LoadDefault reads the template this package is shipped with.
func LoadDefault() (*Template, error) {
	return Load(filepath.FromSlash(TemplatePath))
}

// Parse parses a Zabbix YAML export.
func Parse(data []byte) (*Template, error) {
	var doc struct {
		Export struct {
			Version string `yaml:"version"`
			Groups  []struct {
				Name string `yaml:"name"`
			} `yaml:"template_groups"`
			Templates []struct {
				Template string `yaml:"template"`
				Items    []struct {
					Name       string `yaml:"name"`
					Key        string `yaml:"key"`
					Type       string `yaml:"type"`
					URL        string `yaml:"url"`
					Units      string `yaml:"units"`
					ValueType  string `yaml:"value_type"`
					Delay      string `yaml:"delay"`
					VerifyPeer string `yaml:"verify_peer"`
					VerifyHost string `yaml:"verify_host"`
					Headers    []struct {
						Name  string `yaml:"name"`
						Value string `yaml:"value"`
					} `yaml:"headers"`
					Preprocessing []struct {
						Type       string    `yaml:"type"`
						Parameters yamlValue `yaml:"parameters"`
					} `yaml:"preprocessing"`
					MasterItem struct {
						Key string `yaml:"key"`
					} `yaml:"master_item"`
				} `yaml:"items"`
				DiscoveryRules []struct {
					Name  string `yaml:"name"`
					Key   string `yaml:"key"`
					Type  string `yaml:"type"`
					Items []struct {
						Name       string `yaml:"name"`
						Key        string `yaml:"key"`
						Type       string `yaml:"type"`
						Units      string `yaml:"units"`
						ValueType  string `yaml:"value_type"`
						Delay      string `yaml:"delay"`
						VerifyPeer string `yaml:"verify_peer"`
						VerifyHost string `yaml:"verify_host"`
						Headers    []struct {
							Name  string `yaml:"name"`
							Value string `yaml:"value"`
						} `yaml:"headers"`
						Preprocessing []struct {
							Type       string    `yaml:"type"`
							Parameters yamlValue `yaml:"parameters"`
						} `yaml:"preprocessing"`
						MasterItem struct {
							Key string `yaml:"key"`
						} `yaml:"master_item"`
					} `yaml:"item_prototypes"`
					Triggers []struct {
						Name       string `yaml:"name"`
						Expression string `yaml:"expression"`
					} `yaml:"trigger_prototypes"`
					MasterItem struct {
						Key string `yaml:"key"`
					} `yaml:"master_item"`
					LLDMacroPaths []struct {
						LLDMacro string `yaml:"lld_macro"`
						Path     string `yaml:"path"`
					} `yaml:"lld_macro_paths"`
				} `yaml:"discovery_rules"`
				Macros []struct {
					Macro       string `yaml:"macro"`
					Type        string `yaml:"type"`
					Value       string `yaml:"value"`
					Description string `yaml:"description"`
				} `yaml:"macros"`
			} `yaml:"templates"`
		} `yaml:"zabbix_export"`
	}
	if err := yaml.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("zabbixtemplate: parsing the export: %w", err)
	}
	export := doc.Export
	if len(export.Templates) == 0 {
		return nil, fmt.Errorf("zabbixtemplate: the export contains no templates")
	}
	tmpl := export.Templates[0]
	out := &Template{Version: export.Version}
	for _, g := range export.Groups {
		out.Groups = append(out.Groups, g.Name)
	}
	for _, it := range tmpl.Items {
		out.Items = append(out.Items, convertItem(it.Name, it.Key, it.Type, it.URL, it.Units, it.ValueType, it.Delay, it.VerifyPeer, it.VerifyHost, it.Headers, it.Preprocessing, it.MasterItem.Key))
	}
	for _, rule := range tmpl.DiscoveryRules {
		converted := DiscoveryRule{
			Name:      rule.Name,
			Key:       rule.Key,
			Type:      rule.Type,
			MasterKey: rule.MasterItem.Key,
		}
		for _, mp := range rule.LLDMacroPaths {
			converted.LLDMacroPaths = append(converted.LLDMacroPaths, LLDMacroPath{Macro: mp.LLDMacro, Path: mp.Path})
		}
		for _, it := range rule.Items {
			converted.Items = append(converted.Items, convertItem(it.Name, it.Key, it.Type, "", it.Units, it.ValueType, it.Delay, it.VerifyPeer, it.VerifyHost, it.Headers, it.Preprocessing, it.MasterItem.Key))
		}
		for _, tr := range rule.Triggers {
			converted.Triggers = append(converted.Triggers, Trigger{Name: tr.Name, Expression: tr.Expression})
		}
		out.Rules = append(out.Rules, converted)
	}
	for _, m := range tmpl.Macros {
		out.Macros = append(out.Macros, Macro{Name: m.Macro, Type: m.Type, Value: m.Value, Description: m.Description})
	}
	return out, nil
}

// convertItem turns a parsed item into the typed form.
func convertItem(name, key, itemType, url, units, valueType, delay, verifyPeer, verifyHost string, headers []struct {
	Name  string `yaml:"name"`
	Value string `yaml:"value"`
}, preprocessing []struct {
	Type       string    `yaml:"type"`
	Parameters yamlValue `yaml:"parameters"`
}, masterKey string) Item {
	item := Item{
		Name: name, Key: key, Type: itemType, URL: url, Units: units,
		ValueType: valueType, Delay: delay, VerifyPeer: verifyPeer,
		VerifyHost: verifyHost, MasterKey: masterKey,
	}
	for _, h := range headers {
		item.Headers = append(item.Headers, Header{Name: h.Name, Value: h.Value})
	}
	for _, p := range preprocessing {
		item.Preprocessing = append(item.Preprocessing, Preprocessing{Type: p.Type, Parameters: p.Parameters.list()})
	}
	return item
}

// ItemKeys returns every item key in the template, sorted.
//
// Both the template's own items and the item prototypes count: the
// prototypes are the per-node keys, and they are the ones an operator
// builds dashboards from.
func (t *Template) ItemKeys() []string {
	var keys []string
	for _, it := range t.Items {
		keys = append(keys, it.Key)
	}
	for _, rule := range t.Rules {
		for _, it := range rule.Items {
			keys = append(keys, it.Key)
		}
	}
	sort.Strings(keys)
	return keys
}

// MacroByName returns the macro with the given name.
func (t *Template) MacroByName(name string) (Macro, bool) {
	for _, m := range t.Macros {
		if m.Name == name {
			return m, true
		}
	}
	return Macro{}, false
}

// AllItems returns every item and item prototype.
func (t *Template) AllItems() []Item {
	out := append([]Item(nil), t.Items...)
	for _, rule := range t.Rules {
		out = append(out, rule.Items...)
	}
	return out
}

// URLs returns every URL the template reads from.
func (t *Template) URLs() []string {
	var urls []string
	for _, it := range t.AllItems() {
		if it.URL != "" {
			urls = append(urls, it.URL)
		}
	}
	return urls
}

// String renders the template's item keys for a failure message.
func (t *Template) String() string {
	return strings.Join(t.ItemKeys(), ", ")
}

// yamlValue accepts either shape Zabbix uses for a preprocessing
// step's parameters.
//
// Every step but the JavaScript one exports a LIST of parameters. The
// JavaScript step exports a single string, because its body is a
// script rather than a list of arguments - Zabbix's own export
// builder keeps that one parameter unsplit
// (CConfigurationExportBuilder::formatPreprocessingSteps).
//
// Both shapes are accepted here so that the parser does not have to
// be amended every time a step type is added.
type yamlValue struct {
	List []string
	Text string
}

// UnmarshalYAML implements yaml.Unmarshaler.
func (v *yamlValue) UnmarshalYAML(node *yaml.Node) error {
	switch node.Kind {
	case yaml.SequenceNode:
		return node.Decode(&v.List)
	case yaml.ScalarNode:
		var s string
		if err := node.Decode(&s); err != nil {
			return err
		}
		v.Text = s
		return nil
	default:
		return fmt.Errorf("zabbixtemplate: preprocessing parameters must be a string or a list, got a %s", nodeKindName(node.Kind))
	}
}

// list renders the value as a list of parameters, whatever shape it
// arrived in.
func (v yamlValue) list() []string {
	if v.Text != "" {
		return []string{v.Text}
	}
	return v.List
}

// nodeKindName names a YAML node kind for a refusal message.
func nodeKindName(kind yaml.Kind) string {
	switch kind {
	case yaml.DocumentNode:
		return "document"
	case yaml.SequenceNode:
		return "sequence"
	case yaml.MappingNode:
		return "mapping"
	case yaml.ScalarNode:
		return "scalar"
	case yaml.AliasNode:
		return "alias"
	default:
		return "unknown"
	}
}
