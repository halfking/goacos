package api

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"strings"
)

var errConfigHandler = errors.New("config handler rejected operation")

type configResponse struct {
	headers  http.Header
	body     bytes.Buffer
	code     int
	overflow bool
}

func (b *configResponse) Header() http.Header { return b.headers }
func (b *configResponse) WriteHeader(n int) {
	if b.code == 0 {
		b.code = n
	}
}
func (b *configResponse) Write(p []byte) (int, error) {
	if b.code == 0 {
		b.code = 200
	}
	if b.body.Len()+len(p) > 16*1024*1024 {
		b.overflow = true
		return 0, errConfigHandler
	}
	return b.body.Write(p)
}
func (s *Server) serveConfig(w http.ResponseWriter, r *http.Request, next http.Handler) {
	if s.ST == nil || s.Hub == nil {
		writeText(w, 503, "configuration instance unavailable")
		return
	}
	if strings.HasSuffix(r.URL.Path, "/listener") {
		// The listener releases ownership between fenced checks; buffering its
		// result then a final owned check prevents an old hub returning cached data.
		b := &configResponse{headers: make(http.Header)}
		next.ServeHTTP(b, r)
		e := s.ST.WithConfigTransaction(r.Context(), func(context.Context) error {
			if b.overflow {
				return errConfigHandler
			}
			if b.code >= 400 {
				return errConfigHandler
			}
			return nil
		})
		if e != nil {
			writeText(w, 503, "configuration instance unavailable")
			return
		}
		b.flush(w)
		return
	}
	b := &configResponse{headers: make(http.Header)}
	e := s.ST.WithConfigTransaction(r.Context(), func(ctx context.Context) error {
		next.ServeHTTP(b, r.WithContext(ctx))
		if b.overflow || b.code >= 400 {
			return errConfigHandler
		}
		return nil
	})
	if e != nil {
		if errors.Is(e, errConfigHandler) && !b.overflow {
			b.flush(w)
		} else {
			writeText(w, 503, "configuration instance unavailable")
		}
		return
	}
	b.flush(w)
}
func (b *configResponse) flush(w http.ResponseWriter) {
	for k, v := range b.headers {
		w.Header()[k] = append([]string(nil), v...)
	}
	code := b.code
	if code == 0 {
		code = 200
	}
	w.WriteHeader(code)
	_, _ = w.Write(b.body.Bytes())
}
