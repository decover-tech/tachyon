package main

import (
	"reflect"
	"testing"
)

func TestEgressHostsKeepsURLPorts(t *testing.T) {
	got := egressHosts("github.com", "", "https://npm.r1.tachyon.internal/", "http://go.r1:8080", "http://reg.r1/", "pypi.org:443", "10.0.0.0/8")
	want := []string{"github.com", "npm.r1.tachyon.internal:443", "go.r1:8080", "reg.r1:80", "pypi.org:443", "10.0.0.0/8"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("egressHosts = %v, want %v", got, want)
	}
}
