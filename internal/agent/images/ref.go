// Package images is the shared, pure image vocabulary for the root helper
// and the agent. It does not read the filesystem or talk to Docker.
package images

import (
	"errors"
	"net/url"
	"regexp"
	"strings"
)

// ErrRef is a reference or pattern that is not in the Docker grammar this
// helper accepts. Callers map it to bad_spec and must not echo the input.
var ErrRef = errors.New("bad reference")

// Reference is a normalised image name. Name is host plus path, with
// docker.io and library made explicit, and with no tag and no digest.
// Tag is set whenever Digest is empty, including the default "latest".
// Digest is "sha256:" and 64 lowercase hex, or empty.
type Reference struct {
	Host   string
	Path   []string
	Name   string
	Tag    string
	Digest string
}

const (
	dockerHost  = "docker.io"
	legacyHost  = "index.docker.io"
	libraryName = "library"
	defaultTag  = "latest"
	maxRefBytes = 255
)

var (
	// nameCompRE is the OCI repository component. It rejects "..", uppercase,
	// and a leading separator.
	nameCompRE = regexp.MustCompile(`^[a-z0-9]+(?:(?:(?:[._]|__|[-]*)[a-z0-9]+)+)?$`)
	// domainCompRE is a registry hostname label after the host is lowercased.
	domainCompRE = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]*[a-z0-9])?$`)
	tagRE        = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9_.-]{0,127}$`)
	digestRE     = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
)

// ValidDigest reports the only digest shape the helper sends to the Engine.
func ValidDigest(s string) bool {
	return digestRE.MatchString(s)
}

// Parse applies the Docker reference grammar and the normalisations the
// policy match depends on. A name with neither tag nor digest gets tag
// "latest" so a pull never sends an empty tag. Uppercase in the path is
// rejected; the registry host is lowercased.
func Parse(raw string) (Reference, error) {
	if raw == "" || len(raw) > maxRefBytes || strings.ContainsAny(raw, " \t\r\n\x00?#\\%") || strings.Contains(raw, "://") {
		return Reference{}, ErrRef
	}
	if strings.Count(raw, "@") > 1 {
		return Reference{}, ErrRef
	}
	name := raw
	digest := ""
	if i := strings.LastIndex(raw, "@"); i != -1 {
		digest = raw[i+1:]
		if !digestRE.MatchString(digest) {
			return Reference{}, ErrRef
		}
		name = raw[:i]
	}
	name, tag, err := splitTag(name)
	if err != nil {
		return Reference{}, err
	}
	host, comps, ok := normalizeName(name)
	if !ok {
		return Reference{}, ErrRef
	}
	if tag == "" && digest == "" {
		tag = defaultTag
	}
	ref := Reference{
		Host:   host,
		Path:   comps,
		Name:   host + "/" + strings.Join(comps, "/"),
		Tag:    tag,
		Digest: digest,
	}
	if len(ref.Name) > maxRefBytes {
		return Reference{}, ErrRef
	}
	return ref, nil
}

// PullQuery is the only images/create query this helper sends: fromImage
// plus a non-empty tag, or fromImage already carrying @sha256:<hex>. Never
// both, and never any other key.
func (r Reference) PullQuery() (url.Values, error) {
	if r.Name == "" || strings.ContainsAny(r.Name, "?&# \t\r\n@") {
		return nil, ErrRef
	}
	q := url.Values{}
	if r.Digest != "" {
		if !ValidDigest(r.Digest) {
			return nil, ErrRef
		}
		q.Set("fromImage", r.Name+"@"+r.Digest)
		return q, nil
	}
	if r.Tag == "" || strings.ContainsAny(r.Tag, "?&# \t\r\n") {
		return nil, ErrRef
	}
	q.Set("fromImage", r.Name)
	q.Set("tag", r.Tag)
	return q, nil
}

// ValidPattern reports whether p is a policy pattern: a literal host, then
// path components that are each either a literal or a single "*".
func ValidPattern(p string) bool {
	_, _, ok := splitPattern(p)
	return ok
}

// Match reports whether ref's normalised host and path equal any pattern.
// The host is literal. "*" is one whole path component. The match is anchored:
// a shorter or longer path does not match.
func Match(ref Reference, patterns []string) bool {
	if ref.Name == "" || len(ref.Path) == 0 {
		return false
	}
	for _, p := range patterns {
		if matchOne(ref, p) {
			return true
		}
	}
	return false
}

func matchOne(ref Reference, p string) bool {
	host, comps, ok := splitPattern(p)
	if !ok || host != ref.Host || len(comps) != len(ref.Path) {
		return false
	}
	for i, c := range comps {
		if c == "*" {
			continue
		}
		if c != ref.Path[i] {
			return false
		}
	}
	return true
}

func splitPattern(p string) (string, []string, bool) {
	if p == "" || len(p) > maxRefBytes || strings.ContainsAny(p, " \t\r\n\x00?#\\%@") {
		return "", nil, false
	}
	host, rest, ok := strings.Cut(p, "/")
	if !ok || host == "" || rest == "" || strings.Contains(host, "*") {
		return "", nil, false
	}
	if !validHost(host) {
		return "", nil, false
	}
	comps := strings.Split(rest, "/")
	for _, c := range comps {
		if c == "*" {
			continue
		}
		if strings.Contains(c, "*") || !nameCompRE.MatchString(c) {
			return "", nil, false
		}
	}
	return host, comps, true
}

func splitTag(name string) (string, string, error) {
	i := strings.LastIndex(name, ":")
	if i == -1 {
		return name, "", nil
	}
	if slash := strings.LastIndex(name, "/"); slash != -1 && slash > i {
		return name, "", nil
	}
	tag := name[i+1:]
	if !tagRE.MatchString(tag) {
		return "", "", ErrRef
	}
	return name[:i], tag, nil
}

func normalizeName(name string) (string, []string, bool) {
	if name == "" || strings.Contains(name, " ") {
		return "", nil, false
	}
	domain, rest := splitDomain(name)
	domain = strings.ToLower(domain)
	if domain == legacyHost {
		domain = dockerHost
	}
	if !validHost(domain) || rest == "" {
		return "", nil, false
	}
	// The path is case-sensitive in the registry. Folding it would let an
	// uppercase lookalike pass a lowercase pattern.
	if rest != strings.ToLower(rest) {
		return "", nil, false
	}
	if domain == dockerHost && !strings.Contains(rest, "/") {
		rest = libraryName + "/" + rest
	}
	comps := strings.Split(rest, "/")
	for _, c := range comps {
		if !nameCompRE.MatchString(c) {
			return "", nil, false
		}
	}
	return domain, comps, true
}

func splitDomain(name string) (string, string) {
	i := strings.IndexByte(name, '/')
	if i == -1 || (!strings.ContainsAny(name[:i], ".:") && !strings.EqualFold(name[:i], "localhost")) {
		return dockerHost, name
	}
	return name[:i], name[i+1:]
}

func validHost(host string) bool {
	name := host
	if h, port, ok := strings.Cut(host, ":"); ok {
		if strings.Contains(port, ":") || !validPort(port) {
			return false
		}
		name = h
	}
	if name == "" {
		return false
	}
	for _, c := range strings.Split(name, ".") {
		if !domainCompRE.MatchString(c) {
			return false
		}
	}
	return true
}

func validPort(p string) bool {
	if p == "" || len(p) > 5 || (len(p) > 1 && p[0] == '0') {
		return false
	}
	n := 0
	for _, c := range p {
		if c < '0' || c > '9' {
			return false
		}
		n = n*10 + int(c-'0')
	}
	return n >= 1 && n <= 65535
}
