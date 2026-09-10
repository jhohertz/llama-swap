package main

import (
	"reflect"
	"testing"
)

func TestKubeswap_ParseEnvVars(t *testing.T) {
	got, err := parseEnvVars([]string{"A=1", "B=x=y", "PATH=/usr/bin:/bin"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []envVar{{Key: "A", Value: "1"}, {Key: "B", Value: "x=y"}, {Key: "PATH", Value: "/usr/bin:/bin"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if _, err := parseEnvVars([]string{"NOEQUALS"}); err == nil {
		t.Error("expected error for missing '='")
	}
	if _, err := parseEnvVars([]string{"=v"}); err == nil {
		t.Error("expected error for empty key")
	}
}

func TestKubeswap_ParseGPUs(t *testing.T) {
	got, err := parseGPUs([]string{"amd.com/gpu=1"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["amd.com/gpu"] != "1" {
		t.Errorf("got %v", got)
	}
	if _, err := parseGPUs([]string{"nvidia.com/gpu"}); err == nil {
		t.Error("expected error for missing count")
	}
}

func TestKubeswap_ParseTolerations(t *testing.T) {
	got, err := parseTolerations([]string{"dedicated:Exists::NoSchedule"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []toleration{{Key: "dedicated", Operator: "Exists", Value: "", Effect: "NoSchedule"}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	got, err = parseTolerations([]string{"a=b"})
	if err == nil {
		t.Error("expected error for malformed toleration")
	}
}

func TestKubeswap_ParseVolumes(t *testing.T) {
	got, err := parseVolumes([]string{
		"pvc:llama-swap-models:/models:ro",
		"emptydir:slots:/slots",
		"hostpath:/data/models:/models:ro",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []volumeSpec{
		{Kind: volPVC, Name: "llama-swap-models", Path: "/models", ReadOnly: true},
		{Kind: volEmptyDir, Name: "slots", Path: "/slots"},
		{Kind: volHostPath, Name: "/data/models", Path: "/models", ReadOnly: true},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	for _, bad := range []string{"pvc:only-two", "blob:foo:/mnt", "pvc:/models", "pvc:claim:/models:rw"} {
		if _, err := parseVolumes([]string{bad}); err == nil {
			t.Errorf("expected error for %q", bad)
		}
	}
}

func TestKubeswap_ParseKeyValues(t *testing.T) {
	got, err := parseKeyValues([]string{"k1=v1", "k2=a=b"}, "x")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got["k1"] != "v1" || got["k2"] != "a=b" {
		t.Errorf("got %v", got)
	}
}
