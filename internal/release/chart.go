package release

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	v1 "github.com/google/go-containerregistry/pkg/v1"
	"github.com/google/go-containerregistry/pkg/v1/empty"
	"github.com/google/go-containerregistry/pkg/v1/layout"
	"github.com/google/go-containerregistry/pkg/v1/types"
	"sigs.k8s.io/yaml"
)

// PackageChart constructs a deterministic Helm OCI artifact from the working
// tree. Local chart changes therefore never inherit an older published digest.
func PackageChart(root, dir string) (Artifact, error) {
	b, e := os.ReadFile(filepath.Join(dir, "Chart.yaml"))
	if e != nil {
		return Artifact{}, e
	}
	var meta map[string]any
	if e = yaml.Unmarshal(b, &meta); e != nil {
		return Artifact{}, e
	}
	n, _ := meta["name"].(string)
	version, _ := meta["version"].(string)
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	e = filepath.Walk(dir, func(p string, i os.FileInfo, e error) error {
		if e != nil {
			return e
		}
		if i.IsDir() {
			return nil
		}
		if !i.Mode().IsRegular() {
			return fmt.Errorf("non-regular chart entry %s", p)
		}
		rel, e := filepath.Rel(dir, p)
		if e != nil {
			return e
		}
		data, e := os.ReadFile(p)
		if e != nil {
			return e
		}
		if e = tw.WriteHeader(&tar.Header{Name: n + "/" + filepath.ToSlash(rel), Mode: 0644, Size: int64(len(data))}); e != nil {
			return e
		}
		_, e = tw.Write(data)
		return e
	})
	if e != nil {
		return Artifact{}, e
	}
	if e = tw.Close(); e != nil {
		return Artifact{}, e
	}
	if e = gz.Close(); e != nil {
		return Artifact{}, e
	}
	path := "release/artifacts"
	abs := filepath.Join(root, path)
	if e = os.MkdirAll(filepath.Join(abs, "blobs/sha256"), 0755); e != nil {
		return Artifact{}, e
	}
	lp, e := layout.FromPath(abs)
	if e != nil {
		lp, e = layout.Write(abs, empty.Index)
	}
	if e != nil {
		return Artifact{}, e
	}
	write := func(data []byte, media types.MediaType) (v1.Descriptor, error) {
		sum := sha256.Sum256(data)
		hash := fmt.Sprintf("%x", sum)
		e := os.WriteFile(filepath.Join(abs, "blobs/sha256", hash), data, 0644)
		return v1.Descriptor{MediaType: media, Digest: v1.Hash{Algorithm: "sha256", Hex: hash}, Size: int64(len(data))}, e
	}
	config, e := json.Marshal(meta)
	if e != nil {
		return Artifact{}, e
	}
	cd, e := write(config, "application/vnd.cncf.helm.config.v1+json")
	if e != nil {
		return Artifact{}, e
	}
	ld, e := write(buf.Bytes(), "application/vnd.cncf.helm.chart.content.v1.tar+gzip")
	if e != nil {
		return Artifact{}, e
	}
	ld.Annotations = map[string]string{"org.opencontainers.image.title": n + "-" + version + ".tgz"}
	manifest := v1.Manifest{SchemaVersion: 2, MediaType: types.OCIManifestSchema1, Config: cd, Layers: []v1.Descriptor{ld}}
	raw, e := json.Marshal(manifest)
	if e != nil {
		return Artifact{}, e
	}
	md, e := write(raw, types.OCIManifestSchema1)
	if e != nil {
		return Artifact{}, e
	}
	idx, e := lp.ImageIndex()
	if e != nil {
		return Artifact{}, e
	}
	im, e := idx.IndexManifest()
	if e != nil {
		return Artifact{}, e
	}
	found := false
	for _, d := range im.Manifests {
		if d.Digest == md.Digest {
			found = true
		}
	}
	if !found {
		im.Manifests = append(im.Manifests, md)
	}
	ib, e := json.Marshal(im)
	if e != nil {
		return Artifact{}, e
	}
	if e = os.WriteFile(filepath.Join(abs, "index.json"), ib, 0644); e != nil {
		return Artifact{}, e
	}

	base := filepath.Base(filepath.Dir(dir))
	return Artifact{Kind: "chart", Ref: "oci://ghcr.io/tym83/aep/" + base + "/" + n, Version: version, Digest: md.Digest.String(), Layout: path}, nil
}

// Keep generated layouts limited to the current inventory, so repeated generation
// does not retain stale chart packages or grow the committed release indefinitely.
func compactLayout(root string, m Manifest) error {
	path := filepath.Join(root, "release/artifacts")
	keep := map[string]bool{}
	index := v1.IndexManifest{SchemaVersion: 2, MediaType: types.OCIImageIndex}
	for _, a := range m.Artifacts {
		if a.Layout != "release/artifacts" {
			continue
		}
		h, e := v1.NewHash(a.Digest)
		if e != nil {
			return e
		}
		raw, e := os.ReadFile(filepath.Join(path, "blobs/sha256", h.Hex))
		if e != nil {
			return e
		}
		var manifest v1.Manifest
		if e = json.Unmarshal(raw, &manifest); e != nil {
			return e
		}
		keep[h.Hex] = true
		keep[manifest.Config.Digest.Hex] = true
		for _, d := range manifest.Layers {
			keep[d.Digest.Hex] = true
		}
		index.Manifests = append(index.Manifests, v1.Descriptor{Digest: h, Size: int64(len(raw)), MediaType: manifest.MediaType})
	}
	b, e := json.Marshal(index)
	if e != nil {
		return e
	}
	if e = os.WriteFile(filepath.Join(path, "index.json"), b, 0644); e != nil {
		return e
	}
	entries, e := os.ReadDir(filepath.Join(path, "blobs/sha256"))
	if e != nil {
		return e
	}
	for _, entry := range entries {
		if !entry.IsDir() && !keep[entry.Name()] {
			if e = os.Remove(filepath.Join(path, "blobs/sha256", entry.Name())); e != nil {
				return e
			}
		}
	}
	return nil
}
