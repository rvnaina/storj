// Copyright (C) 2019 Storj Labs, Inc.
// See LICENSE for copying information.

package drpcstream

import (
	"context"
	"io"
	"sync"
	"sync/atomic"

	"github.com/gogo/protobuf/proto"
	"storj.io/storj/drpc"
	"storj.io/storj/drpc/drpcutil"
	"storj.io/storj/drpc/drpcwire"
)

type Stream struct {
	messageID uint64
	ctx       context.Context
	cancel    func()
	streamID  uint64
	buf       *drpcutil.Buffer
	sig       *drpcutil.Signal
	sendSig   *drpcutil.Signal
	recvSig   *drpcutil.Signal
	termSig   *drpcutil.Signal
	queue     chan *drpcwire.Packet
	sendMu    sync.Mutex
}

func New(ctx context.Context, streamID uint64, buf *drpcutil.Buffer) *Stream {
	ctx, cancel := context.WithCancel(ctx)
	s := &Stream{
		ctx:      ctx,
		cancel:   cancel,
		streamID: streamID,
		buf:      buf,
		sig:      drpcutil.NewSignal(),
		sendSig:  drpcutil.NewSignal(),
		recvSig:  drpcutil.NewSignal(),
		termSig:  drpcutil.NewSignal(),
		queue:    make(chan *drpcwire.Packet, 100),
	}
	go s.monitor()
	return s
}

var _ drpc.Stream = (*Stream)(nil)

//
// exported accessors
//

func (s *Stream) Cancel()                      { s.cancel() }
func (s *Stream) Context() context.Context     { return s.ctx }
func (s *Stream) StreamID() uint64             { return s.streamID }
func (s *Stream) Sig() *drpcutil.Signal        { return s.sig }
func (s *Stream) SendSig() *drpcutil.Signal    { return s.sendSig }
func (s *Stream) RecvSig() *drpcutil.Signal    { return s.recvSig }
func (s *Stream) TermSig() *drpcutil.Signal    { return s.termSig }
func (s *Stream) Queue() chan *drpcwire.Packet { return s.queue }

//
// basic helpers
//

func (s *Stream) nextPid() drpcwire.PacketID {
	return drpcwire.PacketID{
		StreamID:  s.streamID,
		MessageID: atomic.AddUint64(&s.messageID, 1),
	}
}

func (s *Stream) monitor() {
	select {
	case <-s.sig.Signal():
		s.RawError(s.sig.Err())
	case <-s.ctx.Done():
		s.RawCancel()
	}
}

func (s *Stream) pollSend() (error, bool) {
	if err, ok := s.sig.Get(); ok {
		return err, false
	}
	if err, ok := s.termSig.Get(); ok {
		return err, false
	}
	if err, ok := s.sendSig.Get(); ok {
		return err, true
	}
	return nil, false
}

func (s *Stream) wireSendFlush(kind drpcwire.PayloadKind, data []byte) error {
	if err := drpcwire.Split(kind, s.nextPid(), data, s.buf.Write); err != nil {
		return err
	}
	return s.buf.Flush()
}

//
// raw send/recv/close primitives
//

func (s *Stream) RawSend(kind drpcwire.PayloadKind, data []byte) error {
	err := drpcwire.Split(kind, s.nextPid(), data, func(pkt drpcwire.Packet) error {
		s.sendMu.Lock()
		defer s.sendMu.Unlock()
		if err, _ := s.pollSend(); err != nil {
			return err
		}
		return s.buf.Write(pkt)
	})
	if err != nil {
		s.sig.Set(err)
		return err
	}
	return nil
}

func (s *Stream) RawFlush() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	if err, _ := s.pollSend(); err != nil {
		return err
	}
	if err := s.buf.Flush(); err != nil {
		s.sig.Set(err)
		return err
	}
	return nil
}

func (s *Stream) RawRecv() (*drpcwire.Packet, error) {
	if err, ok := s.sig.Get(); ok {
		return nil, err
	}
	select {
	case <-s.sig.Signal():
		return nil, s.sig.Err()
	case p, ok := <-s.queue:
		if !ok {
			return nil, io.EOF
		}
		return p, nil
	}
}

func (s *Stream) RawError(err error) {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	defer s.termSig.Set(drpc.Error.New("stream terminated"))

	if _, ok := s.termSig.Get(); !ok {
		_ = s.wireSendFlush(drpcwire.PayloadKind_Error, []byte(err.Error()))
	}
}

func (s *Stream) RawCancel() {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	defer s.termSig.Set(drpc.Error.New("stream terminated"))
	defer s.sig.Set(context.Canceled)

	if _, ok := s.termSig.Get(); !ok {
		_ = s.wireSendFlush(drpcwire.PayloadKind_Cancel, nil)
	}
}

//
// drpc.Stream implementation
//

func (s *Stream) MsgSend(msg drpc.Message) error {
	data, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	if err := s.RawSend(drpcwire.PayloadKind_Message, data); err != nil {
		return err
	}
	if err := s.RawFlush(); err != nil {
		return err
	}
	return nil
}

func (s *Stream) MsgRecv(msg drpc.Message) error {
	p, err := s.RawRecv()
	if err != nil {
		return err
	}
	return proto.Unmarshal(p.Data, msg)
}

func (s *Stream) CloseSend() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	defer s.sendSig.Set(drpc.Error.New("send after CloseSend"))

	if err, sendClosed := s.pollSend(); sendClosed {
		return nil
	} else if err != nil {
		return err
	}
	return s.wireSendFlush(drpcwire.PayloadKind_CloseSend, nil)
}

func (s *Stream) Close() error {
	s.sendMu.Lock()
	defer s.sendMu.Unlock()
	defer s.sendSig.Set(drpc.Error.New("send after CloseSend"))
	defer s.termSig.Set(drpc.Error.New("stream terminated"))
	defer s.sig.Set(drpc.Error.New("stream closed"))

	if err, sendClosed := s.pollSend(); err != nil && !sendClosed {
		return nil
	}
	return s.wireSendFlush(drpcwire.PayloadKind_CloseSend, nil)
}
