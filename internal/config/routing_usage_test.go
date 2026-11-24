package config

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestRoutingUsageRescueRoundTrip(t *testing.T) {
	for _, version := range []string{"", "config-version: 8\n"} {
		for _, headroom := range []string{"0.25", "0"} {
			t.Run(version+headroom, func(t *testing.T) {
				raw := []byte(version + "routing:\n  strategy: fill-most-used\n  usage-rescue-within: 4h\n  usage-rescue-headroom: " + headroom + "\n  session-affinity-ttl: 30m\n")
				cfg, errParse := ParseConfigBytes(raw)
				if errParse != nil {
					t.Fatal(errParse)
				}
				if cfg.Routing.UsageRescueWithin != "4h" || cfg.Routing.UsageRescueHeadroom == nil || cfg.Routing.SessionAffinityTTL != "30m" {
					t.Fatalf("routing fields = %+v", cfg.Routing)
				}
				path := filepath.Join(t.TempDir(), "config.yaml")
				if errWrite := os.WriteFile(path, raw, 0600); errWrite != nil {
					t.Fatal(errWrite)
				}
				if errSave := SaveConfigPreserveComments(path, cfg); errSave != nil {
					t.Fatal(errSave)
				}
				reloaded, errLoad := LoadConfig(path)
				if errLoad != nil {
					t.Fatal(errLoad)
				}
				if !reflect.DeepEqual(reloaded.Routing, cfg.Routing) {
					t.Fatalf("routing changed after save: %+v -> %+v", cfg.Routing, reloaded.Routing)
				}
				data, errRead := os.ReadFile(path)
				if errRead != nil {
					t.Fatal(errRead)
				}
				var document yaml.Node
				if errDecode := yaml.Unmarshal(data, &document); errDecode != nil {
					t.Fatal(errDecode)
				}
				root := document.Content[0]
				for _, key := range []string{"usage-rescue-within", "usage-rescue-headroom", "session-affinity-ttl"} {
					if yamlPath(root, "routing."+key) == nil {
						t.Fatalf("saved routing.%s is missing", key)
					}
				}
				if version != "" {
					if errValidate := ValidateV8Config(data); errValidate != nil {
						t.Fatal(errValidate)
					}
				}
				encoded, errEncode := json.Marshal(cfg.Routing)
				if errEncode != nil {
					t.Fatal(errEncode)
				}
				var decoded RoutingConfig
				if errDecode := json.Unmarshal(encoded, &decoded); errDecode != nil {
					t.Fatal(errDecode)
				}
				if !reflect.DeepEqual(decoded, cfg.Routing) {
					t.Fatalf("routing JSON round trip = %+v, want %+v", decoded, cfg.Routing)
				}
			})
		}
	}
}

func TestRoutingUsageRescueInvalidYAML(t *testing.T) {
	for _, headroom := range []string{"garbage", ".nan", ".inf", "[0.25]"} {
		t.Run(headroom, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte("config-version: 8\nrouting:\n  strategy: fill-most-used\n  usage-rescue-within: garbage\n  usage-rescue-headroom: " + headroom + "\n"))
			if errParse != nil {
				t.Fatal(errParse)
			}
			if cfg.Routing.UsageRescueHeadroom != nil || cfg.Routing.UsageRescueWithin != "garbage" {
				t.Fatalf("invalid rescue settings = %+v", cfg.Routing)
			}
		})
	}
}
