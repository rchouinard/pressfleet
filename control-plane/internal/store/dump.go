package store

import (
	"bufio"
	"cmp"
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"slices"
	"strings"
	"time"
)

// Dumps: a dialect-neutral copy of the whole state database (#96).
//
// A dump is the disaster recovery backup of the state database: the control
// plane writes one on a schedule into the platform's state repository
// (package backup/statebackup), and a new host loads it with the master
// key. It is a logical export rather than a native one (VACUUM INTO or
// pg_dump) so that one format and one code path cover both dialects
// without PostgreSQL client tools in the image, and so that a dump loads
// into either dialect.
//
// The format is a stream of JSON values, one per line:
//
//	{"format":"pressfleet-state-dump","version":1,"dialect":"sqlite","schema_version":30,"created_at":"..."}
//	{"table":"users","columns":["id","email",...]}
//	["01j...","a@example.com",...]
//	...
//	{"end":{"tables":42,"rows":1234}}
//
// Tables come in foreign key order (referenced tables first, and within a
// table that refers to itself, referenced rows first), so a load inserts
// them in file order on PostgreSQL, whose constraints aren't deferrable.
// Values are JSON null, booleans, numbers, and strings; timestamps are
// RFC 3339 strings in UTC, and binary values are {"b64":"..."}. Loading
// converts each value to the type of its column in the target database,
// which is what makes a dump portable between dialects (SQLite stores
// booleans as integers, for one). The end record makes a truncated dump
// fail to load instead of loading part of the state.
//
// schema_migrations isn't dumped: the header's schema_version stands for it.
// [Load] recreates the schema at that version from the binary's own
// migrations, loads the rows, and then applies the newer migrations, so a
// dump taken by an older release loads into a newer one.

// dumpFormat and dumpVersion identify the dump format.
const (
	dumpFormat  = "pressfleet-state-dump"
	dumpVersion = 1
)

// ErrInvalidDump means a dump is unreadable, truncated, or not a PressFleet
// state dump.
var ErrInvalidDump = errors.New("not a complete PressFleet state dump")

// DumpInfo describes a dump.
type DumpInfo struct {
	// Dialect is the database the dump was taken from.
	Dialect Dialect
	// SchemaVersion is the schema version of the dumped database.
	SchemaVersion int
	// CreatedAt is when the dump was taken.
	CreatedAt time.Time
	// Tables and Rows count what the dump holds.
	Tables int
	Rows   int64
}

// dumpHeader is the first record of a dump.
type dumpHeader struct {
	Format        string    `json:"format"`
	Version       int       `json:"version"`
	Dialect       Dialect   `json:"dialect"`
	SchemaVersion int       `json:"schema_version"`
	CreatedAt     time.Time `json:"created_at"`
}

// dumpRecord is a table or end record.
type dumpRecord struct {
	Table   string   `json:"table,omitempty"`
	Columns []string `json:"columns,omitempty"`
	End     *struct {
		Tables int   `json:"tables"`
		Rows   int64 `json:"rows"`
	} `json:"end,omitempty"`
}

// bytesValue is how binary values are written.
type bytesValue struct {
	B64 string `json:"b64"`
}

