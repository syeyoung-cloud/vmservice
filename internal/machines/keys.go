package machines

import (
	"context"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"golang.org/x/crypto/ssh"

	"github.com/syeyoung-cloud/vmservice/internal/store"
)

// SSH keys live in the store, keyed by the SHA-256 of the key itself: a key belongs to one member
// only (the primary key), which is what lets the bastion tell who is connecting from the key alone.
const (
	MaxKeys      = 20
	minRSABits   = 2048
	maxLabelRune = 63
)

// Key is a member's SSH public key as the API shows it.
type Key struct {
	ID          string    `json:"id"` // hex SHA-256 of the key (the object is "sshkey-<id>")
	Fingerprint string    `json:"fingerprint"`
	Type        string    `json:"type"`
	Label       string    `json:"label"`
	CreatedAt   time.Time `json:"createdAt"`
}

var keyID = regexp.MustCompile(`^[0-9a-f]{64}$`)

// KeyID is the id of a public key: hex SHA-256 of its wire form.
func KeyID(k ssh.PublicKey) string {
	sum := sha256.Sum256(k.Marshal())
	return hex.EncodeToString(sum[:])
}

// ParseKey checks one OpenSSH public key line: ed25519, ECDSA or RSA of at least 2048 bits, and
// no options. It returns the key and the line's comment.
func ParseKey(line string) (ssh.PublicKey, string, error) {
	line = strings.TrimSpace(line)
	if strings.ContainsAny(line, "\r\n") {
		return nil, "", refuse(http.StatusBadRequest, "Paste one key: a single line.")
	}
	key, comment, options, rest, err := ssh.ParseAuthorizedKey([]byte(line))
	if err != nil || len(rest) > 0 {
		return nil, "", refuse(http.StatusBadRequest, "That isn't an OpenSSH public key line (like \"ssh-ed25519 AAAA… you@laptop\").")
	}
	if len(options) > 0 {
		return nil, "", refuse(http.StatusBadRequest, "Keys can't carry options (like command= or from=).")
	}
	switch key.Type() {
	case ssh.KeyAlgoED25519, ssh.KeyAlgoECDSA256, ssh.KeyAlgoECDSA384, ssh.KeyAlgoECDSA521:
	case ssh.KeyAlgoRSA:
		ck, ok := key.(ssh.CryptoPublicKey)
		if !ok {
			return nil, "", refuse(http.StatusBadRequest, "That RSA key couldn't be read.")
		}
		if pk, ok := ck.CryptoPublicKey().(*rsa.PublicKey); !ok || pk.N.BitLen() < minRSABits {
			return nil, "", refuse(http.StatusBadRequest, "RSA keys must be at least %d bits.", minRSABits)
		}
	default:
		return nil, "", refuse(http.StatusBadRequest, "Use an ed25519, ECDSA or RSA key (not %s).", key.Type())
	}
	return key, strings.TrimSpace(comment), nil
}

func toKey(k store.Key) Key {
	out := Key{ID: k.ID, Label: k.Label, CreatedAt: k.CreatedAt.UTC()}
	if pk, _, _, _, err := ssh.ParseAuthorizedKey([]byte(k.Line)); err == nil {
		out.Type, out.Fingerprint = pk.Type(), ssh.FingerprintSHA256(pk)
	}
	return out
}

// Keys is the owner's keys, newest first.
func (s *Service) Keys(ctx context.Context, owner string) ([]Key, error) {
	rows, err := s.store.Keys(ctx, owner)
	if err != nil {
		return nil, s.storeError("reading your SSH keys", err)
	}
	out := make([]Key, 0, len(rows))
	for _, k := range rows {
		out = append(out, toKey(k))
	}
	return out, nil
}

// AddKey registers a key for owner. label empty: the key's comment.
func (s *Service) AddKey(ctx context.Context, owner, line, label string) (Key, error) {
	pk, comment, err := ParseKey(line)
	if err != nil {
		return Key{}, err
	}
	label = strings.TrimSpace(label)
	if label == "" {
		label = comment
	}
	if utf8.RuneCountInString(label) > maxLabelRune {
		if label != comment { // a long comment is just shortened; a long label is the member's mistake
			return Key{}, refuse(http.StatusBadRequest, "The label must be at most %d characters.", maxLabelRune)
		}
		label = string([]rune(label)[:maxLabelRune])
	}
	if strings.ContainsFunc(label, func(r rune) bool { return r < 0x20 || r == 0x7f }) {
		return Key{}, refuse(http.StatusBadRequest, "The label can't contain control characters.")
	}
	k := store.Key{ID: KeyID(pk), Owner: owner, Line: strings.TrimSpace(string(ssh.MarshalAuthorizedKey(pk))), Label: label}
	err = s.store.AddKey(ctx, k, MaxKeys)
	var le *store.LimitError
	switch {
	case errors.As(err, &le):
		return Key{}, refuse(http.StatusForbidden, "%s", le.Message)
	case errors.Is(err, store.ErrExists):
		return Key{}, refuse(http.StatusConflict, "This key is registered by another member (or already by you); ask an admin if it's yours.")
	case err != nil:
		return Key{}, s.storeError("saving the key", err)
	}
	s.log.Info("ssh key added", "owner", owner, "fingerprint", ssh.FingerprintSHA256(pk))
	k.CreatedAt = time.Now()
	return toKey(k), nil
}

// DeleteKey removes one of the owner's keys.
func (s *Service) DeleteKey(ctx context.Context, owner, id string) error {
	if !keyID.MatchString(id) {
		return refuse(http.StatusNotFound, "No such key.")
	}
	err := s.store.DeleteKey(ctx, owner, id)
	if errors.Is(err, store.ErrNotFound) {
		return refuse(http.StatusNotFound, "No such key.")
	}
	if err != nil {
		return s.storeError("deleting the key", err)
	}
	s.log.Info("ssh key deleted", "owner", owner, "id", id)
	return nil
}
