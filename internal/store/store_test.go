package store

import (
	"context"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
)

// Every Store passes the same tests: Memory always, Postgres when VMSERVICE_TEST_DATABASE_URL names
// a scratch database (its tables are dropped first).
func stores(t *testing.T) map[string]Store {
	out := map[string]Store{"memory": NewMemory()}
	url := os.Getenv("VMSERVICE_TEST_DATABASE_URL")
	if url == "" {
		return out
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `DROP TABLE IF EXISTS retired_identities, media, ssh_keys, machines, identities, members, schema_migrations`); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	p, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.Close)
	// Opening twice applies nothing twice.
	again, err := Open(ctx, url)
	if err != nil {
		t.Fatalf("second migrate: %v", err)
	}
	again.Close()
	out["postgres"] = p
	return out
}

func limit2(u Usage, _ Quota) error {
	if u.VMs >= 2 {
		return &LimitError{"over"}
	}
	return nil
}

func TestMachines(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s.UpsertMember(ctx, "alice", "alice")
			s.UpsertMember(ctx, "bob", "bob")
			m := Machine{ID: "vm-aaaaaa", Owner: "alice", Name: "box", Image: "ubuntu", Size: "small", CPU: 2, MemoryMiB: 2048, DiskGiB: 20}
			if err := s.CreateMachine(ctx, m, limit2); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateMachine(ctx, m, limit2); !errors.Is(err, ErrExists) {
				t.Errorf("same id again = %v, want ErrExists", err)
			}
			got, err := s.Machine(ctx, "vm-aaaaaa")
			if err != nil || got.Owner != "alice" || got.CPU != 2 || got.CreatedAt.IsZero() {
				t.Errorf("machine = %+v, %v", got, err)
			}
			if _, err := s.Machine(ctx, "vm-zzzzzz"); !errors.Is(err, ErrNotFound) {
				t.Errorf("missing = %v", err)
			}
			if u, _ := s.Usage(ctx, "alice"); u != (Usage{VMs: 1, CPU: 2, MemoryMiB: 2048, DiskGiB: 20}) {
				t.Errorf("usage = %+v", u)
			}
			if byName, err := s.MachineByName(ctx, "box"); err != nil || byName.ID != "vm-aaaaaa" || byName.OwnerName != "alice" {
				t.Errorf("by name = %+v, %v", byName, err)
			}
			if err := s.CreateMachine(ctx, Machine{ID: "vm-zzzzzz", Owner: "bob", Name: "box"}, limit2); !errors.Is(err, ErrExists) {
				t.Errorf("a second machine named box = %v, want ErrExists (names are unique)", err)
			}
			if list, _ := s.Machines(ctx, "bob"); len(list) != 0 {
				t.Errorf("bob sees %v", list)
			}
			if err := s.CreateMachine(ctx, Machine{ID: "vm-bbbbbb", Owner: "nobody", Name: "nobodys"}, limit2); err == nil {
				t.Error("a machine for a non-member was created")
			}
			s.DeleteMachine(ctx, "vm-aaaaaa")
			if _, err := s.Machine(ctx, "vm-aaaaaa"); !errors.Is(err, ErrNotFound) {
				t.Errorf("after delete = %v", err)
			}

		})
	}
}

