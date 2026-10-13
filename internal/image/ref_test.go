package image

import (
	"reflect"
	"testing"
)

func TestParseReference(t *testing.T) {
	for in, want := range map[string]Reference{
		"alpine":                       {dockerHub, "library/alpine", "latest"},
		"alpine:3.20":                  {dockerHub, "library/alpine", "3.20"},
		"bitnami/redis:7.2":            {dockerHub, "bitnami/redis", "7.2"},
		"ghcr.io/owner/app:v1":         {"ghcr.io", "owner/app", "v1"},
		"localhost:5000/app":           {"localhost:5000", "app", "latest"},
		"127.0.0.1:5000/team/app:v1.2": {"127.0.0.1:5000", "team/app", "v1.2"},
		"localhost/app:1":              {"localhost", "app", "1"},
	} {
		got, err := ParseReference(in)
		if err != nil || got != want {
			t.Errorf("ParseReference(%q) = %+v, %v; want %+v", in, got, err, want)
		}
	}
	for _, in := range []string{"", "Alpine", "alpine:", "alpine@sha256:abc", "a//b", "-x", "alpine:bad/tag", "my_rootfs!"} {
		if r, err := ParseReference(in); err == nil {
			t.Errorf("ParseReference(%q) = %+v, want an error", in, r)
		}
	}
}

func TestReferenceString(t *testing.T) {
	for in, want := range map[string]string{
		"alpine":                "alpine:latest",
		"library/alpine:3.20":   "alpine:3.20",
		"bitnami/redis":         "bitnami/redis:latest",
		"localhost:5000/app:v1": "localhost:5000/app:v1",
	} {
		r, err := ParseReference(in)
		if err != nil || r.String() != want {
			t.Errorf("%q: String() = %q, %v; want %q", in, r.String(), err, want)
		}
	}
}

func TestParseChallenge(t *testing.T) {
	scheme, params := parseChallenge(`Bearer realm="https://auth.docker.io/token",service="registry.docker.io",scope="repository:library/alpine:pull,push"`)
	want := map[string]string{
		"realm":   "https://auth.docker.io/token",
		"service": "registry.docker.io",
		"scope":   "repository:library/alpine:pull,push",
	}
	if scheme != "Bearer" || !reflect.DeepEqual(params, want) {
		t.Errorf("got %q %v", scheme, params)
	}
	if _, params := parseChallenge(`Basic realm="a \"quoted\" name", charset=UTF-8`); params["realm"] != `a "quoted" name` || params["charset"] != "UTF-8" {
		t.Errorf("got %v", params)
	}
}
