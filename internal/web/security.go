package web

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"fmt"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

// TokenHeader carries the per-process token on mutating requests.
const TokenHeader = "X-Skald-Token"

// CheckListenAddr refuses addresses that are not loopback unless insecure is
// set. skald has no login, so a reachable port means anyone who can connect
// can change tasks.
func CheckListenAddr(addr string, insecure bool) error {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return fmt.Errorf("invalid address %q (%v). Use host:port, for example 127.0.0.1:7331", addr, err)
	}
	if n, err := strconv.ParseUint(port, 10, 16); err != nil {
		return fmt.Errorf("invalid port %q in address %q. Use a number from 0 to 65535", port, addr)
	} else {
		_ = n
	}
	if isLoopbackHost(host) || insecure {
		return nil
	}
	return fmt.Errorf("refusing to listen on %q because it is not a loopback address. skald has no login, so anyone who can reach the port could change your tasks. Use 127.0.0.1, or pass --insecure-listen if you accept that risk", addr)
}

// isLoopbackHost reports whether host is localhost or a loopback IP. An empty
// host means all interfaces and is not loopback.
func isLoopbackHost(host string) bool {
	host = strings.TrimSuffix(strings.TrimPrefix(host, "["), "]")
	if host == "" {
		return false
	}
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// hostAllowed is the DNS-rebinding defence: a request is served only when its
// Host header names a loopback host (and the pinned port, once known). A
// rebinding page has to use its own domain name, which fails this check.
func (s *Server) hostAllowed(hostport string) bool {
	if s.cfg.Insecure {
		return true
	}
	host, port, err := net.SplitHostPort(hostport)
	if err != nil {
		host, port = hostport, "" // no port, possibly a bare [::1]
	}
	if !isLoopbackHost(host) {
		return false
	}
	if p := s.port(); p != 0 && port != strconv.Itoa(p) {
		return false
	}
	return true
}

// originOK requires a same-origin Origin header: its host must equal the
// request's Host, which must itself be allowed. A missing or "null" Origin is
// refused. A browser also sends Sec-Fetch-Site; anything other than
// same-origin is refused.
func (s *Server) originOK(r *http.Request) bool {
	if v := r.Header.Get("Sec-Fetch-Site"); v != "" && v != "same-origin" {
		return false
	}
	o := r.Header.Get("Origin")
	if o == "" || o == "null" {
		return false
	}
	u, err := url.Parse(o)
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host) && s.hostAllowed(r.Host)
}

func (s *Server) tokenOK(r *http.Request) bool {
	got := r.Header.Get(TokenHeader)
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(s.token)) == 1
}

func newToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return hex.EncodeToString(b), nil
}

// securityHeaders sets headers that apply to every response.
func securityHeaders(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hd := w.Header()
		hd.Set("Content-Security-Policy", "default-src 'self'; script-src 'self'; style-src 'self'; img-src 'self' data:; connect-src 'self'; font-src 'self'; object-src 'none'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
		hd.Set("X-Content-Type-Options", "nosniff")
		hd.Set("X-Frame-Options", "DENY")
		hd.Set("Referrer-Policy", "no-referrer")
		hd.Set("Cross-Origin-Resource-Policy", "same-origin")
		hd.Set("Cross-Origin-Opener-Policy", "same-origin")
		h.ServeHTTP(w, r)
	})
}

// guard rejects requests with a disallowed Host before any routing.
func (s *Server) guard(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.hostAllowed(r.Host) {
			writeAPIError(w, http.StatusForbidden, "forbidden_host",
				"This request used a Host header that is not a loopback address, so it was refused. Open the dashboard at http://127.0.0.1:<port> or http://localhost:<port>.")
			return
		}
		h.ServeHTTP(w, r)
	})
}

// mutating wraps a handler that changes data: it needs a same-origin Origin,
// the per-process token and a JSON content type.
func (s *Server) mutating(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !s.originOK(r) {
			writeAPIError(w, http.StatusForbidden, "forbidden_origin",
				"This request did not come from the dashboard page (missing or foreign Origin), so it was refused. Make the change from the dashboard or with the skald CLI.")
			return
		}
		if !s.tokenOK(r) {
			writeAPIError(w, http.StatusForbidden, "forbidden_token",
				"The request token is missing or wrong. Reload the dashboard page to get a fresh one.")
			return
		}
		if ct := r.Header.Get("Content-Type"); r.ContentLength != 0 && !strings.HasPrefix(ct, "application/json") {
			writeAPIError(w, http.StatusUnsupportedMediaType, "unsupported_media_type", "Send the body as application/json.")
			return
		}
		h(w, r)
	}
}
