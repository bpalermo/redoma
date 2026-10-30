package register

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"runtime"
	"syscall"
)

// maxFiles bounds the descriptors read with a request: one is allowed, the
// rest only to be closed and refused.
const maxFiles = 4

// WriteRequest sends req, and f with it (SCM_RIGHTS) when f is not nil:
// req.Pidfd must then be set, and conn must be a unix socket.
func WriteRequest(conn net.Conn, req Request, f *os.File) error {
	if f == nil {
		return Write(conn, req)
	}
	if !req.Pidfd {
		return errors.New("a descriptor sent with a request that does not announce it")
	}
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		return fmt.Errorf("a descriptor needs a unix socket, not %T", conn)
	}
	data, err := json.Marshal(req)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	// One sendmsg: the descriptor travels with the message's first byte.
	n, _, err := uc.WriteMsgUnix(data, syscall.UnixRights(int(f.Fd())), nil)
	runtime.KeepAlive(f)
	if err != nil {
		return err
	}
	if n != len(data) {
		return io.ErrShortWrite
	}
	return nil
}

// ReadRequest reads one request and the descriptor that came with it, if
// any: exactly one when req.Pidfd is set, none otherwise. Any other count
// is refused, and every descriptor received is then closed.
func ReadRequest(conn net.Conn) (Request, *os.File, error) {
	var req Request
	uc, ok := conn.(*net.UnixConn)
	if !ok {
		if err := Read(conn, &req); err != nil {
			return Request{}, nil, err
		}
		if req.Pidfd {
			return Request{}, nil, errors.New("the request announces a pidfd, but not over a unix socket")
		}
		return req, nil, nil
	}
	buf := make([]byte, MaxMessage)
	oob := make([]byte, syscall.CmsgSpace(maxFiles*4))
	n, oobn, flags, _, err := uc.ReadMsgUnix(buf, oob)
	files, ferr := rights(oob[:oobn])
	if err == nil {
		err = ferr
	}
	if err == nil && flags&syscall.MSG_CTRUNC != 0 {
		err = errors.New("too many descriptors with the request")
	}
	if err == nil {
		err = Read(io.MultiReader(bytes.NewReader(buf[:n]), conn), &req)
	}
	if err == nil {
		err = matchPidfd(req, files)
	}
	if err != nil {
		closeAll(files)
		return Request{}, nil, err
	}
	if len(files) == 0 {
		return req, nil, nil
	}
	return req, files[0], nil
}

// rights are the descriptors of a message's control data.
func rights(oob []byte) ([]*os.File, error) {
	msgs, err := syscall.ParseSocketControlMessage(oob)
	if err != nil {
		return nil, err
	}
	var files []*os.File
	for _, m := range msgs {
		fds, err := syscall.ParseUnixRights(&m)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			syscall.CloseOnExec(fd)
			files = append(files, os.NewFile(uintptr(fd), "received"))
		}
	}
	return files, nil
}

func matchPidfd(req Request, files []*os.File) error {
	switch {
	case req.Pidfd && len(files) == 1, !req.Pidfd && len(files) == 0:
		return nil
	case req.Pidfd:
		return fmt.Errorf("the request announces a pidfd and came with %d descriptors", len(files))
	default:
		return fmt.Errorf("the request came with %d descriptors it does not announce", len(files))
	}
}

func closeAll(files []*os.File) {
	for _, f := range files {
		f.Close()
	}
}
