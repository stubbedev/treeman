package mongo

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/stubbedev/treeman/internal/config"
	"github.com/stubbedev/treeman/internal/db/containerip"
	"github.com/stubbedev/treeman/internal/db/dumpload"
)

// Restore loads a `mongodump --archive` archive into `targetDB`. It
// picks the fastest available path:
//
//  1. `<container-engine> exec -i CID mongorestore …` when the
//     connection's ContainerRef resolves to a running container.
//  2. Host `mongorestore` CLI on PATH, piping the archive over stdin.
//  3. Pure-Go wire-protocol fallback that parses the BSON archive
//     stream itself and inserts every doc via the official mongo Go
//     driver. Always available — selected when neither CLI path is.
//
// `sourceDB` names the DB the archive was made from (`mongodump
// --db=`). All three strategies remap it onto `targetDB`. Pass
// sourceDB="" to skip the rename (the archive must already use the
// target name).
//
// Returns the strategy that actually ran so callers can log it. Fast-
// path FAILURES (CLI ran but exited non-zero) are downgraded to a
// warn log + fall-through, so a dev box missing mongorestore can
// never block a cold build.
func Restore(ctx context.Context, conn *config.MongoConn, targetDB, sourceDB, dumpPath string) (dumpload.LoadStrategy, error) {
	if ok, err := tryDockerExecMongoRestore(ctx, conn, targetDB, sourceDB, dumpPath); ok {
		return dumpload.StrategyDockerExec, nil
	} else if err != nil {
		slog.Warn("mongo restore fast path (docker exec) failed; falling through", "error", err)
	}
	if ok, err := tryNativeCLIMongoRestore(ctx, conn, targetDB, sourceDB, dumpPath); ok {
		return dumpload.StrategyNativeCLI, nil
	} else if err != nil {
		slog.Warn("mongo restore fast path (native CLI) failed; falling through", "error", err)
	}
	return dumpload.StrategyWire, restoreViaDriver(ctx, conn, targetDB, sourceDB, dumpPath)
}

// tryDockerExecMongoRestore runs mongorestore INSIDE the engine
// container via `<engine> exec -i`. Returns (true, nil) on success,
// (false, nil) when the container ref isn't set / not resolvable, and
// (false, err) when exec ran but failed.
func tryDockerExecMongoRestore(ctx context.Context, conn *config.MongoConn, targetDB, sourceDB, dumpPath string) (bool, error) {
	if conn == nil || (conn.Container == "" && conn.ComposeService == "") {
		return false, nil
	}
	opts := containerip.Opts{
		Container:      conn.Container,
		ComposeService: conn.ComposeService,
		ComposeProject: conn.ComposeProject,
		Engine:         conn.ContainerEngine,
		Network:        conn.Network,
	}
	cid, cerr := containerip.ContainerID(ctx, opts)
	if cerr != nil {
		return false, nil
	}
	engineBin := opts.Engine
	if engineBin == "" {
		engineBin = "docker"
	}
	if _, err := exec.LookPath(engineBin); err != nil {
		return false, nil
	}
	args := []string{
		"exec", "-i", cid, "mongorestore",
		"--archive", "--drop", "--quiet", "--uri=" + conn.URI,
	}
	if sourceDB != "" && sourceDB != targetDB {
		args = append(args, "--nsFrom="+sourceDB+".*", "--nsTo="+targetDB+".*")
	}
	rc, format, oerr := dumpload.OpenDump(dumpPath)
	if oerr != nil {
		return false, oerr
	}
	defer func() { _ = rc.Close() }()
	if format == dumpload.FormatGzip {
		args = append(args, "--gzip")
	}
	cmd := exec.CommandContext(ctx, engineBin, args...)
	cmd.Stdin = rc
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		return false, fmt.Errorf("%s exec mongorestore: %w (stderr: %s)", engineBin, err, stderr.String())
	}
	return true, nil
}

