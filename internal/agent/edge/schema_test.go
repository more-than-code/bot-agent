package edge

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func compactFile(t *testing.T, path string) []byte {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var buf bytes.Buffer
	if err := json.Compact(&buf, raw); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

func applyFor(cfg []byte) []byte {
	return applyBody(string(cfg), HashConfig(cfg))
}

// Every golden file of the API's generator (copied from bot-control-plane
// api/internal/edgeconfig/testdata; Main Dev keeps the two in sync) passes.
func TestSchemaAcceptsGolden(t *testing.T) {
	files, err := filepath.Glob("testdata/*.json")
	if err != nil || len(files) < 4 {
		t.Fatalf("%v %v", files, err)
	}
	for _, f := range files {
		if _, err := ParseApply(applyFor(compactFile(t, f))); err != nil {
			t.Fatalf("%s: %v", f, err)
		}
	}
}

// golden is daikou.json decoded, for mutation into each attack.
func golden(t *testing.T) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal(compactFile(t, "testdata/daikou.json"), &m); err != nil {
		t.Fatal(err)
	}
	return m
}

func marshal(t *testing.T, v any) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		t.Fatal(err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n")
}

func server(m map[string]any) map[string]any {
	return m["apps"].(map[string]any)["http"].(map[string]any)["servers"].(map[string]any)["edge"].(map[string]any)
}

// firstProxy is the reverse_proxy handler of daikou.online's route.
func firstProxy(m map[string]any) map[string]any {
	route := server(m)["routes"].([]any)[0].(map[string]any)
	sub := route["handle"].([]any)[0].(map[string]any)
	return sub["routes"].([]any)[1].(map[string]any)["handle"].([]any)[0].(map[string]any)
}

func firstSubroute(m map[string]any) []any {
	route := server(m)["routes"].([]any)[0].(map[string]any)
	return route["handle"].([]any)[0].(map[string]any)["routes"].([]any)
}

func policy0(m map[string]any) map[string]any {
	return m["apps"].(map[string]any)["tls"].(map[string]any)["automation"].(map[string]any)["policies"].([]any)[0].(map[string]any)
}

