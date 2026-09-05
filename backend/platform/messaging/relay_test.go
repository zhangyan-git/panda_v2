package messaging

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type relayOutboxFake struct {
	events       []LeasedEnvelope
	claimErr     error
	markFailures []relayFailure
	successes    []string
	failureErr   error
	successErr   error
	batchSize    int
	owner        string
	lease        time.Duration
}

type relayFailure struct {
	eventID string
	token   string
	err     error
	next    time.Time
}

func (o *relayOutboxFake) ClaimPending(_ context.Context, batchSize int, owner string, lease time.Duration) ([]LeasedEnvelope, error) {
	o.batchSize, o.owner, o.lease = batchSize, owner, lease
	if o.claimErr != nil {
		return nil, o.claimErr
	}
	return o.events, nil
}

func (o *relayOutboxFake) MarkSuccess(_ context.Context, eventID, token string) error {
	if o.successErr != nil {
		return o.successErr
	}
	o.successes = append(o.successes, eventID+":"+token)
	return nil
}

func (o *relayOutboxFake) MarkFailure(_ context.Context, eventID, token string, err error, next time.Time) error {
	o.markFailures = append(o.markFailures, relayFailure{eventID: eventID, token: token, err: err, next: next})
	return o.failureErr
}

type relayPublisherFake struct {
	errs map[string]error
	seen []string
}

func (p *relayPublisherFake) Publish(_ context.Context, event Envelope) error {
	p.seen = append(p.seen, event.EventID)
	return p.errs[event.EventID]
}

func TestNewRelayValidatesDependenciesAndAppliesDefaults(t *testing.T) {
	publisher := &relayPublisherFake{}
	if _, err := NewRelay(nil, publisher, RelayConfig{}); err == nil {
		t.Fatal("expected nil outbox error")
	}
	outbox := &relayOutboxFake{}
	if _, err := NewRelay(outbox, nil, RelayConfig{}); err == nil {
		t.Fatal("expected nil publisher error")
	}
	relay, err := NewRelay(outbox, publisher, RelayConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if relay.config.Owner == "" || relay.config.BatchSize != 100 || relay.config.Lease != time.Minute || relay.config.PollInterval != time.Second || relay.config.RetryDelay != time.Second {
		t.Fatalf("config defaults = %#v", relay.config)
	}
}

func TestRelayRunOncePublishesBatchAndRecordsResults(t *testing.T) {
	publishErr := errors.New("broker unavailable")
	outbox := &relayOutboxFake{events: []LeasedEnvelope{
		{Envelope: Envelope{EventID: "ok"}, LeaseToken: "token-ok"},
		{Envelope: Envelope{EventID: "failed"}, LeaseToken: "token-failed"},
	}}
	publisher := &relayPublisherFake{errs: map[string]error{"failed": publishErr}}
	relay, err := NewRelay(outbox, publisher, RelayConfig{Owner: "worker", BatchSize: 2, RetryDelay: time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	err = relay.RunOnce(context.Background())
	if !errors.Is(err, publishErr) {
		t.Fatalf("RunOnce error = %v, want publish error", err)
	}
	if strings.Join(publisher.seen, ",") != "ok,failed" {
		t.Fatalf("published events = %v", publisher.seen)
	}
	if len(outbox.successes) != 1 || outbox.successes[0] != "ok:token-ok" {
		t.Fatalf("successes = %#v", outbox.successes)
	}
	if len(outbox.markFailures) != 1 {
		t.Fatalf("failures = %#v", outbox.markFailures)
	}
	failure := outbox.markFailures[0]
	if failure.eventID != "failed" || failure.token != "token-failed" || !errors.Is(failure.err, publishErr) {
		t.Fatalf("failure record = %#v", failure)
	}
	if failure.next.Before(started.Add(time.Hour)) {
		t.Fatalf("retry time = %v, want at least %v", failure.next, started.Add(time.Hour))
	}
	if outbox.batchSize != 2 || outbox.owner != "worker" || outbox.lease != time.Minute {
		t.Fatalf("claim arguments = size %d owner %q lease %v", outbox.batchSize, outbox.owner, outbox.lease)
	}
}

func TestRelayRunOnceContinuesAfterBookkeepingError(t *testing.T) {
	markErr := errors.New("database unavailable")
	outbox := &relayOutboxFake{successErr: markErr, events: []LeasedEnvelope{
		{Envelope: Envelope{EventID: "one"}, LeaseToken: "token-one"},
		{Envelope: Envelope{EventID: "two"}, LeaseToken: "token-two"},
	}}
	publisher := &relayPublisherFake{errs: map[string]error{}}
	relay, err := NewRelay(outbox, publisher, RelayConfig{BatchSize: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.RunOnce(context.Background()); !errors.Is(err, markErr) {
		t.Fatalf("RunOnce error = %v, want bookkeeping error", err)
	}
	if len(publisher.seen) != 2 {
		t.Fatalf("published events = %v, want both events", publisher.seen)
	}
}

func TestRelayRunOnceReturnsClaimError(t *testing.T) {
	claimErr := errors.New("database unavailable")
	outbox := &relayOutboxFake{claimErr: claimErr}
	relay, err := NewRelay(outbox, &relayPublisherFake{}, RelayConfig{})
	if err != nil {
		t.Fatal(err)
	}
	if err := relay.RunOnce(context.Background()); !errors.Is(err, claimErr) {
		t.Fatalf("RunOnce error = %v, want claim error", err)
	}
}
