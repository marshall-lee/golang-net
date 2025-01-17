// Copyright 2023 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

//go:build go1.21

package quic

import (
	"context"
	"errors"
	"fmt"
	"io"
)

type Stream struct {
	id   streamID
	conn *Conn

	// ingate's lock guards all receive-related state.
	//
	// The gate condition is set if a read from the stream will not block,
	// either because the stream has available data or because the read will fail.
	ingate      gate
	in          pipe            // received data
	inwin       int64           // last MAX_STREAM_DATA sent to the peer
	insendmax   sentVal         // set when we should send MAX_STREAM_DATA to the peer
	inmaxbuf    int64           // maximum amount of data we will buffer
	insize      int64           // stream final size; -1 before this is known
	inset       rangeset[int64] // received ranges
	inclosed    sentVal         // set by CloseRead
	inresetcode int64           // RESET_STREAM code received from the peer; -1 if not reset

	// outgate's lock guards all send-related state.
	//
	// The gate condition is set if a write to the stream will not block,
	// either because the stream has available flow control or because
	// the write will fail.
	outgate      gate
	out          pipe            // buffered data to send
	outwin       int64           // maximum MAX_STREAM_DATA received from the peer
	outmaxbuf    int64           // maximum amount of data we will buffer
	outunsent    rangeset[int64] // ranges buffered but not yet sent
	outacked     rangeset[int64] // ranges sent and acknowledged
	outopened    sentVal         // set if we should open the stream
	outclosed    sentVal         // set by CloseWrite
	outblocked   sentVal         // set when a write to the stream is blocked by flow control
	outreset     sentVal         // set by Reset
	outresetcode uint64          // reset code to send in RESET_STREAM
	outdone      chan struct{}   // closed when all data sent

	// Atomic stream state bits.
	//
	// These bits provide a fast way to coordinate between the
	// send and receive sides of the stream, and the conn's loop.
	//
	// streamIn* bits must be set with ingate held.
	// streamOut* bits must be set with outgate held.
	// streamConn* bits are set by the conn's loop.
	// streamQueue* bits must be set with streamsState.sendMu held.
	state atomicBits[streamState]

	prev, next *Stream // guarded by streamsState.sendMu
}

type streamState uint32

const (
	// streamInSendMeta is set when there are frames to send for the
	// inbound side of the stream. For example, MAX_STREAM_DATA.
	// Inbound frames are never flow-controlled.
	streamInSendMeta = streamState(1 << iota)

	// streamOutSendMeta is set when there are non-flow-controlled frames
	// to send for the outbound side of the stream. For example, STREAM_DATA_BLOCKED.
	// streamOutSendData is set when there are no non-flow-controlled outbound frames
	// and the stream has data to send.
	//
	// At most one of streamOutSendMeta and streamOutSendData is set at any time.
	streamOutSendMeta
	streamOutSendData

	// streamInDone and streamOutDone are set when the inbound or outbound
	// sides of the stream are finished. When both are set, the stream
	// can be removed from the Conn and forgotten.
	streamInDone
	streamOutDone

	// streamConnRemoved is set when the stream has been removed from the conn.
	streamConnRemoved

	// streamQueueMeta and streamQueueData indicate which of the streamsState
	// send queues the conn is currently on.
	streamQueueMeta
	streamQueueData
)

type streamQueue int

const (
	noQueue   = streamQueue(iota)
	metaQueue // streamsState.queueMeta
	dataQueue // streamsState.queueData
)

// wantQueue returns the send queue the stream should be on.
func (s streamState) wantQueue() streamQueue {
	switch {
	case s&(streamInSendMeta|streamOutSendMeta) != 0:
		return metaQueue
	case s&(streamInDone|streamOutDone|streamConnRemoved) == streamInDone|streamOutDone:
		return metaQueue
	case s&streamOutSendData != 0:
		// The stream has no non-flow-controlled frames to send,
		// but does have data. Put it on the data queue, which is only
		// processed when flow control is available.
		return dataQueue
	}
	return noQueue
}

