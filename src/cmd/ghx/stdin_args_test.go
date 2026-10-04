package main

import (
	"os"
	"testing"
)

func TestStdinArguments(t *testing.T) {
	null, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer null.Close()
	for _, args := range [][]string{
		{"api", "markdown", "--input", "-"},
		{"api", "graphql", "--input=-"},
		{"api", "markdown", "-F", "text=@-"},
		{"api", "markdown", "-Ftext=@-"},
		{"pr", "create", "--body-file=-"},
		{"release", "create", "v1", "--notes-file", "-"},
	} {
		if !readsStdin(args, null) {
			t.Errorf("stdin argument missed: %v", args)
		}
	}
	for _, args := range [][]string{
		{"api", "repos/o/r"},
		{"api", "markdown", "--input", "body.json"},
		{"api", "markdown", "-f", "text=hi"},
	} {
		if readsStdin(args, null) {
			t.Errorf("unexpected stdin bypass: %v", args)
		}
	}
}
