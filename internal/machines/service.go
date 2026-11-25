// Package machines is the members' VMs. A machine is a KubeVirt VirtualMachine in one namespace
// (what it is and does: KubeVirt's spec and status) plus a row in the store (whose it is: the
// rights). The service is the only writer of both; it reads VMs through an informer cache and tells
// listeners about changes as the cache sees them.
//
// Create inserts the row first (its limit check is one SERIALIZABLE transaction), then the VM. A
// crash in between leaves a row without a VM, which the member sees as failed and can delete; never
// a VM nobody owns. Delete deletes the VM; the VM watch removes the row once the VM is gone.
package machines

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/rest"
	toolscache "k8s.io/client-go/tools/cache"
	kubevirtv1 "kubevirt.io/api/core/v1"
	cdiv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/core/v1beta1"
	uploadv1 "kubevirt.io/containerized-data-importer-api/pkg/apis/upload/v1beta1"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cyoung06/vmservice/internal/config"
	"github.com/cyoung06/vmservice/internal/store"
)

// Error is a refusal with the HTTP status the API answers with.
type Error struct {
	Status  int
	Message string
}

func (e *Error) Error() string { return e.Message }

// HTTPStatus marks the error as a refusal meant for the member (its message is safe to show).
func (e *Error) HTTPStatus() int { return e.Status }

func refuse(status int, format string, args ...any) *Error {
	return &Error{status, fmt.Sprintf(format, args...)}
}

// Actor is who's asking: a member, possibly an admin (who may act on every member's machines and
// images, and has no limits).
type Actor struct {
	Owner    string
	Username string
	Admin    bool
}

func (a Actor) may(owner string) bool { return a.Admin || a.Owner == owner }

// ErrNotFound is a machine that doesn't exist or isn't the caller's: the two look the same.
var ErrNotFound = refuse(http.StatusNotFound, "No such machine.")

// Service is the members' machines in Namespace.
type Service struct {
	Namespace string
	Catalog   *config.Catalog
	Config    *rest.Config  // the service account's, for the subresources and the consoles
	read      client.Reader // the informer cache
	write     client.Client // straight to the API server
	store     store.Store
	watch     client.WithWatch // for waiting on one object (an upload becoming ready, a probe Job)
	http      *http.Client
	// MediaConfig is set by the caller before members add media.
	MediaConfig MediaConfig
	hub         *hub
	log         *slog.Logger
}

// New connects to the cluster and starts the cache of the namespace's VirtualMachines and
// DataVolumes. It returns once the cache has synced.
func New(ctx context.Context, cfg *rest.Config, namespace string, catalog *config.Catalog, st store.Store, log *slog.Logger) (*Service, error) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{corev1.AddToScheme, batchv1.AddToScheme, kubevirtv1.AddToScheme, cdiv1.AddToScheme, uploadv1.AddToScheme} {
		if err := add(scheme); err != nil {
			return nil, err
		}
	}
	// Jobs: only the inspector's.
	c, err := cache.New(cfg, cache.Options{Scheme: scheme, DefaultNamespaces: map[string]cache.Config{namespace: {}},
		ByObject: map[client.Object]cache.ByObject{&batchv1.Job{}: {Label: labels.SelectorFromSet(labels.Set{LabelJob: "inspect"})}}})
	if err != nil {
		return nil, err
	}
	write, err := client.NewWithWatch(cfg, client.Options{Scheme: scheme})
	if err != nil {
		return nil, err
	}
	httpClient, err := rest.HTTPClientFor(cfg)
	if err != nil {
		return nil, err
	}
	s := &Service{Namespace: namespace, Catalog: catalog, Config: cfg, read: c, write: write, watch: write, store: st, http: httpClient, log: log}
	s.hub = newHub(ctx, s.render)

	vms, err := c.GetInformer(ctx, &kubevirtv1.VirtualMachine{})
	if err != nil {
		return nil, err
	}
	if _, err := vms.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { s.changed(o) },
		UpdateFunc: func(_, o any) { s.changed(o) },
		DeleteFunc: s.vmGone,
	}); err != nil {
		return nil, err
	}
	disks, err := c.GetInformer(ctx, &cdiv1.DataVolume{})
	if err != nil {
		return nil, err
	}
	// A disk's clone progress is part of its machine's state; a medium's import is the medium's.
	if _, err := disks.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { s.diskChanged(o) },
		UpdateFunc: func(_, o any) { s.diskChanged(o) },
	}); err != nil {
		return nil, err
	}
	jobs, err := c.GetInformer(ctx, &batchv1.Job{})
	if err != nil {
		return nil, err
	}
	if _, err := jobs.AddEventHandler(toolscache.ResourceEventHandlerFuncs{
		AddFunc:    func(o any) { s.jobChanged(o) },
		UpdateFunc: func(_, o any) { s.jobChanged(o) },
	}); err != nil {
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
		return nil, fmt.Errorf("the cache of %s didn't sync (are KubeVirt and CDI installed, and may the service account list them?)", namespace)
	}
	go s.ensureIdentities(ctx)
	return s, nil
}

