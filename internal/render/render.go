// Package render turns service manifests into per-node Telegraf snippets.
// The generated banner still names render_telegraf.py so checked-in files
// stay byte-identical with the historical output.
package render

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	// Scan filter name. A manifest must not reuse it.
	scanFilter = "user"
	// topk keeps this many series for each ranked field (cpu and rss).
	topK    = 20
	periodS = 60
	// User services on the Pis run as uid 1000. Telegraf 1.40.1 resolves a
	// relative cgroup string to a path that does not exist. An absolute path
	// is what `telegraf --test` matches.
	userCgroupPrefix = "/sys/fs/cgroup/user.slice/user-1000.slice/user@1000.service/app.slice/"
)

var (
	nameOK = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	hostOK = regexp.MustCompile(`^[A-Za-z0-9](?:[A-Za-z0-9._-]*[A-Za-z0-9])?$`)
	unitOK = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:._@-]{0,200}\.service$`)
	urlOK  = regexp.MustCompile(`^http://(127\.0\.0\.1|localhost):([1-9][0-9]{0,4})(/[A-Za-z0-9._~/-]*)?$`)
	// Literal substring used as a procstat cmdline regex. No quantifiers.
	patternOK = regexp.MustCompile(`^[A-Za-z0-9_./:@-][A-Za-z0-9_./:@ -]{0,199}$`)
)

var serviceKeys = map[string]struct{}{
	"name":          {},
	"project":       {},
	"host":          {},
	"port":          {},
	"health_url":    {},
	"systemd_unit":  {},
	"systemd_scope": {},
	"process_match": {},
}

// ManifestError is a rejected nodes.json entry or service manifest.
type ManifestError struct {
	Message string
}

func (e *ManifestError) Error() string {
	return e.Message
}

func manifest(message string) error {
	return &ManifestError{Message: message}
}

// Node is one cluster member from nodes.json.
type Node struct {
	Name    string
	Systemd bool
}

// Service is one validated services/<name>.json manifest.
// Empty strings mean the optional field was unset. Port is nil when neither
// port nor health_url set one.
type Service struct {
	Name         string
	Project      string
	Host         string
	Port         *int
	HealthURL    string
	SystemdUnit  string
	SystemdScope string
	ProcessMatch string
}

// ValidateService checks one manifest object. filename is the base name used
// in errors; an empty filename reports "manifests", matching a path-less call.
func ValidateService(raw any, filename string) (Service, error) {
	where := filename
	if where == "" {
		where = "manifests"
	}
	fail := func(message string) (Service, error) {
		return Service{}, manifest(where + ": " + message)
	}
	obj, ok := raw.(map[string]any)
	if !ok {
		return fail("must be a JSON object")
	}
	if extra := unknownKeys(obj, serviceKeys); len(extra) > 0 {
		return fail("unknown keys: " + strings.Join(extra, ", "))
	}
	name, nameOKType := obj["name"].(string)
	if !nameOKType || !nameOK.MatchString(name) || name == scanFilter {
		return fail("name must be a lowercase service id and not 'user'")
	}
	project, projectOK := obj["project"].(string)
	if !projectOK || strings.TrimSpace(project) == "" || utf8.RuneCountInString(project) > 80 {
		return fail("project is required")
	}
	host, hostIsString := obj["host"].(string)
	if !hostIsString || !hostOK.MatchString(host) || len(host) > 253 || strings.Contains(host, "..") {
		return fail("host is not a host name")
	}
	service := Service{
		Name:    name,
		Project: strings.TrimSpace(project),
		Host:    host,
	}
	if v, present := obj["port"]; present && v != nil {
		port, ok := portValue(v)
		if !ok || port < 1 || port > 65535 {
			return fail("port must be 1-65535")
		}
		service.Port = &port
	}
	if v, present := obj["health_url"]; present && v != nil {
		url, isString := v.(string)
		var match []string
		if isString {
			match = urlOK.FindStringSubmatch(url)
		}
		if match == nil {
			return fail("health_url must be http://127.0.0.1 or localhost with an explicit port")
		}
		urlPort, err := strconv.Atoi(match[2])
		if err != nil {
			return fail("health_url must be http://127.0.0.1 or localhost with an explicit port")
		}
		if urlPort > 65535 {
			return fail("health_url port is out of range")
		}
		if service.Port == nil {
			service.Port = &urlPort
		} else if *service.Port != urlPort {
			return fail("port and health_url port differ")
		}
		service.HealthURL = url
	}
	if v, present := obj["systemd_unit"]; present && v != nil {
		unit, isString := v.(string)
		if !isString || !unitOK.MatchString(unit) {
			return fail("systemd_unit must look like a unit name ending in .service")
		}
		service.SystemdUnit = unit
	}
	var scope any
	if v, present := obj["systemd_scope"]; present {
		scope = v
	}
	if service.SystemdUnit != "" {
		scopeName, _ := scope.(string)
		if scopeName != "system" && scopeName != "user" {
			return fail("systemd_scope must be system or user when a unit is set")
		}
		service.SystemdScope = scopeName
	} else if scope != nil {
		return fail("systemd_scope is set without a unit")
	}
	if v, present := obj["process_match"]; present && v != nil {
		matchName, isString := v.(string)
		if !isString || !patternOK.MatchString(matchName) || matchName != strings.TrimSpace(matchName) || strings.Contains(matchName, "..") {
			return fail("process_match must be a cmdline substring (letters, digits, /._:@- and spaces)")
		}
		if _, err := regexp.Compile(matchName); err != nil {
			return fail("process_match is not a regex: " + err.Error())
		}
		service.ProcessMatch = matchName
	}
	if service.SystemdUnit == "" && service.ProcessMatch == "" && service.HealthURL == "" {
		return fail("needs a systemd unit, a process_match, or a health_url")
	}
	return service, nil
}

