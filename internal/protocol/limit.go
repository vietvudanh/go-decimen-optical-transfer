package protocol

import "io"

func newLimitReader(r io.Reader, n int) io.Reader { return io.LimitReader(r, int64(n)) }