func (s *Service) disk(ctx context.Context, id string) *cdiv1.DataVolume {
	var dv cdiv1.DataVolume
	if s.read.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: diskName(id)}, &dv) != nil {
		return nil
	}
	return &dv
}

func (s *Service) vm(ctx context.Context, id string) *kubevirtv1.VirtualMachine {
	var vm kubevirtv1.VirtualMachine
	if s.read.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: id}, &vm) != nil {
		return nil
	}
	return &vm
}

// The informers only say which machine changed; the hub renders the event when it sends it. The
// owner label routes the event (only the service sets it); render checks it against the row.

func vmOf(o any) *kubevirtv1.VirtualMachine {
	if t, ok := o.(toolscache.DeletedFinalStateUnknown); ok {
		o = t.Obj
	}
	vm, _ := o.(*kubevirtv1.VirtualMachine)
	return vm
}

func (s *Service) changed(o any) {
	if vm := vmOf(o); vm != nil && vm.Labels[LabelOwner] != "" {
		s.hub.publish(change{owner: vm.Labels[LabelOwner], id: vm.Name})
	}
}

// vmGone removes a deleted VM's row: the row's only remover once a VM existed.
func (s *Service) vmGone(o any) {
	vm := vmOf(o)
	if vm == nil || !idFormat.MatchString(vm.Name) {
		return
	}
	go s.removeRow(vm) // off the informer's goroutine: a slow database mustn't hold up everyone's live state
}

// removeRow removes a deleted VM's row, and its identity if it was to go with it.
func (s *Service) removeRow(vm *kubevirtv1.VirtualMachine) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	gone, err := s.store.DeleteMachine(ctx, vm.Name)
	if err != nil {
		s.log.Error("removing a deleted machine's row", "id", vm.Name, "err", err)
	}
	if gone != "" {
		s.deleteIdentityAccount(ctx, gone)
		s.log.Info("identity deleted with its machine", "id", vm.Name, "account", gone)
	}
	s.changed(vm)
}

func (s *Service) diskChanged(o any) {
	dv, ok := o.(*cdiv1.DataVolume)
	if !ok || dv.Labels[LabelOwner] == "" {
		return
	}
	switch {
	case dv.Labels[LabelMachine] != "":
		s.hub.publish(change{owner: dv.Labels[LabelOwner], id: dv.Labels[LabelMachine]})
	case dv.Labels[LabelMedium] != "":
		s.hub.publish(change{owner: dv.Labels[LabelOwner], id: dv.Labels[LabelMedium], medium: true})
		go s.ensureInspected(dv.DeepCopy()) // off the informer's goroutine
	}
}

func (s *Service) jobChanged(o any) {
	if j, ok := o.(*batchv1.Job); ok && jobFinished(j) {
		go s.inspected(j.DeepCopy())
	}
}