// inQueue returns the send queue the stream is currently on.
func (s streamState) inQueue() streamQueue {
	switch {
	case s&streamQueueMeta != 0:
		return metaQueue
	case s&streamQueueData != 0:
		return dataQueue
	}
	return noQueue
}

// newStream returns a new stream.
//
// The stream's ingate and outgate are locked.
// (We create the stream with locked gates so after the caller
// initializes the flow control window,
// unlocking outgate will set the stream writability state.)
func newStream(c *Conn, id streamID) *Stream {
	s := &Stream{
		conn:        c,
		id:          id,
		insize:      -1, // -1 indicates the stream size is unknown
		inresetcode: -1, // -1 indicates no RESET_STREAM received
		ingate:      newLockedGate(),
		outgate:     newLockedGate(),
	}
	if !s.IsReadOnly() {
		s.outdone = make(chan struct{})
	}
	return s
}

// IsReadOnly reports whether the stream is read-only
// (a unidirectional stream created by the peer).
func (s *Stream) IsReadOnly() bool {
	return s.id.streamType() == uniStream && s.id.initiator() != s.conn.side
}

// IsWriteOnly reports whether the stream is write-only
// (a unidirectional stream created locally).
func (s *Stream) IsWriteOnly() bool {
	return s.id.streamType() == uniStream && s.id.initiator() == s.conn.side
}

// Read reads data from the stream.
// See ReadContext for more details.
func (s *Stream) Read(b []byte) (n int, err error) {
	return s.ReadContext(context.Background(), b)
}

// ReadContext reads data from the stream.
//
// ReadContext returns as soon as at least one byte of data is available.
//
// If the peer closes the stream cleanly, ReadContext returns io.EOF after
// returning all data sent by the peer.
// If the peer aborts reads on the stream, ReadContext returns
// an error wrapping StreamResetCode.
func (s *Stream) ReadContext(ctx context.Context, b []byte) (n int, err error) {
	if s.IsWriteOnly() {
		return 0, errors.New("read from write-only stream")
	}
	if err := s.ingate.waitAndLock(ctx, s.conn.testHooks); err != nil {
		return 0, err
	}
	defer func() {
		s.inUnlock()
		s.conn.handleStreamBytesReadOffLoop(int64(n)) // must be done with ingate unlocked
	}()
	if s.inresetcode != -1 {
		return 0, fmt.Errorf("stream reset by peer: %w", StreamErrorCode(s.inresetcode))
	}
	if s.inclosed.isSet() {
		return 0, errors.New("read from closed stream")
	}
	if s.insize == s.in.start {
		return 0, io.EOF
	}
	// Getting here indicates the stream contains data to be read.
	if len(s.inset) < 1 || s.inset[0].start != 0 || s.inset[0].end <= s.in.start {
		panic("BUG: inconsistent input stream state")
	}
	if size := int(s.inset[0].end - s.in.start); size < len(b) {
		b = b[:size]
	}
	start := s.in.start
	end := start + int64(len(b))
	s.in.copy(start, b)
	s.in.discardBefore(end)
	if s.insize == -1 || s.insize > s.inwin {
		if shouldUpdateFlowControl(s.inmaxbuf, s.in.start+s.inmaxbuf-s.inwin) {
			// Update stream flow control with a STREAM_MAX_DATA frame.
			s.insendmax.setUnsent()
		}
	}
	if end == s.insize {
		return len(b), io.EOF
	}
	return len(b), nil
}

// shouldUpdateFlowControl determines whether to send a flow control window update.
//
// We want to balance keeping the peer well-supplied with flow control with not sending
// many small updates.
func shouldUpdateFlowControl(maxWindow, addedWindow int64) bool {
	return addedWindow >= maxWindow/8
}

