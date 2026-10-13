package image

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// Media types of manifests. Registries serve Docker's and OCI's, which have
// the same structure.
const (
	ociIndex       = "application/vnd.oci.image.index.v1+json"
	dockerList     = "application/vnd.docker.distribution.manifest.list.v2+json"
	ociManifest    = "application/vnd.oci.image.manifest.v1+json"
	dockerManifest = "application/vnd.docker.distribution.manifest.v2+json"
)

var manifestTypes = strings.Join([]string{ociIndex, dockerList, ociManifest, dockerManifest}, ", ")

// descriptor points to a blob or manifest by its digest.
type descriptor struct {
	MediaType string    `json:"mediaType"`
	Digest    string    `json:"digest"`
	Size      int64     `json:"size"`
	Platform  *platform `json:"platform,omitempty"`
}

type platform struct {
	OS           string `json:"os"`
	Architecture string `json:"architecture"`
}

// manifest is an index (one manifest per platform) or a single platform's
// manifest (a config and layers); MediaType says which.
type manifest struct {
	MediaType string       `json:"mediaType"`
	Manifests []descriptor `json:"manifests"`
	Config    descriptor   `json:"config"`
	Layers    []descriptor `json:"layers"`
}

// Pull downloads an image and its layers, which it unpacks under Dir, and
// writes the image's record. It reports progress to out.
//
// The registry API (the OCI distribution spec) is a few GET requests:
//
//  1. /v2/<repo>/manifests/<tag>: an index for an image built for several
//     platforms, from which Pull takes linux on this machine's
//     architecture, then that manifest by its digest. Or a manifest right
//     away.
//  2. /v2/<repo>/blobs/<digest> for the config, which holds the default
//     command and environment, and for each layer, a gzipped tar.
//
// Everything is fetched by digest except the first manifest, and every
// digest is checked, so a registry or a proxy in between can't swap content.
func Pull(ctx context.Context, name string, out io.Writer) (*Image, error) {
	ref, err := ParseReference(name)
	if err != nil {
		return nil, err
	}
	c := &client{ref: ref, http: &http.Client{Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		ResponseHeaderTimeout: 30 * time.Second,
	}}}

	// A HEAD request gets the tag's digest without counting as a pull on
	// Docker Hub, whose anonymous pulls are rate-limited.
	tagDigest, err := c.headManifest(ctx, ref.Tag)
	if err != nil {
		return nil, err
	}
	if old, err := Lookup(name); err == nil && tagDigest != "" && old.TagDigest == tagDigest && old.complete() {
		fmt.Fprintf(out, "%s is up to date\n", ref)
		return old, nil
	}

	body, err := c.getManifest(ctx, ref.Tag, tagDigest)
	if err != nil {
		return nil, err
	}
	if tagDigest == "" {
		tagDigest = digestOf(body)
	}
	var m manifest
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if m.MediaType == ociIndex || m.MediaType == dockerList || len(m.Manifests) > 0 {
		d, err := choosePlatform(m.Manifests)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", ref, err)
		}
		if body, err = c.getManifest(ctx, d.Digest, d.Digest); err != nil {
			return nil, err
		}
		m = manifest{}
		if err := json.Unmarshal(body, &m); err != nil {
			return nil, fmt.Errorf("manifest: %w", err)
		}
	}
	if len(m.Layers) == 0 {
		return nil, fmt.Errorf("%s: the manifest has no layers", ref)
	}

	var cfg struct {
		Config Config `json:"config"`
	}
	b, err := c.getBlob(ctx, m.Config, 1<<20)
	if err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}

	img := &Image{Ref: ref.String(), TagDigest: tagDigest, ID: m.Config.Digest, Config: cfg.Config, Pulled: time.Now()}
	for _, l := range m.Layers {
		if err := c.pullLayer(ctx, l, out); err != nil {
			return nil, fmt.Errorf("layer %s: %w", short(l.Digest), err)
		}
		img.Layers = append(img.Layers, l.Digest)
		img.Size += l.Size
	}
	if err := img.save(); err != nil {
		return nil, err
	}
	fmt.Fprintf(out, "pulled %s (%s)\n", ref, short(img.ID))
	return img, nil
}