func validateNodes(raw any) ([]Node, error) {
	items, ok := raw.([]any)
	if !ok || len(items) == 0 {
		return nil, manifest("nodes.json: must be a non-empty list")
	}
	nodes := make([]Node, 0, len(items))
	seen := map[string]struct{}{}
	allowed := map[string]struct{}{"name": {}, "systemd": {}}
	for _, item := range items {
		obj, ok := item.(map[string]any)
		if !ok {
			return nil, manifest("nodes.json: each node must be an object")
		}
		if extra := unknownKeys(obj, allowed); len(extra) > 0 {
			return nil, manifest("nodes.json: unknown keys: " + strings.Join(extra, ", "))
		}
		name, isString := obj["name"].(string)
		if !isString || !hostOK.MatchString(name) || len(name) > 253 || strings.Contains(name, "..") {
			return nil, manifest("nodes.json: bad host " + pythonRepr(obj["name"]))
		}
		systemd, isBool := obj["systemd"].(bool)
		if !isBool {
			return nil, manifest(fmt.Sprintf("nodes.json: %s systemd must be true or false", name))
		}
		if _, dup := seen[name]; dup {
			return nil, manifest("nodes.json: duplicate " + name)
		}
		seen[name] = struct{}{}
		nodes = append(nodes, Node{Name: name, Systemd: systemd})
	}
	return nodes, nil
}

// LoadTree reads nodes.json and services/*.json under root.
func LoadTree(root string) ([]Node, []Service, error) {
	nodesRaw, err := parseJSON(filepath.Join(root, "nodes.json"))
	if err != nil {
		return nil, nil, err
	}
	nodes, err := validateNodes(nodesRaw)
	if err != nil {
		return nil, nil, err
	}
	known := map[string]struct{}{}
	for _, node := range nodes {
		known[node.Name] = struct{}{}
	}
	servicesDir := filepath.Join(root, "services")
	entries, err := os.ReadDir(servicesDir)
	if err != nil {
		return nil, nil, err
	}
	var names []string
	for _, entry := range entries {
		name := entry.Name()
		if strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".json") || entry.IsDir() {
			continue
		}
		names = append(names, name)
	}
	if len(names) == 0 {
		return nil, nil, manifest("services: no manifests")
	}
	sort.Strings(names)
	services := make([]Service, 0, len(names))
	seen := map[string]struct{}{}
	for _, name := range names {
		path := filepath.Join(servicesDir, name)
		raw, err := parseJSON(path)
		if err != nil {
			return nil, nil, err
		}
		service, err := ValidateService(raw, name)
		if err != nil {
			return nil, nil, err
		}
		stem := strings.TrimSuffix(name, ".json")
		if service.Name != stem {
			return nil, nil, manifest(name + ": file name must match name")
		}
		if _, dup := seen[service.Name]; dup {
			return nil, nil, manifest(name + ": duplicate name")
		}
		if _, ok := known[service.Host]; !ok {
			return nil, nil, manifest(name + ": host " + service.Host + " is not in nodes.json")
		}
		seen[service.Name] = struct{}{}
		services = append(services, service)
	}
	return nodes, services, nil
}