// Write writes data to the stream.
// See WriteContext for more details.
func (s *Stream) Write(b []byte) (n int, err error) {
	return s.WriteContext(context.Background(), b)
}

// WriteContext writes data to the stream.
//
// WriteContext writes data to the stream write buffer.
// Buffered data is only sent when the buffer is sufficiently full.
// Call the Flush method to ensure buffered data is sent.
//
// TODO: Implement Flush.
func (s *Stream) WriteContext(ctx context.Context, b []byte) (n int, err error) {
	if s.IsReadOnly() {
		return 0, errors.New("write to read-only stream")
	}
	canWrite := s.outgate.lock()
	for {
		// The first time through this loop, we may or may not be write blocked.
		// We exit the loop after writing all data, so on subsequent passes through
		// the loop we are always write blocked.
		if len(b) > 0 && !canWrite {
			// Our send buffer is full. Wait for the peer to ack some data.
			s.outUnlock()
			if err := s.outgate.waitAndLock(ctx, s.conn.testHooks); err != nil {
				return n, err
			}
			// Successfully returning from waitAndLockGate means we are no longer
			// write blocked. (Unlike traditional condition variables, gates do not
			// have spurious wakeups.)
		}
		if s.outreset.isSet() {
			s.outUnlock()
			return n, errors.New("write to reset stream")
		}
		if s.outclosed.isSet() {
			s.outUnlock()
			return n, errors.New("write to closed stream")
		}
		// We set outopened here rather than below,
		// so if this is a zero-length write we still
		// open the stream despite not writing any data to it.
		s.outopened.set()
		if len(b) == 0 {
			break
		}
		// Write limit is our send buffer limit.
		// This is a stream offset.
		lim := s.out.start + s.outmaxbuf
		// Amount to write is min(the full buffer, data up to the write limit).
		// This is a number of bytes.
		nn := min(int64(len(b)), lim-s.out.end)
		// Copy the data into the output buffer and mark it as unsent.
		if s.out.end <= s.outwin {
			s.outunsent.add(s.out.end, min(s.out.end+nn, s.outwin))
		}
		s.out.writeAt(b[:nn], s.out.end)
		b = b[nn:]
		n += int(nn)
		if s.out.end > s.outwin {
			// We're blocked by flow control.
			// Send a STREAM_DATA_BLOCKED frame to let the peer know.
			s.outblocked.set()
		}
		// If we have bytes left to send, we're blocked.
		canWrite = false
	}
	s.outUnlock()
	return n, nil
}

// Close closes the stream.
// See CloseContext for more details.
func (s *Stream) Close() error {
	return s.CloseContext(context.Background())
}

// CloseContext closes the stream.
// Any blocked stream operations will be unblocked and return errors.
//
// CloseContext flushes any data in the stream write buffer and waits for the peer to
// acknowledge receipt of the data.
// If the stream has been reset, it waits for the peer to acknowledge the reset.
// If the context expires before the peer receives the stream's data,
// CloseContext discards the buffer and returns the context error.
func (s *Stream) CloseContext(ctx context.Context) error {
	s.CloseRead()
	if s.IsReadOnly() {
		return nil
	}
	s.CloseWrite()
	// TODO: Return code from peer's RESET_STREAM frame?
	return s.conn.waitOnDone(ctx, s.outdone)
}

// CloseRead aborts reads on the stream.
// Any blocked reads will be unblocked and return errors.
//
// CloseRead notifies the peer that the stream has been closed for reading.
// It does not wait for the peer to acknowledge the closure.
// Use CloseContext to wait for the peer's acknowledgement.
func (s *Stream) CloseRead() {
	if s.IsWriteOnly() {
		return
	}
	s.ingate.lock()
	if s.inset.isrange(0, s.insize) || s.inresetcode != -1 {
		// We've already received all data from the peer,
		// so there's no need to send STOP_SENDING.
		// This is the same as saying we sent one and they got it.
		s.inclosed.setReceived()
	} else {
		s.inclosed.set()
	}
	discarded := s.in.end - s.in.start
	s.in.discardBefore(s.in.end)
	s.inUnlock()
	s.conn.handleStreamBytesReadOffLoop(discarded) // must be done with ingate unlocked
}