// Resize checks the other machines plus this one at its new size, and two parallel resizes that
// would each fit alone can't both win.
func TestResizeMachine(t *testing.T) {
	cpu4 := func(u Usage, _ Quota) error { // checks run with the others; the caller adds the new size
		if u.CPU+3 > 4 {
			return &LimitError{"over"}
		}
		return nil
	}
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s.UpsertMember(ctx, "dave", "dave")
			for _, id := range []string{"vm-d00001", "vm-d00002"} {
				if err := s.CreateMachine(ctx, Machine{ID: id, Owner: "dave", Name: id[3:], Size: "small", CPU: 1, MemoryMiB: 1024, DiskGiB: 10}, limit2); err != nil {
					t.Fatal(err)
				}
			}
			if err := s.ResizeMachine(ctx, "vm-d00001", "large", 3, 4096, cpu4); err != nil {
				t.Fatalf("resize to 3 of 4 vCPUs = %v", err)
			}
			if got, _ := s.Machine(ctx, "vm-d00001"); got.Size != "large" || got.CPU != 3 || got.MemoryMiB != 4096 || got.DiskGiB != 10 {
				t.Errorf("after resize = %+v", got)
			}
			if u, _ := s.Usage(ctx, "dave"); u.CPU != 4 || u.MemoryMiB != 5120 {
				t.Errorf("usage = %+v", u)
			}
			if err := s.ResizeMachine(ctx, "vm-d00001", "large", 3, 4096, cpu4); err != nil {
				t.Errorf("resizing to the same size again = %v (it counts the others, not itself)", err)
			}
			var le *LimitError
			if err := s.ResizeMachine(ctx, "vm-d00002", "large", 3, 4096, cpu4); !errors.As(err, &le) {
				t.Errorf("second machine to 3 vCPUs = %v, want a LimitError", err)
			}
			if err := s.ResizeMachine(ctx, "vm-nope00", "large", 3, 4096, cpu4); !errors.Is(err, ErrNotFound) {
				t.Errorf("missing machine = %v, want ErrNotFound", err)
			}
			// back to 1 each, then both grow at once: only one fits
			s.ResizeMachine(ctx, "vm-d00001", "small", 1, 1024, cpu4)
			var wg sync.WaitGroup
			for _, id := range []string{"vm-d00001", "vm-d00002"} {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.ResizeMachine(ctx, id, "large", 3, 4096, cpu4)
				}()
			}
			wg.Wait()
			if u, _ := s.Usage(ctx, "dave"); u.CPU != 4 {
				t.Errorf("two parallel resizes left %d vCPUs in use; the limit is 4", u.CPU)
			}
		})
	}
}

// Identities: owned, worn by machines, not deletable while worn, never made again once deleted.
func TestIdentities(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s.UpsertMember(ctx, "erin", "erin")
			s.UpsertMember(ctx, "fred", "fred")
			a := Identity{Account: "identity-build-aaaaaaaa", Owner: "erin", Name: "build"}
			if err := s.CreateIdentity(ctx, a, 2); err != nil {
				t.Fatal(err)
			}
			if err := s.CreateIdentity(ctx, Identity{Account: "identity-build-bbbbbbbb", Owner: "erin", Name: "build"}, 2); !errors.Is(err, ErrExists) {
				t.Errorf("a second identity named build = %v, want ErrExists", err)
			}
			if err := s.CreateIdentity(ctx, Identity{Account: "identity-build-cccccccc", Owner: "fred", Name: "build"}, 2); err != nil {
				t.Errorf("another member's identity of the same name = %v", err)
			}
			s.CreateIdentity(ctx, Identity{Account: "identity-two-dddddddd", Owner: "erin", Name: "two"}, 2)
			var le *LimitError
			if err := s.CreateIdentity(ctx, Identity{Account: "identity-three-eeeeeeee", Owner: "erin", Name: "three"}, 2); !errors.As(err, &le) {
				t.Errorf("over the most = %v, want a LimitError", err)
			}
			if err := s.CreateMachine(ctx, Machine{ID: "vm-e00001", Owner: "erin", Name: "e1", Identity: "identity-build-cccccccc"}, limit2); !errors.Is(err, ErrNoIdentity) {
				t.Errorf("wearing fred's identity = %v, want ErrNoIdentity", err)
			}
			for _, id := range []string{"vm-e00001", "vm-e00002"} {
				if err := s.CreateMachine(ctx, Machine{ID: id, Owner: "erin", Name: id[3:], Identity: a.Account}, limit2); err != nil {
					t.Fatal(err)
				}
			}
			if got, _ := s.Identity(ctx, a.Account); len(got.UsedBy) != 2 || got.UsedBy[0] != "e00001" || got.OwnerName != "erin" {
				t.Errorf("identity = %+v", got)
			}
			if err := s.DeleteIdentity(ctx, a.Account); !errors.Is(err, ErrInUse) {
				t.Errorf("deleting a worn identity = %v, want ErrInUse", err)
			}
			// marked to go with its machine, but the other machine still wears it
			s.MarkDeleteIdentity(ctx, "vm-e00001", true)
			if gone, err := s.DeleteMachine(ctx, "vm-e00001"); err != nil || gone != "" {
				t.Errorf("deleting one wearer = %q, %v; want the identity kept", gone, err)
			}
			if err := s.SetMachineIdentity(ctx, "vm-e00002", "identity-two-dddddddd"); err != nil {
				t.Errorf("wearing another of hers = %v", err)
			}
			if err := s.SetMachineIdentity(ctx, "vm-e00002", "identity-build-cccccccc"); !errors.Is(err, ErrNoIdentity) {
				t.Errorf("wearing fred's = %v, want ErrNoIdentity", err)
			}
			if err := s.DeleteIdentity(ctx, a.Account); err != nil {
				t.Errorf("deleting it once nobody wears it = %v", err)
			}
			if err := s.CreateIdentity(ctx, a, 5); !errors.Is(err, ErrExists) {
				t.Errorf("making a deleted account again = %v, want ErrExists (never reused)", err)
			}
			s.MarkDeleteIdentity(ctx, "vm-e00002", true)
			if gone, err := s.DeleteMachine(ctx, "vm-e00002"); err != nil || gone != "identity-two-dddddddd" {
				t.Errorf("deleting its last wearer = %q, %v; want the identity gone with it", gone, err)
			}
			if list, _ := s.Identities(ctx, "erin"); len(list) != 0 {
				t.Errorf("erin's identities = %+v", list)
			}
			if all, _ := s.AllIdentities(ctx); len(all) != 1 || all[0].Owner != "fred" {
				t.Errorf("all = %+v", all)
			}
		})
	}
}

