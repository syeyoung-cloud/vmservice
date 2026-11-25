package machines

import (
	"context"
	"crypto/rand"
	"errors"
	"net/http"
	"regexp"
	"time"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubevirtv1 "kubevirt.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/cyoung06/vmservice/internal/store"
)

// An identity is what machines are to other services: a ServiceAccount of the namespace,
// identity-<name>-<random>, whose tokens (from the metadata service, package metadata) say
// system:serviceaccount:<namespace>:<account>. A member owns it; machines wear it, several may wear
// one, and every machine wears exactly one: a new machine its own, named after it, unless its
// creator picks one of theirs.

// LabelIdentity marks an identity's ServiceAccount ("true"; the account, up to 81 characters, is
// longer than a label value may be).
const LabelIdentity = "vmservice/identity"

// MaxIdentities is how many identities a member may have.
const MaxIdentities = 100

// identityName is a member's name for an identity: a hostname label, as machine names are.
var identityName = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)

// Identity is the API's view of an identity.
type Identity struct {
	Name      string    `json:"name"`
	Account   string    `json:"account"` // the ServiceAccount; the path names it
	Subject   string    `json:"subject"`
	Issuer    string    `json:"issuer"`
	Owner     string    `json:"owner"` // the owner's username
	Mine      bool      `json:"mine"`
	UsedBy    []string  `json:"usedBy"` // names of the machines that wear it
	CreatedAt time.Time `json:"createdAt"`
}

// Issuer is the cluster's service account issuer: what identities' tokens say they're from.
var Issuer = "https://kubernetes.syeyoung.dev"

func (s *Service) subject(account string) string {
	return "system:serviceaccount:" + s.Namespace + ":" + account
}

func (s *Service) toIdentity(i store.Identity, viewer string) Identity {
	used := i.UsedBy
	if used == nil {
		used = []string{}
	}
	return Identity{Name: i.Name, Account: i.Account, Subject: s.subject(i.Account), Issuer: Issuer,
		Owner: i.OwnerName, Mine: i.Owner == viewer, UsedBy: used, CreatedAt: i.CreatedAt}
}

// Identities is the actor's identities (an admin's: everyone's).
func (s *Service) Identities(ctx context.Context, a Actor) ([]Identity, error) {
	var list []store.Identity
	var err error
	if a.Admin {
		list, err = s.store.AllIdentities(ctx)
	} else {
		list, err = s.store.Identities(ctx, a.Owner)
	}
	if err != nil {
		return nil, s.storeError("reading identities", err)
	}
	out := make([]Identity, 0, len(list))
	for _, i := range list {
		out = append(out, s.toIdentity(i, a.Owner))
	}
	return out, nil
}

// CreateIdentity makes an identity of the actor's: its row, then its ServiceAccount.
func (s *Service) CreateIdentity(ctx context.Context, a Actor, name string) (Identity, error) {
	i, err := s.newIdentity(ctx, a.Owner, name)
	if err != nil {
		return Identity{}, err
	}
	i.OwnerName = a.Username
	return s.toIdentity(i, a.Owner), nil
}

func (s *Service) newIdentity(ctx context.Context, owner, name string) (store.Identity, error) {
	if !identityName.MatchString(name) {
		return store.Identity{}, refuse(http.StatusBadRequest, "An identity's name is lowercase letters, digits and hyphens, at most 63, starting and ending with a letter or digit.")
	}
	var i store.Identity
	for attempt := 0; ; attempt++ {
		i = store.Identity{Account: "identity-" + name + "-" + randomSuffix(), Owner: owner, Name: name}
		err := s.store.CreateIdentity(ctx, i, MaxIdentities)
		var le *store.LimitError
		switch {
		case err == nil:
		case errors.As(err, &le):
			return store.Identity{}, refuse(http.StatusForbidden, "%s", le.Message)
		case errors.Is(err, store.ErrExists):
			if s.ownIdentity(ctx, owner, name) != "" {
				return store.Identity{}, refuse(http.StatusConflict, "You already have an identity named %s.", name)
			}
			if attempt < 3 {
				continue // the account was taken
			}
			return store.Identity{}, s.storeError("saving the identity", err)
		default:
			return store.Identity{}, s.storeError("saving the identity", err)
		}
		break
	}
	if err := s.write.Create(ctx, s.identityAccount(i)); err != nil && !apierrors.IsAlreadyExists(err) {
		s.store.DeleteIdentity(ctx, i.Account)
		return store.Identity{}, s.kubeError("creating the identity", err)
	}
	i.CreatedAt = time.Now()
	s.log.Info("identity created", "owner", owner, "account", i.Account)
	return i, nil
}

