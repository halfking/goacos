package config

import (
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"strings"
)

// CasdoorConfig selects the identity authority and explicitly trusted machine applications.
type CasdoorConfig struct {
	Endpoint, Issuer, ClientID, ClientSecret, Organization, Permission, Model string
	ModelResponseGroup                                                        string
	AllowHTTP                                                                 bool
	Applications                                                              map[string]string
	applicationsError                                                         bool
}

func casdoorFromEnv() CasdoorConfig {
	c := CasdoorConfig{Endpoint: getenv("CASDOOR_ENDPOINT", ""), Issuer: getenv("CASDOOR_ISSUER", ""), ClientID: getenv("CASDOOR_CLIENT_ID", ""), ClientSecret: getenv("CASDOOR_CLIENT_SECRET", ""), Organization: getenv("CASDOOR_ORGANIZATION", ""), Permission: getenv("CASDOOR_PERMISSION", ""), Model: getenv("CASDOOR_MODEL", ""), AllowHTTP: getenvBool("CASDOOR_ALLOW_HTTP", false)}
	c.ModelResponseGroup = getenv("CASDOOR_MODEL_RESPONSE_GROUP", "")
	if raw := getenv("CASDOOR_APPLICATIONS", ""); raw != "" {
		c.Applications, c.applicationsError = parseApplications(raw)
	}
	return c
}

// Validate rejects incomplete configuration without exposing credential values.
func (c CasdoorConfig) Validate() error {
	invalid := errors.New("auth configuration: invalid Casdoor configuration")
	u, err := url.Parse(c.Endpoint)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") || (u.Scheme != "https" && !(c.AllowHTTP && u.Scheme == "http")) {
		return invalid
	}
	for _, v := range []string{c.Issuer, c.ClientID, c.ClientSecret, c.Organization} {
		if v == "" || strings.TrimSpace(v) != v {
			return invalid
		}
	}
	// A model combines separate scoped permissions; a permission retains the
	// original single-policy mode. Never send ambiguous selectors to Casdoor.
	if (c.Permission == "") == (c.Model == "") {
		return invalid
	}
	selector := c.Permission
	if c.Model != "" {
		selector = c.Model
	}
	if strings.TrimSpace(selector) != selector || strings.Contains(c.Organization, "/") || !strings.HasPrefix(selector, c.Organization+"/") || strings.Count(selector, "/") != 1 || strings.TrimPrefix(selector, c.Organization+"/") == "" || c.applicationsError {
		return invalid
	}
	// Different Casdoor builds return either a short or qualified model name.
	// Bind one explicit representation, never accept a foreign model or adapter.
	if c.ModelResponseGroup != "" && (c.Model == "" ||
		(c.ModelResponseGroup != c.Model+"/" && c.ModelResponseGroup != strings.TrimPrefix(c.Model, c.Organization+"/")+"/")) {
		return invalid
	}
	for name, id := range c.Applications {
		if name == "" || id == "" || strings.TrimSpace(name) != name || strings.TrimSpace(id) != id || strings.Contains(name, "/") {
			return invalid
		}
	}
	return nil
}

// Reject duplicate names as well as non-object/trailing JSON rather than silently
// letting a later mapping replace an operator's trust declaration.
func parseApplications(raw string) (map[string]string, bool) {
	d := json.NewDecoder(strings.NewReader(raw))
	start, err := d.Token()
	if err != nil || start != json.Delim('{') {
		return nil, true
	}
	result := map[string]string{}
	for d.More() {
		key, err := d.Token()
		if err != nil {
			return nil, true
		}
		name, ok := key.(string)
		if !ok {
			return nil, true
		}
		if _, exists := result[name]; exists {
			return nil, true
		}
		var id string
		if d.Decode(&id) != nil || id == "" {
			return nil, true
		}
		result[name] = id
	}
	end, err := d.Token()
	if err != nil || end != json.Delim('}') {
		return nil, true
	}
	if _, err = d.Token(); err != io.EOF {
		return nil, true
	}
	return result, false
}