func unknownKeys(obj map[string]any, allowed map[string]struct{}) []string {
	var extra []string
	for key := range obj {
		if _, ok := allowed[key]; !ok {
			extra = append(extra, key)
		}
	}
	sort.Strings(extra)
	return extra
}

func portValue(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case json.Number:
		text := n.String()
		if text == "" || strings.ContainsAny(text, ".eE") {
			return 0, false
		}
		port, err := strconv.Atoi(text)
		if err != nil {
			return 0, false
		}
		return port, true
	default:
		return 0, false
	}
}

func parseJSON(path string) (any, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("%s: extra JSON value", filepath.Base(path))
		}
		return nil, fmt.Errorf("%s: %w", filepath.Base(path), err)
	}
	return v, nil
}

func quote(value string) (string, error) {
	if strings.Contains(value, "'") || strings.Contains(value, "\n") {
		return "", manifest("cannot quote " + pythonRepr(value))
	}
	return "'" + value + "'", nil
}

func singleQuoteList(value string) (string, error) {
	quoted, err := quote(value)
	if err != nil {
		return "", err
	}
	return "[" + quoted + "]", nil
}

// serviceFilter is one procstat filter for a declared service.
//
// User units use the cgroup path. procstat's systemd_units selector talks to
// the system bus, and inputs.systemd_units scope=user reads Telegraf's own
// user bus, so neither sees joe's user units.
//
// A template instance (an @ in the unit name) has no unit file of its own.
// Telegraf 1.40.1 lists unit files, so systemd_units returns an empty group.
// The cmdline pattern is the selector then, and recursion_depth is omitted:
// the pattern already matches the children, and recursion would emit each one
// again. The cgroup is not used either. A backend that exits while
// cgroup.procs is read drops the whole filter.
func serviceFilter(service Service, node Node) (string, error) {
	quotedName, err := quote(service.Name)
	if err != nil {
		return "", err
	}
	lines := []string{
		"  [[inputs.procstat.filter]]",
		"    name = " + quotedName,
	}
	instance := service.SystemdScope == "system" &&
		node.Systemd &&
		service.SystemdUnit != "" &&
		strings.Contains(service.SystemdUnit, "@") &&
		service.ProcessMatch != ""
	switch {
	case service.SystemdScope == "user" && service.SystemdUnit != "":
		cgroup, err := singleQuoteList(userCgroupPrefix + service.SystemdUnit)
		if err != nil {
			return "", err
		}
		lines = append(lines, "    cgroups = "+cgroup)
	case instance:
		patterns, err := singleQuoteList(service.ProcessMatch)
		if err != nil {
			return "", err
		}
		lines = append(lines, "    patterns = "+patterns)
	case service.SystemdUnit != "" && service.SystemdScope == "system" && node.Systemd:
		units, err := singleQuoteList(service.SystemdUnit)
		if err != nil {
			return "", err
		}
		lines = append(lines, "    systemd_units = "+units)
	case service.ProcessMatch != "":
		patterns, err := singleQuoteList(service.ProcessMatch)
		if err != nil {
			return "", err
		}
		lines = append(lines, "    patterns = "+patterns)
	default:
		return "", nil
	}
	if service.ProcessMatch != "" && !strings.Contains(lines[len(lines)-1], "patterns") {
		patterns, err := singleQuoteList(service.ProcessMatch)
		if err != nil {
			return "", err
		}
		lines = append(lines, "    patterns = "+patterns)
	}
	if !instance {
		lines = append(lines, "    recursion_depth = -1")
	}
	return strings.Join(lines, "\n"), nil
}

