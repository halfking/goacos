package config

import "testing"

func TestCasdoorConfig(t *testing.T) {
	c := &Config{AuthEnabled: true, AuthProvider: "casdoor", Casdoor: CasdoorConfig{Endpoint: "https://identity.example", Issuer: "https://identity.example", ClientID: "registry", ClientSecret: "secret", Organization: "org", Permission: "org/registry"}}
	if err := c.ValidateAuth(); err != nil {
		t.Fatal(err)
	}
	c.AuthEnabled = false
	if c.ValidateAuth() == nil {
		t.Fatal("casdoor accepted auth disabled")
	}
	c.AuthProvider = "invalid"
	if c.ValidateAuth() == nil {
		t.Fatal("unknown provider accepted")
	}
}
func TestCasdoorApplicationsEnv(t *testing.T) {
	t.Setenv("GOACOS_AUTH_PROVIDER", "casdoor")
	t.Setenv("GOACOS_AUTH_ENABLED", "true")
	t.Setenv("GOACOS_CASDOOR_APPLICATIONS", `{"worker": "client"}`)
	if FromEnv().Casdoor.Applications["worker"] != "client" {
		t.Fatal("missing applications")
	}
	t.Setenv("GOACOS_CASDOOR_APPLICATIONS", `{"worker": 7}`)
	if FromEnv().ValidateAuth() == nil {
		t.Fatal("invalid JSON accepted")
	}
}

func TestCasdoorApplicationsRejectAmbiguousJSON(t *testing.T) {
	for _, raw := range []string{`null`, `[]`, `{"a":"one","a":"two"}`, `{"a":null}`, `{"a":"one"} {}`, `{"a":3}`} {
		t.Setenv("GOACOS_CASDOOR_APPLICATIONS", raw)
		if !FromEnv().Casdoor.applicationsError {
			t.Fatalf("accepted malformed declaration %s", raw)
		}
	}
}

func TestCasdoorPolicySelector(t *testing.T) {
	for _, tc := range []struct {
		name, permission, model string
		valid                   bool
	}{
		{"permission", "org/registry", "", true},
		{"model", "", "org/registry", true},
		{"neither", "", "", false},
		{"both", "org/p", "org/m", false},
		{"foreign permission", "other/p", "", false},
		{"foreign model", "", "other/m", false},
		{"empty model name", "", "org/", false},
		{"nested model name", "", "org/m/n", false},
		{"model whitespace", "", "org/m ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := CasdoorConfig{Endpoint: "https://identity.example", Issuer: "issuer", ClientID: "registry", ClientSecret: "secret", Organization: "org", Permission: tc.permission, Model: tc.model}
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
		})
	}
	t.Setenv("GOACOS_CASDOOR_MODEL", "org/model")
	if FromEnv().Casdoor.Model != "org/model" {
		t.Fatal("model env not loaded")
	}
}

func TestCasdoorModelResponseGroup(t *testing.T) {
	for _, tc := range []struct {
		name, model, permission, group string
		valid                          bool
	}{
		{"legacy default", "org/model", "", "", true},
		{"explicit legacy", "org/model", "", "model/", true},
		{"native qualified", "org/model", "", "org/model/", true},
		{"permission default", "", "org/permission", "", true},
		{"group without model", "", "org/permission", "model/", false},
		{"foreign organization", "org/model", "", "other/model/", false},
		{"wrong model", "org/model", "", "org/other/", false},
		{"native adapter", "org/model", "", "org/model/external", false},
		{"legacy adapter", "org/model", "", "model/external", false},
		{"missing empty adapter", "org/model", "", "org/model", false},
		{"whitespace", "org/model", "", "org/model/ ", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := CasdoorConfig{Endpoint: "https://identity.example", Issuer: "issuer", ClientID: "registry", ClientSecret: "secret", Organization: "org", Permission: tc.permission, Model: tc.model, ModelResponseGroup: tc.group}
			if err := c.Validate(); (err == nil) != tc.valid {
				t.Fatalf("valid=%v, err=%v", tc.valid, err)
			}
		})
	}
	t.Setenv("GOACOS_CASDOOR_MODEL_RESPONSE_GROUP", "org/model/")
	if FromEnv().Casdoor.ModelResponseGroup != "org/model/" {
		t.Fatal("model response group env not loaded")
	}
}