// ownIdentity is the account of owner's identity by that name, or "".
func (s *Service) ownIdentity(ctx context.Context, owner, name string) string {
	list, err := s.store.Identities(ctx, owner)
	if err != nil {
		return ""
	}
	for _, i := range list {
		if i.Name == name {
			return i.Account
		}
	}
	return ""
}

// DeleteIdentity deletes an identity no machine wears: its row (which the database refuses while
// one does), then its ServiceAccount.
func (s *Service) DeleteIdentity(ctx context.Context, a Actor, account string) error {
	i, err := s.store.Identity(ctx, account)
	if errors.Is(err, store.ErrNotFound) || (err == nil && !a.may(i.Owner)) {
		return ErrNotFound
	}
	if err != nil {
		return s.storeError("reading the identity", err)
	}
	switch err := s.store.DeleteIdentity(ctx, account); {
	case errors.Is(err, store.ErrInUse):
		return refuse(http.StatusConflict, "Machines wear this identity: give them another first.")
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case err != nil:
		return s.storeError("deleting the identity", err)
	}
	s.deleteIdentityAccount(ctx, account)
	s.log.Info("identity deleted", "by", a.Username, "account", account)
	return nil
}

func (s *Service) deleteIdentityAccount(ctx context.Context, account string) {
	sa := &corev1.ServiceAccount{ObjectMeta: metav1.ObjectMeta{Namespace: s.Namespace, Name: account}}
	if err := s.write.Delete(ctx, sa); err != nil && !apierrors.IsNotFound(err) {
		s.log.Warn("deleting an identity's service account (the next start removes it)", "account", account, "err", err)
	}
}

// SetIdentity has a machine wear another identity of its owner's, from its next start.
func (s *Service) SetIdentity(ctx context.Context, a Actor, id, name string) error {
	row, _, err := s.get(ctx, a, id)
	if err != nil {
		return err
	}
	account := s.ownIdentity(ctx, row.Owner, name)
	if account == "" {
		return refuse(http.StatusBadRequest, "The machine's owner has no identity named %q.", name)
	}
	switch err := s.store.SetMachineIdentity(ctx, id, account); {
	case errors.Is(err, store.ErrNotFound):
		return ErrNotFound
	case errors.Is(err, store.ErrNoIdentity):
		return refuse(http.StatusBadRequest, "The machine's owner has no identity named %q.", name)
	case err != nil:
		return s.storeError("saving the identity", err)
	}
	if err := s.wearIdentity(ctx, id, account); err != nil {
		return err
	}
	s.log.Info("machine wears another identity (at its next start)", "by", a.Username, "id", id, "account", account)
	return nil
}

// wearIdentity sets the VM's identity volume to account (the row says so already).
func (s *Service) wearIdentity(ctx context.Context, id, account string) error {
	for attempt := 0; ; attempt++ {
		var vm kubevirtv1.VirtualMachine // fresh from the API server: the update is conditional on it
		if err := s.write.Get(ctx, client.ObjectKey{Namespace: s.Namespace, Name: id}, &vm); err != nil {
			return s.kubeError("reading the machine", err)
		}
		vols := &vm.Spec.Template.Spec.Volumes
		found := false
		for k := range *vols {
			if (*vols)[k].Name == volumeIdentity {
				(*vols)[k] = identityVolume(account)
				found = true
			}
		}
		if !found {
			*vols = append(*vols, identityVolume(account))
		}
		err := s.write.Update(ctx, &vm)
		if err == nil {
			return nil
		}
		if !apierrors.IsConflict(err) || attempt == 3 {
			return s.kubeError("giving the machine its identity", err)
		}
	}
}

