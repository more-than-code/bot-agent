package render_test

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/render"
)

func moduleRoot(t *testing.T) string {
	t.Helper()
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "nodes.json")); err != nil {
		t.Fatal(err)
	}
	return root
}

func mustRenderAll(t *testing.T) map[string]string {
	t.Helper()
	rendered, err := render.RenderAll(moduleRoot(t))
	if err != nil {
		t.Fatal(err)
	}
	return rendered
}

func mustValidate(t *testing.T, raw map[string]any) render.Service {
	t.Helper()
	service, err := render.ValidateService(raw, "")
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func mustRenderNode(t *testing.T, node render.Node, services []render.Service) string {
	t.Helper()
	text, err := render.RenderNode(node, services)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func mustContain(t *testing.T, text, sub string) {
	t.Helper()
	if !strings.Contains(text, sub) {
		t.Fatalf("missing %q", sub)
	}
}

func mustNotContain(t *testing.T, text, sub string) {
	t.Helper()
	if strings.Contains(text, sub) {
		t.Fatalf("unexpected %q", sub)
	}
}

func TestSnippetsMatchDisk(t *testing.T) {
	root := moduleRoot(t)
	problems, err := render.Check(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(problems) == 0 {
		return
	}
	rendered, err := render.RenderAll(root)
	if err != nil {
		t.Fatal(err)
	}
	var buf strings.Builder
	buf.WriteString(strings.Join(problems, "\n"))
	for _, name := range []string{"pi1", "pi2", "pi3", "pi4", "grok-bot-box"} {
		path := filepath.Join(root, "telegraf", name+".conf")
		current, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		diff := firstDiff(string(current), rendered[name])
		if diff != "" {
			buf.WriteString("\n" + name + ".conf\n" + diff)
		}
	}
	t.Fatal(buf.String())
}

func firstDiff(got, want string) string {
	gl, wl := strings.Split(got, "\n"), strings.Split(want, "\n")
	n := len(gl)
	if len(wl) > n {
		n = len(wl)
	}
	var b strings.Builder
	shown := 0
	for i := 0; i < n; i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g == w {
			continue
		}
		b.WriteString("line " + strconv.Itoa(i+1) + "\n disk: " + g + "\n gen:  " + w + "\n")
		shown++
		if shown == 8 {
			break
		}
	}
	return b.String()
}

func TestSnippetsAreTOML(t *testing.T) {
	for name, text := range mustRenderAll(t) {
		doc, err := parseTOML(text)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		inputs, ok := doc["inputs"].(map[string]any)
		if !ok {
			t.Fatalf("%s: missing inputs", name)
		}
		if _, ok := inputs["procstat"]; !ok {
			t.Fatalf("%s: missing procstat", name)
		}
		if _, ok := doc["processors"]; !ok {
			t.Fatalf("%s: missing processors", name)
		}
	}
}

func TestEveryNodeHasABoundedProcstatScan(t *testing.T) {
	rendered := mustRenderAll(t)
	if len(rendered) != 5 {
		t.Fatalf("nodes: %d", len(rendered))
	}
	for _, name := range []string{"pi1", "pi2", "pi3", "pi4", "grok-bot-box"} {
		if _, ok := rendered[name]; !ok {
			t.Fatalf("missing %s", name)
		}
	}
	for name, text := range rendered {
		mustContain(t, text, "patterns = ['.+']")
		mustContain(t, text, "name = 'user'")
		mustContain(t, text, "k = 20")
		mustContain(t, text, `fields = ["cpu_usage", "memory_rss"]`)
		mustContain(t, text, `tag_with = ["pid", "user", "cmdline"]`)
		mustContain(t, text, `namedrop = ["procstat_lookup", "procstat_socket"]`)
		mustNotContain(t, text, `properties = ["cpu", "limits", "memory", "mmap"]`)
		mustContain(t, text, "order = 1")
		mustContain(t, text, "order = 2")
		mustContain(t, text, "order = 3")
		i1 := strings.Index(text, "order = 1")
		i2 := strings.Index(text, "order = 2")
		i3 := strings.Index(text, "order = 3")
		if !(i1 < i2 && i2 < i3) {
			t.Fatalf("%s: processor order is not rename, topk, rename", name)
		}
	}
}

func TestGrokBotBoxHasNoSystemdInput(t *testing.T) {
	text := mustRenderAll(t)["grok-bot-box"]
	mustNotContain(t, text, "[[inputs.systemd_units]]")
	mustNotContain(t, text, "[[inputs.http_response]]")
	mustContain(t, text, "[[inputs.procstat]]")
	mustContain(t, text, "--config-directory /etc/telegraf/telegraf.d")
	mustNotContain(t, text, "rerun")
}

func TestPi1UsesCgroupsNotTheUserBus(t *testing.T) {
	text := mustRenderAll(t)["pi1"]
	cgroup := "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/"
	mustContain(t, text, "http://127.0.0.1:5176/")
	mustContain(t, text, "http://127.0.0.1:3031/health")
	mustContain(t, text, "http://127.0.0.1:3001/health")
	mustContain(t, text, "http://127.0.0.1:5173/")
	mustContain(t, text, "http://127.0.0.1:3021/health")
	mustContain(t, text, "http://127.0.0.1:5174/")
	mustContain(t, text, cgroup+"tra-backend.service")
	mustContain(t, text, cgroup+"ttd-webapp.service")
	mustContain(t, text, "patterns = ['/home/joe/workspace/tour-agent/tra-backend']")
	mustContain(t, text, "patterns = ['/home/joe/workspace/briefed/bfd-backend']")
	mustContain(t, text, "recursion_depth = -1")
	mustNotContain(t, text, "[[inputs.systemd_units]]")
	mustNotContain(t, text, `scope = "user"`)
	mustNotContain(t, text, `scope = "system"`)
	mustNotContain(t, text, "cgroups = ['user.slice/")
	mustNotContain(t, text, "briefed.service")
	scan := strings.SplitN(strings.SplitN(text, "name = 'user'", 2)[1], "[[inputs.procstat.filter]]", 2)[0]
	mustNotContain(t, scan, "recursion_depth")
}

func TestPi4SystemUnitsAndNoUserScope(t *testing.T) {
	text := mustRenderAll(t)["pi4"]
	mustContain(t, text, `scope = "system"`)
	mustNotContain(t, text, `scope = "user"`)
	mustContain(t, text, "postgresql@18-main.service")
	mustContain(t, text, "http://127.0.0.1:5050/")
	mustNotContain(t, text, "pgadmin4.service")
	mustNotContain(t, text, "postgresql.service")
	// bot_agent.service was retired from the manifests. The filter for the
	// unit that remains must still precede the systemd_units input.
	before := strings.SplitN(text, "name = 'postgres'", 2)[0]
	mustNotContain(t, before, "[[inputs.systemd_units]]")
}

func TestSQLSeedMatchesManifests(t *testing.T) {
	root := moduleRoot(t)
	_, services, err := render.LoadTree(root)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(root, "sql", "010_procstat_and_services.sql"))
	if err != nil {
		t.Fatal(err)
	}
	sql := string(data)
	mustContain(t, sql, "GRANT INSERT ON TABLE metrics.procstat TO metrics_writer")
	mustNotContain(t, sql, "TO metrics_grokbox")
	mustContain(t, sql, "GRANT SELECT ON TABLE metrics.procstat TO metrics_reader")
	mustContain(t, sql, "GRANT SELECT ON TABLE metrics.services TO metrics_reader")
	mustContain(t, sql, "GRANT INSERT ON TABLE metrics.systemd_units TO metrics_writer")
	mustContain(t, sql, "GRANT SELECT ON TABLE metrics.systemd_units TO metrics_reader")
	mustContain(t, sql, "GRANT INSERT ON TABLE metrics.http_response TO metrics_writer")
	mustContain(t, sql, "GRANT SELECT ON TABLE metrics.http_response TO metrics_reader")
	mustContain(t, sql, "CREATE TABLE IF NOT EXISTS metrics.systemd_units")
	mustContain(t, sql, "CREATE TABLE IF NOT EXISTS metrics.http_response")
	mustContain(t, sql, "result_type text")
	mustContain(t, sql, "SELECT set_chunk_time_interval('metrics.procstat', INTERVAL '1 day');")
	mustContain(t, sql, "INTERVAL '7 days'")
	mustNotContain(t, sql, "GRANT INSERT ON TABLE metrics.services")
	mustNotContain(t, sql, "briefed.service")
	mustNotContain(t, sql, "'postgresql.service'")
	for _, service := range services {
		mustContain(t, sql, "'"+service.Name+"'")
		mustContain(t, sql, "'"+service.Host+"'")
		if service.HealthURL != "" {
			mustContain(t, sql, service.HealthURL)
		}
		if service.SystemdUnit != "" {
			mustContain(t, sql, service.SystemdUnit)
		}
	}
}

func TestWorkstationServiceUsesProcessMatchWithoutSystemd(t *testing.T) {
	node := render.Node{Name: "grok-bot-box", Systemd: false}
	service := mustValidate(t, map[string]any{
		"name":          "defect-drainer",
		"project":       "defect-drainer",
		"host":          "grok-bot-box",
		"process_match": "defect-drainer",
	})
	text := mustRenderNode(t, node, []render.Service{service})
	mustContain(t, text, "patterns = ['defect-drainer']")
	mustNotContain(t, text, "systemd_units =")
	mustNotContain(t, text, "[[inputs.systemd_units]]")
}

func TestPi2AndPi3CollectBotAgentOnTheSystemBus(t *testing.T) {
	rendered := mustRenderAll(t)
	pi2 := rendered["pi2"]
	mustContain(t, pi2, "cgroups = ['/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/bot-console.service']")
	mustContain(t, pi2, "http://127.0.0.1:5180/")
	mustNotContain(t, rendered["pi3"], "[[inputs.http_response]]")
	mustNotContain(t, pi2, `scope = "user"`)
	mustNotContain(t, rendered["pi3"], `scope = "user"`)
	// The live manifests no longer include bot_agent (retired 2026-10-12).
	// The system-bus selector is what a system-scope unit still renders.
	for _, host := range []string{"pi2", "pi3"} {
		service := mustValidate(t, map[string]any{
			"name":          "bot-agent-" + host,
			"project":       "bot-agent",
			"host":          host,
			"systemd_unit":  "bot_agent.service",
			"systemd_scope": "system",
			"process_match": "bot_agent",
		})
		text := mustRenderNode(t, render.Node{Name: host, Systemd: true}, []render.Service{service})
		mustContain(t, text, "bot_agent.service")
		mustContain(t, text, `scope = "system"`)
		mustNotContain(t, text, `scope = "user"`)
	}
}

func TestRejectsNonLoopbackHealthURL(t *testing.T) {
	_, err := render.ValidateService(map[string]any{
		"name":       "tra-backend",
		"project":    "tour-agent",
		"host":       "pi1",
		"health_url": "http://192.168.2.10:3031/health",
	}, "")
	var manifest *render.ManifestError
	if !errors.As(err, &manifest) {
		t.Fatalf("got %v", err)
	}
}

func TestFieldListUsesFieldinclude(t *testing.T) {
	fields := `fieldinclude = ["cpu_usage", "memory_rss", "memory_usage", "num_threads", "created_at"]`
	for name, text := range mustRenderAll(t) {
		if !strings.Contains(text, fields) {
			t.Fatalf("%s: missing fieldinclude", name)
		}
		if strings.Contains(text, "fieldpass") {
			t.Fatalf("%s: fieldpass", name)
		}
	}
}

func TestTemplateInstanceUsesCmdlinePatternWithoutRecursion(t *testing.T) {
	node := render.Node{Name: "pi4", Systemd: true}
	instance := mustValidate(t, map[string]any{
		"name":          "postgres",
		"project":       "postgres",
		"host":          "pi4",
		"systemd_unit":  "postgresql@18-main.service",
		"systemd_scope": "system",
		"process_match": "postgres",
	})
	concrete := mustValidate(t, map[string]any{
		"name":          "bot-agent-pi4",
		"project":       "bot-agent",
		"host":          "pi4",
		"systemd_unit":  "bot_agent.service",
		"systemd_scope": "system",
		"process_match": "bot_agent",
	})
	text := mustRenderNode(t, node, []render.Service{instance, concrete})
	postgres := strings.SplitN(strings.SplitN(text, "name = 'postgres'", 2)[1], "[[", 2)[0]
	agent := strings.SplitN(strings.SplitN(text, "name = 'bot-agent-pi4'", 2)[1], "[[", 2)[0]
	mustContain(t, postgres, "patterns = ['postgres']")
	mustNotContain(t, postgres, "systemd_units")
	mustNotContain(t, postgres, "recursion_depth")
	mustContain(t, agent, "systemd_units = ['bot_agent.service']")
	mustContain(t, agent, "patterns = ['bot_agent']")
	mustContain(t, agent, "recursion_depth = -1")
	mustContain(t, text, "postgresql@18-main.service")
	before := strings.SplitN(text, "name = 'bot-agent-pi4'", 2)[0]
	mustNotContain(t, before, "[[inputs.systemd_units]]")
}

func TestRejectsSecretLookingURL(t *testing.T) {
	_, err := render.ValidateService(map[string]any{
		"name":       "tra-backend",
		"project":    "tour-agent",
		"host":       "pi1",
		"health_url": "http://user:secret@127.0.0.1:3031/health",
	}, "")
	var manifest *render.ManifestError
	if !errors.As(err, &manifest) {
		t.Fatalf("got %v", err)
	}
}
