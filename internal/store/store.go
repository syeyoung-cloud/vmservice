// Package store is VMService's own data in Postgres: who the members are, which machines are whose
// (rights), and their SSH keys. What a machine is and does lives in its KubeVirt object; this says
// whose it is.
//
// Checks that must hold however many requests arrive at once (per-member limits) run in
// SERIALIZABLE transactions: the database refuses one of two conflicting ones and it's retried.
// Uniqueness (a key belongs to one member) is a primary key.
package store

import (
	"context"
	"embed"
	"errors"
	"fmt"
	"io/fs"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// Machine is a machine's row: its owner and what it was made with.
type Machine struct {
	ID        string
	Owner     string
	Name      string
	Image     string
	Size      string
	CPU       int
	MemoryMiB int
	DiskGiB   int
	Media     string // the image it was made from ("" for machines from before images)
	Profile   string // the device profile it got from its image
	CreatedAt time.Time
	OwnerName string // the owner's username (read only, from members)
}

// Quota is a member's limits; nil fields mean the default.
type Quota struct {
	VMs       *int `json:"vms"`
	CPU       *int `json:"cpu"`
	MemoryMiB *int `json:"memoryMiB"`
	DiskGiB   *int `json:"diskGiB"`
}

// Member is a member's row.
type Member struct {
	Owner      string
	Username   string
	UID        *int
	Quota      Quota
	SignedInAt time.Time
}

// MediumUpdate changes some of an image's fields (nil: unchanged).
type MediumUpdate struct {
	Name        *string
	Public      *bool
	CloudConfig map[string]any
}

// Medium is a member's ISO or disk image (the data is a DataVolume named ID).
type Medium struct {
	ID           string
	Owner        string
	Name         string
	Source       string // upload, url
	URL          string
	Kind         string // iso, disk
	Profile      string // "" until the inspector ran
	SizeBytes    int64
	VirtualBytes int64
	Public       bool
	CloudConfig  map[string]any // extras merged into machines' #cloud-config
	CreatedAt    time.Time
	OwnerName    string // read only, from members
}

// Key is an SSH public key.
type Key struct {
	ID        string
	Owner     string
	Line      string
	Label     string
	CreatedAt time.Time
}

// Usage is what an owner's machines add up to.
type Usage struct {
	VMs       int `json:"vms"`
	CPU       int `json:"cpu"`
	MemoryMiB int `json:"memoryMiB"`
	DiskGiB   int `json:"diskGiB"`
}

var (
	// ErrNotFound: no such row (or not the owner's, where an owner is given).
	ErrNotFound = errors.New("not found")
	// ErrExists: the primary key is taken.
	ErrExists = errors.New("already exists")
)

// LimitError is a create refused because it would go over a limit.
type LimitError struct{ Message string }

func (e *LimitError) Error() string { return e.Message }

// Store is the database.
type Store interface {
	// UpsertMember records a sign-in: the username and now as the sign-in time.
	UpsertMember(ctx context.Context, owner, username string) error
	// EnsureMember adds the member if missing (a session from before they had a row), as signed in now.
	EnsureMember(ctx context.Context, owner, username string) error
	// SignedInAt is the member's last sign-in.
	SignedInAt(ctx context.Context, owner string) (time.Time, error)
	// CreateMachine inserts m if the owner's machines plus m stay within check's limits; check
	// returns a *LimitError when they wouldn't. ErrExists if the ID or name is taken, or the ID was
	// a deleted machine's (IDs are never reused).
	CreateMachine(ctx context.Context, m Machine, check func(Usage, Quota) error) error
	// ResizeMachine sets machine id's size if the owner's other machines plus it at that size stay
	// within check's limits; check returns a *LimitError when they wouldn't. ErrNotFound if no machine.
	ResizeMachine(ctx context.Context, id, size string, cpu, memoryMiB int, check func(Usage, Quota) error) error
	// MachineByName is the machine named name (names are unique across everyone).
	MachineByName(ctx context.Context, name string) (Machine, error)
	AllMachines(ctx context.Context) ([]Machine, error)
	// MachinesFrom is the machines made from the image media.
	MachinesFrom(ctx context.Context, media string) ([]Machine, error)
	Member(ctx context.Context, owner string) (Member, error)
	Members(ctx context.Context) ([]Member, error)
	SetUID(ctx context.Context, owner string, uid *int) error
	SetQuota(ctx context.Context, owner string, q Quota) error
	PublicMedia(ctx context.Context) ([]Medium, error)
	AllMedia(ctx context.Context) ([]Medium, error)
	UpdateMedium(ctx context.Context, id string, u MediumUpdate) error
	Machine(ctx context.Context, id string) (Machine, error)
	Machines(ctx context.Context, owner string) ([]Machine, error)
	Usage(ctx context.Context, owner string) (Usage, error)
	DeleteMachine(ctx context.Context, id string) error
	// AddKey inserts k if the owner has fewer than max keys; ErrExists if anyone has it.
	AddKey(ctx context.Context, k Key, max int) error
	Key(ctx context.Context, id string) (Key, error)
	Keys(ctx context.Context, owner string) ([]Key, error)
	DeleteKey(ctx context.Context, owner, id string) error
	AddMedium(ctx context.Context, m Medium) error
	Medium(ctx context.Context, id string) (Medium, error)
	Media(ctx context.Context, owner string) ([]Medium, error)
	// SetMediumProfile records what the inspector found.
	SetMediumProfile(ctx context.Context, id, profile string, virtualBytes int64) error
	DeleteMedium(ctx context.Context, id string) error
}

//go:embed migrations/*.sql
var migrations embed.FS

// Postgres is the Store on a Postgres database.
type Postgres struct{ pool *pgxpool.Pool }

// Open connects and brings the schema up to date.
func Open(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	p := &Postgres{pool}
	if err := p.migrate(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("migrating the database: %w", err)
	}
	return p, nil
}

// OpenReadOnly connects without touching the schema, for a process whose role may only read
// (the bastion).
func OpenReadOnly(ctx context.Context, url string) (*Postgres, error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, err
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, err
	}
	return &Postgres{pool}, nil
}

