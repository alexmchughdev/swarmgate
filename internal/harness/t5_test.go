package harness

import (
	"testing"
	"time"
)

func TestT5ParseLatencyZero(t *testing.T) {
	d, unreachable, err := t5ParseLatency("0")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if unreachable {
		t.Fatalf("unreachable = true, want false")
	}
	if d != 0 {
		t.Fatalf("d = %v, want 0", d)
	}
}

func TestT5ParseLatencyMilliseconds(t *testing.T) {
	d, unreachable, err := t5ParseLatency("500ms")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if unreachable {
		t.Fatalf("unreachable = true, want false")
	}
	if d != 500*time.Millisecond {
		t.Fatalf("d = %v, want 500ms", d)
	}
}

func TestT5ParseLatencySeconds(t *testing.T) {
	d, unreachable, err := t5ParseLatency("5s")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if unreachable {
		t.Fatalf("unreachable = true, want false")
	}
	if d != 5*time.Second {
		t.Fatalf("d = %v, want 5s", d)
	}
}

func TestT5ParseLatencyUnreachable(t *testing.T) {
	d, unreachable, err := t5ParseLatency("unreachable")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !unreachable {
		t.Fatalf("unreachable = false, want true")
	}
	if d != 0 {
		t.Fatalf("d = %v, want 0", d)
	}
}

func TestT5ParseLatencyInvalid(t *testing.T) {
	if _, _, err := t5ParseLatency("bogus"); err == nil {
		t.Fatalf("expected error for invalid latency, got nil")
	}
}

func TestT5Condition(t *testing.T) {
	got := t5Condition("500ms", "web1", "events=on")
	want := "latency=500ms;service=web1;events=on"
	if got != want {
		t.Fatalf("t5Condition = %q, want %q", got, want)
	}
}

func TestT5NetemCommand(t *testing.T) {
	cases := []struct {
		iface string
		delay time.Duration
		want  string
	}{
		{"eth0", 500 * time.Millisecond, "tc qdisc replace dev eth0 root netem delay 500ms"},
		{"eth0", 5 * time.Second, "tc qdisc replace dev eth0 root netem delay 5s"},
		{"eth1", 1500 * time.Millisecond, "tc qdisc replace dev eth1 root netem delay 1.5s"},
	}
	for _, c := range cases {
		if got := t5NetemCommand(c.iface, c.delay); got != c.want {
			t.Errorf("t5NetemCommand(%q, %v) = %q, want %q", c.iface, c.delay, got, c.want)
		}
	}
}

func TestT5NetemDeleteCommand(t *testing.T) {
	got := t5NetemDeleteCommand("eth0")
	want := "tc qdisc del dev eth0 root netem"
	if got != want {
		t.Fatalf("t5NetemDeleteCommand = %q, want %q", got, want)
	}
}

func TestT5DropCommand(t *testing.T) {
	got := t5DropCommand(5000)
	want := "iptables -I DOCKER-USER -p tcp --dport 5000 -j DROP"
	if got != want {
		t.Fatalf("t5DropCommand = %q, want %q", got, want)
	}
}

func TestT5DropDeleteCommand(t *testing.T) {
	got := t5DropDeleteCommand(5000)
	want := "iptables -D DOCKER-USER -p tcp --dport 5000 -j DROP"
	if got != want {
		t.Fatalf("t5DropDeleteCommand = %q, want %q", got, want)
	}
}
