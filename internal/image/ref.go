package image

import (
	"fmt"
	"regexp"
	"strings"
)

// Reference names an image in a registry: [REGISTRY/]REPOSITORY[:TAG].
type Reference struct {
	Registry string // host[:port], e.g. registry-1.docker.io
	Repo     string // e.g. library/alpine
	Tag      string
}

const dockerHub = "registry-1.docker.io"

// The grammar of docker's references, simplified a little.
var (
	registryRE = regexp.MustCompile(`^[A-Za-z0-9.-]+(?::[0-9]+)?$`)
	repoRE     = regexp.MustCompile(`^[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*$`)
	tagRE      = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
)

// ParseReference reads a reference the way docker does. The first part is a
// registry only if it looks like a host: it has a dot or a port, or is
// localhost. Otherwise the image is on Docker Hub, where a name without a
// slash is an official image under library/. The tag defaults to latest.
func ParseReference(s string) (Reference, error) {
	if strings.Contains(s, "@") {
		return Reference{}, fmt.Errorf("%q: pulling by digest isn't supported, use a tag", s)
	}
	r := Reference{Registry: dockerHub, Tag: "latest"}
	name := s
	if first, rest, ok := strings.Cut(s, "/"); ok && (strings.ContainsAny(first, ".:") || first == "localhost") {
		r.Registry, name = first, rest
	}
	// The tag is after the last colon, as long as no slash follows it.
	if i := strings.LastIndexByte(name, ':'); i >= 0 && !strings.Contains(name[i:], "/") {
		name, r.Tag = name[:i], name[i+1:]
	}
	if r.Registry == dockerHub && !strings.Contains(name, "/") {
		name = "library/" + name
	}
	r.Repo = name
	if !registryRE.MatchString(r.Registry) || !repoRE.MatchString(r.Repo) || !tagRE.MatchString(r.Tag) {
		return Reference{}, fmt.Errorf("invalid image reference %q", s)
	}
	return r, nil
}

// String is the short form docker shows: alpine:3.20, user/app:1,
// localhost:5000/app:1.
func (r Reference) String() string {
	name := r.Repo
	if r.Registry == dockerHub {
		name = strings.TrimPrefix(name, "library/")
	} else {
		name = r.Registry + "/" + name
	}
	return name + ":" + r.Tag
}

// scheme is plain HTTP for a registry on this machine and HTTPS otherwise,
// like docker's default for "insecure" registries.
func (r Reference) scheme() string {
	host := r.Registry
	if h, _, ok := strings.Cut(host, ":"); ok {
		host = h
	}
	if host == "localhost" || strings.HasPrefix(host, "127.") {
		return "http"
	}
	return "https"
}
