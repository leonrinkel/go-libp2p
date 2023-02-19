package libp2pwebrtc

import (
	"errors"
	"fmt"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"github.com/libp2p/go-libp2p/p2p/transport/webrtc/internal/async"
	pb "github.com/libp2p/go-libp2p/p2p/transport/webrtc/pb"
	"github.com/libp2p/go-msgio/pbio"
	"github.com/pion/datachannel"
	"google.golang.org/protobuf/proto"
)

// Package pion detached data channel into a net.Conn
// and then a network.MuxedStream
type (
	webRTCStreamWriter struct {
		stream *webRTCStream

		writer    pbio.Writer
		writerMux sync.Mutex

		deadline    time.Time
		deadlineMux sync.Mutex

		deadlineUpdated async.CondVar
		writeAvailable  async.CondVar

		readLoopOnce sync.Once
		closeOnce    sync.Once
	}
)

func (w *webRTCStreamWriter) Write(b []byte) (int, error) {
	if !w.stream.stateHandler.AllowWrite() {
		return 0, io.ErrClosedPipe
	}

	// Check if there is any message on the wire. This is used for control
	// messages only when the read side of the stream is closed
	if w.stream.stateHandler.State() == stateReadClosed {
		w.readLoopOnce.Do(func() {
			go func() {
				// zero the read deadline, so read call only returns
				// when the underlying datachannel closes or there is
				// a message on the channel
				w.stream.rwc.(*datachannel.DataChannel).SetReadDeadline(time.Time{})
				var msg pb.Message
				for {
					if w.stream.stateHandler.Closed() {
						return
					}
					err := w.stream.reader.readMessageFromDataChannel(&msg)
					if err != nil {
						if errors.Is(err, io.EOF) {
							w.stream.close(true, true)
						}
						return
					}
					if msg.Flag != nil {
						state, reset := w.stream.stateHandler.HandleInboundFlag(msg.GetFlag())
						if state == stateClosed {
							log.Debug("closing: after handle inbound flag")
							w.stream.close(reset, true)
						}
					}
				}
			}()
		})
	}

	const chunkSize = maxMessageSize - protoOverhead - varintOverhead

	var (
		err error
		n   int
	)

	for len(b) > 0 {
		end := len(b)
		if chunkSize < end {
			end = chunkSize
		}

		written, err := w.writeMessage(&pb.Message{Message: b[:end]})
		n += written
		if err != nil {
			return n, err
		}
		b = b[end:]
	}
	return n, err
}

func (w *webRTCStreamWriter) writeMessage(msg *pb.Message) (int, error) {
	var writeDeadlineTimer *time.Timer
	defer func() {
		if writeDeadlineTimer != nil {
			writeDeadlineTimer.Stop()
		}
	}()

	for {
		if !w.stream.stateHandler.AllowWrite() {
			return 0, io.ErrClosedPipe
		}
		// prepare waiting for writeAvailable signal
		// if write is blocked
		deadlineUpdated := w.deadlineUpdated.Wait()
		writeAvailable := w.writeAvailable.Wait()

		writeDeadline, hasWriteDeadline := w.getWriteDeadline()
		if !hasWriteDeadline {
			writeDeadline = time.Unix(math.MaxInt64, 0)
		}
		if writeDeadlineTimer == nil {
			writeDeadlineTimer = time.NewTimer(time.Until(writeDeadline))
		} else {
			writeDeadlineTimer.Reset(time.Until(writeDeadline))
		}

		bufferedAmount := int(w.stream.rwc.(*datachannel.DataChannel).BufferedAmount())
		addedBuffer := bufferedAmount + varintOverhead + proto.Size(msg)
		if addedBuffer > maxBufferedAmount {
			select {
			case <-writeDeadlineTimer.C:
				return 0, os.ErrDeadlineExceeded
			case <-writeAvailable:
				err := w.writeMessageToWriter(msg)
				if err != nil {
					return 0, err
				}
				return int(len(msg.Message)), nil
			case <-w.stream.ctx.Done():
				return 0, io.ErrClosedPipe
			case <-deadlineUpdated:
			}
		} else {
			err := w.writeMessageToWriter(msg)
			if err != nil {
				return 0, err
			}
			return int(len(msg.Message)), nil
		}
	}
}

func (w *webRTCStreamWriter) writeMessageToWriter(msg *pb.Message) error {
	w.writerMux.Lock()
	defer w.writerMux.Unlock()
	return w.writer.WriteMsg(msg)
}

func (w *webRTCStreamWriter) SetWriteDeadline(t time.Time) error {
	w.deadlineMux.Lock()
	defer w.deadlineMux.Unlock()
	w.deadline = t
	w.deadlineUpdated.Signal()
	return nil
}

func (w *webRTCStreamWriter) getWriteDeadline() (time.Time, bool) {
	w.deadlineMux.Lock()
	defer w.deadlineMux.Unlock()
	return w.deadline, !w.deadline.IsZero()
}

func (w *webRTCStreamWriter) CloseWrite() error {
	if w.stream.isClosed() {
		return nil
	}
	var err error
	w.closeOnce.Do(func() {
		_, err = w.writeMessage(&pb.Message{Flag: pb.Message_FIN.Enum()})
		if err != nil {
			log.Debug("could not write FIN message")
			err = fmt.Errorf("close stream for writing: %w", err)
			return
		}
		// if successfully written, process the outgoing flag
		state := w.stream.stateHandler.CloseRead()
		// unblock and fail any ongoing writes
		w.writeAvailable.Signal()
		// check if closure required
		if state == stateClosed {
			w.stream.close(false, true)
		}
	})
	return err
}
