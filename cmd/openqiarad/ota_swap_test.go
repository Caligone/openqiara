package main

import (
	"reflect"
	"testing"
)

func TestEnsureLogFlagAddsWhenMissing(t *testing.T) {
	args := []string{"/data/openqiarad", "-web", ":80", "-mode", "fbxhome"}
	got := ensureLogFlag(args, "/data/openqiarad.log")
	want := []string{"/data/openqiarad", "-web", ":80", "-mode", "fbxhome", "-log", "/data/openqiarad.log"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
}

func TestEnsureLogFlagPreservesWhenPresent(t *testing.T) {
	cases := [][]string{
		{"/data/openqiarad", "-log", "/custom/path.log"},
		{"/data/openqiarad", "--log", "/custom/path.log"},
		{"/data/openqiarad", "-log=/custom/path.log"},
		{"/data/openqiarad", "--log=/custom/path.log"},
	}
	for _, args := range cases {
		got := ensureLogFlag(args, "/data/openqiarad.log")
		if !reflect.DeepEqual(got, args) {
			t.Errorf("args %v should be preserved, got %v", args, got)
		}
	}
}
