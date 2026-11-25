package main

import (
	"strings"
	"testing"
)

// v1.24: policy check prints container_cgroupns last, host when root set it,
// and refuses a value other than "private" or "host".
func TestPolicyCheckContainerCgroupns(t *testing.T) {
	out, err := runPolicyCheck(t, writePolicy(t, "container_cgroupns = \"host\"\n"))
	if err != nil || !strings.HasSuffix(out, "console_policy=false\ncontainer_cgroupns=host\n") {
		t.Fatalf("%v %q", err, out)
	}
	out, err = runPolicyCheck(t, writePolicy(t, "container_cgroupns = \"Host\"\n"))
	if err == nil || out != "invalid: parse\n" {
		t.Fatalf("%v %q", err, out)
	}
}
