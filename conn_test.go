package netlink_test

import (
	"errors"
	"io"
	"iter"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/google/go-cmp/cmp"
	"github.com/mdlayher/netlink"
	"github.com/mdlayher/netlink/nltest"
)

// recordingSocket is a netlink.Socket that records the destination address
// passed to Send and SendMessages.
type recordingSocket struct {
	pid   uint32
	group uint32
	msgs  []netlink.Message
}

func (s *recordingSocket) Close() error { return nil }

func (s *recordingSocket) Send(m netlink.Message, pid uint32, group uint32) error {
	s.pid = pid
	s.group = group
	s.msgs = []netlink.Message{m}
	return nil
}

func (s *recordingSocket) SendMessages(msgs []netlink.Message, pid uint32) error {
	s.pid = pid
	s.group = 0
	s.msgs = msgs
	return nil
}

func (s *recordingSocket) Receive() ([]netlink.Message, error) {
	return nil, errors.New("not implemented")
}

func (s *recordingSocket) ReceiveIter() iter.Seq2[netlink.Message, error] {
	return func(yield func(netlink.Message, error) bool) {
		yield(netlink.Message{}, errors.New("not implemented"))
	}
}

func TestConnExecute(t *testing.T) {
	req := netlink.Message{
		Header: netlink.Header{
			Flags:    netlink.Request | netlink.Acknowledge,
			Sequence: 1,
		},
	}

	replies := []netlink.Message{{
		Header: netlink.Header{
			Type:     netlink.Error,
			Sequence: 1,
			PID:      1,
		},
		// Error code "success", no need to echo request back in this test
		Data: make([]byte, 4),
	}}

	c := nltest.Dial(func(_ []netlink.Message) ([]netlink.Message, error) {
		return replies, nil
	})
	defer c.Close()

	msgs, err := c.Execute(req)
	if err != nil {
		t.Fatalf("failed to execute: %v", err)
	}

	// Fill in fields for comparison
	req.Header.Length = 16

	if want, got := replies, msgs; !reflect.DeepEqual(want, got) {
		t.Fatalf("unexpected replies:\n- want: %#v\n-  got: %#v",
			want, got)
	}
}

func TestConnSend(t *testing.T) {
	c := nltest.Dial(func(_ []netlink.Message) ([]netlink.Message, error) {
		return nil, errors.New("should not be received")
	})
	defer c.Close()

	// Let Conn.Send populate length, sequence, PID
	m := netlink.Message{}

	out, err := c.Send(m)
	if err != nil {
		t.Fatalf("failed to send message: %v", err)
	}

	// Make the same changes that Conn.Send should
	m = netlink.Message{
		Header: netlink.Header{
			Length:   16,
			Sequence: out.Header.Sequence,
			PID:      1,
		},
	}

	if want, got := m, out; !reflect.DeepEqual(want, got) {
		t.Fatalf("unexpected output message from Conn.Send:\n- want: %#v\n-  got: %#v",
			want, got)
	}

	// Keep sending to verify sequence number increment
	seq := m.Header.Sequence
	for range 100 {
		out, err := c.Send(netlink.Message{})
		if err != nil {
			t.Fatalf("failed to send message: %v", err)
		}

		seq++
		if want, got := seq, out.Header.Sequence; want != got {
			t.Fatalf("unexpected sequence number:\n- want: %v\n-  got: %v",
				want, got)
		}
	}
}

func TestConnSendDestination(t *testing.T) {
	tests := []struct {
		name      string
		send      func(c *netlink.Conn) error
		wantPID   uint32
		wantGroup uint32
	}{
		{
			name: "Send",
			send: func(c *netlink.Conn) error {
				_, err := c.Send(netlink.Message{})
				return err
			},
		},
		{
			name: "SendTo",
			send: func(c *netlink.Conn) error {
				_, err := c.SendTo(netlink.Message{}, 42)
				return err
			},
			wantPID: 42,
		},
		{
			name: "Multicast",
			send: func(c *netlink.Conn) error {
				_, err := c.Multicast(netlink.Message{}, 0x5)
				return err
			},
			wantGroup: 0x5,
		},
		{
			name: "SendMessages",
			send: func(c *netlink.Conn) error {
				_, err := c.SendMessages([]netlink.Message{{}})
				return err
			},
		},
		{
			name: "SendMessagesTo",
			send: func(c *netlink.Conn) error {
				_, err := c.SendMessagesTo([]netlink.Message{{}}, 99)
				return err
			},
			wantPID: 99,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sock := &recordingSocket{}
			c := netlink.NewConn(sock, 1)
			defer c.Close()

			if err := tt.send(c); err != nil {
				t.Fatalf("failed to send: %v", err)
			}

			if diff := cmp.Diff(tt.wantPID, sock.pid); diff != "" {
				t.Fatalf("unexpected destination pid (-want +got):\n%s", diff)
			}
			if diff := cmp.Diff(tt.wantGroup, sock.group); diff != "" {
				t.Fatalf("unexpected destination group (-want +got):\n%s", diff)
			}
			if len(sock.msgs) == 0 {
				t.Fatal("no messages recorded by socket")
			}
		})
	}
}