// Dump writes a copy of the whole database to w, consistent as of one
// moment: it reads in a single read-only transaction, which neither
// dialect lets block writers.
func (db *DB) Dump(ctx context.Context, w io.Writer) (DumpInfo, error) {
	opts := &sql.TxOptions{ReadOnly: true}
	if db.dialect == Postgres {
		// One snapshot for every table.
		opts.Isolation = sql.LevelRepeatableRead
	}
	tx, err := db.db.BeginTx(ctx, opts)
	if err != nil {
		return DumpInfo{}, fmt.Errorf("dump: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	info, err := dump(ctx, querier{ex: tx, dialect: db.dialect}, w)
	if err != nil {
		return info, fmt.Errorf("dump: %w", err)
	}
	return info, nil
}

func dump(ctx context.Context, q querier, w io.Writer) (DumpInfo, error) {
	info := DumpInfo{Dialect: q.dialect, CreatedAt: now()}
	var err error
	if info.SchemaVersion, err = schemaVersion(ctx, q); err != nil {
		return info, err
	}
	if info.SchemaVersion == 0 {
		return info, errors.New("the database has no schema")
	}
	schema, err := inspect(ctx, q)
	if err != nil {
		return info, err
	}
	order, err := schema.order()
	if err != nil {
		return info, err
	}
	bw := bufio.NewWriter(w)
	enc := json.NewEncoder(bw)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(dumpHeader{
		Format: dumpFormat, Version: dumpVersion, Dialect: q.dialect,
		SchemaVersion: info.SchemaVersion, CreatedAt: info.CreatedAt,
	}); err != nil {
		return info, err
	}
	for _, t := range order {
		n, err := dumpTable(ctx, q, enc, t, schema.selfRef(t))
		if err != nil {
			return info, fmt.Errorf("table %s: %w", t, err)
		}
		info.Tables++
		info.Rows += n
	}
	end := dumpRecord{End: &struct {
		Tables int   `json:"tables"`
		Rows   int64 `json:"rows"`
	}{info.Tables, info.Rows}}
	if err := enc.Encode(end); err != nil {
		return info, err
	}
	return info, bw.Flush()
}

// dumpTable writes one table's record and rows. A table that refers to
// itself (ref non-nil) is ordered so referenced rows come first.
func dumpTable(ctx context.Context, q querier, enc *json.Encoder, table string, ref *foreignKey) (int64, error) {
	rows, err := q.QueryContext(ctx, "SELECT * FROM "+quoteIdent(table))
	if err != nil {
		return 0, err
	}
	defer func() { _ = rows.Close() }()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	if err := enc.Encode(dumpRecord{Table: table, Columns: cols}); err != nil {
		return 0, err
	}
	var (
		n        int64
		buffered [][]any
	)
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i := range vals {
			ptrs[i] = &vals[i]
		}
		if err := rows.Scan(ptrs...); err != nil {
			return n, err
		}
		for i, v := range vals {
			vals[i] = encodeValue(v)
		}
		n++
		if ref != nil {
			buffered = append(buffered, vals)
			continue
		}
		if err := enc.Encode(vals); err != nil {
			return n, err
		}
	}
	if err := rows.Err(); err != nil {
		return n, err
	}
	if ref == nil {
		return n, nil
	}
	sorted, err := parentsFirst(buffered, slices.Index(cols, ref.column), slices.Index(cols, ref.refColumn))
	if err != nil {
		return n, err
	}
	for _, vals := range sorted {
		if err := enc.Encode(vals); err != nil {
			return n, err
		}
	}
	return n, nil
}

// encodeValue converts a scanned value to its JSON form.
func encodeValue(v any) any {
	switch v := v.(type) {
	case []byte:
		return bytesValue{B64: base64.StdEncoding.EncodeToString(v)}
	case time.Time:
		return v.UTC().Format(time.RFC3339Nano)
	default:
		return v
	}
}

// parentsFirst orders the rows of a table that refers to itself so that
// every row comes after the row it refers to.
func parentsFirst(rows [][]any, col, refCol int) ([][]any, error) {
	if col < 0 || refCol < 0 {
		return nil, errors.New("self-referencing columns not found")
	}
	key := func(v any) string { return fmt.Sprint(v) }
	present := map[string]bool{}
	for _, r := range rows {
		present[key(r[refCol])] = true
	}
	done := map[string]bool{}
	sorted := make([][]any, 0, len(rows))
	for len(rows) > 0 {
		var rest [][]any
		for _, r := range rows {
			// A dangling reference can't exist with the foreign key in
			// place; treat it as a root rather than looping forever.
			if r[col] == nil || done[key(r[col])] || !present[key(r[col])] {
				sorted = append(sorted, r)
				done[key(r[refCol])] = true
			} else {
				rest = append(rest, r)
			}
		}
		if len(rest) == len(rows) {
			return nil, errors.New("rows refer to each other in a cycle")
		}
		rows = rest
	}
	return sorted, nil
}

// LoadOptions configure Load.
type LoadOptions struct {
	// BackupDir is where an SQLite database is backed up before it is
	// replaced. Required for SQLite.
	BackupDir string
	// Check, if set, runs in the loading transaction after the rows are in
	// and migrated to the latest schema, before they are committed, e.g.
	// to verify the master key against the loaded state. An error rolls
	// the load back.
	Check func(ctx context.Context, q Querier) error
}

// LoadResult describes what Load did.
type LoadResult struct {
	// Dump describes the dump that was loaded.
	Dump DumpInfo
	// SchemaVersion is the schema version after migrating.
	SchemaVersion int
	// PreRestorePath is a backup of the SQLite database that was replaced,
	// or empty if there was none (or it was PostgreSQL).
	PreRestorePath string
}

// Load replaces the entire database with a dump made by [DB.Dump], from
// either dialect: it drops every table, recreates the schema at the dump's
// version, loads the rows, migrates them to the latest version, runs
// opts.Check, and commits only if all of it succeeded, so a failed load
// leaves the database as it was. It refuses a dump with a schema newer than this
// binary supports (ErrSchemaTooNew) before changing anything.
//
// No other process may use the database. For SQLite, Load takes the same
// exclusive lock as [Restore] (ErrInUse while a control plane has it open)
// and backs the current database up into opts.BackupDir first. A
// PostgreSQL database must be dedicated to PressFleet: every table in its
// schema is dropped.
func Load(ctx context.Context, cfg Config, r io.Reader, opts LoadOptions, log *slog.Logger) (LoadResult, error) {
	var res LoadResult
	dec := json.NewDecoder(bufio.NewReader(r))
	dec.UseNumber()
	h, err := readHeader(dec)
	if err != nil {
		return res, err
	}
	res.Dump = DumpInfo{Dialect: h.Dialect, SchemaVersion: h.SchemaVersion, CreatedAt: h.CreatedAt}
	ms, err := Migrations(cfg.Driver)
	if err != nil {
		return res, err
	}
	if h.SchemaVersion > len(ms) {
		return res, fmt.Errorf("%w: the dump is at schema version %d, this binary supports up to %d; load it with a newer pressfleet release",
			ErrSchemaTooNew, h.SchemaVersion, len(ms))
	}

	db, err := openExclusive(ctx, cfg, log)
	if err != nil {
		return res, err
	}
	defer func() { _ = db.Close() }()
	if cfg.Driver == SQLite {
		if res.PreRestorePath, err = db.backupIfNotEmpty(ctx, opts.BackupDir); err != nil {
			return res, fmt.Errorf("backing up the current database before loading: %w", err)
		}
		if res.PreRestorePath != "" {
			log.Info("backed up current database before loading a dump", "path", res.PreRestorePath)
		}
	}

	err = db.loadTx(ctx, func(q querier) error {
		if err := recreateSchema(ctx, q, ms[:h.SchemaVersion]); err != nil {
			return err
		}
		info, err := loadRows(ctx, q, dec)
		if err != nil {
			return err
		}
		res.Dump.Tables, res.Dump.Rows = info.Tables, info.Rows
		if err := applyMigrations(ctx, q, ms[h.SchemaVersion:]); err != nil {
			return err
		}
		res.SchemaVersion = len(ms)
		if opts.Check != nil {
			return opts.Check(ctx, q)
		}
		return nil
	})
	if err != nil {
		return res, fmt.Errorf("loading the dump: %w", err)
	}
	log.Info("loaded state dump", "from_dialect", h.Dialect, "dump_schema_version", h.SchemaVersion,
		"schema_version", res.SchemaVersion, "tables", res.Dump.Tables, "rows", res.Dump.Rows)
	return res, nil
}

// readHeader reads and checks a dump's header.
func readHeader(dec *json.Decoder) (dumpHeader, error) {
	var h dumpHeader
	if err := dec.Decode(&h); err != nil {
		return h, fmt.Errorf("%w: reading the header: %w", ErrInvalidDump, err)
	}
	if h.Format != dumpFormat {
		return h, ErrInvalidDump
	}
	if h.Version != dumpVersion {
		return h, fmt.Errorf("%w: unsupported dump format version %d", ErrInvalidDump, h.Version)
	}
	if h.SchemaVersion < 1 {
		return h, fmt.Errorf("%w: schema version %d", ErrInvalidDump, h.SchemaVersion)
	}
	return h, nil
}

// openExclusive opens the database for Load: SQLite under the exclusive
// lock, so no control plane has it open.
func openExclusive(ctx context.Context, cfg Config, log *slog.Logger) (*DB, error) {
	if cfg.Driver != SQLite {
		return Open(ctx, cfg, log)
	}
	unlock, err := lockSQLite(cfg.SQLitePath, true)
	if err != nil {
		if errors.Is(err, ErrInUse) {
			return nil, fmt.Errorf("%w: stop the control plane before loading a dump", err)
		}
		return nil, err
	}
	var sqldb *sql.DB
	err = createPrivate(cfg.SQLitePath)
	if err == nil {
		sqldb, err = openSQLite(cfg.SQLitePath, false)
	}
	if err == nil {
		err = sqldb.PingContext(ctx)
	}
	if err != nil {
		if sqldb != nil {
			_ = sqldb.Close()
		}
		_ = unlock()
		return nil, fmt.Errorf("opening sqlite database: %w", err)
	}
	return &DB{querier: querier{ex: sqldb, dialect: SQLite}, db: sqldb, log: log, unlock: unlock}, nil
}

// backupIfNotEmpty backs up an SQLite database that has tables.
func (db *DB) backupIfNotEmpty(ctx context.Context, dir string) (string, error) {
	var tables int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM sqlite_schema WHERE type = 'table'").Scan(&tables); err != nil {
		return "", err
	}
	if tables == 0 {
		return "", nil
	}
	return backupSQLite(ctx, db.querier, dir, "pre-load")
}