// render is the event for a change as viewer (an owner, or Everyone for admins) may see it, from the
// store and the cache as they are now: the machine or image, or "deleted" when it's gone or not
// theirs to see.
func (s *Service) render(c change, viewer string) Event {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a := Actor{Owner: viewer, Admin: viewer == Everyone}
	if c.medium {
		row, err := s.store.Medium(ctx, c.id)
		if err != nil || !visible(row, a) {
			return Event{Kind: "image-deleted", Data: map[string]string{"id": c.id}}
		}
		return Event{Kind: "image", Data: s.toMedium(ctx, row, a, time.Now())}
	}
	row, err := s.store.Machine(ctx, c.id)
	if err != nil || !a.may(row.Owner) {
		return Event{Kind: "deleted", Data: map[string]string{"id": c.id}}
	}
	return Event{Kind: "vm", Data: toMachine(row, s.vm(ctx, c.id), s.disk(ctx, c.id), time.Now())}
}

// Subscribe returns the owner's machine events until cancel is called or the stream falls behind
// (then the channel is closed and the client reconnects for a new snapshot).
func (s *Service) Subscribe(owner string) (<-chan Event, func()) {
	return s.hub.subscribe(owner)
}

// List is the owner's machines, newest first.
func (s *Service) List(ctx context.Context, owner string) ([]Machine, error) {
	rows, err := s.store.Machines(ctx, owner)
	if err != nil {
		return nil, s.storeError("reading your machines", err)
	}
	return s.machinesOf(ctx, rows), nil
}

// AllMachines is every member's machines (admins).
func (s *Service) AllMachines(ctx context.Context, a Actor) ([]Machine, error) {
	if !a.Admin {
		return nil, refuse(http.StatusForbidden, "Only admins can see every machine.")
	}
	rows, err := s.store.AllMachines(ctx)
	if err != nil {
		return nil, s.storeError("reading the machines", err)
	}
	return s.machinesOf(ctx, rows), nil
}

func (s *Service) machinesOf(ctx context.Context, rows []store.Machine) []Machine {
	now := time.Now()
	out := make([]Machine, 0, len(rows))
	for _, row := range rows {
		out = append(out, toMachine(row, s.vm(ctx, row.ID), s.disk(ctx, row.ID), now))
	}
	return out
}

// get is a machine the actor may act on: its row and, if it exists, its VM. Every request naming a
// machine goes through here first; an id the service couldn't have made is refused before it goes
// anywhere, and someone else's machine looks like no machine (except to admins).
func (s *Service) get(ctx context.Context, a Actor, id string) (store.Machine, *kubevirtv1.VirtualMachine, error) {
	if !idFormat.MatchString(id) {
		return store.Machine{}, nil, ErrNotFound
	}
	row, err := s.store.Machine(ctx, id)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !a.may(row.Owner)) {
		return store.Machine{}, nil, ErrNotFound
	}
	if err != nil {
		return store.Machine{}, nil, s.storeError("reading the machine", err)
	}
	return row, s.vm(ctx, id), nil
}

// Get is a machine the actor may see.
func (s *Service) Get(ctx context.Context, a Actor, id string) (Machine, error) {
	row, vm, err := s.get(ctx, a, id)
	if err != nil {
		return Machine{}, err
	}
	return toMachine(row, vm, s.disk(ctx, id), time.Now()), nil
}

// EnsureMember adds the member if a session from before they had a row is in use.
func (s *Service) EnsureMember(ctx context.Context, owner, username string) error {
	if err := s.store.EnsureMember(ctx, owner, username); err != nil {
		return s.storeError("recording you", err)
	}
	return nil
}

// Usage adds up the owner's machines, running or not.
func (s *Service) Usage(ctx context.Context, owner string) (store.Usage, error) {
	return s.store.Usage(ctx, owner)
}

// hostname is a machine's name: a DNS label, and not shaped like a machine id.
var hostname = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// unixName is a username that can be an account on a machine.
var unixName = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)

// NameAvailable says whether name can be a new machine's name, and why not.
func (s *Service) NameAvailable(ctx context.Context, name string) (bool, string, error) {
	if !hostname.MatchString(name) || idFormat.MatchString(name) {
		return false, "Use lowercase letters, digits and dashes (1 to 63, not starting or ending with a dash).", nil
	}
	if _, err := s.store.MachineByName(ctx, name); err == nil {
		return false, "Another machine already has this name.", nil
	} else if !errors.Is(err, store.ErrNotFound) {
		return false, "", s.storeError("checking the name", err)
	}
	return true, "", nil
}