// Each attack from the security review, and the shapes around it, is
// bad_spec with a path.
func TestSchemaRejectsAttacks(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(m map[string]any)
	}{
		{"listen on another port", func(m map[string]any) { server(m)["listen"] = []any{":2019"} }},
		{"listen on two ports", func(m map[string]any) { server(m)["listen"] = []any{":443", ":8080"} }},
		{"metadata upstream", func(m map[string]any) {
			firstProxy(m)["upstreams"] = []any{map[string]any{"dial": "169.254.169.254:80"}}
		}},
		{"loopback low port", func(m map[string]any) { firstProxy(m)["upstreams"] = []any{map[string]any{"dial": "127.0.0.1:22"}} }},
		{"localhost name", func(m map[string]any) { firstProxy(m)["upstreams"] = []any{map[string]any{"dial": "localhost:3000"}} }},
		{"port too large", func(m map[string]any) { firstProxy(m)["upstreams"] = []any{map[string]any{"dial": "127.0.0.1:70000"}} }},
		{"unix upstream", func(m map[string]any) {
			firstProxy(m)["upstreams"] = []any{map[string]any{"dial": "unix//var/run/docker.sock"}}
		}},
		{"proxy transport", func(m map[string]any) { firstProxy(m)["transport"] = map[string]any{"protocol": "fastcgi"} }},
		{"proxy lb_policy", func(m map[string]any) { firstProxy(m)["lb_policy"] = map[string]any{"policy": "first"} }},
		{"proxy handle_response", func(m map[string]any) { firstProxy(m)["handle_response"] = []any{} }},
		{"proxy rewrite", func(m map[string]any) { firstProxy(m)["rewrite"] = map[string]any{"uri": "/x"} }},
		{"file_server /data", func(m map[string]any) {
			firstSubroute(m)[1].(map[string]any)["handle"] = []any{map[string]any{"handler": "file_server", "root": "/data"}}
		}},
		{"static_response", func(m map[string]any) {
			firstSubroute(m)[1].(map[string]any)["handle"] = []any{map[string]any{"handler": "static_response", "body": "{file./data/x}"}}
		}},
		{"templates", func(m map[string]any) {
			firstSubroute(m)[1].(map[string]any)["handle"] = []any{map[string]any{"handler": "templates"}}
		}},
		{"file placeholder in header", func(m map[string]any) {
			firstProxy(m)["headers"] = map[string]any{"request": map[string]any{"set": map[string]any{"X-Key": []any{"{file./data/caddy/key}"}}}}
		}},
		{"env placeholder in header", func(m map[string]any) {
			firstProxy(m)["headers"] = map[string]any{"request": map[string]any{"set": map[string]any{"X-Env": []any{"a{env.HOME}"}}}}
		}},
		{"header add op", func(m map[string]any) {
			firstProxy(m)["headers"] = map[string]any{"request": map[string]any{"add": map[string]any{"X": []any{"y"}}}}
		}},
		{"logging to file", func(m map[string]any) {
			m["logging"] = map[string]any{"logs": map[string]any{"x": map[string]any{"writer": map[string]any{"output": "file", "filename": "/run/edge/config.json"}}}}
		}},
		{"storage", func(m map[string]any) { m["storage"] = map[string]any{"module": "file_system", "root": "/run/edge"} }},
		{"pki app", func(m map[string]any) { m["apps"].(map[string]any)["pki"] = map[string]any{} }},
		{"http option", func(m map[string]any) { m["apps"].(map[string]any)["http"].(map[string]any)["http_port"] = 8080 }},
		{"server option", func(m map[string]any) { server(m)["automatic_https"] = map[string]any{"disable": true} }},
		{"route group", func(m map[string]any) { server(m)["routes"].([]any)[0].(map[string]any)["group"] = "g" }},
		{"matcher expression", func(m map[string]any) {
			server(m)["routes"].([]any)[0].(map[string]any)["match"] = []any{map[string]any{"expression": "true"}}
		}},
		{"host with placeholder", func(m map[string]any) {
			server(m)["routes"].([]any)[0].(map[string]any)["match"] = []any{map[string]any{"host": []any{"{env.X}"}}}
		}},
		{"relative path", func(m map[string]any) {
			server(m)["routes"].([]any)[0].(map[string]any)["match"] = []any{map[string]any{"host": []any{"a.b"}, "path": []any{"x"}}}
		}},
		{"bad cidr", func(m map[string]any) {
			server(m)["routes"].([]any)[0].(map[string]any)["match"] = []any{map[string]any{"remote_ip": map[string]any{"ranges": []any{"1.2.3.4/99"}}}}
		}},
		{"encode brotli", func(m map[string]any) {
			firstSubroute(m)[0].(map[string]any)["handle"].([]any)[0].(map[string]any)["encodings"] = map[string]any{"br": map[string]any{}}
		}},
		{"tls ca", func(m map[string]any) {
			policy0(m)["issuers"] = []any{map[string]any{"module": "acme", "email": "a@b.co", "ca": "https://evil.example/dir"}}
		}},
		{"tls external account", func(m map[string]any) {
			policy0(m)["issuers"] = []any{map[string]any{"module": "acme", "external_account": map[string]any{}}}
		}},
		{"tls zerossl", func(m map[string]any) { policy0(m)["issuers"] = []any{map[string]any{"module": "zerossl"}} }},
		{"tls storage", func(m map[string]any) { policy0(m)["storage"] = map[string]any{} }},
		{"tls certificates", func(m map[string]any) {
			m["apps"].(map[string]any)["tls"].(map[string]any)["certificates"] = map[string]any{"load_files": []any{}}
		}},
		{"tls session tickets", func(m map[string]any) {
			m["apps"].(map[string]any)["tls"].(map[string]any)["session_tickets"] = map[string]any{}
		}},
		{"bad email", func(m map[string]any) { policy0(m)["issuers"] = []any{map[string]any{"module": "acme", "email": "x"}} }},
		{"subroute too deep", func(m map[string]any) {
			inner := []any{}
			for i := 0; i < 6; i++ {
				inner = []any{map[string]any{"handle": []any{map[string]any{"handler": "subroute", "routes": inner}}}}
			}
			server(m)["routes"] = inner
		}},
		{"admin persist", func(m map[string]any) {
			m["admin"] = map[string]any{"listen": AdminListen, "config": map[string]any{"persist": false}}
		}},
		{"admin remote", func(m map[string]any) {
			m["admin"] = map[string]any{"listen": AdminListen, "remote": map[string]any{"listen": ":2021"}}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := golden(t)
			tc.mutate(m)
			_, err := ParseApply(applyFor(marshal(t, m)))
			if !errors.Is(err, ErrSpec) || SpecPath(err) == "" {
				t.Fatalf("accepted or no path: %v", err)
			}
		})
	}
}