// loadTx runs fn in one transaction on one connection. On SQLite, foreign
// keys are checked at commit, since dropping a referenced table deletes
// its rows first; on PostgreSQL, the migration lock keeps a starting
// control plane from migrating meanwhile.
func (db *DB) loadTx(ctx context.Context, fn func(q querier) error) error {
	tx, err := db.db.BeginTx(ctx, nil)
	if err != nil {
		return mapError(err)
	}
	defer func() { _ = tx.Rollback() }()
	q := querier{ex: tx, dialect: db.dialect}
	if db.dialect == Postgres {
		_, err = q.ExecContext(ctx, "SELECT pg_advisory_xact_lock(?)", migrationLockKey)
	} else {
		_, err = q.ExecContext(ctx, "PRAGMA defer_foreign_keys = ON")
	}
	if err != nil {
		return err
	}
	if err := fn(q); err != nil {
		return err
	}
	return mapError(tx.Commit())
}

// recreateSchema drops every table and applies ms, recording them in
// schema_migrations, then empties the tables the migrations seeded.
func recreateSchema(ctx context.Context, q querier, ms []Migration) error {
	current, err := inspect(ctx, q)
	if err != nil {
		return err
	}
	order, err := current.order()
	if err != nil {
		return err
	}
	slices.Reverse(order)
	for _, t := range append(order, "schema_migrations") {
		stmt := "DROP TABLE IF EXISTS " + quoteIdent(t)
		if q.dialect == Postgres {
			stmt += " CASCADE"
		}
		if _, err := q.ExecContext(ctx, stmt); err != nil {
			return fmt.Errorf("dropping table %s: %w", t, err)
		}
	}
	if _, err := q.ExecContext(ctx, createSchemaMigrations[q.dialect]); err != nil {
		return fmt.Errorf("creating schema_migrations: %w", err)
	}
	if err := applyMigrations(ctx, q, ms); err != nil {
		return err
	}
	fresh, err := inspect(ctx, q)
	if err != nil {
		return err
	}
	if order, err = fresh.order(); err != nil {
		return err
	}
	slices.Reverse(order)
	for _, t := range order {
		if _, err := q.ExecContext(ctx, "DELETE FROM "+quoteIdent(t)); err != nil {
			return fmt.Errorf("emptying table %s: %w", t, err)
		}
	}
	return nil
}

