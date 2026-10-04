package humansize

import "testing"

func TestBytes(t *testing.T) {
	t.Parallel()
	for _, c := range []struct {
		in   int64
		want string
	}{
		{0, "0 B"},
		{411, "411 B"},
		{1023, "1023 B"},
		{1024, "1.0 KB"},
		{14336, "14.0 KB"},
		{1258291, "1.2 MB"},
		{3328599654, "3.1 GB"},
	} {
		if got := Bytes(c.in); got != c.want {
			t.Errorf("Bytes(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}

// A negative count cannot arise from a file, but a formatter that renders one
// as a unit-less number would be a worse answer than the plain byte form.
func TestBytesRendersANegativeCountAsBytes(t *testing.T) {
	t.Parallel()
	if got := Bytes(-1); got != "-1 B" {
		t.Errorf("Bytes(-1) = %q, want %q", got, "-1 B")
	}
}
