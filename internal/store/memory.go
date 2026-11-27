package store

import (
	"context"
	"fmt"
	"sort"
	"sync"
	"time"
)

// Memory is a Store in memory, for tests. It keeps the same rules as Postgres (owner rows must
// exist, keys are unique, limits are checked atomically).
type Memory struct {
	mu       sync.Mutex
	members  map[string]string
	signedIn map[string]time.Time
	uids     map[string]*int
	quotas   map[string]Quota
	machines map[string]Machine
	keys     map[string]Key
	media    map[string]Medium
	idents   map[string]Identity
	retired  map[string]bool // retired identity accounts
	clock    time.Time
}

// NewMemory is an empty Memory store.
func NewMemory() *Memory {
	return &Memory{members: map[string]string{}, signedIn: map[string]time.Time{}, uids: map[string]*int{}, quotas: map[string]Quota{}, machines: map[string]Machine{}, keys: map[string]Key{}, media: map[string]Medium{}, idents: map[string]Identity{}, retired: map[string]bool{}, clock: time.Unix(1_800_000_000, 0)}
}

func (m *Memory) now() time.Time { m.clock = m.clock.Add(time.Second); return m.clock }

func (m *Memory) UpsertMember(_ context.Context, owner, username string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.members[owner] = username
	m.signedIn[owner] = time.Now()
	return nil
}

func (m *Memory) EnsureMember(_ context.Context, owner, username string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[owner]; !ok {
		m.members[owner] = username
		m.signedIn[owner] = time.Now()
	}
	return nil
}

func (m *Memory) SignedInAt(_ context.Context, owner string) (time.Time, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	t, ok := m.signedIn[owner]
	if !ok {
		return t, ErrNotFound
	}
	return t, nil
}

// SetSignedIn is for tests: pretend owner last signed in at t.
func (m *Memory) SetSignedIn(owner string, t time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.signedIn[owner] = t
}

func (m *Memory) usage(owner string) Usage {
	var u Usage
	for _, x := range m.machines {
		if x.Owner == owner {
			u.VMs++
			u.CPU += x.CPU
			u.MemoryMiB += x.MemoryMiB
			u.DiskGiB += x.DiskGiB
		}
	}
	return u
}

func (m *Memory) ResizeMachine(_ context.Context, id, size string, cpu, memoryMiB int, check func(Usage, Quota) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, ok := m.machines[id]
	if !ok {
		return ErrNotFound
	}
	u := m.usage(x.Owner)
	u.VMs, u.CPU, u.MemoryMiB, u.DiskGiB = u.VMs-1, u.CPU-x.CPU, u.MemoryMiB-x.MemoryMiB, u.DiskGiB-x.DiskGiB
	if err := check(u, m.quotas[x.Owner]); err != nil {
		return err
	}
	x.Size, x.CPU, x.MemoryMiB = size, cpu, memoryMiB
	m.machines[id] = x
	return nil
}

func (m *Memory) CreateMachine(_ context.Context, x Machine, check func(Usage, Quota) error) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[x.Owner]; !ok {
		return fmt.Errorf("owner %q isn't a member (foreign key)", x.Owner)
	}
	if err := check(m.usage(x.Owner), m.quotas[x.Owner]); err != nil {
		return err
	}
	if _, ok := m.machines[x.ID]; ok {
		return ErrExists
	}
	if x.Identity != "" && m.idents[x.Identity].Owner != x.Owner {
		return ErrNoIdentity
	}
	for _, o := range m.machines {
		if o.Name == x.Name {
			return ErrExists
		}
	}
	x.CreatedAt = m.now()
	x.OwnerName = m.members[x.Owner]
	x.IdentityName = m.idents[x.Identity].Name
	m.machines[x.ID] = x
	return nil
}

func (m *Memory) MachineID(_ context.Context, idOrName string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.machines {
		if x.ID == idOrName || x.Name == idOrName {
			return x.ID, nil
		}
	}
	return "", ErrNotFound
}

func (m *Memory) Machine(_ context.Context, id string) (Machine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, ok := m.machines[id]
	if !ok {
		return Machine{}, ErrNotFound
	}
	return x, nil
}