// limits is a member's effective quota: theirs where set, else the catalog's.
func (s *Service) limits(q store.Quota) store.Quota {
	l := s.Catalog.Limits
	pick := func(v *int, d int) *int {
		if v != nil {
			return v
		}
		return &d
	}
	return store.Quota{VMs: pick(q.VMs, l.VMs), CPU: pick(q.CPU, l.CPU), MemoryMiB: pick(q.MemoryMiB, l.MemoryMiB), DiskGiB: pick(q.DiskGiB, l.DiskGiB)}
}

// EffectiveLimits is the actor's quota (nil fields: no limit, for admins).
func (s *Service) EffectiveLimits(ctx context.Context, a Actor) (store.Quota, error) {
	if a.Admin {
		return store.Quota{}, nil
	}
	m, err := s.store.Member(ctx, a.Owner)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return store.Quota{}, s.storeError("reading your quota", err)
	}
	return s.limits(m.Quota), nil
}

// createTimeout bounds the steps after the row: the VM, its Secret and the start.
const createTimeout = time.Minute

// Create makes a machine for the actor from an image and starts it.
func (s *Service) Create(ctx context.Context, a Actor, r Request) (Machine, error) {
	if ok, why, err := s.NameAvailable(ctx, r.Name); err != nil {
		return Machine{}, err
	} else if !ok {
		status := http.StatusBadRequest
		if hostname.MatchString(r.Name) && !idFormat.MatchString(r.Name) {
			status = http.StatusConflict // valid, but taken
		}
		return Machine{}, refuse(status, "%s", why)
	}
	ref, err := s.resolveMedium(ctx, a, r.MediaID)
	if err != nil {
		return Machine{}, err
	}
	r.media, r.profile, r.driversImage, r.cloudExtras = &ref, ref.Profile, s.MediaConfig.DriversImage, ref.CloudConfig
	if p := r.ProfileOverride; p != "" {
		if p != "windows" && p != "linux" && p != "uefi" && p != "bios" {
			return Machine{}, refuse(http.StatusBadRequest, "There is no profile %q.", p)
		}
		r.profile = p
	}
	if need := (ref.PVCBytes + (1<<30 - 1)) >> 30; int64(r.DiskGiB) < need {
		return Machine{}, refuse(http.StatusBadRequest, "The disk must be at least %d GiB for this image.", need)
	}
	keys, err := s.store.Keys(ctx, a.Owner)
	if err != nil {
		return Machine{}, s.storeError("reading your SSH keys", err)
	}
	for _, k := range keys {
		r.AuthorizedKeys = append(r.AuthorizedKeys, k.Line)
	}
	if r.profile == "windows" {
		r.PasswordHash = "" // Setup makes the account
	} else {
		if !unixName.MatchString(a.Username) {
			return Machine{}, refuse(http.StatusBadRequest, "Your username %q can't be an account name on a machine.", a.Username)
		}
		if len(keys) == 0 && r.PasswordHash == "" {
			return Machine{}, refuse(http.StatusBadRequest, "Set a password or add an SSH key first: otherwise nothing can sign in to the machine.")
		}
		member, err := s.store.Member(ctx, a.Owner)
		if err != nil {
			return Machine{}, s.storeError("reading your account", err)
		}
		r.OwnerName, r.UID = a.Username, member.UID
	}
	// The identity it wears: one of the member's by name, or a new one named after it, which goes
	// again if the machine isn't made.
	newIdentity := ""
	dropNewIdentity := func() {
		if newIdentity != "" && s.store.DeleteIdentity(ctx, newIdentity) == nil {
			s.deleteIdentityAccount(ctx, newIdentity)
		}
	}
	if r.Identity != "" {
		if r.identity = s.ownIdentity(ctx, a.Owner, r.Identity); r.identity == "" {
			return Machine{}, refuse(http.StatusBadRequest, "You have no identity named %q.", r.Identity)
		}
	} else {
		if s.ownIdentity(ctx, a.Owner, r.Name) != "" {
			return Machine{}, refuse(http.StatusConflict, "You already have an identity named %s: choose it for this machine, or name the machine differently.", r.Name)
		}
		i, err := s.newIdentity(ctx, a.Owner, r.Name)
		if err != nil {
			return Machine{}, err
		}
		r.identity, newIdentity = i.Account, i.Account
	}
	check := s.within(a, store.Usage{VMs: 1, CPU: r.Size.CPU, MemoryMiB: r.Size.MemoryMiB, DiskGiB: r.DiskGiB})
	var row store.Machine
	for attempt := 0; ; attempt++ {
		row = store.Machine{ID: newID(), Owner: a.Owner, Name: r.Name, Image: ref.Name, Size: r.Size.ID, CPU: r.Size.CPU, MemoryMiB: r.Size.MemoryMiB, DiskGiB: r.DiskGiB, Media: ref.ID, Profile: r.profile, Identity: r.identity}
		err := s.store.CreateMachine(ctx, row, check)
		var le *store.LimitError
		switch {
		case err == nil:
		case errors.As(err, &le):
			dropNewIdentity()
			return Machine{}, refuse(http.StatusForbidden, "%s", le.Message)
		case errors.Is(err, store.ErrExists):
			if _, nerr := s.store.MachineByName(ctx, r.Name); nerr == nil {
				dropNewIdentity()
				return Machine{}, refuse(http.StatusConflict, "Another machine already has this name.")
			}
			if attempt < 3 {
				continue // the id was taken
			}
			dropNewIdentity()
			return Machine{}, s.storeError("saving the machine", err)
		case errors.Is(err, store.ErrNoIdentity):
			return Machine{}, refuse(http.StatusConflict, "That identity was just deleted; choose another.")
		default:
			dropNewIdentity()
			return Machine{}, s.storeError("saving the machine", err)
		}
		break
	}
	row.CreatedAt, row.OwnerName, row.IdentityName = time.Now(), a.Username, r.Identity
	if newIdentity != "" {
		row.IdentityName = r.Name
	}

	// From here the machine is the member's; the rest finishes even if they go away (one deadline
	// covers it). The VM is created with run strategy Manual (not running), then its cloud-init
	// Secret (owned by it, so it can't outlive it), then it is started.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), createTimeout)
	defer cancel()
	// A failed VM create gives the row (and the limit) back, but only once the API server says the
	// VM really doesn't exist: a create that timed out may still have happened, and its row must stay.
	abandon := func(what string, err error) (Machine, error) {
		var live kubevirtv1.VirtualMachine
		if gerr := s.write.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: row.ID}, &live); apierrors.IsNotFound(gerr) {
			s.store.DeleteMachine(ctx, row.ID)
			dropNewIdentity()
		}
		return Machine{}, s.kubeError(what, err)
	}
	vm := buildVM(s.Namespace, row.ID, a.Owner, s.Catalog.StorageClass, r)
	if err := s.write.Create(ctx, vm); err != nil {
		return abandon("creating the machine", err)
	}
	if hasVolume(vm, "cloudinit") {
		userData, err := cloudConfig(r.Name, r)
		if err == nil {
			err = s.write.Create(ctx, buildSecret(vm, userData))
		}
		if err != nil {
			s.write.Delete(ctx, vm) // the watch then removes the row
			return Machine{}, s.kubeError("creating the machine's cloud-init", err)
		}
	}
	if err := s.subresource(ctx, vm.Name, "start"); err != nil {
		return Machine{}, err
	}
	s.log.Info("machine created", "owner", a.Owner, "id", vm.Name, "name", r.Name, "image", ref.ID, "profile", r.profile, "size", r.Size.ID, "diskGiB", r.DiskGiB)
	return toMachine(row, vm, nil, time.Now()), nil
}

