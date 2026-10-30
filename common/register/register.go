// Package register is the protocol between the OCI hook (redoma oci-hook,
// run as root by the container runtime) and the control plane's hook Source
// (redoma-xds), on the control plane's registration socket.
//
// One request per connection, one JSON object each way: the hook sends a
// Request and reads one Response. A register is answered only once Envoy has
// bound the agent's listeners in its namespace (or it failed), so the hook
// can hold the container's start until then.
package register

import (
	"encoding/json"
	"errors"
	"io"
)

// Ops of a Request.
const (
	OpRegister   = "register"
	OpUnregister = "unregister"
)

// MaxMessage bounds a message, so a client cannot make the other side read
// without end.
const MaxMessage = 64 << 10

// Request is what the hook asks.
type Request struct {
	// Op is OpRegister or OpUnregister.
	Op string `json:"op"`
	// Container is the OCI container ID, which names the registration.
	Container string `json:"container"`
	// Agent is the agent's name (register only): its listeners' names, its
	// egress allowlist and its SPIFFE ID derive from it.
	Agent string `json:"agent,omitempty"`
	// Netns is the path of the bind mount of the container's network
	// namespace (register only).
	Netns string `json:"netns,omitempty"`
	// Pidfd says that a pidfd of the container's process comes with the
	// request (register only), as SCM_RIGHTS data: WriteRequest and
	// ReadRequest.
	Pidfd bool `json:"pidfd,omitempty"`
}

// Response is the control plane's answer.
type Response struct {
	// Error is empty on success.
	Error string `json:"error,omitempty"`
}

// Write sends one message.
func Write(w io.Writer, v any) error {
	return json.NewEncoder(w).Encode(v)
}

// Read reads one message into v.
func Read(r io.Reader, v any) error {
	dec := json.NewDecoder(io.LimitReader(r, MaxMessage))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		if errors.Is(err, io.ErrUnexpectedEOF) {
			return errors.New("message truncated or over the size limit")
		}
		return err
	}
	return nil
}