// applyMigrations applies ms in the caller's transaction, recording each in
// schema_migrations.
func applyMigrations(ctx context.Context, q querier, ms []Migration) error {
	for _, m := range ms {
		// As applyMigration: the file as-is, without placeholder rewriting.
		if _, err := q.ex.ExecContext(ctx, m.sql); err != nil {
			return fmt.Errorf("migration %04d_%s: %w", m.Version, m.Name, err)
		}
		if _, err := q.ExecContext(ctx, "INSERT INTO schema_migrations (version, name, applied_at) VALUES (?, ?, ?)",
			m.Version, m.Name, now()); err != nil {
			return fmt.Errorf("migration %04d: recording version: %w", m.Version, err)
		}
	}
	return nil
}

// loadRows inserts the dump's tables and rows, up to its end record.
func loadRows(ctx context.Context, q querier, dec *json.Decoder) (DumpInfo, error) {
	var info DumpInfo
	schema, err := inspect(ctx, q)
	if err != nil {
		return info, err
	}
	var (
		ins  *inserter
		seen = map[string]bool{}
	)
	flush := func() error {
		if ins == nil {
			return nil
		}
		return ins.flush(ctx, q)
	}
	for {
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			if errors.Is(err, io.EOF) {
				return info, fmt.Errorf("%w: it ends before its end record", ErrInvalidDump)
			}
			return info, fmt.Errorf("%w: %w", ErrInvalidDump, err)
		}
		if len(raw) > 0 && raw[0] == '[' {
			if ins == nil {
				return info, fmt.Errorf("%w: a row before any table", ErrInvalidDump)
			}
			if err := ins.add(ctx, q, raw); err != nil {
				return info, err
			}
			info.Rows++
			continue
		}
		var rec dumpRecord
		if err := unmarshalNumbers(raw, &rec); err != nil {
			return info, fmt.Errorf("%w: %w", ErrInvalidDump, err)
		}
		if err := flush(); err != nil {
			return info, err
		}
		switch {
		case rec.End != nil:
			if rec.End.Tables != info.Tables || rec.End.Rows != info.Rows {
				return info, fmt.Errorf("%w: it holds %d tables and %d rows, its end record says %d and %d",
					ErrInvalidDump, info.Tables, info.Rows, rec.End.Tables, rec.End.Rows)
			}
			if err := resetIdentities(ctx, q, schema); err != nil {
				return info, err
			}
			return info, nil
		case rec.Table != "":
			if seen[rec.Table] {
				return info, fmt.Errorf("%w: table %s appears twice", ErrInvalidDump, rec.Table)
			}
			seen[rec.Table] = true
			if ins, err = newInserter(schema, rec.Table, rec.Columns); err != nil {
				return info, err
			}
			info.Tables++
		default:
			return info, fmt.Errorf("%w: unknown record %.80s", ErrInvalidDump, raw)
		}
	}
}