// tryNativeCLIMongoRestore runs the host's `mongorestore` CLI when
// it's on PATH.
func tryNativeCLIMongoRestore(ctx context.Context, conn *config.MongoConn, targetDB, sourceDB, dumpPath string) (bool, error) {
	if conn == nil {
		return false, nil
	}
	if _, err := exec.LookPath("mongorestore"); err != nil {
		return false, nil
	}
	args := []string{"--uri=" + conn.URI, "--drop", "--quiet"}
	if sourceDB != "" && sourceDB != targetDB {
		args = append(args, "--nsFrom="+sourceDB+".*", "--nsTo="+targetDB+".*")
	}
	rc, format, err := dumpload.OpenDump(dumpPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = rc.Close() }()
	var cmd *exec.Cmd
	switch format {
	case dumpload.FormatNone:
		cmd = exec.CommandContext(ctx, "mongorestore", append(args, "--archive="+dumpPath)...)
	case dumpload.FormatGzip:
		// mongorestore handles gzip natively — let it do the work.
		cmd = exec.CommandContext(ctx, "mongorestore", append(args, "--archive="+dumpPath, "--gzip")...)
	default:
		// zstd/bzip2/xz: decompress in Go, pipe via stdin.
		cmd = exec.CommandContext(ctx, "mongorestore", append(args, "--archive")...)
		cmd.Stdin = rc
	}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if rerr := cmd.Run(); rerr != nil {
		return false, fmt.Errorf("mongorestore (%s): %w: %s", format, rerr, stderr.String())
	}
	return true, nil
}

// restoreStagePrefix names the staging collection a namespace loads
// into before being promoted onto its final name. Loading into a
// staging collection + renameCollection(dropTarget) at the end gives
// the wire fallback the same per-collection all-or-nothing semantics
// `mongorestore --drop` has: a mid-stream failure leaves the existing
// target collections untouched instead of half-dropped/half-loaded.
const restoreStagePrefix = "_tmload_"

// restoreBatchSize bounds one InsertMany call. 500 docs per round-trip
// replaces the old per-doc InsertOne without buffering whole dumps.
const restoreBatchSize = 500

// restoreViaDriver is the pure-Go wire-protocol fallback. It parses
// the `mongodump --archive` stream itself and loads every doc via the
// official mongo Go driver, which speaks the same BSON wire protocol
// the CLI does. Compression is auto-detected via dumpload.OpenDump.
//
// Semantics (mirroring `mongorestore --drop` where possible):
//   - Docs batch-insert into `_tmload_<coll>` staging collections; on
//     success every staging collection is promoted onto its final name
//     via renameCollection(dropTarget:true) — atomic per collection.
//     Any failure drops the staging collections and leaves the
//     pre-restore targets intact.
//   - Indexes are replayed from the prelude's collection metadata
//     (skipping the implicit `_id_`). Unparseable metadata (mongodump
//     version drift) downgrades to a warning — the data still loads.
//   - Empty collections declared in the prelude are created even when
//     no body documents follow.
//   - Collection OPTIONS (capped, validators, collation) are NOT
//     replayed, and views/timeseries are skipped — logged so the
//     operator can install mongorestore if those matter.
func restoreViaDriver(ctx context.Context, conn *config.MongoConn, targetDB, sourceDB, dumpPath string) error {
	if conn == nil || conn.URI == "" {
		return errors.New("mongo wire restore: missing connection URI")
	}
	client, err := mongo.Connect(options.Client().ApplyURI(conn.URI))
	if err != nil {
		return fmt.Errorf("mongo connect for wire restore: %w", err)
	}
	defer func() { _ = client.Disconnect(ctx) }()

	rc, _, err := dumpload.OpenDump(dumpPath)
	if err != nil {
		return err
	}
	defer func() { _ = rc.Close() }()

	r := &archiveReader{r: rc}
	metas, err := r.readHeader()
	if err != nil {
		return fmt.Errorf("read archive header: %w", err)
	}

	remap := func(db string) string {
		if sourceDB != "" && db == sourceDB {
			return targetDB
		}
		return db
	}

	st := &wireStager{
		client: client,
		staged: map[string]nsParts{},
		opts:   collectCollOptions(metas, remap),
	}
	if err := st.run(ctx, r, metas, remap); err != nil {
		st.cleanup(ctx)
		return err
	}

	if err := replayIndexes(ctx, client, metas, remap); err != nil {
		return err
	}

	slog.Info("mongo restore: wire-protocol fallback used (indexes + collection options replayed; views/timeseries not)",
		"path", dumpPath, "target_db", targetDB,
		"hint", "install mongorestore on PATH or set ContainerRef on connections.mongodb for full fidelity")
	return nil
}