// CloseWrite aborts writes on the stream.
// Any blocked writes will be unblocked and return errors.
//
// CloseWrite sends any data in the stream write buffer to the peer.
// It does not wait for the peer to acknowledge receipt of the data.
// Use CloseContext to wait for the peer's acknowledgement.
func (s *Stream) CloseWrite() {
	if s.IsReadOnly() {
		return
	}
	s.outgate.lock()
	defer s.outUnlock()
	s.outclosed.set()
}

// Reset aborts writes on the stream and notifies the peer
// that the stream was terminated abruptly.
// Any blocked writes will be unblocked and return errors.
//
// Reset sends the application protocol error code, which must be
// less than 2^62, to the peer.
// It does not wait for the peer to acknowledge receipt of the error.
// Use CloseContext to wait for the peer's acknowledgement.
//
// Reset does not affect reads.
// Use CloseRead to abort reads on the stream.
func (s *Stream) Reset(code uint64) {
	const userClosed = true
	s.resetInternal(code, userClosed)
}

// resetInternal resets the send side of the stream.
//
// If userClosed is true, this is s.Reset.
// If userClosed is false, this is a reaction to a STOP_SENDING frame.
func (s *Stream) resetInternal(code uint64, userClosed bool) {
	s.outgate.lock()
	defer s.outUnlock()
	if s.IsReadOnly() {
		return
	}
	if userClosed {
		// Mark that the user closed the stream.
		s.outclosed.set()
	}
	if s.outreset.isSet() {
		return
	}
	if code > maxVarint {
		code = maxVarint
	}
	// We could check here to see if the stream is closed and the
	// peer has acked all the data and the FIN, but sending an
	// extra RESET_STREAM in this case is harmless.
	s.outreset.set()
	s.outresetcode = code
	s.out.discardBefore(s.out.end)
	s.outunsent = rangeset[int64]{}
	s.outblocked.clear()
}

// inUnlock unlocks s.ingate.
// It sets the gate condition if reads from s will not block.
// If s has receive-related frames to write or if both directions
// are done and the stream should be removed, it notifies the Conn.
func (s *Stream) inUnlock() {
	state := s.inUnlockNoQueue()
	s.conn.maybeQueueStreamForSend(s, state)
}

// inUnlockNoQueue is inUnlock,
// but reports whether s has frames to write rather than notifying the Conn.
func (s *Stream) inUnlockNoQueue() streamState {
	canRead := s.inset.contains(s.in.start) || // data available to read
		s.insize == s.in.start || // at EOF
		s.inresetcode != -1 || // reset by peer
		s.inclosed.isSet() // closed locally
	defer s.ingate.unlock(canRead)
	var state streamState
	switch {
	case s.IsWriteOnly():
		state = streamInDone
	case s.inresetcode != -1: // reset by peer
		fallthrough
	case s.in.start == s.insize: // all data received and read
		// We don't increase MAX_STREAMS until the user calls ReadClose or Close,
		// so the receive side is not finished until inclosed is set.
		if s.inclosed.isSet() {
			state = streamInDone
		}
	case s.insendmax.shouldSend(): // STREAM_MAX_DATA
		state = streamInSendMeta
	case s.inclosed.shouldSend(): // STOP_SENDING
		state = streamInSendMeta
	}
	const mask = streamInDone | streamInSendMeta
	return s.state.set(state, mask)
}

// outUnlock unlocks s.outgate.
// It sets the gate condition if writes to s will not block.
// If s has send-related frames to write or if both directions
// are done and the stream should be removed, it notifies the Conn.
func (s *Stream) outUnlock() {
	state := s.outUnlockNoQueue()
	s.conn.maybeQueueStreamForSend(s, state)
}

