// Package etcdprefix copies the raw keys of one Kamaji tenant control plane.
//
// Kamaji stores each tenant cluster under its own etcd prefix and gives it an
// etcd user limited to that prefix. Copying the stored values verbatim keeps
// every object's UID, so a new control plane can take over from a snapshot
// without touching other tenants of a shared datastore.
package etcdprefix

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	clientv3 "go.etcd.io/etcd/client/v3"
)

const (
	pageSize = 500
	txnOps   = 64
	maxBytes = 256 << 20
)

// KV is one stored key relative to the tenant prefix.
type KV struct {
	Key   string `json:"key"`
	Value []byte `json:"value"`
}

// Snapshot is every key of one prefix read at one revision.
type Snapshot struct {
	Schema   int    `json:"schema"`
	Prefix   string `json:"prefix"`
	Revision int64  `json:"revision"`
	KVs      []KV   `json:"kvs"`
	SHA256   string `json:"sha256"`
}

// Client is the subset of the etcd client the package uses.
type Client interface {
	Get(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.GetResponse, error)
	Delete(ctx context.Context, key string, opts ...clientv3.OpOption) (*clientv3.DeleteResponse, error)
	Txn(ctx context.Context) clientv3.Txn
}

func normalize(prefix string) (string, error) {
	if !strings.HasPrefix(prefix, "/") || len(prefix) < 2 {
		return "", fmt.Errorf("etcd prefix must be an absolute key prefix such as /<schema>")
	}
	return strings.TrimSuffix(prefix, "/") + "/", nil
}

func digest(kvs []KV) string {
	h := sha256.New()
	for _, kv := range kvs {
		fmt.Fprintf(h, "%d:%s%d:", len(kv.Key), kv.Key, len(kv.Value))
		h.Write(kv.Value)
	}
	return hex.EncodeToString(h.Sum(nil))
}

// Dump reads all keys under prefix at a single revision, so the snapshot is
// consistent even while the source control plane keeps writing.
func Dump(ctx context.Context, c Client, prefix string) (Snapshot, error) {
	p, err := normalize(prefix)
	if err != nil {
		return Snapshot{}, err
	}
	end := clientv3.GetPrefixRangeEnd(p)
	s := Snapshot{Schema: 1, Prefix: p}
	from := p
	for {
		opts := []clientv3.OpOption{clientv3.WithRange(end), clientv3.WithLimit(pageSize), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend)}
		if s.Revision > 0 {
			opts = append(opts, clientv3.WithRev(s.Revision))
		}
		r, err := c.Get(ctx, from, opts...)
		if err != nil {
			return Snapshot{}, err
		}
		if s.Revision == 0 {
			s.Revision = r.Header.Revision
		}
		for _, kv := range r.Kvs {
			s.KVs = append(s.KVs, KV{Key: strings.TrimPrefix(string(kv.Key), p), Value: kv.Value})
		}
		if !r.More || len(r.Kvs) == 0 {
			break
		}
		from = string(r.Kvs[len(r.Kvs)-1].Key) + "\x00"
	}
	if len(s.KVs) == 0 {
		return Snapshot{}, fmt.Errorf("no keys under %s: wrong prefix or credentials", p)
	}
	s.SHA256 = digest(s.KVs)
	return s, nil
}

// Load writes a snapshot under target. A target that already holds keys is
// refused unless replace is set, which first deletes everything under target.
func Load(ctx context.Context, c Client, s Snapshot, target string, replace bool) (int, error) {
	if s.SHA256 != digest(s.KVs) {
		return 0, fmt.Errorf("snapshot checksum mismatch")
	}
	p, err := normalize(target)
	if err != nil {
		return 0, err
	}
	existing, err := c.Get(ctx, p, clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		return 0, err
	}
	if existing.Count > 0 {
		if !replace {
			return 0, fmt.Errorf("%s holds %d keys; restore into a new control plane with --replace after stopping its API server writes", p, existing.Count)
		}
		if _, err = c.Delete(ctx, p, clientv3.WithPrefix()); err != nil {
			return 0, err
		}
	}
	written := 0
	for start := 0; start < len(s.KVs); start += txnOps {
		stop := min(start+txnOps, len(s.KVs))
		ops := make([]clientv3.Op, 0, stop-start)
		for _, kv := range s.KVs[start:stop] {
			ops = append(ops, clientv3.OpPut(p+kv.Key, string(kv.Value)))
		}
		if _, err = c.Txn(ctx).Then(ops...).Commit(); err != nil {
			return written, err
		}
		written += len(ops)
	}
	return written, nil
}

// Encrypt seals a snapshot for an age X25519 recipient.
func Encrypt(s Snapshot, recipient string) ([]byte, error) {
	r, err := age.ParseX25519Recipient(recipient)
	if err != nil {
		return nil, fmt.Errorf("age recipient: %w", err)
	}
	plain, err := json.Marshal(s)
	if err != nil {
		return nil, err
	}
	var out bytes.Buffer
	w, err := age.Encrypt(&out, r)
	if err != nil {
		return nil, err
	}
	if _, err = w.Write(plain); err != nil {
		return nil, err
	}
	if err = w.Close(); err != nil {
		return nil, err
	}
	if out.Len() > maxBytes {
		return nil, fmt.Errorf("encrypted snapshot exceeds 256 MiB")
	}
	return out.Bytes(), nil
}

// Decrypt opens a snapshot with the offline recovery identity.
func Decrypt(ciphertext []byte, identityFile io.Reader) (Snapshot, error) {
	ids, err := age.ParseIdentities(identityFile)
	if err != nil {
		return Snapshot{}, err
	}
	r, err := age.Decrypt(bytes.NewReader(ciphertext), ids...)
	if err != nil {
		return Snapshot{}, err
	}
	b, err := io.ReadAll(io.LimitReader(r, maxBytes+1))
	if err != nil {
		return Snapshot{}, err
	}
	if len(b) > maxBytes {
		return Snapshot{}, fmt.Errorf("decrypted snapshot exceeds 256 MiB")
	}
	var s Snapshot
	if err = json.Unmarshal(b, &s); err != nil {
		return Snapshot{}, err
	}
	if s.Schema != 1 {
		return Snapshot{}, fmt.Errorf("unsupported snapshot schema %d", s.Schema)
	}
	return s, nil
}
