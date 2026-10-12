package ledgerfile

import (
	"testing"
	"time"

	"github.com/reliant-labs/forge/pkg/release"
)

func TestHostedSnapshot_RoundTripAndReplace(t *testing.T) {
	s := openTest(t)
	if got, err := s.HostedSnapshot("prod"); err != nil || got != nil {
		t.Fatalf("no snapshot yet: %v %v", got, err)
	}
	p := promotion("prod", "v1")
	p.ID = "p-1"
	snap := HostedSnapshot{Env: "prod", Endpoint: "https://cp", RefreshedAt: time.Now().UTC().Truncate(time.Second), Bound: true, Current: &p}
	if err := s.PutHostedSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	snap.Bound, snap.Current = false, nil
	if err := s.PutHostedSnapshot(snap); err != nil {
		t.Fatal(err)
	}
	got, err := s.HostedSnapshot("prod")
	if err != nil || got == nil || got.Bound || got.Current != nil {
		t.Fatalf("the newest snapshot replaces the old: %+v %v", got, err)
	}
	if err := s.PutHostedSnapshot(HostedSnapshot{Env: "prod", Endpoint: "x", Bound: true}); err == nil {
		t.Fatal("a bound snapshot without its promotion is invalid")
	}
}

func TestQueuedPromotions_IdempotentOnKeyAndGatesAttach(t *testing.T) {
	s := openTest(t)
	q := QueuedPromotion{Key: "k1", Env: "prod", Endpoint: "https://cp", Release: "v1", QueuedAt: time.Now().UTC()}
	for i := 0; i < 2; i++ {
		if err := s.QueuePromotion(q); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.QueuedPromotions("prod"); len(got) != 1 {
		t.Fatalf("queueing one key twice must queue it once, got %d", len(got))
	}
	if err := s.AttachQueuedGate("prod", "k1", release.Gate{Name: "apply", Status: release.GateStatusPassed}); err != nil {
		t.Fatal(err)
	}
	if err := s.AttachQueuedGate("prod", "nope", release.Gate{Name: "apply", Status: release.GateStatusPassed}); err == nil {
		t.Fatal("evidence for a record that is not queued must be an error")
	}
	got, _ := s.QueuedPromotions("prod")
	if len(got[0].Gates) != 1 {
		t.Fatalf("gate not attached: %+v", got[0])
	}
	for i := 0; i < 2; i++ {
		if err := s.DropQueuedPromotion("prod", "k1"); err != nil {
			t.Fatal(err)
		}
	}
	if got, _ := s.QueuedPromotions("prod"); len(got) != 0 {
		t.Fatalf("drop must be idempotent and empty the queue, got %d", len(got))
	}
	if err := s.QueuePromotion(QueuedPromotion{Env: "prod", Release: "v1"}); err == nil {
		t.Fatal("a record with no key is invalid")
	}
}