func hasVolume(vm *kubevirtv1.VirtualMachine, name string) bool {
	for _, v := range vm.Spec.Template.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

// within checks that an owner's machines (u) plus add stay within their limits; admins have none.
func (s *Service) within(a Actor, add store.Usage) func(store.Usage, store.Quota) error {
	return func(u store.Usage, q store.Quota) error {
		if a.Admin {
			return nil
		}
		l := s.limits(q)
		switch {
		case u.VMs+add.VMs > *l.VMs:
			return &store.LimitError{Message: fmt.Sprintf("You already have %d of %d machines.", u.VMs, *l.VMs)}
		case u.CPU+add.CPU > *l.CPU:
			return &store.LimitError{Message: fmt.Sprintf("This would use %d of your %d vCPUs.", u.CPU+add.CPU, *l.CPU)}
		case u.MemoryMiB+add.MemoryMiB > *l.MemoryMiB:
			return &store.LimitError{Message: fmt.Sprintf("This would use %d of your %d MiB of memory.", u.MemoryMiB+add.MemoryMiB, *l.MemoryMiB)}
		case u.DiskGiB+add.DiskGiB > *l.DiskGiB:
			return &store.LimitError{Message: fmt.Sprintf("This would use %d of your %d GiB of disk.", u.DiskGiB+add.DiskGiB, *l.DiskGiB)}
		}
		return nil
	}
}

// Resize gives a machine another catalog size, within the owner's limits. Like Eject it leaves a
// running machine alone: the new size applies when it next starts (Restart or Stop/Start here).
func (s *Service) Resize(ctx context.Context, a Actor, id string, size config.Size) error {
	row, _, err := s.get(ctx, a, id)
	if err != nil {
		return err
	}
	// The row is the limit's record, so it changes first; a VM update that fails after it is put
	// right by resizing again (both steps are idempotent).
	check := s.within(a, store.Usage{VMs: 1, CPU: size.CPU, MemoryMiB: size.MemoryMiB, DiskGiB: row.DiskGiB})
	var le *store.LimitError
	switch err := s.store.ResizeMachine(ctx, id, size.ID, size.CPU, size.MemoryMiB, check); {
	case errors.As(err, &le):
		return refuse(http.StatusForbidden, "%s", le.Message)
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case err != nil:
		return s.storeError("saving the size", err)
	}
	memory := resource.MustParse(strconv.Itoa(size.MemoryMiB) + "Mi") // as buildVM writes it
	for attempt := 0; ; attempt++ {
		var vm kubevirtv1.VirtualMachine // fresh from the API server: the update is conditional on it
		if err := s.write.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: id}, &vm); err != nil {
			return s.kubeError("reading the machine", err)
		}
		d := &vm.Spec.Template.Spec.Domain
		d.CPU.Cores = uint32(size.CPU)
		d.Memory.Guest = &memory
		if d.Resources.Requests == nil {
			d.Resources.Requests = corev1.ResourceList{}
		}
		d.Resources.Requests[corev1.ResourceMemory] = memory
		err := s.write.Update(ctx, &vm)
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || attempt == 3 {
			return s.kubeError("resizing the machine", err)
		}
	}
	s.log.Info("machine resized", "by", a.Username, "id", id, "size", size.ID)
	return nil
}