func (m *Memory) Machines(_ context.Context, owner string) ([]Machine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Machine
	for _, x := range m.machines {
		if x.Owner == owner {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) Usage(_ context.Context, owner string) (Usage, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.usage(owner), nil
}

func (m *Memory) DeleteMachine(_ context.Context, id string) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, ok := m.machines[id]
	if !ok {
		return "", nil
	}
	delete(m.machines, id)
	if !x.DeleteIdentity || x.Identity == "" || m.worn(x.Identity) {
		return "", nil
	}
	delete(m.idents, x.Identity)
	m.retired[x.Identity] = true
	return x.Identity, nil
}

func (m *Memory) worn(account string) bool {
	for _, o := range m.machines {
		if o.Identity == account {
			return true
		}
	}
	return false
}

func (m *Memory) SetMachineIdentity(_ context.Context, id, account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, ok := m.machines[id]
	if !ok {
		return ErrNotFound
	}
	if account == "" || m.idents[account].Owner != x.Owner {
		return ErrNoIdentity
	}
	x.Identity, x.IdentityName, x.DeleteIdentity = account, m.idents[account].Name, false
	m.machines[id] = x
	return nil
}

func (m *Memory) MarkDeleteIdentity(_ context.Context, id string, with bool) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, ok := m.machines[id]
	if !ok {
		return ErrNotFound
	}
	x.DeleteIdentity = with
	m.machines[id] = x
	return nil
}

func (m *Memory) CreateIdentity(_ context.Context, i Identity, max int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[i.Owner]; !ok {
		return fmt.Errorf("owner %q isn't a member (foreign key)", i.Owner)
	}
	n := 0
	for _, o := range m.idents {
		if o.Owner == i.Owner {
			n++
			if o.Name == i.Name {
				return ErrExists
			}
		}
	}
	if n >= max {
		return &LimitError{Message: fmt.Sprintf("You have %d identities, the most there may be.", n)}
	}
	if _, ok := m.idents[i.Account]; ok || m.retired[i.Account] {
		return ErrExists
	}
	i.CreatedAt, i.OwnerName, i.UsedBy = m.now(), m.members[i.Owner], nil
	m.idents[i.Account] = i
	return nil
}

func (m *Memory) DeleteIdentity(_ context.Context, account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.idents[account]; !ok {
		return ErrNotFound
	}
	if m.worn(account) {
		return ErrInUse
	}
	delete(m.idents, account)
	m.retired[account] = true
	return nil
}

func (m *Memory) withUse(i Identity) Identity {
	i.UsedBy = []string{}
	for _, o := range m.machines {
		if o.Identity == i.Account {
			i.UsedBy = append(i.UsedBy, o.Name)
		}
	}
	sort.Strings(i.UsedBy)
	return i
}

func (m *Memory) Identity(_ context.Context, account string) (Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	i, ok := m.idents[account]
	if !ok {
		return Identity{}, ErrNotFound
	}
	return m.withUse(i), nil
}

func (m *Memory) Identities(_ context.Context, owner string) ([]Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Identity{}
	for _, i := range m.idents {
		if i.Owner == owner {
			out = append(out, m.withUse(i))
		}
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.Before(out[b].CreatedAt) })
	return out, nil
}

func (m *Memory) AllIdentities(_ context.Context) ([]Identity, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := []Identity{}
	for _, i := range m.idents {
		out = append(out, m.withUse(i))
	}
	sort.Slice(out, func(a, b int) bool { return out[a].CreatedAt.Before(out[b].CreatedAt) })
	return out, nil
}

func (m *Memory) AddKey(_ context.Context, k Key, max int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[k.Owner]; !ok {
		return fmt.Errorf("owner %q isn't a member (foreign key)", k.Owner)
	}
	n := 0
	for _, x := range m.keys {
		if x.Owner == k.Owner {
			n++
		}
	}
	if n >= max {
		return &LimitError{fmt.Sprintf("You already have %d keys.", max)}
	}
	if _, ok := m.keys[k.ID]; ok {
		return ErrExists
	}
	k.CreatedAt = m.now()
	m.keys[k.ID] = k
	return nil
}