// nsParts is one staged namespace, split into database + collection.
type nsParts struct{ db, coll string }

// wireStager owns the staging-collection lifecycle of one wire
// restore: batch inserts into `_tmload_<coll>` collections, promotion
// onto the final names, and failure cleanup.
type wireStager struct {
	client *mongo.Client
	// staged maps the final "db.coll" of every live staging collection
	// to its split pieces.
	staged map[string]nsParts
	// opts carries each namespace's collection options (capped,
	// validators, collation, …) parsed from the prelude metadata,
	// keyed by final "db.coll". Applied at staging-create time so the
	// promoted collection keeps them (rename preserves options).
	opts   map[string]bson.D
	batch  []any
	curDB  string
	curCol string
}

// run drains the archive body into staging collections, materializes
// prelude-only (empty) collections, then promotes everything.
func (s *wireStager) run(ctx context.Context, r *archiveReader, metas []collMeta, remap func(string) string) error {
	for {
		ns, doc, terminate, rerr := r.next()
		if rerr != nil {
			return rerr
		}
		if terminate {
			break
		}
		dbName, coll := splitNamespace(ns)
		if dbName == "" || coll == "" {
			continue
		}
		if err := s.add(ctx, remap(dbName), coll, doc); err != nil {
			return err
		}
	}
	if err := s.flush(ctx); err != nil {
		return err
	}
	// Prelude-declared collections with no body docs still exist in
	// the dump — create them empty so the restored DB matches.
	for _, m := range metas {
		if m.skip() {
			continue
		}
		db := remap(m.DB)
		if _, ok := s.staged[db+"."+m.Collection]; ok {
			continue
		}
		if err := s.stage(ctx, db, m.Collection); err != nil {
			return err
		}
		if !s.stagedWithOptions(db, m.Collection) {
			if cerr := s.client.Database(db).CreateCollection(ctx, restoreStagePrefix+m.Collection); cerr != nil {
				return fmt.Errorf("create empty staging %s.%s%s: %w", db, restoreStagePrefix, m.Collection, cerr)
			}
		}
	}
	return s.promote(ctx)
}

// add routes one body document into its staging batch, flushing on
// namespace switches and on full batches.
func (s *wireStager) add(ctx context.Context, db, coll string, doc bson.Raw) error {
	if db != s.curDB || coll != s.curCol {
		if err := s.flush(ctx); err != nil {
			return err
		}
		s.curDB, s.curCol = db, coll
	}
	if err := s.stage(ctx, db, coll); err != nil {
		return err
	}
	s.batch = append(s.batch, doc)
	if len(s.batch) >= restoreBatchSize {
		return s.flush(ctx)
	}
	return nil
}

// flush InsertMany-s the pending batch into the current staging
// collection.
func (s *wireStager) flush(ctx context.Context) error {
	if len(s.batch) == 0 {
		return nil
	}
	_, err := s.client.Database(s.curDB).Collection(restoreStagePrefix+s.curCol).InsertMany(ctx, s.batch)
	s.batch = s.batch[:0]
	if err != nil {
		return fmt.Errorf("insert batch into %s.%s%s: %w", s.curDB, restoreStagePrefix, s.curCol, err)
	}
	return nil
}

