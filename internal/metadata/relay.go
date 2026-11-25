// Package metadata gives every machine an identity token at http://169.254.169.254, the address
// clouds' metadata services use.
//
// The relay runs as a container that an admission policy adds to every virt-launcher pod (the way
// NVIDIA's DSX IMDS does). It puts 169.254.169.254 on the pod's loopback, so the guest's requests
// to it, which arrive through the masquerade gateway, are delivered in the pod; then it gives up
// root and every capability before it serves. It holds one thing: a token of the pod's service
// account (the machine's own, named by its machine ID), projected for the metadata server only and
// mounted in this container only. The guest, inside QEMU, never sees it.
//
// The server (Server) is a Deployment of its own. It verifies the relay's token against the
// cluster's keys, checks it names a live launcher pod of that machine, and asks the API server for
// a token of the machine's service account with the audience the guest asked for (TokenRequest,
// bound to the pod).
package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// RelayConfig is how the relay runs.
type RelayConfig struct {
	Address   string // the metadata address, 169.254.169.254
	Port      int    // 80
	TokenFile string // the projected service account token (audience: the metadata server)
	Upstream  string // the metadata server, http://host:port
	UID, GID  int    // who the relay is once it has the address and its socket
}

// RunRelay puts the address on lo, listens, drops root, and serves until ctx ends.
func RunRelay(ctx context.Context, log *slog.Logger, c RelayConfig) error {
	ip := net.ParseIP(c.Address).To4()
	if ip == nil {
		return fmt.Errorf("address %q is not IPv4", c.Address)
	}
	if err := addLoopbackAddress(ip); err != nil {
		return fmt.Errorf("adding %s to lo: %w", ip, err)
	}
	// Replies go one hop: to the guest, and no further (as NVIDIA's IMDS does).
	lc := net.ListenConfig{Control: func(_, _ string, raw syscall.RawConn) error {
		var serr error
		if err := raw.Control(func(fd uintptr) { serr = unix.SetsockoptInt(int(fd), unix.IPPROTO_IP, unix.IP_TTL, 1) }); err != nil {
			return err
		}
		return serr
	}}
	ln, err := lc.Listen(ctx, "tcp4", net.JoinHostPort(c.Address, fmt.Sprint(c.Port)))
	if err != nil {
		return err
	}
	// Root and the capabilities were for the address and the port; nothing after needs them.
	// Go applies these to every thread, and leaving uid 0 clears the capabilities.
	if err := syscall.Setgroups([]int{c.GID}); err != nil {
		return fmt.Errorf("setgroups: %w", err)
	}
	if err := syscall.Setgid(c.GID); err != nil {
		return fmt.Errorf("setgid: %w", err)
	}
	if err := syscall.Setuid(c.UID); err != nil {
		return fmt.Errorf("setuid: %w", err)
	}
	srv := &http.Server{Handler: RelayHandler(c, http.DefaultClient), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shut)
	}()
	log.Info("metadata relay serving", "address", ln.Addr().String(), "upstream", c.Upstream)
	if err := srv.Serve(ln); !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Headers a proxy adds: a request carrying one came through something that forwards requests, which
// a tricked web app or proxy in the guest would be, so it is refused.
var forwardHeaders = []string{"Forwarded", "X-Forwarded-For", "X-Forwarded-Host", "X-Forwarded-Proto", "X-Real-Ip", "Via"}

// RelayHandler answers the guest. GET /v1/token?audience=… with the header "Metadata: true".
func RelayHandler(c RelayConfig, client *http.Client) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { io.WriteString(w, "ok\n") })
	mux.HandleFunc("GET /v1/token", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.Header.Get("Metadata") != "true" {
			fail(w, http.StatusBadRequest, `Send the header "Metadata: true".`)
			return
		}
		for _, h := range forwardHeaders {
			if r.Header.Get(h) != "" {
				fail(w, http.StatusBadRequest, "Requests forwarded by a proxy are refused.")
				return
			}
		}
		aud := r.URL.Query()["audience"]
		if len(aud) != 1 || aud[0] == "" {
			fail(w, http.StatusBadRequest, "Give exactly one audience: ?audience=<who the token is for>.")
			return
		}
		token, err := os.ReadFile(c.TokenFile) // read each time: the kubelet replaces it
		if err != nil {
			fail(w, http.StatusServiceUnavailable, "The machine's identity is not available.")
			return
		}
		ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
		defer cancel()
		req, _ := http.NewRequestWithContext(ctx, "GET", c.Upstream+"/v1/token?audience="+url.QueryEscape(aud[0]), nil)
		req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(token)))
		resp, err := client.Do(req)
		if err != nil {
			fail(w, http.StatusBadGateway, "The metadata server can't be reached.")
			return
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		if err != nil {
			fail(w, http.StatusBadGateway, "The metadata server's answer was cut off.")
			return
		}
		if resp.StatusCode == http.StatusOK && strings.HasPrefix(r.Header.Get("Accept"), "text/plain") {
			var t struct {
				Token string `json:"token"`
			}
			if json.Unmarshal(body, &t) == nil && t.Token != "" {
				w.Header().Set("Content-Type", "text/plain; charset=utf-8")
				io.WriteString(w, t.Token)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		w.Write(body)
	})
	return mux
}

func fail(w http.ResponseWriter, status int, message string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"detail": message})
}
