package ratelimit

import (
	"strconv"
	"testing"
	"time"
)

type clock struct{ now time.Time }

func (c *clock) Now() time.Time { return c.now }

func TestAllow_ANameTakesItsBurstThenRefillsAtTheRate(t *testing.T) {
	c := &clock{now: time.Unix(1e9, 0)}
	l := New(c, 2, 3)
	for i := range 3 {
		if !l.Allow("a") {
			t.Fatalf("token %d refused", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("a fourth token within the burst was allowed")
	}
	if !l.Allow("b") {
		t.Fatal("another name shares the first's bucket")
	}
	c.now = c.now.Add(500 * time.Millisecond)
	if !l.Allow("a") || l.Allow("a") {
		t.Fatal("half a second at 2 per second is one token")
	}
	c.now = c.now.Add(time.Hour)
	for i := range 3 {
		if !l.Allow("a") {
			t.Fatalf("refilled token %d refused", i)
		}
	}
	if l.Allow("a") {
		t.Fatal("a bucket refilled past its burst")
	}
}

func TestAllow_AFullMapRefusesANewNameUntilABucketHasRefilled(t *testing.T) {
	c := &clock{now: time.Unix(1e9, 0)}
	l := New(c, 1, 2)
	for i := range maxBuckets {
		if !l.Allow(strconv.Itoa(i)) {
			t.Fatalf("name %d refused below the cap", i)
		}
	}
	if l.Allow("new") {
		t.Fatal("a new name was allowed with every bucket held and none refilled")
	}
	if len(l.buckets) != maxBuckets {
		t.Fatalf("the map holds %d buckets, over %d", len(l.buckets), maxBuckets)
	}
	if !l.Allow("7") {
		t.Fatal("a held name was refused at the cap")
	}
	c.now = c.now.Add(time.Second)
	if !l.Allow("new") {
		t.Fatal("a new name was refused after the buckets refilled")
	}
	if len(l.buckets) > maxBuckets {
		t.Fatalf("the map holds %d buckets, over %d", len(l.buckets), maxBuckets)
	}
}

func TestSource_IsTheIPv4AddressOrTheIPv6Slash64(t *testing.T) {
	for in, want := range map[string]string{
		"192.0.2.7:4431":                 "192.0.2.7",
		"[::ffff:192.0.2.7]:4431":        "192.0.2.7",
		"[2001:db8:1:2:aaaa::1]:4431":    "2001:db8:1:2::/64",
		"[2001:db8:1:2:ffff::9%eth0]:80": "2001:db8:1:2::/64",
		"[2001:db8:1:3::1]:4431":         "2001:db8:1:3::/64",
		"192.0.2.8":                      "192.0.2.8",
		"not-an-address":                 "not-an-address",
	} {
		if got := Source(in); got != want {
			t.Errorf("Source(%q) = %q, want %q", in, got, want)
		}
	}
}
