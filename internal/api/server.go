package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"recycle/internal/auth"
	"recycle/internal/service"
)

type Server struct {
	svc    *service.Service
	secret string
	mux    *http.ServeMux
}

func New(svc *service.Service, secret string) *Server {
	s := &Server{svc: svc, secret: secret, mux: http.NewServeMux()}
	s.routes()
	return s
}

func (s *Server) Handler() http.Handler {
	return s.mux
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, err error) {
	var be *service.BizError
	if errors.As(err, &be) {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": be.Msg, "code": be.Code})
		return
	}
	switch {
	case errors.Is(err, service.ErrNotFound):
		writeJSON(w, http.StatusNotFound, map[string]string{"error": err.Error()})
	case errors.Is(err, service.ErrForbidden):
		writeJSON(w, http.StatusForbidden, map[string]string{"error": err.Error()})
	case errors.Is(err, service.ErrConflict):
		writeJSON(w, http.StatusConflict, map[string]string{"error": err.Error()})
	case errors.Is(err, service.ErrInvalidCredentials):
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": err.Error()})
	default:
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": err.Error()})
	}
}

func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		return true
	}
	dec := json.NewDecoder(r.Body)
	if err := dec.Decode(v); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "请求体 JSON 解析失败: " + err.Error()})
		return false
	}
	return true
}

func claims(r *http.Request) auth.Claims {
	c, _ := auth.FromContext(r.Context())
	return c
}

// apptIDFromPath 从 /api/.../<id>/<action> 中解析 id 与剩余动作。
func apptIDFromPath(prefix, path string) (int64, string, error) {
	rest := strings.TrimPrefix(path, prefix)
	parts := strings.SplitN(rest, "/", 2)
	id, err := strconv.ParseInt(parts[0], 10, 64)
	if err != nil {
		return 0, "", err
	}
	action := ""
	if len(parts) == 2 {
		action = parts[1]
	}
	return id, action, nil
}
