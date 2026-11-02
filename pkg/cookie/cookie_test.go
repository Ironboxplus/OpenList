package cookie

import "testing"

func TestDelStr(t *testing.T) {
	cases := []struct {
		name string
		in   string
		del  string
		want string
	}{
		{"delete middle", "a=1; __puus=old; b=2", "__puus", "a=1;b=2"},
		{"delete first", "__puus=old; a=1", "__puus", "a=1"},
		{"delete last", "a=1; __puus=old", "__puus", "a=1"},
		{"missing name is no-op", "a=1; b=2", "__puus", "a=1;b=2"},
		{"empty input", "", "__puus", ""},
		{"only the target", "__puus=old", "__puus", ""},
		{"only removes first occurrence", "a=1; __puus=x; b=2; __puus=y", "__puus", "a=1;b=2;__puus=y"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := DelStr(c.in, c.del); got != c.want {
				t.Fatalf("DelStr(%q, %q) = %q, want %q", c.in, c.del, got, c.want)
			}
		})
	}
}

func TestSetStrAndDelStrRoundTrip(t *testing.T) {
	// SetStr 更新后 DelStr 删除，应回到初始状态（不含被更新的字段）
	got := DelStr(SetStr("a=1; __puus=old; b=2", "__puus", "new"), "__puus")
	want := "a=1;b=2"
	if got != want {
		t.Fatalf("round trip = %q, want %q", got, want)
	}
}
