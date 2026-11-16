package edge

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"net/netip"
	"regexp"
	"strconv"
	"strings"
)

// SpecError is a refused edge request. Path names the offending part of
// the config (or "request"); it never holds a value from the request.
type SpecError struct {
	Path string
}

func (e *SpecError) Error() string { return "edge spec: " + e.Path }

// Unwrap lets errors.Is(err, ErrSpec) hold for every refusal.
func (e *SpecError) Unwrap() error { return ErrSpec }

// SpecPath is the offending path of a refusal, or "".
func SpecPath(err error) string {
	var se *SpecError
	if errors.As(err, &se) {
		return se.Path
	}
	return ""
}

func bad(path string) error { return &SpecError{Path: path} }

// The config schema (agent-protocol v1.15; security review H1/H2). The edge
// runs as root in the host network with the certificate store mounted, so
// the helper does not trust the config's content: only the shape the API's
// generator (bot-control-plane api/internal/edgeconfig) emits is accepted.
// Keys match exactly and case-sensitively, extra and duplicate keys are
// refused at every level (Caddy merges keys case-insensitively, so "Admin"
// or a second "admin" would otherwise slip past a map-based check).
const (
	maxTreeDepth     = 64
	maxSubrouteDepth = 4
	maxStringBytes   = 1024
)

var (
	hostRE       = regexp.MustCompile(`^(\*\.)?([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?$`)
	dialRE       = regexp.MustCompile(`^127\.0\.0\.1:([1-9][0-9]{3,4})$`)
	serverNameRE = regexp.MustCompile(`^[a-z0-9-]{1,32}$`)
	keyRE        = regexp.MustCompile(`^[a-z0-9_-]{1,64}$`)
	pathSegRE    = regexp.MustCompile(`^[a-z0-9_~.-]{1,128}$`)
	emailRE      = regexp.MustCompile(`^[A-Za-z0-9._%+-]{1,64}@([A-Za-z0-9-]{1,63}\.)+[A-Za-z]{2,63}$`)
)

// node is one JSON value with object keys kept in order.
type node struct {
	kind  byte // 'o' object, 'a' array, 's' string, 'n' number, 'b' bool, 'z' null
	keys  []string
	vals  []*node
	items []*node
	str   string
	b     bool
}

// parseTree reads one JSON value with a token walk, refusing duplicate
// object keys (exact match) and anything after the value.
func parseTree(raw []byte) (*node, error) {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	n, err := readNode(dec, "config", 0)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, bad("config")
	}
	return n, nil
}

func readNode(dec *json.Decoder, path string, depth int) (*node, error) {
	if depth > maxTreeDepth {
		return nil, bad(path)
	}
	tok, err := dec.Token()
	if err != nil {
		return nil, bad(path)
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &node{kind: 'o'}
			seen := map[string]bool{}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, bad(path)
				}
				k, ok := kt.(string)
				if !ok {
					return nil, bad(path)
				}
				child := path + "." + safeKey(k)
				if seen[k] {
					return nil, bad(child + " (duplicate)")
				}
				seen[k] = true
				v, err := readNode(dec, child, depth+1)
				if err != nil {
					return nil, err
				}
				n.keys = append(n.keys, k)
				n.vals = append(n.vals, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, bad(path)
			}
			return n, nil
		case '[':
			n := &node{kind: 'a'}
			for i := 0; dec.More(); i++ {
				v, err := readNode(dec, path+"["+strconv.Itoa(i)+"]", depth+1)
				if err != nil {
					return nil, err
				}
				n.items = append(n.items, v)
			}
			if _, err := dec.Token(); err != nil {
				return nil, bad(path)
			}
			return n, nil
		}
		return nil, bad(path)
	case string:
		return &node{kind: 's', str: t}, nil
	case json.Number:
		return &node{kind: 'n'}, nil
	case bool:
		return &node{kind: 'b', b: t}, nil
	case nil:
		return &node{kind: 'z'}, nil
	}
	return nil, bad(path)
}

// safeKey keeps a key printable in a detail line. A key that is not a
// short identifier is shown as "?".
func safeKey(k string) string {
	if keyRE.MatchString(strings.ToLower(k)) {
		return k
	}
	return "?"
}

// fields checks n is an object whose keys are all in required or optional
// and that has every required key. It returns the values by key.
func fields(n *node, path string, required, optional []string) (map[string]*node, error) {
	if n == nil || n.kind != 'o' {
		return nil, bad(path)
	}
	allowed := map[string]bool{}
	for _, k := range required {
		allowed[k] = true
	}
	for _, k := range optional {
		allowed[k] = true
	}
	out := map[string]*node{}
	for i, k := range n.keys {
		if !allowed[k] {
			return nil, bad(path + "." + safeKey(k))
		}
		out[k] = n.vals[i]
	}
	for _, k := range required {
		if _, ok := out[k]; !ok {
			return nil, bad(path + "." + k + " (missing)")
		}
	}
	return out, nil
}