// Eject removes a machine's install CD-ROMs (the ISO and the driver CD) from its spec so it boots
// from its disk. A running machine is left alone: the CDs go when it next starts (Restart or
// Stop/Start here, not a reboot inside the guest). The ISO's per-machine copy is deleted with them;
// PVC protection keeps it until the running machine lets go of it.
func (s *Service) Eject(ctx context.Context, a Actor, id string) error {
	if _, _, err := s.get(ctx, a, id); err != nil {
		return err
	}
	for attempt := 0; ; attempt++ {
		var vm kubevirtv1.VirtualMachine // fresh from the API server: the update is conditional on it
		if err := s.write.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: id}, &vm); err != nil {
			return s.kubeError("reading the machine", err)
		}
		if !hasInstallMedia(&vm) {
			return refuse(http.StatusConflict, "The machine has no install media attached.")
		}
		spec := &vm.Spec.Template.Spec
		cd := func(name string) bool { return name == volumeInstall || name == volumeDrivers }
		spec.Domain.Devices.Disks = slices.DeleteFunc(spec.Domain.Devices.Disks, func(d kubevirtv1.Disk) bool { return cd(d.Name) })
		spec.Volumes = slices.DeleteFunc(spec.Volumes, func(v kubevirtv1.Volume) bool { return cd(v.Name) })
		vm.Spec.DataVolumeTemplates = slices.DeleteFunc(vm.Spec.DataVolumeTemplates, func(t kubevirtv1.DataVolumeTemplateSpec) bool { return t.Name == installName(id) })
		for i := range spec.Domain.Devices.Disks {
			spec.Domain.Devices.Disks[i].BootOrder = nil
		}
		err := s.write.Update(ctx, &vm)
		if err == nil {
			break
		}
		if !apierrors.IsConflict(err) || attempt == 3 {
			return s.kubeError("ejecting the install media", err)
		}
	}
	install := &cdiv1.DataVolume{ObjectMeta: metav1.ObjectMeta{Name: installName(id), Namespace: s.Namespace}}
	if err := s.write.Delete(ctx, install, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
		s.log.Warn("deleting an ejected ISO copy", "id", id, "err", err)
	}
	s.log.Info("install media ejected", "by", a.Username, "id", id)
	return nil
}