func filters(services []Service, node Node) (string, error) {
	blocks := []string{"  [[inputs.procstat.filter]]\n    name = 'user'\n    patterns = ['.+']"}
	for _, service := range services {
		block, err := serviceFilter(service, node)
		if err != nil {
			return "", err
		}
		if block != "" {
			blocks = append(blocks, block)
		}
	}
	return strings.Join(blocks, "\n"), nil
}

// RenderNode renders one host. services may include other hosts; they are skipped.
func RenderNode(node Node, services []Service) (string, error) {
	var mine []Service
	for _, service := range services {
		if service.Host == node.Name {
			mine = append(mine, service)
		}
	}
	sort.Slice(mine, func(i, j int) bool { return mine[i].Name < mine[j].Name })
	install := "# After the copy, restart the Telegraf service on this host (Joe)."
	if node.Name == "grok-bot-box" {
		install = strings.Join(grokInstallLines, "\n")
	}
	body, err := filters(mine, node)
	if err != nil {
		return "", err
	}
	parts := []string{
		banner(node.Name, install),
		procstatHead + body + "\n",
		processorsText(),
	}
	if node.Systemd {
		var units []string
		for _, service := range mine {
			if service.SystemdScope == "system" && service.SystemdUnit != "" {
				units = append(units, service.SystemdUnit)
			}
		}
		sort.Strings(units)
		if len(units) > 0 {
			parts = append(parts, systemdInput(strings.Join(units, " ")))
		}
	}
	var urls []string
	for _, service := range mine {
		if service.HealthURL != "" {
			urls = append(urls, service.HealthURL)
		}
	}
	sort.Strings(urls)
	if len(urls) > 0 {
		quoted := make([]string, len(urls))
		for i, url := range urls {
			quoted[i] = jsonString(url)
		}
		parts = append(parts, httpInput(strings.Join(quoted, ", ")))
	}
	return joinParts(parts), nil
}

// RenderAll renders every node. The map is keyed by host name.
func RenderAll(root string) (map[string]string, error) {
	nodes, services, err := LoadTree(root)
	if err != nil {
		return nil, err
	}
	out := make(map[string]string, len(nodes))
	for _, node := range nodes {
		text, err := RenderNode(node, services)
		if err != nil {
			return nil, err
		}
		out[node.Name] = text
	}
	return out, nil
}

// WriteAll writes telegraf/<host>.conf and removes unexpected *.conf files.
// It returns how many host files were written.
func WriteAll(root string) (int, error) {
	output := filepath.Join(root, "telegraf")
	if err := os.MkdirAll(output, 0o755); err != nil {
		return 0, err
	}
	nodes, services, err := LoadTree(root)
	if err != nil {
		return 0, err
	}
	expected := make(map[string]struct{}, len(nodes))
	for _, node := range nodes {
		text, err := RenderNode(node, services)
		if err != nil {
			return 0, err
		}
		name := node.Name + ".conf"
		expected[name] = struct{}{}
		if err := os.WriteFile(filepath.Join(output, name), []byte(text), 0o644); err != nil {
			return 0, err
		}
	}
	entries, err := os.ReadDir(output)
	if err != nil {
		return 0, err
	}
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".conf") {
			continue
		}
		if _, ok := expected[name]; ok {
			continue
		}
		if err := os.Remove(filepath.Join(output, name)); err != nil {
			return 0, err
		}
	}
	return len(nodes), nil
}