func TestConnExecuteMultipart(t *testing.T) {
	msg := netlink.Message{
		Header: netlink.Header{
			Sequence: 1,
		},
		Data: []byte{0xff, 0xff, 0xff, 0xff},
	}

	c := nltest.Dial(func(_ []netlink.Message) ([]netlink.Message, error) {
		return nltest.Multipart([]netlink.Message{
			msg,
			// Will be filled with multipart done information.
			{},
		})
	})
	defer c.Close()

	msgs, err := c.Execute(msg)
	if err != nil {
		t.Fatalf("failed to receive messages: %v", err)
	}

	msg.Header.Flags |= netlink.Multi

	if want, got := []netlink.Message{msg}, msgs; !reflect.DeepEqual(want, got) {
		t.Fatalf("unexpected output messages from Conn.Receive:\n- want: %#v\n-  got: %#v",
			want, got)
	}
}

func TestConnExecuteNoMessages(t *testing.T) {
	c := nltest.Dial(func(_ []netlink.Message) ([]netlink.Message, error) {
		return nil, io.EOF
	})
	defer c.Close()

	msgs, err := c.Execute(netlink.Message{})
	if err != nil {
		t.Fatalf("failed to execute: %v", err)
	}

	if l := len(msgs); l > 0 {
		t.Fatalf("expected no messages, but got: %d", l)
	}
}

func TestConnReceiveNoMessages(t *testing.T) {
	c := nltest.Dial(func(_ []netlink.Message) ([]netlink.Message, error) {
		return nil, io.EOF
	})
	defer c.Close()

	msgs, err := c.Receive()
	if err != nil {
		t.Fatalf("failed to execute: %v", err)
	}

	if l := len(msgs); l > 0 {
		t.Fatalf("expected no messages, but got: %d", l)
	}
}

func TestConnReceiveShortErrorNumber(t *testing.T) {
	c := nltest.Dial(func(_ []netlink.Message) ([]netlink.Message, error) {
		return []netlink.Message{{
			Header: netlink.Header{
				Length: 20,
				Type:   netlink.Error,
			},
			Data: []byte{0x01},
		}}, nil
	})
	defer c.Close()

	_, err := c.Receive()
	if !strings.Contains(err.Error(), "not enough data") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnReceiveShortErrorAcknowledgementHeader(t *testing.T) {
	c := nltest.Dial(func(_ []netlink.Message) ([]netlink.Message, error) {
		return []netlink.Message{{
			Header: netlink.Header{
				Length: 20,
				Type:   netlink.Error,
				Flags:  netlink.AcknowledgeTLVs,
			},
			Data: []byte{
				// errno.
				0x01, 0x00, 0x00, 0x00,
				// nlmsghdr
				0xff,
			},
		}}, nil
	})
	defer c.Close()

	_, err := c.Receive()
	if !strings.Contains(err.Error(), "not enough data") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnJoinLeaveGroupUnsupported(t *testing.T) {
	c := nltest.Dial(nil)
	defer c.Close()

	ops := []func(group uint32) error{
		c.JoinGroup,
		c.LeaveGroup,
	}

	for _, op := range ops {
		err := op(0)
		if !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestConnSetBPFUnsupported(t *testing.T) {
	c := nltest.Dial(nil)
	defer c.Close()

	err := c.SetBPF(nil)
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnSetDeadlineUnsupported(t *testing.T) {
	c := nltest.Dial(nil)
	defer c.Close()

	err := c.SetDeadline(time.Now())
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnSetOptionUnsupported(t *testing.T) {
	c := nltest.Dial(nil)
	defer c.Close()

	err := c.SetOption(0, false)
	if !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnSetBuffersUnsupported(t *testing.T) {
	c := nltest.Dial(nil)
	defer c.Close()

	ops := []func(n int) error{
		c.SetReadBuffer,
		c.SetWriteBuffer,
	}

	for _, op := range ops {
		err := op(0)
		if !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestConnBuffersUnsupported(t *testing.T) {
	c := nltest.Dial(nil)
	defer c.Close()

	ops := []func() (int, error){
		c.ReadBuffer,
		c.WriteBuffer,
	}

	for _, op := range ops {
		_, err := op()
		if !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("unexpected error: %v", err)
		}
	}
}

func TestConnSyscallConnUnsupported(t *testing.T) {
	c := nltest.Dial(nil)
	defer c.Close()

	if _, err := c.SyscallConn(); !strings.Contains(err.Error(), "not supported") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestConnReceiveIterMultipaEarlyExit(t *testing.T) {
	msgs := []netlink.Message{
		{
			Data: []byte{0x00, 0x00, 0x00, 0x01},
		},
		{
			Data: []byte{0x00, 0x00, 0x00, 0x02},
		},
		{
			Data: []byte{0x00, 0x00, 0x00, 0x03},
		},
		{
			Data: []byte{0x00, 0x00, 0x00, 0x04},
		},
		{},
	}

	responded := false
	c := nltest.Dial(func(_ []netlink.Message) ([]netlink.Message, error) {
		if responded {
			return nil, io.EOF
		}
		responded = true
		return nltest.Multipart(msgs)
	})
	defer c.Close()

	// Send a message to trigger the multipart response.
	if _, err := c.Send(netlink.Message{}); err != nil {
		t.Fatalf("failed to send request: %v", err)
	}

	for msg, err := range c.ReceiveIter() {
		if err != nil {
			t.Fatalf("failed to receive messages: %v", err)
		}
		if diff := cmp.Diff(msgs[0], msg); diff != "" {
			t.Fatalf("unexpected message received (-want +got):\n%s", diff)
		}
		break
	}

	got, err := c.Receive()
	if err != nil {
		t.Fatalf("failed to receive messages: %v", err)
	}

	// Early exit should have drained the buffer.
	if diff := cmp.Diff([]netlink.Message(nil), got); diff != "" {
		t.Fatalf("unexpected messages after early exit from multipart response (-want +got):\n%s", diff)
	}
}