func (s *Service) identityAccount(i store.Identity) *corev1.ServiceAccount {
	no := false
	return &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{
			Name: i.Account, Namespace: s.Namespace,
			Labels: map[string]string{LabelOwner: i.Owner, LabelIdentity: "true"},
		},
		AutomountServiceAccountToken: &no, // KubeVirt mounts it in the launcher pod itself
	}
}

// ensureIdentities, at every start: each identity has its ServiceAccount and no ServiceAccount
// outlives its identity; each machine from before identities gets one of its own, named after it,
// in effect at its next start. All of it is idempotent.
func (s *Service) ensureIdentities(ctx context.Context) {
	all, err := s.store.AllIdentities(ctx)
	if err != nil {
		s.log.Warn("listing identities", "err", err)
		return
	}
	known := map[string]bool{}
	for _, i := range all {
		known[i.Account] = true
		if err := s.write.Create(ctx, s.identityAccount(i)); err != nil && !apierrors.IsAlreadyExists(err) {
			s.log.Warn("creating an identity's service account", "account", i.Account, "err", err)
		}
	}
	var sas corev1.ServiceAccountList
	if err := s.write.List(ctx, &sas, client.InNamespace(s.Namespace), client.HasLabels{LabelIdentity}); err != nil {
		s.log.Warn("listing identities' service accounts", "err", err)
	}
	for _, sa := range sas.Items {
		if !known[sa.Name] {
			s.deleteIdentityAccount(ctx, sa.Name)
		}
	}
	rows, err := s.store.AllMachines(ctx)
	if err != nil {
		s.log.Warn("listing machines for their identities", "err", err)
		return
	}
	for _, row := range rows {
		if row.Identity != "" {
			// The row is the record; a VM left wearing another (a change that failed half way) is
			// put right, in effect at its next start.
			if vm := s.vm(ctx, row.ID); vm != nil && identityOfVM(vm) != row.Identity {
				if err := s.wearIdentity(ctx, row.ID, row.Identity); err != nil {
					s.log.Warn("putting a machine's identity right", "id", row.ID, "err", err)
				} else {
					s.log.Info("machine's identity put right (at its next start)", "id", row.ID, "account", row.Identity)
				}
			}
			continue
		}
		var i store.Identity
		var err error
		for _, name := range []string{row.Name, row.ID} {
			if i, err = s.newIdentity(ctx, row.Owner, name); err == nil {
				break
			}
		}
		if err != nil {
			s.log.Warn("giving a machine its identity", "id", row.ID, "err", err)
			continue
		}
		if err := s.store.SetMachineIdentity(ctx, row.ID, i.Account); err != nil {
			s.log.Warn("giving a machine its identity", "id", row.ID, "err", err)
			continue
		}
		if err := s.wearIdentity(ctx, row.ID, i.Account); err != nil {
			s.log.Warn("giving a machine its identity", "id", row.ID, "err", err)
			continue
		}
		s.log.Info("machine given its own identity (at its next start)", "id", row.ID, "account", i.Account)
	}
}

// identityOfVM is the account the VM's identity volume names, or "".
func identityOfVM(vm *kubevirtv1.VirtualMachine) string {
	for _, v := range vm.Spec.Template.Spec.Volumes {
		if v.Name == volumeIdentity && v.ServiceAccount != nil {
			return v.ServiceAccount.ServiceAccountName
		}
	}
	return ""
}

// randomSuffix is 8 characters from the ID alphabet (no 0/o, 1/l).
func randomSuffix() string {
	const alphabet = "abcdefghijkmnpqrstuvwxyz23456789"
	b := make([]byte, 8)
	rand.Read(b)
	for k := range b {
		b[k] = alphabet[int(b[k])%len(alphabet)]
	}
	return string(b)
}