// Action is start, stop or restart.
func (s *Service) Action(ctx context.Context, a Actor, id, action string) error {
	_, vm, err := s.get(ctx, a, id)
	if err != nil {
		return err
	}
	if vm == nil {
		return refuse(http.StatusConflict, "The machine's VM is missing; delete the machine.")
	}
	return s.subresource(ctx, id, action)
}

// Delete deletes the machine's VM (its disk and cloud-init Secret go with it); the watch removes the
// row once the VM is gone. A machine whose VM is already missing loses its row here.
func (s *Service) Delete(ctx context.Context, a Actor, id string, withIdentity bool) error {
	row, vm, err := s.get(ctx, a, id)
	if err != nil {
		return err
	}
	// A machine whose VM the cache doesn't have loses its row only when the API server confirms the
	// VM is missing and the row is older than a create can take: a create still under way would
	// otherwise go on to make a VM nobody owns.
	if vm == nil {
		var live kubevirtv1.VirtualMachine
		err := s.write.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: id}, &live)
		switch {
		case err == nil:
			vm = &live
		case !apierrors.IsNotFound(err):
			return s.kubeError("reading the machine", err)
		case time.Since(row.CreatedAt) < missingGrace:
			return refuse(http.StatusConflict, "The machine is still being created; try again in a moment.")
		default:
			if withIdentity {
				s.store.MarkDeleteIdentity(ctx, id)
			}
			gone, err := s.store.DeleteMachine(ctx, id)
			if err != nil {
				return s.storeError("deleting the machine", err)
			}
			if gone != "" {
				s.deleteIdentityAccount(ctx, gone)
			}
			s.hub.publish(change{owner: row.Owner, id: id})
		}
	}
	if vm != nil {
		// The identity goes with the row, which the VM watch removes once the VM is gone.
		if withIdentity {
			if err := s.store.MarkDeleteIdentity(ctx, id); err != nil && !errors.Is(err, store.ErrNotFound) {
				return s.storeError("marking the identity to go", err)
			}
		}
		if err := s.write.Delete(ctx, vm, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !apierrors.IsNotFound(err) {
			return s.kubeError("deleting the machine", err)
		}
	}
	s.log.Info("machine deleted", "by", a.Username, "owner", row.Owner, "id", id)
	return nil
}

// Running says whether the machine has a running instance (its consoles exist).
func (s *Service) Running(ctx context.Context, a Actor, id string) (bool, error) {
	_, vm, err := s.get(ctx, a, id)
	if err != nil || vm == nil {
		return false, err
	}
	return vm.Status.PrintableStatus == kubevirtv1.VirtualMachineStatusRunning || vm.Status.PrintableStatus == kubevirtv1.VirtualMachineStatusPaused, nil
}

func (s *Service) storeError(what string, err error) error {
	s.log.Warn("store", "what", what, "err", err)
	return refuse(http.StatusBadGateway, "The database refused %s.", what)
}