func (m *Memory) Key(_ context.Context, id string) (Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	k, ok := m.keys[id]
	if !ok {
		return Key{}, ErrNotFound
	}
	return k, nil
}

func (m *Memory) Keys(_ context.Context, owner string) ([]Key, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Key
	for _, k := range m.keys {
		if k.Owner == owner {
			out = append(out, k)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) DeleteKey(_ context.Context, owner, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if k, ok := m.keys[id]; !ok || k.Owner != owner {
		return ErrNotFound
	}
	delete(m.keys, id)
	return nil
}

func (m *Memory) AddMedium(_ context.Context, x Medium) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[x.Owner]; !ok {
		return fmt.Errorf("owner %q isn't a member (foreign key)", x.Owner)
	}
	if _, ok := m.media[x.ID]; ok {
		return ErrExists
	}
	x.CreatedAt = m.now()
	m.media[x.ID] = x
	return nil
}

func (m *Memory) Medium(_ context.Context, id string) (Medium, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, ok := m.media[id]
	if !ok {
		return Medium{}, ErrNotFound
	}
	x.OwnerName = m.members[x.Owner]
	return x, nil
}

func (m *Memory) Media(_ context.Context, owner string) ([]Medium, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Medium
	for _, x := range m.media {
		if x.Owner == owner {
			out = append(out, x)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) SetMediumProfile(_ context.Context, id, profile string, virtualBytes int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, ok := m.media[id]
	if ok {
		x.Profile = profile
		x.VirtualBytes = max(x.VirtualBytes, virtualBytes)
		m.media[id] = x
	}
	return nil
}

func (m *Memory) DeleteMedium(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.media, id)
	return nil
}

func (m *Memory) MachineByName(_ context.Context, name string) (Machine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, x := range m.machines {
		if x.Name == name {
			return x, nil
		}
	}
	return Machine{}, ErrNotFound
}

func (m *Memory) AllMachines(_ context.Context) ([]Machine, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Machine
	for _, x := range m.machines {
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) member(owner string) Member {
	return Member{Owner: owner, Username: m.members[owner], UID: m.uids[owner], Quota: m.quotas[owner], SignedInAt: m.signedIn[owner]}
}

func (m *Memory) Member(_ context.Context, owner string) (Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[owner]; !ok {
		return Member{}, ErrNotFound
	}
	return m.member(owner), nil
}

func (m *Memory) Members(_ context.Context) ([]Member, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Member
	for owner := range m.members {
		out = append(out, m.member(owner))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Username < out[j].Username })
	return out, nil
}

func (m *Memory) SetUID(_ context.Context, owner string, uid *int) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.uids[owner] = uid
	return nil
}

func (m *Memory) SetQuota(_ context.Context, owner string, q Quota) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.members[owner]; !ok {
		return ErrNotFound
	}
	m.quotas[owner] = q
	return nil
}

func (m *Memory) PublicMedia(ctx context.Context) ([]Medium, error) {
	all, _ := m.AllMedia(ctx)
	var out []Medium
	for _, x := range all {
		if x.Public {
			out = append(out, x)
		}
	}
	return out, nil
}

func (m *Memory) AllMedia(_ context.Context) ([]Medium, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Medium
	for _, x := range m.media {
		x.OwnerName = m.members[x.Owner]
		out = append(out, x)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, nil
}

func (m *Memory) UpdateMedium(_ context.Context, id string, u MediumUpdate) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	x, ok := m.media[id]
	if !ok {
		return ErrNotFound
	}
	if u.Name != nil {
		x.Name = *u.Name
	}
	if u.Public != nil {
		x.Public = *u.Public
	}
	if u.CloudConfig != nil {
		x.CloudConfig = u.CloudConfig
	}
	m.media[id] = x
	return nil
}

func (m *Memory) MachinesFrom(ctx context.Context, media string) ([]Machine, error) {
	all, _ := m.AllMachines(ctx)
	var out []Machine
	for _, x := range all {
		if x.Media == media {
			out = append(out, x)
		}
	}
	return out, nil
}