// choosePlatform picks linux on this machine's architecture.
func choosePlatform(ms []descriptor) (descriptor, error) {
	for _, d := range ms {
		if d.Platform != nil && d.Platform.OS == "linux" && d.Platform.Architecture == runtime.GOARCH {
			return d, nil
		}
	}
	return descriptor{}, fmt.Errorf("no image for linux/%s", runtime.GOARCH)
}

// pullLayer downloads a layer into a temporary file, checks its digest, and
// only then unpacks it: nothing from an unchecked download is parsed. It
// unpacks into a temporary directory and renames that into place, so a
// layer directory is always complete.
func (c *client) pullLayer(ctx context.Context, l descriptor, out io.Writer) error {
	dir := layerDir(l.Digest)
	if _, err := os.Stat(dir); err == nil {
		fmt.Fprintf(out, "%s already exists\n", short(l.Digest))
		return nil
	}
	var gzipped bool
	switch l.MediaType {
	case "application/vnd.oci.image.layer.v1.tar+gzip", "application/vnd.docker.image.rootfs.diff.tar.gzip":
		gzipped = true
	case "application/vnd.oci.image.layer.v1.tar":
	default:
		return fmt.Errorf("unsupported layer type %s", l.MediaType)
	}
	layers := filepath.Dir(dir)
	if err := os.MkdirAll(layers, 0o755); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s pulling %s\n", short(l.Digest), formatSize(l.Size))

	f, err := os.CreateTemp(layers, ".tmp-download-")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	if err := c.downloadBlob(ctx, l, f); err != nil {
		return err
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	var tarStream io.Reader = f
	if gzipped {
		zr, err := gzip.NewReader(f)
		if err != nil {
			return err
		}
		defer zr.Close()
		tarStream = zr
	}

	tmp, err := os.MkdirTemp(layers, ".tmp-"+short(l.Digest)+"-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(tmp) // a no-op after the rename
	// MkdirTemp makes it 0700; the layer's root becomes / in a container.
	if err := os.Chmod(tmp, 0o755); err != nil {
		return err
	}
	if err := unpack(tarStream, tmp); err != nil {
		return err
	}
	if err := os.Rename(tmp, dir); err != nil {
		// Another pull may have unpacked the same layer meanwhile. Then that
		// copy stays.
		if _, serr := os.Stat(dir); serr == nil {
			return nil
		}
		return err
	}
	return nil
}

// client talks to one repository of a registry.
type client struct {
	ref   Reference
	http  *http.Client
	token string
}

func (c *client) url(kind, ref string) string {
	return fmt.Sprintf("%s://%s/v2/%s/%s/%s", c.ref.scheme(), c.ref.Registry, c.ref.Repo, kind, ref)
}

// do sends a request, and if the registry answers 401 with a Bearer
// challenge, gets an anonymous token from the server it names and retries.
// That is how Docker Hub works even for public images.
func (c *client) do(ctx context.Context, method, u, accept string) (*http.Response, error) {
	for try := 0; ; try++ {
		req, err := http.NewRequestWithContext(ctx, method, u, nil)
		if err != nil {
			return nil, err
		}
		if accept != "" {
			req.Header.Set("Accept", accept)
		}
		if c.token != "" {
			req.Header.Set("Authorization", "Bearer "+c.token)
		}
		resp, err := c.http.Do(req)
		if err != nil {
			return nil, err
		}
		if resp.StatusCode == http.StatusUnauthorized && try == 0 {
			challenge := resp.Header.Get("WWW-Authenticate")
			resp.Body.Close()
			if c.token, err = c.fetchToken(ctx, challenge); err != nil {
				return nil, err
			}
			continue
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			return nil, fmt.Errorf("%s %s: %s", method, u, resp.Status)
		}
		return resp, nil
	}
}

// fetchToken follows a challenge like
//
//	Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull"
//
// with GET realm?service=...&scope=..., which answers {"token": "..."}.
func (c *client) fetchToken(ctx context.Context, challenge string) (string, error) {
	scheme, params := parseChallenge(challenge)
	if !strings.EqualFold(scheme, "Bearer") || params["realm"] == "" {
		return "", fmt.Errorf("registry wants authentication microbox can't do: %q", challenge)
	}
	u, err := url.Parse(params["realm"])
	if err != nil {
		return "", err
	}
	q := u.Query()
	for _, k := range []string{"service", "scope"} {
		if v := params[k]; v != "" {
			q.Set(k, v)
		}
	}
	if q.Get("scope") == "" {
		q.Set("scope", "repository:"+c.ref.Repo+":pull")
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, "GET", u.String(), nil)
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("token from %s: %s", u.Host, resp.Status)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&t); err != nil {
		return "", fmt.Errorf("token: %w", err)
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	return t.Token, nil
}

// parseChallenge splits a WWW-Authenticate header into its scheme and its
// key=value parameters. A quoted value may contain commas, as a scope like
// "repository:app:pull,push" does.
func parseChallenge(h string) (string, map[string]string) {
	scheme, rest, _ := strings.Cut(strings.TrimSpace(h), " ")
	params := map[string]string{}
	for {
		rest = strings.TrimLeft(rest, ", ")
		key, after, ok := strings.Cut(rest, "=")
		if !ok {
			return scheme, params
		}
		var val string
		if strings.HasPrefix(after, `"`) {
			var b strings.Builder
			i := 1
			for ; i < len(after) && after[i] != '"'; i++ {
				if after[i] == '\\' && i+1 < len(after) {
					i++
				}
				b.WriteByte(after[i])
			}
			val, rest = b.String(), after[min(i+1, len(after)):]
		} else {
			val, rest, _ = strings.Cut(after, ",")
		}
		params[strings.ToLower(strings.TrimSpace(key))] = strings.TrimSpace(val)
	}
}

// headManifest returns the digest the registry reports for a tag, or "" if
// it doesn't say.
func (c *client) headManifest(ctx context.Context, tag string) (string, error) {
	resp, err := c.do(ctx, "HEAD", c.url("manifests", tag), manifestTypes)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	return resp.Header.Get("Docker-Content-Digest"), nil
}

// getManifest fetches a manifest by tag or digest and checks it against want,
// unless want is empty.
func (c *client) getManifest(ctx context.Context, ref, want string) ([]byte, error) {
	resp, err := c.do(ctx, "GET", c.url("manifests", ref), manifestTypes)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, err
	}
	if want != "" && digestOf(body) != want {
		return nil, fmt.Errorf("manifest %s: got content with digest %s", want, digestOf(body))
	}
	return body, nil
}

// getBlob fetches a small blob into memory and checks its digest.
func (c *client) getBlob(ctx context.Context, d descriptor, limit int64) ([]byte, error) {
	if d.Size > limit {
		return nil, fmt.Errorf("%s is %d bytes, more than %d", d.Digest, d.Size, limit)
	}
	var buf bytes.Buffer
	if err := c.downloadBlob(ctx, d, &buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// downloadBlob writes a blob to w, checking its size and sha256 digest.
// Registries usually answer with a redirect to a CDN, which net/http
// follows; it leaves out the Authorization header when the host changes.
func (c *client) downloadBlob(ctx context.Context, d descriptor, w io.Writer) error {
	want, ok := strings.CutPrefix(d.Digest, "sha256:")
	if !ok || len(want) != 64 {
		return fmt.Errorf("unsupported digest %q", d.Digest)
	}
	if _, err := hex.DecodeString(want); err != nil {
		return fmt.Errorf("bad digest %q", d.Digest)
	}
	resp, err := c.do(ctx, "GET", c.url("blobs", d.Digest), "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(w, h), io.LimitReader(resp.Body, d.Size+1))
	if err != nil {
		return err
	}
	if n != d.Size {
		return fmt.Errorf("%s: got %d bytes, want %d", d.Digest, n, d.Size)
	}
	if got := hex.EncodeToString(h.Sum(nil)); got != want {
		return fmt.Errorf("%s: content has digest sha256:%s", d.Digest, got)
	}
	return nil
}

func digestOf(b []byte) string {
	sum := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(sum[:])
}

// short is the first 12 hex digits of a digest, as docker shows image IDs.
func short(digest string) string {
	hex := strings.TrimPrefix(digest, "sha256:")
	if len(hex) > 12 {
		hex = hex[:12]
	}
	return hex
}

func formatSize(n int64) string {
	switch {
	case n >= 1<<20:
		return fmt.Sprintf("%.1f MB", float64(n)/(1<<20))
	case n >= 1<<10:
		return fmt.Sprintf("%.1f kB", float64(n)/(1<<10))
	}
	return fmt.Sprintf("%d B", n)
}