// GrantRead lets role read what the bastion needs (members, machines, ssh_keys) and nothing else.
// Run by the owner at every start, so it holds whichever was created first.
func (p *Postgres) GrantRead(ctx context.Context, role string) error {
	_, err := p.pool.Exec(ctx, `GRANT SELECT ON members, machines, ssh_keys TO `+pgx.Identifier{role}.Sanitize())
	return err
}

// Close closes the pool.
func (p *Postgres) Close() { p.pool.Close() }

// migrate applies migrations/NNN_*.sql in order, each once, in its own transaction. A concurrent
// starter applying the same version fails on the primary key and its transaction rolls back.
func (p *Postgres) migrate(ctx context.Context) error {
	if _, err := p.pool.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (version text PRIMARY KEY, applied_at timestamptz NOT NULL DEFAULT now())`); err != nil {
		return err
	}
	names, err := fs.Glob(migrations, "migrations/*.sql")
	if err != nil {
		return err
	}
	sort.Strings(names)
	for _, name := range names {
		version := strings.TrimSuffix(strings.TrimPrefix(name, "migrations/"), ".sql")
		sql, err := migrations.ReadFile(name)
		if err != nil {
			return err
		}
		err = pgx.BeginFunc(ctx, p.pool, func(tx pgx.Tx) error {
			tag, err := tx.Exec(ctx, `INSERT INTO schema_migrations (version) VALUES ($1) ON CONFLICT DO NOTHING`, version)
			if err != nil || tag.RowsAffected() == 0 {
				return err // applied already
			}
			_, err = tx.Exec(ctx, string(sql))
			return err
		})
		if err != nil {
			return fmt.Errorf("%s: %w", version, err)
		}
	}
	return nil
}

// serializable runs fn in a SERIALIZABLE transaction, again when the database refuses it for a
// conflict with a concurrent one (SQLSTATE 40001).
func (p *Postgres) serializable(ctx context.Context, fn func(pgx.Tx) error) error {
	for attempt := 0; ; attempt++ {
		err := pgx.BeginTxFunc(ctx, p.pool, pgx.TxOptions{IsoLevel: pgx.Serializable}, fn)
		var pe *pgconn.PgError
		if errors.As(err, &pe) && pe.Code == "40001" && attempt < 10 {
			continue
		}
		return err
	}
}

func isUniqueViolation(err error) bool {
	var pe *pgconn.PgError
	return errors.As(err, &pe) && pe.Code == "23505"
}

func (p *Postgres) UpsertMember(ctx context.Context, owner, username string) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO members (owner, username) VALUES ($1, $2)
		ON CONFLICT (owner) DO UPDATE SET username = EXCLUDED.username, signed_in_at = now()`, owner, username)
	return err
}

