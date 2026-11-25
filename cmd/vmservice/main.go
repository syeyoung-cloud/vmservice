// vmservice is a self-service portal for KubeVirt VMs: members sign in with OpenID Connect and
// create, start, stop and delete their own machines, and open their serial and VNC consoles in the
// browser. Every machine lives in one namespace, labeled with its owner; members never get
// Kubernetes credentials.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/cyoung06/vmservice/internal/api"
	"github.com/cyoung06/vmservice/internal/auth"
	"github.com/cyoung06/vmservice/internal/bastion"
	"github.com/cyoung06/vmservice/internal/config"
	"github.com/cyoung06/vmservice/internal/console"
	"github.com/cyoung06/vmservice/internal/inspect"
	"github.com/cyoung06/vmservice/internal/limit"
	"github.com/cyoung06/vmservice/internal/machines"
	"github.com/cyoung06/vmservice/internal/store"
	"github.com/cyoung06/vmservice/internal/web"
)

var version = "dev"

// publicSSH parses -ssh-public (host:port); empty: no bastion is advertised.
func publicSSH(public string) (*web.SSH, error) {
	if public == "" {
		return nil, nil
	}
	host, port, err := net.SplitHostPort(public)
	n, perr := strconv.Atoi(port)
	if err != nil || perr != nil || n < 1 || n > 65535 || host == "" {
		return nil, fmt.Errorf("need host:port, got %q", public)
	}
	return &web.SSH{Host: host, Port: n}, nil
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if len(os.Args) > 1 && os.Args[1] == "bastion" {
		os.Exit(runBastion(log, os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "relay" {
		os.Exit(runRelay(log, os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "metadata" {
		os.Exit(runMetadata(log, os.Args[2:]))
	}
	if len(os.Args) > 1 && os.Args[1] == "inspect" {
		os.Exit(runInspect(os.Args[2:]))
	}
	var a auth.Config
	var ui web.Config
	listen := flag.String("listen", ":8080", "listen address (plain HTTP; TLS terminates in front)")
	namespace := flag.String("namespace", "", "the namespace of the machines and the images (required)")
	catalogFile := flag.String("catalog", "", "the catalog file: images, sizes, disk range, limits (required)")
	databaseURLFile := flag.String("database-url-file", "", "file holding the Postgres URL, e.g. postgres://user:pass@host/db (required)")
	flag.StringVar(&a.Origin, "origin", "", "the public origin, e.g. https://vm.example.org (required)")
	flag.StringVar(&a.Issuer, "oidc-issuer", "", "the OpenID Connect issuer URL (required)")
	flag.StringVar(&a.ClientID, "oidc-client-id", "", "the OIDC client id (required)")
	clientSecretFile := flag.String("oidc-client-secret-file", "", "file holding the OIDC client secret (required)")
	flag.StringVar(&a.Group, "group", "", "group the ID token must list to sign in (empty: anyone the provider signs in)")
	flag.StringVar(&a.AdminGroup, "admin-group", "", "group whose members are admins: every member's machines and images, quotas, no limits (empty: none)")
	flag.StringVar(&a.GroupsClaim, "groups-claim", "groups", "the ID token claim with the user's groups")
	flag.StringVar(&a.UsernameClaim, "username-claim", "preferred_username", "the ID token claim with the username")
	flag.StringVar(&a.KeyFile, "session-key-file", "", "file holding the 32-byte session cookie key, raw or base64 (required)")
	flag.DurationVar(&a.Lifetime, "session-lifetime", 7*24*time.Hour, "how long a sign-in lasts; also how long a removed member keeps access")
	maxStreams := flag.Int("max-streams", 8, "open consoles and live views per member, together")
	sshPublic := flag.String("ssh-public", "", "the bastion's address as members reach it, host:port, shown in the UI (empty: none); the bastion itself is \"vmservice bastion\"")
	var media machines.MediaConfig
	flag.StringVar(&media.UploadProxy, "cdi-upload-proxy", "https://cdi-uploadproxy.cdi.svc", "CDI's upload proxy, for media uploads")
	flag.StringVar(&media.UploadCA, "cdi-upload-ca", "cdi/cdi-uploadproxy-signer-bundle", "namespace/name of the ConfigMap holding the upload proxy's CA")
	flag.StringVar(&media.InspectImage, "inspect-image", "", "the image the media inspector Jobs run: this vmservice's own (required)")
	flag.StringVar(&media.PullSecret, "inspect-pull-secret", "", "image pull secret for -inspect-image")
	flag.StringVar(&media.DriversImage, "virtio-drivers-image", "quay.io/kubevirt/virtio-container-disk:v1.9.1", "the virtio driver CD attached to Windows installs")
	bastionRole := flag.String("bastion-db-role", "", "a database role the bastion runs as: granted read on members, machines and ssh_keys at every start (empty: none)")
	iconsDir := flag.String("icons", "", "a directory with the site's icons: favicon.svg, favicon.ico, apple-touch-icon.png (any may be missing)")
	flag.StringVar(&ui.Title, "title", "VMService", "name shown in the sidebar and the tab title")
	flag.StringVar(&ui.LoginProvider, "login-provider", "", "the sign-in provider's name for the button, e.g. \"Keycloak\" (empty: \"Sign in\")")
	flag.Parse()

	for name, v := range map[string]string{"namespace": *namespace, "catalog": *catalogFile, "database-url-file": *databaseURLFile, "origin": a.Origin, "oidc-issuer": a.Issuer,
		"oidc-client-id": a.ClientID, "inspect-image": media.InspectImage, "oidc-client-secret-file": *clientSecretFile, "session-key-file": a.KeyFile} {
		if v == "" {
			log.Error("-" + name + " is required")
			os.Exit(2)
		}
	}
	a.Origin = strings.TrimRight(a.Origin, "/")
	secret, err := os.ReadFile(*clientSecretFile)
	if err != nil {
		log.Error("client secret", "err", err)
		os.Exit(1)
	}
	a.ClientSecret = strings.TrimSpace(string(secret))
	catalog, err := config.Load(*catalogFile)
	if err != nil {
		log.Error("catalog", "err", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	dbURL, err := os.ReadFile(*databaseURLFile)
	if err != nil {
		log.Error("database url", "err", err)
		os.Exit(1)
	}
	db, err := store.Open(ctx, strings.TrimSpace(string(dbURL)))
	if err != nil {
		log.Error("database", "err", err)
		os.Exit(1)
	}
	defer db.Close()
	a.OnSignIn = db.UpsertMember
	signIn, err := auth.New(ctx, a, log)
	if err != nil {
		log.Error("sign-in", "err", err)
		os.Exit(1)
	}
	kube, err := ctrlconfig.GetConfig()
	if err != nil {
		log.Error("kubernetes config", "err", err)
		os.Exit(1)
	}
	vms, err := machines.New(ctx, kube, *namespace, catalog, db, log)
	if err != nil {
		log.Error("machines", "err", err)
		os.Exit(1)
	}
	vms.MediaConfig = media
	proxy, err := console.New(kube, machines.APIServer(kube))
	if err != nil {
		log.Error("console", "err", err)
		os.Exit(1)
	}
	if *iconsDir != "" {
		ui.Icons = map[string][]byte{}
		for name := range web.IconFiles {
			if data, err := os.ReadFile(filepath.Join(*iconsDir, name)); err == nil {
				ui.Icons[name] = data
			}
		}
	}
	streams := &limit.PerOwner{Max: int64(*maxStreams)}
	if ui.SSH, err = publicSSH(*sshPublic); err != nil {
		log.Error("-ssh-public", "err", err)
		os.Exit(2)
	}
	if *bastionRole != "" {
		if err := db.GrantRead(ctx, *bastionRole); err != nil {
			log.Error("granting the bastion's role", "role", *bastionRole, "err", err)
			os.Exit(1)
		}
	}
	h, err := web.Handler(web.UI(), ui, a.Origin, signIn.Routes, api.New(signIn, vms, proxy, a.Origin, streams, log).Routes)
	if err != nil {
		log.Error("ui", "err", err)
		os.Exit(1)
	}
	// The event stream and the consoles are long-lived: each clears these deadlines for itself.
	srv := &http.Server{Addr: *listen, Handler: h, ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second, WriteTimeout: time.Minute}
	go func() { <-ctx.Done(); srv.Close() }()
	log.Info("vmservice listening", "addr", *listen, "version", version, "namespace", *namespace)
	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		log.Error("serve", "err", err)
		os.Exit(1)
	}
}

// runBastion is "vmservice bastion": the open SSH jump host (port 22 of machines, by name), in a
// process of its own with only read access (its database role reads machines; its service account
// only reads VirtualMachineInstances).
func runBastion(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("bastion", flag.ExitOnError)
	namespace := fs.String("namespace", "", "the machines' namespace (required)")
	databaseURLFile := fs.String("database-url-file", "", "file holding the Postgres URL of a role that may read machines (required)")
	listen := fs.String("listen", ":2222", "the SSH listen address")
	hostKey := fs.String("host-key-file", "", "the host key, an OpenSSH private key file (required)")
	maxSessions := fs.Int("max-sessions", 8, "open SSH sessions per source address")
	fs.Parse(args)
	for name, v := range map[string]string{"namespace": *namespace, "database-url-file": *databaseURLFile, "host-key-file": *hostKey} {
		if v == "" {
			log.Error("-" + name + " is required")
			return 2
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	dbURL, err := os.ReadFile(*databaseURLFile)
	if err != nil {
		log.Error("database url", "err", err)
		return 1
	}
	db, err := store.OpenReadOnly(ctx, strings.TrimSpace(string(dbURL)))
	if err != nil {
		log.Error("database", "err", err)
		return 1
	}
	defer db.Close()
	kube, err := ctrlconfig.GetConfig()
	if err != nil {
		log.Error("kubernetes config", "err", err)
		return 1
	}
	lookup, err := machines.NewLookup(ctx, kube, *namespace, db, log)
	if err != nil {
		log.Error("lookup", "err", err)
		return 1
	}
	b, err := bastion.New(*hostKey, lookup, &limit.PerOwner{Max: int64(*maxSessions)}, log) // per source address
	if err != nil {
		log.Error("bastion", "err", err)
		return 1
	}
	l, err := net.Listen("tcp", *listen)
	if err != nil {
		log.Error("bastion", "err", err)
		return 1
	}
	log.Info("bastion listening", "addr", *listen, "version", version, "namespace", *namespace)
	if err := b.Serve(ctx, l); err != nil {
		log.Error("bastion", "err", err)
		return 1
	}
	return 0
}

// runInspect is "vmservice inspect -file F | -url U", run in the inspector Jobs: it prints what the
// file is (inspect.Result) as JSON, or {"error": "..."}, to stdout and the termination log, which
// the portal reads. It always exits 0 so a readable answer is never a failed Job.
func runInspect(args []string) int {
	fs := flag.NewFlagSet("inspect", flag.ExitOnError)
	file := fs.String("file", "", "a file to inspect (a medium's disk.img)")
	u := fs.String("url", "", "a URL to probe (its first 64 KiB)")
	fs.Parse(args)
	var r inspect.Result
	var err error
	switch {
	case *file != "":
		var f *os.File
		if f, err = os.Open(*file); err == nil {
			var st os.FileInfo
			if st, err = f.Stat(); err == nil {
				r, err = inspect.File(f, st.Size())
			}
			f.Close()
		}
	case *u != "":
		r, err = inspect.Probe(context.Background(), *u)
	default:
		err = fmt.Errorf("give -file or -url")
	}
	var out []byte
	if err != nil {
		out, _ = json.Marshal(map[string]string{"error": err.Error()})
	} else {
		out, _ = json.Marshal(r)
	}
	os.Stdout.Write(append(out, '\n'))
	os.WriteFile("/dev/termination-log", out, 0o644)
	return 0
}
