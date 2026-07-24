package emit

import "testing"

func TestMaxIndex(t *testing.T) {
	r := New(2, 960)
	if _, ok := r.MaxIndex(); ok {
		t.Fatal("empty reassembler should report ok=false")
	}
	pcm := make([]int16, 960*2)
	r.Add(5, 0, pcm, false, 0)
	r.Add(9, 0, pcm, false, 0)
	r.Add(7, 0, pcm, false, 0)
	if max, ok := r.MaxIndex(); !ok || max != 9 {
		t.Fatalf("MaxIndex = %d, %v; want 9, true", max, ok)
	}
	// Drop discards up to and including the index; 9 remains.
	r.Drop(8)
	if max, ok := r.MaxIndex(); !ok || max != 9 {
		t.Fatalf("after Drop(8): MaxIndex = %d, %v; want 9, true", max, ok)
	}
	r.Drop(9)
	if _, ok := r.MaxIndex(); ok {
		t.Fatal("after Drop(9): want ok=false")
	}
}
