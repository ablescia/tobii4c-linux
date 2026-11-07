package main

import "testing"

func TestUserUnit(t *testing.T) {
	for _, c := range []struct{ cgroup, want string }{
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/tobii-ws.service\n", "tobii-ws.service"},
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/tobii4c.service\n", "tobii4c.service"},
		{"0::/user.slice/user-1000.slice/user@1000.service/app.slice/app-com.microsoft.VSCode-1407115.scope\n", ""},
		{"0::/system.slice/tobii4c.service\n", ""}, // servizio di sistema: non si ferma con --user
		{"12:pids:/user.slice\n0::/user.slice/user-1000.slice/session-2.scope\n", ""},
	} {
		if got := userUnit(c.cgroup); got != c.want {
			t.Errorf("%q: %q, want %q", c.cgroup, got, c.want)
		}
	}
}