func unmarshalNumbers(raw []byte, v any) error {
	dec := json.NewDecoder(strings.NewReader(string(raw)))
	dec.UseNumber()
	return dec.Decode(v)
}

// maxBindVars bounds the placeholders of one INSERT, below SQLite's
// default limit (32766) and PostgreSQL's (65535).
const maxBindVars = 30000

// maxBatchRows bounds the rows of one INSERT.
const maxBatchRows = 500

// inserter inserts one table's rows in batches.
type inserter struct {
	table   string
	columns []string
	kinds   []columnKind
	// overriding says the table has an identity column (PostgreSQL), whose
	// dumped values must be kept.
	overriding bool
	batch      int
	pending    []any
	rows       int
}

func newInserter(s *schemaInfo, table string, columns []string) (*inserter, error) {
	t, ok := s.tables[table]
	if !ok {
		return nil, fmt.Errorf("%w: table %s doesn't exist at the dump's schema version", ErrInvalidDump, table)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("%w: table %s has no columns", ErrInvalidDump, table)
	}
	ins := &inserter{table: table, columns: columns, batch: max(1, min(maxBatchRows, maxBindVars/len(columns)))}
	for _, c := range columns {
		col, ok := t.columns[c]
		if !ok {
			return nil, fmt.Errorf("%w: table %s has no column %s at the dump's schema version", ErrInvalidDump, table, c)
		}
		ins.kinds = append(ins.kinds, col.kind)
		ins.overriding = ins.overriding || col.identity
	}
	return ins, nil
}

