package metadata

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	"github.com/go-jose/go-jose/v4"
	"github.com/go-jose/go-jose/v4/jwt"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
)

const ACC = "identity-build-aaaaaaaa"

const (
	issuer   = "https://kubernetes.example"
	audience = "https://metadata.example"
	ns       = "cloud"
)

type signer struct {
	key *rsa.PrivateKey
	s   jose.Signer
}

func newSigner(t *testing.T) *signer {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	s, err := jose.NewSigner(jose.SigningKey{Algorithm: jose.RS256, Key: key}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return &signer{key, s}
}

// relayToken is what the kubelet would project into the relay of pod (namespace, sa).
func (s *signer) relayToken(t *testing.T, namespace, sa, pod, uid string) string {
	claims := map[string]any{
		"iss": issuer, "aud": []string{audience}, "sub": "system:serviceaccount:" + namespace + ":" + sa,
		"exp": time.Now().Add(time.Hour).Unix(), "iat": time.Now().Unix(),
		"kubernetes.io": map[string]any{
			"namespace": namespace, "pod": map[string]string{"name": pod, "uid": uid},
			"serviceaccount": map[string]string{"name": sa, "uid": "sa-uid"},
		},
	}
	raw, err := jwt.Signed(s.s).Claims(claims).Serialize()
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func launcher(name, uid, account string) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, UID: types.UID(uid),
			Labels: map[string]string{"kubevirt.io": "virt-launcher", "vm.kubevirt.io/name": "vm-abcdef"}},
		Spec: corev1.PodSpec{ServiceAccountName: account},
	}
}

