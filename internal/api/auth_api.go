package api

import (
	"net/http"
	"strconv"

	"github.com/halfking/goacos/internal/core"
	"golang.org/x/crypto/bcrypt"
)

// ---------- auth endpoints (Nacos-compatible) ----------

// authLogin handles POST /nacos/v1/auth/users/login.
func (s *Server) authLogin(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	username := firstNonEmpty(r.Form.Get("username"), r.Form.Get("user"))
	password := firstNonEmpty(r.Form.Get("password"), r.Form.Get("pass"))
	if username == "" || password == "" {
		writeJSON(w, http.StatusBadRequest, map[string]any{"code": 400, "message": "username/password required"})
		return
	}
	token, ttl, admin, err := s.Auth.Login(r.Context(), username, password)
	if err != nil {
		writeJSON(w, http.StatusUnauthorized, map[string]any{"code": 401, "message": "unknown user"})
		return
	}
	// flat fields for v1 clients + envelope for v2-style consumers
	writeJSON(w, http.StatusOK, map[string]any{
		"accessToken": token,
		"tokenTtl":    ttl,
		"globalAdmin": admin,
		"username":    username,
		"code":        200,
		"message":     "success",
		"data": map[string]any{
			"accessToken": token,
			"tokenTtl":    ttl,
			"globalAdmin": admin,
			"username":    username,
		},
	})
}

// authLoginV2 wraps login in the v2 envelope.
func (s *Server) authLoginV2(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	token, ttl, admin, err := s.Auth.Login(r.Context(), r.Form.Get("username"), r.Form.Get("password"))
	if err != nil {
		v2Fail(w, http.StatusUnauthorized, 401, "unknown user")
		return
	}
	v2OK(w, map[string]any{"accessToken": token, "tokenTtl": ttl, "globalAdmin": admin})
}

// authListUsers handles GET /nacos/v1/auth/users.
func (s *Server) authListUsers(w http.ResponseWriter, r *http.Request) {
	pageNo, _ := strconv.Atoi(q(r, "pageNo"))
	pageSize, _ := strconv.Atoi(q(r, "pageSize"))
	if pageNo < 1 {
		pageNo = 1
	}
	if pageSize < 1 {
		pageSize = 25
	}
	users, roles, total, err := s.ST.ListUsers(r.Context(), pageNo, pageSize)
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	type userInfo struct {
		Username string   `json:"username"`
		Roles    []string `json:"roles"`
		Enabled  bool     `json:"enabled"`
	}
	items := make([]userInfo, 0, len(users))
	for _, u := range users {
		rlist := roles[u.Username]
		if rlist == nil {
			rlist = []string{}
		}
		items = append(items, userInfo{Username: u.Username, Roles: rlist, Enabled: u.Enabled})
	}
	writeJSON(w, http.StatusOK, newPage(total, pageNo, pageSize, items))
}

// authCreateUser handles POST /nacos/v1/auth/users.
func (s *Server) authCreateUser(w http.ResponseWriter, r *http.Request) {
	p := formOrJSON(r)
	username, password := p["username"], p["password"]
	if username == "" || password == "" {
		v1Fail(w, http.StatusBadRequest, 400, "username/password required")
		return
	}
	hash, err := core.HashPassword(password)
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if err := s.ST.CreateUser(r.Context(), username, hash); err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	_ = s.ST.AddRole(r.Context(), username, "ROLE_USER")
	v1OK(w, "ok")
}

// authUpdateUser handles PUT /nacos/v1/auth/users (password change).
func (s *Server) authUpdateUser(w http.ResponseWriter, r *http.Request) {
	p := formOrJSON(r)
	username := p["username"]
	oldPassword := firstNonEmpty(p["oldPassword"], p["oldPass"])
	newPassword := firstNonEmpty(p["newPassword"], p["newPass"])
	if username == "" || newPassword == "" {
		v1Fail(w, http.StatusBadRequest, 400, "username/newPassword required")
		return
	}
	// non-admin callers may only change their own password with the old one
	claims := ClaimsFrom(r.Context())
	if claims == nil || !claims.Admin || claims.Username != username || oldPassword != "" {
		u, err := s.ST.GetUser(r.Context(), username)
		if err != nil || u == nil {
			v1Fail(w, http.StatusNotFound, 404, "user not found")
			return
		}
		if oldPassword != "" && bcrypt.CompareHashAndPassword([]byte(u.Password), []byte(oldPassword)) != nil {
			v1Fail(w, http.StatusBadRequest, 400, "old password mismatch")
			return
		}
	}
	hash, err := core.HashPassword(newPassword)
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	if err := s.ST.UpdatePassword(r.Context(), username, hash); err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v1OK(w, "ok")
}

// authDeleteUser handles DELETE /nacos/v1/auth/users.
func (s *Server) authDeleteUser(w http.ResponseWriter, r *http.Request) {
	username := q(r, "username")
	if username == "" {
		v1Fail(w, http.StatusBadRequest, 400, "username required")
		return
	}
	if err := s.ST.DeleteUser(r.Context(), username); err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v1OK(w, "ok")
}

// authListRoles handles GET /nacos/v1/auth/roles.
func (s *Server) authListRoles(w http.ResponseWriter, r *http.Request) {
	if username := q(r, "username"); username != "" {
		roles, err := s.ST.ListRolesByUser(r.Context(), username)
		if err != nil {
			v1Fail(w, http.StatusInternalServerError, 500, err.Error())
			return
		}
		type roleInfo struct {
			Role     string `json:"role"`
			Username string `json:"username"`
		}
		items := make([]roleInfo, 0, len(roles))
		for _, ro := range roles {
			items = append(items, roleInfo{Role: ro, Username: username})
		}
		v1OK(w, map[string]any{"totalCount": len(items), "pageItems": items})
		return
	}
	users, roles, _, err := s.ST.ListUsers(r.Context(), 1, 500)
	if err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	type roleInfo struct {
		Role     string `json:"role"`
		Username string `json:"username"`
	}
	items := []roleInfo{}
	for _, u := range users {
		for _, ro := range roles[u.Username] {
			items = append(items, roleInfo{Role: ro, Username: u.Username})
		}
	}
	v1OK(w, map[string]any{"totalCount": len(items), "pageItems": items})
}

// authAddRole handles POST /nacos/v1/auth/roles.
func (s *Server) authAddRole(w http.ResponseWriter, r *http.Request) {
	p := formOrJSON(r)
	username, role := p["username"], p["role"]
	if username == "" || role == "" {
		v1Fail(w, http.StatusBadRequest, 400, "username/role required")
		return
	}
	if err := s.ST.AddRole(r.Context(), username, role); err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v1OK(w, "ok")
}

// authDeleteRole handles DELETE /nacos/v1/auth/roles.
func (s *Server) authDeleteRole(w http.ResponseWriter, r *http.Request) {
	username, role := q(r, "username"), q(r, "role")
	if username == "" || role == "" {
		v1Fail(w, http.StatusBadRequest, 400, "username/role required")
		return
	}
	if err := s.ST.DeleteRole(r.Context(), username, role); err != nil {
		v1Fail(w, http.StatusInternalServerError, 500, err.Error())
		return
	}
	v1OK(w, "ok")
}