func (s *Service) subresource(ctx context.Context, id, action string) error {
	url := APIServer(s.Config) + fmt.Sprintf("/apis/subresources.kubevirt.io/v1/namespaces/%s/virtualmachines/%s/%s", s.Namespace, id, action)
	req, err := http.NewRequestWithContext(ctx, http.MethodPut, url, bytes.NewReader([]byte("{}")))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return s.kubeError(action, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 == 2 {
		return nil
	}
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	var status metav1.Status
	message := strings.TrimSpace(string(body))
	if json.Unmarshal(body, &status) == nil && status.Message != "" {
		message = status.Message
	}
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusBadRequest {
		// KubeVirt refuses e.g. stopping a stopped VM; say what it said.
		return refuse(http.StatusConflict, "KubeVirt refused to %s the machine: %s", action, message)
	}
	s.log.Warn("subresource", "action", action, "id", id, "status", resp.StatusCode, "message", message)
	return refuse(http.StatusBadGateway, "The cluster couldn't %s the machine.", action)
}

func (s *Service) kubeError(what string, err error) error {
	s.log.Warn("kubernetes", "what", what, "err", err)
	return refuse(http.StatusBadGateway, "The cluster refused %s.", what)
}

// APIServer is the API server's base URL, with a scheme.
func APIServer(cfg *rest.Config) string {
	host := strings.TrimRight(cfg.Host, "/")
	if !strings.Contains(host, "://") {
		host = "https://" + host
	}
	return host
}

// Account is the actor's account settings for new machines.
type Account struct {
	Username string `json:"username"`
	UID      *int   `json:"uid"`
}

// GetAccount is the actor's account.
func (s *Service) GetAccount(ctx context.Context, a Actor) (Account, error) {
	m, err := s.store.Member(ctx, a.Owner)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return Account{}, s.storeError("reading your account", err)
	}
	return Account{Username: a.Username, UID: m.UID}, nil
}

// SetUID sets the uid of the actor's account on machines made from now on (nil: the distro picks).
func (s *Service) SetUID(ctx context.Context, a Actor, uid *int) error {
	if uid != nil && (*uid < 1000 || *uid > 60000) {
		return refuse(http.StatusBadRequest, "The UID must be 1000 to 60000.")
	}
	if err := s.store.EnsureMember(ctx, a.Owner, a.Username); err != nil {
		return s.storeError("recording you", err)
	}
	if err := s.store.SetUID(ctx, a.Owner, uid); err != nil {
		return s.storeError("saving your UID", err)
	}
	return nil
}

// MemberInfo is a member as admins see them.
type MemberInfo struct {
	Owner      string      `json:"owner"`
	Username   string      `json:"username"`
	Usage      store.Usage `json:"usage"`
	Quota      store.Quota `json:"quota"`
	SignedInAt time.Time   `json:"signedInAt"`
}

// Members is every member with usage and quota (admins).
func (s *Service) Members(ctx context.Context, a Actor) ([]MemberInfo, error) {
	if !a.Admin {
		return nil, refuse(http.StatusForbidden, "Only admins can see the members.")
	}
	members, err := s.store.Members(ctx)
	if err != nil {
		return nil, s.storeError("reading the members", err)
	}
	out := make([]MemberInfo, 0, len(members))
	for _, m := range members {
		u, err := s.store.Usage(ctx, m.Owner)
		if err != nil {
			return nil, s.storeError("reading usage", err)
		}
		out = append(out, MemberInfo{Owner: m.Owner, Username: m.Username, Usage: u, Quota: m.Quota, SignedInAt: m.SignedInAt.UTC()})
	}
	return out, nil
}

// SetQuota sets a member's quota (nil fields: the default; admins).
func (s *Service) SetQuota(ctx context.Context, a Actor, owner string, q store.Quota) error {
	if !a.Admin {
		return refuse(http.StatusForbidden, "Only admins can change quotas.")
	}
	for _, v := range []*int{q.VMs, q.CPU, q.MemoryMiB, q.DiskGiB} {
		if v != nil && *v < 0 {
			return refuse(http.StatusBadRequest, "Quotas can't be negative.")
		}
	}
	err := s.store.SetQuota(ctx, owner, q)
	if errors.Is(err, store.ErrNotFound) {
		return refuse(http.StatusNotFound, "No such member.")
	}
	if err != nil {
		return s.storeError("saving the quota", err)
	}
	s.log.Info("quota set", "by", a.Username, "owner", owner)
	return nil
}