func (p *Postgres) EnsureMember(ctx context.Context, owner, username string) error {
	_, err := p.pool.Exec(ctx, `INSERT INTO members (owner, username) VALUES ($1, $2) ON CONFLICT (owner) DO NOTHING`, owner, username)
	return err
}

func (p *Postgres) SignedInAt(ctx context.Context, owner string) (time.Time, error) {
	var t time.Time
	err := p.pool.QueryRow(ctx, `SELECT signed_in_at FROM members WHERE owner = $1`, owner).Scan(&t)
	if errors.Is(err, pgx.ErrNoRows) {
		return t, ErrNotFound
	}
	return t, err
}

const usageQuery = `SELECT count(*), coalesce(sum(cpu), 0), coalesce(sum(memory_mib), 0), coalesce(sum(disk_gib), 0) FROM machines WHERE owner = $1`

func scanUsage(row pgx.Row) (Usage, error) {
	var u Usage
	err := row.Scan(&u.VMs, &u.CPU, &u.MemoryMiB, &u.DiskGiB)
	return u, err
}

func (p *Postgres) CreateMachine(ctx context.Context, m Machine, check func(Usage, Quota) error) error {
	return p.serializable(ctx, func(tx pgx.Tx) error {
		u, err := scanUsage(tx.QueryRow(ctx, usageQuery, m.Owner))
		if err != nil {
			return err
		}
		var q Quota
		if err := tx.QueryRow(ctx, `SELECT quota_vms, quota_cpu, quota_memory_mib, quota_disk_gib FROM members WHERE owner = $1`, m.Owner).Scan(&q.VMs, &q.CPU, &q.MemoryMiB, &q.DiskGiB); err != nil {
			return err
		}
		if err := check(u, q); err != nil {
			return err
		}
		var retired bool
		if err := tx.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM retired_machine_ids WHERE id = $1)`, m.ID).Scan(&retired); err != nil {
			return err
		}
		if retired {
			return ErrExists // the caller draws another ID
		}
		_, err = tx.Exec(ctx, `INSERT INTO machines (id, owner, name, image, size, cpu, memory_mib, disk_gib, media, profile) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
			m.ID, m.Owner, m.Name, m.Image, m.Size, m.CPU, m.MemoryMiB, m.DiskGiB, m.Media, m.Profile)
		if isUniqueViolation(err) {
			return ErrExists
		}
		return err
	})
}

func (p *Postgres) ResizeMachine(ctx context.Context, id, size string, cpu, memoryMiB int, check func(Usage, Quota) error) error {
	return p.serializable(ctx, func(tx pgx.Tx) error {
		var owner string
		var disk int
		err := tx.QueryRow(ctx, `SELECT owner, disk_gib FROM machines WHERE id = $1`, id).Scan(&owner, &disk)
		if errors.Is(err, pgx.ErrNoRows) {
			return ErrNotFound
		}
		if err != nil {
			return err
		}
		u, err := scanUsage(tx.QueryRow(ctx, usageQuery+` AND id <> $2`, owner, id)) // the others
		if err != nil {
			return err
		}
		var q Quota
		if err := tx.QueryRow(ctx, `SELECT quota_vms, quota_cpu, quota_memory_mib, quota_disk_gib FROM members WHERE owner = $1`, owner).Scan(&q.VMs, &q.CPU, &q.MemoryMiB, &q.DiskGiB); err != nil {
			return err
		}
		if err := check(u, q); err != nil {
			return err
		}
		_, err = tx.Exec(ctx, `UPDATE machines SET size = $2, cpu = $3, memory_mib = $4 WHERE id = $1`, id, size, cpu, memoryMiB)
		return err
	})
}

const machineSelect = `SELECT m.id, m.owner, m.name, m.image, m.size, m.cpu, m.memory_mib, m.disk_gib, m.media, m.profile, m.created_at, coalesce(u.username, '')
	FROM machines m LEFT JOIN members u ON u.owner = m.owner `

