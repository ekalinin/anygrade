package main

import "testing"

func TestAdd(t *testing.T) {
	got := Add(Point{X: 1, Y: 2}, Point{X: 3, Y: 4})
	want := Point{X: 4, Y: 6}
	if got != want {
		t.Errorf("Add() = %+v, want %+v", got, want)
	}
}
