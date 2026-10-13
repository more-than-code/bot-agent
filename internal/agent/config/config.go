// Package config loads and rewrites the bot-agent env file.
package config

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"regexp"
	"strings"

	"github.com/google/uuid"
)

const (
	DefaultEnvFile  = "/etc/bot-agent/env"
	DefaultStateDir = "/var/lib/bot-agent"

	keyURL      = "BOT_AGENT_SERVER_URL"
	keyJoin     = "BOT_AGENT_JOIN_TOKEN"
	keyHost     = "BOT_AGENT_HOST_ID"
	keyCredID   = "BOT_AGENT_CREDENTIAL_ID"
	keyCred     = "BOT_AGENT_CREDENTIAL"
	keyHostname = "BOT_AGENT_HOSTNAME"
	keyAlwaysOn = "BOT_AGENT_ALWAYS_ON"
	keyInsecure = "BOT_AGENT_INSECURE_DEV"
	keyState    = "BOT_AGENT_STATE_DIR"
	keyEnvFile  = "BOT_AGENT_ENV_FILE"
)

// Config is the effective runtime configuration. The disk fields are the
// on-disk file and are not overwritten by process-environment overlays.
type Config struct {
	Path         string
	ServerURL    string
	JoinToken    string
	HostID       string
	CredentialID string
	Credential   string
	Hostname     string
	AlwaysOn     bool
	InsecureDev  bool
	StateDir     string

	disk      map[string]string
	diskOrder []string
}

// Load reads path, requires mode 0600 and a regular file, then lets a
// non-empty process environment override the same keys.
func Load(path string) (Config, error) {
	fi, err := os.Lstat(path)
	if err != nil {
		return Config{}, fmt.Errorf("env file: %w", err)
	}
	if fi.Mode()&os.ModeSymlink != 0 {
		return Config{}, errors.New("env file must not be a symlink")
	}
	if !fi.Mode().IsRegular() {
		return Config{}, errors.New("env file must be a regular file")
	}
	if fi.Mode().Perm() != 0o600 {
		return Config{}, errors.New("env file mode is not 0600")
	}
	body, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("env file: %w", err)
	}
	disk, order, err := parseEnv(body)
	if err != nil {
		return Config{}, err
	}
	eff := copyMap(disk)
	overlayEnv(eff)
	cfg := Config{
		Path:         path,
		ServerURL:    eff[keyURL],
		JoinToken:    eff[keyJoin],
		HostID:       eff[keyHost],
		CredentialID: eff[keyCredID],
		Credential:   eff[keyCred],
		Hostname:     eff[keyHostname],
		AlwaysOn:     true,
		StateDir:     eff[keyState],
		disk:         disk,
		diskOrder:    order,
	}
	if cfg.StateDir == "" {
		cfg.StateDir = DefaultStateDir
	}
	if v := eff[keyAlwaysOn]; v != "" {
		b, err := parseBool(v)
		if err != nil {
			return Config{}, errors.New("always_on is not valid")
		}
		cfg.AlwaysOn = b
	}
	if v := eff[keyInsecure]; v != "" {
		b, err := parseBool(v)
		if err != nil {
			return Config{}, errors.New("insecure dev flag is not valid")
		}
		cfg.InsecureDev = b
	}
	return cfg, nil
}

// Validate checks URL, credential shape, and the state directory.
// Error strings do not contain secrets. The caller checks euid.
func (c Config) Validate() error {
	if err := ValidateServerURL(c.ServerURL, c.InsecureDev); err != nil {
		return err
	}
	if !absPath(c.StateDir) {
		return errors.New("state dir must be absolute")
	}
	if c.Credential != "" || c.HostID != "" || c.CredentialID != "" {
		if c.Credential == "" || c.HostID == "" || c.CredentialID == "" {
			return errors.New("credential is incomplete")
		}
		if _, err := uuid.Parse(c.HostID); err != nil {
			return errors.New("host id is not a uuid")
		}
		if _, err := uuid.Parse(c.CredentialID); err != nil {
			return errors.New("credential id is not a uuid")
		}
		if !secretOK(c.Credential) {
			return errors.New("credential is not usable")
		}
		return nil
	}
	if c.JoinToken == "" {
		return errors.New("missing credentials")
	}
	if !secretOK(c.JoinToken) {
		return errors.New("join token is not usable")
	}
	return nil
}

// HasCredential reports whether this process can send Bearer.
func (c Config) HasCredential() bool {
	return c.Credential != "" && c.HostID != "" && c.CredentialID != ""
}

// DiskHasJoin reports a join token still stored in the env file.
func (c Config) DiskHasJoin() bool {
	return c.disk[keyJoin] != ""
}

