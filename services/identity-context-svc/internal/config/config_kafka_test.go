package config_test

import (
	"slices"
	"testing"

	"zoiko.io/identity-context-svc/internal/config"
)

// The reader must cover every topic a handled event is published on. Until
// 2026-09-28 it read this service's own publish topic alone, so no revocation
// from delegated-authority-svc, access-control-svc or tenant-entity-registry-svc
// ever arrived, and every event-driven revocation handler was unreachable.
func TestConsumeTopicsDefaultToTheProducerTopics(t *testing.T) {
	baseEnv(t)
	t.Setenv("KAFKA_CONSUME_TOPICS", "")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, want := range []string{
		"zoiko.delegated-authority.events", // authority.revoked / authority.expired
		"zoiko.access-control.events",      // role.updated
		"zoiko.entity.events",              // entity.updated
	} {
		if !slices.Contains(cfg.Kafka.ConsumeTopics, want) {
			t.Errorf("consumer does not read %s: %v", want, cfg.Kafka.ConsumeTopics)
		}
	}
	if cfg.Kafka.Topic != "zoiko.identity.events" {
		t.Errorf("publish topic changed: %s", cfg.Kafka.Topic)
	}
}

func TestConsumeTopicsCanBeOverridden(t *testing.T) {
	baseEnv(t)
	t.Setenv("KAFKA_CONSUME_TOPICS", " a.events , b.events ,")

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if !slices.Equal(cfg.Kafka.ConsumeTopics, []string{"a.events", "b.events"}) {
		t.Errorf("got %v", cfg.Kafka.ConsumeTopics)
	}
}