func testServer(t *testing.T, s *signer, pods ...*corev1.Pod) (*Server, *atomic.Int32) {
	keys := &oidc.StaticKeySet{PublicKeys: []crypto.PublicKey{&s.key.PublicKey}}
	mints := new(atomic.Int32)
	srv := &Server{
		Namespace: ns, Issuer: issuer, RefusedAudiences: []string{"https://kubernetes.default.svc", audience},
		Verifier: oidc.NewVerifier(issuer, keys, &oidc.Config{ClientID: audience}),
		Pod: func(name string) (*corev1.Pod, bool) {
			for _, p := range pods {
				if p.Name == name {
					return p, true
				}
			}
			return nil, false
		},
		Mint: func(_ context.Context, sa string, pod *corev1.Pod, aud string) (Minted, error) {
			mints.Add(1)
			return Minted{Token: sa + "|" + string(pod.UID) + "|" + aud, Expires: time.Now().Add(time.Hour)}, nil
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now: time.Now,
	}
	return srv, mints
}

func ask(h http.Handler, bearer, aud string) (int, map[string]any) {
	r := httptest.NewRequest("GET", "/v1/token?audience="+aud, nil)
	if bearer != "" {
		r.Header.Set("Authorization", "Bearer "+bearer)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	var body map[string]any
	json.Unmarshal(w.Body.Bytes(), &body)
	return w.Code, body
}

func TestServerMintsForTheMachinesLivePod(t *testing.T) {
	s := newSigner(t)
	srv, mints := testServer(t, s, launcher("virt-launcher-vm-abcdef-x", "uid-1", ACC))
	h := srv.Handler()
	good := s.relayToken(t, ns, ACC, "virt-launcher-vm-abcdef-x", "uid-1")

	code, body := ask(h, good, "https://files.example")
	if code != 200 || body["token"] != ACC+"|uid-1|https://files.example" || body["subject"] != "system:serviceaccount:cloud:"+ACC {
		t.Fatalf("good request = %d %v", code, body)
	}
	if code, _ := ask(h, good, "https://files.example"); code != 200 || mints.Load() != 1 {
		t.Errorf("asking again = %d, %d mints; want the kept token", code, mints.Load())
	}
	srv.Forget("uid-1")
	if ask(h, good, "https://files.example"); mints.Load() != 2 {
		t.Errorf("after the pod went, %d mints; want a new one", mints.Load())
	}

	for name, bearer := range map[string]string{
		"no token":               "",
		"garbage":                "not-a-jwt",
		"other namespace":        s.relayToken(t, "elsewhere", ACC, "virt-launcher-vm-abcdef-x", "uid-1"),
		"pod gone (other uid)":   s.relayToken(t, ns, ACC, "virt-launcher-vm-abcdef-x", "uid-old"),
		"no such pod":            s.relayToken(t, ns, ACC, "virt-launcher-vm-zzzzzz-y", "uid-1"),
		"another identity's sa":  s.relayToken(t, ns, "identity-other-bbbbbbbb", "virt-launcher-vm-abcdef-x", "uid-1"),
		"not an identity's sa":   s.relayToken(t, ns, "vmservice", "virt-launcher-vm-abcdef-x", "uid-1"),
		"signed by someone else": newSigner(t).relayToken(t, ns, ACC, "virt-launcher-vm-abcdef-x", "uid-1"),
	} {
		if code, _ := ask(h, bearer, "https://files.example"); code != http.StatusUnauthorized {
			t.Errorf("%s = %d, want 401", name, code)
		}
	}
	for _, aud := range []string{"", "a%0Ab", strings.Repeat("a", 257), "https://kubernetes.default.svc", audience} {
		if code, _ := ask(h, good, aud); code != http.StatusBadRequest {
			t.Errorf("audience %q = %d, want 400", aud, code)
		}
	}
}

func TestServerCapsAudiencesPerPod(t *testing.T) {
	s := newSigner(t)
	srv, _ := testServer(t, s, launcher("p", "uid-1", ACC))
	h := srv.Handler()
	good := s.relayToken(t, ns, ACC, "p", "uid-1")
	for i := 0; i < maxAudiences; i++ {
		if code, _ := ask(h, good, "aud"+string(rune('a'+i))); code != 200 {
			t.Fatalf("audience %d = %d", i, code)
		}
	}
	if code, _ := ask(h, good, "one-too-many"); code != http.StatusTooManyRequests {
		t.Errorf("audience %d = %d, want 429", maxAudiences+1, code)
	}
	if code, _ := ask(h, good, "auda"); code != 200 {
		t.Errorf("a kept audience after the cap = %d, want 200", code)
	}
}

func TestRelay(t *testing.T) {
	var gotAuth, gotAud string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth, gotAud = r.Header.Get("Authorization"), r.URL.Query().Get("audience")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"token":"tok","expiresIn":3600}`)
	}))
	defer up.Close()
	file := filepath.Join(t.TempDir(), "token")
	os.WriteFile(file, []byte("relay-token\n"), 0o600)
	h := RelayHandler(RelayConfig{TokenFile: file, Upstream: up.URL}, up.Client())

	do := func(target string, headers map[string]string) *httptest.ResponseRecorder {
		r := httptest.NewRequest("GET", target, nil)
		for k, v := range headers {
			r.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w
	}
	md := map[string]string{"Metadata": "true"}
	if w := do("/v1/token?audience=https://files.example/x", md); w.Code != 200 || !strings.Contains(w.Body.String(), `"tok"`) ||
		gotAuth != "Bearer relay-token" || gotAud != "https://files.example/x" || w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("relay = %d %q, upstream saw %q %q", w.Code, w.Body.String(), gotAuth, gotAud)
	}
	if w := do("/v1/token?audience=a", map[string]string{"Metadata": "true", "Accept": "text/plain"}); w.Body.String() != "tok" {
		t.Errorf("text/plain = %q", w.Body.String())
	}
	for name, c := range map[string]struct {
		target  string
		headers map[string]string
	}{
		"no Metadata header": {"/v1/token?audience=a", nil},
		"through a proxy":    {"/v1/token?audience=a", map[string]string{"Metadata": "true", "X-Forwarded-For": "1.2.3.4"}},
		"no audience":        {"/v1/token", md},
		"two audiences":      {"/v1/token?audience=a&audience=b", md},
	} {
		if w := do(c.target, c.headers); w.Code != http.StatusBadRequest {
			t.Errorf("%s = %d, want 400", name, w.Code)
		}
	}
}

func TestFailedMintsDontUseUpAudiences(t *testing.T) {
	s := newSigner(t)
	srv, _ := testServer(t, s, launcher("p", "uid-1", ACC))
	ok := srv.Mint
	srv.Mint = func(context.Context, string, *corev1.Pod, string) (Minted, error) {
		return Minted{}, io.ErrUnexpectedEOF
	}
	h := srv.Handler()
	good := s.relayToken(t, ns, ACC, "p", "uid-1")
	for i := 0; i < maxAudiences+4; i++ {
		ask(h, good, "aud"+string(rune('a'+i)))
	}
	srv.Mint = ok
	if code, _ := ask(h, good, "after-the-blip"); code != 200 {
		t.Errorf("after %d failed mints = %d, want 200", maxAudiences+4, code)
	}
}