// stage registers a namespace's staging collection on first sight:
// clears any leftover from a previously crashed restore, then — when
// the prelude carried collection options for this namespace — creates
// the staging collection explicitly with those options via a raw
// `create` command (listCollections options ARE create options, so
// they pass through verbatim). Optionless collections materialize
// lazily on first insert, as before.
func (s *wireStager) stage(ctx context.Context, db, coll string) error {
	key := db + "." + coll
	if _, ok := s.staged[key]; ok {
		return nil
	}
	if err := s.client.Database(db).Collection(restoreStagePrefix + coll).Drop(ctx); err != nil {
		return fmt.Errorf("drop stale staging %s.%s%s: %w", db, restoreStagePrefix, coll, err)
	}
	if o := s.opts[key]; len(o) > 0 {
		cmd := append(bson.D{{Key: "create", Value: restoreStagePrefix + coll}}, o...)
		if err := s.client.Database(db).RunCommand(ctx, cmd).Err(); err != nil {
			return fmt.Errorf("create staging %s.%s%s with options: %w", db, restoreStagePrefix, coll, err)
		}
	}
	s.staged[key] = nsParts{db: db, coll: coll}
	return nil
}

// stagedWithOptions reports whether stage() already created this
// namespace's staging collection explicitly (options path) — callers
// materializing EMPTY collections skip their plain create in that case.
func (s *wireStager) stagedWithOptions(db, coll string) bool {
	return len(s.opts[db+"."+coll]) > 0
}

// promote renames every staging collection onto its final name —
// atomic per collection, replacing the previous contents wholesale.
func (s *wireStager) promote(ctx context.Context) error {
	for key, p := range s.staged {
		cmd := bson.D{
			{Key: "renameCollection", Value: p.db + "." + restoreStagePrefix + p.coll},
			{Key: "to", Value: key},
			{Key: "dropTarget", Value: true},
		}
		if err := s.client.Database("admin").RunCommand(ctx, cmd).Err(); err != nil {
			return fmt.Errorf("promote %s: %w", key, err)
		}
		delete(s.staged, key)
	}
	return nil
}

// cleanup reaps the surviving staging collections after a failed
// restore, on a fresh short-lived context — ctx may be cancelled.
func (s *wireStager) cleanup(ctx context.Context) {
	bgctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	for _, p := range s.staged {
		_ = s.client.Database(p.db).Collection(restoreStagePrefix + p.coll).Drop(bgctx)
	}
}

// replayIndexes re-creates the indexes encoded in the prelude's
// collection metadata on the (already promoted) target collections.
// The implicit `_id_` index is skipped; the legacy `ns` field is
// stripped (createIndexes rejects it on modern servers). Metadata that
// fails to parse (mongodump extended-JSON drift) is warned + skipped —
// the data already restored; a createIndexes refusal is a hard error,
// matching mongorestore.
func replayIndexes(ctx context.Context, client *mongo.Client, metas []collMeta, remap func(string) string) error {
	for _, m := range metas {
		if m.skip() {
			continue
		}
		specs, perr := parseIndexSpecs(m.Metadata)
		if perr != nil {
			slog.Warn("mongo restore: collection metadata unparseable; indexes skipped",
				"ns", m.DB+"."+m.Collection, "err", perr)
			continue
		}
		if len(specs) == 0 {
			continue
		}
		db := remap(m.DB)
		cmd := bson.D{
			{Key: "createIndexes", Value: m.Collection},
			{Key: "indexes", Value: specs},
		}
		if err := client.Database(db).RunCommand(ctx, cmd).Err(); err != nil {
			return fmt.Errorf("replay %d index(es) on %s.%s: %w", len(specs), db, m.Collection, err)
		}
	}
	return nil
}

// collectCollOptions parses every prelude entry's `options` blob into
// a final-namespace-keyed map. Unparseable metadata is skipped (the
// data still restores; same policy as index replay).
func collectCollOptions(metas []collMeta, remap func(string) string) map[string]bson.D {
	out := map[string]bson.D{}
	for _, m := range metas {
		if m.skip() || m.Metadata == "" {
			continue
		}
		var parsed struct {
			Options bson.D `bson:"options"`
		}
		if err := bson.UnmarshalExtJSON([]byte(m.Metadata), false, &parsed); err != nil {
			slog.Warn("mongo restore: collection metadata unparseable; options skipped",
				"ns", m.DB+"."+m.Collection, "err", err)
			continue
		}
		if len(parsed.Options) > 0 {
			out[remap(m.DB)+"."+m.Collection] = parsed.Options
		}
	}
	return out
}

