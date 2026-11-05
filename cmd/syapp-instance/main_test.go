package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestCLIArguments(t *testing.T) {
	for _, c := range []struct {
		args []string
		euid int
		code int
		err  string
	}{
		{nil, 0, 2, "用法"},
		{[]string{"add", "blue", "--http", "18080"}, 1000, 1, "root"},
		{[]string{"add"}, 0, 2, "缺少实例名"},
		{[]string{"add", "--http", "1"}, 0, 2, "缺少实例名"},
		{[]string{"add", "blue", "--http", "18080"}, 0, 2, "都必须指定"},
		{[]string{"add", "blue", "--http", "18080", "--https", "18443", "--status", "8082", "extra"}, 0, 2, "参数错误"},
		{[]string{"remove"}, 0, 2, "缺少实例名"},
		{[]string{"bogus"}, 0, 2, "用法"},
	} {
		var out, errb bytes.Buffer
		if code := run(c.args, &out, &errb, c.euid); code != c.code || !strings.Contains(errb.String(), c.err) {
			t.Errorf("%q: code %d, stderr %q; want %d containing %q", c.args, code, errb.String(), c.code, c.err)
		}
	}
	var out bytes.Buffer
	if code := run([]string{"--help"}, &out, &out, 1000); code != 0 || !strings.Contains(out.String(), "syapp-instance add") {
		t.Fatalf("help: %d %s", code, out.String())
	}
}

func TestCLIUserMode(t *testing.T) {
	for _, c := range []struct {
		args []string
		euid int
		code int
		err  string
	}{
		{[]string{"--root", "/home/ops/edge", "list"}, 0, 1, "普通用户"},
		{[]string{"--root", "relative", "list"}, 1000, 2, "--root"},
		{[]string{"--root"}, 1000, 2, "用法"},
		{[]string{"ensure"}, 0, 2, "只用于 user 模式"},
		{[]string{"init", "--http", "8080"}, 0, 2, "只用于 user 模式"},
		{[]string{"add", "blue"}, 1000, 1, "--root"},
		{[]string{"--root", "/home/ops/edge", "init", "--http", "8080"}, 1000, 2, "都必须指定"},
	} {
		var out, errb bytes.Buffer
		if code := run(c.args, &out, &errb, c.euid); code != c.code || !strings.Contains(errb.String(), c.err) {
			t.Errorf("%q: code %d, stderr %q; want %d containing %q", c.args, code, errb.String(), c.code, c.err)
		}
	}
}
