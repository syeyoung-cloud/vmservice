package bastion

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/pem"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"

	"github.com/syeyoung-cloud/vmservice/internal/limit"
)

type fakeMachines struct{}

func (fakeMachines) Address(_ context.Context, host string) (string, error) {
	if host == "mybox" || host == "vm-aaaaaa" {
		return "127.0.0.1", nil
	}
	return "", errors.New("No such machine.")
}

func setup(t *testing.T, max int64) string {
	t.Helper()
	echo, _ := net.Listen("tcp", "127.0.0.1:0") // the machine's "sshd": echoes
	t.Cleanup(func() { echo.Close() })
	go func() {
		for {
			c, err := echo.Accept()
			if err != nil {
				return
			}
			go func() { io.Copy(c, c); c.Close() }()
		}
	}()
	_, hostPriv, _ := ed25519.GenerateKey(rand.Reader)
	block, _ := ssh.MarshalPrivateKey(hostPriv, "")
	hostFile := filepath.Join(t.TempDir(), "host")
	os.WriteFile(hostFile, pem.EncodeToMemory(block), 0o600)
	b, err := New(hostFile, fakeMachines{}, &limit.PerOwner{Max: max}, slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil {
		t.Fatal(err)
	}
	_, port, _ := net.SplitHostPort(echo.Addr().String())
	b.dialPort, _ = strconv.Atoi(port)
	l, _ := net.Listen("tcp", "127.0.0.1:0")
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go b.Serve(ctx, l)
	return l.Addr().String()
}

// Anyone may connect: no key, no password (ssh -J sends "none" auth first).
func dial(addr string) (*ssh.Client, error) {
	return ssh.Dial("tcp", addr, &ssh.ClientConfig{User: "anyone", HostKeyCallback: ssh.InsecureIgnoreHostKey(), Timeout: 5 * time.Second})
}

func TestOpenButOnlyMachinesPort22(t *testing.T) {
	addr := setup(t, 8)
	c, err := dial(addr)
	if err != nil {
		t.Fatalf("anyone should get in: %v", err)
	}
	defer c.Close()
	for _, target := range []string{"mybox:22", "vm-aaaaaa:22"} {
		conn, err := c.Dial("tcp", target)
		if err != nil {
			t.Fatalf("forward to %s: %v", target, err)
		}
		conn.Write([]byte("SSH-2.0-hi\r\n"))
		buf := make([]byte, 12)
		conn.SetReadDeadline(time.Now().Add(2 * time.Second))
		if _, err := io.ReadFull(conn, buf); err != nil || string(buf) != "SSH-2.0-hi\r\n" {
			t.Errorf("%s echo = %q, %v", target, buf, err)
		}
		conn.Close()
	}
	for _, target := range []string{"nothere:22", "mybox:80", "127.0.0.1:22", "10.0.0.1:22", "example.com:22"} {
		if conn, err := c.Dial("tcp", target); err == nil {
			conn.Close()
			t.Errorf("forward to %s was allowed", target)
		}
	}
	if _, _, err := c.OpenChannel("session", nil); err == nil {
		t.Error("a shell session was opened")
	}
	if _, err := c.Listen("tcp", "127.0.0.1:0"); err == nil {
		t.Error("remote forwarding was allowed")
	}
}

func TestSessionCapPerAddress(t *testing.T) {
	addr := setup(t, 1)
	c1, err := dial(addr)
	if err != nil {
		t.Fatal(err)
	}
	defer c1.Close()
	c2, err := dial(addr)
	if err != nil {
		return // refused outright: fine
	}
	defer c2.Close()
	if _, err := c2.Dial("tcp", "mybox:22"); err == nil {
		t.Error("a second session from one address over the cap of 1 could forward")
	}
}