func (ins *inserter) add(ctx context.Context, q querier, raw json.RawMessage) error {
	var vals []any
	if err := unmarshalNumbers(raw, &vals); err != nil {
		return fmt.Errorf("%w: table %s: %w", ErrInvalidDump, ins.table, err)
	}
	if len(vals) != len(ins.columns) {
		return fmt.Errorf("%w: table %s: a row has %d values for %d columns", ErrInvalidDump, ins.table, len(vals), len(ins.columns))
	}
	for i, v := range vals {
		arg, err := decodeValue(v, ins.kinds[i])
		if err != nil {
			return fmt.Errorf("%w: table %s, column %s: %w", ErrInvalidDump, ins.table, ins.columns[i], err)
		}
		ins.pending = append(ins.pending, arg)
	}
	ins.rows++
	if ins.rows >= ins.batch {
		return ins.flush(ctx, q)
	}
	return nil
}

func (ins *inserter) flush(ctx context.Context, q querier) error {
	if ins.rows == 0 {
		return nil
	}
	cols := make([]string, len(ins.columns))
	for i, c := range ins.columns {
		cols[i] = quoteIdent(c)
	}
	var b strings.Builder
	b.WriteString("INSERT INTO " + quoteIdent(ins.table) + " (" + strings.Join(cols, ", ") + ")")
	if ins.overriding {
		b.WriteString(" OVERRIDING SYSTEM VALUE")
	}
	b.WriteString(" VALUES ")
	row := "(" + strings.TrimSuffix(strings.Repeat("?, ", len(cols)), ", ") + ")"
	for i := range ins.rows {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(row)
	}
	if _, err := q.ExecContext(ctx, b.String(), ins.pending...); err != nil {
		return fmt.Errorf("inserting into %s: %w", ins.table, err)
	}
	ins.pending, ins.rows = ins.pending[:0], 0
	return nil
}

// decodeValue converts a dumped value to an argument for a column of kind k.
func decodeValue(v any, k columnKind) (any, error) {
	if v == nil {
		return nil, nil
	}
	if m, ok := v.(map[string]any); ok {
		s, ok := m["b64"].(string)
		if !ok || len(m) != 1 {
			return nil, errors.New("unknown value object")
		}
		b, err := base64.StdEncoding.DecodeString(s)
		if err != nil {
			return nil, err
		}
		if k == kindText {
			return string(b), nil
		}
		return b, nil
	}
	switch k {
	case kindTime:
		s, ok := v.(string)
		if !ok {
			return nil, fmt.Errorf("want a timestamp, got %T", v)
		}
		t, err := time.Parse(time.RFC3339Nano, s)
		if err != nil {
			return nil, err
		}
		return t, nil
	case kindBool:
		switch v := v.(type) {
		case bool:
			return v, nil
		case json.Number:
			return v.String() != "0", nil
		}
		return nil, fmt.Errorf("want a boolean, got %T", v)
	case kindInt:
		switch v := v.(type) {
		case json.Number:
			return v.Int64()
		case bool:
			if v {
				return int64(1), nil
			}
			return int64(0), nil
		}
		return nil, fmt.Errorf("want an integer, got %T", v)
	case kindFloat:
		if n, ok := v.(json.Number); ok {
			return n.Float64()
		}
		return nil, fmt.Errorf("want a number, got %T", v)
	case kindBytes:
		if s, ok := v.(string); ok {
			return []byte(s), nil
		}
		return nil, fmt.Errorf("want binary data, got %T", v)
	default:
		switch v := v.(type) {
		case json.Number:
			return v.String(), nil
		case bool:
			return v, nil
		}
		return v, nil
	}
}

