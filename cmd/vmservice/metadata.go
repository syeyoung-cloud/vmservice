package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/coreos/go-oidc/v3/oidc"
	authenticationv1 "k8s.io/api/authentication/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/informers"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	ctrlconfig "sigs.k8s.io/controller-runtime/pkg/client/config"

	"github.com/cyoung06/vmservice/internal/metadata"
)

// runRelay is the container the admission policy adds to every launcher pod.
func runRelay(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("relay", flag.ExitOnError)
	var c metadata.RelayConfig
	fs.StringVar(&c.Address, "address", "169.254.169.254", "the metadata address, put on the pod's lo")
	fs.IntVar(&c.Port, "port", 80, "the port the guest asks on")
	fs.StringVar(&c.TokenFile, "token-file", "/var/run/vmservice-metadata/token", "the projected token of the pod's service account")
	fs.StringVar(&c.Upstream, "upstream", "", "the metadata server, http://host:port (required)")
	fs.IntVar(&c.UID, "uid", 65534, "the uid to serve as, once the address and port are had")
	fs.IntVar(&c.GID, "gid", 107, "the gid to serve as (the pod's fsGroup, which may read the token)")
	fs.Parse(args)
	if c.Upstream == "" {
		log.Error("-upstream is required")
		return 2
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	if err := metadata.RunRelay(ctx, log, c); err != nil {
		log.Error("relay", "err", err)
		return 1
	}
	return 0
}

// runMetadata is the metadata server's Deployment.
func runMetadata(log *slog.Logger, args []string) int {
	fs := flag.NewFlagSet("metadata", flag.ExitOnError)
	namespace := fs.String("namespace", "", "the machines' namespace (required)")
	listen := fs.String("listen", ":8081", "where the relays reach the server")
	issuer := fs.String("issuer", "", "the cluster's service account issuer (required)")
	audience := fs.String("audience", "", "the audience the relays' tokens are made out to (required)")
	jwksURL := fs.String("jwks-url", "", "the cluster's signing keys (default: the API server's /openid/v1/jwks)")
	fs.Parse(args)
	for name, v := range map[string]string{"namespace": *namespace, "issuer": *issuer, "audience": *audience} {
		if v == "" {
			log.Error("-" + name + " is required")
			return 2
		}
	}
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGTERM, syscall.SIGINT)
	defer stop()
	cfg, err := ctrlconfig.GetConfig()
	if err != nil {
		log.Error("kubernetes config", "err", err)
		return 1
	}
	cs, err := kubernetes.NewForConfig(cfg)
	if err != nil {
		log.Error("kubernetes client", "err", err)
		return 1
	}
	// The cluster's keys are behind the API server, which takes no anonymous requests: they are
	// fetched with this pod's own credentials (every service account may read them).
	transport, err := rest.TransportFor(cfg)
	if err != nil {
		log.Error("kubernetes transport", "err", err)
		return 1
	}
	if *jwksURL == "" {
		*jwksURL = strings.TrimSuffix(cfg.Host, "/") + "/openid/v1/jwks"
	}
	keyCtx := oidc.ClientContext(ctx, &http.Client{Transport: transport, Timeout: 15 * time.Second})
	verifier := oidc.NewVerifier(*issuer, oidc.NewRemoteKeySet(keyCtx, *jwksURL), &oidc.Config{ClientID: *audience})

	factory := informers.NewSharedInformerFactoryWithOptions(cs, 0, informers.WithNamespace(*namespace),
		informers.WithTweakListOptions(func(o *metav1.ListOptions) { o.LabelSelector = "kubevirt.io=virt-launcher" }))
	pods := factory.Core().V1().Pods()
	srv := &metadata.Server{
		Namespace: *namespace, Issuer: *issuer, Verifier: verifier, Log: log, Now: time.Now,
		Pod: func(name string) (*corev1.Pod, bool) {
			p, err := pods.Lister().Pods(*namespace).Get(name)
			return p, err == nil
		},
		Mint: func(ctx context.Context, sa string, pod *corev1.Pod, aud string) (metadata.Minted, error) {
			ttl := int64(metadata.TokenTTL().Seconds())
			tr, err := cs.CoreV1().ServiceAccounts(*namespace).CreateToken(ctx, sa, &authenticationv1.TokenRequest{
				Spec: authenticationv1.TokenRequestSpec{
					Audiences: []string{aud}, ExpirationSeconds: &ttl,
					BoundObjectRef: &authenticationv1.BoundObjectReference{Kind: "Pod", APIVersion: "v1", Name: pod.Name, UID: pod.UID},
				},
			}, metav1.CreateOptions{})
			if err != nil {
				return metadata.Minted{}, err
			}
			return metadata.Minted{Token: tr.Status.Token, Expires: tr.Status.ExpirationTimestamp.Time}, nil
		},
	}
	pods.Informer().AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		DeleteFunc: func(obj any) {
			if t, ok := obj.(toolscache.DeletedFinalStateUnknown); ok {
				obj = t.Obj
			}
			if p, ok := obj.(*corev1.Pod); ok {
				srv.Forget(string(p.UID))
			}
		},
	})
	factory.Start(ctx.Done())
	if !toolscache.WaitForCacheSync(ctx.Done(), pods.Informer().HasSynced) {
		log.Error("pod cache did not sync")
		return 1
	}
	hs := &http.Server{Addr: *listen, Handler: srv.Handler(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shut, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		hs.Shutdown(shut)
	}()
	log.Info("metadata server listening", "addr", *listen, "issuer", *issuer, "jwks", *jwksURL)
	if err := hs.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
		log.Error("serve", "err", err)
		return 1
	}
	return 0
}
