package libp2pwebrtc

import (
	"errors"
	"os"
	"time"

	"github.com/libp2p/go-libp2p/core/network"
	"github.com/libp2p/go-libp2p/p2p/transport/webrtc/pb"
)

var errWriteAfterClose = errors.New("write after close")

// If we have less space than minMessageSize, we don't put a new message on the data channel.
// Instead, we wait until more space opens up.
const minMessageSize = 1 << 10

func (s *stream) Write(b []byte) (int, error) {
	s.mx.Lock()
	defer s.mx.Unlock()

	if s.closeErr != nil {
		return 0, s.closeErr
	}
	switch s.sendState {
	case sendStateReset:
		return 0, network.ErrReset
	case sendStateDataSent, sendStateDataReceived:
		return 0, errWriteAfterClose
	}

	if !s.writeDeadline.IsZero() && time.Now().After(s.writeDeadline) {
		return 0, os.ErrDeadlineExceeded
	}

	var writeDeadlineTimer *time.Timer
	defer func() {
		if writeDeadlineTimer != nil {
			writeDeadlineTimer.Stop()
		}
	}()

	var n int
	for len(b) > 0 {
		if s.closeErr != nil {
			return n, s.closeErr
		}
		switch s.sendState {
		case sendStateReset:
			return n, network.ErrReset
		case sendStateDataSent, sendStateDataReceived:
			return n, errWriteAfterClose
		}

		writeDeadline := s.writeDeadline
		// deadline deleted, stop and remove the timer
		if writeDeadline.IsZero() && writeDeadlineTimer != nil {
			writeDeadlineTimer.Stop()
			writeDeadlineTimer = nil
		}
		var writeDeadlineChan <-chan time.Time
		if !writeDeadline.IsZero() {
			if writeDeadlineTimer == nil {
				writeDeadlineTimer = time.NewTimer(time.Until(writeDeadline))
			} else {
				if !writeDeadlineTimer.Stop() {
					<-writeDeadlineTimer.C
				}
				writeDeadlineTimer.Reset(time.Until(writeDeadline))
			}
			writeDeadlineChan = writeDeadlineTimer.C
		}

		availableSpace := s.availableSendSpace()
		if availableSpace < minMessageSize {
			s.mx.Unlock()
			select {
			case <-s.writeAvailable:
			case <-writeDeadlineChan:
				s.mx.Lock()
				return n, os.ErrDeadlineExceeded
			case <-s.sendStateChanged:
			case <-s.writeDeadlineUpdated:
			}
			s.mx.Lock()
			continue
		}
		end := maxMessageSize
		if end > availableSpace {
			end = availableSpace
		}
		end -= protoOverhead + varintOverhead
		if end > len(b) {
			end = len(b)
		}
		msg := &pb.Message{Message: b[:end]}
		if err := s.writer.WriteMsg(msg); err != nil {
			return n, err
		}
		n += end
		b = b[end:]
	}
	return n, nil
}

func (s *stream) SetWriteDeadline(t time.Time) error {
	s.mx.Lock()
	defer s.mx.Unlock()
	s.writeDeadline = t
	select {
	case s.writeDeadlineUpdated <- struct{}{}:
	default:
	}
	return nil
}

func (s *stream) availableSendSpace() int {
	buffered := int(s.dataChannel.BufferedAmount())
	availableSpace := maxBufferedAmount - buffered
	if availableSpace < 0 { // this should never happen, but better check
		log.Errorw("data channel buffered more data than the maximum amount", "max", maxBufferedAmount, "buffered", buffered)
	}
	return availableSpace
}

func (s *stream) cancelWrite() error {
	s.mx.Lock()
	defer s.mx.Unlock()

	// Don't wait for FIN_ACK on reset
	if s.sendState != sendStateSending && s.sendState != sendStateDataSent {
		return nil
	}
	s.sendState = sendStateReset
	select {
	case s.sendStateChanged <- struct{}{}:
	default:
	}
	if err := s.writer.WriteMsg(&pb.Message{Flag: pb.Message_RESET.Enum()}); err != nil {
		return err
	}
	return nil
}

func (s *stream) CloseWrite() error {
	s.mx.Lock()
	defer s.mx.Unlock()

	if s.sendState != sendStateSending {
		return nil
	}
	s.sendState = sendStateDataSent
	select {
	case s.sendStateChanged <- struct{}{}:
	default:
	}
	if err := s.writer.WriteMsg(&pb.Message{Flag: pb.Message_FIN.Enum()}); err != nil {
		return err
	}
	return nil
}
