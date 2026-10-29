package service

import (
	"testing"

	"billing-service/internal/config"
)

func TestPushModeStartsHealthyWithoutExporterCollection(t *testing.T) {
	push := New(config.Config{IngestMode: "push"}, nil, nil)
	if ok, _ := push.Health(); !ok {
		t.Fatal("push mode must not depend on the disabled exporter collection loop")
	}
	pull := New(config.Config{IngestMode: "pull", PullEnabled: true}, nil, nil)
	if ok, _ := pull.Health(); ok {
		t.Fatal("pull mode must await a successful collection")
	}
	malformed := New(config.Config{IngestMode: "push", PullEnabled: true}, nil, nil)
	if ok, _ := malformed.Health(); ok {
		t.Fatal("inconsistent push configuration must not report an initial success")
	}
}