// Check compares telegraf/<host>.conf to a fresh render.
// Problems are "missing", "stale", or "extra" file names. A non-nil error
// means the manifests or a snippet could not be read.
func Check(root string) ([]string, error) {
	nodes, services, err := LoadTree(root)
	if err != nil {
		return nil, err
	}
	output := filepath.Join(root, "telegraf")
	rendered := make(map[string]string, len(nodes))
	var problems []string
	for _, node := range nodes {
		text, err := RenderNode(node, services)
		if err != nil {
			return nil, err
		}
		rendered[node.Name] = text
		path := filepath.Join(output, node.Name+".conf")
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			problems = append(problems, "missing "+node.Name+".conf")
			continue
		}
		current, err := readText(path)
		if err != nil {
			return nil, err
		}
		if current != text {
			problems = append(problems, "stale "+node.Name+".conf")
		}
	}
	info, err := os.Stat(output)
	if err == nil && info.IsDir() {
		entries, err := os.ReadDir(output)
		if err != nil {
			return nil, err
		}
		var extras []string
		for _, entry := range entries {
			name := entry.Name()
			if entry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, ".conf") {
				continue
			}
			stem := strings.TrimSuffix(name, ".conf")
			if _, ok := rendered[stem]; !ok {
				extras = append(extras, name)
			}
		}
		sort.Strings(extras)
		for _, name := range extras {
			problems = append(problems, "extra "+name)
		}
	}
	return problems, nil
}

func readText(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	if !utf8.Valid(data) {
		return "", fmt.Errorf("%s: invalid UTF-8", filepath.Base(path))
	}
	text := strings.ReplaceAll(string(data), "\r\n", "\n")
	text = strings.ReplaceAll(text, "\r", "\n")
	return text, nil
}

func joinParts(parts []string) string {
	out := make([]string, len(parts))
	for i, part := range parts {
		out[i] = strings.TrimRightFunc(part, unicode.IsSpace) + "\n"
	}
	return strings.Join(out, "\n")
}

func jsonString(value string) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(value); err != nil {
		return strconv.Quote(value)
	}
	return strings.TrimSuffix(buf.String(), "\n")
}

const procstatHead = `[[inputs.procstat]]
  interval = "60s"
  pid_finder = "native"
  properties = ["cpu", "memory"]
  tag_with = ["pid", "user", "cmdline"]
  fieldinclude = ["cpu_usage", "memory_rss", "memory_usage", "num_threads", "created_at"]
  taginclude = ["host", "process_name", "pid", "user", "cmdline", "filter", "systemd_unit"]
  namedrop = ["procstat_lookup", "procstat_socket"]
`

func processorsText() string {
	return fmt.Sprintf(`
# Move the full scan aside so topk cannot drop declared service series.
# order is explicit: same-type processors are not otherwise sequenced around topk.
[[processors.rename]]
  order = 1
  namepass = ["procstat"]
  [processors.rename.tagpass]
    filter = ["user"]
  [[processors.rename.replace]]
    measurement = "procstat"
    dest = "procstat_scan"

[[processors.topk]]
  order = 2
  namepass = ["procstat_scan"]
  period = %d
  k = %d
  fields = ["cpu_usage", "memory_rss"]
  group_by = ["*"]
  aggregation = "max"

[[processors.rename]]
  order = 3
  namepass = ["procstat_scan"]
  [[processors.rename.replace]]
    measurement = "procstat_scan"
    dest = "procstat"
`, periodS, topK)
}

func systemdInput(pattern string) string {
	return "[[inputs.systemd_units]]\n" +
		"  interval = \"30s\"\n" +
		"  pattern = \"" + pattern + "\"\n" +
		"  unittype = \"service\"\n" +
		"  scope = \"system\"\n" +
		"  collect_disabled_units = false\n" +
		"  details = false\n"
}

func httpInput(urls string) string {
	return "[[inputs.http_response]]\n" +
		"  interval = \"30s\"\n" +
		"  urls = [" + urls + "]\n" +
		"  method = \"GET\"\n" +
		"  follow_redirects = false\n" +
		"  response_timeout = \"5s\"\n"
}

var grokInstallLines = []string{
	"# grok-bot-box has no systemd. Telegraf is started by",
	"# /home/box/.local/bin/grokbox-metrics-start as",
	"# `telegraf --config /etc/telegraf/telegraf.conf` with no",
	"# --config-directory, and the script skips when Telegraf is already",
	"# running. Copying this file into telegraf.d does not load it until",
	"# Joe adds `--config-directory /etc/telegraf/telegraf.d` and stop/starts",
	"# Telegraf. That change is not made here. See bot-agent/README.md.",
}