func array(n *node, path string, minItems int) ([]*node, error) {
	if n == nil || n.kind != 'a' || len(n.items) < minItems {
		return nil, bad(path)
	}
	return n.items, nil
}

func str(n *node, path string) (string, error) {
	if n == nil || n.kind != 's' || len(n.str) > maxStringBytes || hasControl(n.str) {
		return "", bad(path)
	}
	return n.str, nil
}

func stringList(n *node, path string, minItems int, check func(string) bool) error {
	items, err := array(n, path, minItems)
	if err != nil {
		return err
	}
	for i, it := range items {
		p := path + "[" + strconv.Itoa(i) + "]"
		s, err := str(it, p)
		if err != nil || !check(s) {
			return bad(p)
		}
	}
	return nil
}

func hasControl(s string) bool {
	for _, r := range s {
		if r < 0x20 || r == 0x7f {
			return true
		}
	}
	return false
}

// checkConfig is the whole schema.
func checkConfig(cfg []byte) error {
	root, err := parseTree(cfg)
	if err != nil {
		return err
	}
	top, err := fields(root, "config", []string{"admin", "apps"}, nil)
	if err != nil {
		return err
	}
	admin, err := fields(top["admin"], "admin", []string{"listen"}, nil)
	if err != nil {
		return err
	}
	if s, err := str(admin["listen"], "admin.listen"); err != nil || s != AdminListen {
		return bad("admin.listen")
	}
	apps, err := fields(top["apps"], "apps", nil, []string{"http", "tls"})
	if err != nil {
		return err
	}
	if n, ok := apps["http"]; ok {
		if err := checkHTTP(n); err != nil {
			return err
		}
	}
	if n, ok := apps["tls"]; ok {
		if err := checkTLS(n); err != nil {
			return err
		}
	}
	return nil
}

// Fixed values the generator emits (security review H1, final API shape).
// Placeholders appear nowhere else: {file.*}, {env.*} and the like could
// read the certificate store or the container's environment.
const (
	cfHeader       = "Cf-Connecting-Ip"
	forwardedFor   = "X-Forwarded-For"
	cfPlaceholder  = "{http.request.header.CF-Connecting-IP}"
	cacheControl   = "Cache-Control"
	noStoreValue   = "private, no-store"
	listenHTTPS    = ":443"
	maxPathEntries = 64
)

// deletable is the request headers a reverse_proxy may delete.
var deletable = map[string]bool{"Cf-Connecting-Ip": true, "True-Client-Ip": true, "X-Real-Ip": true}

func checkHTTP(n *node) error {
	h, err := fields(n, "apps.http", []string{"servers"}, nil)
	if err != nil {
		return err
	}
	servers := h["servers"]
	if servers.kind != 'o' {
		return bad("apps.http.servers")
	}
	for i, name := range servers.keys {
		path := "apps.http.servers." + safeKey(name)
		if !serverNameRE.MatchString(name) {
			return bad(path)
		}
		s, err := fields(servers.vals[i], path, []string{"listen", "routes"}, nil)
		if err != nil {
			return err
		}
		listen, err := array(s["listen"], path+".listen", 1)
		if err != nil || len(listen) != 1 || listen[0].kind != 's' || listen[0].str != listenHTTPS {
			return bad(path + ".listen")
		}
		if err := checkRoutes(s["routes"], path+".routes", 0); err != nil {
			return err
		}
	}
	return nil
}

// checkRoutes: a top-level route has match and handle; an inner (subroute)
// route has handle and may have match. Nothing else (no terminal, group).
func checkRoutes(n *node, path string, depth int) error {
	if depth > maxSubrouteDepth {
		return bad(path)
	}
	routes, err := array(n, path, 0)
	if err != nil {
		return err
	}
	required, optional := []string{"match", "handle"}, []string(nil)
	if depth > 0 {
		required, optional = []string{"handle"}, []string{"match"}
	}
	for i, r := range routes {
		rp := path + "[" + strconv.Itoa(i) + "]"
		f, err := fields(r, rp, required, optional)
		if err != nil {
			return err
		}
		if m, ok := f["match"]; ok {
			if err := checkMatchers(m, rp+".match"); err != nil {
				return err
			}
		}
		if err := checkHandlers(f["handle"], rp+".handle", depth); err != nil {
			return err
		}
	}
	return nil
}

