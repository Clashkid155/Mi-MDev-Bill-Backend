package main

import "testing"

func TestRealMoney(t *testing.T) {
	cases := []struct {
		name string
		cfg  Config
		want bool
	}{
		{"sandbox everywhere", Config{Env: "sandbox", ABSSandbox: true}, false},
		{"production", Config{Env: "production", ABSSandbox: true}, true},
		{"live ABS purchases", Config{Env: "sandbox", ABSSandbox: false}, true},
	}
	for _, c := range cases {
		if got := c.cfg.RealMoney(); got != c.want {
			t.Errorf("%s: RealMoney() = %t, want %t", c.name, got, c.want)
		}
	}
}
