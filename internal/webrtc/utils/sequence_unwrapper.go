package utils

import "sync"

type SequenceUnwrapper struct {
	m                     *sync.Mutex
	wrapArounds           int64
	highestSequenceNumber uint64
	inMax                 uint64
	lastSeqNo             uint64
	lastWrappedSeqNo      int64
	started               bool
}

func NewSequenceUnwrapper(base int) *SequenceUnwrapper {
	return &SequenceUnwrapper{
		m:     &sync.Mutex{},
		inMax: 1 << uint64(base),
	}
}

// Unwrap accepts a uint<base> values which form a sequence, and converts the values into
// int64 while adjusting the sequence every time a wraparound happens. For example:
// unwrapper := NewSequenceUnwrapper(16)
// ...
// unwrapper.Unwrap(65534) == 65534
// unwrapper.Unwrap(65535) == 65535
// unwrapper.Unwrap(0) == 65536
// unwrapper.Unwrap(1) == 65537
// ...
// unwrapper.Unwrap(65534) == 131070
// unwrapper.Unwrap(65535) == 131071
// unwrapper.Unwrap(0) == 131072
// unwrapper.Unwrap(1) == 131073
// ...
func (sw *SequenceUnwrapper) Unwrap(n uint64) int64 {
	sw.m.Lock()
	defer sw.m.Unlock()

	sw.lastSeqNo = n
	// If this is the first time we're called, just set the highest sequence number
	// and return the value
	if !sw.started {
		sw.started = true
		sw.highestSequenceNumber = n
		sw.lastWrappedSeqNo = int64(n)

		return sw.lastWrappedSeqNo
	}

	// If the sequence number is the same as the highest sequence number, just return
	// the last wrapped sequence number
	if n == sw.highestSequenceNumber {
		sw.lastWrappedSeqNo = sw.wrapArounds + int64(n)
		return sw.lastWrappedSeqNo
	}

	// If the sequence number is lower than the highest sequence number, it means that
	// the sequence number has wrapped around. If the difference between the highest
	// sequence number and the current sequence number is greater than half the maximum
	// value of the sequence number, it means that the sequence number has wrapped
	// around more than once. In this case, we need to adjust the wrapArounds
	// counter and the highest sequence number accordingly and return the new value.
	// Otherwise, we just return the last wrapped sequence number
	if n < sw.highestSequenceNumber {
		if sw.highestSequenceNumber-n > sw.inMax/2 {
			sw.wrapArounds += int64(sw.inMax)
			sw.highestSequenceNumber = n
			sw.lastWrappedSeqNo = sw.wrapArounds + int64(n)
			return sw.lastWrappedSeqNo
		}

		sw.lastWrappedSeqNo = sw.wrapArounds + int64(n)
		return sw.lastWrappedSeqNo
	}

	// If the difference between the current sequence number and the highest
	// sequence number is greater than half the maximum value of the sequence
	// number, it means that the sequence number has wrapped around.
	// In this case, we need to adjust the wrapArounds counter and the
	// highest sequence number accordingly and return the new value. Otherwise, we just
	// return the last wrapped sequence number
	delta := n - sw.highestSequenceNumber
	if delta > (sw.inMax >> 1) {
		// If the unwrapped seqnum ends up being negative, it means that the sequence
		// number if from a previous wraparound. In this case, we need to adjust the
		// wrapArounds counter accordingly
		if sw.wrapArounds-int64(sw.inMax)+int64(n) <= 0 {
			sw.wrapArounds += int64(sw.inMax)
		}

		sw.lastWrappedSeqNo = sw.wrapArounds - int64(sw.inMax) + int64(n)
		return sw.lastWrappedSeqNo
	}

	sw.highestSequenceNumber = n
	sw.lastWrappedSeqNo = sw.wrapArounds + int64(n)
	return sw.lastWrappedSeqNo
}