// checkMatchers: each matcher object holds host, path, remote_ip, and
// header only as exactly {"Cf-Connecting-Ip":["*"]} next to remote_ip.
func checkMatchers(n *node, path string) error {
	sets, err := array(n, path, 1)
	if err != nil {
		return err
	}
	for i, set := range sets {
		mp := path + "[" + strconv.Itoa(i) + "]"
		f, err := fields(set, mp, nil, []string{"host", "path", "remote_ip", "header"})
		if err != nil {
			return err
		}
		if len(f) == 0 {
			return bad(mp)
		}
		if h, ok := f["host"]; ok {
			if err := stringList(h, mp+".host", 1, validHost); err != nil {
				return err
			}
		}
		if p, ok := f["path"]; ok {
			if len(p.items) > maxPathEntries {
				return bad(mp + ".path")
			}
			if err := stringList(p, mp+".path", 1, validPath); err != nil {
				return err
			}
		}
		if r, ok := f["remote_ip"]; ok {
			ri, err := fields(r, mp+".remote_ip", []string{"ranges"}, nil)
			if err != nil {
				return err
			}
			if err := stringList(ri["ranges"], mp+".remote_ip.ranges", 1, validCIDR); err != nil {
				return err
			}
		}
		if h, ok := f["header"]; ok {
			if _, withIP := f["remote_ip"]; !withIP {
				return bad(mp + ".header")
			}
			hf, err := fields(h, mp+".header", []string{cfHeader}, nil)
			if err != nil {
				return err
			}
			if !exactStrings(hf[cfHeader], "*") {
				return bad(mp + ".header." + cfHeader)
			}
		}
	}
	return nil
}

