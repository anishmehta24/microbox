// Package imagetest is a registry in memory, for testing pulls without the
// network. It speaks enough of the distribution API for image.Pull: Bearer
// token auth, an OCI index pointing to per-platform manifests, and blobs.
package imagetest

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

const token = "test-token"

// Registry serves images added with Add.
type Registry struct {
	*httptest.Server
	mu       sync.Mutex
	blobs    map[string][]byte // by digest; manifests too
	tags     map[string]string // repo:tag -> digest of the index
	corrupt  map[string]bool   // digests served with a byte changed
	Requests []string          // "METHOD path" of each authorized request
}

// NewRegistry starts a registry on 127.0.0.1, which image.Pull talks to over
// plain HTTP.
func NewRegistry(t *testing.T) *Registry {
	r := &Registry{blobs: map[string][]byte{}, tags: map[string]string{}, corrupt: map[string]bool{}}
	r.Server = httptest.NewServer(http.HandlerFunc(r.serve))
	t.Cleanup(r.Close)
	return r
}

// Host is the registry's host:port, the first part of its image references.
func (r *Registry) Host() string { return strings.TrimPrefix(r.URL, "http://") }

// Add stores an image built for linux on this machine's architecture, with
// a manifest for another platform next to it in the index, and returns the
// digests of its layers.
func (r *Registry) Add(repo, tag string, config any, layers ...[]byte) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	cfg, _ := json.Marshal(map[string]any{"architecture": runtime.GOARCH, "os": "linux", "config": config})
	m := map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.manifest.v1+json",
		"config":        r.put(cfg, "application/vnd.oci.image.config.v1+json"),
	}
	var ls []any
	var digests []string
	for _, l := range layers {
		d := r.put(l, "application/vnd.oci.image.layer.v1.tar+gzip")
		ls = append(ls, d)
		digests = append(digests, d["digest"].(string))
	}
	m["layers"] = ls
	mb, _ := json.Marshal(m)
	native := r.put(mb, "application/vnd.oci.image.manifest.v1+json")
	native["platform"] = map[string]string{"os": "linux", "architecture": runtime.GOARCH}
	other := r.put([]byte(`{"schemaVersion":2}`), "application/vnd.oci.image.manifest.v1+json")
	other["platform"] = map[string]string{"os": "linux", "architecture": "riscv64"}
	ib, _ := json.Marshal(map[string]any{
		"schemaVersion": 2,
		"mediaType":     "application/vnd.oci.image.index.v1+json",
		"manifests":     []any{other, native},
	})
	r.tags[repo+":"+tag] = r.put(ib, "")["digest"].(string)
	return digests
}

// Corrupt makes the registry serve a blob with one byte changed.
func (r *Registry) Corrupt(digest string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.corrupt[digest] = true
}

// Count returns how many requests started with prefix, e.g. "GET /v2/app/blobs/".
func (r *Registry) Count(prefix string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, req := range r.Requests {
		if strings.HasPrefix(req, prefix) {
			n++
		}
	}
	return n
}

func (r *Registry) put(b []byte, mediaType string) map[string]any {
	sum := sha256.Sum256(b)
	d := "sha256:" + hex.EncodeToString(sum[:])
	r.blobs[d] = b
	return map[string]any{"mediaType": mediaType, "digest": d, "size": len(b)}
}

func (r *Registry) serve(w http.ResponseWriter, req *http.Request) {
	if req.URL.Path == "/token" {
		json.NewEncoder(w).Encode(map[string]string{"token": token})
		return
	}
	if req.Header.Get("Authorization") != "Bearer "+token {
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer realm="%s/token",service="test",scope="repository:x:pull"`, r.URL))
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.Requests = append(r.Requests, req.Method+" "+req.URL.Path)
	// /v2/<repo>/manifests/<ref> or /v2/<repo>/blobs/<digest>
	rest := strings.TrimPrefix(req.URL.Path, "/v2/")
	i := strings.LastIndex(rest, "/")
	j := strings.LastIndex(rest[:max(i, 0)], "/")
	if i < 0 || j < 0 {
		http.NotFound(w, req)
		return
	}
	repo, kind, ref := rest[:j], rest[j+1:i], rest[i+1:]
	digest := ref
	if kind == "manifests" && !strings.HasPrefix(ref, "sha256:") {
		digest = r.tags[repo+":"+ref]
	}
	b, ok := r.blobs[digest]
	if !ok {
		http.NotFound(w, req)
		return
	}
	if r.corrupt[digest] {
		b = bytes.Clone(b)
		b[len(b)/2] ^= 0xff
	}
	if kind == "manifests" {
		var m struct{ MediaType string }
		json.Unmarshal(b, &m)
		w.Header().Set("Content-Type", m.MediaType)
		w.Header().Set("Docker-Content-Digest", digest)
	}
	w.Header().Set("Content-Length", fmt.Sprint(len(b)))
	if req.Method != http.MethodHead {
		w.Write(b)
	}
}

// Entry is one file in a layer built by Layer.
type Entry struct {
	Name     string
	Type     byte // tar.TypeReg if zero
	Body     string
	Linkname string
	Mode     int64 // 0644, or 0755 for directories, if zero
	Uid, Gid int
}

// Layer builds a gzipped tar from entries.
func Layer(t *testing.T, entries ...Entry) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	for _, e := range entries {
		h := &tar.Header{Name: e.Name, Typeflag: e.Type, Linkname: e.Linkname, Mode: e.Mode,
			Uid: e.Uid, Gid: e.Gid, Size: int64(len(e.Body)), ModTime: time.Unix(1700000000, 0)}
		if h.Typeflag == 0 {
			h.Typeflag = tar.TypeReg
		}
		if h.Typeflag != tar.TypeReg {
			h.Size = 0
		}
		if h.Mode == 0 {
			h.Mode = 0o644
			if h.Typeflag == tar.TypeDir {
				h.Mode = 0o755
			}
		}
		if err := tw.WriteHeader(h); err != nil {
			t.Fatal(err)
		}
		if h.Typeflag == tar.TypeReg {
			if _, err := tw.Write([]byte(e.Body)); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// DirLayer builds a gzipped tar of a directory tree, such as a root
// filesystem, keeping owners, modes and symlinks.
func DirLayer(t *testing.T, dir string) []byte {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(zw)
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, err error) error {
		if err != nil || path == dir {
			return err
		}
		fi, err := d.Info()
		if err != nil {
			return err
		}
		var link string
		if fi.Mode()&fs.ModeSymlink != 0 {
			if link, err = os.Readlink(path); err != nil {
				return err
			}
		}
		h, err := tar.FileInfoHeader(fi, link)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, path)
		h.Name = filepath.ToSlash(rel)
		if fi.IsDir() {
			h.Name += "/"
		}
		if err := tw.WriteHeader(h); err != nil {
			return err
		}
		if fi.Mode().IsRegular() {
			b, err := os.ReadFile(path)
			if err != nil {
				return err
			}
			_, err = tw.Write(b)
			return err
		}
		return nil
	})
	if err == nil {
		err = tw.Close()
	}
	if err == nil {
		err = zw.Close()
	}
	if err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}
