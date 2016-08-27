package main

import "testing"

func Test_gitProject(t *testing.T) {
	if userProject := gitProject("https://github.com/nbio/cart"); userProject != "nbio/cart" {
		t.Errorf("Expected %q, got %q", "nbio/cart", userProject)
	}
	if userProject := gitProject("git@github.com:nbio/cart.git"); userProject != "nbio/cart" {
		t.Errorf("Expected %q, got %q", "nbio/cart", userProject)
	}
	// TODO: recognize other Git hosts
}