// resetIdentities moves each PostgreSQL identity column's sequence past the
// loaded values, so new rows don't collide with them. SQLite's integer
// primary keys need nothing.
func resetIdentities(ctx context.Context, q querier, s *schemaInfo) error {
	if q.dialect != Postgres {
		return nil
	}
	for _, t := range s.sortedTables() {
		for _, c := range s.tables[t].sortedColumns() {
			if !s.tables[t].columns[c].identity {
				continue
			}
			stmt := fmt.Sprintf("SELECT setval(pg_get_serial_sequence(?, ?), COALESCE(MAX(%s), 0) + 1, false) FROM %s",
				quoteIdent(c), quoteIdent(t))
			if _, err := q.ExecContext(ctx, stmt, t, c); err != nil {
				return fmt.Errorf("resetting the identity of %s.%s: %w", t, c, err)
			}
		}
	}
	return nil
}

// quoteIdent quotes an identifier for both dialects. Names come from the
// database's own catalog or are checked against it before use.
func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// columnKind is the kind of value a column holds, as far as converting
// dumped values goes.
type columnKind int

const (
	kindText columnKind = iota
	kindInt
	kindFloat
	kindBool
	kindTime
	kindBytes
)

// kindOf maps a declared (SQLite) or information_schema (PostgreSQL) type
// to its kind.
func kindOf(typ string) columnKind {
	t := strings.ToLower(strings.TrimSpace(typ))
	switch {
	case strings.HasPrefix(t, "timestamp"), t == "datetime", t == "date":
		return kindTime
	case t == "boolean" || t == "bool":
		return kindBool
	case t == "blob" || t == "bytea":
		return kindBytes
	case strings.Contains(t, "int"):
		return kindInt
	case strings.Contains(t, "double"), strings.Contains(t, "real"), strings.Contains(t, "float"), strings.Contains(t, "numeric"):
		return kindFloat
	default:
		return kindText
	}
}

// schemaInfo is what Dump and Load need to know about the tables.
type schemaInfo struct {
	tables map[string]*tableInfo
}

type tableInfo struct {
	columns map[string]columnInfo
	// refs are the table's single-column foreign keys.
	refs []foreignKey
}

type columnInfo struct {
	kind     columnKind
	identity bool
}

// foreignKey is a column referring to a column of another (or the same)
// table.
type foreignKey struct {
	column, refTable, refColumn string
}

func (s *schemaInfo) sortedTables() []string {
	names := make([]string, 0, len(s.tables))
	for t := range s.tables {
		names = append(names, t)
	}
	slices.Sort(names)
	return names
}

func (t *tableInfo) sortedColumns() []string {
	names := make([]string, 0, len(t.columns))
	for c := range t.columns {
		names = append(names, c)
	}
	slices.Sort(names)
	return names
}

// selfRef returns the table's foreign key to itself, or nil.
func (s *schemaInfo) selfRef(table string) *foreignKey {
	for _, fk := range s.tables[table].refs {
		if fk.refTable == table {
			return &fk
		}
	}
	return nil
}

// order returns the tables with every table after the tables it refers to,
// otherwise alphabetically.
func (s *schemaInfo) order() ([]string, error) {
	names := s.sortedTables()
	done := map[string]bool{}
	order := make([]string, 0, len(names))
	for len(order) < len(names) {
		progressed := false
		for _, t := range names {
			if done[t] {
				continue
			}
			ready := true
			for _, fk := range s.tables[t].refs {
				if fk.refTable != t && !done[fk.refTable] && s.tables[fk.refTable] != nil {
					ready = false
					break
				}
			}
			if ready {
				done[t] = true
				order = append(order, t)
				progressed = true
			}
		}
		if !progressed {
			return nil, errors.New("tables refer to each other in a cycle")
		}
	}
	return order, nil
}