// outUnlockNoQueue is outUnlock,
// but reports whether s has frames to write rather than notifying the Conn.
func (s *Stream) outUnlockNoQueue() streamState {
	isDone := s.outclosed.isReceived() && s.outacked.isrange(0, s.out.end) || // all data acked
		s.outreset.isSet() // reset locally
	if isDone {
		select {
		case <-s.outdone:
		default:
			if !s.IsReadOnly() {
				close(s.outdone)
			}
		}
	}
	lim := s.out.start + s.outmaxbuf
	canWrite := lim > s.out.end || // available send buffer
		s.outclosed.isSet() || // closed locally
		s.outreset.isSet() // reset locally
	defer s.outgate.unlock(canWrite)
	var state streamState
	switch {
	case s.IsReadOnly():
		state = streamOutDone
	case s.outclosed.isReceived() && s.outacked.isrange(0, s.out.end): // all data sent and acked
		fallthrough
	case s.outreset.isReceived(): // RESET_STREAM sent and acked
		// We don't increase MAX_STREAMS until the user calls WriteClose or Close,
		// so the send side is not finished until outclosed is set.
		if s.outclosed.isSet() {
			state = streamOutDone
		}
	case s.outreset.shouldSend(): // RESET_STREAM
		state = streamOutSendMeta
	case s.outreset.isSet(): // RESET_STREAM sent but not acknowledged
	case s.outblocked.shouldSend(): // STREAM_DATA_BLOCKED
		state = streamOutSendMeta
	case len(s.outunsent) > 0: // STREAM frame with data
		state = streamOutSendData
	case s.outclosed.shouldSend(): // STREAM frame with FIN bit, all data already sent
		state = streamOutSendMeta
	case s.outopened.shouldSend(): // STREAM frame with no data
		state = streamOutSendMeta
	}
	const mask = streamOutDone | streamOutSendMeta | streamOutSendData
	return s.state.set(state, mask)
}

// handleData handles data received in a STREAM frame.
func (s *Stream) handleData(off int64, b []byte, fin bool) error {
	s.ingate.lock()
	defer s.inUnlock()
	end := off + int64(len(b))
	if err := s.checkStreamBounds(end, fin); err != nil {
		return err
	}
	if s.inclosed.isSet() || s.inresetcode != -1 {
		// The user read-closed the stream, or the peer reset it.
		// Either way, we can discard this frame.
		return nil
	}
	if s.insize == -1 && end > s.in.end {
		added := end - s.in.end
		if err := s.conn.handleStreamBytesReceived(added); err != nil {
			return err
		}
	}
	s.in.writeAt(b, off)
	s.inset.add(off, end)
	if fin {
		s.insize = end
		// The peer has enough flow control window to send the entire stream.
		s.insendmax.clear()
	}
	return nil
}

// handleReset handles a RESET_STREAM frame.
func (s *Stream) handleReset(code uint64, finalSize int64) error {
	s.ingate.lock()
	defer s.inUnlock()
	const fin = true
	if err := s.checkStreamBounds(finalSize, fin); err != nil {
		return err
	}
	if s.inresetcode != -1 {
		// The stream was already reset.
		return nil
	}
	if s.insize == -1 {
		added := finalSize - s.in.end
		if err := s.conn.handleStreamBytesReceived(added); err != nil {
			return err
		}
	}
	s.conn.handleStreamBytesReadOnLoop(finalSize - s.in.start)
	s.in.discardBefore(s.in.end)
	s.inresetcode = int64(code)
	s.insize = finalSize
	return nil
}

