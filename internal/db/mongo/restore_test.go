package mongo

import (
	"bytes"
	"encoding/binary"
	"testing"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// TestArchiveReader_BasicStream constructs a minimal mongodump archive
// in memory and walks it through archiveReader, verifying that the
// magic + prelude are consumed and that each document is returned with
// the correct namespace. This pins the wire-protocol fallback's load-
// bearing piece: the parser the driver-side restore feeds documents
// from. No docker/mongo needed — pure byte stream.
func TestArchiveReader_BasicStream(t *testing.T) {
	var buf bytes.Buffer
	// Magic.
	if err := binary.Write(&buf, binary.LittleEndian, archiveMagic); err != nil {
		t.Fatal(err)
	}
	// Prelude (a real mongodump archive's prelude is one Header BSON
	// followed by N CollectionMetadata BSONs, terminated by 0xFFFFFFFF).
	// The minimal valid form: just the header + terminator.
	preludeBSON, err := bson.Marshal(bson.M{"concurrent_collections": int32(1)})
	if err != nil {
		t.Fatal(err)
	}
	buf.Write(preludeBSON)
	if err := binary.Write(&buf, binary.LittleEndian, archiveTerminator); err != nil {
		t.Fatal(err)
	}
	// Namespace header: identifies the next block as testdb.coll.
	nsBSON, err := bson.Marshal(bson.M{"db": "testdb", "collection": "coll"})
	if err != nil {
		t.Fatal(err)
	}
	buf.Write(nsBSON)
	// One document under that namespace.
	docBSON, err := bson.Marshal(bson.M{"_id": int32(1), "name": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	buf.Write(docBSON)
	// Block terminator → end of this namespace's documents.
	if err := binary.Write(&buf, binary.LittleEndian, archiveTerminator); err != nil {
		t.Fatal(err)
	}
	// Archive terminator → end of stream.
	if err := binary.Write(&buf, binary.LittleEndian, archiveTerminator); err != nil {
		t.Fatal(err)
	}

	r := &archiveReader{r: &buf}
	if _, err := r.readHeader(); err != nil {
		t.Fatalf("readHeader: %v", err)
	}

	ns, doc, terminate, nerr := r.next()
	if nerr != nil {
		t.Fatalf("next: %v", nerr)
	}
	if terminate {
		t.Fatal("unexpected early termination on first doc")
	}
	if ns != "testdb.coll" {
		t.Errorf("ns = %q, want testdb.coll", ns)
	}
	var got bson.M
	if err := bson.Unmarshal(doc, &got); err != nil {
		t.Fatalf("doc bson unmarshal: %v", err)
	}
	if got["name"] != "hello" {
		t.Errorf("doc.name = %v, want hello", got["name"])
	}

	// Next read should hit the archive terminator and report end.
	_, _, terminate2, nerr2 := r.next()
	if nerr2 != nil {
		t.Fatalf("second next: %v", nerr2)
	}
	if !terminate2 {
		t.Error("expected terminate=true after the only document")
	}
}

// TestArchiveReader_MultipleNamespaces walks two namespace blocks and
// confirms the reader keeps each document under its OWN ns — a single
// hand-crafted archive exercises the namespace switch logic without
// any external tools.
func TestArchiveReader_MultipleNamespaces(t *testing.T) {
	var buf bytes.Buffer
	mustWriteUint32(t, &buf, archiveMagic)
	mustMarshalInto(t, &buf, bson.M{"concurrent_collections": int32(2)})
	mustWriteUint32(t, &buf, archiveTerminator) // end of prelude

	// First block: db1.coll1 with two documents.
	mustMarshalInto(t, &buf, bson.M{"db": "db1", "collection": "coll1"})
	mustMarshalInto(t, &buf, bson.M{"_id": int32(1)})
	mustMarshalInto(t, &buf, bson.M{"_id": int32(2)})
	mustWriteUint32(t, &buf, archiveTerminator)

	// Second block: db2.coll2 with one document.
	mustMarshalInto(t, &buf, bson.M{"db": "db2", "collection": "coll2"})
	mustMarshalInto(t, &buf, bson.M{"_id": int32(99)})
	mustWriteUint32(t, &buf, archiveTerminator)

	// Archive end.
	mustWriteUint32(t, &buf, archiveTerminator)

	r := &archiveReader{r: &buf}
	if _, err := r.readHeader(); err != nil {
		t.Fatalf("readHeader: %v", err)
	}

	type entry struct {
		ns string
		id int32
	}
	var seen []entry
	for {
		ns, doc, terminate, err := r.next()
		if err != nil {
			t.Fatal(err)
		}
		if terminate {
			break
		}
		var v struct {
			ID int32 `bson:"_id"`
		}
		if err := bson.Unmarshal(doc, &v); err != nil {
			t.Fatal(err)
		}
		seen = append(seen, entry{ns: ns, id: v.ID})
	}
	want := []entry{
		{"db1.coll1", 1},
		{"db1.coll1", 2},
		{"db2.coll2", 99},
	}
	if len(seen) != len(want) {
		t.Fatalf("got %d docs, want %d: %+v", len(seen), len(want), seen)
	}
	for i := range want {
		if seen[i] != want[i] {
			t.Errorf("doc %d = %+v, want %+v", i, seen[i], want[i])
		}
	}
}

func TestSplitNamespace(t *testing.T) {
	cases := []struct {
		in   string
		db   string
		coll string
	}{
		{"db.coll", "db", "coll"},
		{"app_testing.users", "app_testing", "users"},
		{"db.col.with.dots", "db", "col.with.dots"},
		{"nodot", "", ""},
		{"", "", ""},
	}
	for _, tc := range cases {
		db, coll := splitNamespace(tc.in)
		if db != tc.db || coll != tc.coll {
			t.Errorf("splitNamespace(%q) = (%q,%q), want (%q,%q)", tc.in, db, coll, tc.db, tc.coll)
		}
	}
}

func mustWriteUint32(t *testing.T, w *bytes.Buffer, v uint32) {
	t.Helper()
	if err := binary.Write(w, binary.LittleEndian, v); err != nil {
		t.Fatal(err)
	}
}

func mustMarshalInto(t *testing.T, w *bytes.Buffer, v any) {
	t.Helper()
	raw, err := bson.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	w.Write(raw)
}

// TestParseIndexSpecs pins the metadata→createIndexes translation:
// `_id_` and the legacy `ns` field are stripped, everything else
// (unique, sparse, compound keys) passes through.
func TestParseIndexSpecs(t *testing.T) {
	meta := `{"indexes":[` +
		`{"v":2,"key":{"_id":1},"name":"_id_"},` +
		`{"v":2,"key":{"email":1},"name":"email_1","unique":true,"ns":"old.users"},` +
		`{"v":2,"key":{"a":1,"b":-1},"name":"a_1_b_-1"}` +
		`],"uuid":"abc","collectionName":"users","type":"collection"}`
	specs, err := parseIndexSpecs(meta)
	if err != nil {
		t.Fatal(err)
	}
	if len(specs) != 2 {
		t.Fatalf("got %d specs, want 2 (skip _id_): %+v", len(specs), specs)
	}
	for _, spec := range specs {
		for _, e := range spec {
			if e.Key == "ns" {
				t.Errorf("legacy ns field not stripped: %+v", spec)
			}
		}
	}
	if specs, err := parseIndexSpecs(""); err != nil || specs != nil {
		t.Errorf("empty metadata: got %v, %v", specs, err)
	}
	if _, err := parseIndexSpecs("{not json"); err == nil {
		t.Error("malformed metadata: want error")
	}
}

// TestCollMetaSkip: views/timeseries are skipped, plain + untyped +
// unparseable-metadata collections restore.
func TestCollMetaSkip(t *testing.T) {
	cases := []struct {
		m    collMeta
		want bool
	}{
		{collMeta{DB: "d", Collection: "c", Metadata: `{"type":"collection"}`}, false},
		{collMeta{DB: "d", Collection: "c", Metadata: `{}`}, false},
		{collMeta{DB: "d", Collection: "c", Metadata: ""}, false},
		{collMeta{DB: "d", Collection: "c", Metadata: `{"type":"view"}`}, true},
		{collMeta{DB: "d", Collection: "c", Metadata: `{"type":"timeseries"}`}, true},
		{collMeta{DB: "d", Collection: "c", Metadata: `{broken`}, false},
		{collMeta{DB: "", Collection: "c"}, true},
	}
	for i, tc := range cases {
		if got := tc.m.skip(); got != tc.want {
			t.Errorf("case %d (%+v): skip = %v, want %v", i, tc.m, got, tc.want)
		}
	}
}

// TestReadHeaderParsesCollectionMetadata: the prelude's metadata docs
// come back parsed (they feed index replay), and the body stream still
// reads from the right offset.
func TestReadHeaderParsesCollectionMetadata(t *testing.T) {
	var buf bytes.Buffer
	mustWriteUint32(t, &buf, archiveMagic)
	mustMarshalInto(t, &buf, bson.M{"concurrent_collections": int32(1)})
	mustMarshalInto(t, &buf, bson.M{
		"db": "srcdb", "collection": "users",
		"metadata": `{"indexes":[{"v":2,"key":{"email":1},"name":"email_1"}],"type":"collection"}`,
	})
	mustWriteUint32(t, &buf, archiveTerminator) // end of prelude
	mustMarshalInto(t, &buf, bson.M{"db": "srcdb", "collection": "users"})
	mustMarshalInto(t, &buf, bson.M{"_id": int32(1)})
	mustWriteUint32(t, &buf, archiveTerminator)
	mustWriteUint32(t, &buf, archiveTerminator)

	r := &archiveReader{r: &buf}
	metas, err := r.readHeader()
	if err != nil {
		t.Fatal(err)
	}
	if len(metas) != 1 || metas[0].DB != "srcdb" || metas[0].Collection != "users" {
		t.Fatalf("metas = %+v", metas)
	}
	specs, err := parseIndexSpecs(metas[0].Metadata)
	if err != nil || len(specs) != 1 {
		t.Fatalf("index specs from metadata: %v, %v", specs, err)
	}
	ns, _, terminate, err := r.next()
	if err != nil || terminate || ns != "srcdb.users" {
		t.Fatalf("body misaligned after metadata parse: ns=%q terminate=%v err=%v", ns, terminate, err)
	}
}