// H2: Caddy reads JSON keys case-insensitively and keeps the last
// duplicate, so the admin check must not be bypassable by case or
// repetition. Raw strings, so the key text is exactly what is sent.
func TestSchemaRejectsAdminBypass(t *testing.T) {
	apps := `"apps":{"http":{"servers":{"edge":{"listen":[":443"],"routes":[]}}}}`
	good := `{"admin":{"listen":"unix//run/edge/admin.sock"},` + apps + `}`
	if _, err := ParseApply(applyFor([]byte(good))); err != nil {
		t.Fatal(err)
	}
	for name, cfg := range map[string]string{
		"ADMIN":            `{"admin":{"listen":"unix//run/edge/admin.sock"},"ADMIN":{"listen":"localhost:2019"},` + apps + `}`,
		"Admin":            `{"admin":{"listen":"unix//run/edge/admin.sock"},"Admin":{"listen":"localhost:2019"},` + apps + `}`,
		"Admin only":       `{"Admin":{"listen":"unix//run/edge/admin.sock"},` + apps + `}`,
		"duplicate admin":  `{"admin":{"listen":"unix//run/edge/admin.sock"},"admin":{"listen":"localhost:2019"},` + apps + `}`,
		"duplicate listen": `{"admin":{"listen":"unix//run/edge/admin.sock","listen":"localhost:2019"},` + apps + `}`,
		"Listen":           `{"admin":{"listen":"unix//run/edge/admin.sock","Listen":"localhost:2019"},` + apps + `}`,
		"nested Remote":    `{"admin":{"listen":"unix//run/edge/admin.sock","Remote":{"listen":":2021"}},` + apps + `}`,
		"nested Config":    `{"admin":{"listen":"unix//run/edge/admin.sock","Config":{"load":{"module":"http","url":"http://x"}}},` + apps + `}`,
		"Apps":             `{"admin":{"listen":"unix//run/edge/admin.sock"},` + strings.Replace(apps, `"apps"`, `"Apps"`, 1) + `}`,
		"duplicate apps":   `{"admin":{"listen":"unix//run/edge/admin.sock"},` + apps + `,` + apps + `}`,
		"Handler case": `{"admin":{"listen":"unix//run/edge/admin.sock"},"apps":{"http":{"servers":{"edge":{"listen":[":443"],"routes":[` +
			`{"handle":[{"handler":"reverse_proxy","Handler":"file_server","upstreams":[{"dial":"127.0.0.1:3000"}]}]}]}}}}}`,
		"duplicate handler": `{"admin":{"listen":"unix//run/edge/admin.sock"},"apps":{"http":{"servers":{"edge":{"listen":[":443"],"routes":[` +
			`{"handle":[{"handler":"reverse_proxy","handler":"file_server","upstreams":[{"dial":"127.0.0.1:3000"}]}]}]}}}}}`,
	} {
		_, err := ParseApply(applyFor([]byte(cfg)))
		if !errors.Is(err, ErrSpec) {
			t.Fatalf("%s accepted", name)
		}
	}
}