// ValidateServerURL accepts wss, and ws only for the dev loopback rule.
func ValidateServerURL(raw string, insecureDev bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil {
		return errors.New("server url is not valid")
	}
	switch u.Scheme {
	case "wss":
		return nil
	case "ws":
		host := strings.ToLower(u.Hostname())
		if insecureDev && (host == "127.0.0.1" || host == "localhost") {
			return nil
		}
		return errors.New("server url must use wss")
	default:
		return errors.New("server url must use wss")
	}
}

// Init writes a new env file. It refuses when the file already holds a credential.
// The join token is one trimmed line from stdin, not an argument.
func Init(args []string) error {
	return initFrom(args, os.Stdin)
}

func initFrom(args []string, in io.Reader) error {
	server, path, err := parseInitArgs(args)
	if err != nil {
		return err
	}
	if path == "" {
		if v := os.Getenv(keyEnvFile); v != "" {
			path = v
		} else {
			path = DefaultEnvFile
		}
	}
	insecure, err := envBool(keyInsecure)
	if err != nil {
		return errors.New("insecure dev flag is not valid")
	}
	if err := ValidateServerURL(server, insecure); err != nil {
		return err
	}
	token, err := readJoinToken(in)
	if err != nil {
		return err
	}
	if !joinTokenOK(token) {
		return errors.New("join token is not usable")
	}
	return WriteInitial(path, server, token)
}

func readJoinToken(r io.Reader) (string, error) {
	if r == nil {
		return "", errors.New("join token is not usable")
	}
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 256), 512)
	if !sc.Scan() {
		return "", errors.New("join token is not usable")
	}
	return strings.TrimSpace(sc.Text()), nil
}

func joinTokenOK(v string) bool {
	return joinTokenRE.MatchString(v)
}

// WriteInitial creates or replaces path when it has no credential.
func WriteInitial(path, server, token string) error {
	if st, err := os.Lstat(path); err == nil {
		if st.Mode()&os.ModeSymlink != 0 || !st.Mode().IsRegular() {
			return errors.New("env file is not a regular file")
		}
		body, err := os.ReadFile(path)
		if err != nil {
			return errors.New("env file cannot be read")
		}
		disk, _, err := parseEnv(body)
		if err != nil {
			return err
		}
		if disk[keyCred] != "" || disk[keyCredID] != "" || disk[keyHost] != "" {
			return errors.New("env file already has a credential")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("env file cannot be read")
	}
	vals := map[string]string{keyURL: server, keyJoin: token}
	return writeEnv(path, vals, []string{keyURL, keyJoin})
}

// PersistEnrolled removes the join token and stores the issued credential.
// Other on-disk keys are kept. Process-environment overlays are not written,
// except the server URL, which restart needs.
func (c *Config) PersistEnrolled(hostID, credID, cred string) error {
	if _, err := uuid.Parse(hostID); err != nil {
		return errors.New("host id is not a uuid")
	}
	if _, err := uuid.Parse(credID); err != nil {
		return errors.New("credential id is not a uuid")
	}
	if !secretOK(cred) {
		return errors.New("credential is not usable")
	}
	if c.disk == nil {
		c.disk = map[string]string{}
	}
	delete(c.disk, keyJoin)
	if c.ServerURL != "" {
		c.disk[keyURL] = c.ServerURL
	}
	c.disk[keyHost] = hostID
	c.disk[keyCredID] = credID
	c.disk[keyCred] = cred
	order := enrollOrder(c.diskOrder, c.disk)
	if err := writeEnv(c.Path, c.disk, order); err != nil {
		return err
	}
	c.diskOrder = order
	c.JoinToken = ""
	c.HostID = hostID
	c.CredentialID = credID
	c.Credential = cred
	return nil
}

func enrollOrder(prev []string, disk map[string]string) []string {
	var order []string
	seen := map[string]bool{}
	add := func(k string) {
		if seen[k] {
			return
		}
		if _, ok := disk[k]; !ok {
			return
		}
		order = append(order, k)
		seen[k] = true
	}
	for _, k := range prev {
		if k == keyJoin {
			continue
		}
		add(k)
	}
	for _, k := range []string{keyURL, keyHost, keyCredID, keyCred} {
		add(k)
	}
	for k := range disk {
		add(k)
	}
	return order
}

func parseInitArgs(args []string) (server, path string, err error) {
	stdin := false
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--join-token" || strings.HasPrefix(a, "--join-token=") {
			return "", "", errors.New("--join-token was removed; use --join-token-stdin")
		}
		if strings.HasPrefix(a, "--join-token-stdin=") {
			return "", "", errors.New("invalid init flags")
		}
		if a == "--join-token-stdin" {
			stdin = true
			continue
		}
		name, val, consumed, ok := takeFlag(a, args, i)
		if !ok {
			return "", "", errors.New("init requires --server and --join-token-stdin")
		}
		i += consumed
		switch name {
		case "--server":
			server = val
		case "--env-file":
			path = val
		default:
			return "", "", errors.New("invalid init flags")
		}
	}
	if server == "" || !stdin {
		return "", "", errors.New("init requires --server and --join-token-stdin")
	}
	return server, path, nil
}

