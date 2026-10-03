package governor

import (
	"bufio"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

func (g *Gate) Start(addr string) (net.Listener, error) {
	cert, err := tls.LoadX509KeyPair(filepath.Join(g.Dir, "ca.pem"), filepath.Join(g.Dir, "ca-key.pem"))
	if err != nil {
		return nil, err
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	serve := func(w http.ResponseWriter, r *http.Request) {
		if r.Method == "GET" && r.URL.Path == "/health" {
			g.mu.Lock()
			defer g.mu.Unlock()
			json.NewEncoder(w).Encode(map[string]any{"counts": g.State.Counts, "box_hourly": g.Policy.BoxHourly, "hour_calls": len(g.keep("box", g.Now().Unix()-3600))})
			return
		}
		if r.Method != "CONNECT" {
			http.Error(w, "ghgate: CONNECT only", 405)
			return
		}
		var caller Caller
		raw := strings.TrimPrefix(r.Header.Get("Proxy-Authorization"), "Basic ")
		b, _ := base64.StdEncoding.DecodeString(raw)
		user, proof, _ := strings.Cut(string(b), ":")
		if subtle.ConstantTimeCompare([]byte(proof), []byte(proxyProof(g.Dir))) != 1 {
			http.Error(w, "ghgate: local proxy credential required", 403)
			return
		}
		meta, _ := base64.RawURLEncoding.DecodeString(user)
		if json.Unmarshal(meta, &caller) != nil {
			http.Error(w, "ghgate: missing caller identity", 403)
			return
		}
		// Only GitHub API TLS is terminated. Other HTTPS hosts retain opaque native TLS.
		var remote net.Conn
		if r.Host != "api.github.com:443" {
			var err error
			remote, err = net.DialTimeout("tcp", r.Host, 5*time.Second)
			if err != nil {
				http.Error(w, "ghgate: upstream unavailable", 503)
				return
			}
			defer remote.Close()
		}
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		rw.WriteString("HTTP/1.1 200 Connection Established\r\n\r\n")
		rw.Flush()
		if remote != nil {
			go io.Copy(remote, conn)
			io.Copy(conn, remote)
			return
		}
		secure := tls.Server(conn, &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12})
		secure.SetDeadline(time.Now().Add(30 * time.Second))
		if secure.Handshake() != nil {
			return
		}
		secure.SetDeadline(time.Time{})
		reader := bufio.NewReader(secure)
		for {
			req, err := http.ReadRequest(reader)
			if err != nil {
				return
			}
			req.URL.Scheme = "https"
			req.URL.Host = "api.github.com"
			resp := g.RoundTrip(req, caller)
			resp.Request = req
			resp.Proto = "HTTP/1.1"
			resp.ProtoMajor = 1
			resp.ProtoMinor = 1
			if resp.ContentLength < 0 && len(resp.TransferEncoding) == 0 {
				resp.TransferEncoding = []string{"chunked"}
			}
			if err = resp.Write(secure); err != nil {
				resp.Body.Close()
				return
			}
			resp.Body.Close()
			if req.Close {
				return
			}
		}
	}
	go (&http.Server{Handler: http.HandlerFunc(serve), ReadHeaderTimeout: 10 * time.Second, MaxHeaderBytes: 16384}).Serve(ln)
	return ln, nil
}
func ProxyURL(addr string, c Caller, dir string) string {
	b, _ := json.Marshal(c)
	return fmt.Sprintf("http://%s:%s@%s", base64.RawURLEncoding.EncodeToString(b), proxyProof(dir), addr)
}

func proxyProof(dir string) string {
	if b, err := os.ReadFile(filepath.Join(dir, "proxy-proof")); err == nil {
		return strings.TrimSpace(string(b))
	}
	b, _ := os.ReadFile(filepath.Join(dir, "ca-key.pem"))
	return fmt.Sprintf("%x", sha256.Sum256(b))
}
