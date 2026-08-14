package controlplane

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestServicePublishesIdentityAndStopsGracefully(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	started := time.Date(2026, 8, 14, 9, 30, 0, 123000000, time.UTC)
	identity := Identity{PID: 42, ProcessStartedAt: started, PlatformVersion: "1.2.3", ApprovedPlanDigestSHA256: repeatDigest('a')}
	service := Service{Listener: listener, Identity: identity}
	done := make(chan error, 1)
	go func() { done <- service.Run(context.Background()) }()

	response, err := http.Get("http://" + listener.Addr().String() + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	var health Health
	if err := json.NewDecoder(response.Body).Decode(&health); err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != http.StatusOK || health.Status != "ready" || health.Identity != identity {
		t.Fatalf("health = %#v status=%d", health, response.StatusCode)
	}

	request, _ := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/shutdown", nil)
	shutdown, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	shutdown.Body.Close()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("service did not stop")
	}
}

func TestServiceCancelsComposedLoopsOnShutdown(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	loopStopped := make(chan struct{})
	service := Service{Listener: listener, Identity: Identity{PID: 1, ProcessStartedAt: time.Now().UTC(), PlatformVersion: "dev", ApprovedPlanDigestSHA256: repeatDigest('b')}, Loops: []Loop{
		func(ctx context.Context) error { <-ctx.Done(); close(loopStopped); return nil },
	}}
	done := make(chan error, 1)
	go func() { done <- service.Run(context.Background()) }()
	request, _ := http.NewRequest(http.MethodPost, "http://"+listener.Addr().String()+"/shutdown", nil)
	if _, err := http.DefaultClient.Do(request); err != nil {
		t.Fatal(err)
	}
	<-done
	select {
	case <-loopStopped:
	case <-time.After(time.Second):
		t.Fatal("loop was not cancelled")
	}
}

func repeatDigest(value byte) string {
	result := make([]byte, 64)
	for index := range result {
		result[index] = value
	}
	return string(result)
}