func banner(nodeName, install string) string {
	series := strconv.Itoa(topK * 2)
	period := strconv.Itoa(periodS)
	lines := []string{
		"# Generated by render_telegraf.py for " + nodeName + ". Do not edit by hand.",
		"# Not installed. After review, copy to /etc/telegraf/telegraf.d/deploybot-services.conf",
		"# on this host.",
	}
	lines = append(lines, strings.Split(install, "\n")...)
	lines = append(lines,
		"#",
		"# procstat has no top-N. processors.topk keeps "+strconv.Itoa(topK)+" series per ranked field",
		"# (cpu_usage and memory_rss), so at most "+series+" scan series per host per",
		"# "+period+"s. order pins rename -> topk -> rename. Declared services use a",
		"# second filter and skip topk, so a quiet service is still stored. The scan",
		"# pattern '.+' skips kernel threads (empty command line). pid, user, and",
		"# cmdline are tags so same-named processes stay distinct. Node's process name",
		"# is MainThread.",
		"#",
		"# User-scope units are not inputs.systemd_units. Telegraf runs as user telegraf,",
		`# so scope="user" is telegraf's bus, and procstat systemd_units uses the system`,
		"# bus only. Those services use a procstat cgroup under",
		"# user.slice/user-1000.slice/user@1000.service/app.slice/<unit> (absolute path).",
		"# Their health is HTTP plus process presence, not a unit state.",
		"# Service filters set recursion_depth = -1 so a node grandchild of pnpm is kept.",
		"# inputs.systemd_units below is system scope only, details = false.",
		"#",
		"# Expected rows, this host, if this file is what Telegraf runs:",
		"#   scan: up to "+series+" rows every "+period+"s = "+strconv.Itoa(topK*2*1440)+" rows/day",
		"#   services: one row per matched process every "+period+"s (usually a handful)",
		"#   7-day retention of that stream is the cap in sql/010_procstat_and_services.sql",
		"# Cluster of 5 hosts, all busy: about 2 to 3 million procstat rows retained.",
		"#",
		"# CPU mode is the procstat default (irix): 100 means one core, as in top.",
		"# Field list is locked to the procstat table. New fields need a SQL column first.",
		"# topk emits the winning samples, not just the ranked fields.",
		"# If the main config adds aggregators, set skip_processors_after_aggregators = true",
		"# so topk does not hold the same series for a second period.",
	)
	return strings.Join(lines, "\n") + "\n"
}

func pythonRepr(v any) string {
	switch t := v.(type) {
	case nil:
		return "None"
	case bool:
		if t {
			return "True"
		}
		return "False"
	case string:
		return pythonStringRepr(t)
	case json.Number:
		return t.String()
	case int:
		return strconv.Itoa(t)
	case []any:
		parts := make([]string, len(t))
		for i, item := range t {
			parts[i] = pythonRepr(item)
		}
		return "[" + strings.Join(parts, ", ") + "]"
	case map[string]any:
		keys := make([]string, 0, len(t))
		for key := range t {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		parts := make([]string, len(keys))
		for i, key := range keys {
			parts[i] = pythonStringRepr(key) + ": " + pythonRepr(t[key])
		}
		return "{" + strings.Join(parts, ", ") + "}"
	default:
		return fmt.Sprint(t)
	}
}

func pythonStringRepr(s string) string {
	useDouble := strings.Contains(s, "'") && !strings.Contains(s, "\"")
	quote := "'"
	if useDouble {
		quote = "\""
	}
	var b strings.Builder
	b.WriteString(quote)
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		case '\'':
			if !useDouble {
				b.WriteString(`\'`)
			} else {
				b.WriteRune(r)
			}
		case '"':
			if useDouble {
				b.WriteString(`\"`)
			} else {
				b.WriteRune(r)
			}
		default:
			if r < 0x20 || r == 0x7f {
				fmt.Fprintf(&b, `\x%02x`, r)
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteString(quote)
	return b.String()
}