func checkHandlers(n *node, path string, depth int) error {
	handlers, err := array(n, path, 1)
	if err != nil {
		return err
	}
	for i, h := range handlers {
		hp := path + "[" + strconv.Itoa(i) + "]"
		if h.kind != 'o' {
			return bad(hp)
		}
		name := ""
		for j, k := range h.keys {
			if k == "handler" && h.vals[j].kind == 's' {
				name = h.vals[j].str
			}
		}
		var err error
		switch name {
		case "subroute":
			var f map[string]*node
			if f, err = fields(h, hp, []string{"handler", "routes"}, nil); err == nil {
				err = checkRoutes(f["routes"], hp+".routes", depth+1)
			}
		case "headers":
			err = checkNoStore(h, hp)
		case "encode":
			err = checkEncode(h, hp)
		case "reverse_proxy":
			err = checkProxy(h, hp)
		default:
			err = bad(hp + ".handler")
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// checkNoStore is the only headers handler: response, deferred, and
// Cache-Control set to "private, no-store".
func checkNoStore(h *node, hp string) error {
	f, err := fields(h, hp, []string{"handler", "response"}, nil)
	if err != nil {
		return err
	}
	r, err := fields(f["response"], hp+".response", []string{"deferred", "set"}, nil)
	if err != nil {
		return err
	}
	if d := r["deferred"]; d.kind != 'b' || !d.b {
		return bad(hp + ".response.deferred")
	}
	set, err := fields(r["set"], hp+".response.set", []string{cacheControl}, nil)
	if err != nil {
		return err
	}
	if !exactStrings(set[cacheControl], noStoreValue) {
		return bad(hp + ".response.set." + cacheControl)
	}
	return nil
}

func checkEncode(h *node, hp string) error {
	f, err := fields(h, hp, []string{"handler", "encodings"}, []string{"prefer"})
	if err != nil {
		return err
	}
	enc, err := fields(f["encodings"], hp+".encodings", nil, []string{"zstd", "gzip"})
	if err != nil {
		return err
	}
	if len(enc) == 0 {
		return bad(hp + ".encodings")
	}
	for k, v := range enc {
		if v.kind != 'o' || len(v.keys) != 0 {
			return bad(hp + ".encodings." + k)
		}
	}
	if p, ok := f["prefer"]; ok {
		seen := map[string]bool{}
		if err := stringList(p, hp+".prefer", 1, func(s string) bool {
			ok := (s == "zstd" || s == "gzip") && !seen[s]
			seen[s] = true
			return ok
		}); err != nil {
			return err
		}
	}
	return nil
}

// checkProxy: upstreams on 127.0.0.1 ports 1024-65535 only; headers.request
// may set X-Forwarded-For to the Cloudflare client IP and may delete the
// client-IP headers. Nothing else (no transport, lb_policy, rewrite, ...).
func checkProxy(h *node, hp string) error {
	f, err := fields(h, hp, []string{"handler", "upstreams"}, []string{"headers"})
	if err != nil {
		return err
	}
	ups, err := array(f["upstreams"], hp+".upstreams", 1)
	if err != nil {
		return err
	}
	for j, u := range ups {
		up := hp + ".upstreams[" + strconv.Itoa(j) + "]"
		uf, err := fields(u, up, []string{"dial"}, nil)
		if err != nil {
			return err
		}
		d, err := str(uf["dial"], up+".dial")
		if err != nil || !validDial(d) {
			return bad(up + ".dial")
		}
	}
	hd, ok := f["headers"]
	if !ok {
		return nil
	}
	hf, err := fields(hd, hp+".headers", []string{"request"}, nil)
	if err != nil {
		return err
	}
	rf, err := fields(hf["request"], hp+".headers.request", nil, []string{"set", "delete"})
	if err != nil {
		return err
	}
	if len(rf) == 0 {
		return bad(hp + ".headers.request")
	}
	if s, ok := rf["set"]; ok {
		set, err := fields(s, hp+".headers.request.set", []string{forwardedFor}, nil)
		if err != nil {
			return err
		}
		if !exactStrings(set[forwardedFor], cfPlaceholder) {
			return bad(hp + ".headers.request.set." + forwardedFor)
		}
	}
	if d, ok := rf["delete"]; ok {
		seen := map[string]bool{}
		if err := stringList(d, hp+".headers.request.delete", 1, func(s string) bool {
			ok := deletable[s] && !seen[s]
			seen[s] = true
			return ok
		}); err != nil {
			return err
		}
	}
	return nil
}

// exactStrings reports whether n is exactly the one-element array [want].
func exactStrings(n *node, want string) bool {
	return n != nil && n.kind == 'a' && len(n.items) == 1 && n.items[0].kind == 's' && n.items[0].str == want
}

func checkTLS(n *node) error {
	t, err := fields(n, "apps.tls", []string{"automation"}, nil)
	if err != nil {
		return err
	}
	a, err := fields(t["automation"], "apps.tls.automation", []string{"policies"}, nil)
	if err != nil {
		return err
	}
	policies, err := array(a["policies"], "apps.tls.automation.policies", 1)
	if err != nil {
		return err
	}
	for i, p := range policies {
		pp := "apps.tls.automation.policies[" + strconv.Itoa(i) + "]"
		f, err := fields(p, pp, []string{"subjects"}, []string{"issuers"})
		if err != nil {
			return err
		}
		if err := stringList(f["subjects"], pp+".subjects", 1, validHost); err != nil {
			return err
		}
		iss, ok := f["issuers"]
		if !ok {
			continue
		}
		issuers, err := array(iss, pp+".issuers", 1)
		if err != nil {
			return err
		}
		for j, is := range issuers {
			ip := pp + ".issuers[" + strconv.Itoa(j) + "]"
			if is.kind != 'o' {
				return bad(ip)
			}
			module := ""
			for k, key := range is.keys {
				if key == "module" && is.vals[k].kind == 's' {
					module = is.vals[k].str
				}
			}
			switch module {
			case "internal":
				if _, err := fields(is, ip, []string{"module"}, nil); err != nil {
					return err
				}
			case "acme":
				f, err := fields(is, ip, []string{"module", "email"}, nil)
				if err != nil {
					return err
				}
				e, err := str(f["email"], ip+".email")
				if err != nil || len(e) > 254 || !emailRE.MatchString(e) {
					return bad(ip + ".email")
				}
			default:
				return bad(ip + ".module")
			}
		}
	}
	return nil
}

func validHost(s string) bool {
	return len(s) <= 253 && hostRE.MatchString(s)
}

// validPath is "/a/b", "/a/b/*" or "/a/*": lowercase segments, none empty,
// "." or "..", and "*" only as the whole last segment.
func validPath(s string) bool {
	if len(s) < 2 || len(s) > 256 || s[0] != '/' {
		return false
	}
	segs := strings.Split(s[1:], "/")
	for i, seg := range segs {
		if seg == "*" && i == len(segs)-1 && i > 0 {
			continue
		}
		if seg == "." || seg == ".." || !pathSegRE.MatchString(seg) {
			return false
		}
	}
	return true
}

func validCIDR(s string) bool {
	_, err := netip.ParsePrefix(s)
	return err == nil
}

func validDial(s string) bool {
	m := dialRE.FindStringSubmatch(s)
	if m == nil {
		return false
	}
	p, err := strconv.Atoi(m[1])
	return err == nil && p >= 1024 && p <= 65535
}
