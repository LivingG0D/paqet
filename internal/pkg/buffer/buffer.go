package buffer

import (
	"io"
	"sync"
)

var (
	TPool sync.Pool
	UPool sync.Pool
)

func Initialize(tPool, uPool int) {
	TPool = sync.Pool{
		New: func() any {
			b := make([]byte, tPool)
			return &b
		},
	}
	UPool = sync.Pool{
		New: func() any {
			b := make([]byte, uPool)
			return &b
		},
	}
}

// CopyT copies TCP stream data with a pooled buffer.
func CopyT(dst io.Writer, src io.Reader) error { return copyPooled(dst, src, &TPool) }

// CopyU copies UDP stream data with a pooled buffer.
func CopyU(dst io.Writer, src io.Reader) error { return copyPooled(dst, src, &UPool) }

func copyPooled(dst io.Writer, src io.Reader, pool *sync.Pool) error {
	bufp := pool.Get().(*[]byte)
	defer pool.Put(bufp)

	_, err := io.CopyBuffer(dst, src, *bufp)
	return err
}
