package main

import (
	"bytes"
	"errors"
	"io"
)

var errSSEComplete = errors.New("SSE terminal event received")

// sseFramer buffers one bounded event. Consumers own JSON interpretation and
// terminal semantics; streaming proxies can forward the original bytes intact.
type sseFramer struct {
	limit   int
	line    []byte
	data    []byte
	hasData bool
	consume func([]byte) error
}

func (f *sseFramer) feed(chunk []byte) error {
	for len(chunk) > 0 {
		n := bytes.IndexByte(chunk, '\n')
		if n < 0 {
			n = len(chunk)
		}
		if len(f.line)+len(f.data)+n > f.limit {
			return errors.New("upstream SSE event exceeds size limit")
		}
		f.line = append(f.line, chunk[:n]...)
		if n == len(chunk) {
			return nil
		}
		chunk = chunk[n+1:]
		if err := f.flushLine(); err != nil {
			return err
		}
	}
	return nil
}

func (f *sseFramer) flushLine() error {
	line := bytes.TrimSuffix(f.line, []byte{'\r'})
	defer func() { f.line = f.line[:0] }()
	if len(line) == 0 {
		return f.flushEvent()
	}
	if bytes.HasPrefix(line, []byte("data:")) {
		value := bytes.TrimPrefix(line[5:], []byte{' '})
		extra := len(value)
		if f.hasData {
			extra++
		}
		if len(f.data)+extra > f.limit {
			return errors.New("upstream SSE event exceeds size limit")
		}
		if f.hasData {
			f.data = append(f.data, '\n')
		}
		f.data = append(f.data, value...)
		f.hasData = true
	}
	return nil
}

func (f *sseFramer) flushEvent() error {
	if !f.hasData {
		return nil
	}
	err := f.consume(f.data)
	f.data = f.data[:0]
	f.hasData = false
	return err
}

func (f *sseFramer) finish() error {
	if len(f.line) > 0 {
		if err := f.flushLine(); err != nil {
			return err
		}
	}
	return f.flushEvent()
}

func readSSE(r io.Reader, limit int, consume func([]byte) error) error {
	f := sseFramer{limit: limit, consume: consume}
	buf := make([]byte, 32*1024)
	for {
		n, readErr := r.Read(buf)
		if err := f.feed(buf[:n]); err != nil {
			if errors.Is(err, errSSEComplete) {
				return nil
			}
			return err
		}
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return errors.New("upstream stream read failed")
			}
			err := f.finish()
			if errors.Is(err, errSSEComplete) {
				return nil
			}
			return err
		}
	}
}
