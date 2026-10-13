// Package image pulls OCI and Docker images from a registry and keeps them
// unpacked, one directory per layer, for overlayfs to stack.
package image

import (
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"
)

// Dir holds the pulled images:
//
//	images/<reference>.json  one record per image
//	layers/<sha256 hex>/     one unpacked layer, shared by every image that has it
var Dir = "/var/lib/microbox/oci"

// Image is the record of a pulled image.
type Image struct {
	Ref string `json:"ref"` // as Reference.String gives it
	// Digest of what the tag pointed to when pulled: an index for an image
	// built for several platforms. A pull compares it to skip unchanged ones.
	TagDigest string    `json:"tagDigest"`
	ID        string    `json:"id"`     // digest of the config, which is what docker calls the image ID
	Layers    []string  `json:"layers"` // layer digests, bottom layer first
	Size      int64     `json:"size"`   // of the compressed layers
	Config    Config    `json:"config"`
	Pulled    time.Time `json:"pulled"`
}

// Config is the part of the image config that says how to run it.
type Config struct {
	Env        []string `json:"Env,omitempty"`
	Entrypoint []string `json:"Entrypoint,omitempty"`
	Cmd        []string `json:"Cmd,omitempty"`
	WorkingDir string   `json:"WorkingDir,omitempty"`
	User       string   `json:"User,omitempty"`
}

func recordPath(ref string) string {
	return filepath.Join(Dir, "images", url.PathEscape(ref)+".json")
}

// layerDir is where a layer is unpacked. The digest is checked when the
// layer is pulled, so the name is safe to use as a path.
func layerDir(digest string) string {
	return filepath.Join(Dir, "layers", strings.TrimPrefix(digest, "sha256:"))
}

// LowerDirs are the image's layer directories in the order overlayfs wants
// lowerdir: the top layer first.
func (img *Image) LowerDirs() []string {
	var dirs []string
	for _, l := range slices.Backward(img.Layers) {
		dirs = append(dirs, layerDir(l))
	}
	return dirs
}

// complete reports whether all the image's layers are unpacked.
func (img *Image) complete() bool {
	for _, d := range img.LowerDirs() {
		if _, err := os.Stat(d); err != nil {
			return false
		}
	}
	return true
}

// Lookup finds a pulled image. An image that isn't there is an
// fs.ErrNotExist error.
func Lookup(name string) (*Image, error) {
	ref, err := ParseReference(name)
	if err != nil {
		return nil, err
	}
	b, err := os.ReadFile(recordPath(ref.String()))
	if err != nil {
		return nil, err
	}
	var img Image
	if err := json.Unmarshal(b, &img); err != nil {
		return nil, fmt.Errorf("image %s: %w", ref, err)
	}
	return &img, nil
}

// List returns the pulled images, sorted by reference.
func List() ([]*Image, error) {
	entries, err := os.ReadDir(filepath.Join(Dir, "images"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var imgs []*Image
	for _, e := range entries {
		ref, err := url.PathUnescape(strings.TrimSuffix(e.Name(), ".json"))
		if err != nil || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		img, err := Lookup(ref)
		if err != nil {
			return nil, err
		}
		imgs = append(imgs, img)
	}
	slices.SortFunc(imgs, func(a, b *Image) int { return strings.Compare(a.Ref, b.Ref) })
	return imgs, nil
}

// Remove deletes an image's record. Its layers stay until RemoveUnusedLayers,
// since other images or containers may use them.
func Remove(name string) error {
	ref, err := ParseReference(name)
	if err != nil {
		return err
	}
	err = os.Remove(recordPath(ref.String()))
	if errors.Is(err, fs.ErrNotExist) {
		return fmt.Errorf("no such image: %s", ref)
	}
	return err
}

// RemoveUnusedLayers deletes the layers that no image uses and that aren't in
// inUse (the layer directories of existing containers), and returns them.
func RemoveUnusedLayers(inUse []string) ([]string, error) {
	keep := map[string]bool{}
	for _, d := range inUse {
		keep[filepath.Clean(d)] = true
	}
	imgs, err := List()
	if err != nil {
		return nil, err
	}
	for _, img := range imgs {
		for _, d := range img.LowerDirs() {
			keep[d] = true
		}
	}
	entries, err := os.ReadDir(filepath.Join(Dir, "layers"))
	if errors.Is(err, fs.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var removed []string
	for _, e := range entries {
		dir := filepath.Join(Dir, "layers", e.Name())
		if keep[dir] {
			continue
		}
		// A pull unpacks into .tmp-* first. Leave those alone for a while, in
		// case a pull is running; older ones were interrupted.
		if strings.HasPrefix(e.Name(), ".tmp-") {
			if fi, err := e.Info(); err != nil || time.Since(fi.ModTime()) < time.Hour {
				continue
			}
		}
		if err := os.RemoveAll(dir); err != nil {
			return removed, err
		}
		removed = append(removed, e.Name())
	}
	return removed, nil
}

func (img *Image) save() error {
	b, err := json.MarshalIndent(img, "", "  ")
	if err != nil {
		return err
	}
	path := recordPath(img.Ref)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
