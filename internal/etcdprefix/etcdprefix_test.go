package etcdprefix

import (
	"bytes"
	"context"
	"sort"
	"strings"
	"testing"

	"filippo.io/age"
	"go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// memKV is an in-memory etcd KV covering ranges, limits, counts and puts.
type memKV struct {
	data map[string]string
	rev  int64
}

func (m *memKV) keys(op clientv3.Op) []string {
	from, end := string(op.KeyBytes()), string(op.RangeBytes())
	var out []string
	for k := range m.data {
		if (end == "" && k == from) || (end != "" && k >= from && (end == "\x00" || k < end)) {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}

func (m *memKV) Get(_ context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error) {
	op := clientv3.OpGet(key, opts...)
	keys := m.keys(op)
	r := &clientv3.GetResponse{Header: &etcdserverpb.ResponseHeader{Revision: m.rev}, Count: int64(len(keys))}
	if op.IsCountOnly() {
		return r, nil
	}
	if l := int(op.Limit()); l > 0 && len(keys) > l {
		keys, r.More = keys[:l], true
	}
	for _, k := range keys {
		r.Kvs = append(r.Kvs, &mvccpb.KeyValue{Key: []byte(k), Value: []byte(m.data[k])})
	}
	return r, nil
}

func (m *memKV) Delete(_ context.Context, key string, opts ...clientv3.OpOption) (*clientv3.DeleteResponse, error) {
	for _, k := range m.keys(clientv3.OpDelete(key, opts...)) {
		delete(m.data, k)
	}
	m.rev++
	return &clientv3.DeleteResponse{}, nil
}

func (m *memKV) Txn(context.Context) clientv3.Txn { return &memTxn{m: m} }

type memTxn struct {
	m   *memKV
	ops []clientv3.Op
}

func (t *memTxn) If(...clientv3.Cmp) clientv3.Txn { return t }
func (t *memTxn) Then(ops ...clientv3.Op) clientv3.Txn {
	t.ops = ops
	return t
}
func (t *memTxn) Else(...clientv3.Op) clientv3.Txn { return t }
func (t *memTxn) Commit() (*clientv3.TxnResponse, error) {
	for _, op := range t.ops {
		t.m.data[string(op.KeyBytes())] = string(op.ValueBytes())
	}
	t.m.rev++
	return &clientv3.TxnResponse{Succeeded: true}, nil
}

func TestDumpLoadKeepsValuesAndIsolatesPrefixes(t *testing.T) {
	src := &memKV{data: map[string]string{"/other/registry/secret": "foreign"}, rev: 7}
	for i := 0; i < 1234; i++ {
		src.data["/old/registry/obj/"+strings.Repeat("x", i%7)+string(rune('a'+i%26))+string(rune('a'+i/26%26))+string(rune('a'+i/676))] = "uid-preserving-value"
	}
	s, err := Dump(context.Background(), src, "/old")
	if err != nil {
		t.Fatal(err)
	}
	if s.Revision != 7 || s.Prefix != "/old/" {
		t.Fatal(s.Revision, s.Prefix)
	}
	for _, kv := range s.KVs {
		if strings.Contains(kv.Key, "other") || string(kv.Value) == "foreign" {
			t.Fatal("another tenant's key leaked into the snapshot")
		}
	}
	dst := &memKV{data: map[string]string{"/new/registry/fresh": "bootstrap", "/neighbour/keep": "x"}}
	if _, err = Load(context.Background(), dst, s, "/new", false); err == nil {
		t.Fatal("non-empty target accepted without replace")
	}
	n, err := Load(context.Background(), dst, s, "/new/", true)
	if err != nil || n != len(s.KVs) {
		t.Fatal(n, err)
	}
	if _, ok := dst.data["/new/registry/fresh"]; ok {
		t.Fatal("replace must clear the target prefix")
	}
	if dst.data["/neighbour/keep"] != "x" {
		t.Fatal("replace must not touch other prefixes")
	}
	for _, kv := range s.KVs {
		if dst.data["/new/"+kv.Key] != string(kv.Value) {
			t.Fatal("value changed for", kv.Key)
		}
	}
}

func TestSnapshotEncryptionAndChecksum(t *testing.T) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		t.Fatal(err)
	}
	src := &memKV{data: map[string]string{"/p/a": "1", "/p/b": "2"}, rev: 3}
	s, err := Dump(context.Background(), src, "/p")
	if err != nil {
		t.Fatal(err)
	}
	sealed, err := Encrypt(s, id.Recipient().String())
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(sealed, []byte(`"/p/"`)) {
		t.Fatal("snapshot is not encrypted")
	}
	got, err := Decrypt(sealed, strings.NewReader(id.String()))
	if err != nil || got.SHA256 != s.SHA256 || len(got.KVs) != 2 {
		t.Fatal(got, err)
	}
	got.KVs[0].Value = []byte("tampered")
	if _, err = Load(context.Background(), &memKV{data: map[string]string{}}, got, "/q", false); err == nil {
		t.Fatal("tampered snapshot accepted")
	}
	if _, err = Dump(context.Background(), src, "/missing"); err == nil {
		t.Fatal("empty prefix must fail: wrong prefix or credentials")
	}
	if _, err = Dump(context.Background(), src, "relative"); err == nil {
		t.Fatal("relative prefix accepted")
	}
}
