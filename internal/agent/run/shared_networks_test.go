package run

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/more-than-code/bot-agent/internal/agent/policy"
	"github.com/more-than-code/bot-agent/internal/agent/programs"
	"github.com/more-than-code/bot-agent/internal/agent/protocol"
)

// v1.13 item 2: hello.programs.shared_networks mirrors the policy in file
// order, and is [] when the keys are unset.
func TestHelloSharedNetworks(t *testing.T) {
	proc, sys, passwd := fixtureProc(t, 9)
	h := &fakeHelper{fn: func(verb string, body []byte) ([]byte, error) {
		if verb == "list" {
			return programs.EncodeList(nil), nil
		}
		return nil, errors.New(verb)
	}}
	path := writeRunner(t, true, false)
	_, raw := helloFrame(t, Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd, GroupPath: joeGroup(t),
		PolicyPath: path, PolicyUID: rootUID, Helper: h,
	})
	if !strings.Contains(string(raw), `"projects":["tour-agent","acme"],"shared_networks":[]`) {
		t.Fatalf("%s", raw)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.WriteString("shared_networks = [\"zz=192.168.40.0/24\", \"dd=10.200.10.0/24\"]\n" +
		"shared_network_projects = [\"dd:acme\", \"dd:tour-agent\"]\n")
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	hello, raw := helloFrame(t, Options{
		ProcRoot: proc, SysRoot: sys, Passwd: passwd, GroupPath: joeGroup(t),
		PolicyPath: path, PolicyUID: rootUID, Helper: h,
	})
	want := `"shared_networks":[{"name":"zz","subnet":"192.168.40.0/24","projects":[]},{"name":"dd","subnet":"10.200.10.0/24","projects":["acme","tour-agent"]}]`
	if !strings.Contains(string(raw), want) {
		t.Fatalf("%s", raw)
	}
	if hello.Programs == nil || len(hello.Programs.SharedNetworks) != 2 {
		t.Fatalf("%+v", hello.Programs)
	}
}

// An invalid policy advertises no networks: [] and never null.
func TestHelloInvalidPolicyNoSharedNetworks(t *testing.T) {
	a := &agent{opt: Options{Log: stdLog(&bytes.Buffer{})}}
	a.policy.Exists = true
	a.policy.SharedNetworks = []policy.SharedNetwork{{Name: "dd", Subnet: "10.200.10.0/24", Projects: []string{"x"}}}
	p, _ := a.helloPrograms()
	b, err := json.Marshal(p)
	if err != nil || !strings.Contains(string(b), `"shared_networks":[]`) {
		t.Fatalf("%s %v", b, err)
	}
}

// The hello validator bounds the list like the policy does.
func TestHelloSharedNetworksBounds(t *testing.T) {
	base := protocol.Hello{
		V: protocol.Version, Type: "hello", HostID: "h", CredentialID: "c", AgentVersion: protocol.AgentVersion,
		OS: "linux", Arch: "arm64", Capabilities: []string{"metrics", "probe"},
		Programs: &protocol.Programs{RunAs: []string{}, Projects: []string{}},
		Services: []protocol.Discovered{}, ServicesComplete: true,
	}
	validate := func(h protocol.Hello) error {
		b, err := json.Marshal(h)
		if err != nil {
			return err
		}
		return protocol.ValidateAgent(b)
	}
	nine := make([]protocol.SharedNetwork, 9)
	for i := range nine {
		nine[i] = protocol.SharedNetwork{Name: "n", Subnet: "10.0.0.0/24", Projects: []string{}}
	}
	members := make([]string, 33)
	for i := range members {
		members[i] = "p"
	}
	for name, list := range map[string][]protocol.SharedNetwork{
		"nine":         nine,
		"33 members":   {{Name: "dd", Subnet: "10.0.0.0/24", Projects: members}},
		"null members": {{Name: "dd", Subnet: "10.0.0.0/24"}},
		"no subnet":    {{Name: "dd", Projects: []string{}}},
	} {
		h := base
		p := *base.Programs
		p.SharedNetworks = list
		h.Programs = &p
		if err := validate(h); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	h := base
	p := *base.Programs
	p.SharedNetworks = []protocol.SharedNetwork{{Name: "dd", Subnet: "10.0.0.0/24", Projects: []string{"a"}}}
	h.Programs = &p
	if err := validate(h); err != nil {
		t.Fatal(err)
	}
}