func takeFlag(a string, args []string, i int) (name, val string, consumed int, ok bool) {
	for _, flag := range []string{"--server", "--env-file"} {
		if a == flag {
			if i+1 >= len(args) {
				return "", "", 0, false
			}
			return flag, args[i+1], 1, true
		}
		if strings.HasPrefix(a, flag+"=") {
			return flag, strings.TrimPrefix(a, flag+"="), 0, true
		}
	}
	return "", "", 0, false
}

func parseEnv(body []byte) (map[string]string, []string, error) {
	out := map[string]string{}
	var order []string
	sc := bufio.NewScanner(bytes.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 64*1024)
	lineNo := 0
	for sc.Scan() {
		lineNo++
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		line = strings.TrimPrefix(line, "export ")
		k, v, found := strings.Cut(line, "=")
		if !found {
			return nil, nil, fmt.Errorf("env file line %d", lineNo)
		}
		k = strings.TrimSpace(k)
		if k == "" || strings.ContainsAny(k, " \t") {
			return nil, nil, fmt.Errorf("env file line %d", lineNo)
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && v[0] == '"' && v[len(v)-1] == '"' {
			u, err := unquote(v)
			if err != nil {
				return nil, nil, fmt.Errorf("env file line %d", lineNo)
			}
			v = u
		}
		if _, exists := out[k]; !exists {
			order = append(order, k)
		}
		out[k] = v
	}
	if err := sc.Err(); err != nil {
		return nil, nil, errors.New("env file is not readable")
	}
	return out, order, nil
}

func unquote(v string) (string, error) {
	inner := v[1 : len(v)-1]
	var b strings.Builder
	esc := false
	for _, r := range inner {
		if esc {
			b.WriteRune(r)
			esc = false
			continue
		}
		if r == '\\' {
			esc = true
			continue
		}
		b.WriteRune(r)
	}
	if esc {
		return "", errors.New("escape")
	}
	return b.String(), nil
}

func writeEnv(path string, vals map[string]string, order []string) error {
	var b strings.Builder
	seen := map[string]bool{}
	for _, k := range order {
		v, ok := vals[k]
		if !ok {
			continue
		}
		if err := writeKV(&b, k, v); err != nil {
			return err
		}
		seen[k] = true
	}
	for k, v := range vals {
		if seen[k] {
			continue
		}
		if err := writeKV(&b, k, v); err != nil {
			return err
		}
	}
	return atomicWrite(path, []byte(b.String()), 0o600, 0o750)
}

func writeKV(b *strings.Builder, k, v string) error {
	if strings.ContainsAny(k, "=\r\n #") || strings.ContainsAny(v, "\r\n") {
		return errors.New("env value is not usable")
	}
	b.WriteString(k)
	b.WriteByte('=')
	if strings.ContainsAny(v, " \t#\"\\") {
		b.WriteByte('"')
		b.WriteString(strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(v))
		b.WriteByte('"')
	} else {
		b.WriteString(v)
	}
	b.WriteByte('\n')
	return nil
}

func overlayEnv(dst map[string]string) {
	for _, k := range []string{keyURL, keyJoin, keyHost, keyCredID, keyCred, keyHostname, keyAlwaysOn, keyInsecure, keyState} {
		v, ok := os.LookupEnv(k)
		if ok && v != "" {
			dst[k] = v
		}
	}
}

func copyMap(in map[string]string) map[string]string {
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}

func parseBool(v string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on":
		return true, nil
	case "0", "false", "no", "off":
		return false, nil
	default:
		return false, errors.New("bool")
	}
}

func envBool(key string) (bool, error) {
	v, ok := os.LookupEnv(key)
	if !ok || v == "" {
		return false, nil
	}
	return parseBool(v)
}

func secretOK(v string) bool {
	if v == "" || len(v) > 4096 {
		return false
	}
	for _, r := range v {
		if r < 0x21 || r > 0x7e {
			return false
		}
	}
	return true
}

func absPath(p string) bool {
	return strings.HasPrefix(p, "/")
}

// SanitizeHostname lowercases and replaces characters outside [a-z0-9-]
// so the enroll frame matches the contract. The result is 1–63 characters.
func SanitizeHostname(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", errors.New("hostname is empty")
	}
	var b strings.Builder
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('-')
		}
	}
	out := b.String()
	if len(out) > 63 {
		out = out[:63]
	}
	if !hostnameRE.MatchString(out) {
		return "", errors.New("hostname is not valid")
	}
	return out, nil
}

var hostnameRE = regexp.MustCompile(`^[a-z0-9-]{1,63}$`)

var joinTokenRE = regexp.MustCompile(`^[A-Za-z0-9_-]{16,128}$`)