// inspect reads the tables (other than schema_migrations and SQLite's own),
// their columns, and their foreign keys from the database's catalog.
func inspect(ctx context.Context, q querier) (*schemaInfo, error) {
	s := &schemaInfo{tables: map[string]*tableInfo{}}
	tablesQuery := "SELECT name FROM sqlite_schema WHERE type = 'table' AND name NOT LIKE 'sqlite\\_%' ESCAPE '\\'"
	if q.dialect == Postgres {
		tablesQuery = "SELECT table_name FROM information_schema.tables WHERE table_schema = current_schema() AND table_type = 'BASE TABLE'"
	}
	names, err := queryStrings(ctx, q, tablesQuery)
	if err != nil {
		return nil, fmt.Errorf("listing tables: %w", err)
	}
	for _, n := range names {
		if n != "schema_migrations" {
			s.tables[n] = &tableInfo{columns: map[string]columnInfo{}}
		}
	}
	for _, t := range s.sortedTables() {
		if err := inspectColumns(ctx, q, t, s.tables[t]); err != nil {
			return nil, fmt.Errorf("inspecting table %s: %w", t, err)
		}
	}
	if err := inspectForeignKeys(ctx, q, s); err != nil {
		return nil, fmt.Errorf("inspecting foreign keys: %w", err)
	}
	return s, nil
}

func inspectColumns(ctx context.Context, q querier, table string, t *tableInfo) error {
	query := "SELECT name, type, 'NO' FROM pragma_table_info(?)"
	if q.dialect == Postgres {
		query = "SELECT column_name, data_type, is_identity FROM information_schema.columns WHERE table_schema = current_schema() AND table_name = ?"
	}
	rows, err := q.QueryContext(ctx, query, table)
	if err != nil {
		return err
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, typ, identity string
		if err := rows.Scan(&name, &typ, &identity); err != nil {
			return err
		}
		t.columns[name] = columnInfo{kind: kindOf(typ), identity: identity == "YES"}
	}
	return rows.Err()
}

func inspectForeignKeys(ctx context.Context, q querier, s *schemaInfo) error {
	if q.dialect == Postgres {
		rows, err := q.QueryContext(ctx, `
			SELECT cl.relname, a.attname, rcl.relname, ra.attname
			FROM pg_constraint con
			JOIN pg_class cl ON cl.oid = con.conrelid
			JOIN pg_namespace n ON n.oid = cl.relnamespace
			JOIN pg_class rcl ON rcl.oid = con.confrelid
			JOIN pg_attribute a ON a.attrelid = con.conrelid AND a.attnum = con.conkey[1]
			JOIN pg_attribute ra ON ra.attrelid = con.confrelid AND ra.attnum = con.confkey[1]
			WHERE con.contype = 'f' AND n.nspname = current_schema()`)
		if err != nil {
			return err
		}
		defer func() { _ = rows.Close() }()
		for rows.Next() {
			var table string
			var fk foreignKey
			if err := rows.Scan(&table, &fk.column, &fk.refTable, &fk.refColumn); err != nil {
				return err
			}
			if t := s.tables[table]; t != nil {
				t.refs = append(t.refs, fk)
			}
		}
		if err := rows.Err(); err != nil {
			return err
		}
	} else {
		for _, table := range s.sortedTables() {
			rows, err := q.QueryContext(ctx, `SELECT "from", "table", COALESCE("to", '') FROM pragma_foreign_key_list(?)`, table)
			if err != nil {
				return err
			}
			for rows.Next() {
				var fk foreignKey
				if err := rows.Scan(&fk.column, &fk.refTable, &fk.refColumn); err != nil {
					_ = rows.Close()
					return err
				}
				s.tables[table].refs = append(s.tables[table].refs, fk)
			}
			err = rows.Err()
			_ = rows.Close()
			if err != nil {
				return err
			}
		}
	}
	for _, t := range s.tables {
		slices.SortFunc(t.refs, func(a, b foreignKey) int {
			return cmp.Or(cmp.Compare(a.refTable, b.refTable), cmp.Compare(a.column, b.column))
		})
	}
	return nil
}

func queryStrings(ctx context.Context, q querier, query string, args ...any) ([]string, error) {
	rows, err := q.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
