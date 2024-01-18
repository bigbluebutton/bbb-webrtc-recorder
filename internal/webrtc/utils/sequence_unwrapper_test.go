package utils

import (
	"testing"
)

func TestSequenceUnwrapper(t *testing.T) {
	unwrapper := NewSequenceUnwrapper(16)

	// Test case 1: Initialisation
	result := unwrapper.Unwrap(uint64(65534))
	expected := int64(65534)
	if result != expected {
		t.Errorf("[T1] Unexpected result. Expected: %d, Got: %d", expected, result)
	}

	// Test case 2: in-order seqnum increment
	result = unwrapper.Unwrap(uint64(65535))
	expected = int64(65535)
	if result != expected {
		t.Errorf("[T2] Unexpected result. Expected: %d, Got: %d", expected, result)
	}

	// Test case 3: first wraparound (in-order seqnum increment)
	result = unwrapper.Unwrap(uint64(0))
	expected = int64(65536)
	if result != expected {
		t.Errorf("[T3] Unexpected result. Expected: %d, Got: %d", expected, result)
	}

	// Test case 4: Increment after wraparound (with gap)
	result = unwrapper.Unwrap(uint64(2))
	expected = int64(65538)
	if result != expected {
		t.Errorf("[T4] Unexpected result. Expected: %d, Got: %d", expected, result)
	}

	// Test case 5: second wraparound (simulated in-order seqnum increments)
	for i := 3; i < (1 << 16); i++ {
		unwrapper.Unwrap(uint64(i))
	}
	result = unwrapper.Unwrap(uint64(0))
	expected = int64(131072)
	if result != expected {
		t.Errorf("[T5] Unexpected result. Expected: %d, Got: %d", expected, result)
	}

	// Test case 6: Duplicate seqnum
	result = unwrapper.Unwrap(uint64(0))
	expected = int64(131072)
	if result != expected {
		t.Errorf("[T6] Unexpected result. Expected: %d, Got: %d", expected, result)
	}

	// Test case for negative values
	// Assuming highestSequenceNumber is 2, wrapArounds is 0, inMax is 65536
	// and lastSeqNo is 65535
	// Test case 7: Negative value
	unwrapper.highestSequenceNumber = 2
	unwrapper.wrapArounds = 0
	unwrapper.inMax = 65536
	unwrapper.lastSeqNo = 65530
	result = unwrapper.Unwrap(uint64(65534))
	expected = int64(65534)
	if result != expected {
		t.Errorf("[T7] Unexpected result for negative values. Expected: %d, Got: %d", expected, result)
	}
}