// checkStreamBounds validates the stream offset in a STREAM or RESET_STREAM frame.
func (s *Stream) checkStreamBounds(end int64, fin bool) error {
	if end > s.inwin {
		// The peer sent us data past the maximum flow control window we gave them.
		return localTransportError(errFlowControl)
	}
	if s.insize != -1 && end > s.insize {
		// The peer sent us data past the final size of the stream they previously gave us.
		return localTransportError(errFinalSize)
	}
	if fin && s.insize != -1 && end != s.insize {
		// The peer changed the final size of the stream.
		return localTransportError(errFinalSize)
	}
	if fin && end < s.in.end {
		// The peer has previously sent us data past the final size.
		return localTransportError(errFinalSize)
	}
	return nil
}

// handleStopSending handles a STOP_SENDING frame.
func (s *Stream) handleStopSending(code uint64) error {
	// Peer requests that we reset this stream.
	// https://www.rfc-editor.org/rfc/rfc9000#section-3.5-4
	const userReset = false
	s.resetInternal(code, userReset)
	return nil
}

// handleMaxStreamData handles an update received in a MAX_STREAM_DATA frame.
func (s *Stream) handleMaxStreamData(maxStreamData int64) error {
	s.outgate.lock()
	defer s.outUnlock()
	if maxStreamData <= s.outwin {
		return nil
	}
	if s.out.end > s.outwin {
		s.outunsent.add(s.outwin, min(maxStreamData, s.out.end))
	}
	s.outwin = maxStreamData
	if s.out.end > s.outwin {
		// We've still got more data than flow control window.
		s.outblocked.setUnsent()
	} else {
		s.outblocked.clear()
	}
	return nil
}

// ackOrLoss handles the fate of stream frames other than STREAM.
func (s *Stream) ackOrLoss(pnum packetNumber, ftype byte, fate packetFate) {
	// Frames which carry new information each time they are sent
	// (MAX_STREAM_DATA, STREAM_DATA_BLOCKED) must only be marked
	// as received if the most recent packet carrying this frame is acked.
	//
	// Frames which are always the same (STOP_SENDING, RESET_STREAM)
	// can be marked as received if any packet carrying this frame is acked.
	switch ftype {
	case frameTypeResetStream:
		s.outgate.lock()
		s.outreset.ackOrLoss(pnum, fate)
		s.outUnlock()
	case frameTypeStopSending:
		s.ingate.lock()
		s.inclosed.ackOrLoss(pnum, fate)
		s.inUnlock()
	case frameTypeMaxStreamData:
		s.ingate.lock()
		s.insendmax.ackLatestOrLoss(pnum, fate)
		s.inUnlock()
	case frameTypeStreamDataBlocked:
		s.outgate.lock()
		s.outblocked.ackLatestOrLoss(pnum, fate)
		s.outUnlock()
	default:
		panic("unhandled frame type")
	}
}

// ackOrLossData handles the fate of a STREAM frame.
func (s *Stream) ackOrLossData(pnum packetNumber, start, end int64, fin bool, fate packetFate) {
	s.outgate.lock()
	defer s.outUnlock()
	s.outopened.ackOrLoss(pnum, fate)
	if fin {
		s.outclosed.ackOrLoss(pnum, fate)
	}
	if s.outreset.isSet() {
		// If the stream has been reset, we don't care any more.
		return
	}
	switch fate {
	case packetAcked:
		s.outacked.add(start, end)
		s.outunsent.sub(start, end)
		// If this ack is for data at the start of the send buffer, we can now discard it.
		if s.outacked.contains(s.out.start) {
			s.out.discardBefore(s.outacked[0].end)
		}
	case packetLost:
		// Mark everything lost, but not previously acked, as needing retransmission.
		// We do this by adding all the lost bytes to outunsent, and then
		// removing everything already acked.
		s.outunsent.add(start, end)
		for _, a := range s.outacked {
			s.outunsent.sub(a.start, a.end)
		}
	}
}