// Twenty parallel creates against a limit of two: exactly two rows.
func TestLimitHoldsUnderConcurrency(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s.UpsertMember(ctx, "carol", "carol")
			var wg sync.WaitGroup
			for i := 0; i < 20; i++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					s.CreateMachine(ctx, Machine{ID: fmt.Sprintf("vm-c%05d", i), Owner: "carol", Name: fmt.Sprintf("c%d", i), CPU: 1, MemoryMiB: 1, DiskGiB: 1}, limit2)
				}()
			}
			wg.Wait()
			if u, _ := s.Usage(ctx, "carol"); u.VMs != 2 {
				t.Errorf("20 parallel creates made %d machines; the limit is 2", u.VMs)
			}
		})
	}
}

func TestKeys(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s.UpsertMember(ctx, "alice", "alice")
			s.UpsertMember(ctx, "bob", "bob")
			k := Key{ID: "k1", Owner: "alice", Line: "ssh-ed25519 AAAA", Label: "laptop"}
			if err := s.AddKey(ctx, k, 2); err != nil {
				t.Fatal(err)
			}
			if err := s.AddKey(ctx, Key{ID: "k1", Owner: "bob", Line: "ssh-ed25519 AAAA"}, 2); !errors.Is(err, ErrExists) {
				t.Errorf("bob adding alice's key = %v, want ErrExists", err)
			}
			s.AddKey(ctx, Key{ID: "k2", Owner: "alice", Line: "x"}, 2)
			var le *LimitError
			if err := s.AddKey(ctx, Key{ID: "k3", Owner: "alice", Line: "y"}, 2); !errors.As(err, &le) {
				t.Errorf("third key = %v, want LimitError", err)
			}
			if got, err := s.Key(ctx, "k1"); err != nil || got.Owner != "alice" || got.Label != "laptop" {
				t.Errorf("key = %+v, %v", got, err)
			}
			if err := s.DeleteKey(ctx, "bob", "k1"); !errors.Is(err, ErrNotFound) {
				t.Errorf("bob deleting alice's key = %v", err)
			}
			if err := s.DeleteKey(ctx, "alice", "k1"); err != nil {
				t.Errorf("alice delete = %v", err)
			}
			if keys, _ := s.Keys(ctx, "alice"); len(keys) != 1 || keys[0].ID != "k2" {
				t.Errorf("keys = %+v", keys)
			}
		})
	}
}

