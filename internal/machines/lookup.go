package machines

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"regexp"
	"strings"
	"time"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/syeyoung-cloud/vmservice/internal/store"
)

// Lookup is the read-only view the SSH bastion runs on, in a process of its own: which machine a
// name means (from the store, which it may only read) and where a running machine is (its
// VirtualMachineInstance, the only Kubernetes object it may read). It can't change anything.
type Lookup struct {
	Namespace string
	store     store.Store
	read      client.Reader
}

// NewLookup starts a cache of the namespace's VirtualMachineInstances and returns once it synced.
func NewLookup(ctx context.Context, cfg *rest.Config, namespace string, st store.Store, log *slog.Logger) (*Lookup, error) {
	scheme := runtime.NewScheme()
	if err := kubevirtv1.AddToScheme(scheme); err != nil {
		return nil, err
	}
	c, err := cache.New(cfg, cache.Options{Scheme: scheme, DefaultNamespaces: map[string]cache.Config{namespace: {}}})
	if err != nil {
		return nil, err
	}
	if _, err := c.GetInformer(ctx, &kubevirtv1.VirtualMachineInstance{}); err != nil {
		return nil, err
	}
	go func() {
		if err := c.Start(ctx); err != nil {
			log.Error("cache", "err", err)
		}
	}()
	syncCtx, cancel := context.WithTimeout(ctx, time.Minute)
	defer cancel()
	if !c.WaitForCacheSync(syncCtx) {
		return nil, fmt.Errorf("the cache of %s didn't sync (may the service account list VirtualMachineInstances?)", namespace)
	}
	return &Lookup{Namespace: namespace, store: st, read: c}, nil
}

// Address is the pod-network address of a running machine, by its name (a hostname, unique across
// everyone) or its id. The bastion is open to anyone: the machine's own sshd signs people in.
func (l *Lookup) Address(ctx context.Context, host string) (string, error) {
	if !idFormat.MatchString(host) && !hostName.MatchString(host) {
		return "", ErrNotFound
	}
	// Only the id, from machines alone: the read-only role reads nothing more (a lookup through
	// the portal's machine query once joined a table it may not read, and every jump failed).
	id, err := l.store.MachineID(ctx, strings.ToLower(host))
	if errors.Is(err, store.ErrNotFound) {
		return "", ErrNotFound
	}
	if err != nil {
		return "", err // internal: the bastion shows a generic message and logs it
	}
	var vmi kubevirtv1.VirtualMachineInstance
	if err := l.read.Get(ctx, client.ObjectKey{Namespace: l.Namespace, Name: id}, &vmi); err != nil || vmi.Status.Phase != kubevirtv1.Running {
		return "", refuse(http.StatusConflict, "The machine isn't running.")
	}
	for _, iface := range vmi.Status.Interfaces {
		if iface.Name == "default" && iface.IP != "" {
			return iface.IP, nil
		}
	}
	return "", refuse(http.StatusConflict, "The machine has no address yet.")
}

// hostName is a host typed after ssh -J that could be a machine name.
var hostName = regexp.MustCompile(`^[A-Za-z0-9]([A-Za-z0-9-]{0,61}[A-Za-z0-9])?$`)