// appendInFramesLocked appends STOP_SENDING and MAX_STREAM_DATA frames
// to the current packet.
//
// It returns true if no more frames need appending,
// false if not everything fit in the current packet.
func (s *Stream) appendInFramesLocked(w *packetWriter, pnum packetNumber, pto bool) bool {
	if s.inclosed.shouldSendPTO(pto) {
		// We don't currently have an API for setting the error code.
		// Just send zero.
		code := uint64(0)
		if !w.appendStopSendingFrame(s.id, code) {
			return false
		}
		s.inclosed.setSent(pnum)
	}
	// TODO: STOP_SENDING
	if s.insendmax.shouldSendPTO(pto) {
		// MAX_STREAM_DATA
		maxStreamData := s.in.start + s.inmaxbuf
		if !w.appendMaxStreamDataFrame(s.id, maxStreamData) {
			return false
		}
		s.inwin = maxStreamData
		s.insendmax.setSent(pnum)
	}
	return true
}

// appendOutFramesLocked appends RESET_STREAM, STREAM_DATA_BLOCKED, and STREAM frames
// to the current packet.
//
// It returns true if no more frames need appending,
// false if not everything fit in the current packet.
func (s *Stream) appendOutFramesLocked(w *packetWriter, pnum packetNumber, pto bool) bool {
	if s.outreset.isSet() {
		// RESET_STREAM
		if s.outreset.shouldSendPTO(pto) {
			if !w.appendResetStreamFrame(s.id, s.outresetcode, min(s.outwin, s.out.end)) {
				return false
			}
			s.outreset.setSent(pnum)
			s.frameOpensStream(pnum)
		}
		return true
	}
	if s.outblocked.shouldSendPTO(pto) {
		// STREAM_DATA_BLOCKED
		if !w.appendStreamDataBlockedFrame(s.id, s.outwin) {
			return false
		}
		s.outblocked.setSent(pnum)
		s.frameOpensStream(pnum)
	}
	for {
		// STREAM
		off, size := dataToSend(min(s.out.start, s.outwin), min(s.out.end, s.outwin), s.outunsent, s.outacked, pto)
		size = min(size, s.conn.streams.outflow.avail())
		fin := s.outclosed.isSet() && off+size == s.out.end
		shouldSend := size > 0 || // have data to send
			s.outopened.shouldSendPTO(pto) || // should open the stream
			(fin && s.outclosed.shouldSendPTO(pto)) // should close the stream
		if !shouldSend {
			return true
		}
		b, added := w.appendStreamFrame(s.id, off, int(size), fin)
		if !added {
			return false
		}
		s.out.copy(off, b)
		s.conn.streams.outflow.consume(int64(len(b)))
		s.outunsent.sub(off, off+int64(len(b)))
		s.frameOpensStream(pnum)
		if fin {
			s.outclosed.setSent(pnum)
		}
		if pto {
			return true
		}
		if int64(len(b)) < size {
			return false
		}
	}
}

// frameOpensStream records that we're sending a frame that will open the stream.
//
// If we don't have an acknowledgement from the peer for a previous frame opening the stream,
// record this packet as being the latest one to open it.
func (s *Stream) frameOpensStream(pnum packetNumber) {
	if !s.outopened.isReceived() {
		s.outopened.setSent(pnum)
	}
}

// dataToSend returns the next range of data to send in a STREAM or CRYPTO_STREAM.
func dataToSend(start, end int64, outunsent, outacked rangeset[int64], pto bool) (sendStart, size int64) {
	switch {
	case pto:
		// On PTO, resend unacked data that fits in the probe packet.
		// For simplicity, we send the range starting at s.out.start
		// (which is definitely unacked, or else we would have discarded it)
		// up to the next acked byte (if any).
		//
		// This may miss unacked data starting after that acked byte,
		// but avoids resending data the peer has acked.
		for _, r := range outacked {
			if r.start > start {
				return start, r.start - start
			}
		}
		return start, end - start
	case outunsent.numRanges() > 0:
		return outunsent.min(), outunsent[0].size()
	default:
		return end, 0
	}
}
