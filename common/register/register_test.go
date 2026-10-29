package register

import (
	"bytes"
	"strings"
	"testing"
)

func TestRoundTrip(t *testing.T) {
	var buf bytes.Buffer
	cid := strings.Repeat("ab", 32)
	in := Request{Op: OpRegister, Container: cid, Agent: "a", Netns: NetnsPath("/run/netns", cid)}
	if err := Write(&buf, in); err != nil {
		t.Fatal(err)
	}
	var out Request
	if err := Read(&buf, &out); err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Errorf("read %+v, want %+v", out, in)
	}
	if out.Netns != "/run/netns/redoma-oci-"+cid {
		t.Errorf("netns path %q", out.Netns)
	}
}

func TestReadRefuses(t *testing.T) {
	var r Request
	if err := Read(strings.NewReader(`{"op":"register","pid":1}`), &r); err == nil {
		t.Error("an unknown field was accepted")
	}
	big := `{"op":"` + strings.Repeat("x", MaxMessage) + `"}`
	if err := Read(strings.NewReader(big), &r); err == nil {
		t.Error("a message over the limit was accepted")
	}
}

func TestValidContainer(t *testing.T) {
	for id, want := range map[string]bool{
		strings.Repeat("a", 64): true,
		"0123456789ab":          false, // redoma-oci-0123456789ab is a launcher's agent oci-0123456789ab
		strings.Repeat("a", 63): false,
		"../../etc/passwd":      false,
		"ABCDEF012345":          false,
		strings.Repeat("a", 65): false,
	} {
		if got := ValidContainer(id); got != want {
			t.Errorf("ValidContainer(%q) = %v", id, got)
		}
	}
}