// The bastion's role reads members, machines and keys, and can change nothing.
func TestReadOnlyRole(t *testing.T) {
	url := os.Getenv("VMSERVICE_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("needs VMSERVICE_TEST_DATABASE_URL")
	}
	ctx := context.Background()
	stores(t) // fresh schema
	owner, err := Open(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	owner.pool.Exec(ctx, `DROP ROLE IF EXISTS bastion_test`)
	if _, err := owner.pool.Exec(ctx, `CREATE ROLE bastion_test LOGIN PASSWORD 'pw'`); err != nil {
		t.Fatal(err)
	}
	if err := owner.GrantRead(ctx, "bastion_test"); err != nil {
		t.Fatal(err)
	}
	owner.UpsertMember(ctx, "alice", "alice")
	owner.AddKey(ctx, Key{ID: "k1", Owner: "alice", Line: "ssh-ed25519 AAAA"}, 20)
	u, _ := neturl.Parse(url)
	u.User = neturl.UserPassword("bastion_test", "pw")
	ro, err := OpenReadOnly(ctx, u.String())
	if err != nil {
		t.Fatal(err)
	}
	defer ro.Close()
	if k, err := ro.Key(ctx, "k1"); err != nil || k.Owner != "alice" {
		t.Errorf("read as bastion = %+v, %v", k, err)
	}
	if _, err := ro.SignedInAt(ctx, "alice"); err != nil {
		t.Errorf("sign-in time as bastion: %v", err)
	}
	if err := ro.AddKey(ctx, Key{ID: "k2", Owner: "alice", Line: "x"}, 20); err == nil {
		t.Error("the bastion's role could add a key")
	}
	if err := ro.DeleteKey(ctx, "alice", "k1"); err == nil {
		t.Error("the bastion's role could delete a key")
	}
}

func TestMedia(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s.UpsertMember(ctx, "alice", "alice")
			s.UpsertMember(ctx, "bob", "bob")
			m := Medium{ID: "md-aaaaaa", Owner: "alice", Name: "Win 11", Source: "upload", Kind: "iso", SizeBytes: 6 << 30, VirtualBytes: 6 << 30}
			if err := s.AddMedium(ctx, m); err != nil {
				t.Fatal(err)
			}
			if err := s.AddMedium(ctx, m); !errors.Is(err, ErrExists) {
				t.Errorf("again = %v", err)
			}
			s.SetMediumProfile(ctx, "md-aaaaaa", "windows", 1)
			got, err := s.Medium(ctx, "md-aaaaaa")
			if err != nil || got.Profile != "windows" || got.VirtualBytes != 6<<30 || got.Owner != "alice" {
				t.Errorf("medium = %+v, %v", got, err)
			}
			if list, _ := s.Media(ctx, "bob"); len(list) != 0 {
				t.Errorf("bob sees %v", list)
			}
			mm := Machine{ID: "vm-mmmmmm", Owner: "alice", Name: "w", Media: "md-aaaaaa", Profile: "windows"}
			if err := s.CreateMachine(ctx, mm, func(Usage, Quota) error { return nil }); err != nil {
				t.Fatal(err)
			}
			if got, _ := s.Machine(ctx, "vm-mmmmmm"); got.Media != "md-aaaaaa" || got.Profile != "windows" {
				t.Errorf("machine media = %+v", got)
			}
			s.DeleteMedium(ctx, "md-aaaaaa")
			if _, err := s.Medium(ctx, "md-aaaaaa"); !errors.Is(err, ErrNotFound) {
				t.Errorf("after delete = %v", err)
			}
		})
	}
}

func TestMembersQuotaPublic(t *testing.T) {
	for name, s := range stores(t) {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			s.UpsertMember(ctx, "alice", "alice")
			s.UpsertMember(ctx, "bob", "bob")
			three, uid := 3, 10001
			if err := s.SetQuota(ctx, "alice", Quota{VMs: &three}); err != nil {
				t.Fatal(err)
			}
			s.SetUID(ctx, "alice", &uid)
			m, err := s.Member(ctx, "alice")
			if err != nil || m.Quota.VMs == nil || *m.Quota.VMs != 3 || m.Quota.CPU != nil || m.UID == nil || *m.UID != 10001 {
				t.Errorf("member = %+v, %v", m, err)
			}
			var seen Quota
			s.CreateMachine(ctx, Machine{ID: "vm-qqqqqq", Owner: "alice", Name: "q"}, func(_ Usage, q Quota) error { seen = q; return nil })
			if seen.VMs == nil || *seen.VMs != 3 {
				t.Errorf("CreateMachine saw quota %+v", seen)
			}
			if err := s.SetQuota(ctx, "nobody", Quota{}); !errors.Is(err, ErrNotFound) {
				t.Errorf("quota for a non-member = %v", err)
			}
			s.AddMedium(ctx, Medium{ID: "md-pppppp", Owner: "bob", Name: "Fedora", Source: "url", Kind: "disk", SizeBytes: 1, VirtualBytes: 1})
			if pub, _ := s.PublicMedia(ctx); len(pub) != 0 {
				t.Errorf("public before publishing = %v", pub)
			}
			yes := true
			s.UpdateMedium(ctx, "md-pppppp", MediumUpdate{Public: &yes, CloudConfig: map[string]any{"packages": []any{"git"}}})
			s.UpdateMedium(ctx, "md-pppppp", MediumUpdate{}) // nil fields change nothing
			pub, _ := s.PublicMedia(ctx)
			if len(pub) != 1 || pub[0].OwnerName != "bob" || pub[0].CloudConfig["packages"] == nil {
				t.Errorf("public = %+v", pub)
			}
			if all, _ := s.Members(ctx); len(all) != 2 {
				t.Errorf("members = %+v", all)
			}
		})
	}
}
