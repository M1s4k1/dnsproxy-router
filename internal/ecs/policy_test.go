package ecs

import "testing"

func TestNewPolicy(t *testing.T) {
	cases := []struct {
		name    string
		mode    string
		address string
		want    string
		err     bool
	}{
		{"off", "off", "", "off", false},
		{"pass", "pass", "", "pass", false},
		{"override-ok", "override", "1.2.3.4/24", "override", false},
		{"override-bad", "override", "not-a-prefix", "", true},
		{"unknown", "bogus", "", "", true},
	}
	for _, c := range cases {
		p, err := New(c.mode, c.address)
		if c.err {
			if err == nil {
				t.Errorf("%s: 期望报错，实际成功", c.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: 意外报错 %v", c.name, err)
			continue
		}
		if p.mode != c.want {
			t.Errorf("%s: mode=%s want=%s", c.name, p.mode, c.want)
		}
	}
}