// parseIndexSpecs extracts the index documents from one collection's
// metadata blob (extended JSON), dropping the implicit `_id_` index
// and the legacy per-index `ns` field.
func parseIndexSpecs(metaJSON string) ([]bson.D, error) {
	if metaJSON == "" {
		return nil, nil
	}
	var m struct {
		Indexes []bson.D `bson:"indexes"`
	}
	if err := bson.UnmarshalExtJSON([]byte(metaJSON), false, &m); err != nil {
		return nil, err
	}
	out := make([]bson.D, 0, len(m.Indexes))
	for _, idx := range m.Indexes {
		spec := make(bson.D, 0, len(idx))
		isID := false
		for _, e := range idx {
			if e.Key == "ns" {
				continue
			}
			if e.Key == "name" && e.Value == "_id_" {
				isID = true
			}
			spec = append(spec, e)
		}
		if !isID && len(spec) > 0 {
			out = append(out, spec)
		}
	}
	return out, nil
}

// archiveMagic is the first four bytes every `mongodump --archive`
// stream begins with.
const archiveMagic uint32 = 0x8199e26d

// archiveTerminator marks the end of a namespace block or the end of
// the archive (two terminators in a row).
const archiveTerminator uint32 = 0xFFFFFFFF

// archiveReader walks a mongodump archive stream:
//
//	[4-byte magic][prelude BSON][ block ... ]*[terminator][terminator]
//
// where each block is:
//
//	[namespace header BSON][document BSON]*[terminator]
//
// The header BSON carries `ns` (and metadata we don't currently
// replay); subsequent BSON docs (until the terminator) belong to that
// namespace.
type archiveReader struct {
	r       io.Reader
	curNs   string
	atEnd   bool
	inBlock bool
}

// collMeta is one prelude CollectionMetadata entry: the namespace plus
// its metadata blob (an extended-JSON string carrying index defs,
// collection options, and the collection type).
type collMeta struct {
	DB         string `bson:"db"`
	Collection string `bson:"collection"`
	Metadata   string `bson:"metadata"`
}

// skip reports whether this prelude entry should be ignored by the
// restore: views and timeseries aren't plain collections — the wire
// fallback can't recreate them via insert+rename (mongorestore is the
// path for those).
func (m collMeta) skip() bool {
	if m.DB == "" || m.Collection == "" {
		return true
	}
	if m.Metadata == "" {
		return false
	}
	var t struct {
		Type string `bson:"type"`
	}
	if err := bson.UnmarshalExtJSON([]byte(m.Metadata), false, &t); err != nil {
		return false // unparseable metadata: still restore the data
	}
	return t.Type != "" && t.Type != "collection"
}

func (a *archiveReader) readHeader() ([]collMeta, error) {
	var magic uint32
	if err := binary.Read(a.r, binary.LittleEndian, &magic); err != nil {
		return nil, err
	}
	if magic != archiveMagic {
		return nil, fmt.Errorf("not a mongodump archive (magic %#x; want %#x)", magic, archiveMagic)
	}
	// Prelude wire format:
	//
	//   [Header BSON: {concurrent_collections, server_version, ...}]
	//   [CollectionMetadata BSON for ns1]
	//   [CollectionMetadata BSON for ns2]
	//   ...
	//   [TERMINATOR (0xFFFFFFFF)]
	//
	// Every doc up to the terminator must be consumed so the body-stream
	// parser sees the first NamespaceHeader exactly where it should.
	// Reading one BSON doc and stopping (the old bug) misclassified
	// subsequent CollectionMetadata BSONs as namespace headers and
	// body documents, doubling rows for archives with >1 collection.
	// The metadata docs are parsed (not just skipped) — they carry the
	// index definitions replayIndexes re-creates after the load.
	if _, err := readBSONDoc(a.r); err != nil {
		return nil, fmt.Errorf("prelude header doc: %w", err)
	}
	var metas []collMeta
	for {
		var head [4]byte
		if _, err := io.ReadFull(a.r, head[:]); err != nil {
			return nil, fmt.Errorf("prelude metadata: %w", err)
		}
		sz := binary.LittleEndian.Uint32(head[:])
		if sz == archiveTerminator {
			return metas, nil
		}
		if sz < 5 {
			return nil, fmt.Errorf("invalid prelude BSON length %d", sz)
		}
		body := make([]byte, sz-4)
		if _, err := io.ReadFull(a.r, body); err != nil {
			return nil, fmt.Errorf("prelude metadata body: %w", err)
		}
		raw := make([]byte, sz)
		copy(raw[:4], head[:])
		copy(raw[4:], body)
		var m collMeta
		if err := bson.Unmarshal(raw, &m); err != nil {
			return nil, fmt.Errorf("prelude metadata bson: %w", err)
		}
		metas = append(metas, m)
	}
}

