package zeroconf

import (
	"errors"
	"net"
	"strings"
	"testing"
)

// The condition this guards against is silent: the backend receives nothing and reports
// no error, so the exporter finds zero devices forever with no clue why.
func TestDetectsWhenAnotherDaemonOwnsThePort(t *testing.T) {
	holder, err := net.ListenUDP("udp4", &net.UDPAddr{Port: mdnsPort})
	if err != nil {
		// Something already owns 5353 on this machine, which is itself the condition
		// under test.
		var inUse *portInUseError
		if !errors.As(checkPortOwnership(), &inUse) {
			t.Fatalf("port 5353 is taken (%v) but checkPortOwnership did not report it", err)
		}
		t.Log("port 5353 is already owned on this host; detection confirmed")
		return
	}
	defer func() { _ = holder.Close() }()

	var inUse *portInUseError
	if !errors.As(checkPortOwnership(), &inUse) {
		t.Fatal("an exclusively held port 5353 should be reported as in use")
	}
	// The message has to name a concrete next step, not just the failure.
	if advice := inUse.Advice(); !strings.Contains(advice, "discovery.sources") {
		t.Errorf("advice = %q, want it to name the configuration change to make", advice)
	}
}

func TestReportsNothingWhenThePortIsFree(t *testing.T) {
	probe, err := net.ListenUDP("udp4", &net.UDPAddr{Port: mdnsPort})
	if err != nil {
		t.Skip("port 5353 is in use on this host, so the free case cannot be tested")
	}
	_ = probe.Close()

	if err := checkPortOwnership(); err != nil {
		t.Errorf("checkPortOwnership = %v, want nil when the port is free", err)
	}
}
