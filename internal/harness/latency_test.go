package harness

import (
	"testing"
	"time"
)

func TestLatencyParseLatencyZero(t *testing.T) {
	d, unreachable, err := latencyParseLatency("0")
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

func TestLatencyParseLatencyMilliseconds(t *testing.T) {
	d, unreachable, err := latencyParseLatency("500ms")
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

func TestLatencyParseLatencySeconds(t *testing.T) {
	d, unreachable, err := latencyParseLatency("5s")
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

func TestLatencyParseLatencyUnreachable(t *testing.T) {
	d, unreachable, err := latencyParseLatency("unreachable")
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

func TestLatencyParseLatencyInvalid(t *testing.T) {
	if _, _, err := latencyParseLatency("bogus"); err == nil {
		t.Fatalf("expected error for invalid latency, got nil")
	}
}

func TestLatencyCondition(t *testing.T) {
	got := latencyCondition("500ms", "web1", "events=on")
	want := "latency=500ms;service=web1;events=on"
	if got != want {
		t.Fatalf("latencyCondition = %q, want %q", got, want)
	}
}

func TestLatencyNetemCommand(t *testing.T) {
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
		if got := latencyNetemCommand(c.iface, c.delay); got != c.want {
			t.Errorf("latencyNetemCommand(%q, %v) = %q, want %q", c.iface, c.delay, got, c.want)
		}
	}
}

func TestLatencyNetemDeleteCommand(t *testing.T) {
	got := latencyNetemDeleteCommand("eth0")
	want := "tc qdisc del dev eth0 root netem"
	if got != want {
		t.Fatalf("latencyNetemDeleteCommand = %q, want %q", got, want)
	}
}

func TestLatencyDropCommand(t *testing.T) {
	got := latencyDropCommand(5000)
	want := "iptables -I DOCKER-USER -p tcp --dport 5000 -j DROP"
	if got != want {
		t.Fatalf("latencyDropCommand = %q, want %q", got, want)
	}
}

func TestLatencyDropDeleteCommand(t *testing.T) {
	got := latencyDropDeleteCommand(5000)
	want := "iptables -D DOCKER-USER -p tcp --dport 5000 -j DROP"
	if got != want {
		t.Fatalf("latencyDropDeleteCommand = %q, want %q", got, want)
	}
}