// next returns the next document in the archive along with its
// fully-qualified namespace, or terminate=true when the archive ends.
func (a *archiveReader) next() (ns string, doc bson.Raw, terminate bool, err error) {
	if a.atEnd {
		return "", nil, true, nil
	}
	for {
		// Peek the next 4 bytes: either a terminator (0xFFFFFFFF) or
		// the length prefix of a BSON document.
		var head [4]byte
		if _, err := io.ReadFull(a.r, head[:]); err != nil {
			if errors.Is(err, io.EOF) {
				a.atEnd = true
				return "", nil, true, nil
			}
			return "", nil, false, err
		}
		sz := binary.LittleEndian.Uint32(head[:])
		if sz == archiveTerminator {
			// End of a namespace block; the next iteration peeks
			// again to see whether the archive ended (another
			// terminator → EOF) or a new block starts.
			a.inBlock = false
			a.curNs = ""
			continue
		}
		if sz < 5 {
			return "", nil, false, fmt.Errorf("invalid BSON length %d", sz)
		}
		body := make([]byte, sz-4)
		if _, err := io.ReadFull(a.r, body); err != nil {
			return "", nil, false, err
		}
		raw := make([]byte, sz)
		copy(raw[:4], head[:])
		copy(raw[4:], body)

		if !a.inBlock {
			// Namespace header: extract db + collection and start the
			// block. mongo-tools encodes namespace headers as separate
			// `db` and `collection` fields (NOT a combined `ns`); an
			// optional EOF flag closes a namespace's stream without
			// any body documents to follow.
			var meta struct {
				DB         string `bson:"db"`
				Collection string `bson:"collection"`
				EOF        bool   `bson:"EOF"`
			}
			if uerr := bson.Unmarshal(raw, &meta); uerr != nil {
				return "", nil, false, fmt.Errorf("namespace header bson: %w", uerr)
			}
			if meta.EOF {
				// EOF marker for an empty namespace block; reset and
				// peek the next item without entering the body state.
				a.curNs = ""
				continue
			}
			if meta.DB == "" || meta.Collection == "" {
				return "", nil, false, fmt.Errorf("namespace header missing db/collection: %s.%s", meta.DB, meta.Collection)
			}
			a.curNs = meta.DB + "." + meta.Collection
			a.inBlock = true
			continue
		}
		return a.curNs, raw, false, nil
	}
}

// readBSONDoc reads one length-prefixed BSON document from r and
// returns its raw bytes (including the length prefix). Used for the
// archive prelude which we skip after parsing the magic.
func readBSONDoc(r io.Reader) (bson.Raw, error) {
	var head [4]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return nil, err
	}
	sz := binary.LittleEndian.Uint32(head[:])
	if sz < 5 {
		return nil, fmt.Errorf("invalid BSON length %d", sz)
	}
	body := make([]byte, sz-4)
	if _, err := io.ReadFull(r, body); err != nil {
		return nil, err
	}
	out := make([]byte, sz)
	copy(out[:4], head[:])
	copy(out[4:], body)
	return out, nil
}

// splitNamespace splits a `db.collection` namespace into its two
// pieces. Returns ("", "") for malformed inputs so the caller can
// skip them without erroring.
func splitNamespace(ns string) (string, string) {
	for i := range len(ns) {
		if ns[i] == '.' {
			return ns[:i], ns[i+1:]
		}
	}
	return "", ""
}
