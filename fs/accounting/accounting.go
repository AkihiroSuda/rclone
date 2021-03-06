// Package accounting providers an accounting and limiting reader
package accounting

import (
	"fmt"
	"io"
	"sync"
	"time"

	"github.com/VividCortex/ewma"
	"github.com/ncw/rclone/fs"
	"github.com/ncw/rclone/fs/asyncreader"
)

// Account limits and accounts for one transfer
type Account struct {
	// The mutex is to make sure Read() and Close() aren't called
	// concurrently.  Unfortunately the persistent connection loop
	// in http transport calls Read() after Do() returns on
	// CancelRequest so this race can happen when it apparently
	// shouldn't.
	mu      sync.Mutex
	in      io.Reader
	origIn  io.ReadCloser
	close   io.Closer
	size    int64
	name    string
	statmu  sync.Mutex         // Separate mutex for stat values.
	bytes   int64              // Total number of bytes read
	start   time.Time          // Start time of first read
	lpTime  time.Time          // Time of last average measurement
	lpBytes int                // Number of bytes read since last measurement
	avg     ewma.MovingAverage // Moving average of last few measurements
	closed  bool               // set if the file is closed
	exit    chan struct{}      // channel that will be closed when transfer is finished
	withBuf bool               // is using a buffered in
}

// NewAccountSizeName makes a Account reader for an io.ReadCloser of
// the given size and name
func NewAccountSizeName(in io.ReadCloser, size int64, name string) *Account {
	acc := &Account{
		in:     in,
		close:  in,
		origIn: in,
		size:   size,
		name:   name,
		exit:   make(chan struct{}),
		avg:    ewma.NewMovingAverage(),
		lpTime: time.Now(),
	}
	go acc.averageLoop()
	Stats.inProgress.set(acc.name, acc)
	return acc
}

// NewAccount makes a Account reader for an object
func NewAccount(in io.ReadCloser, obj fs.Object) *Account {
	return NewAccountSizeName(in, obj.Size(), obj.Remote())
}

// WithBuffer - If the file is above a certain size it adds an Async reader
func (acc *Account) WithBuffer() *Account {
	acc.withBuf = true
	var buffers int
	if acc.size >= int64(fs.Config.BufferSize) || acc.size == -1 {
		buffers = int(int64(fs.Config.BufferSize) / asyncreader.BufferSize)
	} else {
		buffers = int(acc.size / asyncreader.BufferSize)
	}
	// On big files add a buffer
	if buffers > 0 {
		rc, err := asyncreader.New(acc.origIn, buffers)
		if err != nil {
			fs.Errorf(acc.name, "Failed to make buffer: %v", err)
		} else {
			acc.in = rc
			acc.close = rc
		}
	}
	return acc
}

// GetReader returns the underlying io.ReadCloser under any Buffer
func (acc *Account) GetReader() io.ReadCloser {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	return acc.origIn
}

// StopBuffering stops the async buffer doing any more buffering
func (acc *Account) StopBuffering() {
	if asyncIn, ok := acc.in.(*asyncreader.AsyncReader); ok {
		asyncIn.Abandon()
	}
}

// UpdateReader updates the underlying io.ReadCloser stopping the
// asynb buffer (if any) and re-adding it
func (acc *Account) UpdateReader(in io.ReadCloser) {
	acc.mu.Lock()
	acc.StopBuffering()
	acc.in = in
	acc.close = in
	acc.origIn = in
	acc.WithBuffer()
	acc.mu.Unlock()
}

// averageLoop calculates averages for the stats in the background
func (acc *Account) averageLoop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case now := <-tick.C:
			acc.statmu.Lock()
			// Add average of last second.
			elapsed := now.Sub(acc.lpTime).Seconds()
			avg := float64(acc.lpBytes) / elapsed
			acc.avg.Add(avg)
			acc.lpBytes = 0
			acc.lpTime = now
			// Unlock stats
			acc.statmu.Unlock()
		case <-acc.exit:
			return
		}
	}
}

// read bytes from the io.Reader passed in and account them
func (acc *Account) read(in io.Reader, p []byte) (n int, err error) {
	// Set start time.
	acc.statmu.Lock()
	if acc.start.IsZero() {
		acc.start = time.Now()
	}
	acc.statmu.Unlock()

	n, err = in.Read(p)

	// Update Stats
	acc.statmu.Lock()
	acc.lpBytes += n
	acc.bytes += int64(n)
	acc.statmu.Unlock()

	Stats.Bytes(int64(n))

	limitBandwidth(n)
	return
}

// Read bytes from the object - see io.Reader
func (acc *Account) Read(p []byte) (n int, err error) {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	return acc.read(acc.in, p)
}

// Close the object
func (acc *Account) Close() error {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	if acc.closed {
		return nil
	}
	acc.closed = true
	close(acc.exit)
	Stats.inProgress.clear(acc.name)
	return acc.close.Close()
}