func scanMachine(row pgx.Row) (Machine, error) {
	var m Machine
	err := row.Scan(&m.ID, &m.Owner, &m.Name, &m.Image, &m.Size, &m.CPU, &m.MemoryMiB, &m.DiskGiB, &m.Media, &m.Profile, &m.CreatedAt, &m.OwnerName)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

func (p *Postgres) Machine(ctx context.Context, id string) (Machine, error) {
	return scanMachine(p.pool.QueryRow(ctx, machineSelect+`WHERE m.id = $1`, id))
}

func (p *Postgres) Machines(ctx context.Context, owner string) ([]Machine, error) {
	return p.machines(ctx, machineSelect+`WHERE m.owner = $1 ORDER BY m.created_at DESC`, owner)
}

func (p *Postgres) machines(ctx context.Context, sql string, args ...any) ([]Machine, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Machine, error) { return scanMachine(r) })
}

func (p *Postgres) MachineByName(ctx context.Context, name string) (Machine, error) {
	return scanMachine(p.pool.QueryRow(ctx, machineSelect+`WHERE m.name = $1`, name))
}

func (p *Postgres) MachinesFrom(ctx context.Context, media string) ([]Machine, error) {
	return p.machines(ctx, machineSelect+`WHERE m.media = $1 ORDER BY m.created_at DESC`, media)
}

func (p *Postgres) AllMachines(ctx context.Context) ([]Machine, error) {
	return p.machines(ctx, machineSelect+`ORDER BY m.created_at DESC`)
}

const memberColumns = `owner, username, uid, quota_vms, quota_cpu, quota_memory_mib, quota_disk_gib, signed_in_at`

func scanMember(row pgx.Row) (Member, error) {
	var m Member
	err := row.Scan(&m.Owner, &m.Username, &m.UID, &m.Quota.VMs, &m.Quota.CPU, &m.Quota.MemoryMiB, &m.Quota.DiskGiB, &m.SignedInAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

func (p *Postgres) Member(ctx context.Context, owner string) (Member, error) {
	return scanMember(p.pool.QueryRow(ctx, `SELECT `+memberColumns+` FROM members WHERE owner = $1`, owner))
}

func (p *Postgres) Members(ctx context.Context) ([]Member, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+memberColumns+` FROM members ORDER BY username`)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Member, error) { return scanMember(r) })
}

func (p *Postgres) SetUID(ctx context.Context, owner string, uid *int) error {
	_, err := p.pool.Exec(ctx, `UPDATE members SET uid = $2 WHERE owner = $1`, owner, uid)
	return err
}

func (p *Postgres) SetQuota(ctx context.Context, owner string, q Quota) error {
	tag, err := p.pool.Exec(ctx, `UPDATE members SET quota_vms = $2, quota_cpu = $3, quota_memory_mib = $4, quota_disk_gib = $5 WHERE owner = $1`,
		owner, q.VMs, q.CPU, q.MemoryMiB, q.DiskGiB)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) Usage(ctx context.Context, owner string) (Usage, error) {
	return scanUsage(p.pool.QueryRow(ctx, usageQuery, owner))
}

// DeleteMachine removes the row and retires its ID, in one transaction.
func (p *Postgres) DeleteMachine(ctx context.Context, id string) error {
	return p.serializable(ctx, func(tx pgx.Tx) error {
		tag, err := tx.Exec(ctx, `DELETE FROM machines WHERE id = $1`, id)
		if err != nil || tag.RowsAffected() == 0 {
			return err
		}
		_, err = tx.Exec(ctx, `INSERT INTO retired_machine_ids (id) VALUES ($1) ON CONFLICT DO NOTHING`, id)
		return err
	})
}

func (p *Postgres) AddKey(ctx context.Context, k Key, max int) error {
	return p.serializable(ctx, func(tx pgx.Tx) error {
		var n int
		if err := tx.QueryRow(ctx, `SELECT count(*) FROM ssh_keys WHERE owner = $1`, k.Owner).Scan(&n); err != nil {
			return err
		}
		if n >= max {
			return &LimitError{fmt.Sprintf("You already have %d keys.", max)}
		}
		_, err := tx.Exec(ctx, `INSERT INTO ssh_keys (id, owner, key, label) VALUES ($1, $2, $3, $4)`, k.ID, k.Owner, k.Line, k.Label)
		if isUniqueViolation(err) {
			return ErrExists
		}
		return err
	})
}

const keyColumns = `id, owner, key, label, created_at`

func scanKey(row pgx.Row) (Key, error) {
	var k Key
	err := row.Scan(&k.ID, &k.Owner, &k.Line, &k.Label, &k.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return k, ErrNotFound
	}
	return k, err
}

