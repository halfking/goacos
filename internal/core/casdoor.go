package core

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/halfking/goacos/internal/config"
)

var (
	ErrCasdoorUnavailable = errors.New("identity service unavailable")
	ErrCasdoorDenied      = errors.New("identity authorization denied")
)

// CasdoorAuthorizer rechecks token, current principal and permission at every admission.
// It has no positive cache; an admitted operation is not serialized with revocation.
type CasdoorAuthorizer struct {
	cfg    config.CasdoorConfig
	client *http.Client
}

func NewCasdoorAuthorizer(c config.CasdoorConfig) (*CasdoorAuthorizer, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	c.Endpoint = strings.TrimRight(c.Endpoint, "/")
	apps := make(map[string]string, len(c.Applications))
	for k, v := range c.Applications {
		apps[k] = v
	}
	c.Applications = apps
	return &CasdoorAuthorizer{cfg: c, client: &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}
func (a *CasdoorAuthorizer) call(ctx context.Context, path, contentType string, body []byte, out any) error {
	method := http.MethodPost
	if body == nil {
		method = http.MethodGet
	}
	req, err := http.NewRequestWithContext(ctx, method, a.cfg.Endpoint+path, bytes.NewReader(body))
	if err != nil {
		return ErrCasdoorUnavailable
	}
	req.Header.Set("Content-Type", contentType)
	req.SetBasicAuth(a.cfg.ClientID, a.cfg.ClientSecret)
	resp, err := a.client.Do(req)
	if err != nil {
		return ErrCasdoorUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ErrCasdoorUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 65537))
	if err != nil || len(raw) > 65536 || json.Unmarshal(raw, out) != nil {
		return ErrCasdoorUnavailable
	}
	return nil
}
func audienceContains(raw json.RawMessage, want string) bool {
	var list []string
	if json.Unmarshal(raw, &list) != nil {
		var one string
		if json.Unmarshal(raw, &one) != nil {
			return false
		}
		list = []string{one}
	}
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// Authorize trusts payload identity only after remote introspection validates the exact token.
func (a *CasdoorAuthorizer) Authorize(ctx context.Context, token, resource, action string) (*Claims, error) {
	if len(token) == 0 || len(token) > 16384 || resource == "" || (action != "read" && action != "write") {
		return nil, ErrCasdoorDenied
	}
	parts := strings.Split(token, ".")
	if len(parts) != 3 || parts[2] == "" {
		return nil, ErrCasdoorDenied
	}
	var header struct {
		Alg string `json:"alg"`
	}
	raw, err := base64.RawURLEncoding.DecodeString(parts[0])
	if err != nil || json.Unmarshal(raw, &header) != nil || header.Alg != "RS256" {
		return nil, ErrCasdoorDenied
	}
	var identity struct {
		Active   bool            `json:"active"`
		Issuer   string          `json:"iss"`
		Audience json.RawMessage `json:"aud"`
		Subject  string          `json:"sub"`
		Username string          `json:"username"`
		Type     string          `json:"token_type"`
		Exp      int64           `json:"exp"`
		Nbf      int64           `json:"nbf"`
		Iat      int64           `json:"iat"`
	}
	form := url.Values{"token": {token}, "token_type_hint": {"access_token"}}
	if err = a.call(ctx, "/api/login/oauth/introspect", "application/x-www-form-urlencoded", []byte(form.Encode()), &identity); err != nil {
		return nil, err
	}
	now := time.Now().Unix()
	if !identity.Active || identity.Issuer != a.cfg.Issuer || identity.Exp <= now || identity.Nbf > now || identity.Iat > now+30 || identity.Subject == "" || identity.Username == "" || (identity.Type != "access-token" && identity.Type != "Bearer") {
		return nil, ErrCasdoorDenied
	}
	var claims struct {
		Owner   string `json:"owner"`
		Name    string `json:"name"`
		Subject string `json:"sub"`
		Type    string `json:"type"`
	}
	raw, err = base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil || json.Unmarshal(raw, &claims) != nil || claims.Subject != identity.Subject || claims.Name != identity.Username || strings.TrimSpace(claims.Name) != claims.Name || strings.Contains(claims.Name, "/") {
		return nil, ErrCasdoorDenied
	}
	if claims.Type == "application" {
		clientID, ok := a.cfg.Applications[claims.Name]
		if !ok || claims.Owner != "admin" || claims.Subject != "admin/"+claims.Name || !audienceContains(identity.Audience, clientID) {
			return nil, ErrCasdoorDenied
		}
		var app struct {
			Status string `json:"status"`
			Data   *struct {
				Owner        string `json:"owner"`
				Name         string `json:"name"`
				Organization string `json:"organization"`
				ClientID     string `json:"clientId"`
			} `json:"data"`
		}
		if err = a.call(ctx, "/api/get-application?"+url.Values{"id": {"admin/" + claims.Name}}.Encode(), "", nil, &app); err != nil {
			return nil, err
		}
		if app.Status != "ok" {
			return nil, ErrCasdoorUnavailable
		}
		if app.Data == nil || app.Data.Owner != "admin" || app.Data.Name != claims.Name || app.Data.Organization != a.cfg.Organization || app.Data.ClientID != clientID {
			return nil, ErrCasdoorDenied
		}
	} else {
		if claims.Owner != a.cfg.Organization || !audienceContains(identity.Audience, a.cfg.ClientID) {
			return nil, ErrCasdoorDenied
		}
		var user struct {
			Status string `json:"status"`
			Data   []struct {
				ID        string `json:"id"`
				Owner     string `json:"owner"`
				Name      string `json:"name"`
				Forbidden *bool  `json:"isForbidden"`
				Deleted   *bool  `json:"isDeleted"`
			} `json:"data"`
		}
		// The list API preserves account flags; Casdoor get-user may mask them.
		query := url.Values{"owner": {a.cfg.Organization}, "field": {"id"}, "value": {identity.Subject}, "pageSize": {"2"}, "p": {"1"}}
		if err = a.call(ctx, "/api/get-users?"+query.Encode(), "", nil, &user); err != nil {
			return nil, err
		}
		if user.Status != "ok" {
			return nil, ErrCasdoorUnavailable
		}
		if len(user.Data) != 1 {
			return nil, ErrCasdoorDenied
		}
		current := user.Data[0]
		// Casdoor filters by LIKE, so bind every field to the introspected identity.
		if current.ID != identity.Subject || current.Owner != claims.Owner || current.Name != claims.Name || current.Forbidden == nil || current.Deleted == nil || *current.Forbidden || *current.Deleted {
			return nil, ErrCasdoorDenied
		}
	}
	body, _ := json.Marshal([]string{claims.Owner + "/" + claims.Name, resource, action})
	var decision struct {
		Status string          `json:"status"`
		Data   []bool          `json:"data"`
		Groups json.RawMessage `json:"data2"`
	}
	selector := url.Values{"permissionId": {a.cfg.Permission}}
	if a.cfg.Model != "" {
		selector = url.Values{"modelId": {a.cfg.Model}}
	}
	if err = a.call(ctx, "/api/enforce?"+selector.Encode(), "application/json", body, &decision); err != nil {
		return nil, err
	}
	if decision.Status != "ok" || len(decision.Data) != 1 {
		return nil, ErrCasdoorUnavailable
	}
	// Model mode initially supports only the built-in (empty) adapter. Casdoor
	// returns one decision per model/adapter group: bind that decision to ours.
	if a.cfg.Model != "" {
		var groups []string
		expected := a.cfg.ModelResponseGroup
		if expected == "" {
			expected = strings.TrimPrefix(a.cfg.Model, a.cfg.Organization+"/") + "/"
		}
		if json.Unmarshal(decision.Groups, &groups) != nil || len(groups) != 1 || groups[0] != expected {
			return nil, ErrCasdoorUnavailable
		}
	}
	if !decision.Data[0] {
		return nil, ErrCasdoorDenied
	}
	return &Claims{Username: claims.Owner + "/" + claims.Name}, nil
}