// progress returns bytes read as well as the size.
// Size can be <= 0 if the size is unknown.
func (acc *Account) progress() (bytes, size int64) {
	if acc == nil {
		return 0, 0
	}
	acc.statmu.Lock()
	bytes, size = acc.bytes, acc.size
	acc.statmu.Unlock()
	return bytes, size
}

// speed returns the speed of the current file transfer
// in bytes per second, as well a an exponentially weighted moving average
// If no read has completed yet, 0 is returned for both values.
func (acc *Account) speed() (bps, current float64) {
	if acc == nil {
		return 0, 0
	}
	acc.statmu.Lock()
	defer acc.statmu.Unlock()
	if acc.bytes == 0 {
		return 0, 0
	}
	// Calculate speed from first read.
	total := float64(time.Now().Sub(acc.start)) / float64(time.Second)
	bps = float64(acc.bytes) / total
	current = acc.avg.Value()
	return
}

// eta returns the ETA of the current operation,
// rounded to full seconds.
// If the ETA cannot be determined 'ok' returns false.
func (acc *Account) eta() (eta time.Duration, ok bool) {
	if acc == nil || acc.size <= 0 {
		return 0, false
	}
	acc.statmu.Lock()
	defer acc.statmu.Unlock()
	if acc.bytes == 0 {
		return 0, false
	}
	left := acc.size - acc.bytes
	if left <= 0 {
		return 0, true
	}
	avg := acc.avg.Value()
	if avg <= 0 {
		return 0, false
	}
	seconds := float64(left) / acc.avg.Value()

	return time.Duration(time.Second * time.Duration(int(seconds))), true
}

// String produces stats for this file
func (acc *Account) String() string {
	a, b := acc.progress()
	_, cur := acc.speed()
	eta, etaok := acc.eta()
	etas := "-"
	if etaok {
		if eta > 0 {
			etas = fmt.Sprintf("%v", eta)
		} else {
			etas = "0s"
		}
	}
	name := []rune(acc.name)
	if fs.Config.StatsFileNameLength > 0 {
		if len(name) > fs.Config.StatsFileNameLength {
			where := len(name) - fs.Config.StatsFileNameLength
			name = append([]rune{'.', '.', '.'}, name[where:]...)
		}
	}

	if fs.Config.DataRateUnit == "bits" {
		cur = cur * 8
	}

	percentageDone := 0
	if b > 0 {
		percentageDone = int(100 * float64(a) / float64(b))
	}

	done := fmt.Sprintf("%2d%% /%s", percentageDone, fs.SizeSuffix(b))

	return fmt.Sprintf("%45s: %s, %s/s, %s",
		string(name),
		done,
		fs.SizeSuffix(cur),
		etas,
	)
}

// OldStream returns the top io.Reader
func (acc *Account) OldStream() io.Reader {
	acc.mu.Lock()
	defer acc.mu.Unlock()
	return acc.in
}

// SetStream updates the top io.Reader
func (acc *Account) SetStream(in io.Reader) {
	acc.mu.Lock()
	acc.in = in
	acc.mu.Unlock()
}

// WrapStream wraps an io Reader so it will be accounted in the same
// way as account
func (acc *Account) WrapStream(in io.Reader) io.Reader {
	return &accountStream{
		acc: acc,
		in:  in,
	}
}

// accountStream accounts a single io.Reader into a parent *Account
type accountStream struct {
	acc *Account
	in  io.Reader
}

// OldStream return the underlying stream
func (a *accountStream) OldStream() io.Reader {
	return a.in
}

// SetStream set the underlying stream
func (a *accountStream) SetStream(in io.Reader) {
	a.in = in
}

// WrapStream wrap in in an accounter
func (a *accountStream) WrapStream(in io.Reader) io.Reader {
	return a.acc.WrapStream(in)
}

// Read bytes from the object - see io.Reader
func (a *accountStream) Read(p []byte) (n int, err error) {
	return a.acc.read(a.in, p)
}

// Accounter accounts a stream allowing the accounting to be removed and re-added
type Accounter interface {
	io.Reader
	OldStream() io.Reader
	SetStream(io.Reader)
	WrapStream(io.Reader) io.Reader
}

// WrapFn wraps an io.Reader (for accounting purposes usually)
type WrapFn func(io.Reader) io.Reader

// UnWrap unwraps a reader returning unwrapped and wrap, a function to
// wrap it back up again.  If `in` is an Accounter then this function
// will take the accounting unwrapped and wrap will put it back on
// again the new Reader passed in.
//
// This allows functions which wrap io.Readers to move the accounting
// to the end of the wrapped chain of readers.  This is very important
// if buffering is being introduced and if the Reader might be wrapped
// again.
func UnWrap(in io.Reader) (unwrapped io.Reader, wrap WrapFn) {
	acc, ok := in.(Accounter)
	if !ok {
		return in, func(r io.Reader) io.Reader { return r }
	}
	return acc.OldStream(), acc.WrapStream
}