func (p *Postgres) Key(ctx context.Context, id string) (Key, error) {
	return scanKey(p.pool.QueryRow(ctx, `SELECT `+keyColumns+` FROM ssh_keys WHERE id = $1`, id))
}

func (p *Postgres) Keys(ctx context.Context, owner string) ([]Key, error) {
	rows, err := p.pool.Query(ctx, `SELECT `+keyColumns+` FROM ssh_keys WHERE owner = $1 ORDER BY created_at DESC`, owner)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Key, error) { return scanKey(r) })
}

func (p *Postgres) DeleteKey(ctx context.Context, owner, id string) error {
	tag, err := p.pool.Exec(ctx, `DELETE FROM ssh_keys WHERE id = $1 AND owner = $2`, id, owner)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

const mediumSelect = `SELECT d.id, d.owner, d.name, d.source, d.url, d.kind, d.profile, d.size_bytes, d.virtual_bytes, d.public, d.cloud_config, d.created_at, coalesce(u.username, '')
	FROM media d LEFT JOIN members u ON u.owner = d.owner `

func scanMedium(row pgx.Row) (Medium, error) {
	var m Medium
	err := row.Scan(&m.ID, &m.Owner, &m.Name, &m.Source, &m.URL, &m.Kind, &m.Profile, &m.SizeBytes, &m.VirtualBytes, &m.Public, &m.CloudConfig, &m.CreatedAt, &m.OwnerName)
	if errors.Is(err, pgx.ErrNoRows) {
		return m, ErrNotFound
	}
	return m, err
}

func (p *Postgres) AddMedium(ctx context.Context, m Medium) error {
	if m.CloudConfig == nil {
		m.CloudConfig = map[string]any{}
	}
	_, err := p.pool.Exec(ctx, `INSERT INTO media (id, owner, name, source, url, kind, profile, size_bytes, virtual_bytes, public, cloud_config) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)`,
		m.ID, m.Owner, m.Name, m.Source, m.URL, m.Kind, m.Profile, m.SizeBytes, m.VirtualBytes, m.Public, m.CloudConfig)
	if isUniqueViolation(err) {
		return ErrExists
	}
	return err
}

func (p *Postgres) Medium(ctx context.Context, id string) (Medium, error) {
	return scanMedium(p.pool.QueryRow(ctx, mediumSelect+`WHERE d.id = $1`, id))
}

func (p *Postgres) Media(ctx context.Context, owner string) ([]Medium, error) {
	return p.media(ctx, mediumSelect+`WHERE d.owner = $1 ORDER BY d.created_at DESC`, owner)
}

func (p *Postgres) media(ctx context.Context, sql string, args ...any) ([]Medium, error) {
	rows, err := p.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	return pgx.CollectRows(rows, func(r pgx.CollectableRow) (Medium, error) { return scanMedium(r) })
}

func (p *Postgres) PublicMedia(ctx context.Context) ([]Medium, error) {
	return p.media(ctx, mediumSelect+`WHERE d.public ORDER BY d.created_at DESC`)
}

func (p *Postgres) AllMedia(ctx context.Context) ([]Medium, error) {
	return p.media(ctx, mediumSelect+`ORDER BY d.created_at DESC`)
}

func (p *Postgres) UpdateMedium(ctx context.Context, id string, u MediumUpdate) error {
	var cloudConfig any // a nil map would be JSON null, not SQL NULL ("unchanged")
	if u.CloudConfig != nil {
		cloudConfig = u.CloudConfig
	}
	tag, err := p.pool.Exec(ctx, `UPDATE media SET name = coalesce($2, name), public = coalesce($3, public), cloud_config = coalesce($4, cloud_config) WHERE id = $1`,
		id, u.Name, u.Public, cloudConfig)
	if err == nil && tag.RowsAffected() == 0 {
		return ErrNotFound
	}
	return err
}

func (p *Postgres) SetMediumProfile(ctx context.Context, id, profile string, virtualBytes int64) error {
	_, err := p.pool.Exec(ctx, `UPDATE media SET profile = $2, virtual_bytes = greatest(virtual_bytes, $3) WHERE id = $1`, id, profile, virtualBytes)
	return err
}

func (p *Postgres) DeleteMedium(ctx context.Context, id string) error {
	_, err := p.pool.Exec(ctx, `DELETE FROM media WHERE id = $1`, id)
	return err
}
