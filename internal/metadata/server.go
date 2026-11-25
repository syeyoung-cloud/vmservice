package metadata

import (
	"context"
	"encoding/json"
	"log/slog"
	"net"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"github.com/coreos/go-oidc/v3/oidc"
	corev1 "k8s.io/api/core/v1"
)

// IdentityAccount is the form of an identity's service account (package machines makes them).
var IdentityAccount = regexp.MustCompile(`^identity-[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?-[a-km-np-z2-9]{8}$`)

// Minted is a token the API server gave for a machine.
type Minted struct {
	Token   string
	Expires time.Time
}

// Server answers the relays: GET /v1/token?audience=… with the relay's token as a bearer.
type Server struct {
	Namespace string
	Issuer    string // the cluster's, which is also what the minted tokens say
	// RefusedAudiences are the API server's (k0s --api-audiences), whose token would work against
	// the cluster, and this server's own, whose token would be as good as a relay's and could be
	// replayed here for more. The admission policy refuses them too.
	RefusedAudiences []string
	Verifier         *oidc.IDTokenVerifier
	// Pod is a launcher pod from the informer's cache.
	Pod func(name string) (*corev1.Pod, bool)
	// Mint asks the API server for a token of the service account, bound to the pod.
	Mint func(ctx context.Context, serviceAccount string, pod *corev1.Pod, audience string) (Minted, error)
	Log  *slog.Logger
	Now  func() time.Time

	// Requests per source address and second, counted before any verification: a token naming an
	// unknown key makes go-oidc fetch the cluster's keys, so unchecked requests would load the API
	// server. Each launcher pod has its own address.
	PerSecond int
	windows   sync.Map // source IP -> *window

	// Minted tokens, per pod and audience: a guest asking again gets the same token until it is
	// near its end, not a call to the API server each time. Dropped when the pod goes (Forget).
	cache sync.Map // pod UID -> *podTokens
}

type podTokens struct {
	n      atomic.Int32
	tokens sync.Map // audience -> Minted
}

const (
	// A guest can ask for this many audiences per pod; their tokens are kept until the pod goes.
	maxAudiences = 16
	// A kept token is handed out until this long before it ends.
	renewBefore = 15 * time.Minute
	tokenTTL    = time.Hour // the Kubernetes default for a projected token
)

// Forget drops what was minted for a pod that is gone.
func (s *Server) Forget(uid string) { s.cache.Delete(uid) }

// kubernetesClaims is what the API server writes into a pod's service account token.
type kubernetesClaims struct {
	K struct {
		Namespace string `json:"namespace"`
		Pod       struct {
			Name string `json:"name"`
			UID  string `json:"uid"`
		} `json:"pod"`
		ServiceAccount struct {
			Name string `json:"name"`
		} `json:"serviceaccount"`
	} `json:"kubernetes.io"`
}

func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok\n")) })
	mux.HandleFunc("GET /v1/token", s.token)
	return mux
}

type window struct {
	second atomic.Int64
	n      atomic.Int32
}

// allow counts a request from ip in the current second. Racy at the second's edge, which only
// lets a few more through.
func (s *Server) allow(ip string) bool {
	if s.PerSecond <= 0 {
		return true
	}
	v, _ := s.windows.LoadOrStore(ip, &window{})
	win := v.(*window)
	now := s.Now().Unix()
	if win.second.Swap(now) != now {
		win.n.Store(0)
	}
	return win.n.Add(1) <= int32(s.PerSecond)
}

func (s *Server) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	ip, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		ip = r.RemoteAddr
	}
	if !s.allow(ip) {
		fail(w, http.StatusTooManyRequests, "Too many requests; slow down.")
		return
	}
	raw, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok {
		fail(w, http.StatusUnauthorized, "No relay token.")
		return
	}
	idt, err := s.Verifier.Verify(r.Context(), raw)
	if err != nil {
		s.Log.Info("relay token refused", "err", err)
		fail(w, http.StatusUnauthorized, "The relay's token is not accepted.")
		return
	}
	var c kubernetesClaims
	if err := idt.Claims(&c); err != nil {
		fail(w, http.StatusUnauthorized, "The relay's token is not accepted.")
		return
	}
	account, podName, podUID := c.K.ServiceAccount.Name, c.K.Pod.Name, c.K.Pod.UID
	// The token must be a launcher pod's, alive now, running as an identity's service account (which
	// only VMService sets, to one its owner owns).
	pod, found := s.Pod(podName)
	if c.K.Namespace != s.Namespace || !IdentityAccount.MatchString(account) || !found || string(pod.UID) != podUID ||
		pod.Labels["kubevirt.io"] != "virt-launcher" || pod.Spec.ServiceAccountName != account || pod.DeletionTimestamp != nil {
		s.Log.Info("relay token names no live machine pod with an identity", "account", account, "pod", podName)
		fail(w, http.StatusUnauthorized, "The relay's token names no running machine with an identity.")
		return
	}
	aud := r.URL.Query()["audience"]
	if len(aud) != 1 || !validAudience(aud[0]) {
		fail(w, http.StatusBadRequest, "Give exactly one audience: one line of text, at most 256 bytes.")
		return
	}
	audience := aud[0]
	for _, refused := range s.RefusedAudiences {
		if audience == refused {
			fail(w, http.StatusBadRequest, "Tokens can't be made out to the cluster's API server or to the metadata service.")
			return
		}
	}
	v, _ := s.cache.LoadOrStore(podUID, &podTokens{})
	pt := v.(*podTokens)
	if m, ok := pt.tokens.Load(audience); ok && m.(Minted).Expires.Sub(s.Now()) > renewBefore {
		s.answer(w, m.(Minted), account)
		return
	}
	_, kept := pt.tokens.Load(audience)
	if !kept {
		if pt.n.Add(1) > maxAudiences {
			pt.n.Add(-1)
			fail(w, http.StatusTooManyRequests, "This machine has asked for too many different audiences.")
			return
		}
	}
	m, err := s.Mint(r.Context(), account, pod, audience)
	if err != nil {
		if !kept {
			pt.n.Add(-1) // nothing was kept for it
		}
		s.Log.Warn("minting a machine token", "account", account, "err", err)
		fail(w, http.StatusServiceUnavailable, "A token can't be made right now; try again.")
		return
	}
	// Two requests at once may both mint; either token is good, and the later one is kept.
	pt.tokens.Store(audience, m)
	s.answer(w, m, account)
}

func (s *Server) answer(w http.ResponseWriter, m Minted, account string) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"token":     m.Token,
		"expiresIn": int(m.Expires.Sub(s.Now()).Seconds()),
		"issuer":    s.Issuer,
		"subject":   "system:serviceaccount:" + s.Namespace + ":" + account,
	})
}

func validAudience(a string) bool {
	if a == "" || len(a) > 256 || !utf8.ValidString(a) {
		return false
	}
	for _, r := range a {
		if r < 0x20 || r == 0x7f {
			return false
		}
	}
	return true
}

// TokenTTL is how long a minted token lasts.
func TokenTTL() time.Duration { return tokenTTL }
