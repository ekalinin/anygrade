package main

import "testing"

func TestBasic(t *testing.T) {
	if got := Greet("world"); got != "Hello, world!" {
		t.Errorf("Greet(%q) = %q", "world", got)
	}
}

func TestAdvanced(t *testing.T) {
	if got := Greet(""); got != "Hello, !" {
		t.Errorf("Greet(%q) = %q", "", got)
	}
}