func TestSchemaPathDetail(t *testing.T) {
	m := golden(t)
	firstProxy(m)["upstreams"] = []any{map[string]any{"dial": "169.254.169.254:80"}}
	_, err := ParseApply(applyFor(marshal(t, m)))
	if got := SpecPath(err); got != "apps.http.servers.edge.routes[0].handle[0].routes[1].handle[0].upstreams[0].dial" {
		t.Fatal(got)
	}
	if strings.Contains(err.Error(), "169.254") {
		t.Fatal("value in error")
	}
}

func TestValidDial(t *testing.T) {
	for s, ok := range map[string]bool{
		"127.0.0.1:1024": true, "127.0.0.1:65535": true, "127.0.0.1:3000": true,
		"127.0.0.1:1023": false, "127.0.0.1:65536": false, "127.0.0.1:0800": false,
		"127.0.0.2:3000": false, "[::1]:3000": false, "127.0.0.1:3000 ": false,
	} {
		if validDial(s) != ok {
			t.Fatal(s)
		}
	}
}

// The final generator shape, field by field: only the exact forms pass.
func TestSchemaExactForms(t *testing.T) {
	wrap := func(routes string) []byte {
		return []byte(`{"admin":{"listen":"unix//run/edge/admin.sock"},"apps":{"http":{"servers":{"edge":{"listen":[":443"],"routes":[` + routes + `]}}}}}`)
	}
	proxy := `{"handler":"reverse_proxy","upstreams":[{"dial":"127.0.0.1:8788"}]}`
	route := func(match, handle string) string {
		return `{"match":[` + match + `],"handle":[` + handle + `]}`
	}
	host := `{"host":["defectdrainer.com"]}`
	cf := `"remote_ip":{"ranges":["173.245.48.0/20"]}`
	sub := func(inner string) string { return `{"handler":"subroute","routes":[` + inner + `]}` }
	for _, tc := range []struct {
		name string
		cfg  []byte
		ok   bool
	}{
		{"plain", wrap(route(host, proxy)), true},
		{"cf header with remote_ip", wrap(route(host, sub(`{"match":[{"header":{"Cf-Connecting-Ip":["*"]},`+cf+`}],"handle":[`+proxy+`]}`))), true},
		{"header without remote_ip", wrap(route(host, sub(`{"match":[{"header":{"Cf-Connecting-Ip":["*"]}}],"handle":[`+proxy+`]}`))), false},
		{"other header", wrap(route(host, sub(`{"match":[{"header":{"X-Real-Ip":["*"]},`+cf+`}],"handle":[`+proxy+`]}`))), false},
		{"cf header and another", wrap(route(host, sub(`{"match":[{"header":{"Cf-Connecting-Ip":["*"],"X-A":["*"]},`+cf+`}],"handle":[`+proxy+`]}`))), false},
		{"cf header other value", wrap(route(host, sub(`{"match":[{"header":{"Cf-Connecting-Ip":["1.2.3.4"]},`+cf+`}],"handle":[`+proxy+`]}`))), false},
		{"header in another matcher object", wrap(route(host, sub(`{"match":[{"header":{"Cf-Connecting-Ip":["*"]}},{`+cf+`}],"handle":[`+proxy+`]}`))), false},
		{"terminal", wrap(`{"match":[` + host + `],"handle":[` + proxy + `],"terminal":true}`), false},
		{"top route without match", wrap(`{"handle":[` + proxy + `]}`), false},
		{"inner route without match", wrap(route(host, sub(`{"handle":[`+proxy+`]}`))), true},
		{"no-store headers", wrap(route(host, sub(`{"match":[{"path":["/evidence/*"]}],"handle":[{"handler":"headers","response":{"deferred":true,"set":{"Cache-Control":["private, no-store"]}}}]}`))), true},
		{"headers other value", wrap(route(host, `{"handler":"headers","response":{"deferred":true,"set":{"Cache-Control":["public"]}}}`)), false},
		{"headers not deferred", wrap(route(host, `{"handler":"headers","response":{"deferred":false,"set":{"Cache-Control":["private, no-store"]}}}`)), false},
		{"headers request side", wrap(route(host, `{"handler":"headers","request":{"set":{"X-A":["b"]}},"response":{"deferred":true,"set":{"Cache-Control":["private, no-store"]}}}`)), false},
		{"headers other key", wrap(route(host, `{"handler":"headers","response":{"deferred":true,"set":{"Location":["https://x.example"]}}}`)), false},
		{"xff exact", wrap(route(host, `{"handler":"reverse_proxy","headers":{"request":{"set":{"X-Forwarded-For":["{http.request.header.CF-Connecting-IP}"]}}},"upstreams":[{"dial":"127.0.0.1:8788"}]}`)), true},
		{"xff other placeholder", wrap(route(host, `{"handler":"reverse_proxy","headers":{"request":{"set":{"X-Forwarded-For":["{http.request.header.Authorization}"]}}},"upstreams":[{"dial":"127.0.0.1:8788"}]}`)), false},
		{"other set header", wrap(route(host, `{"handler":"reverse_proxy","headers":{"request":{"set":{"Host":["x"]}}},"upstreams":[{"dial":"127.0.0.1:8788"}]}`)), false},
		{"delete subset", wrap(route(host, `{"handler":"reverse_proxy","headers":{"request":{"delete":["X-Real-Ip"]}},"upstreams":[{"dial":"127.0.0.1:8788"}]}`)), true},
		{"delete other", wrap(route(host, `{"handler":"reverse_proxy","headers":{"request":{"delete":["Authorization"]}},"upstreams":[{"dial":"127.0.0.1:8788"}]}`)), false},
		{"proxy response headers", wrap(route(host, `{"handler":"reverse_proxy","headers":{"response":{"set":{"X":["y"]}}},"upstreams":[{"dial":"127.0.0.1:8788"}]}`)), false},
		{"stream close delay 1h", wrap(route(host, `{"handler":"reverse_proxy","stream_close_delay":"1h","upstreams":[{"dial":"127.0.0.1:8788"}]}`)), true},
		{"stream close delay other", wrap(route(host, `{"handler":"reverse_proxy","stream_close_delay":"24h","upstreams":[{"dial":"127.0.0.1:8788"}]}`)), false},
		{"stream close delay number", wrap(route(host, `{"handler":"reverse_proxy","stream_close_delay":3600000000000,"upstreams":[{"dial":"127.0.0.1:8788"}]}`)), false},
		{"stream close delay repeated", wrap(route(host, `{"handler":"reverse_proxy","stream_close_delay":"1h","stream_close_delay":"1h","upstreams":[{"dial":"127.0.0.1:8788"}]}`)), false},
		{"stream timeout", wrap(route(host, `{"handler":"reverse_proxy","stream_timeout":"1h","upstreams":[{"dial":"127.0.0.1:8788"}]}`)), false},
		{"server name", []byte(`{"admin":{"listen":"unix//run/edge/admin.sock"},"apps":{"http":{"servers":{"Edge_1":{"listen":[":443"],"routes":[]}}}}}`), false},
		{"path upper", wrap(route(`{"host":["a.example"],"path":["/API"]}`, proxy)), false},
		{"path dotdot", wrap(route(`{"host":["a.example"],"path":["/a/../b"]}`, proxy)), false},
		{"path dot", wrap(route(`{"host":["a.example"],"path":["/a/./b"]}`, proxy)), false},
		{"path empty segment", wrap(route(`{"host":["a.example"],"path":["/a//b"]}`, proxy)), false},
		{"path star middle", wrap(route(`{"host":["a.example"],"path":["/a/*/b"]}`, proxy)), false},
		{"path placeholder", wrap(route(`{"host":["a.example"],"path":["/{env.X}"]}`, proxy)), false},
		{"path forms", wrap(route(`{"host":["a.example"],"path":["/p","/p/*","/evidence/*","/v1.2/x"]}`, proxy)), true},
	} {
		_, err := ParseApply(applyFor(tc.cfg))
		if (err == nil) != tc.ok {
			t.Fatalf("%s: %v", tc.name, err)
		}
	}
}
